package adapter

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver"
	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver/fakehost"
	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/execution"
	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/ledger"
)

// CRW-904: the relay's host side of the idle edge. HoldThread subscribes the relay's own connection
// to a recipient whose delivery waits out a busy backoff, without overrides and without a watch, and
// IdleReports reads the status reports that subscription makes arrive. The scripted App Server is
// what proves both: what the resume carried, and which notifications become reports.

func idleAdapter(t *testing.T) (*Adapter, *fakehost.Server) {
	t.Helper()
	host := fakehost.Start(t)
	client := appserver.New(host.SocketPath, appserver.DefaultBounds)
	l, err := ledger.OpenWithOptions(filepath.Join(t.TempDir(), "operations.sqlite3"), ledger.Options{Encode: encodeReceipt})
	if err != nil {
		t.Fatal(err)
	}
	a := New(Options{RPC: client, Ledger: l, Policy: execution.Policy{}})
	t.Cleanup(func() { _ = a.Close() })
	host.Respond("thread/resume", fakehost.Reply{Result: resume()})
	host.Respond("thread/unsubscribe", fakehost.Reply{Result: map[string]any{"status": "unsubscribed"}})
	return a, host
}

func TestIdleReports_reads_the_status_the_subscription_makes_arrive(t *testing.T) {
	a, host := idleAdapter(t)
	if reports := a.IdleReports(); len(reports) != 0 {
		t.Fatalf("reports before any subscription: %v", reports)
	}
	if err := a.HoldThread(context.Background(), "parent"); err != nil {
		t.Fatal(err)
	}
	// The hold's resume carries no overrides and starts no turn: a busy recipient is not
	// interrupted and its settings are not changed.
	resumes := 0
	for _, request := range host.Requests() {
		if request.Method != "thread/resume" {
			continue
		}
		resumes++
		var params map[string]any
		if err := json.Unmarshal(request.Params, &params); err != nil {
			t.Fatal(err)
		}
		if len(params) != 2 || params["threadId"] != "parent" || params["excludeTurns"] != true {
			t.Fatalf("the hold's resume carries %v, want only threadId and excludeTurns", params)
		}
	}
	if resumes != 1 {
		t.Fatalf("%d resumes reached the host, want 1", resumes)
	}
	if got := host.Count("turn/start"); got != 0 {
		t.Fatalf("the hold started %d turns", got)
	}
	if !a.ThreadSubscribed("parent") {
		t.Fatal("the hold is not recorded")
	}
	// The status reports the App Server pushes for a subscribed thread, in both shapes the host
	// has been seen to use: an object carrying a type, and a bare string. A notification of
	// another method is not a status report.
	host.Respond("probe/idle", fakehost.Reply{Before: []fakehost.Notification{
		{Method: "thread/status/changed", Params: map[string]any{"threadId": "parent", "status": map[string]any{"type": "idle"}}},
		{Method: "thread/status/changed", Params: map[string]any{"threadId": "parent", "status": "notLoaded"}},
		{Method: "thread/status/changed", Params: map[string]any{"threadId": "parent", "status": map[string]any{"type": "active"}}},
		{Method: "turn/started", Params: map[string]any{"threadId": "parent"}},
	}})
	if _, err := a.HostCall(context.Background(), "probe/idle", nil); err != nil {
		t.Fatal(err)
	}
	reports := a.IdleReports()
	want := []string{"idle", "notLoaded", "active"}
	if len(reports) != len(want) {
		t.Fatalf("reports %v, want the three status reports and nothing else", reports)
	}
	for i, report := range reports {
		if report.ThreadID != "parent" || report.Status != want[i] {
			t.Fatalf("report %d is %v, want parent/%s", i, report, want[i])
		}
	}
	if reports := a.IdleReports(); len(reports) != 0 {
		t.Fatalf("a second read returned %v; the stream is drained, not repeated", reports)
	}
	// The backlog emptied: the hold is dropped and the subscription released on the same socket.
	a.ReleaseThread("parent")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := host.WaitCount(ctx, "thread/unsubscribe", 1); err != nil {
		t.Fatalf("the released hold did not unsubscribe: %v", err)
	}
	if a.ThreadSubscribed("parent") {
		t.Fatal("the released hold is still recorded")
	}
}

func TestIdleSubscription_a_transport_that_is_not_the_app_server_reports_nothing(t *testing.T) {
	a := New(Options{RPC: scriptedRPC{}, Policy: execution.Policy{}})
	if got := a.IdleReports(); len(got) != 0 {
		t.Fatalf("a scripted double reported %v", got)
	}
	if a.ThreadSubscribed("parent") {
		t.Fatal("a scripted double claims a subscription")
	}
	if err := a.HoldThread(context.Background(), "parent"); err == nil {
		t.Fatal("a scripted double accepted a hold")
	}
	// Releasing is silent, so a host that never held anything is not an error on the way out.
	a.ReleaseThread("parent")
}

// scriptedRPC is a transport that is not the App Server client: the bridge's own doubles have this
// shape, and the idle edge must leave them exactly as they were.
type scriptedRPC struct{}

func (scriptedRPC) Call(context.Context, string, map[string]any) (json.RawMessage, error) {
	return json.RawMessage(`{}`), nil
}
