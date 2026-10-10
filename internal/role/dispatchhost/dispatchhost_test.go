package dispatchhost

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver/fakehost"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/host"
	"github.com/thisisjun786/codex-relay-workflow/internal/role"
)

func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}

func check(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// codexHome makes a CODEX_HOME whose app-server-control/app-server-control.sock is a symlink to
// socket, which is what the opener resolves and dials. A unix socket path has a platform length
// bound (103 bytes), so the home is made with a short name under TMPDIR rather than t.TempDir().
func codexHome(t *testing.T, socket string) string {
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

// fake answers the two reads the stopped close makes: thread/read with child-a, a subagent of
// session-test, and thread/turns/list with one turn of the given status.
func fake(t *testing.T, turn string) *fakehost.Server {
	t.Helper()
	server := fakehost.Start(t)
	server.Respond(role.DispatchHostThreadRead, fakehost.Reply{Result: map[string]any{"thread": map[string]any{
		"id": "child-a", "parentThreadId": "session-test", "threadSource": "subagent",
		"status": map[string]any{"type": "idle"},
	}}})
	server.Respond(role.DispatchHostTurnsList, fakehost.Reply{Result: map[string]any{
		"data": []any{map[string]any{"id": "turn-1", "status": turn}}, "nextCursor": nil,
	}})
	return server
}

// seed writes the native row the created report reads, so the sequence reaches the stopped close.
func seed(t *testing.T, native, id, parent string) {
	t.Helper()
	db := must(sql.Open("sqlite", (&url.URL{Scheme: "file", Path: filepath.Join(native, "state_5.sqlite")}).String()))
	defer db.Close()
	_, err := db.Exec("CREATE TABLE IF NOT EXISTS threads (id TEXT PRIMARY KEY, source TEXT, archived INTEGER)")
	check(t, err)
	source := string(must(json.Marshal(map[string]any{"subagent": map[string]any{"thread_spawn": map[string]any{"parent_thread_id": parent, "depth": 1}}})))
	_, err = db.Exec("INSERT OR REPLACE INTO threads VALUES (?,?,0)", id, source)
	check(t, err)
}

func tempEnv(t *testing.T, codex string) host.LookupEnv {
	t.Helper()
	vars := map[string]string{"CRW_HOME": filepath.Join(t.TempDir(), "crw"), "HOME": t.TempDir(), "CODEX_HOME": codex}
	return func(key string) (string, bool) { v, ok := vars[key]; return v, ok }
}

func command(t *testing.T, env host.LookupEnv, fields map[string]any) (role.DispatchResult, error) {
	t.Helper()
	b := map[string]any{"sessionId": "session-test", "dispatchId": "task-test"}
	for k, v := range fields {
		b[k] = v
	}
	var out bytes.Buffer
	code := role.DispatchCommand(nil, strings.NewReader(string(must(json.Marshal(b)))), &out, env)
	if code != 0 {
		var answer map[string]string
		check(t, json.Unmarshal(out.Bytes(), &answer))
		return role.DispatchResult{}, errors.New(answer["error"])
	}
	var result role.DispatchResult
	check(t, json.Unmarshal(out.Bytes(), &result))
	return result, nil
}

// started drives start, claim and the created report through the CLI and returns the attempt id.
func started(t *testing.T, env host.LookupEnv) string {
	t.Helper()
	start, err := command(t, env, map[string]any{"action": "start", "role": "executor"})
	check(t, err)
	claim, err := command(t, env, map[string]any{"action": "claim", "attemptId": start.AttemptID})
	check(t, err)
	issue(t, claim.Marker)
	if _, err := command(t, env, map[string]any{"action": "report", "attemptId": start.AttemptID, "outcome": "created", "agentId": "child-a"}); err != nil {
		t.Fatal(err)
	}
	return start.AttemptID
}

func stopped(attempt string) map[string]any {
	return map[string]any{"action": "report", "attemptId": attempt, "outcome": "stopped", "agentId": "child-a",
		"executionState": "stopped", "reconciliation": "child stopped; partial work inspected"}
}

// issue is the spawn hook's issuance of the claimed attempt, which a created report requires; the CLI runs in the working
// directory, and so does the issuance.
func issue(t *testing.T, marker string) {
	t.Helper()
	cwd, err := os.Getwd()
	check(t, err)
	tool := "call-1"
	if _, err := role.IssueManagedSpawn(cwd, "session-test", marker+"\nTASK", &tool); err != nil {
		t.Fatal(err)
	}
}

// install makes role.OpenDispatchHost the real opener for one test and restores it after.
func install(t *testing.T) {
	t.Helper()
	was := role.OpenDispatchHost
	role.OpenDispatchHost = Open
	t.Cleanup(func() { role.OpenDispatchHost = was })
}

// The CLI's stopped close refuses while the child's newest turn runs, because the installed opener
// hands it a host: the fake socket's newest turn is inProgress and the record must stay as it was.
// On dev the CLI passes nil, closes the record and appends the "runtime not confirmed" note.
func TestDispatchCommandRefusesAStoppedCloseWhileTheChildTurnRuns(t *testing.T) {
	install(t)
	ws := t.TempDir()
	t.Chdir(ws)
	server := fake(t, "inProgress")
	codex := codexHome(t, server.SocketPath)
	seed(t, codex, "child-a", "session-test")
	env := tempEnv(t, codex)
	attempt := started(t, env)
	file := filepath.Join(ws, ".crw", "dispatches", "session-test", "task-test.json")
	before := must(os.ReadFile(file))
	_, err := command(t, env, stopped(attempt))
	if err == nil || err.Error() != "recorded child has a turn in progress; stop it before closing" {
		t.Fatalf("close of a child with a turn in progress: %v", err)
	}
	if string(before) != string(must(os.ReadFile(file))) {
		t.Fatal("the refused close wrote state")
	}
}

// Every other answer closes as on dev: the newest turn is seen to end and the note is gone.
func TestDispatchCommandClosesWhenTheNewestTurnEnded(t *testing.T) {
	install(t)
	ws := t.TempDir()
	t.Chdir(ws)
	server := fake(t, "completed")
	codex := codexHome(t, server.SocketPath)
	seed(t, codex, "child-a", "session-test")
	env := tempEnv(t, codex)
	out, err := command(t, env, stopped(started(t, env)))
	check(t, err)
	if out.Action != "stop" || strings.Contains(out.Reason, "runtime not confirmed") {
		t.Fatalf("close with a completed turn = %q %q", out.Action, out.Reason)
	}
}

// A created report never dials: with the real opener installed and a socket that would refuse
// thread/read, the created report still succeeds from the native database.
func TestCreatedReportNeverDials(t *testing.T) {
	install(t)
	ws := t.TempDir()
	t.Chdir(ws)
	server := fakehost.Start(t)
	server.Respond(role.DispatchHostThreadRead, fakehost.Reply{Error: &fakehost.RPCError{Code: -32601, Message: "no"}})
	codex := codexHome(t, server.SocketPath)
	seed(t, codex, "child-a", "session-test")
	env := tempEnv(t, codex)
	start, err := command(t, env, map[string]any{"action": "start", "role": "executor"})
	check(t, err)
	claim, err := command(t, env, map[string]any{"action": "claim", "attemptId": start.AttemptID})
	check(t, err)
	issue(t, claim.Marker)
	out, err := command(t, env, map[string]any{"action": "report", "attemptId": start.AttemptID, "outcome": "created", "agentId": "child-a"})
	check(t, err)
	if out.Action != "wait" {
		t.Fatalf("created report = %q", out.Action)
	}
	if n := server.Count(role.DispatchHostThreadRead); n != 0 {
		t.Fatalf("the created report dialled thread/read %d times", n)
	}
}

// The host forwards only the two reads, without turns and with one entry, and refuses everything
// else without sending it.
func TestHostForwardsOnlyTheTwoReads(t *testing.T) {
	server := fake(t, "completed")
	env := tempEnv(t, codexHome(t, server.SocketPath))
	h, closeHost, err := Open(env)
	check(t, err)
	if h == nil {
		t.Fatal("the opener returned no host against a live socket")
	}
	defer closeHost()
	ctx := context.Background()
	if _, err := h.Call(ctx, role.DispatchHostThreadRead, map[string]any{"threadId": "child-a", "includeTurns": false}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Call(ctx, role.DispatchHostTurnsList, map[string]any{"threadId": "child-a", "limit": 1, "itemsView": "summary"}); err != nil {
		t.Fatal(err)
	}
	for _, refused := range []struct {
		method string
		params map[string]any
	}{
		{"turn/start", map[string]any{"threadId": "child-a"}},
		{"thread/resume", map[string]any{"threadId": "child-a"}},
		{"thread/archive", map[string]any{"threadId": "child-a"}},
		{role.DispatchHostThreadRead, map[string]any{"threadId": "child-a", "includeTurns": true}},
		{role.DispatchHostTurnsList, map[string]any{"threadId": "child-a", "limit": 5}},
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
// dial is bounded and the opener returns an error and no host.
func TestDialIsBounded(t *testing.T) {
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
	begin := time.Now()
	h, closeHost, err := Open(tempEnv(t, dir))
	if closeHost != nil {
		defer closeHost()
	}
	if h != nil || err == nil {
		t.Fatalf("a socket that never answers returned host %v err %v", h, err)
	}
	if elapsed := time.Since(begin); elapsed > 10*time.Second {
		t.Fatalf("the dial took %v", elapsed)
	}
}

// A socket nobody listens on is an error, not a host, so the CLI falls back to nil.
func TestMissingSocketReturnsNoHost(t *testing.T) {
	h, closeHost, err := Open(tempEnv(t, t.TempDir()))
	if closeHost != nil {
		defer closeHost()
	}
	if h != nil || err == nil {
		t.Fatalf("a missing socket returned host %v err %v", h, err)
	}
}
