package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge"
	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver"
	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver/fakehost"
	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/execution"
	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/ledger"
)

// CRW-1486/S3 (B2): a create_thread reservation the ledger recorded before a prompt was required, with
// no prompt in its arguments and nothing of it reaching the host (not_attempted), is not resumed on its
// request_id: resumed without a prompt it would create a thread with no first turn, and a call that
// brings one is other arguments for that id. Both calls on that id are refused naming the way on, the
// reservation is left as it is, and the creation goes under a new request_id with a prompt.
func TestCreateThreadOnAPreUpgradeReservationGoesUnderANewRequestID(t *testing.T) {
	home, env := isolated(t)
	cwd, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(home, "ledger")
	socket := fakehost.SocketPath(t, "absent.sock")
	// Before the upgrade: the bridge took create_thread without a prompt, and the host could not be
	// reached, so the reservation was left not_attempted.
	policy, err := execution.FromEnvironment(env)
	if err != nil {
		t.Fatal(err)
	}
	canonical, store, err := ledger.Endpoint(socket, state)
	if err != nil {
		t.Fatal(err)
	}
	client := appserver.New(canonical, appserver.DefaultBounds)
	reserved, err := bridge.New(client, store, policy).CreateThread(context.Background(), bridge.CreateThread{RequestID: "pre-upgrade", CWD: cwd, Sandbox: "read-only", Model: "anthropic/claude-opus-5", Effort: "xhigh"})
	client.Close()
	if closeErr := store.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
	if err != nil || reserved["status"] != "not_attempted" {
		t.Fatalf("the pre-upgrade reservation: %v %v", reserved, err)
	}

	s := serve(t, []string{"--socket", socket, "--state-dir", state}, env)
	operation := func() string {
		t.Helper()
		result := call(t, s, "get_operation", map[string]any{"request_id": "pre-upgrade"})
		return result.Content[0].(*sdk.TextContent).Text
	}
	before := operation()
	arguments := map[string]any{"request_id": "pre-upgrade", "cwd": cwd, "sandbox": "read-only", "model": "anthropic/claude-opus-5", "reasoning_effort": "xhigh"}
	for _, prompt := range []any{nil, "READY"} {
		if prompt != nil {
			arguments["prompt"] = prompt
		}
		result := call(t, s, "create_thread", arguments)
		text := result.Content[0].(*sdk.TextContent).Text
		if !result.IsError || !strings.Contains(text, "not_attempted") || !strings.Contains(text, "new request_id") {
			t.Fatalf("create_thread on the pre-upgrade reservation with prompt %v answered %q", prompt, text)
		}
		if after := operation(); after != before {
			t.Fatalf("the reservation changed:\n%s\n%s", before, after)
		}
	}
	// The way on: a new request_id with a prompt passes the guard and goes to the host (absent here, so
	// it is recorded not_attempted under its own id).
	arguments["request_id"] = "post-upgrade"
	result := call(t, s, "create_thread", arguments)
	if result.IsError {
		t.Fatalf("a new request_id with a prompt was refused: %s", result.Content[0].(*sdk.TextContent).Text)
	}
	if got := structured(t, result); got["requestId"] != "post-upgrade" {
		t.Fatalf("the new creation answered %v", got)
	}
	s.finish(t)
}

// CRW-1486/S3 (A2): a ledger that cannot be read is not a ledger with no receipt. A prompt-less
// create_thread whose request_id cannot be looked up is answered with the read failure and held, not
// refused as missing its prompt (which would tell a retry of an accepted creation that nothing exists).
func TestCreateThreadWithoutAPromptReturnsALedgerReadFailure(t *testing.T) {
	home, env := isolated(t)
	policy, err := execution.FromEnvironment(env)
	if err != nil {
		t.Fatal(err)
	}
	canonical, store, err := ledger.Endpoint(fakehost.SocketPath(t, "absent.sock"), filepath.Join(home, "ledger"))
	if err != nil {
		t.Fatal(err)
	}
	client := appserver.New(canonical, appserver.DefaultBounds)
	defer client.Close()
	h := handlers{b: bridge.New(client, store, policy), log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	_, err = h.createThread(context.Background(), createThreadIn{RequestID: "unreadable", CWD: home, Sandbox: "read-only", Model: "anthropic/claude-opus-5", ReasoningEffort: "xhigh"})
	if err == nil || errors.Is(err, errMissingPrompt) || strings.Contains(err.Error(), "prompt is required") {
		t.Fatalf("a ledger read failure was answered as %v", err)
	}
}

// A reservation made with a prompt (after the upgrade) that never reached the host is resumed by a call
// with its own arguments, prompt included, as any not_attempted reservation is.
func TestCreateThreadResumesANotAttemptedReservationWithItsOwnPrompt(t *testing.T) {
	home, env := isolated(t)
	cwd, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	socket := fakehost.SocketPath(t, "absent.sock")
	s := serve(t, []string{"--socket", socket, "--state-dir", filepath.Join(home, "ledger")}, env)
	arguments := map[string]any{"request_id": "transient", "cwd": cwd, "sandbox": "read-only", "model": "anthropic/claude-opus-5", "reasoning_effort": "xhigh", "prompt": "READY"}
	for attempt := 1; attempt <= 2; attempt++ {
		result := call(t, s, "create_thread", arguments)
		if result.IsError {
			t.Fatalf("attempt %d with the reservation's own prompt was refused: %s", attempt, result.Content[0].(*sdk.TextContent).Text)
		}
		if got := structured(t, result); got["status"] != "not_attempted" || got["attempt"] != float64(attempt) && attempt > 1 {
			t.Fatalf("attempt %d answered %v", attempt, got)
		}
	}
	s.finish(t)
}

// reserveWithoutHost records a create_thread reservation that never reached the host (not_attempted) in the ledger the
// bridge served on socket and state keeps: the call goes through a client dialling an absent socket, so nothing of it
// reaches the host on socket.
func reserveWithoutHost(t *testing.T, env map[string]string, socket, state string, in bridge.CreateThread) {
	t.Helper()
	policy, err := execution.FromEnvironment(env)
	if err != nil {
		t.Fatal(err)
	}
	_, store, err := ledger.Endpoint(socket, state)
	if err != nil {
		t.Fatal(err)
	}
	client := appserver.New(fakehost.SocketPath(t, "absent.sock"), appserver.DefaultBounds)
	reserved, err := bridge.New(client, store, policy).CreateThread(context.Background(), in)
	client.Close()
	if closeErr := store.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
	if err != nil || reserved["status"] != "not_attempted" {
		t.Fatalf("the reservation: %v %v", reserved, err)
	}
}

// CRW-1506: a reservation made with a prompt is not one made before prompts were required. A prompt-less retry on its
// request_id is refused without the advice to take a new request_id (following it would leave this reservation behind
// while the same creation goes on elsewhere), and the call that repeats the reservation's own arguments, prompt
// included, resumes it under the same id: the host sees one thread/start.
func TestCreateThreadPromptlessRetryOnAPromptedReservationThenSameIDMakesOneThread(t *testing.T) {
	host := fakehost.Start(t)
	home, env := isolated(t)
	cwd, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(home, "ledger")
	reserveWithoutHost(t, env, host.SocketPath, state, bridge.CreateThread{RequestID: "prompted", CWD: cwd, Prompt: "READY", Sandbox: "read-only", Model: "anthropic/claude-opus-5", Effort: "xhigh"})
	host.Respond("thread/start", fakehost.Reply{Result: map[string]any{"thread": map[string]any{"id": "thread-1", "cwd": cwd}, "cwd": cwd, "runtimeWorkspaceRoots": []any{cwd}, "model": "anthropic/claude-opus-5", "reasoningEffort": "xhigh", "approvalPolicy": "never", "sandbox": map[string]any{"type": "readOnly", "networkAccess": false}}})
	host.Respond("turn/start", fakehost.Reply{Result: map[string]any{"turn": map[string]any{"id": "turn-1"}}})
	host.Handle("thread/read", func(json.RawMessage) fakehost.Reply {
		return fakehost.Reply{Result: map[string]any{"thread": map[string]any{"id": "thread-1", "cwd": cwd, "model": "anthropic/claude-opus-5", "reasoningEffort": "xhigh"}}}
	})

	s := serve(t, []string{"--socket", host.SocketPath, "--state-dir", state}, env)
	arguments := map[string]any{"request_id": "prompted", "cwd": cwd, "sandbox": "read-only", "model": "anthropic/claude-opus-5", "reasoning_effort": "xhigh"}
	promptless := call(t, s, "create_thread", arguments)
	text := promptless.Content[0].(*sdk.TextContent).Text
	if !promptless.IsError || strings.Contains(text, "new request_id") || !strings.Contains(text, "not_attempted") {
		t.Fatalf("a prompt-less retry on a prompted reservation answered %q", text)
	}
	if host.Count("thread/start") != 0 {
		t.Fatalf("the prompt-less retry reached the host: %v", host.Requests())
	}
	arguments["prompt"] = "READY"
	resumed := call(t, s, "create_thread", arguments)
	if resumed.IsError {
		t.Fatalf("the reservation's own arguments were refused: %s", resumed.Content[0].(*sdk.TextContent).Text)
	}
	if got := structured(t, resumed); got["status"] != "accepted" || got["threadId"] != "thread-1" || got["requestId"] != "prompted" {
		t.Fatalf("the resumed creation answered %v", got)
	}
	s.finish(t)
	if host.Count("thread/start") != 1 {
		t.Fatalf("the host saw %d thread/start: %v", host.Count("thread/start"), host.Requests())
	}
}

// CRW-1506: a prompted call on a prompt-null reservation made with other arguments (another title) is not the
// reservation's own call without its prompt, so it gets the plain conflict, not the way to a new request_id.
func TestCreateThreadPromptedCallOnAPromptlessReservationWithOtherArgumentsIsAPlainConflict(t *testing.T) {
	home, env := isolated(t)
	cwd, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(home, "ledger")
	socket := fakehost.SocketPath(t, "absent.sock")
	reserveWithoutHost(t, env, socket, state, bridge.CreateThread{RequestID: "titled", CWD: cwd, Title: "T1", Sandbox: "read-only", Model: "anthropic/claude-opus-5", Effort: "xhigh"})

	s := serve(t, []string{"--socket", socket, "--state-dir", state}, env)
	result := call(t, s, "create_thread", map[string]any{"request_id": "titled", "cwd": cwd, "title": "T2", "prompt": "READY", "sandbox": "read-only", "model": "anthropic/claude-opus-5", "reasoning_effort": "xhigh"})
	text := result.Content[0].(*sdk.TextContent).Text
	if !result.IsError || strings.Contains(text, "new request_id") || !strings.Contains(text, ledger.ErrConflict.Error()) {
		t.Fatalf("a prompted call with another title on a prompt-null reservation answered %q", text)
	}
	s.finish(t)
}
