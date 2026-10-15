package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge"
	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver"
	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver/fakehost"
	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/execution"
	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/ledger"
)

// CRW-1486/S3 (B2), CRW-1506: a create_thread reservation the ledger recorded before a prompt was required, with no
// prompt in its arguments and nothing of it reaching the host (not_attempted), is not resumed on its request_id: resumed
// without a prompt it would create a thread with no first turn, and a call that brings one is other arguments for that
// id. The first call on it, with or without a prompt, retires it (failed, its error naming this refusal, every other
// member kept) before answering with the way to a new request_id; the next call gets the same way and changes nothing,
// and the creation goes under a new request_id with a prompt.
func TestCreateThreadOnAPreUpgradeReservationGoesUnderANewRequestID(t *testing.T) {
	for _, order := range [][]any{{nil, "READY"}, {"READY", nil}} {
		t.Run(fmt.Sprintf("first prompt %v", order[0]), func(t *testing.T) {
			home, env := isolated(t)
			cwd, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			state := filepath.Join(home, "ledger")
			socket := fakehost.SocketPath(t, "absent.sock")
			// Before the upgrade: the bridge took create_thread without a prompt, and the host could not be
			// reached, so the reservation was left not_attempted.
			reserveWithoutHost(t, env, socket, state, bridge.CreateThread{RequestID: "pre-upgrade", CWD: cwd, Sandbox: "read-only", Model: "anthropic/claude-opus-5", Effort: "xhigh"})

			s := serve(t, []string{"--socket", socket, "--state-dir", state}, env)
			operation := func() map[string]any {
				t.Helper()
				return structured(t, call(t, s, "get_operation", map[string]any{"request_id": "pre-upgrade"}))
			}
			before := operation()
			var retired map[string]any
			for _, prompt := range order {
				arguments := map[string]any{"request_id": "pre-upgrade", "cwd": cwd, "sandbox": "read-only", "model": "anthropic/claude-opus-5", "reasoning_effort": "xhigh"}
				if prompt != nil {
					arguments["prompt"] = prompt
				}
				result := call(t, s, "create_thread", arguments)
				text := result.Content[0].(*sdk.TextContent).Text
				if !result.IsError || !strings.Contains(text, "not_attempted") || !strings.Contains(text, "retired") || !strings.Contains(text, "new request_id") {
					t.Fatalf("create_thread on the pre-upgrade reservation with prompt %v answered %q", prompt, text)
				}
				after := operation()
				if retired != nil {
					if !reflect.DeepEqual(after, retired) {
						t.Fatalf("a call on the retired reservation changed it:\n%v\n%v", retired, after)
					}
					continue
				}
				retired = after
				if after["status"] != "failed" || after["retrySafe"] != false || !strings.HasPrefix(fmt.Sprint(after["error"]), bridge.RetiredBeforePrompt) || !strings.Contains(fmt.Sprint(after["error"]), fmt.Sprint(before["error"])) {
					t.Fatalf("the reservation was not retired: %v", after)
				}
				for key, value := range before {
					if key != "status" && key != "retrySafe" && key != "error" && key != "updatedAt" && !reflect.DeepEqual(after[key], value) {
						t.Fatalf("the retirement changed %s: %v -> %v", key, value, after[key])
					}
				}
				if len(after) != len(before) {
					t.Fatalf("the retirement added members:\n%v\n%v", before, after)
				}
			}
			// The way on: a new request_id with a prompt passes the guard and goes to the host (absent here, so
			// it is recorded not_attempted under its own id).
			result := call(t, s, "create_thread", map[string]any{"request_id": "post-upgrade", "cwd": cwd, "sandbox": "read-only", "model": "anthropic/claude-opus-5", "reasoning_effort": "xhigh", "prompt": "READY"})
			if result.IsError {
				t.Fatalf("a new request_id with a prompt was refused: %s", result.Content[0].(*sdk.TextContent).Text)
			}
			if got := structured(t, result); got["requestId"] != "post-upgrade" {
				t.Fatalf("the new creation answered %v", got)
			}
			s.finish(t)
		})
	}
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

// answerCreations makes host answer a creation in cwd as the read-only defaults the tests in this file ask for, so a
// thread/start that goes out succeeds and is counted.
func answerCreations(host *fakehost.Server, cwd string) {
	host.Respond("thread/start", fakehost.Reply{Result: map[string]any{"thread": map[string]any{"id": "thread-1", "cwd": cwd}, "cwd": cwd, "runtimeWorkspaceRoots": []any{cwd}, "model": "anthropic/claude-opus-5", "reasoningEffort": "xhigh", "approvalPolicy": "never", "sandbox": map[string]any{"type": "readOnly", "networkAccess": false}}})
	host.Respond("turn/start", fakehost.Reply{Result: map[string]any{"turn": map[string]any{"id": "turn-1"}}})
	host.Handle("thread/read", func(json.RawMessage) fakehost.Reply {
		return fakehost.Reply{Result: map[string]any{"thread": map[string]any{"id": "thread-1", "cwd": cwd, "model": "anthropic/claude-opus-5", "reasoningEffort": "xhigh"}}}
	})
}

// CRW-1506 (x1506 #1): the MCP layer's own reservation lookup is not the guard. A prompt-less create_thread that finds an
// attempt on its request_id in flight (in_progress_or_unknown) goes on to the bridge; when that attempt ends with nothing
// reaching the host (not_attempted) between the MCP lookup and the bridge's own lookup, the bridge still refuses the call
// and never re-runs the reservation without a prompt: no thread/start goes out.
func TestCreateThreadPromptlessCallStaysRefusedWhenTheReservationTurnsNotAttemptedAfterTheMCPLookup(t *testing.T) {
	host := fakehost.Start(t)
	home, env := isolated(t)
	cwd, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(home, "ledger")
	reserveWithoutHost(t, env, host.SocketPath, state, bridge.CreateThread{RequestID: "in-flight", CWD: cwd, Sandbox: "read-only", Model: "anthropic/claude-opus-5", Effort: "xhigh"})
	answerCreations(host, cwd)
	policy, err := execution.FromEnvironment(env)
	if err != nil {
		t.Fatal(err)
	}
	canonical, store, err := ledger.Endpoint(host.SocketPath, state)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	client := appserver.New(canonical, appserver.DefaultBounds)
	defer client.Close()
	ctx := context.Background()
	status := func(to string) {
		t.Helper()
		stored, err := store.Get(ctx, "in-flight")
		if err != nil {
			t.Fatal(err)
		}
		stored["status"] = to
		if _, err = store.Save(ctx, stored); err != nil {
			t.Fatal(err)
		}
	}
	// Another attempt on the reservation is in flight (another process, or a bridge that still takes a prompt-less create).
	status("in_progress_or_unknown")
	h := handlers{b: bridge.New(client, store, policy), log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	looked := 0
	h.lookedUp = func() {
		looked++
		// That attempt ends with nothing reaching the host after the MCP lookup saw it in flight.
		status("not_attempted")
	}
	_, err = h.createThread(ctx, createThreadIn{RequestID: "in-flight", CWD: cwd, Sandbox: "read-only", Model: "anthropic/claude-opus-5", ReasoningEffort: "xhigh"})
	if looked != 1 {
		t.Fatalf("the MCP lookup ran %d times", looked)
	}
	if host.Count("thread/start") != 0 || host.Count("turn/start") != 0 {
		t.Fatalf("a prompt-less create_thread reached the host (error %v): %v", err, host.Requests())
	}
	if err == nil {
		t.Fatal("a prompt-less create_thread on a not_attempted reservation was not refused")
	}
}

// CRW-1506 (x1506 #2): the way to a new request_id leaves nothing behind that can still create a thread. A reservation
// made without a prompt, before prompts were required, is retired when that way is given, so a bridge that still takes a
// prompt-less create (a build from before the refusal, serving another session on the same ledger) retrying it replays
// the retired receipt instead of creating a thread with no first turn; every later call on its request_id, with or without
// a prompt, gets the same way, and only the creation under the new request_id reaches the host.
func TestCreateThreadWayToANewRequestIDRetiresThePromptlessReservation(t *testing.T) {
	host := fakehost.Start(t)
	home, env := isolated(t)
	cwd, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(home, "ledger")
	reservation := bridge.CreateThread{RequestID: "pre-upgrade", CWD: cwd, Sandbox: "read-only", Model: "anthropic/claude-opus-5", Effort: "xhigh"}
	reserveWithoutHost(t, env, host.SocketPath, state, reservation)
	answerCreations(host, cwd)

	s := serve(t, []string{"--socket", host.SocketPath, "--state-dir", state}, env)
	arguments := map[string]any{"request_id": "pre-upgrade", "cwd": cwd, "sandbox": "read-only", "model": "anthropic/claude-opus-5", "reasoning_effort": "xhigh"}
	guided := call(t, s, "create_thread", arguments)
	if text := guided.Content[0].(*sdk.TextContent).Text; !guided.IsError || !strings.Contains(text, "new request_id") {
		t.Fatalf("a prompt-less call on the pre-upgrade reservation answered %q", text)
	}

	// A bridge that still takes a prompt-less create retries the reservation on the same ledger.
	policy, err := execution.FromEnvironment(env)
	if err != nil {
		t.Fatal(err)
	}
	canonical, store, err := ledger.Endpoint(host.SocketPath, state)
	if err != nil {
		t.Fatal(err)
	}
	client := appserver.New(canonical, appserver.DefaultBounds)
	retried, err := bridge.New(client, store, policy).CreateThread(context.Background(), reservation)
	client.Close()
	if closeErr := store.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
	if host.Count("thread/start") != 0 {
		t.Fatalf("the retry created a thread from the reservation (%v %v): %v", retried, err, host.Requests())
	}
	if err != nil || retried["status"] != "failed" || retried["replayed"] != true {
		t.Fatalf("the retry is not a replay of the retired receipt: %v %v", retried, err)
	}

	for _, prompt := range []any{nil, "READY"} {
		if prompt != nil {
			arguments["prompt"] = prompt
		}
		result := call(t, s, "create_thread", arguments)
		if text := result.Content[0].(*sdk.TextContent).Text; !result.IsError || !strings.Contains(text, "new request_id") {
			t.Fatalf("a later call with prompt %v on the retired reservation answered %q", prompt, text)
		}
	}
	arguments["request_id"] = "post-upgrade"
	created := call(t, s, "create_thread", arguments)
	if created.IsError {
		t.Fatalf("the creation under a new request_id was refused: %s", created.Content[0].(*sdk.TextContent).Text)
	}
	if got := structured(t, created); got["status"] != "accepted" || got["requestId"] != "post-upgrade" {
		t.Fatalf("the creation under a new request_id answered %v", got)
	}
	s.finish(t)
	if host.Count("thread/start") != 1 || host.Count("turn/start") != 1 {
		t.Fatalf("the host saw %d thread/start and %d turn/start, want one creation with its first turn: %v", host.Count("thread/start"), host.Count("turn/start"), host.Requests())
	}
}
