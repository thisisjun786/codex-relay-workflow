package dagsched

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dag"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/registry"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// A generation opened by hand for an accepted node whose plan revision changed only its criteria (CRW-1036). The route of such a node is a re-validation
// (revalidation.go): the same output is ruled again under the plan's criteria and accepted again, with no new generation and no new child. dag-correct
// therefore refuses to record a hand-opened generation for it, under any reason it was opened with, and dag-accept refuses a generation that is not recorded as an execution, so the child
// would finish work that can only be refused afterwards. generation-open and generation-bind ask this inside the transaction that writes the generation or the binding.

func init() { registry.HandOpenedGenerationGuard = guardHandOpenedGeneration }

// generationOpenedByRuling is whether a needs_changes ruling opened the generation: such a generation is recorded by dag-correct whatever the node's route is now.
func generationOpenedByRuling(ctx context.Context, q store.Querier, relationship string, generation int64) (bool, error) {
	var one int
	return queryOne(ctx, q, "SELECT 1 FROM verdicts v JOIN events e ON e.event_id = v.event_id WHERE e.relationship_id = ? AND v.verdict = 'needs_changes' AND v.next_generation = ? LIMIT 1", []any{relationship, generation}, &one)
}

// generationRecorded is whether the generation is recorded as an execution of a plan node (dag_node_executions): dag-accept accepts its result whatever opened it.
func generationRecorded(ctx context.Context, q store.Querier, relationship string, generation int64) (bool, error) {
	var one int
	return queryOne(ctx, q, "SELECT 1 FROM dag_node_executions WHERE relationship_id = ? AND execution_generation = ? LIMIT 1", []any{relationship, generation}, &one)
}

// guardHandOpenedGeneration refuses a generation opened or bound by hand for a relationship that executes an accepted plan node whose route is a re-validation,
// whatever reason it is opened under: dag-correct records none of them on that route (a correction reason is refused by the route, initial_assignment and an
// empty reason state no correction), and dag-accept refuses a generation that is not recorded. Every other generation passes: one for a node that is not
// accepted (the first generation of an assignment among them), one for an accepted node that is not stale (CRW-906), one for a node whose route is a
// correction, a decision reply (dag-correct records it by its own rule) and a generation a ruling opened.
func guardHandOpenedGeneration(ctx context.Context, st *store.Store, relationship, reason string, generation int64) error {
	if reason == delivery.DecisionReply {
		return nil
	}
	q := st.Q(ctx)
	if present, err := tableExists(ctx, q, "dag_node_executions"); err != nil || !present {
		return err
	}
	var plan, node string
	if has, err := queryOne(ctx, q, "SELECT plan_id, node_id FROM dag_node_executions WHERE relationship_id = ? ORDER BY execution_generation DESC LIMIT 1", []any{relationship}, &plan, &node); err != nil || !has {
		return err
	}
	if generation > 0 {
		if ruled, err := generationOpenedByRuling(ctx, q, relationship, generation); err != nil || ruled {
			return err
		}
	}
	s := &Scheduler{Store: st}
	snap, n, err := liveNode(ctx, q, plan, node)
	if err != nil {
		// a node the plan no longer holds has no route to ask about
		return nil
	}
	rel, found, err := currentRelationshipOf(ctx, q, plan, node)
	if err != nil || !found || rel.ID != relationship {
		return err
	}
	if _, has, err := loadActiveAcceptance(ctx, q, plan, node); err != nil || !has {
		return err
	}
	ctx, _ = memoFor(ctx, plan, snap)
	stale, err := s.staleOf(ctx, q, plan, snap, n)
	if err != nil || stale == nil {
		return err
	}
	action, detail, err := s.routeOf(ctx, q, plan, snap, n, stale, true)
	if err != nil || action != ActionRevalidate {
		return err
	}
	return refuse(contract.RefusalDispositionConflict, "the result of %s is accepted and its route is %s, not a correction by hand: %s. A generation opened or bound by hand for it can be recorded by neither dag-correct nor dag-accept, so none is written; if one is open and was never sent to the child, close it with dag-generation-withdraw", n.NodeID, action, detail)
}

// openGenerationDeadEnd says where a stale node goes when a generation opened by hand sits open on a route that cannot record it: the criteria alone changed, and no ruling opened the generation. "" when the
// generation is not that case, so the reading keeps the words it has for a correction that is on its way to the child.
func (s *Scheduler) openGenerationDeadEnd(ctx context.Context, q store.Querier, plan string, snap dag.Snapshot, n dag.SnapNode, st *Stale, rel relRow) (string, error) {
	if st.Cause != CauseCriteriaChanged {
		return "", nil
	}
	if ruled, err := generationOpenedByRuling(ctx, q, rel.ID, rel.Generation); err != nil || ruled {
		return "", err
	}
	// a generation dag-correct already recorded is accepted with --supersedes once its result is ruled under the plan's criteria, and a decision reply is recorded by its own rule: neither is a dead end
	if recorded, err := generationRecorded(ctx, q, rel.ID, rel.Generation); err != nil || recorded {
		return "", err
	}
	var reason sql.NullString
	if _, err := queryOne(ctx, q, "SELECT reason FROM generations WHERE relationship_id = ? AND execution_generation = ?", []any{rel.ID, rel.Generation}, &reason); err != nil || reason.String == delivery.DecisionReply {
		return "", err
	}
	changed, unavailable, err := s.consumedChange(ctx, q, plan, snap, n)
	if err != nil || changed != nil || unavailable != "" {
		return "", err
	}
	return fmt.Sprintf("generation %d of relationship %s was opened by hand, and only the criteria of %s changed: its route is a re-validation, so dag-correct refuses to record that generation and dag-accept refuses a generation that is not recorded. "+
		"If the generation was never sent to the child, close it with dag-generation-withdraw, then rule the same output again under the plan's criteria and accept it again with dag-accept (no new generation, no new child). "+
		"A generation already bound to a turn cannot be withdrawn: its result is integrated through git and the node's record is left as it is (docs/relay/dag-scheduler.md, Corrections)", rel.Generation, short(rel.ID), n.NodeID), nil
}

// criteriaRegistrationDrift names the step that a plan revision of a node's criteria leaves undone on the relationship: the criteria registered for it (criteria-register) are still the old set, and a ruling under
// the plan's criteria is refused (unknown_criterion, criteria_set_changed) until they are registered again. "" when the registration is the plan's or the relationship has none.
func criteriaRegistrationDrift(ctx context.Context, q store.Querier, rel relRow, n dag.SnapNode) (string, error) {
	var rows, distinct int
	var registered *string
	if _, err := queryOne(ctx, q, "SELECT COUNT(*), COUNT(DISTINCT set_digest), MIN(set_digest) FROM canonical_criteria WHERE relationship_id = ?", []any{rel.ID}, &rows, &distinct, &registered); err != nil {
		return "", err
	}
	if rows == 0 || registered == nil || (distinct == 1 && *registered == n.CriteriaSetDigest) {
		return "", nil
	}
	return fmt.Sprintf("The criteria registered for relationship %s (set %s) are not the plan's (set %s) for %s: register the plan's criteria with criteria-register first, or the ruling is refused (unknown_criterion, criteria_set_changed)", short(rel.ID), short(*registered), short(n.CriteriaSetDigest), n.NodeID), nil
}
