package managed

import (
	"database/sql"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

func nullableString(s sql.NullString) any {
	if s.Valid {
		return s.String
	}
	return nil
}
func nullableInt(s sql.NullInt64) any {
	if s.Valid {
		return s.Int64
	}
	return nil
}

// reservationRecord is registry.py:1551's complete public managed reservation.
func reservationRecord(r store.ManagedStartRequestsRow) contract.OrderedObject {
	return contract.OrderedObject{
		{Key: "request_id", Value: r.RequestID}, {Key: "issue_key", Value: r.IssueKey}, {Key: "request_fingerprint", Value: r.RequestFingerprint}, {Key: "fingerprint_version", Value: r.FingerprintVersion},
		{Key: "workspace", Value: r.Workspace}, {Key: "marker_root", Value: r.MarkerRoot}, {Key: "socket_identity", Value: r.SocketIdentity}, {Key: "create_request_id", Value: r.CreateRequestID}, {Key: "dispatch_request_id", Value: r.DispatchRequestID},
		{Key: "state", Value: r.State}, {Key: "revision", Value: r.Revision}, {Key: "child_task_id", Value: nullableString(r.ChildTaskID)}, {Key: "standby_turn_id", Value: nullableString(r.StandbyTurnID)},
		{Key: "relationship_id", Value: nullableString(r.RelationshipID)}, {Key: "execution_generation", Value: nullableInt(r.ExecutionGeneration)}, {Key: "receipt_status", Value: nullableString(r.ReceiptStatus)},
		{Key: "release_reason", Value: nullableString(r.ReleaseReason)}, {Key: "created_at", Value: r.CreatedAt}, {Key: "updated_at", Value: r.UpdatedAt},
	}
}
