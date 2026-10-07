package daemon

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// CRW-904: the daemon's idle edge. A delivery that waits out its recipient's busy backoff is
// released by that recipient's idle report rather than only by its timer, and the relay holds the
// subscription that makes the report arrive. The delivery side of the wake is proved in
// internal/relay/delivery (idle_wake_test.go); what is proved here is the wiring: a report the host
// pushed reaches the wake, the subscription is opened for a waiting recipient, and it is released
// when the backlog empties.

const idleNow = 1700000000.0

const (
	idleParent = "parent-904"
	// idleThread is the recipient's own thread, which is the recipient task's id: a delivery names
	// both, and the App Server's status report carries the thread.
	idleThread = idleParent
	idleEvent  = "ev-904"
)

// idleHost is the relay's host as the idle edge sees it: the lifecycle read the busy decision uses,
// the status reports the App Server pushed, and the subscription calls. It answers the recipient
// busy, so the woken attempt meets the recipient mid-turn again and the delivery keeps waiting,
// which is the state the hold exists for. The embedded adapter is nil, so a call this path must not
// make (a send) would panic rather than pass quietly.
type idleHost struct {
	delivery.Adapter
	// status is the recipient's status on the lifecycle read.
	status string
	// reports are the status reports the App Server pushed since the last read.
	reports []delivery.IdleReport
	// holds and releases record the subscription calls the daemon made.
	holds, releases []string
	// holdErr, when set, is what HoldThread answers.
	holdErr error
	// holdTries counts every HoldThread call, refused ones included: a refusal is still an attempt.
	holdTries int
	// lost stands for a socket that went away: the subscription the client held went with it, so the
	// daemon has to hold it again.
	lost bool
	// foreign is another owner's subscription on the recipient's thread, as a live watch or the bridge's
	// retention leaves it. The relay's own hold is a separate reference: the foreign owner can release
	// its subscription mid-backlog, and the backlog has to take its own (CRW-904 d4).
	foreign bool
}

func (h *idleHost) Close() error { return nil }

func (h *idleHost) ReadThread(context.Context, string) (delivery.ThreadFacts, error) {
	status := h.status
	if status == "" {
		status = "active"
	}
	// A busy thread still accepts direct input as far as this read is concerned: the lifecycle
	// decision reads the status, and canAcceptDirectInput false is its own refusal.
	accepts := true
	return delivery.ThreadFacts{RuntimeStatus: status, CanAcceptInput: &accepts}, nil
}

func (h *idleHost) IsArchived(context.Context, string, any) (*bool, error) {
	no := false
	return &no, nil
}

func (h *idleHost) ReadGoalStatus(context.Context, string) (any, error) { return nil, nil }

func (h *idleHost) ListTurnIDs(context.Context, string, int) ([]any, error) { return nil, nil }

func (h *idleHost) ReadTurn(context.Context, string, string) (*delivery.TurnInfo, error) {
	return nil, nil
}

// IdleReports is the App Server's status reports since the last read: the daemon drains them, as it
// drains the real channel.
func (h *idleHost) IdleReports() []delivery.IdleReport {
	out := h.reports
	h.reports = nil
	return out
}

func (h *idleHost) HoldThread(_ context.Context, thread string) error {
	h.holdTries++
	if h.holdErr != nil {
		return h.holdErr
	}
	h.holds = append(h.holds, thread)
	return nil
}

func (h *idleHost) ReleaseThread(thread string) { h.releases = append(h.releases, thread) }

// ThreadSubscribed is another owner's subscription on the thread, never the relay's own hold. The idle
// edge must not read it as one: a watch can be released mid-backlog, and the relay then holds nothing.
func (h *idleHost) ThreadSubscribed(string) bool { return h.foreign }

// ThreadHeld is whether this relay's own hold still stands: a lost socket takes it with it, and the
// relay then has to hold it again. A foreign watch or the bridge's retention is deliberately not
// modelled here: the daemon asks about its own hold, not about any subscription (CRW-904 d4).
func (h *idleHost) ThreadHeld(thread string) bool {
	if h.lost {
		return false
	}
	return slices.Contains(h.holds, thread)
}

// seedWaitingHead registers one relationship and leaves its parent's completion delivery waiting out
// a busy backoff 300 s from now: the head the idle edge is for.
func seedWaitingHead(t *testing.T, s *store.Store, now float64) {
	t.Helper()
	seed(t, s, "rel-904", idleParent, "child-904", "anchor-904")
	const stamp = "2023-11-14T22:13:20Z"
	exec(t, s, "INSERT INTO events(event_id,relationship_id,execution_generation,revision_hash,outcome,producer,turn_thread_id,turn_id,turn_status,receipt,stage,first_seen_at,last_seen_at) VALUES(?,'rel-904',1,'rev-904','ready_for_review','child','child-904','turn-904','completed','{}','final',?,?)", idleEvent, stamp, stamp)
	exec(t, s, "INSERT INTO deliveries(event_id,relationship_id,kind,recipient_task_id,recipient_thread_id,state,attempt_count,next_eligible_at,created_at,updated_at) VALUES(?,'rel-904','completion_event',?,?,'deferred_busy',0,?,?,?)", idleEvent, idleParent, idleThread, now+300, stamp, stamp)
}

// idleDaemon is a daemon over a store holding one waiting head, and the host the test drives.
func idleDaemon(t *testing.T, host *idleHost) (*Daemon, *store.Store) {
	t.Helper()
	ctx := context.Background()
	s, err := fixtureStore(ctx, filepath.Join(t.TempDir(), "relay.sqlite3"), "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	seedWaitingHead(t, s, idleNow)
	return New(s, host, &delivery.FakeClock{T: idleNow}, nil), s
}

// busyAnswers is how many times a delivery found its recipient busy: deferBusy journals one per
// answer. The seeded head has none, so any answer is the woken attempt's.
func busyAnswers(t *testing.T, s *store.Store) int {
	t.Helper()
	return count(t, s, "SELECT COUNT(*) FROM journal WHERE kind = 'delivery_deferred_busy' AND subject = ?", idleEvent)
}

func TestIdleWake_an_idle_report_releases_the_head_in_the_same_tick(t *testing.T) {
	t.Parallel()
	host := &idleHost{reports: []delivery.IdleReport{{ThreadID: idleThread, Status: "idle"}}}
	d, s := idleDaemon(t, host)
	ctx := context.Background()
	if n := count(t, s, "SELECT COUNT(*) FROM delivery_wakes"); n != 0 {
		t.Fatal("a head was woken before any report")
	}
	if _, err := d.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	// The woken head was due in this same tick and the recipient answered busy again: the answer
	// can only exist if the wake released the head, because the seeded deadline is 300 s away.
	if n := busyAnswers(t, s); n != 1 {
		t.Fatalf("%d busy answers, want the woken attempt to have met the recipient once", n)
	}
	row, err := s.One(ctx, "SELECT state, next_eligible_at FROM deliveries WHERE event_id = ?", idleEvent)
	if err != nil {
		t.Fatal(err)
	}
	if row.Text("state") != delivery.DeferredBusy {
		t.Fatalf("the delivery is %s, want %s", row.Text("state"), delivery.DeferredBusy)
	}
	// due = min(original, recomputed): the wake never pushes the safety-net timer later.
	if due := row.Get("next_eligible_at").(float64); due > idleNow+300 {
		t.Fatalf("the woken attempt pushed the deadline to %.0f, past the original %.0f", due, idleNow+300)
	}
	if n := count(t, s, "SELECT COUNT(*) FROM delivery_wakes"); n != 0 {
		t.Fatal("the busy answer did not spend the wake")
	}
	// The relay holds the subscription that makes the next report arrive.
	if !slices.Equal(host.holds, []string{idleThread}) {
		t.Fatalf("holds %v, want one on %s", host.holds, idleThread)
	}
	if len(host.releases) != 0 {
		t.Fatalf("releases %v while the backlog waits", host.releases)
	}
}

func TestIdleWake_a_report_for_a_thread_with_no_waiting_head_wakes_nothing(t *testing.T) {
	t.Parallel()
	host := &idleHost{reports: []delivery.IdleReport{{ThreadID: "a-thread-with-no-backlog", Status: "idle"}}}
	d, s := idleDaemon(t, host)
	if _, err := d.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := count(t, s, "SELECT COUNT(*) FROM delivery_wakes"); n != 0 {
		t.Fatalf("%d wakes were written for a thread with no waiting head", n)
	}
	if n := busyAnswers(t, s); n != 0 {
		t.Fatal("the head was attempted though nothing woke it")
	}
}

func TestIdleWake_only_idle_and_notLoaded_reports_release_a_head(t *testing.T) {
	t.Parallel()
	for _, status := range []string{"active", "systemError", ""} {
		t.Run("a "+status+" report", func(t *testing.T) {
			host := &idleHost{reports: []delivery.IdleReport{{ThreadID: idleThread, Status: status}}}
			d, s := idleDaemon(t, host)
			if _, err := d.Tick(context.Background()); err != nil {
				t.Fatal(err)
			}
			if n := count(t, s, "SELECT COUNT(*) FROM delivery_wakes"); n != 0 {
				t.Fatalf("a %q report woke a head", status)
			}
			if n := busyAnswers(t, s); n != 0 {
				t.Fatalf("a %q report released the head", status)
			}
		})
	}
}

func TestIdleWake_the_hold_is_released_when_the_backlog_empties(t *testing.T) {
	t.Parallel()
	host := &idleHost{}
	d, s := idleDaemon(t, host)
	ctx := context.Background()
	if _, err := d.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(host.holds, []string{idleThread}) {
		t.Fatalf("holds %v, want one while the head waits", host.holds)
	}
	// A second tick with no new report opens no second subscription.
	if _, err := d.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if len(host.holds) != 1 {
		t.Fatalf("holds %v, want the subscription kept once", host.holds)
	}
	// The delivery was delivered: nothing waits on that recipient any more.
	exec(t, s, "UPDATE deliveries SET state = 'dispatched', next_eligible_at = NULL WHERE event_id = ?", idleEvent)
	if _, err := d.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(host.releases, []string{idleThread}) {
		t.Fatalf("releases %v, want one on %s once the backlog emptied", host.releases, idleThread)
	}
}

func TestIdleWake_a_refused_hold_leaves_the_backoff_as_the_only_trigger(t *testing.T) {
	t.Parallel()
	host := &idleHost{holdErr: errors.New("the host refused the resume")}
	d, s := idleDaemon(t, host)
	report, err := d.Tick(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Notes) == 0 {
		t.Fatal("a hold the host refused was not reported")
	}
	if n := busyAnswers(t, s); n != 0 {
		t.Fatal("the head was attempted though no subscription could be held")
	}
	row, err := s.One(context.Background(), "SELECT next_eligible_at FROM deliveries WHERE event_id = ?", idleEvent)
	if err != nil {
		t.Fatal(err)
	}
	if due := row.Get("next_eligible_at").(float64); due != idleNow+300 {
		t.Fatalf("the deadline moved to %.0f without a subscription", due)
	}
}

// CRW-904 (correction, d3): a hold the host refused is not retried on every tick. The delivery's own
// busy deadline is the relay's next reason to look at that recipient, so the resume is tried again
// then and not before; until then the backoff alone is the trigger, which is management decision 4.
func TestIdleWake_a_refused_hold_is_not_retried_before_the_busy_deadline(t *testing.T) {
	t.Parallel()
	host := &idleHost{holdErr: errors.New("the host refused the resume")}
	d, _ := idleDaemon(t, host)
	ctx := context.Background()
	if _, err := d.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if host.holdTries != 1 {
		t.Fatalf("the first tick made %d hold attempts, want one", host.holdTries)
	}
	// Ticks inside the backoff: the refusal stands, and the relay does not resume again.
	for _, at := range []float64{idleNow + 20, idleNow + 60, idleNow + 299} {
		d.Clock.(*delivery.FakeClock).T = at
		report, err := d.Tick(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, note := range report.Notes {
			if strings.Contains(note, "not opened") {
				t.Fatalf("the refused hold was reported again at %.0f: %v", at, note)
			}
		}
	}
	if host.holdTries != 1 {
		t.Fatalf("the relay retried the refused resume %d times before the busy deadline, want one", host.holdTries)
	}
	// The delivery's own deadline is the next reason to look at that recipient: the hold is tried then.
	d.Clock.(*delivery.FakeClock).T = idleNow + 301
	if _, err := d.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if host.holdTries != 2 {
		t.Fatalf("the hold was not retried at the busy deadline: %d attempts", host.holdTries)
	}
}

func TestIdleWake_a_host_without_the_capability_leaves_the_backoff_alone(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, err := fixtureStore(ctx, filepath.Join(t.TempDir(), "relay.sqlite3"), "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	seedWaitingHead(t, s, idleNow)
	// The observation host of the other daemon tests has no status reports and no subscriptions.
	d := New(s, &observationHost{status: "completed"}, &delivery.FakeClock{T: idleNow}, nil)
	if _, err := d.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if n := count(t, s, "SELECT COUNT(*) FROM delivery_wakes"); n != 0 {
		t.Fatal("a host with no status reports woke a head")
	}
	if n := busyAnswers(t, s); n != 0 {
		t.Fatal("the head was attempted with no subscription and no report")
	}
}

// CRW-904 (correction, d4): a recipient whose thread another owner already subscribes still gets the
// backlog's own hold. The idle edge must not borrow that subscription: a live watch or the bridge's
// retention is released by its own owner (turn/completed, the release worker), and the hold is what
// keeps the recipient's reports arriving through the rest of the backlog.
func TestIdleWake_takes_its_own_hold_beside_another_owners_subscription(t *testing.T) {
	t.Parallel()
	host := &idleHost{foreign: true}
	d, s := idleDaemon(t, host)
	ctx := context.Background()
	if _, err := d.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(host.holds, idleThread) {
		t.Fatalf("holds %v: the backlog did not take its own hold beside another owner's subscription", host.holds)
	}
	// That owner releases its subscription; the relay's own hold is what keeps the idle edge standing.
	host.foreign = false
	if _, err := d.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if len(host.releases) != 0 {
		t.Fatalf("releases %v while the backlog still waits", host.releases)
	}
	// The backlog empties: the relay's own hold is released, once.
	exec(t, s, "UPDATE deliveries SET state = 'dispatched', next_eligible_at = NULL WHERE event_id = ?", idleEvent)
	if _, err := d.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(host.releases, []string{idleThread}) {
		t.Fatalf("releases %v, want one on %s once the backlog emptied", host.releases, idleThread)
	}
}

func TestIdleWake_a_lost_socket_is_held_again(t *testing.T) {
	t.Parallel()
	host := &idleHost{}
	d, _ := idleDaemon(t, host)
	ctx := context.Background()
	if _, err := d.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if len(host.holds) != 1 {
		t.Fatalf("holds %v, want one while the head waits", host.holds)
	}
	// The socket went away and the subscription with it: the relay holds it again on the next tick
	// rather than believing a hold that no longer exists.
	host.lost = true
	if _, err := d.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if len(host.holds) != 2 {
		t.Fatalf("holds %v, want the hold reopened after the socket was lost", host.holds)
	}
}

func TestIdleWake_a_note_is_reported_once(t *testing.T) {
	t.Parallel()
	host := &idleHost{reports: []delivery.IdleReport{{ThreadID: idleThread, Status: "idle"}}, holdErr: errors.New("the host refused the resume")}
	d, s := idleDaemon(t, host)
	// Two failures in one tick: the wake cannot be written, and the hold is refused. (The zone table is
	// replaced with one this build does not declare: the wake's insert names original_deadline, which
	// the replacement does not have, while the head set's join reads only event_id and spent_at, so the
	// recipient is still seen as a waiting head and the hold is still tried.) The tick's notes name
	// each failure once.
	exec(t, s, "DROP TABLE delivery_wakes")
	exec(t, s, "CREATE TABLE delivery_wakes (event_id TEXT PRIMARY KEY, spent_at TEXT)")
	report, err := d.Tick(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	wakes, holds := 0, 0
	for _, note := range report.Notes {
		if strings.Contains(note, "not applied") {
			wakes++
		}
		if strings.Contains(note, "not opened") {
			holds++
		}
	}
	if wakes != 1 || holds != 1 {
		t.Fatalf("the tick reported %d wake notes and %d hold notes, want one of each: %v", wakes, holds, report.Notes)
	}
}
