package adapter

import (
	"context"
	"encoding/json"
	"path/filepath"
	"sync"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver"
	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver/fakehost"
	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/execution"
	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/ledger"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/daemon"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/supervisor"
)

// CRW-1334 (CRW-1314 design section 3): a supervisor message to a busy recipient, end to end over
// the real transport. The first tick attempts the staged report while the scripted App Server reads
// the supervisor's thread active, so the message is deferred with the real busy backoff and the
// relay opens its own hold on the supervisor's thread. The host then unloads the thread: a real
// thread/status/changed to notLoaded crosses the socket, the next tick wakes the message long before
// its deadline, and the supervisor channel's transport loads the thread with thread/resume and opens
// the turn with turn/start. No relay store schema is involved beyond the rows the relay already
// writes.

func TestSupervisorWake_end_to_end_a_notLoaded_supervisor_is_resumed_and_given_the_turn(t *testing.T) {
	ctx := context.Background()
	const now = 1700000000.0
	host := fakehost.Start(t)
	var mu sync.Mutex
	status := "active"
	setStatus := func(s string) {
		mu.Lock()
		status = s
		mu.Unlock()
	}
	host.Handle("thread/read", func(json.RawMessage) fakehost.Reply {
		mu.Lock()
		defer mu.Unlock()
		return fakehost.Reply{Result: map[string]any{"thread": map[string]any{"status": map[string]any{"type": status}, "canAcceptDirectInput": true}}}
	})
	host.Respond("thread/goal/get", fakehost.Reply{Result: map[string]any{"goal": map[string]any{"status": nil}}})
	host.Handle("thread/list", func(raw json.RawMessage) fakehost.Reply {
		var params map[string]any
		_ = json.Unmarshal(raw, &params)
		data := []any{}
		if params["archived"] == false {
			data = append(data, map[string]any{"id": "supervisor"})
		}
		return fakehost.Reply{Result: map[string]any{"data": data, "nextCursor": nil}}
	})
	host.Respond("thread/resume", fakehost.Reply{Result: resume()})
	host.Respond("thread/unsubscribe", fakehost.Reply{Result: map[string]any{"status": "unsubscribed"}})
	host.Respond("turn/start", fakehost.Reply{Result: map[string]any{"turn": map[string]any{"id": "supervisor-turn"}}})
	host.Respond("thread/turns/list", fakehost.Reply{Result: map[string]any{"data": []any{}, "nextCursor": nil}})
	host.Respond("thread/items/list", fakehost.Reply{Result: map[string]any{"data": []any{}, "nextCursor": nil}})

	state := t.TempDir()
	id := seedSupervisorCLI(t, state, host.SocketPath)
	s, err := openStore(ctx, filepath.Join(state, "relay.sqlite3"), host.SocketPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	client := appserver.New(host.SocketPath, appserver.DefaultBounds)
	l, err := ledger.OpenWithOptions(filepath.Join(t.TempDir(), "operations.sqlite3"), ledger.Options{Encode: encodeReceipt})
	if err != nil {
		t.Fatal(err)
	}
	a := New(Options{RPC: client, Ledger: l, Policy: execution.Policy{}})
	t.Cleanup(func() { _ = a.Close() })
	channel := &supervisor.Channel{Store: s, Linkage: supervisor.StoreLinkage{Store: s}, Program: "codex-session-relay", Socket: host.SocketPath, Settings: &delivery.TaskSettings{Data: ordered(authorized()).(delivery.Obj)}}
	d := daemon.New(s, a, &delivery.FakeClock{T: now}, channel)

	// Busy: the report is deferred with the real backoff and the supervisor's thread is held.
	if _, err := d.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	row, err := channel.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if row.State != "deferred_busy" || !row.NextEligibleAt.Valid || row.NextEligibleAt.Float64 <= now {
		t.Fatalf("after the busy tick the message is %+v, want deferred_busy with a backoff ahead", row)
	}
	deadline := row.NextEligibleAt.Float64
	if got := host.Count("thread/resume"); got != 1 {
		t.Fatalf("%d resumes reached the host, want the relay's own hold on the supervisor", got)
	}
	if got := host.Count("turn/start"); got != 0 {
		t.Fatalf("the busy tick started %d turns; a busy recipient is never interrupted", got)
	}

	// The host unloads the supervisor and pushes the notLoaded edge for the subscribed thread.
	setStatus("notLoaded")
	host.Respond("probe/unloaded", fakehost.Reply{Before: []fakehost.Notification{{
		Method: "thread/status/changed",
		Params: map[string]any{"threadId": "supervisor", "status": map[string]any{"type": "notLoaded"}},
	}}})
	if _, err := a.HostCall(ctx, "probe/unloaded", nil); err != nil {
		t.Fatal(err)
	}
	before := len(host.Requests())

	// The next tick, at the same instant and so long before the deadline, wakes the message and sends
	// it: the thread is loaded with thread/resume and the turn is opened with turn/start.
	if _, err := d.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if now >= deadline {
		t.Fatalf("the clock reached the deadline %.0f; the wake is not what sent the message", deadline)
	}
	var woken int
	if err := s.DB.QueryRow("SELECT COUNT(*) FROM journal WHERE kind = 'supervisor_message_woken' AND subject = ?", id).Scan(&woken); err != nil || woken != 1 {
		t.Fatalf("%d supervisor_message_woken rows (%v), want 1", woken, err)
	}
	row, err = channel.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if row.State != "dispatched" {
		t.Fatalf("the woken message is %s, want dispatched", row.State)
	}
	var methods []string
	for _, request := range host.Requests()[before:] {
		methods = append(methods, request.Method)
	}
	resumed, started := -1, -1
	for i, method := range methods {
		if method == "thread/resume" && resumed < 0 {
			resumed = i
		}
		if method == "turn/start" {
			started = i
		}
	}
	if resumed < 0 || started < resumed {
		t.Fatalf("the woken send made %v, want thread/resume and then turn/start", methods)
	}
	if got := host.Count("turn/start"); got != 1 {
		t.Fatalf("%d turns started, want the one the woken send opened", got)
	}
	attempts, err := s.SupervisorAttempts(ctx, id)
	if err != nil || len(attempts) != 1 || attempts[0].State != "dispatched" || attempts[0].TurnID.String != "supervisor-turn" {
		t.Fatalf("attempts %+v %v, want the one dispatched attempt in supervisor-turn", attempts, err)
	}
}
