package role

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver/fakehost"
)

type createdCheckFake struct {
	reply json.RawMessage
	err   error
	calls int
}

func (f *createdCheckFake) Call(_ context.Context, method string, args map[string]any) (json.RawMessage, error) {
	f.calls++
	if method != "thread/read" || args["threadId"] != "child-a" || args["includeTurns"] != false {
		return nil, errors.New("unexpected host call")
	}
	return f.reply, f.err
}
func createdCheckReply(parent, source, status string) json.RawMessage {
	return must(json.Marshal(map[string]any{"thread": map[string]any{"id": "child-a", "parentThreadId": parent, "threadSource": source, "status": map[string]any{"type": status}}}))
}
func createdCheckInput(attempt, outcome string) map[string]any {
	return map[string]any{"action": "report", "sessionId": "session-test", "dispatchId": "task-test", "attemptId": attempt, "outcome": outcome, "agentId": "child-a"}
}

func TestCreatedCheckHostWitnessAndRollback(t *testing.T) {
	for _, tc := range []struct {
		name   string
		reply  json.RawMessage
		err    error
		accept bool
	}{
		{"child", createdCheckReply("session-test", "subagent", "idle"), nil, true},
		{"active-child", createdCheckReply("session-test", "subagent", "active"), nil, true},
		{"source-witness", json.RawMessage(`{"thread":{"id":"child-a","parentThreadId":"session-test","source":{"subAgent":{"thread_spawn":{"parent_thread_id":"session-test"}}},"status":{"type":"notLoaded"}}}`), nil, true},
		{"foreign-parent", createdCheckReply("other", "subagent", "idle"), nil, false},
		{"root", createdCheckReply("session-test", "cli", "idle"), nil, false},
		{"missing", json.RawMessage(`{}`), nil, false},
		{"wrong-id", json.RawMessage(`{"thread":{"id":"other","parentThreadId":"session-test","threadSource":"subagent"}}`), nil, false},
		{"corrupt", json.RawMessage(`{`), nil, false},
		{"unavailable", nil, errors.New("host unavailable"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ws, env, start, file := dispatchTestFixture(t)
			dispatchTestCall(t, ws, env, map[string]any{"action": "claim", "attemptId": start.AttemptID})
			before := must(os.ReadFile(file))
			h := &createdCheckFake{reply: tc.reply, err: tc.err}
			out, err := CheckedDispatch(context.Background(), ws, createdCheckInput(start.AttemptID, "created"), env, h)
			if tc.accept {
				check(t, err)
				if out.Action != "wait" || out.Attempts[0].AgentID == nil || *out.Attempts[0].AgentID != "child-a" {
					t.Fatalf("created = %+v", out)
				}
				// Complete remains independent of host availability after created.
				h.err = errors.New("disconnected")
				out, err = CheckedDispatch(context.Background(), ws, createdCheckInput(start.AttemptID, "complete"), env, h)
				check(t, err)
				if out.Action != "complete" || h.calls != 1 {
					t.Fatal("complete rechecked host")
				}
			} else {
				if err == nil || !strings.Contains(err.Error(), "copy agentId") {
					t.Fatalf("refusal lacks correction: %v", err)
				}
				if string(before) != string(must(os.ReadFile(file))) {
					t.Fatal("refused report changed ledger")
				}
			}
			if temps := must(filepath.Glob(file + ".*.tmp")); len(temps) != 0 {
				t.Fatal("owned temporary remains")
			}
			if _, err := os.Lstat(file + ".lock"); !os.IsNotExist(err) {
				t.Fatal("owned lock remains")
			}
		})
	}
}

func TestCreatedCheckValidationPrecedesHost(t *testing.T) {
	ws, env, start, _ := dispatchTestFixture(t)
	h := &createdCheckFake{err: errors.New("unavailable")}
	input := createdCheckInput(start.AttemptID, "created")
	_, err := CheckedDispatch(context.Background(), ws, input, env, h)
	if err == nil || !strings.Contains(err.Error(), "claim") || h.calls != 0 {
		t.Fatalf("precedence: %v %d", err, h.calls)
	}
	dispatchTestCall(t, ws, env, map[string]any{"action": "claim", "attemptId": start.AttemptID})
	input["attemptId"] = "stale"
	_, err = CheckedDispatch(context.Background(), ws, input, env, h)
	if err == nil || !strings.Contains(err.Error(), "stale") || h.calls != 0 {
		t.Fatal("stale reached host")
	}
	input["attemptId"] = start.AttemptID
	input["agentId"] = "../bad"
	_, err = CheckedDispatch(context.Background(), ws, input, env, h)
	if err == nil || !strings.Contains(err.Error(), "invalid agentId") || h.calls != 0 {
		t.Fatal("invalid identity reached host")
	}
}

// Independently recorded oracle answers: invented created -> wait; correcting
// that ID -> agentId changed; stopped report -> invalid report outcome.
func TestCreatedCheckChangedOracleAnswersAndHonestClose(t *testing.T) {
	ws, env, start, file := dispatchTestFixture(t)
	dispatchTestCall(t, ws, env, map[string]any{"action": "claim", "attemptId": start.AttemptID})
	oracle := dispatchTestCall(t, ws, env, map[string]any{"action": "report", "attemptId": start.AttemptID, "outcome": "created", "agentId": "PLACEHOLDER"})
	if oracle.Action != "wait" {
		t.Fatal("parity oracle fixture")
	}
	correction := createdCheckInput(start.AttemptID, "created")
	if _, err := RunDispatch(ws, correction, env); err == nil || err.Error() != "agentId changed" {
		t.Fatal("oracle correction fixture")
	}
	stop := createdCheckInput(start.AttemptID, "stopped")
	stop["agentId"] = "PLACEHOLDER"
	stop["executionState"] = "stopped"
	stop["reconciliation"] = "actual child stopped; partial work inspected"
	if _, err := RunDispatch(ws, stop, env); err == nil || err.Error() != "invalid report outcome" {
		t.Fatal("oracle close fixture")
	}
	for _, field := range []string{"reconciliation", "executionState", "agentId"} {
		bad := make(map[string]any)
		for k, v := range stop {
			bad[k] = v
		}
		delete(bad, field)
		if _, err := CheckedDispatch(context.Background(), ws, bad, env, &createdCheckFake{err: errors.New("missing")}); err == nil {
			t.Fatalf("accepted missing %s", field)
		}
	}
	out, err := CheckedDispatch(context.Background(), ws, stop, env, &createdCheckFake{err: errors.New("unknown invented thread")})
	check(t, err)
	if out.Action != "stop" || len(out.Attempts) != 1 || *out.Attempts[0].AgentID != "PLACEHOLDER" || out.Attempts[0].Reconciliation == nil || out.Attempts[0].Code != nil {
		t.Fatalf("closure = %+v", out)
	}
	stored := must(dispatchRead(file, "session-test", "task-test"))
	if stored.Status != "stopped" || stored.Attempts[0].Status != "failed" {
		t.Fatal("closure not durable")
	}
	for _, action := range []string{"status", "claim"} {
		out := dispatchTestCall(t, ws, env, map[string]any{"action": action, "attemptId": start.AttemptID})
		if out.Action != "stop" {
			t.Fatalf("terminal %s = %+v", action, out)
		}
	}
}

func TestCreatedCheckCloseRefusesActiveAndKeepsLock(t *testing.T) {
	ws, env, start, file := dispatchTestFixture(t)
	dispatchTestCall(t, ws, env, map[string]any{"action": "claim", "attemptId": start.AttemptID})
	dispatchTestCall(t, ws, env, map[string]any{"action": "report", "attemptId": start.AttemptID, "outcome": "created", "agentId": "child-a"})
	input := createdCheckInput(start.AttemptID, "stopped")
	input["executionState"] = "stopped"
	input["reconciliation"] = "inspected child"
	before := must(os.ReadFile(file))
	h := &createdCheckFake{reply: createdCheckReply("session-test", "subagent", "active")}
	if _, err := CheckedDispatch(context.Background(), ws, input, env, h); err == nil || !strings.Contains(err.Error(), "active") {
		t.Fatal("active child closure accepted")
	}
	if string(before) != string(must(os.ReadFile(file))) {
		t.Fatal("active refusal wrote state")
	}
	check(t, os.Mkdir(file+".lock", 0700))
	if _, err := CheckedDispatch(context.Background(), ws, input, env, h); err == nil {
		t.Fatal("held lock closure accepted")
	}
}

func TestCreatedCheckDefaultHostRealTransport(t *testing.T) {
	server := fakehost.Start(t)
	server.Respond("thread/read", fakehost.Reply{Result: map[string]any{"thread": map[string]any{"id": "child-a", "parentThreadId": "session-test", "threadSource": "subagent", "status": map[string]any{"type": "idle"}}}})
	// Keep the unix pathname short; fakehost and this root own their cleanups.
	root := must(os.MkdirTemp("/tmp", "crw530-host-"))
	t.Cleanup(func() { check(t, os.RemoveAll(root)) })
	native := filepath.Join(root, "codex")
	check(t, os.MkdirAll(filepath.Join(native, "app-server-control"), 0700))
	check(t, os.Symlink(server.SocketPath, filepath.Join(native, "app-server-control", "app-server-control.sock")))
	ws, env, start, _ := dispatchTestFixture(t)
	old := env
	env = func(k string) (string, bool) {
		if k == "CODEX_HOME" {
			return native, true
		}
		return old(k)
	}
	dispatchTestCall(t, ws, env, map[string]any{"action": "claim", "attemptId": start.AttemptID})
	out, err := CheckedDispatch(context.Background(), ws, createdCheckInput(start.AttemptID, "created"), env, nil)
	check(t, err)
	if out.Action != "wait" || server.Count("thread/read") != 1 || server.Count("thread/resume") != 0 {
		t.Fatalf("real transport = %+v", out)
	}
}
