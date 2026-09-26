package registry

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
)

// Subset ported for todo 27; registry owns and extends this. The receipt and relationship
// must move together: the transaction's ctx is used for both reads and the attachment.
func (r *Registry) guardManagedRegistration(ctx context.Context, in Registration) error {
	if in.ManagedRequestID == "" {
		return r.guardIssueReservation(ctx, in.IssueKey)
	}
	named, err := r.Store.ManagedStartRequest(ctx, in.ManagedRequestID)
	if errors.Is(err, sql.ErrNoRows) {
		return refuse(contract.RefusalUnregisteredRelationship, "managed request %s is not reserved on this store", pyStr(in.ManagedRequestID))
	}
	if err != nil {
		return err
	}
	if named.IssueKey != in.IssueKey {
		return refuse(contract.RefusalRelationshipConflict, "managed request %s is for issue %s, not %s", pyStr(in.ManagedRequestID), pyStr(named.IssueKey), pyStr(in.IssueKey))
	}
	if named.State == "attached" {
		if named.DispatchRequestID != in.DispatchRequestID || named.ChildTaskID.String != in.Child.TaskID || named.StandbyTurnID.String != in.DispatchTurnID.String {
			return refuse(contract.RefusalRelationshipConflict, "attached request %s does not match its published child, standby and dispatch", pyStr(in.ManagedRequestID))
		}
		return nil
	}
	if named.State != "create_armed" || named.ReceiptStatus.String != "accepted" {
		extra := ""
		if named.ReceiptStatus.Valid {
			extra = fmt.Sprintf(" with receipt %s", pyStr(named.ReceiptStatus.String))
		}
		return refuse(contract.RefusalRelationshipConflict, "managed request %s is %s%s; attaching it needs an armed request and an accepted receipt", pyStr(in.ManagedRequestID), pyStr(named.State), extra)
	}
	if named.DispatchRequestID != in.DispatchRequestID {
		return refuse(contract.RefusalRelationshipConflict, "managed request %s retained dispatch %s, not %s", pyStr(in.ManagedRequestID), pyStr(named.DispatchRequestID), pyStr(in.DispatchRequestID))
	}
	if named.ChildTaskID.String != in.Child.TaskID {
		return refuse(contract.RefusalRelationshipConflict, "managed request %s published child %s, not %s", pyStr(in.ManagedRequestID), pyStr(named.ChildTaskID.String), pyStr(in.Child.TaskID))
	}
	if named.StandbyTurnID.String != in.DispatchTurnID.String {
		return refuse(contract.RefusalRelationshipConflict, "managed request %s published standby %s, not %s", pyStr(in.ManagedRequestID), pyStr(named.StandbyTurnID.String), pyStr(in.DispatchTurnID.String))
	}
	pending, err := r.Store.PendingManagedStart(ctx, in.IssueKey)
	if err != nil {
		return err
	}
	if pending.RequestID != in.ManagedRequestID {
		return refuse(contract.RefusalDuplicateAssignment, "issue %s is already held by request %s (%s)", pyStr(in.IssueKey), pyStr(pending.RequestID), pending.State)
	}
	return nil
}
func (r *Registry) attachManagedRegistration(ctx context.Context, id, rid, now string) error {
	if id == "" {
		return nil
	}
	changed, err := r.Store.AttachManagedStart(ctx, id, rid, 1, now)
	if err != nil {
		return err
	}
	if !changed {
		return refuse(contract.RefusalRelationshipConflict, "managed request %s could not be attached", pyStr(id))
	}
	return journal(ctx, r.Store, "managed_start_attached", id, contract.OrderedObject{{Key: "relationshipId", Value: rid}, {Key: "executionGeneration", Value: 1}}, now)
}
