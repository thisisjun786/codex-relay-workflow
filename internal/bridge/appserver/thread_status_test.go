package appserver

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver/fakehost"
)

// CRW-904: the relay's own hold on a recipient thread while a delivery to it waits out a busy
// backoff. Only thread/start and thread/resume subscribe a connection, and an admitted watch holds
// the root's mutation gate until Finish, so the hold is the resume plus the retention path and not a
// watch. A scripted App Server proves what the hold sent and what it kept.

func holdClient(t *testing.T) (*Client, *fakehost.Server) {
	t.Helper()
	host := fakehost.Start(t)
	host.Respond("thread/resume", fakehost.Reply{Result: map[string]any{"thread": map[string]any{"id": "thread-1"}}})
	host.Respond("thread/unsubscribe", fakehost.Reply{Result: map[string]any{"status": "unsubscribed"}})
	c, err := Dial(context.Background(), host.SocketPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c, host
}

// holdParams is the params of the thread/resume the hold sent.
func holdParams(t *testing.T, host *fakehost.Server) map[string]any {
	t.Helper()
	var params map[string]any
	seen := false
	for _, request := range host.Requests() {
		if request.Method != "thread/resume" {
			continue
		}
		if err := json.Unmarshal(request.Params, &params); err != nil {
			t.Fatal(err)
		}
		seen = true
	}
	if !seen {
		t.Fatal("no thread/resume reached the host")
	}
	return params
}

func TestThreadHold_resumes_once_with_no_overrides_and_keeps_the_subscription(t *testing.T) {
	c, host := holdClient(t)
	if c.ThreadSubscribed("thread-1") {
		t.Fatal("the client claims a subscription it never took")
	}
	if err := c.HoldThread(context.Background(), "thread-1"); err != nil {
		t.Fatal(err)
	}
	if !c.ThreadSubscribed("thread-1") {
		t.Fatal("the hold was not recorded")
	}
	if c.ThreadSubscribed("thread-2") {
		t.Fatal("another thread reads as subscribed")
	}
	if got := host.Count("thread/resume"); got != 1 {
		t.Fatalf("%d resumes reached the host, want 1", got)
	}
	if got := host.Count("turn/start"); got != 0 {
		t.Fatalf("the hold started %d turns; it starts none", got)
	}
	// No overrides: the resume carries the thread and excludeTurns and nothing else, so a busy
	// recipient's settings are not changed.
	if params := holdParams(t, host); len(params) != 2 || params["threadId"] != "thread-1" || params["excludeTurns"] != true {
		t.Fatalf("the hold's resume carries %v, want only threadId and excludeTurns", params)
	}
	// A second hold is the same subscription, not a second resume.
	if err := c.HoldThread(context.Background(), "thread-1"); err != nil {
		t.Fatal(err)
	}
	if got := host.Count("thread/resume"); got != 1 {
		t.Fatalf("a repeated hold resumed again (%d resumes)", got)
	}
}

func TestThreadHold_takes_no_gate_so_the_deliverys_own_watch_is_admitted(t *testing.T) {
	c, _ := holdClient(t)
	if err := c.HoldThread(context.Background(), "thread-1"); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		watch, err := c.WatchTurn(context.Background(), "thread-1")
		if err == nil {
			watch.Finish("turn-1", false)
		}
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("the delivery's own watch was not admitted while the hold stood: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the hold blocked the delivery's own watch: it held the root's gate")
	}
}

func TestThreadHold_a_completed_turn_does_not_drop_the_hold(t *testing.T) {
	c, host := holdClient(t)
	if err := c.HoldThread(context.Background(), "thread-1"); err != nil {
		t.Fatal(err)
	}
	host.Respond("probe/end", fakehost.Reply{Before: []fakehost.Notification{{Method: "turn/completed", Params: map[string]any{"threadId": "thread-1", "turn": map[string]any{"id": "turn-1", "status": "completed"}}}}})
	if _, err := c.Call(context.Background(), "probe/end", nil); err != nil {
		t.Fatal(err)
	}
	if !c.ThreadSubscribed("thread-1") {
		t.Fatal("a completed turn dropped the busy hold")
	}
	if got := host.Count("thread/unsubscribe"); got != 0 {
		t.Fatalf("the hold's subscription was released while it stood (%d unsubscribes)", got)
	}
}

// CRW-904 (correction, d4): the busy backlog takes its own hold even when another owner already
// subscribed the thread, and it is the relay's own hold that keeps the subscription. ThreadHeld answers
// for that hold alone; ThreadSubscribed also counts a live watch and the bridge's retention, which their
// own owners release.
func TestThreadHold_takes_its_own_hold_beside_a_live_watch(t *testing.T) {
	c, host := holdClient(t)
	watch, err := c.WatchTurn(context.Background(), "thread-1")
	if err != nil {
		t.Fatal(err)
	}
	if !c.ThreadSubscribed("thread-1") {
		t.Fatal("the live watch did not subscribe the thread")
	}
	if c.ThreadHeld("thread-1") {
		t.Fatal("a watch alone reads as the relay's own hold")
	}
	// An admitted watch holds the root's gate until it finishes, so the hold waits for it and is taken
	// as it ends: the interleaving the release worker and the hold share in production.
	returned := make(chan error, 1)
	go func() { returned <- c.HoldThread(context.Background(), "thread-1") }()
	select {
	case err := <-returned:
		t.Fatalf("the hold was taken while the watch still held the root's gate: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	watch.Finish("", false)
	if err := <-returned; err != nil {
		t.Fatal(err)
	}
	if !c.ThreadHeld("thread-1") {
		t.Fatal("the backlog did not record its own hold beside the watch")
	}
	if !c.ThreadSubscribed("thread-1") {
		t.Fatal("the thread is not subscribed while the backlog waits")
	}
	// CRW-904 (correction, d2): a live watch is not evidence that this connection receives the
	// thread's reports. WatchTurn only admits a watch and sends no subscribing RPC, so a watch whose
	// resume was refused or cancelled leaves the connection unsubscribed while the watch is still live.
	// The hold therefore sends its own override-free resume beside the watch; the cost of the other
	// error is one redundant resume, which starts no turn and changes nothing.
	if got := host.Count("thread/resume"); got != 1 {
		t.Fatalf("%d resumes reached the host, want the hold's own override-free resume", got)
	}
	// The watch is gone and nothing but the backlog's hold keeps the root: no release may drop it.
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	if err := host.WaitCount(ctx, "thread/unsubscribe", 1); err == nil {
		t.Fatal("the watch's end unsubscribed a thread the busy backlog holds")
	}
	if !c.ThreadHeld("thread-1") {
		t.Fatal("the hold did not survive the watch ending")
	}
	// Releasing the backlog's hold drops the subscription it kept.
	c.ReleaseThread("thread-1")
	done, cancelDone := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancelDone()
	if err := host.WaitCount(done, "thread/unsubscribe", 1); err != nil {
		t.Fatalf("releasing the backlog's hold did not unsubscribe: %v", err)
	}
}

// CRW-904 (correction, d4): releasing the backlog's hold drops only its own reference. A live watch
// that subscribed the thread keeps its subscription, and nothing is sent for it.
func TestThreadHold_releases_only_its_own_hold(t *testing.T) {
	c, host := holdClient(t)
	if err := c.HoldThread(context.Background(), "thread-1"); err != nil {
		t.Fatal(err)
	}
	watch, err := c.WatchTurn(context.Background(), "thread-1")
	if err != nil {
		t.Fatal(err)
	}
	c.ReleaseThread("thread-1")
	if c.ThreadHeld("thread-1") {
		t.Fatal("the released hold is still recorded")
	}
	if !c.ThreadSubscribed("thread-1") {
		t.Fatal("releasing the backlog's hold dropped the watch's subscription")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	if err := host.WaitCount(ctx, "thread/unsubscribe", 1); err == nil {
		t.Fatal("releasing the backlog's hold unsubscribed a thread a live watch still holds")
	}
	watch.Finish("", false)
}

func TestThreadHold_releases_once_the_backlog_empties(t *testing.T) {
	c, host := holdClient(t)
	if err := c.HoldThread(context.Background(), "thread-1"); err != nil {
		t.Fatal(err)
	}
	c.ReleaseThread("thread-1")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := host.WaitCount(ctx, "thread/unsubscribe", 1); err != nil {
		t.Fatalf("the released hold did not unsubscribe: %v", err)
	}
	if c.ThreadSubscribed("thread-1") {
		t.Fatal("the released hold is still recorded")
	}
}

func TestThreadHold_leaves_the_bridges_own_retention_alone(t *testing.T) {
	c, host := holdClient(t)
	if err := c.HoldThread(context.Background(), "thread-1"); err != nil {
		t.Fatal(err)
	}
	// The bridge's own retention of a never-run root, as the creation path records it.
	m := c.subscriptions
	m.mu.Lock()
	root := m.roots["thread-1"]
	root.retainedOn = root.connection
	m.mu.Unlock()
	c.ReleaseThread("thread-1")
	if !c.ThreadSubscribed("thread-1") {
		t.Fatal("releasing the hold dropped the bridge's own retention of the root")
	}
	if got := host.Count("thread/unsubscribe"); got != 0 {
		t.Fatalf("the root was unsubscribed while its retention stood (%d unsubscribes)", got)
	}
}

func TestThreadHold_a_refused_resume_holds_nothing(t *testing.T) {
	c, host := holdClient(t)
	host.Respond("thread/resume", fakehost.Reply{Error: &fakehost.RPCError{Code: -32600, Message: "no such thread"}})
	if err := c.HoldThread(context.Background(), "thread-1"); err == nil {
		t.Fatal("a refused resume was taken as a hold")
	}
	if c.ThreadSubscribed("thread-1") {
		t.Fatal("a refused resume left a subscription recorded")
	}
}

// CRW-904 (correction, d2): a hold whose resume was transmitted but never answered can leave a real
// subscription with nobody owning it. The reply is missing, so the caller must not treat the
// subscription as established, but the hold is still recorded: the ordinary release path then
// unsubscribes it, instead of the subscription surviving until the socket dies.
func TestThreadHold_a_transmitted_resume_without_a_reply_keeps_an_owned_hold(t *testing.T) {
	c, host := holdClient(t)
	// The host applies the resume and withholds its answer past the acknowledgement deadline. The
	// connection stays up, so the subscription it made is real and unreleased.
	entered, release := make(chan struct{}, 1), make(chan struct{})
	host.Script("thread/resume", fakehost.Reply{Paused: entered, Release: release})
	c.BoundAck(200 * time.Millisecond)
	err := c.HoldThread(context.Background(), "thread-1")
	if err == nil {
		t.Fatal("a resume with no answer was reported as an established hold")
	}
	if !c.ThreadHeld("thread-1") {
		t.Fatal("the subscription the host applied is owned by nobody, so no release can drop it")
	}
	close(release)
	// The backlog empties: the hold is released and the ordinary worker unsubscribes it.
	c.ReleaseThread("thread-1")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := host.WaitCount(ctx, "thread/unsubscribe", 1); err != nil {
		t.Fatalf("the released hold did not unsubscribe the subscription the resume made: %v", err)
	}
	if c.ThreadHeld("thread-1") {
		t.Fatal("the released hold is still recorded")
	}
}

// CRW-904 (correction, d2): a live watch is not evidence that this connection receives the thread's
// reports. WatchTurn only admits a watch and sends no subscribing RPC of its own, so when the resume
// the delivery path sends beside it is refused or cancelled the watch is still live while the
// connection receives nothing. `receivesOn`, which decides whether the hold owes its own resume, must
// not read that watch as a subscription. `ThreadSubscribed` still counts it: that is the question
// "does a busy deferral find a subscription", a different one from "do I already receive this".
func TestThreadHold_a_live_watch_is_not_evidence_the_connection_receives_the_thread(t *testing.T) {
	c, _ := holdClient(t)
	watch, err := c.WatchTurn(context.Background(), "thread-1")
	if err != nil {
		t.Fatal(err)
	}
	defer watch.Finish("", false)
	if !c.ThreadSubscribed("thread-1") {
		t.Fatal("the live watch does not read as a subscription at all")
	}
	if c.ThreadHeld("thread-1") {
		t.Fatal("a watch alone reads as the relay's own hold")
	}
	// The hold's own predicate: this connection receives the thread only through this relay's hold or
	// the bridge's retention, never through a watch whose subscribing resume nobody acknowledged.
	c.mu.Lock()
	conn := c.conn
	c.mu.Unlock()
	c.subscriptions.mu.Lock()
	receives := c.subscriptions.receivesOn("thread-1", conn)
	c.subscriptions.mu.Unlock()
	if receives {
		t.Fatal("a live watch read as a connection that already receives the thread's reports")
	}
}

func TestThreadHold_releasing_a_thread_it_never_held_is_silent(t *testing.T) {
	c, host := holdClient(t)
	c.ReleaseThread("never-held")
	if c.ThreadSubscribed("never-held") {
		t.Fatal("a thread that was never held reports a subscription")
	}
	if got := host.Count("thread/unsubscribe"); got != 0 {
		t.Fatalf("releasing a hold that was never taken sent %d unsubscribes", got)
	}
}

// CRW-904 (review): a hold taken while a release worker is mid-unsubscribe must not be cancelled by
// it. HoldThread takes the root's gate, which is the same gate the worker takes before it rechecks
// readiness, so the two cannot interleave: the resume lands after the unsubscribe, or the worker's
// recheck sees the hold and leaves the subscription standing.
func TestThreadHold_a_pending_release_does_not_cancel_a_new_hold(t *testing.T) {
	c, host := holdClient(t)
	if err := c.HoldThread(context.Background(), "thread-1"); err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}, 1), make(chan struct{})
	host.Script("thread/unsubscribe", fakehost.Reply{Paused: entered, Release: release})
	c.ReleaseThread("thread-1")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := host.WaitCount(ctx, "thread/unsubscribe", 1); err != nil {
		t.Fatalf("the release never reached the host: %v", err)
	}
	// The worker holds the gate while its unsubscribe is outstanding, so this hold waits for it and
	// then re-subscribes: the subscription stands when both are done.
	returned := make(chan error, 1)
	go func() { returned <- c.HoldThread(context.Background(), "thread-1") }()
	select {
	case err := <-returned:
		t.Fatalf("the hold was admitted while the release held the gate: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	if err := <-returned; err != nil {
		t.Fatal(err)
	}
	if !c.ThreadSubscribed("thread-1") {
		t.Fatal("the new hold was cancelled by the release it raced")
	}
	if got := host.Count("thread/resume"); got != 2 {
		t.Fatalf("%d resumes reached the host, want the first hold and the reopened one", got)
	}
}

// CRW-904 (review): a hold taken after the release worker finished subscribes the thread again.
func TestThreadHold_a_hold_after_a_release_subscribes_again(t *testing.T) {
	c, host := holdClient(t)
	if err := c.HoldThread(context.Background(), "thread-1"); err != nil {
		t.Fatal(err)
	}
	c.ReleaseThread("thread-1")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := host.WaitCount(ctx, "thread/unsubscribe", 1); err != nil {
		t.Fatal(err)
	}
	if c.ThreadSubscribed("thread-1") {
		t.Fatal("the released hold is still recorded")
	}
	if err := c.HoldThread(context.Background(), "thread-1"); err != nil {
		t.Fatal(err)
	}
	if !c.ThreadSubscribed("thread-1") {
		t.Fatal("the reopened hold was not recorded")
	}
	if got := host.Count("thread/resume"); got != 2 {
		t.Fatalf("%d resumes reached the host, want one per hold", got)
	}
}
