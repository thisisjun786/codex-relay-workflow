package role

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver/fakehost"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/host"
)

// dispatchHostCodexHome makes a CODEX_HOME whose app-server-control/app-server-control.sock is a
// symlink to the fake server's socket, which is what the CLI resolves and dials. A unix socket path
// has a platform length bound (103 bytes), so the home is made with a short name under TMPDIR rather
// than t.TempDir(), whose path carries the test's own name.
func dispatchHostCodexHome(t *testing.T, socket string) string {
	t.Helper()
	home, err := os.MkdirTemp("", "crw653-")
	check(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(home) })
	dir := filepath.Join(home, "app-server-control")
	check(t, os.MkdirAll(dir, 0o700))
	link := filepath.Join(dir, "app-server-control.sock")
	if len(link) > 103 {
		t.Fatalf("CODEX_HOME socket path is %d bytes, over the 103-byte unix bound: %s", len(link), link)
	}
	check(t, os.Symlink(socket, link))
	return home
}

// dispatchHostEnv points CODEX_HOME at dir and leaves HOME and CRW_HOME temporary.
func dispatchHostEnv(env host.LookupEnv, dir string) host.LookupEnv {
	return func(key string) (string, bool) {
		if key == "CODEX_HOME" {
			return dir, true
		}
		return env(key)
	}
}

// dispatchHostFake answers the two reads the created check makes: thread/read with child-a, a
// subagent of session-test, and thread/turns/list with one turn of the given status.
func dispatchHostFake(t *testing.T, turn string) *fakehost.Server {
	t.Helper()
	server := fakehost.Start(t)
	server.Respond("thread/read", fakehost.Reply{Result: map[string]any{"thread": map[string]any{
		"id": "child-a", "parentThreadId": "session-test", "threadSource": "subagent",
		"status": map[string]any{"type": "idle"},
	}}})
	server.Respond("thread/turns/list", fakehost.Reply{Result: map[string]any{
		"data": []any{map[string]any{"id": "turn-1", "status": turn}}, "nextCursor": nil,
	}})
	return server
}

// dispatchHostCommand runs one dispatch command through the CLI entry point, as the host does.
func dispatchHostCommand(t *testing.T, env host.LookupEnv, fields map[string]any) (DispatchResult, error) {
	t.Helper()
	b := map[string]any{"sessionId": "session-test", "dispatchId": "task-test"}
	for k, v := range fields {
		b[k] = v
	}
	var out bytes.Buffer
	code := DispatchCommand(nil, strings.NewReader(string(must(json.Marshal(b)))), &out, env)
	if code != 0 {
		var answer map[string]string
		check(t, json.Unmarshal(out.Bytes(), &answer))
		return DispatchResult{}, errors.New(answer["error"])
	}
	var result DispatchResult
	check(t, json.Unmarshal(out.Bytes(), &result))
	return result, nil
}

// dispatchHostStarted drives start, claim and the created report through the CLI and returns the
// attempt id and the record's path.
func dispatchHostStarted(t *testing.T, ws string, env host.LookupEnv) (string, string) {
	t.Helper()
	start, err := dispatchHostCommand(t, env, map[string]any{"action": "start", "role": "executor"})
	check(t, err)
	if _, err := dispatchHostCommand(t, env, map[string]any{"action": "claim", "attemptId": start.AttemptID}); err != nil {
		t.Fatal(err)
	}
	if _, err := dispatchHostCommand(t, env, map[string]any{"action": "report", "attemptId": start.AttemptID, "outcome": "created", "agentId": "child-a"}); err != nil {
		t.Fatal(err)
	}
	return start.AttemptID, filepath.Join(ws, ".crw", "dispatches", "session-test", "task-test.json")
}

// dispatchHostStopped is the stopped close the CLI sends.
func dispatchHostStopped(attempt string) map[string]any {
	return map[string]any{"action": "report", "attemptId": attempt, "outcome": "stopped", "agentId": "child-a",
		"executionState": "stopped", "reconciliation": "child stopped; partial work inspected"}
}

// The CLI's stopped close refuses while the child's newest turn runs, because the CLI now passes a
// host: the fake socket's newest turn is inProgress and the record must stay as it was. On dev the
// CLI passes nil, closes the record and appends the "runtime not confirmed" note instead.
func TestDispatchCommandHostRefusesAStoppedCloseWhileTheChildTurnRuns(t *testing.T) {
	ws := t.TempDir()
	t.Chdir(ws)
	server := dispatchHostFake(t, "inProgress")
	codexHome := dispatchHostCodexHome(t, server.SocketPath)
	// The same home carries the native database the CLI used before it had a host, so the created
	// report succeeds on dev too and the only difference this issue makes is the stopped close.
	createdCheckSeed(t, codexHome, "child-a", "session-test")
	env, _ := home(t)
	env = dispatchHostEnv(env, codexHome)
	attempt, file := dispatchHostStarted(t, ws, env)
	before := must(os.ReadFile(file))
	_, err := dispatchHostCommand(t, env, dispatchHostStopped(attempt))
	if err == nil || err.Error() != "recorded child has a turn in progress; stop it before closing" {
		t.Fatalf("close of a child with a turn in progress: %v", err)
	}
	if string(before) != string(must(os.ReadFile(file))) {
		t.Fatal("the refused close wrote state")
	}
}

// With no socket the CLI answers exactly as it does on dev: the close goes through and says the
// runtime was not confirmed.
func TestDispatchCommandWithoutASocketClosesAsOnDev(t *testing.T) {
	ws := t.TempDir()
	t.Chdir(ws)
	native := t.TempDir()
	createdCheckSeed(t, native, "child-a", "session-test")
	env, _ := home(t)
	env = dispatchHostEnv(env, native)
	attempt, file := dispatchHostStarted(t, ws, env)
	out, err := dispatchHostCommand(t, env, dispatchHostStopped(attempt))
	check(t, err)
	if out.Action != "stop" || out.Reason != createdRuntimeClosed+"; "+createdRuntimeNote {
		t.Fatalf("close without a socket = %q %q", out.Action, out.Reason)
	}
	if stored := must(dispatchRead(file, "session-test", "task-test")); stored.Status != "stopped" {
		t.Fatalf("closure not durable: %+v", stored)
	}
}

// The host forwards only the two reads, without turns and with one entry, and refuses everything
// else without sending it.
func TestDispatchHostForwardsOnlyTheTwoReads(t *testing.T) {
	server := dispatchHostFake(t, "completed")
	env, _ := home(t)
	env = dispatchHostEnv(env, dispatchHostCodexHome(t, server.SocketPath))
	h, closeHost := dispatchHostOpen(context.Background(), env)
	if h == nil {
		t.Fatal("the host was not opened against a live socket")
	}
	defer closeHost()
	ctx := context.Background()
	if _, err := h.Call(ctx, "thread/read", map[string]any{"threadId": "child-a", "includeTurns": false}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Call(ctx, "thread/turns/list", map[string]any{"threadId": "child-a", "limit": 1, "itemsView": "summary"}); err != nil {
		t.Fatal(err)
	}
	for _, refused := range []struct {
		method string
		params map[string]any
	}{
		{"turn/start", map[string]any{"threadId": "child-a"}},
		{"thread/resume", map[string]any{"threadId": "child-a"}},
		{"thread/archive", map[string]any{"threadId": "child-a"}},
		{"thread/read", map[string]any{"threadId": "child-a", "includeTurns": true}},
		{"thread/turns/list", map[string]any{"threadId": "child-a", "limit": 5}},
	} {
		if _, err := h.Call(ctx, refused.method, refused.params); err == nil {
			t.Fatalf("%s %v was not refused", refused.method, refused.params)
		}
	}
	for _, method := range []string{"turn/start", "thread/resume", "thread/archive"} {
		if n := server.Count(method); n != 0 {
			t.Fatalf("%s reached the host %d times", method, n)
		}
	}
}

// A socket that accepts a connection but never finishes the handshake does not hold the CLI: the
// dial is bounded and no host is passed.
func TestDispatchHostDialIsBounded(t *testing.T) {
	dir, err := os.MkdirTemp("", "crw653-")
	check(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	link := filepath.Join(dir, "app-server-control", "app-server-control.sock")
	check(t, os.MkdirAll(filepath.Dir(link), 0o700))
	listener, err := net.Listen("unix", link)
	check(t, err)
	t.Cleanup(func() { _ = listener.Close() })
	var mu sync.Mutex
	var held []net.Conn
	t.Cleanup(func() {
		mu.Lock()
		defer mu.Unlock()
		for _, conn := range held {
			_ = conn.Close()
		}
	})
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			held = append(held, conn)
			mu.Unlock()
		}
	}()
	env, _ := home(t)
	env = dispatchHostEnv(env, dir)
	begin := time.Now()
	h, closeHost := dispatchHostOpen(context.Background(), env)
	defer closeHost()
	if h != nil {
		t.Fatal("a socket that never answers must not produce a host")
	}
	if elapsed := time.Since(begin); elapsed > 10*time.Second {
		t.Fatalf("the dial took %v", elapsed)
	}
}

// dispatchHostSocket resolves the same default socket the bridge and the relay use: CODEX_HOME
// when it is set and non-empty, else Path.home()/.codex, with an empty home or one of slashes
// being the root and trailing slashes dropped (internal/bridge/mcp Defaults and its own pin,
// internal/relay/store DefaultSocket).
func TestDispatchHostSocketMatchesTheBridgeDefault(t *testing.T) {
	for _, tc := range []struct {
		name string
		vars map[string]string
		want string
	}{
		{"home", map[string]string{"HOME": "/h"}, "/h/.codex/app-server-control/app-server-control.sock"},
		{"codex-home", map[string]string{"HOME": "/h", "CODEX_HOME": "/c"}, "/c/app-server-control/app-server-control.sock"},
		{"empty codex-home is unset", map[string]string{"HOME": "/h", "CODEX_HOME": ""}, "/h/.codex/app-server-control/app-server-control.sock"},
		{"empty home is the root", map[string]string{"HOME": ""}, "/.codex/app-server-control/app-server-control.sock"},
		{"slashes are the root", map[string]string{"HOME": "///"}, "/.codex/app-server-control/app-server-control.sock"},
		{"trailing slashes are dropped", map[string]string{"HOME": "/h//"}, "/h/.codex/app-server-control/app-server-control.sock"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := func(key string) (string, bool) { v, ok := tc.vars[key]; return v, ok }
			got, err := dispatchHostSocket(env)
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("socket %q, want %q", got, tc.want)
			}
		})
	}
}
