package childcleanup

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dagsched"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/registry"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// Mark is the merged mark that counts now: the one on the current head event, generation, revision and criteria of the relationship.
type Mark struct {
	Event      string
	Generation int64
	Revision   string
}

// Facts are what the relay store says of one relationship, read in one snapshot.
type Facts struct {
	RelationshipID, ParentTaskID, ChildTaskID string
	Status, State                             string // relationship status and the assignment's state word
	Superseded                                bool
	Mark                                      *Mark  // nil when no merged mark counts
	OtherLive                                 string // another live relationship that names the same child
	Owed                                      string // what of it is still owed, "" for nothing
	PlanNode                                  bool   // the relationship executed a plan node
	Integrated                                bool   // that node's accepted head stands on Mark and has landed on every target
}

func refuse(reason contract.RefusalReason, format string, args ...any) error {
	return &store.RefusedError{Reason: string(reason), Detail: fmt.Sprintf(format, args...)}
}

// Judge decides whether the child of f may be cleaned up on behalf of actor. All of these must hold, because the cleanup unloads the child and a correction sent to an unloaded child would resume a thread
// nobody expects to run: the actor is the relationship's parent; the relationship is live or closed (not paused or cancelled) and was not handed to another one; a merged mark counts for its current head,
// generation, revision and criteria (a correction, a new revision or a new generation stops it counting); no other live relationship uses the child; nothing is still owed; and, for a plan node's
// execution, the node's accepted head stands on that mark and has landed on every target (the integration observation).
func Judge(f Facts, actor string) error {
	switch {
	case f.ParentTaskID != actor:
		return refuse(contract.RefusalScopeRoleMismatch, "task %s is not the parent of relationship %s, which is held by %s", actor, f.RelationshipID, f.ParentTaskID)
	case f.Status != "active" && f.Status != "archived":
		return refuse(contract.RefusalRelationshipNotActive, "relationship %s is %s", f.RelationshipID, f.Status)
	case f.Superseded:
		return refuse(contract.RefusalDispositionConflict, "relationship %s was superseded by another one, which now owns the child", f.RelationshipID)
	case f.Mark == nil:
		return refuse(contract.RefusalDispositionConflict, "no merged mark counts for relationship %s (the assignment is %s): a correction can still be sent", f.RelationshipID, f.State)
	case f.OtherLive != "":
		return refuse(contract.RefusalDispositionConflict, "relationship %s also names child %s and is still live", f.OtherLive, f.ChildTaskID)
	case f.Owed != "":
		return refuse(contract.RefusalDispositionConflict, "%s", f.Owed)
	case f.PlanNode && !f.Integrated:
		return refuse(contract.RefusalDispositionConflict, "the plan node relationship %s executed is not integrated on this merged mark yet: observe its integration first", f.RelationshipID)
	}
	return nil
}

// What of the relationship is still owed: an unsent delivery to its parent or an unsent supervisor message (the states registry.owedOf, settled.go, keeps).
var owedQueries = []struct{ query, noun string }{
	{"SELECT state FROM deliveries WHERE relationship_id = ? AND state IN ('queued','deferred_busy','withheld_pre_send','sending','held_uncertain','inbox_only') LIMIT 1", "delivery of it to its parent"},
	{"SELECT state FROM supervisor_messages WHERE relationship_id = ? AND state IN ('queued','deferred_busy','withheld_pre_send','sending','held_uncertain') LIMIT 1", "supervisor message about it"},
}

// ReadFacts reads the facts of a relationship inside one store transaction.
func ReadFacts(ctx context.Context, st *store.Store, relationship string) (f Facts, err error) {
	err = st.Transaction(ctx, func(ctx context.Context, _ *sql.Conn) error {
		reg := &registry.Registry{Store: st}
		rel, err := reg.Get(ctx, relationship)
		if err != nil {
			return err
		}
		view := registry.NewAssignmentView(reg)
		view.Program = func() []string { return []string{"codex-session-relay"} }
		state, err := view.State(ctx, relationship)
		if err != nil {
			return err
		}
		f = Facts{RelationshipID: relationship, ParentTaskID: rel.Parent.TaskID, ChildTaskID: rel.Child.TaskID, Status: rel.Status, Superseded: rel.SupersededBy.String != "", State: textOf(state, "state")}
		if mark, ok := state.Lookup("mark"); ok {
			if m, ok := mark.(contract.OrderedObject); ok {
				generation, _ := strconv.ParseInt(textOf(m, "executionGeneration"), 10, 64)
				f.Mark = &Mark{Event: textOf(m, "eventId"), Generation: generation, Revision: textOf(m, "revisionHash")}
			}
		}
		row, err := st.One(ctx, "SELECT relationship_id FROM relationships WHERE child_task_id = ? AND relationship_id <> ? AND status IN ('active','paused') AND superseded_by IS NULL ORDER BY created_at LIMIT 1", f.ChildTaskID, relationship)
		if err != nil {
			return err
		} else if row != nil {
			f.OtherLive = fmt.Sprint(row.Get("relationship_id"))
		}
		for _, owed := range owedQueries {
			if row, err := st.One(ctx, owed.query, relationship); err != nil {
				return err
			} else if row != nil && f.Owed == "" {
				f.Owed = fmt.Sprintf("a %s %s is still owed", row.Get("state"), owed.noun)
			}
		}
		if f.Mark != nil {
			f.PlanNode, f.Integrated, err = (&dagsched.Scheduler{Store: st}).ExecutionIntegrated(ctx, relationship, f.Mark.Event, f.Mark.Generation, f.Mark.Revision)
		}
		return err
	})
	return f, err
}

func textOf(o contract.OrderedObject, key string) string {
	if v, _ := o.Lookup(key); v != nil {
		return fmt.Sprint(v)
	}
	return ""
}

// Result is what Execute answers: the relationship and the cleanup report.
type Result struct {
	RelationshipID string
	Report
}

// Execute cleans up the child of a relationship: it reads the facts and judges them (a refusal before any App Server call), reads them again right before the first archive, and then runs Clean. Nothing
// keeps the relationship from changing after that second reading; an archive is undone with codex unarchive.
func Execute(ctx context.Context, st *store.Store, host Host, relationship, actor string, dryRun bool) (Result, error) {
	result := Result{RelationshipID: relationship}
	facts, err := ReadFacts(ctx, st, relationship)
	if err == nil {
		err = Judge(facts, actor)
	}
	if err != nil {
		return result, err
	}
	recheck := func(ctx context.Context) error {
		again, err := ReadFacts(ctx, st, relationship)
		if err == nil {
			err = Judge(again, actor)
		}
		if err == nil && *again.Mark != *facts.Mark {
			err = refuse(contract.RefusalDispositionConflict, "the merged mark of relationship %s changed while its threads were being read", relationship)
		}
		return err
	}
	result.Report, err = Clean(ctx, host, facts.ChildTaskID, Options{DryRun: dryRun, Recheck: recheck})
	return result, err
}
