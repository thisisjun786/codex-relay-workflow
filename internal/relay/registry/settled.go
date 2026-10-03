package registry

import (
	"context"
	"database/sql"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
)

// CRW-288: a relationship whose work is merged is settled once nothing of it is still owed, and a settled one is
// closed (archived) instead of staying live for good. Two writers close them: a project handover, for the rows it
// would otherwise leave naming the parent that stepped down, and relationship-close-merged, for an operator
// sweeping a project or a whole store. Both decide with the one predicate below, and both close through
// setStatusIn, the write relationship-status makes.

// unsentDeliveryStates are the delivery states in which something has not reached the parent yet. inbox_only counts
// too: nothing was pushed, and a verified acknowledgement moves a delivery to acknowledged, so one still in the inbox
// has not been acknowledged.
var unsentDeliveryStates = []string{"queued", "deferred_busy", "withheld_pre_send", "sending", "held_uncertain", "inbox_only"}

// unsentMessageStates are the states in which a supervisor message has not left yet (dispatched and read are the
// settled ones). The supervisor channel turns a push its recipient cannot serve into withheld_pre_send, so inbox_only
// is not a message state.
var unsentMessageStates = []string{"queued", "deferred_busy", "withheld_pre_send", "sending", "held_uncertain"}

// closedMergedReason is the reason a journal row carries when a relationship was closed because it was merged.
const closedMergedReason = "merged_settled"

// settledReading is the relay's answer to whether one live assignment has nothing left to do.
type settledReading struct {
	id, issue, parent, child, state string
	// settled is true for a merged assignment with nothing owed; otherwise why says what keeps it open.
	settled bool
	why     string
	// mark is the merge mark that counts now (an ordered object), when settled.
	mark contract.OrderedObject
}

func placeholders(n int) string { return strings.TrimSuffix(strings.Repeat("?,", n), ",") }

func stringArgs(prefix any, values []string) []any {
	out := []any{prefix}
	for _, v := range values {
		out = append(out, v)
	}
	return out
}

// readSettled decides one live relationship. The assignment must read merged, which counts a mark only where it
// matches the current head event, generation and revision with the criteria still current, and nothing of it may
// be owed: not a delivery, not a supervisor message, and, for a plan node's execution, not an acceptance.
func (r *Registry) readSettled(ctx context.Context, view *AssignmentView, rid string) (settledReading, error) {
	state, err := view.State(ctx, rid)
	if err != nil {
		return settledReading{}, err
	}
	reading := settledReading{id: rid, issue: pyjson.Text(state.Get("issueKey")), parent: pyjson.Text(state.Get("parentTaskId")),
		child: pyjson.Text(state.Get("childTaskId")), state: pyjson.Text(state.Get("state"))}
	if status := pyjson.Text(state.Get("relationshipStatus")); status != Active {
		reading.why = "the relationship is " + status
		return reading, nil
	}
	if reading.state != StateMerged {
		reading.why = "the assignment is " + reading.state + ", not merged"
		return reading, nil
	}
	mark, _ := state.Lookup("mark")
	reading.mark, _ = mark.(contract.OrderedObject)
	criteria, _ := state.Lookup("criteria")
	registered, _ := criteria.(contract.OrderedObject)
	digest, _ := registered.Lookup("setDigest")
	if why, err := r.owedOf(ctx, rid, reading.mark, digest); err != nil || why != "" {
		reading.why = why
		return reading, err
	}
	reading.settled = true
	return reading, nil
}

// owedOf names what of rid is still owed, or "" when nothing is.
func (r *Registry) owedOf(ctx context.Context, rid string, mark contract.OrderedObject, criteria any) (string, error) {
	row, err := r.Store.One(ctx, "SELECT d.state AS state FROM deliveries d WHERE d.relationship_id = ?"+
		" AND d.state IN ("+placeholders(len(unsentDeliveryStates))+") ORDER BY d.created_at LIMIT 1", stringArgs(rid, unsentDeliveryStates)...)
	if err != nil {
		return "", err
	}
	if row != nil {
		return "a " + colString(row, "state") + " delivery of it is still owed to its parent", nil
	}
	row, err = r.Store.One(ctx, "SELECT m.state AS state FROM supervisor_messages m WHERE m.relationship_id = ?"+
		" AND m.state IN ("+placeholders(len(unsentMessageStates))+") ORDER BY m.staged_at LIMIT 1", stringArgs(rid, unsentMessageStates)...)
	if err != nil {
		return "", err
	}
	if row != nil {
		return "a " + colString(row, "state") + " supervisor message about it is still owed", nil
	}
	// A plan node's result is accepted, and its acceptance revalidated, while its relationship is active (dag-accept),
	// so closing an execution whose current head has no active acceptance under the current criteria would shut the
	// node's own door. The zone is created by the first writable open; a store opened read-only may predate it.
	zone, err := r.Store.One(ctx, "SELECT 1 FROM sqlite_master WHERE type = 'table' AND name = 'dag_acceptances'")
	if err != nil || zone == nil {
		return "", err
	}
	// The acceptance is looked up by the merge mark's own relationship, generation, event and revision, not by the
	// generation of the execution row, so a node's older executions (a correction leaves one row per generation) do not
	// hold it open once the current head is accepted: every row of the node asks the same question.
	event, generation, revision := pyjson.Text(mark.Get("eventId")), markNumber(mark, "executionGeneration"), pyjson.Text(mark.Get("revisionHash"))
	// the acceptance may also stand on the marked head through a base refresh recorded for it (dag_base_refreshes, which arrived after the first zone): its newest record names the later generation the merged mark sits on
	match, args := "a.relationship_id = ? AND a.execution_generation = ? AND a.event_id = ? AND a.revision_hash = ?", []any{rid, rid, generation, event, revision}
	refreshes, err := r.Store.One(ctx, "SELECT 1 FROM sqlite_master WHERE type = 'table' AND name = 'dag_base_refreshes'")
	if err != nil {
		return "", err
	}
	if refreshes != nil {
		match += " OR EXISTS (SELECT 1 FROM dag_base_refreshes f WHERE f.acceptance_id = a.acceptance_id AND f.relationship_id = ? AND f.execution_generation = ? AND f.event_id = ? AND f.revision_hash = ?" +
			" AND f.refresh_seq = (SELECT MAX(x.refresh_seq) FROM dag_base_refreshes x WHERE x.acceptance_id = a.acceptance_id))"
		args = append(args, rid, generation, event, revision)
	}
	query := "SELECT e.node_id AS node FROM dag_node_executions e WHERE e.relationship_id = ?" +
		" AND NOT EXISTS (SELECT 1 FROM dag_acceptances a WHERE a.plan_id = e.plan_id AND a.node_id = e.node_id AND a.state = 'active'" +
		"   AND (" + match + ")%s) LIMIT 1"
	if criteria != nil {
		// the acceptance's effective criteria: its newest revalidation, else the digest it was accepted with.
		query = strings.Replace(query, "%s", " AND COALESCE((SELECT v.criteria_set_digest FROM dag_acceptance_revalidations v"+
			" WHERE v.acceptance_id = a.acceptance_id ORDER BY v.reval_seq DESC LIMIT 1), a.criteria_set_digest) = ?", 1)
		args = append(args, criteria)
	} else {
		query = strings.Replace(query, "%s", "", 1)
	}
	row, err = r.Store.One(ctx, query, args...)
	if err != nil {
		return "", err
	}
	if row != nil {
		return "its plan node " + colString(row, "node") + " has no active acceptance of this head yet", nil
	}
	return "", nil
}

func markNumber(mark contract.OrderedObject, key string) any {
	v, _ := mark.Lookup(key)
	return v
}

// closeSettledIn archives rid because it is merged and settled, inside the transaction the caller owns: the write
// relationship-status makes, so the issue scope is released and relationship-resume is the way back.
func (r *Registry) closeSettledIn(ctx context.Context, rid, actor, now string) error {
	return r.setStatusIn(ctx, rid, statusArchived, actor, now, contract.Field{Key: "reason", Value: closedMergedReason})
}

// liveRelationships are the live relationships of a project (all false) or of the whole store, oldest first.
func (r *Registry) liveRelationships(ctx context.Context, project string, all bool) ([]string, error) {
	query, args := "SELECT r.relationship_id AS rid FROM relationships r WHERE r.status IN ('active','paused') AND r.superseded_by IS NULL"+
		" ORDER BY r.created_at, r.relationship_id", []any{}
	if !all {
		query = "SELECT r.relationship_id AS rid FROM relationships r JOIN relationship_scope s ON s.relationship_id = r.relationship_id" +
			" WHERE s.project_key = ? AND r.status IN ('active','paused') AND r.superseded_by IS NULL ORDER BY r.created_at, r.relationship_id"
		args = []any{project}
	}
	rows, err := r.Store.All(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	out := make([]string, len(rows))
	for i, row := range rows {
		out[i] = colString(row, "rid")
	}
	return out, nil
}

// CloseMerged is relationship-close-merged: look at the live relationships of a project (or of the store with all),
// and close, with apply, exactly those that are merged with nothing owed. Without apply it reads and changes nothing.
// With apply the decision and the writes share one transaction, so a mark that stopped counting meanwhile (a new
// generation, a new head, changed criteria) is not closed.
func (r *Registry) CloseMerged(ctx context.Context, project string, all bool, actor string, apply bool) (contract.OrderedObject, error) {
	var answer contract.OrderedObject
	pass := func(ctx context.Context) error {
		live, err := r.liveRelationships(ctx, project, all)
		if err != nil {
			return err
		}
		view := NewAssignmentView(r)
		closable, kept, settled := []any{}, []any{}, []string{}
		for _, rid := range live {
			reading, err := r.readSettled(ctx, view, rid)
			if err != nil {
				return err
			}
			if !reading.settled {
				kept = append(kept, contract.OrderedObject{{Key: "relationshipId", Value: rid}, {Key: "issueKey", Value: reading.issue},
					{Key: "state", Value: reading.state}, {Key: "reason", Value: reading.why}})
				continue
			}
			settled = append(settled, rid)
			closable = append(closable, contract.OrderedObject{{Key: "relationshipId", Value: rid}, {Key: "issueKey", Value: reading.issue},
				{Key: "parentTaskId", Value: reading.parent}, {Key: "childTaskId", Value: reading.child},
				{Key: "mergedEvent", Value: markNumber(reading.mark, "eventId")}, {Key: "executionGeneration", Value: markNumber(reading.mark, "executionGeneration")}})
		}
		closed := []string{}
		if apply {
			now := r.now()
			for _, rid := range settled {
				if err := r.closeSettledIn(ctx, rid, actor, now); err != nil {
					return err
				}
				closed = append(closed, rid)
			}
		}
		var scope any
		if !all {
			scope = project
		}
		answer = contract.OrderedObject{{Key: "projectKey", Value: scope}, {Key: "apply", Value: apply}, {Key: "closable", Value: closable},
			{Key: "closed", Value: strList(closed)}, {Key: "kept", Value: kept}}
		return nil
	}
	if !apply {
		err := pass(ctx)
		return answer, err
	}
	err := r.Store.Transaction(ctx, func(ctx context.Context, _ *sql.Conn) error { return pass(ctx) })
	return answer, err
}
