package adapter

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver"
	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver/fakehost"
	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/execution"
	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/ledger"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/daemon"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// CRW-904 (correction, d5): the idle edge over the real transport, end to end. A delivery to a
// recipient waits out a busy backoff with no subscription at all; the daemon's first tick opens the
// relay's own hold on that recipient; a real thread/status/changed notification for it arrives on the
// scripted App Server's socket; and the next tick wakes the head and attempts it. Nothing here is a
// double for the notification path: the frame crosses the socket, the client's reader turns it into a
// report, the adapter's IdleReports hands it to the daemon, and the wake is written in the store.

const (
	idleE2EParent = "parent-e2e"
	idleE2EThread = idleE2EParent
	idleE2EEvent  = "ev-e2e"
)

// idleE2EStore is a store holding one relationship and one completion delivery to its parent that
// waits out a busy backoff 300 s from the clock's instant: the head the idle edge is for.
func idleE2EStore(t *testing.T, ctx context.Context, socket string, now float64) *store.Store {
	t.Helper()
	s, err := openStore(ctx, filepath.Join(t.TempDir(), "relay.sqlite3"), socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	const stamp = "2023-11-14T22:13:20Z"
	if _, err := s.DB.Exec("INSERT INTO relationships(relationship_id,issue_key,status,parent_task_id,parent_host_id,parent_cwd,child_task_id,child_host_id,child_cwd,execution_generation,artifact_roots,allowed_recipients,created_at,updated_at) VALUES(?,'REL-1','active',?,'host','/parent',?,'host','/child',1,'[]',?,?,?)", "rel-e2e", idleE2EParent, "child-e2e", `["`+idleE2EParent+`"]`, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.Exec("INSERT INTO generations(relationship_id,execution_generation,dispatch_request_id,anchor_state,dispatch_turn_id,reason,opened_at,bound_at) VALUES('rel-e2e',1,'dispatch-rel-e2e','bound','anchor-e2e','initial_assignment',?,?)", stamp, stamp); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.Exec("INSERT INTO events(event_id,relationship_id,execution_generation,revision_hash,outcome,producer,turn_thread_id,turn_id,turn_status,receipt,stage,first_seen_at,last_seen_at) VALUES(?, 'rel-e2e',1,'rev-e2e','ready_for_review','child','child-e2e','turn-e2e','completed','{}','final',?,?)", idleE2EEvent, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.Exec("INSERT INTO deliveries(event_id,relationship_id,kind,recipient_task_id,recipient_thread_id,state,attempt_count,next_eligible_at,created_at,updated_at) VALUES(?,'rel-e2e','completion_event',?,?,'deferred_busy',0,?,?,?)", idleE2EEvent, idleE2EParent, idleE2EThread, now+300, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	return s
}

// idleE2EHost scripts the App Server reads the observation pass and the busy decision make. The
// recipient reports active, so the woken attempt meets it mid-turn again and the delivery keeps
// waiting: what the test proves is that the attempt happened at all, 300 s before its own deadline.
func idleE2EHost(t *testing.T) *fakehost.Server {
	t.Helper()
	host := fakehost.Start(t)
	host.Handle("thread/read", func(json.RawMessage) fakehost.Reply {
		return fakehost.Reply{Result: map[string]any{"thread": map[string]any{"status": map[string]any{"type": "active"}, "canAcceptDirectInput": true}}}
	})
	host.Handle("thread/list", func(json.RawMessage) fakehost.Reply {
		return fakehost.Reply{Result: map[string]any{"data": []any{}, "nextCursor": nil}}
	})
	host.Handle("thread/turns/list", func(json.RawMessage) fakehost.Reply {
		return fakehost.Reply{Result: map[string]any{"data": []any{}, "nextCursor": nil}}
	})
	host.Handle("thread/items/list", func(json.RawMessage) fakehost.Reply {
		return fakehost.Reply{Result: map[string]any{"data": []any{}, "nextCursor": nil}}
	})
	host.Respond("thread/goal/get", fakehost.Reply{Result: map[string]any{"goal": nil}})
	host.Respond("thread/resume", fakehost.Reply{Result: map[string]any{"thread": map[string]any{"id": idleE2EThread}}})
	host.Respond("thread/unsubscribe", fakehost.Reply{Result: map[string]any{"status": "unsubscribed"}})
	return host
}

func TestIdleWake_end_to_end_from_a_busy_deferral_to_the_woken_attempt(t *testing.T) {
	ctx := context.Background()
	const now = 1700000000.0
	host := idleE2EHost(t)
	s := idleE2EStore(t, ctx, host.SocketPath, now)
	client := appserver.New(host.SocketPath, appserver.DefaultBounds)
	l, err := ledger.OpenWithOptions(filepath.Join(t.TempDir(), "operations.sqlite3"), ledger.Options{Encode: encodeReceipt})
	if err != nil {
		t.Fatal(err)
	}
	a := New(Options{RPC: client, Ledger: l, Policy: execution.Policy{}})
	t.Cleanup(func() { _ = a.Close() })
	d := daemon.New(s, a, &delivery.FakeClock{T: now}, nil)

	// No subscription exists yet, and the head waits 300 s: the first tick opens the relay's own hold
	// on the recipient, and nothing else happens because no report has arrived.
	if _, err := d.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if got := host.Count("thread/resume"); got != 1 {
		t.Fatalf("%d resumes reached the host, want the relay's own hold on the recipient", got)
	}
	if got := busyAnswers(t, s); got != 0 {
		t.Fatalf("the head was attempted before any idle report (%d busy answers)", got)
	}
	var params map[string]any
	for _, request := range host.Requests() {
		if request.Method == "thread/resume" {
			if err := json.Unmarshal(request.Params, &params); err != nil {
				t.Fatal(err)
			}
		}
	}
	if len(params) != 2 || params["threadId"] != idleE2EThread || params["excludeTurns"] != true {
		t.Fatalf("the hold's resume carries %v, want only threadId and excludeTurns", params)
	}
	if got := host.Count("turn/start"); got != 0 {
		t.Fatalf("the hold started %d turns; a busy recipient is never interrupted", got)
	}

	// The App Server pushes a real thread/status/changed to idle for that subscribed thread. It rides
	// a request the test makes, which is how the scripted host delivers a notification.
	host.Respond("probe/idle", fakehost.Reply{Before: []fakehost.Notification{{
		Method: "thread/status/changed",
		Params: map[string]any{"threadId": idleE2EThread, "status": map[string]any{"type": "idle"}},
	}}})
	if _, err := a.HostCall(ctx, "probe/idle", nil); err != nil {
		t.Fatal(err)
	}

	// The next tick turns that report into a wake and attempts the head, 300 s before its own deadline.
	if _, err := d.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if got := busyAnswers(t, s); got != 1 {
		t.Fatalf("the idle report did not release the head: %d busy answers, want 1", got)
	}
	row, err := s.One(ctx, "SELECT state, next_eligible_at FROM deliveries WHERE event_id = ?", idleE2EEvent)
	if err != nil {
		t.Fatal(err)
	}
	if row.Text("state") != delivery.DeferredBusy {
		t.Fatalf("the delivery is %s, want %s", row.Text("state"), delivery.DeferredBusy)
	}
	// due = min(original, recomputed): the early wake never pushes the safety-net timer later.
	if due := row.Get("next_eligible_at").(float64); due > now+300 {
		t.Fatalf("the woken attempt pushed the deadline to %.0f, past the original %.0f", due, now+300)
	}
	if n := countRows(t, s, "SELECT COUNT(*) FROM delivery_wakes"); n != 0 {
		t.Fatalf("the answered attempt left %d wakes behind", n)
	}
}

// busyAnswers is how many busy answers the delivery has journaled: any answer is the woken attempt's.
func busyAnswers(t *testing.T, s *store.Store) int64 {
	t.Helper()
	var n int64
	if err := s.DB.QueryRow("SELECT COUNT(*) FROM journal WHERE kind = 'delivery_deferred_busy' AND subject = ?", idleE2EEvent).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func countRows(t *testing.T, s *store.Store, query string) int64 {
	t.Helper()
	var n int64
	if err := s.DB.QueryRow(query).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}
