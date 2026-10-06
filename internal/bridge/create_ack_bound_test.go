package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver"
	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver/fakehost"
	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/execution"
	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/ledger"
)

// CRW-788. Under App Server load the host answered the create path's calls after the client's
// default ack bound, and a creation whose answer was lost keeps an outcome_unknown receipt
// although the host did the work. The create path therefore raises the ack bound for its own
// context, and only for it: every other caller keeps the client's bound.
//
// The client's bounds are scaled down here so the default ack is reachable inside a test; the
// create path's own constant is untouched.

// createAckBoundIsSixtySeconds pins the bound the issue names.
func TestCreatePathAckBoundIsSixtySeconds(t *testing.T) {
	if createAckBound != 60*time.Second {
		t.Fatalf("the create path's ack bound is %s", createAckBound)
	}
}

// slowCreateBridge is a bridge over a client whose default ack bound is scaled down, so a test can
// reach it in milliseconds.
func slowCreateBridge(t *testing.T, ack time.Duration) (*Bridge, *fakehost.Server, *appserver.Client) {
	t.Helper()
	host := fakehost.Start(t)
	client := appserver.New(host.SocketPath, appserver.PhaseBounds{Establish: time.Second, Transmit: time.Second, Ack: ack})
	if err := client.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	store, err := ledger.Open(filepath.Join(t.TempDir(), "operations.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	policy, err := execution.FromBytes([]byte(mcpChildPolicy), "test")
	if err != nil {
		t.Fatal(err)
	}
	return New(client, store, policy), host, client
}

// slowCreateScript answers every call of a creation under the child role's ui-qa profile, each held
// back longer than the client's scaled default ack bound: the profile reads, thread/start,
// thread/name/set, the status list and turn/start.
func slowCreateScript(host *fakehost.Server, cwd string, delay time.Duration) {
	servers := map[string]any{}
	for _, name := range mcpConfigured {
		servers[name] = map[string]any{"enabled": true}
	}
	host.Respond("config/read", fakehost.Reply{Result: map[string]any{"config": map[string]any{"mcp_servers": servers}}, Delay: delay})
	host.Respond("plugin/installed", fakehost.Reply{Result: map[string]any{"marketplaces": []any{map[string]any{"name": "openai-bundled", "plugins": []any{map[string]any{"id": "cua@openai-bundled", "installed": true, "enabled": true}}}}}, Delay: delay})
	start := startReply(cwd)
	start.Result["model"], start.Result["reasoningEffort"] = pyModel, pyEffort
	start.Delay = delay
	host.Respond("thread/start", start)
	host.Respond("thread/name/set", fakehost.Reply{Delay: delay})
	host.Respond("thread/read", fakehost.Reply{Result: map[string]any{"thread": map[string]any{"status": map[string]any{"type": "idle"}}}, Delay: delay})
	host.Respond("turn/start", fakehost.Reply{Result: map[string]any{"turn": map[string]any{"id": "turn-1"}}, Delay: delay})
	host.Handle("mcpServerStatus/list", func(json.RawMessage) fakehost.Reply {
		rows := []any{}
		for _, name := range mcpConfigured {
			status := "disabled"
			if name == "node_repl" {
				status = "connected" // the ui-qa profile keeps it
			}
			rows = append(rows, map[string]any{"name": name, "pluginId": nil, "runtimeStatus": status})
		}
		rows = append(rows, map[string]any{"name": "cua_repl", "pluginId": "cua@openai-bundled", "runtimeStatus": "connected"})
		return fakehost.Reply{Result: map[string]any{"data": rows, "nextCursor": nil}, Delay: delay}
	})
}

// Every App Server call the create path makes succeeds although the host answers later than the
// client's default ack bound, the mcpServerStatus/list profile check included, while a call outside
// the create path on the same client times out at that default and names the bound it used.
func TestCreatePathCallsUseTheRaisedAckBoundAndOtherCallsDoNot(t *testing.T) {
	t.Parallel()
	const ack = 40 * time.Millisecond
	b, host, client := slowCreateBridge(t, ack)
	cwd := t.TempDir()
	slowCreateScript(host, cwd, 3*ack)
	in := mcpCreate(cwd, "create-slow-ack", "ui-qa")
	in.Title = "Verify"
	receipt, err := b.CreateThread(context.Background(), in)
	if err != nil || receipt["status"] != "accepted" {
		t.Fatalf("a creation the host answered past the default ack bound failed: receipt=%v err=%v", receipt, err)
	}
	for _, method := range []string{"config/read", "thread/start", "mcpServerStatus/list", "thread/name/set", "turn/start"} {
		if host.Count(method) != 1 {
			t.Fatalf("%s was asked %d times", method, host.Count(method))
		}
	}
	// The same client, on a context that is not the create path's, keeps the default bound.
	host.Respond("thread/list", fakehost.Reply{Result: map[string]any{}, Delay: 3 * ack})
	_, err = client.Call(context.Background(), "thread/list", map[string]any{})
	var timeout *appserver.PhaseTimeout
	if !errors.As(err, &timeout) || timeout.Phase != "ack" {
		t.Fatalf("a call outside the create path did not time out at its ack bound: %v", err)
	}
	if timeout.Bound != ack {
		t.Fatalf("PhaseTimeout names %s, want the client's own %s", timeout.Bound, ack)
	}
}

// A context that names its own bound is the one the timeout reports, so the bound a call used is
// readable from the error.
func TestAckBoundFromTheContextIsTheBoundReported(t *testing.T) {
	t.Parallel()
	const ack = 40 * time.Millisecond
	_, host, client := slowCreateBridge(t, ack)
	host.Respond("thread/list", fakehost.Reply{Result: map[string]any{}, Delay: 5 * ack})
	ctx := appserver.WithAckBound(context.Background(), 3*ack)
	_, err := client.Call(ctx, "thread/list", map[string]any{})
	var timeout *appserver.PhaseTimeout
	if !errors.As(err, &timeout) || timeout.Phase != "ack" || timeout.Bound != 3*ack {
		t.Fatalf("want an ack timeout at %s, got %v", 3*ack, err)
	}
}
