package daemon

import (
	"context"
	"database/sql"
	"path/filepath"
	"slices"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

func standbyStore(t *testing.T) (context.Context, *store.Store) {
	t.Helper()
	ctx := context.Background()
	s, err := fixtureStore(ctx, filepath.Join(t.TempDir(), "relay.sqlite3"), "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	seed(t, s, "r", "parent", "child", "anchor")
	return ctx, s
}

// Attach through the store operations used by managed registration. The original
// generation/child/dispatch/standby association distinguishes preparation from work.
func attachStandby(t *testing.T, ctx context.Context, s *store.Store) {
	t.Helper()
	m := store.ManagedStartRequestsRow{RequestID: "managed", IssueKey: "REL-1", RequestFingerprint: "fp", FingerprintVersion: "v1", Workspace: "/child", MarkerRoot: "/markers", SocketIdentity: "socket", CreateRequestID: "create", DispatchRequestID: "dispatch-r", CreatedAt: "2023-11-14T22:13:20Z", UpdatedAt: "2023-11-14T22:13:20Z"}
	if err := s.ReserveManagedStart(ctx, m); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.ArmManagedStart(ctx, m.RequestID, 0, m.CreatedAt); err != nil || !ok {
		t.Fatalf("arm: %v %v", ok, err)
	}
	if err := s.RecordManagedStartReceipt(ctx, m.RequestID, "accepted", sql.NullString{String: "child", Valid: true}, sql.NullString{String: "anchor", Valid: true}, m.CreatedAt); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.AttachManagedStart(ctx, m.RequestID, "r", 1, m.CreatedAt); err != nil || !ok {
		t.Fatalf("attach: %v %v", ok, err)
	}
}

func standbyDaemon(s *store.Store, h *observationHost) *Daemon {
	d := New(s, h, &delivery.FakeClock{T: 1700000000}, nil)
	d.Policy.MaxTurnReads, d.Policy.MaxSends = 8, -1
	return d
}

func noStandbySettlement(t *testing.T, s *store.Store) {
	t.Helper()
	for _, table := range []string{"observations", "assignment_settlements", "events"} {
		if n := count(t, s, "SELECT COUNT(*) FROM "+table+" WHERE turn_id='anchor'"); n != 0 {
			t.Errorf("inert standby left %d %s rows", n, table)
		}
	}
}

func TestManagedStandbyIsNotFailedWorkBeforeOrDuringBusiness(t *testing.T) {
	for _, status := range []string{"interrupted", "failed"} {
		t.Run(status, func(t *testing.T) {
			ctx, s := standbyStore(t)
			attachStandby(t, ctx, s)
			h := &observationHost{statuses: map[string]string{"anchor": status, "business": "inProgress"}}
			d := standbyDaemon(s, h)
			if report, err := d.Tick(ctx); err != nil || report.Observed != 0 {
				t.Fatalf("before business: observed=%d err=%v", report.Observed, err)
			}
			noStandbySettlement(t, s)
			exec(t, s, admit("business", "explicit_admission_bound:anchor"))
			for range 3 {
				if report, err := d.Tick(ctx); err != nil || report.Observed != 0 {
					t.Fatalf("running business: observed=%d err=%v", report.Observed, err)
				}
			}
			noStandbySettlement(t, s)
			if !slices.Equal(h.reads, []string{"business", "business", "business"}) || h.statuses["anchor"] != status {
				t.Fatalf("standby polled or status changed: %v %v", h.reads, h.statuses)
			}
			// A genuine business failure still produces a parent delivery, with its
			// actual terminal status. It does not settle the standby beside it.
			h.statuses["business"] = status
			if _, err := d.Tick(ctx); err != nil {
				t.Fatal(err)
			}
			if n := count(t, s, "SELECT COUNT(*) FROM events WHERE producer='daemon_observation' AND turn_id='business' AND outcome=?", status); n != 1 {
				t.Fatalf("genuine business failure events: %d", n)
			}
			if n := count(t, s, "SELECT COUNT(*) FROM deliveries d JOIN events e ON e.event_id=d.event_id WHERE e.turn_id='business'"); n != 1 {
				t.Fatalf("business failure deliveries: %d", n)
			}
			noStandbySettlement(t, s)
		})
	}
}

func TestManagedStandbyExclusionKeepsOrdinaryAndRevisionAnchors(t *testing.T) {
	for _, revision := range []bool{false, true} {
		t.Run(map[bool]string{false: "ordinary", true: "revision"}[revision], func(t *testing.T) {
			ctx, s := standbyStore(t)
			anchor := "anchor"
			if revision {
				attachStandby(t, ctx, s)
				anchor = "revision"
				exec(t, s, "INSERT INTO generations(relationship_id,execution_generation,dispatch_request_id,anchor_state,dispatch_turn_id,reason,opened_at,bound_at) VALUES('r',2,'revision-request','bound','revision','revision','2023-11-14T22:13:20Z','2023-11-14T22:13:20Z')")
				exec(t, s, "UPDATE relationships SET execution_generation=2 WHERE relationship_id='r'")
			}
			h := &observationHost{status: "failed"}
			if _, err := standbyDaemon(s, h).Tick(ctx); err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(h.reads, []string{anchor}) || count(t, s, "SELECT COUNT(*) FROM events WHERE producer='daemon_observation' AND turn_id=?", anchor) != 1 {
				t.Fatalf("real anchor failure concealed: %v", h.reads)
			}
		})
	}
}

func TestManagedStandbyIsNotFailureWhileBusinessAlreadyRuns(t *testing.T) {
	ctx, s := standbyStore(t)
	attachStandby(t, ctx, s)
	exec(t, s, admit("business", "explicit_admission_bound:anchor"))
	h := &observationHost{statuses: map[string]string{"anchor": "interrupted", "business": "inProgress"}}
	if report, err := standbyDaemon(s, h).Tick(ctx); err != nil || report.Observed != 0 {
		t.Fatalf("business already running: observed=%d err=%v", report.Observed, err)
	}
	noStandbySettlement(t, s)
}

func TestManagedStandbyExclusionRequiresExactIdentity(t *testing.T) {
	for _, change := range []string{
		"UPDATE managed_start_requests SET child_task_id='other'",
		"UPDATE managed_start_requests SET standby_turn_id='other'",
		"UPDATE managed_start_requests SET dispatch_request_id='other'",
		"UPDATE managed_start_requests SET execution_generation=2",
		"UPDATE managed_start_requests SET relationship_id=NULL",
		"UPDATE managed_start_requests SET state='create_armed'",
		"UPDATE managed_start_requests SET receipt_status='failed'",
	} {
		t.Run(change, func(t *testing.T) {
			ctx, s := standbyStore(t)
			attachStandby(t, ctx, s)
			exec(t, s, change)
			h := &observationHost{status: "interrupted"}
			if _, err := standbyDaemon(s, h).Tick(ctx); err != nil {
				t.Fatal(err)
			}
			if n := count(t, s, "SELECT COUNT(*) FROM events WHERE producer='daemon_observation' AND turn_id='anchor'"); n != 1 {
				t.Fatalf("unrelated managed record concealed anchor: %d", n)
			}
		})
	}
}

func TestManagedStandbyRecheckedAfterSelectionBeforeSettlement(t *testing.T) {
	ctx, s := standbyStore(t)
	h := &observationHost{status: "interrupted"}
	d := standbyDaemon(s, h)
	d.beforeSettle = func(store.TurnReference) { attachStandby(t, ctx, s) }
	if _, err := d.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(h.reads, []string{"anchor"}) {
		t.Fatalf("stale selection was not exercised: %v", h.reads)
	}
	noStandbySettlement(t, s)
	var status string
	if err := s.DB.QueryRow("SELECT last_status FROM poll_observations WHERE turn_id='anchor'").Scan(&status); err != nil || status != "interrupted" {
		t.Fatalf("terminal status rewritten: %s %v", status, err)
	}
}

func TestManagedStandbyRecheckedWhenAnotherPendingPathSelectsIt(t *testing.T) {
	ctx, s := standbyStore(t)
	attachStandby(t, ctx, s)
	// A redundant admission is another census path to the very same standby.
	exec(t, s, admit("anchor", "explicit_admission_bound:anchor"))
	h := &observationHost{status: "failed"}
	if _, err := standbyDaemon(s, h).Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(h.reads, []string{"anchor"}) {
		t.Fatalf("alternate pending path not exercised: %v", h.reads)
	}
	noStandbySettlement(t, s)
}

func TestManagedStandbyCorrectionSettlesGenuineFailureAtomically(t *testing.T) {
	ctx, s := standbyStore(t)
	h := &observationHost{status: "failed"}
	d := standbyDaemon(s, h)
	exec(t, s, "CREATE TRIGGER refuse_settlement BEFORE INSERT ON assignment_settlements BEGIN SELECT RAISE(ABORT, 'settlement refused'); END")
	if _, err := d.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	noStandbySettlement(t, s)
	exec(t, s, "DROP TRIGGER refuse_settlement")
	if _, err := d.Tick(ctx); err != nil || count(t, s, "SELECT COUNT(*) FROM events WHERE producer='daemon_observation'") != 1 {
		t.Fatalf("genuine failure did not recover from rollback: %v", err)
	}
}

func TestManagedStandbyCorrectionRechecksPauseBeforeSynthesizing(t *testing.T) {
	ctx, s := standbyStore(t)
	h := &observationHost{status: "interrupted"}
	d := standbyDaemon(s, h)
	d.beforeSettle = func(store.TurnReference) {
		exec(t, s, "UPDATE relationships SET status='paused' WHERE relationship_id='r'")
	}
	if report, err := d.Tick(ctx); err != nil || notesSaying(report.Notes, "observation deferred") != 1 {
		t.Fatalf("pause before synthesis: %+v %v", report, err)
	}
	noStandbySettlement(t, s)
}
