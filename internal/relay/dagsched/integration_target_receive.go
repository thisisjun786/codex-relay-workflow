package dagsched

import (
	"context"
	"database/sql"
	"errors"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// The receive and acknowledgement comparison (CRW-1302). When a parent acknowledges a child's event, the relay compares the
// integration target recorded for the child's task with the baselineRevision of the child's intent, without a network call and
// without a git call: it reads the newest observation the push or the checker stored (dag_integration_targets), the intent file
// of the assignment, and the heads recorded for the integration ref. The comparison has four inputs, and it compares nothing when
// one is missing:
//
//   - the intent: the event's generation was started by managed-start, whose intent file under the marker root names a
//     baselineRevision that is a full commit id (the baseline of a node without a base is an opaque string, "dag-input-manifest:...");
//   - the plan: the child's issue key names live nodes of plans, and exactly one current target of those plans has an observation;
//   - the observation: a head the push or the checker read on that target's remote branch;
//   - the store rows: each of them readable.
//
// An observed head equal to the baseline is a target that did not move. An observed head that differs from the baseline and is one of
// the heads recorded for the target's integration ref is the target advancing along the integration path (another node was integrated),
// and records nothing. An observed head that is neither is a move outside the path, the one CRW-1301 records: the same broken
// integration_target_moved fault, one occurrence per remote head, with the source receive in its evidence, and the acknowledgement's
// output names it to the parent. The step proceeds either way: the acknowledgement is what it was, nothing is reverted and nothing is
// refused. intent.json and the intent fields of the delivery package carry no target ref name; the target is the store's record.

func init() { delivery.AckTargetNotice = ReceiveTargetNotice }

// ReceiveTargetNotice answers the notice an acknowledgement of event carries, or nil when the comparison finds nothing outside the
// path or compares nothing. An error is an unreadable row; the acknowledgement does not read it.
func ReceiveTargetNotice(ctx context.Context, st *store.Store, event string) (delivery.Obj, error) {
	baseline, issue, err := childBaseline(ctx, st, event)
	if err != nil || baseline == "" || !fullCommitPattern.MatchString(baseline) {
		return nil, err
	}
	s := &Scheduler{Store: st}
	target, observed, found, err := s.observedTargetOf(ctx, issue, baseline)
	if err != nil || !found || observed == baseline {
		return nil, err
	}
	expected, plan, _, err := s.recordedTargetHeads(ctx, target.IntegrationRef)
	if err != nil || len(expected) == 0 || headSet(expected)[observed] {
		return nil, err
	}
	if target.PlanID != "" {
		plan = target.PlanID
	}
	moved := &TargetMoved{IntegrationRef: target.IntegrationRef, RemoteHead: observed, Expected: expected[:min(len(expected), expectedShown)], ExpectedTotal: len(expected), Strict: true, Source: SourceReceive}
	signature := map[string]any{"checkout": target.Checkout, "remote": target.Remote, "remoteRef": target.RemoteRef}
	if id, err := s.recordTargetMoved(ctx, moved, signature, plan); err != nil {
		moved.FaultError = err.Error()
	} else {
		moved.FaultID = id
	}
	shown := make([]any, len(moved.Expected))
	for i, head := range moved.Expected {
		shown[i] = head
	}
	return delivery.Obj{{Key: "faultClass", Value: ClassIntegrationTargetMoved}, {Key: "faultId", Value: nullableString(moved.FaultID)}, {Key: "faultError", Value: nullableString(moved.FaultError)},
		{Key: "integrationRef", Value: target.IntegrationRef}, {Key: "remote", Value: target.Remote}, {Key: "remoteRef", Value: target.RemoteRef},
		{Key: "observedHead", Value: observed}, {Key: "baselineRevision", Value: baseline}, {Key: "expectedHeads", Value: shown}, {Key: "expectedTotal", Value: int64(moved.ExpectedTotal)}}, nil
}

func nullableString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// childBaseline reads the baselineRevision of the intent that started the generation of the event, and the issue key of the child's
// relationship. "" means there is no such intent: the generation was not started by managed-start, or its intent file is absent or
// unreadable.
func childBaseline(ctx context.Context, st *store.Store, event string) (baseline, issue string, err error) {
	var workspace, root, request string
	err = st.Q(ctx).QueryRowContext(ctx, "SELECT r.issue_key, m.workspace, m.marker_root, g.dispatch_request_id FROM events e"+
		" JOIN relationships r ON r.relationship_id = e.relationship_id"+
		" JOIN generations g ON g.relationship_id = e.relationship_id AND g.execution_generation = e.execution_generation"+
		" JOIN managed_start_requests m ON m.relationship_id = e.relationship_id AND m.dispatch_request_id = g.dispatch_request_id"+
		" WHERE e.event_id = ? ORDER BY m.created_at DESC LIMIT 1", event).Scan(&issue, &workspace, &root, &request)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", nil
	}
	if err != nil {
		return "", "", err
	}
	dir, err := delivery.AssignmentDir(root, workspace, delivery.AssignmentID(request))
	if err != nil {
		return "", "", nil
	}
	facts, _ := delivery.ReadAssignment(ctx, dir)
	intent, ok := facts.Lookup("intent")
	if !ok {
		return "", issue, nil
	}
	record, ok := intent.(delivery.Obj)
	if !ok {
		return "", issue, nil
	}
	text, _ := record.Get("baselineRevision").(string)
	return text, issue, nil
}

// observedTargetOf finds the one current target the child's integration ref names that has an observed head, and the newest head read
// there. The child's integration refs are the refs its plans recorded a batch for and the refs whose recorded heads include the child's
// baselineRevision (CRW-1374): the target is looked up by the ref, so a child of a later plan on the same integration ref and remote branch
// is found although the target row names the plan that recorded it first (a repeat of a recorded remote branch writes nothing). The targets
// the child's plans name themselves are still candidates. Zero targets with an observation, or more than one, find none: the comparison
// does not guess which target the child was started from, and an issue no live node of a plan carries compares nothing.
func (s *Scheduler) observedTargetOf(ctx context.Context, issue, baseline string) (store.IntegrationTargetRow, string, bool, error) {
	rows, err := s.Store.Q(ctx).QueryContext(ctx, "SELECT DISTINCT plan_id FROM dag_nodes WHERE issue_key = ? AND retired_rev IS NULL ORDER BY plan_id", issue)
	if err != nil {
		return store.IntegrationTargetRow{}, "", false, err
	}
	plans := map[string]bool{}
	for rows.Next() {
		var plan string
		if err := rows.Scan(&plan); err != nil {
			_ = rows.Close()
			return store.IntegrationTargetRow{}, "", false, err
		}
		plans[plan] = true
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return store.IntegrationTargetRow{}, "", false, err
	}
	if len(plans) == 0 {
		return store.IntegrationTargetRow{}, "", false, nil
	}
	refs, err := s.integrationRefsOfChild(ctx, plans, baseline)
	if err != nil {
		return store.IntegrationTargetRow{}, "", false, err
	}
	all, err := store.IntegrationTargets(ctx, s.Store)
	if err != nil {
		return store.IntegrationTargetRow{}, "", false, err
	}
	var found []store.IntegrationTargetRow
	var heads []string
	for _, t := range all {
		if !plans[t.PlanID] && !refs[t.IntegrationRef] {
			continue
		}
		latest, has, err := store.LatestIntegrationObservation(ctx, s.Store, t)
		if err != nil {
			return store.IntegrationTargetRow{}, "", false, err
		}
		if has {
			found, heads = append(found, t), append(heads, latest.ObservedHead)
		}
	}
	if len(found) != 1 {
		return store.IntegrationTargetRow{}, "", false, nil
	}
	return found[0], heads[0], true, nil
}

// integrationRefsOfChild are the integration refs a child belongs to: the refs a batch of one of its plans was recorded for, and the refs whose
// recorded heads (the bases of their batches and the heads their ref_moved stages name) include the child's baseline, the commit the child was
// started from. Both are read from the stage rows alone, without a git or network call.
func (s *Scheduler) integrationRefsOfChild(ctx context.Context, plans map[string]bool, baseline string) (map[string]bool, error) {
	all, batchRef, err := s.targetStageRows(ctx)
	if err != nil {
		return nil, err
	}
	refs := map[string]bool{}
	for _, r := range all {
		ref := batchRef[r.batch]
		if ref == "" {
			continue
		}
		if plans[r.plan] || (r.detail != "" && r.detail == baseline) {
			refs[ref] = true
		}
	}
	return refs, nil
}
