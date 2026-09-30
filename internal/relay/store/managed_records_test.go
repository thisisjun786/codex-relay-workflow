package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
)

func request(id, issue string) ManagedStartRequestsRow {
	return ManagedStartRequestsRow{RequestID: id, IssueKey: issue, RequestFingerprint: "fp", FingerprintVersion: "1", Workspace: "/w", MarkerRoot: "/m", SocketIdentity: "sock", CreateRequestID: "c-" + id, DispatchRequestID: "d-" + id, State: "attached", Revision: 7, CreatedAt: "t1", UpdatedAt: "t1"}
}

func TestManagedStart_reserves_one_pending_request_per_issue(t *testing.T) {
	// Given: a request reserved for CRW-1 whatever state the row claimed.
	s := recordStore(t)
	ctx := context.Background()
	must(t, s.ReserveManagedStart(ctx, request("req-1", "CRW-1")))
	row, err := s.ManagedStartRequest(ctx, "req-1")
	must(t, err)
	pending, err := s.PendingManagedStart(ctx, "CRW-1")
	must(t, err)
	// When: a second request for the same issue is reserved.
	second := s.ReserveManagedStart(ctx, request("req-2", "CRW-1"))
	// Then: it starts reserved at revision 0 with nothing attached, and the pending index refuses
	// a second pending request as the same IntegrityError Python raises (not a guard refusal).
	if row.State != "reserved" || row.Revision != 0 || row.ChildTaskID.Valid || row.RelationshipID.Valid || pending.RequestID != "req-1" {
		t.Fatalf("row=%+v pending=%v", row, pending.RequestID)
	}
	if second == nil || !strings.Contains(second.Error(), "UNIQUE constraint failed: managed_start_requests.issue_key") || RefusalReason(second) != "" {
		t.Fatalf("second pending request: %v", second)
	}
}

func TestManagedStart_moves_only_from_the_expected_state_and_revision(t *testing.T) {
	// Given: a reserved request.
	s := recordStore(t)
	ctx := context.Background()
	must(t, s.ReserveManagedStart(ctx, request("req-1", "CRW-1")))
	// When: it is armed at a stale revision, then the current one; released after arming; given a
	// receipt; attached once, then again.
	stale, err := s.ArmManagedStart(ctx, "req-1", 1, "t2")
	must(t, err)
	armed, err := s.ArmManagedStart(ctx, "req-1", 0, "t3")
	must(t, err)
	released, err := s.ReleaseManagedStart(ctx, "req-1", 1, "late", "t4")
	must(t, err)
	must(t, s.RecordManagedStartReceipt(ctx, "req-1", "accepted", text("child"), text("turn"), "t5"))
	attached, err := s.AttachManagedStart(ctx, "req-1", "rel-1", 1, "t6")
	must(t, err)
	again, err := s.AttachManagedStart(ctx, "req-1", "rel-2", 1, "t7")
	must(t, err)
	row, err := s.ManagedStartRequest(ctx, "req-1")
	must(t, err)
	// Then: each transition is compare-and-set, and the pending index releases the issue.
	if stale || !armed || released || !attached || again {
		t.Fatalf("stale=%v armed=%v released=%v attached=%v again=%v", stale, armed, released, attached, again)
	}
	if row.State != "attached" || row.Revision != 2 || row.RelationshipID.String != "rel-1" || row.ReceiptStatus.String != "accepted" || row.ChildTaskID.String != "child" {
		t.Fatalf("row=%+v", row)
	}
	if _, err := s.PendingManagedStart(ctx, "CRW-1"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("an attached request still pending: %v", err)
	}
}

func TestManagedStart_release_leaves_a_tombstone_that_frees_the_issue(t *testing.T) {
	// Given/When: a reserved request released at its revision.
	s := recordStore(t)
	ctx := context.Background()
	must(t, s.ReserveManagedStart(ctx, request("req-1", "CRW-1")))
	released, err := s.ReleaseManagedStart(ctx, "req-1", 0, "abandoned", "t2")
	must(t, err)
	// Then: the row stays as released, and a new request may take the issue but not the id.
	row, err := s.ManagedStartRequest(ctx, "req-1")
	must(t, err)
	if !released || row.State != "released" || row.ReleaseReason.String != "abandoned" || row.Revision != 1 {
		t.Fatalf("released=%v row=%+v", released, row)
	}
	must(t, s.ReserveManagedStart(ctx, request("req-2", "CRW-1")))
	if err := s.ReserveManagedStart(ctx, request("req-1", "CRW-2")); err == nil {
		t.Fatal("a released request id was reserved again")
	}
}

func TestReportingSessionAndTurnDeclaration_are_insert_once(t *testing.T) {
	// Given: a reporting session and a declaration recorded.
	s := recordStore(t)
	ctx := context.Background()
	must(t, s.RecordReportingSession(ctx, ReportingSessionsRow{AssignmentID: "a", SessionID: "s", DispatchRequestID: "d", MarkerRoot: "/m", Workspace: "/w", Capability: "declarations/1", RecordedAt: "t"}))
	must(t, s.RecordTurnDeclaration(ctx, TurnDeclarationsRow{AssignmentID: "a", SessionID: "s", TurnID: "turn", Outcome: "done", DeclaredAt: "t1", RecordedAt: "t2"}))
	// When: either is recorded again.
	session := s.RecordReportingSession(ctx, ReportingSessionsRow{AssignmentID: "a", SessionID: "s", DispatchRequestID: "d2", MarkerRoot: "/m", Workspace: "/w", Capability: "declarations/1", RecordedAt: "t"})
	declared := s.RecordTurnDeclaration(ctx, TurnDeclarationsRow{AssignmentID: "a", SessionID: "s", TurnID: "turn", Outcome: "blocked", DeclaredAt: "t1", RecordedAt: "t2"})
	// Then: the key refuses the second record and the first stands (declarations.py reads first).
	row, err := s.ReportingSession(ctx, "a", "s")
	must(t, err)
	turn, err := s.TurnDeclaration(ctx, "a", "s", "turn")
	must(t, err)
	if session == nil || declared == nil || row.DispatchRequestID != "d" || row.IssueKey.Valid || turn.Outcome != "done" {
		t.Fatalf("session=%v declared=%v row=%+v turn=%+v", session, declared, row, turn)
	}
}
