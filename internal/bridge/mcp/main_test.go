package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver/fakehost"
)

// served runs Main over real OS-level pipes, as a host spawning `crw bridge` would, and
// connects a Go MCP client to it. It returns the session, everything Main wrote to stdout (so
// a test can prove it was only protocol frames) and its stderr.
type served struct {
	session *sdk.ClientSession
	stdout  *lockedBuffer
	stderr  *lockedBuffer
	exit    chan int
}

type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// teeCloser copies the server's stdout to a record while the client reads it.
type teeCloser struct {
	io.Writer
	closer io.Closer
}

func (t teeCloser) Close() error { return t.closer.Close() }

func serve(t *testing.T, args []string, env map[string]string) *served {
	t.Helper()
	serverIn, clientOut := io.Pipe()
	clientIn, serverOut := io.Pipe()
	s := &served{stdout: &lockedBuffer{}, stderr: &lockedBuffer{}, exit: make(chan int, 1)}
	go func() {
		s.exit <- Main(context.Background(), args, env, serverIn, teeCloser{io.MultiWriter(serverOut, s.stdout), serverOut}, s.stderr)
		_ = serverOut.Close()
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	session, err := sdk.NewClient(&sdk.Implementation{Name: "mcp-test", Version: "0"}, nil).Connect(ctx, &sdk.IOTransport{Reader: clientIn, Writer: clientOut}, nil)
	if err != nil {
		t.Fatalf("connect: %v (stderr %s)", err, s.stderr.String())
	}
	s.session = session
	t.Cleanup(func() { _ = session.Close() })
	return s
}

// finish closes the client side and waits for Main to return, then proves stdout held only
// JSON-RPC frames.
func (s *served) finish(t *testing.T) {
	t.Helper()
	if err := s.session.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case code := <-s.exit:
		if code != 0 {
			t.Fatalf("Main returned %d (stderr %s)", code, s.stderr.String())
		}
	case <-time.After(20 * time.Second):
		t.Fatal("Main did not return after stdin closed")
	}
	scanner := bufio.NewScanner(strings.NewReader(s.stdout.String()))
	scanner.Buffer(nil, 16<<20)
	for scanner.Scan() {
		var frame map[string]json.RawMessage
		if err := json.Unmarshal(scanner.Bytes(), &frame); err != nil || string(frame["jsonrpc"]) != `"2.0"` {
			t.Fatalf("stdout carried a non-MCP line %q", scanner.Text())
		}
	}
}

func call(t *testing.T, s *served, name string, arguments map[string]any) *sdk.CallToolResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	result, err := s.session.CallTool(ctx, &sdk.CallToolParams{Name: name, Arguments: arguments})
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return result
}

func structured(t *testing.T, result *sdk.CallToolResult) map[string]any {
	t.Helper()
	raw, err := json.Marshal(result.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func isolated(t *testing.T) (string, map[string]string) {
	home := t.TempDir()
	return home, map[string]string{"HOME": home, "CODEX_HOME": filepath.Join(home, "codex"), "XDG_STATE_HOME": filepath.Join(home, "state")}
}

func Test_a_stdio_round_trip_creates_a_thread_on_the_host_and_replays_it(t *testing.T) {
	// Given a fake App Server and a bridge served over stdio
	host := fakehost.Start(t)
	home, env := isolated(t)
	cwd, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	host.Respond("thread/start", fakehost.Reply{Result: map[string]any{"thread": map[string]any{"id": "thread-1", "cwd": cwd}, "cwd": cwd, "runtimeWorkspaceRoots": []any{cwd}, "model": "anthropic/claude-opus-5", "reasoningEffort": "xhigh", "approvalPolicy": "never", "sandbox": map[string]any{"type": "readOnly", "networkAccess": false}}})
	host.Respond("turn/start", fakehost.Reply{Result: map[string]any{"turn": map[string]any{"id": "turn-1"}}})
	host.Handle("thread/read", func(json.RawMessage) fakehost.Reply {
		return fakehost.Reply{Result: map[string]any{"thread": map[string]any{"id": "thread-1", "cwd": cwd, "model": "anthropic/claude-opus-5", "reasoningEffort": "xhigh"}}}
	})
	s := serve(t, []string{"--socket", host.SocketPath, "--state-dir", filepath.Join(home, "ledger")}, env)
	arguments := map[string]any{"request_id": "round-trip", "cwd": cwd, "prompt": "READY", "model": "anthropic/claude-opus-5", "reasoning_effort": "xhigh"}
	// When the same creation is asked twice
	first := call(t, s, "create_thread", arguments)
	second := call(t, s, "create_thread", arguments)
	s.finish(t)
	// Then one thread and one turn were started, and the second answer is the replayed receipt
	one, two := structured(t, first), structured(t, second)
	if first.IsError || one["status"] != "accepted" || one["threadId"] != "thread-1" || one["turnId"] != "turn-1" {
		t.Fatalf("first reply %v", one)
	}
	if second.IsError || two["replayed"] != true || two["threadId"] != "thread-1" {
		t.Fatalf("second reply %v", two)
	}
	if host.Count("thread/start") != 1 || host.Count("turn/start") != 1 {
		t.Fatalf("host saw %v", host.Requests())
	}
	// And the text content is the same receipt the structured content carries
	var text map[string]any
	if err := json.Unmarshal([]byte(first.Content[0].(*sdk.TextContent).Text), &text); err != nil || text["requestId"] != "round-trip" {
		t.Fatalf("text content %v: %v", first.Content, err)
	}
}

func Test_with_no_socket_the_server_still_lists_its_tools_and_explains_the_failure_on_stderr(t *testing.T) {
	// Given a socket path where nothing listens
	home, env := isolated(t)
	s := serve(t, []string{"--socket", filepath.Join(home, "absent.sock"), "--state-dir", filepath.Join(home, "ledger")}, env)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	// When tools are listed and a thread is requested
	listed, err := s.session.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	created := call(t, s, "create_thread", map[string]any{"request_id": "nobody-home", "cwd": home, "model": "anthropic/claude-opus-5", "reasoning_effort": "xhigh"})
	capabilities := call(t, s, "get_capabilities", map[string]any{})
	again, err := s.session.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("the server stopped answering after the failure: %v", err)
	}
	s.finish(t)
	// Then all twelve tools, get_capabilities among them, were listed before and after
	names := []string{}
	for _, tool := range listed.Tools {
		names = append(names, tool.Name)
	}
	if strings.Join(names, ",") != strings.Join(order, ",") || len(again.Tools) != 12 {
		t.Fatalf("listed %v then %d tools", names, len(again.Tools))
	}
	// And create_thread answered, as Python does, with a receipt that says nothing was attempted
	// and why, while a read tool answered with an error result naming the missing socket
	receipt := structured(t, created)
	if created.IsError || receipt["status"] != "not_attempted" || receipt["retrySafe"] != true || receipt["error"] != "FileNotFoundError: [Errno 2] No such file or directory" {
		t.Fatalf("create_thread %v", receipt)
	}
	if !capabilities.IsError || capabilities.Content[0].(*sdk.TextContent).Text != "Error executing tool get_capabilities: [Errno 2] No such file or directory" {
		t.Fatalf("get_capabilities %v", capabilities.Content)
	}
	// And stderr explains both
	log := s.stderr.String()
	if !strings.Contains(log, "tool=create_thread status=not_attempted") || !strings.Contains(log, "tool=get_capabilities") || !strings.Contains(log, "No such file or directory") {
		t.Fatalf("stderr does not explain the failure:\n%s", log)
	}
}

func Test_a_log_line_lands_on_stderr_and_stdout_carries_only_protocol_frames(t *testing.T) {
	// Given a served bridge
	home, env := isolated(t)
	s := serve(t, []string{"--socket", filepath.Join(home, "absent.sock"), "--state-dir", filepath.Join(home, "ledger")}, env)
	// When a call makes the server log (an unknown request id is logged as a failed call)
	result := call(t, s, "get_operation", map[string]any{"request_id": "never-made"})
	s.finish(t) // fails if any stdout line is not a JSON-RPC frame
	// Then the log line is on stderr and nowhere on stdout
	if !result.IsError || !strings.Contains(s.stderr.String(), `msg="tool call failed" tool=get_operation error="Unknown request_id"`) {
		t.Fatalf("result %v stderr %s", result.Content, s.stderr.String())
	}
	if strings.Contains(s.stdout.String(), "tool call failed") {
		t.Fatal("a log line reached stdout")
	}
}

func Test_help_and_version_exit_zero_and_start_no_protocol(t *testing.T) {
	for _, tc := range []struct {
		arg, stdout, stderr string
	}{
		{"--help", "", "usage: crw bridge"},
		{"-h", "", "--state-dir"},
		{"--version", PackageVersion + "\n", ""},
	} {
		t.Run(tc.arg, func(t *testing.T) {
			_, env := isolated(t)
			var stdout, stderr bytes.Buffer
			code := Main(context.Background(), []string{tc.arg}, env, io.NopCloser(strings.NewReader("")), nopWriteCloser{&stdout}, &stderr)
			if code != 0 || stdout.String() != tc.stdout || !strings.Contains(stderr.String(), tc.stderr) {
				t.Fatalf("exit %d stdout %q stderr %q", code, stdout.String(), stderr.String())
			}
		})
	}
}

type nopWriteCloser struct{ io.Writer }

func (nopWriteCloser) Close() error { return nil }

func Test_an_unusable_policy_stops_the_server_before_any_ledger_exists(t *testing.T) {
	home, env := isolated(t)
	policy := filepath.Join(home, "missing-policy.json")
	env["CODEX_THREAD_BRIDGE_EXECUTION_POLICY"] = policy
	var stdout, stderr bytes.Buffer
	state := filepath.Join(home, "ledger")
	code := Main(context.Background(), []string{"--socket", filepath.Join(home, "absent.sock"), "--state-dir", state}, env, io.NopCloser(strings.NewReader("")), nopWriteCloser{&stdout}, &stderr)
	// Byte for byte what `python -m codex_thread_bridge.server` writes for the same file.
	want := "execution_policy_unreadable: cannot read " + policy + ": [Errno 2] No such file or directory: '" + policy + "'\n"
	if code != 1 || stdout.Len() != 0 || stderr.String() != want {
		t.Fatalf("exit %d stdout %q stderr %q", code, stdout.String(), stderr.String())
	}
	if matches, _ := filepath.Glob(filepath.Join(state, "*")); len(matches) != 0 {
		t.Fatalf("ledger created: %v", matches)
	}
}

func Test_the_default_socket_and_ledger_follow_codex_home_and_xdg_state_home(t *testing.T) {
	// Computed only: nothing here is opened or dialled.
	for _, tc := range []struct {
		env           map[string]string
		socket, state string
	}{
		{map[string]string{"HOME": "/h"}, "/h/.codex/app-server-control/app-server-control.sock", "/h/.local/state/codex-thread-bridge"},
		{map[string]string{"HOME": "/h", "CODEX_HOME": "/c", "XDG_STATE_HOME": "/s"}, "/c/app-server-control/app-server-control.sock", "/s/codex-thread-bridge"},
	} {
		socket, state := Defaults(tc.env)
		if socket != tc.socket || state != tc.state {
			t.Fatalf("%v: %s %s", tc.env, socket, state)
		}
	}
}
