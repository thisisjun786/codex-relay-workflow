package managed

import (
	"bytes"
	"context"
	"database/sql"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// startResult is managed.py:273's public receipt. A business turn is only reported
// after its retained bridge receipt has been admitted, never on an unknown send.
type startResult struct {
	RequestID, AssignmentID, ChildTaskID, BusinessTurnID                             string
	StatePath, MarkerRoot, Workspace                                                 string
	RelationshipID, StandbyTurnID, CreationRequestID, BusinessRequestID, Fingerprint string
	Generation, Revision                                                             any
	Ledger                                                                           any
	ReservationState                                                                 any
	Observed                                                                         contract.OrderedObject
}

func (r startResult) result(state, stage string, reason any) contract.OrderedObject {
	nullable := func(s string) any {
		if s == "" {
			return nil
		}
		return s
	}
	out := contract.OrderedObject{
		{Key: "schema", Value: Schema}, {Key: "requestId", Value: r.RequestID},
		{Key: "state", Value: state}, {Key: "stage", Value: stage}, {Key: "reason", Value: reason},
		{Key: "assignmentId", Value: r.AssignmentID}, {Key: "relationshipId", Value: nullable(r.RelationshipID)},
		{Key: "executionGeneration", Value: r.Generation}, {Key: "childTaskId", Value: nullable(r.ChildTaskID)},
		{Key: "standbyTurnId", Value: nullable(r.StandbyTurnID)},
		{Key: "creationRequestId", Value: r.CreationRequestID}, {Key: "businessRequestId", Value: r.BusinessRequestID},
		{Key: "requestFingerprint", Value: r.Fingerprint}, {Key: "ledger", Value: r.Ledger},
		{Key: "reservationState", Value: r.ReservationState}, {Key: "reservationRevision", Value: r.Revision},
		{Key: "recovery", Value: "Retry only this same complete request; do not create a replacement."},
		{Key: "selectors", Value: contract.OrderedObject{{Key: "state", Value: r.StatePath}, {Key: "markerRoot", Value: r.MarkerRoot}, {Key: "workspace", Value: r.Workspace}}},
	}
	out = append(out, r.Observed...)
	if r.BusinessTurnID != "" {
		out = append(out, contract.Field{Key: "businessTurnId", Value: r.BusinessTurnID}, contract.Field{Key: "childClaim", Value: "not_observed"}, contract.Field{Key: "hookFiring", Value: "not_observed"})
		if r.AssignmentID != "" && r.ChildTaskID != "" && validReportingSegment(r.BusinessTurnID) && validReportingSegment(r.ChildTaskID) {
			out = append(out, contract.Field{Key: "reportingArgv", Value: []any{"--state", r.StatePath, "reporting-show", "--marker-root", r.MarkerRoot, "--workspace", r.Workspace, "--assignment", r.AssignmentID, "--session", r.ChildTaskID, "--turn", r.BusinessTurnID}})
		}
	}
	return out
}
func validReportingSegment(value string) bool { return delivery.ValidSegment(value) }

// observe persists the full public receipt, including refusals, without retaining a prompt.
func (r startResult) Observe(ctx context.Context, s *store.Store, at, state, stage string, reason any) (contract.OrderedObject, error) {
	out := r.result(state, stage, reason)
	if reason == "" {
		for i := range out {
			if out[i].Key == "reason" {
				out[i].Value = nil
				break
			}
		}
	}
	err := s.Transaction(ctx, func(ctx context.Context, _ *sql.Conn) error {
		var encoded bytes.Buffer
		if err := contract.Emit(&encoded, out); err != nil {
			return err
		}
		data := bytes.TrimSpace(encoded.Bytes())
		_, err := s.Querier(ctx).ExecContext(ctx, "INSERT INTO journal(at,kind,subject,detail) VALUES(?,?,?,?)", at, "managed_start_observed", r.RequestID, string(data))
		return err
	})
	return out, err
}

func NewStartResult(id Identity, row store.ManagedStartRequestsRow, assignment, state string) startResult {
	return startResult{RequestID: id.RequestID, AssignmentID: assignment, ChildTaskID: row.ChildTaskID.String, StatePath: state, MarkerRoot: id.MarkerRoot, Workspace: id.Workspace, RelationshipID: row.RelationshipID.String, StandbyTurnID: row.StandbyTurnID.String, CreationRequestID: id.CreateRequestID, BusinessRequestID: id.DispatchRequestID, Fingerprint: id.Fingerprint, Generation: nullableInt(row.ExecutionGeneration), Revision: row.Revision, ReservationState: row.State}
}
