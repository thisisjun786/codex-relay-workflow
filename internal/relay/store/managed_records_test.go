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

func TestAuthorizedSettingsAndViolations_keep_the_latest_record(t *testing.T) {
	// Given/When: settings and a violation, each recorded twice.
	s := recordStore(t)
	ctx := context.Background()
	must(t, s.RecordAuthorizedSettings(ctx, AuthorizedSettingsRow{TaskID: "task", Settings: `{"a":1}`, Source: "creation_result", RecordedAt: "t1"}))
	must(t, s.RecordAuthorizedSettings(ctx, AuthorizedSettingsRow{TaskID: "task", Settings: `{"a":2}`, Source: "resume", RecordedAt: "t2"}))
	must(t, s.RecordSettingsViolation(ctx, AttemptSettingsViolationsRow{RequestID: "req", EventID: "e1", Findings: "[1]", ObservedAt: "t1"}))
	must(t, s.RecordSettingsViolation(ctx, AttemptSettingsViolationsRow{RequestID: "req", EventID: "e2", Findings: "[2]", ObservedAt: "t2"}))
	settings, err := s.AuthorizedSettings(ctx, "task")
	must(t, err)
	violation, err := s.SettingsViolation(ctx, "req")
	must(t, err)
	// Then: settings are replaced whole; a violation replaces findings but keeps its event.
	if settings.Settings != `{"a":2}` || settings.Source != "resume" || violation.Findings != "[2]" || violation.EventID != "e1" || violation.ObservedAt != "t2" {
		t.Fatalf("settings=%+v violation=%+v", settings, violation)
	}
}

func TestCanonicalCriteria_replace_the_whole_set(t *testing.T) {
	// Given: a set of two criteria, replaced by a set of one inside a transaction.
	s := recordStore(t)
	ctx := context.Background()
	must(t, s.Transaction(ctx, func(ctx context.Context, _ *sql.Conn) error {
		return s.ReplaceCanonicalCriteria(ctx, "rel", []CanonicalCriteriaRow{
			{CriterionID: "c2", Title: "two", Required: 1, SetDigest: "d1", RecordedAt: "t1"},
			{CriterionID: "c1", Title: "one", Required: 0, SourceRef: text("doc"), SetDigest: "d1", RecordedAt: "t1"},
		})
	}))
	first, err := s.CanonicalCriteria(ctx, "rel")
	must(t, err)
	must(t, s.Transaction(ctx, func(ctx context.Context, _ *sql.Conn) error {
		return s.ReplaceCanonicalCriteria(ctx, "rel", []CanonicalCriteriaRow{{CriterionID: "c3", Title: "three", Required: 1, SetDigest: "d2", RecordedAt: "t2"}})
	}))
	second, err := s.CanonicalCriteria(ctx, "rel")
	must(t, err)
	// Then: listings are by criterion id, and a replacement leaves nothing of the old set.
	if len(first) != 2 || first[0].CriterionID != "c1" || first[0].SourceRef.String != "doc" || len(second) != 1 || second[0].SetDigest != "d2" {
		t.Fatalf("first=%v second=%v", first, second)
	}
}

func TestVerificationModeAndClaimContext_upsert_and_bind_once(t *testing.T) {
	// Given: a mode written twice and a claim bound twice, then cleared and rebound.
	s := recordStore(t)
	ctx := context.Background()
	must(t, s.WriteVerificationMode(ctx, "rel", "legacy", "t1"))
	must(t, s.WriteVerificationMode(ctx, "rel", "managed", "t2"))
	must(t, s.BindClaimContext(ctx, "e1", text("d1"), "t1"))
	must(t, s.BindClaimContext(ctx, "e1", text("d2"), "t2"))
	bound, err := s.ClaimContext(ctx, "e1")
	must(t, err)
	must(t, s.ClearClaimContext(ctx, "e1"))
	must(t, s.BindClaimContext(ctx, "e1", sql.NullString{}, "t3"))
	rebound, err := s.ClaimContext(ctx, "e1")
	must(t, err)
	mode, err := s.VerificationMode(ctx, "rel")
	must(t, err)
	// Then: the mode is the latest; the first binding stands until cleared.
	if mode.Mode != "managed" || mode.RecordedAt != "t2" || bound.SetDigest.String != "d1" || rebound.SetDigest.Valid || rebound.BoundAt != "t3" {
		t.Fatalf("mode=%+v bound=%+v rebound=%+v", mode, bound, rebound)
	}
}

func TestVerdictContextAndAssignmentMarks_replace_on_rereview(t *testing.T) {
	// Given: a verdict context and a mark, each written twice.
	s := recordStore(t)
	ctx := context.Background()
	must(t, s.RecordVerdictContext(ctx, VerdictContextRow{EventID: "e1", SetDigest: text("d1"), Coverage: "[]", Currency: "current", AckEvidence: "{}", RecordedAt: "t1"}))
	must(t, s.RecordVerdictContext(ctx, VerdictContextRow{EventID: "e1", Coverage: "[1]", Currency: "stale", HeadEventID: text("e2"), AckEvidence: "{}", RecordedAt: "t2"}))
	mark := AssignmentMarksRow{RelationshipID: "rel", Mark: "integrated", EventID: "e1", ExecutionGeneration: 1, RevisionHash: "h1", Evidence: "pr1", Actor: "a", MarkedAt: "t1"}
	must(t, s.RecordAssignmentMark(ctx, mark))
	mark.Evidence, mark.RevisionHash, mark.MarkedAt = "pr2", "ignored", "t2"
	must(t, s.RecordAssignmentMark(ctx, mark))
	must(t, s.RecordAssignmentMark(ctx, AssignmentMarksRow{RelationshipID: "rel", Mark: "reviewed", EventID: "e0", ExecutionGeneration: 1, RevisionHash: "h0", Evidence: "x", Actor: "a", MarkedAt: "t0"}))
	context_, err := s.VerdictContext(ctx, "e1")
	must(t, err)
	marks, err := s.AssignmentMarks(ctx, "rel")
	must(t, err)
	// Then: a re-review replaces every context column; a mark updates evidence, actor and time only.
	if context_.SetDigest.Valid || context_.Coverage != "[1]" || context_.HeadEventID.String != "e2" || context_.RecordedAt != "t2" {
		t.Fatalf("context=%+v", context_)
	}
	if len(marks) != 2 || marks[0].Mark != "reviewed" || marks[1].Evidence != "pr2" || marks[1].RevisionHash != "h1" {
		t.Fatalf("marks=%+v", marks)
	}
}

func TestDeliveryIntent_keeps_its_first_note_and_leaves_pending_once_delivered(t *testing.T) {
	// Given: two intents due at 10 and 30, one noted twice, and a delivery for another.
	s := recordStore(t)
	ctx := context.Background()
	intent := DeliveryIntentRow{EventID: "e1", RelationshipID: "rel", Kind: "completion_event", RecipientTaskID: "p", Attempts: 1, NextRetryAt: sql.NullFloat64{Float64: 10, Valid: true}, LastError: text("first"), NotedAt: "t1"}
	must(t, s.NoteDeliveryIntent(ctx, intent))
	intent.Attempts, intent.LastError, intent.NotedAt, intent.Kind = 2, text("second"), "t2", "ignored"
	must(t, s.NoteDeliveryIntent(ctx, intent))
	must(t, s.NoteDeliveryIntent(ctx, DeliveryIntentRow{EventID: "e2", RelationshipID: "rel", Kind: "k", RecipientTaskID: "p", Attempts: 1, NextRetryAt: sql.NullFloat64{Float64: 30, Valid: true}, NotedAt: "t1"}))
	noted, err := s.DeliveryIntent(ctx, "e1")
	must(t, err)
	due, err := s.PendingIntents(ctx, 20, 4)
	must(t, err)
	// When: e1 gets a delivery row, and e2 is cleared.
	must(t, s.RecordDelivery(ctx, Delivery{EventID: "e1", RelationshipID: "rel", Kind: "completion_event", RecipientTaskID: "p", RecipientThreadID: "p", State: "queued", CreatedAt: "t", UpdatedAt: "t"}))
	must(t, s.ClearDeliveryIntent(ctx, "e2"))
	after, err := s.PendingIntents(ctx, 100, 4)
	must(t, err)
	// Then: a re-note updates attempts, retry and error only; delivered and cleared ones leave.
	if noted.Attempts != 2 || noted.LastError.String != "second" || noted.NotedAt != "t1" || noted.Kind != "completion_event" {
		t.Fatalf("noted=%+v", noted)
	}
	if len(due) != 1 || due[0].EventID != "e1" || len(after) != 0 {
		t.Fatalf("due=%v after=%v", due, after)
	}
}

func TestPollObservation_keeps_the_last_successful_poll_through_a_failed_read(t *testing.T) {
	// Given: a successful poll, then a failed read of the same anchor.
	s := recordStore(t)
	ctx := context.Background()
	must(t, s.RecordPoll(ctx, PollObservationsRow{RelationshipID: "rel", ExecutionGeneration: 1, TurnID: "turn", LastStatus: text("inProgress"), LastPolledAt: text("t1"), LastAttemptAt: "t1"}))
	must(t, s.RecordPoll(ctx, PollObservationsRow{RelationshipID: "rel", ExecutionGeneration: 1, TurnID: "turn", LastAttemptAt: "t2", LastError: text("OSError: gone")}))
	// When/Then: the poll time survives, the attempt and error move.
	row, err := s.PollObservation(ctx, "rel", 1, "turn")
	if err != nil || row.LastPolledAt.String != "t1" || row.LastAttemptAt != "t2" || row.LastError.String != "OSError: gone" || row.LastStatus.Valid {
		t.Fatalf("row=%+v err=%v", row, err)
	}
}

func TestRecipientRate_counts_sends_per_window(t *testing.T) {
	// Given: two sends in window 3600 and one in 7200.
	s := recordStore(t)
	ctx := context.Background()
	must(t, s.CountSend(ctx, "p", 3600, 3601))
	must(t, s.CountSend(ctx, "p", 3600, 3700))
	must(t, s.CountSend(ctx, "p", 7200, 7300))
	// When: the windows are read.
	sends, err := s.WindowSends(ctx, "p", 3600)
	must(t, err)
	none, err := s.WindowSends(ctx, "p", 0)
	must(t, err)
	last, err := s.LastSend(ctx, "p", 0, 3600)
	must(t, err)
	row, err := s.RecipientRate(ctx, "p", 7200)
	must(t, err)
	// Then: sends accumulate per window, an empty window counts zero, and the last send is bounded.
	if sends != 2 || none != 0 || last.Float64 != 3700 || row.Sends != 1 {
		t.Fatalf("sends=%d none=%d last=%v row=%+v", sends, none, last, row)
	}
}

func TestRecipientLifecycle_keeps_the_latest_observation(t *testing.T) {
	// Given/When: a recipient observed deliverable, then withheld with unknown fields.
	s := recordStore(t)
	ctx := context.Background()
	must(t, s.RecordRecipientLifecycle(ctx, RecipientLifecycleRow{TaskID: "p", RuntimeStatus: text("idle"), Archived: sql.NullInt64{Valid: true}, Deliverable: "yes", ObservedAt: "t1"}))
	must(t, s.RecordRecipientLifecycle(ctx, RecipientLifecycleRow{TaskID: "p", Deliverable: "no", WithholdReason: text("archived"), ObservedAt: "t2"}))
	// Then: every column is the latest, including the NULLs.
	row, err := s.RecipientLifecycle(ctx, "p")
	if err != nil || row.Deliverable != "no" || row.WithholdReason.String != "archived" || row.RuntimeStatus.Valid || row.Archived.Valid || row.ObservedAt != "t2" {
		t.Fatalf("row=%+v err=%v", row, err)
	}
}

func TestOvertakenDelivery_is_judged_once(t *testing.T) {
	// Given/When: one delivery judged overtaken twice.
	s := recordStore(t)
	ctx := context.Background()
	must(t, s.NoteOvertakenDelivery(ctx, "e1", "later generation", "t1"))
	must(t, s.NoteOvertakenDelivery(ctx, "e1", "other", "t2"))
	// Then: the first judgement stands.
	row, err := s.OvertakenDelivery(ctx, "e1")
	if err != nil || row.Reason != "later generation" || row.NotedAt != "t1" {
		t.Fatalf("row=%+v err=%v", row, err)
	}
}
