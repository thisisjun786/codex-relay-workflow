package dagsched

import (
	"context"
	"database/sql"
	"strings"
	"unicode/utf8"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dag"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// SchemaWithdraw is the answer of dag-generation-withdraw.
const SchemaWithdraw = "dag-generation-withdraw/1"

// MaxWithdrawReason bounds the coordinator's statement of why the generation is withdrawn.
const MaxWithdrawReason = 1000

// WithdrawInput names the generation to withdraw and says why. The relationship is named, not looked up from the node, so a repeated call (a lost answer, a node adopted by a successor since) can only be
// the same withdrawal and never a withdrawal of another relationship's generation that has the same number.
type WithdrawInput struct {
	Relationship string
	Generation   int64
	Reason       string
}

// WithdrawResult is the answer of WithdrawGeneration.
type WithdrawResult struct {
	PlanID, NodeID, RelationshipID, DispatchRequestID, Reason string
	Generation, RestoredGeneration                            int64
	Replayed                                                  bool
}

// WithdrawGeneration closes a generation the coordinator opened by hand (generation-open) and never bound or sent to the child, and puts the relationship back on the generation before it, so
// that the node can be accepted, merged and integrated on the result it already has (CRW-446). A coordinator rules a child's report verified, finds that the branch needs the base, opens the next generation to
// send it and then refreshes the branch itself: the relationship stands on a generation that holds nothing, and dag-accept (which reads the relationship's current generation), the merged mark and
// every other reader of that generation find no report there.
//
// It writes only what the relay can show was never used: the generation's row is an anchor still pending (no dispatch turn), it carries no event of any outcome, no turn was admitted to it, no ruling
// opened it, and no node execution was recorded on it (dag-correct and dag-adopt record one). What the relay cannot see is a message that reached the child outside the relay: a child that worked on a
// generation that was never bound has its work ignored, because a receipt in an unbound generation is refused and the generation cannot be bound any more. The generation's row is kept and a record of the
// withdrawal is appended (dag_generation_withdrawals), so a generation number is never reused: the next generation takes the number after the highest the relationship ever held. The command is the
// counterpart of generation-open for the DAG, a repeat with the same facts is a replay, and a refusal writes nothing.
func (s *Scheduler) WithdrawGeneration(ctx context.Context, plan, node, actor string, in WithdrawInput) (WithdrawResult, error) {
	out := WithdrawResult{PlanID: plan, NodeID: node, Generation: in.Generation}
	reason := strings.TrimSpace(in.Reason)
	if strings.TrimSpace(in.Relationship) == "" {
		return out, refuse(contract.RefusalMalformedReceipt, "a withdrawal names the relationship whose generation it closes (--relationship)")
	}
	if in.Generation < 2 {
		return out, refuse(contract.RefusalMalformedReceipt, "a generation opened by hand is withdrawn by its number, 2 or more: generation 1 is the assignment itself")
	}
	if reason == "" || utf8.RuneCountInString(reason) > MaxWithdrawReason {
		return out, refuse(contract.RefusalMalformedReceipt, "a withdrawal records why: --reason is 1 to %d characters", MaxWithdrawReason)
	}
	out.Reason = reason
	err := s.Store.Compose(ctx, func(txCtx context.Context, _ *sql.Conn) error {
		tx := s.Store.Q(txCtx)
		if err := s.fence(txCtx, tx, plan, actor); err != nil {
			return err
		}
		snap, _, err := dag.SnapshotAt(txCtx, tx, plan, 0)
		if err != nil {
			return err
		}
		n, ok := nodeOf(snap, node)
		if !ok {
			return refuse(contract.RefusalUnregisteredScope, "plan %s has no live node %s", plan, node)
		}
		if err := lifecycleRefusal(snap, n, "withdrawing a generation of it", false); err != nil {
			return err
		}
		rel, found, err := loadRelationship(txCtx, tx, in.Relationship)
		if err != nil {
			return err
		}
		if !found {
			return refuse(contract.RefusalUnregisteredRelationship, "no relationship %s", in.Relationship)
		}
		var executed int
		if has, err := queryOne(txCtx, tx, "SELECT 1 FROM dag_node_executions WHERE plan_id = ? AND node_id = ? AND relationship_id = ? LIMIT 1", []any{plan, node, rel.ID}, &executed); err != nil {
			return err
		} else if !has {
			return refuse(contract.RefusalDispositionConflict, "relationship %s is not an execution of node %s of plan %s", rel.ID, node, plan)
		}
		if rel.ParentTaskID != actor {
			return notParentOf(actor, rel)
		}
		out.RelationshipID = rel.ID
		// the same withdrawal again: the record answers, and the relationship may have moved on since
		var recordedPlan, recordedNode string
		var restored int64
		if replay, err := queryOne(txCtx, tx, "SELECT plan_id, node_id, dispatch_request_id, restored_generation, reason FROM dag_generation_withdrawals WHERE relationship_id = ? AND execution_generation = ?",
			[]any{rel.ID, in.Generation}, &recordedPlan, &recordedNode, &out.DispatchRequestID, &restored, &out.Reason); err != nil {
			return err
		} else if replay {
			if recordedPlan != plan || recordedNode != node {
				return refuse(contract.RefusalDispositionConflict, "generation %d of %s was withdrawn for node %s of plan %s", in.Generation, rel.ID, recordedNode, recordedPlan)
			}
			out.RestoredGeneration, out.Replayed = restored, true
			return nil
		}
		if rel.Status != "active" || rel.Superseded {
			return refuse(contract.RefusalRelationshipNotActive, "the relationship %s of %s is %s: a generation is withdrawn while its relationship is active", rel.ID, node, map[bool]string{true: "superseded", false: rel.Status}[rel.Superseded])
		}
		if rel.Generation != in.Generation {
			return refuse(contract.RefusalStaleGeneration, "generation %d is not the generation %s stands on (%d): only the newest generation, the one opened by hand and never sent, is withdrawn", in.Generation, rel.ID, rel.Generation)
		}
		var anchor string
		var turn, bound, opened sql.NullString
		if has, err := queryOne(txCtx, tx, "SELECT dispatch_request_id, anchor_state, dispatch_turn_id, bound_at, reason FROM generations WHERE relationship_id = ? AND execution_generation = ?",
			[]any{rel.ID, in.Generation}, &out.DispatchRequestID, &anchor, &turn, &bound, &opened); err != nil {
			return err
		} else if !has {
			return refuse(contract.RefusalUnknownGeneration, "%s has no generation %d", rel.ID, in.Generation)
		}
		// generation-open gives the generation a reason; a returning tenure's generation has none, and undoing a tenure is not what a withdrawal does (it leaves the supersession and the linkage as they are)
		if opened.String != "initial_assignment" && opened.String != "needs_changes_revision" {
			return refuse(contract.RefusalDispositionConflict, "generation %d of %s was not opened by generation-open (its reason is %q): a returning tenure or a reply opened it, and a withdrawal does not undo that", in.Generation, rel.ID, opened.String)
		}
		if anchor != "anchor_pending" || turn.Valid || bound.Valid {
			return refuse(contract.RefusalDispositionConflict, "generation %d of %s is bound to a dispatch turn (%s): it was sent to the child, so it is not withdrawn; the child reports in it", in.Generation, rel.ID, turn.String)
		}
		for _, c := range []struct{ what, query string }{
			{"an event (a report, a revision request or a reply)", "SELECT COUNT(*) FROM events WHERE relationship_id = ? AND execution_generation = ?"},
			{"an admitted turn", "SELECT COUNT(*) FROM generation_turns WHERE relationship_id = ? AND execution_generation = ?"},
			{"a recorded execution of a node", "SELECT COUNT(*) FROM dag_node_executions WHERE relationship_id = ? AND execution_generation = ?"},
			{"a needs_changes ruling that opened it", "SELECT COUNT(*) FROM verdicts v JOIN events e ON e.event_id = v.event_id WHERE e.relationship_id = ? AND v.next_generation = ?"},
		} {
			var count int
			if err := tx.QueryRowContext(txCtx, c.query, rel.ID, in.Generation).Scan(&count); err != nil {
				return err
			}
			if count > 0 {
				return refuse(contract.RefusalDispositionConflict, "generation %d of %s carries %s: it was used, so it is not withdrawn", in.Generation, rel.ID, c.what)
			}
		}
		if restored, err = store.LiveGenerationBefore(txCtx, tx, rel.ID, in.Generation); err != nil {
			return err
		}
		var before int
		if err := tx.QueryRowContext(txCtx, "SELECT COUNT(*) FROM generations WHERE relationship_id = ? AND execution_generation = ?", rel.ID, restored).Scan(&before); err != nil {
			return err
		} else if before == 0 {
			return refuse(contract.RefusalUnknownGeneration, "%s has no generation %d to stand on again", rel.ID, restored)
		}
		at := s.now()
		if _, err := tx.ExecContext(txCtx, "INSERT INTO dag_generation_withdrawals (relationship_id, execution_generation, plan_id, node_id, dispatch_request_id, opened_reason, restored_generation, reason, withdrawn_by_task_id, coordinator_epoch, withdrawn_at)"+
			" VALUES (?,?,?,?,?,(SELECT COALESCE(reason, '') FROM generations WHERE relationship_id = ? AND execution_generation = ?),?,?,?,?,?)",
			rel.ID, in.Generation, plan, node, out.DispatchRequestID, rel.ID, in.Generation, restored, reason, actor, s.ExpectedEpoch, at); err != nil {
			return err
		}
		if _, err := tx.ExecContext(txCtx, "UPDATE relationships SET execution_generation = ?, updated_at = ? WHERE relationship_id = ?", restored, at, rel.ID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(txCtx, "INSERT INTO journal (at, kind, subject, detail) VALUES (?,?,?,?)", at, "generation_withdrawn", rel.ID,
			dag.Canonical(map[string]any{"generation": in.Generation, "restored_generation": restored, "plan_id": plan, "node_id": node})); err != nil {
			return err
		}
		out.RestoredGeneration = restored
		return nil
	})
	return out, err
}
