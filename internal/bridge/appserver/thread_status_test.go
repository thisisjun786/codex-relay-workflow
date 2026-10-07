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
