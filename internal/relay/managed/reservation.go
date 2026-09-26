package managed

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// Reservation implements registry.py's durable managed request exclusion on one store.
// This subset is ported for todo 27; registry's managed admission extends it.
type Reservation struct {
	Store *store.Store
	Now   func() string
}
type Identity struct{ RequestID, IssueKey, Fingerprint, Version, Workspace, MarkerRoot, SocketIdentity, CreateRequestID, DispatchRequestID string }

func (r Reservation) now() string {
	if r.Now != nil {
		return r.Now()
	}
	return ""
}
func refusal(reason, detail string) error { return &store.RefusedError{Reason: reason, Detail: detail} }
func (r Reservation) get(ctx context.Context, id string) (store.ManagedStartRequestsRow, error) {
	return r.Store.ManagedStartRequest(ctx, id)
}
func (r Reservation) Reserve(ctx context.Context, in Identity) (out store.ManagedStartRequestsRow, err error) {
	err = r.Store.Transaction(ctx, func(ctx context.Context, _ *sql.Conn) error {
		current, e := r.get(ctx, in.RequestID)
		if e == nil {
			if current.RequestFingerprint != in.Fingerprint || current.IssueKey != in.IssueKey || current.Workspace != in.Workspace || current.MarkerRoot != in.MarkerRoot || current.SocketIdentity != in.SocketIdentity || current.CreateRequestID != in.CreateRequestID || current.DispatchRequestID != in.DispatchRequestID || current.FingerprintVersion != in.Version {
				return refusal("relationship_conflict", fmt.Sprintf("request %s already retained request_fingerprint %s, not %s", store.PythonRepr(in.RequestID), store.PythonRepr(current.RequestFingerprint), store.PythonRepr(in.Fingerprint)))
			}
			if current.State == "released" {
				return refusal("relationship_conflict", fmt.Sprintf("request %s was released and cannot be reused", store.PythonRepr(in.RequestID)))
			}
			out = current
			return nil
		}
		if !errors.Is(e, sql.ErrNoRows) {
			return e
		}
		live, e := r.Store.One(ctx, "SELECT relationship_id,status FROM relationships WHERE issue_key=? AND status IN ('active','paused') AND superseded_by IS NULL", in.IssueKey)
		if e != nil {
			return e
		}
		if live != nil {
			return refusal("duplicate_assignment", fmt.Sprintf("issue %s is already assigned under %s (%s); a reservation cannot take it", store.PythonRepr(in.IssueKey), store.PythonRepr(fmt.Sprint(live.Get("relationship_id"))), live.Get("status")))
		}
		other, e := r.Store.PendingManagedStart(ctx, in.IssueKey)
		if e == nil {
			return refusal("duplicate_assignment", fmt.Sprintf("issue %s is already held by request %s (%s)", store.PythonRepr(in.IssueKey), store.PythonRepr(other.RequestID), other.State))
		}
		if !errors.Is(e, sql.ErrNoRows) {
			return e
		}
		at := r.now()
		e = r.Store.ReserveManagedStart(ctx, store.ManagedStartRequestsRow{RequestID: in.RequestID, IssueKey: in.IssueKey, RequestFingerprint: in.Fingerprint, FingerprintVersion: in.Version, Workspace: in.Workspace, MarkerRoot: in.MarkerRoot, SocketIdentity: in.SocketIdentity, CreateRequestID: in.CreateRequestID, DispatchRequestID: in.DispatchRequestID, CreatedAt: at, UpdatedAt: at})
		if e != nil {
			return e
		}
		out, e = r.get(ctx, in.RequestID)
		return e
	})
	return
}
func (r Reservation) Arm(ctx context.Context, id, fp string, revision int64) (out store.ManagedStartRequestsRow, err error) {
	err = r.Store.Transaction(ctx, func(ctx context.Context, _ *sql.Conn) error {
		row, e := r.get(ctx, id)
		if e != nil {
			return e
		}
		if row.RequestFingerprint != fp {
			return refusal("relationship_conflict", "managed request fingerprint changed")
		}
		if row.State == "create_armed" && row.Revision == revision+1 {
			out = row
			return nil
		}
		if row.State != "reserved" || row.Revision != revision {
			return refusal("relationship_conflict", fmt.Sprintf("request %q is %q at revision %d, not reserved at %d", id, row.State, row.Revision, revision))
		}
		changed, e := r.Store.ArmManagedStart(ctx, id, revision, r.now())
		if e != nil {
			return e
		}
		if !changed {
			return refusal("relationship_conflict", "managed request changed before it could be armed")
		}
		out, e = r.get(ctx, id)
		return e
	})
	return
}
func (r Reservation) Release(ctx context.Context, id, fp string, revision int64, reason string) (out store.ManagedStartRequestsRow, err error) {
	err = r.Store.Transaction(ctx, func(ctx context.Context, _ *sql.Conn) error {
		row, e := r.get(ctx, id)
		if e != nil {
			return e
		}
		if row.RequestFingerprint != fp {
			return refusal("relationship_conflict", "managed request fingerprint changed")
		}
		if strings.TrimSpace(reason) == "" {
			return fmt.Errorf("reason must be nonblank text")
		}
		if row.State != "reserved" || row.Revision != revision {
			return refusal("relationship_conflict", fmt.Sprintf("request %s is %s at revision %d; only a reserved row at revision %d can be released", store.PyRepr(id), store.PyRepr(row.State), row.Revision, revision))
		}
		changed, e := r.Store.ReleaseManagedStart(ctx, id, revision, reason, r.now())
		if e != nil {
			return e
		}
		if !changed {
			return refusal("relationship_conflict", "managed request changed before it could be released")
		}
		out, e = r.get(ctx, id)
		return e
	})
	return
}
func (r Reservation) Receipt(ctx context.Context, id, fp string, receipt map[string]any) (out store.ManagedStartRequestsRow, err error) {
	status, ok := receipt["status"].(string)
	if !ok || strings.TrimSpace(status) == "" {
		return out, refusal("malformed_receipt", "a start receipt needs a status")
	}
	child, _ := receipt["threadId"].(string)
	turn, _ := receipt["turnId"].(string)
	if status != "accepted" || strings.TrimSpace(child) == "" || strings.TrimSpace(turn) == "" {
		child = ""
		turn = ""
		if status == "accepted" {
			status = "partial"
		}
	}
	err = r.Store.Transaction(ctx, func(ctx context.Context, _ *sql.Conn) error {
		row, e := r.get(ctx, id)
		if e != nil {
			return e
		}
		if row.RequestFingerprint != fp {
			return refusal("relationship_conflict", "managed request fingerprint changed")
		}
		if row.State == "attached" || row.ReceiptStatus.Valid && row.ReceiptStatus.String == "accepted" {
			if status == "accepted" && row.ChildTaskID.String == child && row.StandbyTurnID.String == turn {
				out = row
				return nil
			}
			return refusal("relationship_conflict", fmt.Sprintf("request %q already published child %q and standby %q", id, row.ChildTaskID.String, row.StandbyTurnID.String))
		}
		if row.State != "create_armed" {
			return refusal("relationship_conflict", fmt.Sprintf("request %q is %q; a receipt is recorded only while the request is create_armed", id, row.State))
		}
		e = r.Store.RecordManagedStartReceipt(ctx, id, status, sql.NullString{String: child, Valid: child != ""}, sql.NullString{String: turn, Valid: turn != ""}, r.now())
		if e != nil {
			return e
		}
		out, e = r.get(ctx, id)
		return e
	})
	return
}
