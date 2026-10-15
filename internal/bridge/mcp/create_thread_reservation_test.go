package mcp

import (
	"context"
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
