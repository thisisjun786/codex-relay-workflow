package appserver_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver"
	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver/fakehost"
)

// fixtureFile reads a recorded frame or result under testdata/fixtures.
func fixtureFile(t *testing.T, name string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "fixtures", name+".json"))
	if err != nil {
		t.Fatal(err)
	}
	var value map[string]any
	if err := json.Unmarshal(raw, &value); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return value
}

// notificationFor is a recorded notification about the given thread.
func notificationFor(t *testing.T, name, thread string) fakehost.Notification {
	t.Helper()
	frame := fixtureFile(t, name)
	params, _ := frame["params"].(map[string]any)
	if thread != "" {
		params["threadId"] = thread
	} else {
		delete(params, "threadId")
	}
	return fakehost.Notification{Method: frame["method"].(string), Params: params}
}

// next takes one notification from a feed or fails after a bounded wait.
func next(t *testing.T, feed *appserver.Feed) appserver.Notification {
	t.Helper()
	select {
	case n, open := <-feed.Notifications():
		if !open {
			t.Fatal("feed closed")
		}
		return n
	case <-time.After(5 * time.Second):
		t.Fatal("no notification on the feed")
	}
	return appserver.Notification{}
}

func quiet(t *testing.T, feed *appserver.Feed) {
	t.Helper()
	select {
	case n, open := <-feed.Notifications():
		if open {
			t.Fatalf("unexpected notification %s %s", n.Method, n.Params)
		}
	default:
	}
}

func methodsOf(ns []appserver.Notification) []string {
	out := []string{}
	for _, n := range ns {
		out = append(out, n.Method)
	}
	return out
}

// CRW-1193. One connection carries the notifications of every thread; the bus gives each thread's
// consumer only its own, in the order the host sent them, and leaves Client.Notifications() as it was.
func TestBusDemuxPerThread(t *testing.T) {
	// Given: a host that raises a mix of recorded notifications of two threads, an unrelated
	// notification, one without a thread, and one of a thread nobody follows, ahead of one answer.
	host := fakehost.Start(t)
	host.Script("turn/start", fakehost.Reply{Result: map[string]any{"turn": map[string]any{"id": "turn-a1"}}, Before: []fakehost.Notification{
		notificationFor(t, "notification-turn-completed", "thread-a"),
		notificationFor(t, "notification-item-completed", "thread-b"),
		notificationFor(t, "notification-turn-started", "thread-a"),
		notificationFor(t, "notification-item-completed", "thread-a"),
		notificationFor(t, "notification-token-usage-updated", "thread-b"),
		notificationFor(t, "notification-error", ""),
		notificationFor(t, "notification-thread-status-changed", "thread-a"),
		notificationFor(t, "notification-error", "thread-b"),
		notificationFor(t, "notification-thread-status-changed", "thread-z"),
	}})
	client, err := appserver.Dial(bounded(t), host.SocketPath)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	a := client.Bus().Subscribe("thread-a")
	b := client.Bus().Subscribe("thread-b")
	idle := client.Bus().Subscribe("thread-idle")
	defer a.Close()
	defer b.Close()
	defer idle.Close()

	// When
	if _, err := client.Call(bounded(t), "turn/start", map[string]any{"threadId": "thread-a", "input": []any{}}); err != nil {
		t.Fatal(err)
	}

	// Then: each feed holds its own thread's five-kind notifications in order, nothing else.
	var gotA, gotB []appserver.Notification
	for range 3 {
		gotA = append(gotA, next(t, a))
	}
	for range 3 {
		gotB = append(gotB, next(t, b))
	}
	if want := []string{"turn/completed", "item/completed", "thread/status/changed"}; !reflect.DeepEqual(methodsOf(gotA), want) {
		t.Fatalf("thread-a: %v, want %v", methodsOf(gotA), want)
	}
	if want := []string{"item/completed", "thread/tokenUsage/updated", "error"}; !reflect.DeepEqual(methodsOf(gotB), want) {
		t.Fatalf("thread-b: %v, want %v", methodsOf(gotB), want)
	}
	for _, n := range append(gotA, gotB...) {
		if got := n.ThreadID(); got != "thread-a" && got != "thread-b" {
			t.Fatalf("a foreign thread reached a feed: %s", n.Params)
		}
	}
	quiet(t, a)
	quiet(t, b)
	quiet(t, idle)
	if a.Dropped() != 0 {
		t.Fatalf("dropped = %d", a.Dropped())
	}

	// The shared channel still carries every notification, unfiltered: the bus is an addition.
	var shared []string
	for range 9 {
		select {
		case n := <-client.Notifications():
			shared = append(shared, n.Method)
		case <-time.After(5 * time.Second):
			t.Fatalf("shared channel held only %v", shared)
		}
	}
	if len(shared) != 9 || shared[2] != "turn/started" || shared[5] != "error" {
		t.Fatalf("shared channel = %v", shared)
	}

	// A closed feed receives nothing more and its channel ends.
	a.Close()
	host.Script("turn/start", fakehost.Reply{Before: []fakehost.Notification{notificationFor(t, "notification-turn-completed", "thread-a")}})
	if _, err := client.Call(bounded(t), "turn/start", map[string]any{"threadId": "thread-a", "input": []any{}}); err != nil {
		t.Fatal(err)
	}
	if n, open := <-a.Notifications(); open {
		t.Fatalf("closed feed delivered %s", n.Method)
	}
}

// A consumer that stops reading loses its oldest notifications, never the newest, and is told:
// a lost turn/completed is recovered by reading the turn, which only a consumer that knows it
// missed something does.
func TestBusOverflowDropsOldestAndSignalsLost(t *testing.T) {
	host := fakehost.Start(t)
	var before []fakehost.Notification
	for i := range 5 {
		n := notificationFor(t, "notification-token-usage-updated", "thread-a")
		n.Params["turnId"] = "turn-" + string(rune('0'+i))
		before = append(before, n)
	}
	host.Script("turn/start", fakehost.Reply{Before: before})
	client, err := appserver.Dial(bounded(t), host.SocketPath)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	feed := client.Bus().SubscribeBuffered("thread-a", 2)
	defer feed.Close()
	if _, err := client.Call(bounded(t), "turn/start", map[string]any{"threadId": "thread-a", "input": []any{}}); err != nil {
		t.Fatal(err)
	}
	if feed.Dropped() != 3 {
		t.Fatalf("dropped = %d, want 3", feed.Dropped())
	}
	select {
	case <-feed.Lost():
	default:
		t.Fatal("overflow did not signal")
	}
	var turns []string
	for range 2 {
		var params struct {
			TurnID string `json:"turnId"`
		}
		if err := json.Unmarshal(next(t, feed).Params, &params); err != nil {
			t.Fatal(err)
		}
		turns = append(turns, params.TurnID)
	}
	if !reflect.DeepEqual(turns, []string{"turn-3", "turn-4"}) {
		t.Fatalf("kept %v, want the newest two", turns)
	}
}

// reconnected returns a client whose connection the host ended, having made the loss visible on feed.
func reconnected(t *testing.T, host *fakehost.Server, thread string) (*appserver.Client, *appserver.Feed) {
	t.Helper()
	client, err := appserver.Dial(bounded(t), host.SocketPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	feed := client.Bus().Subscribe(thread)
	t.Cleanup(feed.Close)
	host.Script("thread/read", fakehost.Reply{Close: &fakehost.CloseFrame{Code: websocket.StatusGoingAway, Reason: "host restart"}})
	if _, err := client.Call(bounded(t), "thread/read", map[string]any{"threadId": thread}); err == nil {
		t.Fatal("the closed connection answered")
	}
	select {
	case <-feed.Lost():
	case <-time.After(5 * time.Second):
		t.Fatal("the loss of the connection was not signalled")
	}
	return client, feed
}

func requestsOf(host *fakehost.Server, method string) []string {
	var out []string
	for _, r := range host.Requests() {
		if r.Method == method {
			out = append(out, string(r.Params))
		}
	}
	return out
}

// CRW-1193. After the connection is lost and re-established the bus does not guess what happened to
// the turns it was waiting for: it reads each one by its exact id, first as full turns and, for a
// turn whose items were not returned in full, item by item. includeTurns hydration is never used.
func TestBusReconnectReconcilesTurns(t *testing.T) {
	// Given: a host with three turns, the wanted turn-a only summarised in the turn list.
	host := fakehost.Start(t)
	host.Handle("thread/turns/list", func(params json.RawMessage) fakehost.Reply {
		var p struct {
			Cursor string `json:"cursor"`
		}
		_ = json.Unmarshal(params, &p)
		if p.Cursor == "cursor-2" {
			return fakehost.Reply{Result: fixtureFile(t, "turns-list-page2")}
		}
		return fakehost.Reply{Result: fixtureFile(t, "turns-list-page1")}
	})
	host.Handle("thread/items/list", func(params json.RawMessage) fakehost.Reply {
		var p struct {
			Cursor string `json:"cursor"`
		}
		_ = json.Unmarshal(params, &p)
		if p.Cursor == "items-2" {
			return fakehost.Reply{Result: fixtureFile(t, "items-list-turn-a-page2")}
		}
		return fakehost.Reply{Result: fixtureFile(t, "items-list-turn-a-page1")}
	})
	client, _ := reconnected(t, host, "thread-1")

	// When: the turns the backend journalled are reconciled; turn-missing never existed.
	readings, err := client.Bus().ReconcileTurns(bounded(t), "thread-1", []string{"turn-a", "turn-b", "turn-missing"})
	if err != nil {
		t.Fatal(err)
	}

	// Then: a second connection was made, and the readings are per exact id, in the order asked.
	if host.Count("initialize") != 2 {
		t.Fatalf("initialize count = %d, want a reconnect", host.Count("initialize"))
	}
	if len(readings) != 3 || readings[0].TurnID != "turn-a" || readings[1].TurnID != "turn-b" || readings[2].TurnID != "turn-missing" {
		t.Fatalf("readings = %+v", readings)
	}
	b := readings[1]
	if !b.Found || !b.Readable || b.Status != "completed" || len(b.Items) != 2 {
		t.Fatalf("turn-b: %+v", b)
	}
	a := readings[0]
	if !a.Found || !a.Readable || a.Status != "completed" || len(a.Items) != 2 {
		t.Fatalf("turn-a read item by item: %+v", a)
	}
	if missing := readings[2]; missing.Found || missing.Readable {
		t.Fatalf("turn-missing: %+v", missing)
	}
	// The pulls, exactly: full turns paged by cursor, then the summarised turn's items by turn id.
	if got, want := requestsOf(host, "thread/turns/list"), []string{`{"itemsView":"full","threadId":"thread-1"}`, `{"cursor":"cursor-2","itemsView":"full","threadId":"thread-1"}`}; !reflect.DeepEqual(got, want) {
		t.Fatalf("thread/turns/list = %v", got)
	}
	if got, want := requestsOf(host, "thread/items/list"), []string{`{"threadId":"thread-1","turnId":"turn-a"}`, `{"cursor":"items-2","threadId":"thread-1","turnId":"turn-a"}`}; !reflect.DeepEqual(got, want) {
		t.Fatalf("thread/items/list = %v", got)
	}
	for _, r := range host.Requests() {
		if strings.Contains(string(r.Params), "includeTurns") {
			t.Fatalf("includeTurns hydration used: %s %s", r.Method, r.Params)
		}
	}
}

// Paging stops at the last wanted turn, and a turn whose items cannot be read is unreadable, not empty.
func TestBusReconcileStopsAtWantedAndKeepsUnreadableApart(t *testing.T) {
	host := fakehost.Start(t)
	host.Handle("thread/turns/list", func(params json.RawMessage) fakehost.Reply {
		if strings.Contains(string(params), "cursor") {
			return fakehost.Reply{Result: fixtureFile(t, "turns-list-page2")}
		}
		return fakehost.Reply{Result: fixtureFile(t, "turns-list-page1")}
	})
	host.Respond("thread/items/list", fakehost.Reply{Error: &fakehost.RPCError{Code: -32000, Message: "thread items unavailable"}})
	client, err := appserver.Dial(bounded(t), host.SocketPath)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	readings, err := client.Bus().ReconcileTurns(bounded(t), "thread-1", []string{"turn-b"})
	if err != nil || len(readings) != 1 || !readings[0].Readable {
		t.Fatalf("%+v %v", readings, err)
	}
	if n := len(requestsOf(host, "thread/turns/list")); n != 1 {
		t.Fatalf("paged past the wanted turn: %d requests", n)
	}

	readings, err = client.Bus().ReconcileTurns(bounded(t), "thread-1", []string{"turn-a"})
	if err != nil {
		t.Fatal(err)
	}
	if a := readings[0]; !a.Found || a.Readable || a.ReadError == nil || len(a.Items) != 0 || a.Status != "completed" {
		t.Fatalf("an unreadable turn must say so: %+v", a)
	}

	// A host that refuses the list itself (Codex 0.154 does for an ephemeral thread) is an error, not an empty answer.
	host.Script("thread/turns/list", fakehost.Reply{Error: &fakehost.RPCError{Code: -32600, Message: "ephemeral threads do not support thread/turns/list"}})
	if _, err := client.Bus().ReconcileTurns(bounded(t), "thread-1", []string{"turn-a"}); err == nil {
		t.Fatal("a refused list read as an empty thread")
	}
}

// CRW-1193. Reading the turns after a reconnect recovers history; it does not re-subscribe the new
// connection. A turn still in progress completes on a live notification only after the caller resumes
// the thread on that connection, so a consumer that follows Lost resumes the thread (thread/resume)
// whenever a reading says inProgress, and then goes back to waiting on the same feed.
func TestBusReconcileDoesNotResubscribeAndResumeRestoresLiveDelivery(t *testing.T) {
	host := fakehost.Start(t)
	host.Respond("thread/turns/list", fakehost.Reply{Result: map[string]any{"data": []any{
		map[string]any{"id": "turn-a1", "status": "inProgress", "itemsView": "full", "items": []any{}},
	}, "nextCursor": nil}})
	var resumed atomic.Bool
	host.Handle("thread/resume", func(json.RawMessage) fakehost.Reply {
		resumed.Store(true)
		return fakehost.Reply{Result: map[string]any{"thread": map[string]any{"id": "thread-a"}}}
	})
	// The host reports the turn's completion on a connection only once the thread is subscribed on it.
	host.Handle("thread/read", func(json.RawMessage) fakehost.Reply {
		if !resumed.Load() {
			return fakehost.Reply{Result: map[string]any{}}
		}
		return fakehost.Reply{Result: map[string]any{}, Before: []fakehost.Notification{notificationFor(t, "notification-turn-completed", "thread-a")}}
	})
	client, feed := reconnected(t, host, "thread-a")

	// When: the consumer reconciles after the loss.
	readings, err := client.Bus().ReconcileTurns(bounded(t), "thread-a", []string{"turn-a1"})
	if err != nil || len(readings) != 1 || !readings[0].Found || readings[0].Status != "inProgress" {
		t.Fatalf("%+v %v", readings, err)
	}

	// Then: the reads sent no resume, and a later turn event is not delivered on the unsubscribed connection.
	if n := host.Count("thread/resume"); n != 0 {
		t.Fatalf("ReconcileTurns subscribed the thread: %d thread/resume", n)
	}
	if _, err := client.Call(bounded(t), "thread/read", map[string]any{"threadId": "thread-a"}); err != nil {
		t.Fatal(err)
	}
	select {
	case n := <-feed.Notifications():
		t.Fatalf("a notification arrived before the thread was resumed: %s", n.Method)
	case <-time.After(200 * time.Millisecond):
	}

	// When: the consumer resumes the thread on the new connection, the same feed gets the completion.
	if _, err := client.Call(bounded(t), "thread/resume", map[string]any{"threadId": "thread-a"}); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Call(bounded(t), "thread/read", map[string]any{"threadId": "thread-a"}); err != nil {
		t.Fatal(err)
	}
	if n := next(t, feed); n.Method != "turn/completed" {
		t.Fatalf("method = %s", n.Method)
	}
}
