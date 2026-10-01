package registry

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"

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
		return refuse(contract.RefusalUnregisteredRelationship, "managed request %s is not reserved on this store", strconv.Quote(in.ManagedRequestID))
	}
	if err != nil {
		return err
	}
	if named.IssueKey != in.IssueKey {
		return refuse(contract.RefusalRelationshipConflict, "managed request %s is for issue %s, not %s", strconv.Quote(in.ManagedRequestID), strconv.Quote(named.IssueKey), strconv.Quote(in.IssueKey))
	}
	if named.State == "attached" {
		if named.DispatchRequestID != in.DispatchRequestID || named.ChildTaskID.String != in.Child.TaskID || named.StandbyTurnID.String != in.DispatchTurnID.String {
			return refuse(contract.RefusalRelationshipConflict, "attached request %s does not match its published child, standby and dispatch", strconv.Quote(in.ManagedRequestID))
		}
		return nil
	}
	if named.State != "create_armed" || named.ReceiptStatus.String != "accepted" {
		extra := ""
		if named.ReceiptStatus.Valid {
			extra = fmt.Sprintf(" with receipt %q", named.ReceiptStatus.String)
		}
		return refuse(contract.RefusalRelationshipConflict, "managed request %s is %s%s; attaching it needs an armed request and an accepted receipt", strconv.Quote(in.ManagedRequestID), strconv.Quote(named.State), extra)
	}
	if named.DispatchRequestID != in.DispatchRequestID {
		return refuse(contract.RefusalRelationshipConflict, "managed request %s retained dispatch %s, not %s", strconv.Quote(in.ManagedRequestID), strconv.Quote(named.DispatchRequestID), strconv.Quote(in.DispatchRequestID))
	}
	if named.ChildTaskID.String != in.Child.TaskID {
		return refuse(contract.RefusalRelationshipConflict, "managed request %s published child %s, not %s", strconv.Quote(in.ManagedRequestID), strconv.Quote(named.ChildTaskID.String), strconv.Quote(in.Child.TaskID))
	}
	if named.StandbyTurnID.String != in.DispatchTurnID.String {
		return refuse(contract.RefusalRelationshipConflict, "managed request %s published standby %s, not %s", strconv.Quote(in.ManagedRequestID), strconv.Quote(named.StandbyTurnID.String), strconv.Quote(in.DispatchTurnID.String))
	}
	pending, err := r.Store.PendingManagedStart(ctx, in.IssueKey)
	if err != nil {
		return err
	}
	if pending.RequestID != in.ManagedRequestID {
		return refuse(contract.RefusalDuplicateAssignment, "issue %s is already held by request %s (%s)", strconv.Quote(in.IssueKey), strconv.Quote(pending.RequestID), pending.State)
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
		return refuse(contract.RefusalRelationshipConflict, "managed request %s could not be attached", strconv.Quote(id))
	}
	return journal(ctx, r.Store, "managed_start_attached", id, contract.OrderedObject{{Key: "relationshipId", Value: rid}, {Key: "executionGeneration", Value: 1}}, now)
}
