package role

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver/fakehost"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/host"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
)

type dispatchCmdReadError struct{}

func (dispatchCmdReadError) Read([]byte) (int, error) { return 0, errors.New("injected read failure") }

func TestDispatchCommandBoundedJSONErrors(t *testing.T) {
	env, _ := home(t)
	t.Chdir(t.TempDir())
	for _, tc := range []struct {
		name string
		in   io.Reader
		want string
	}{
		{"empty", strings.NewReader(""), "unexpected end of JSON input"},
		{"malformed", strings.NewReader("unread input"), "invalid character"},
		{"trailing", strings.NewReader(`{} {}`), "invalid character"},
		{"overflow", strings.NewReader(strings.Repeat(" ", 64*1024+1)), "dispatch input exceeds 64 KiB"},
		{"exact-bound", strings.NewReader(strings.Repeat(" ", 64*1024)), "unexpected end of JSON input"},
		{"read-error", dispatchCmdReadError{}, "injected read failure"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			code := DispatchCommand([]string{"--help"}, tc.in, &out, env)
			var answer map[string]string
			if code != 1 || json.Unmarshal(out.Bytes(), &answer) != nil || !strings.Contains(answer["error"], tc.want) || !strings.HasSuffix(out.String(), "\n") {
				t.Fatalf("error answer = %d %q", code, out.String())
			}
		})
	}
	var out, errOut bytes.Buffer
	if code := CLI([]string{"helper", "dispatch"}, strings.NewReader("unread input"), &out, &errOut, env); code != 1 || errOut.Len() != 0 || !strings.HasPrefix(out.String(), `{"error":`) {
		t.Fatalf("table adapter = %d %q", code, out.String())
	}
}

func TestDispatchCommandHostReportsAcrossProcesses(t *testing.T) {
	bin := testsupport.CRW(t)
	server := fakehost.Start(t)
	server.Handle("thread/read", func(raw json.RawMessage) fakehost.Reply {
		var request struct {
			ID string `json:"threadId"`
		}
		check(t, json.Unmarshal(raw, &request))
		if request.ID == "invented" {
			return fakehost.Reply{Error: &fakehost.RPCError{Code: -32000, Message: "no such thread"}}
		}
		parent := "session-test"
		if request.ID == "foreign" {
			parent = "other-session"
		}
		return fakehost.Reply{Result: map[string]any{"thread": map[string]any{"id": request.ID, "parentThreadId": parent, "threadSource": "subagent", "status": map[string]any{"type": "idle"}}}}
	})
	root := must(os.MkdirTemp("/tmp", "crw530-cli-"))
	t.Cleanup(func() { check(t, os.RemoveAll(root)) })
	ws := t.TempDir()
	native := filepath.Join(root, "codex")
	global := t.TempDir()
	userHome := t.TempDir()
	check(t, os.MkdirAll(filepath.Join(native, "app-server-control"), 0700))
	check(t, os.Symlink(server.SocketPath, filepath.Join(native, "app-server-control", "app-server-control.sock")))
	vars := map[string]string{"HOME": userHome, "CODEX_HOME": native, "CRW_HOME": global}
	var env host.LookupEnv = func(k string) (string, bool) { v, ok := vars[k]; return v, ok }
	_, err := SetRole(env, Executor, RolePatch{Mode: Some(ModeModel), Model: Some("primary/model"), Fallback: Some(FallbackPatch{Model: Some("fallback/model"), Effort: Some(EffortLow)})})
	check(t, err)
	run := func(id string, fields map[string]any, wantCode int) DispatchResult {
		t.Helper()
		b := map[string]any{"sessionId": "session-test", "dispatchId": id}
		for k, v := range fields {
			b[k] = v
		}
		cmd := exec.Command(bin, "role", "helper", "dispatch")
		cmd.Dir = ws
		cmd.Env = append(os.Environ(), "HOME="+userHome, "CODEX_HOME="+native, "CRW_HOME="+global, "CODEX_THREAD_ID=")
		cmd.Stdin = strings.NewReader(string(must(json.Marshal(b))))
		out, err := cmd.CombinedOutput()
		code := 0
		if err != nil {
			var exit *exec.ExitError
			if !errors.As(err, &exit) {
				t.Fatal(err)
			}
			code = exit.ExitCode()
		}
		if code != wantCode {
			t.Fatalf("process outcome = %d, want %d: %s", code, wantCode, out)
		}
		if code != 0 {
			var e map[string]string
			check(t, json.Unmarshal(out, &e))
			if !strings.Contains(e["error"], "copy agentId") {
				t.Fatalf("missing correction: %s", out)
			}
			return DispatchResult{}
		}
		var result DispatchResult
		check(t, json.Unmarshal(out, &result))
		return result
	}
	start := run("reports", map[string]any{"action": "start", "role": "executor"}, 0)
	claim := run("reports", map[string]any{"action": "claim", "attemptId": start.AttemptID}, 0)
	if claim.Candidate == nil || *claim.Candidate.Model != "primary/model" {
		t.Fatal("primary snapshot")
	}
	file := filepath.Join(ws, ".crw", "dispatches", "session-test", "reports.json")
	before := must(os.ReadFile(file))
	for _, id := range []string{"invented", "foreign"} {
		run("reports", map[string]any{"action": "report", "attemptId": start.AttemptID, "outcome": "created", "agentId": id}, 1)
		if string(before) != string(must(os.ReadFile(file))) {
			t.Fatal("CLI host refusal wrote state")
		}
	}
	if got := run("reports", map[string]any{"action": "report", "attemptId": start.AttemptID, "outcome": "created", "agentId": "child-a"}, 0); got.Action != "wait" {
		t.Fatal("CLI child not accepted")
	}
	next := run("reports", map[string]any{"action": "report", "attemptId": start.AttemptID, "outcome": "task_failed", "agentId": "child-a", "executionState": "stopped", "reconciliation": "child stopped; diff inspected", "taskFailure": map[string]any{"kind": "unusable_output", "evidence": "final output unrelated to packet"}}, 0)
	if next.Action != "ready" {
		t.Fatal("CLI task failure did not offer fallback")
	}
	claim = run("reports", map[string]any{"action": "claim", "attemptId": next.AttemptID}, 0)
	if claim.Candidate == nil || *claim.Candidate.Model != "fallback/model" || *claim.Candidate.Effort != EffortLow {
		t.Fatal("CLI fallback snapshot")
	}
	run("reports", map[string]any{"action": "report", "attemptId": next.AttemptID, "outcome": "created", "agentId": "child-b"}, 0)
	end := run("reports", map[string]any{"action": "report", "attemptId": next.AttemptID, "outcome": "task_failed", "agentId": "child-b", "executionState": "stopped", "reconciliation": "second child stopped; partial work retained", "taskFailure": map[string]any{"kind": "stagnation", "evidence": "no progress at checkpoint"}}, 0)
	if end.Action != "main-direct" || end.Attempts[0].TaskFailure == nil || end.Attempts[1].TaskFailure == nil {
		t.Fatal("CLI task failure not durable")
	}
	complete := run("completion", map[string]any{"action": "start", "role": "executor"}, 0)
	run("completion", map[string]any{"action": "claim", "attemptId": complete.AttemptID}, 0)
	run("completion", map[string]any{"action": "report", "attemptId": complete.AttemptID, "outcome": "created", "agentId": "child-a"}, 0)
	wrong := run("wrong", map[string]any{"action": "start", "role": "executor"}, 0)
	run("wrong", map[string]any{"action": "claim", "attemptId": wrong.AttemptID}, 0)
	// Replay the old oracle's accepted wrong ID through the parity API, then
	// prove the product CLI can close it across processes without another spawn.
	_, err = RunDispatch(ws, map[string]any{"action": "report", "sessionId": "session-test", "dispatchId": "wrong", "attemptId": wrong.AttemptID, "outcome": "created", "agentId": "PLACEHOLDER"}, env)
	check(t, err)
	check(t, os.Remove(filepath.Join(native, "app-server-control", "app-server-control.sock")))
	if got := run("completion", map[string]any{"action": "report", "attemptId": complete.AttemptID, "outcome": "complete", "agentId": "child-a"}, 0); got.Action != "complete" {
		t.Fatal("complete depends on connected host")
	}
	stop := run("wrong", map[string]any{"action": "report", "attemptId": wrong.AttemptID, "outcome": "stopped", "agentId": "PLACEHOLDER", "executionState": "stopped", "reconciliation": "actual child stopped; partial work inspected"}, 0)
	if stop.Action != "stop" || *stop.Attempts[0].AgentID != "PLACEHOLDER" {
		t.Fatal("CLI wrong-ID close")
	}
	for _, action := range []string{"status", "claim"} {
		if got := run("wrong", map[string]any{"action": action, "attemptId": wrong.AttemptID}, 0); got.Action != "stop" {
			t.Fatal("closed CLI dispatch reopened")
		}
	}
}

// Port of fallback-dispatch-cli.test.ts:13-47,49-76 using separate crw
// processes. Created successes use the host boundary test above; no live host
// or synthetic UUID grants creation authority here.
func TestDispatchCommandSeparateProcessesAndCorruptState(t *testing.T) {
	bin := testsupport.CRW(t)
	ws := t.TempDir()
	native := t.TempDir()
	global := t.TempDir()
	run := func(input string) (int, string) {
		t.Helper()
		cmd := exec.Command(bin, "role", "helper", "dispatch")
		cmd.Dir = ws
		cmd.Env = append(os.Environ(), "HOME="+t.TempDir(), "CODEX_HOME="+native, "CRW_HOME="+global, "CODEX_THREAD_ID=")
		cmd.Stdin = strings.NewReader(input)
		out, err := cmd.CombinedOutput()
		code := 0
		if err != nil {
			var exit *exec.ExitError
			if !errors.As(err, &exit) {
				t.Fatal(err)
			}
			code = exit.ExitCode()
		}
		return code, string(out)
	}
	call := func(fields map[string]any) DispatchResult {
		t.Helper()
		b := map[string]any{"sessionId": "session-test", "dispatchId": "task-test"}
		for k, v := range fields {
			b[k] = v
		}
		code, out := run(string(must(json.Marshal(b))))
		var r DispatchResult
		if code != 0 || json.Unmarshal([]byte(out), &r) != nil {
			t.Fatalf("process = %d %s", code, out)
		}
		return r
	}
	if code, out := run("{"); code != 1 || !strings.Contains(out, `"error"`) {
		t.Fatal("invalid JSON process")
	}
	first := call(map[string]any{"action": "start", "role": "executor"})
	claim := call(map[string]any{"action": "claim", "attemptId": first.AttemptID})
	if claim.Action != "spawn" {
		t.Fatal("claim")
	}
	end := call(map[string]any{"action": "report", "attemptId": first.AttemptID, "outcome": "failed", "error": "insufficient_quota", "executionState": "not_created", "reconciliation": "no native child was created"})
	if end.Action != "main-direct" || call(map[string]any{"action": "status"}).Action != "main-direct" {
		t.Fatal("restart state")
	}
	file := filepath.Join(ws, ".crw", "dispatches", "session-test", "task-test.json")
	check(t, os.WriteFile(file, []byte(`{}`), 0600))
	code, out := run(`{"action":"status","sessionId":"session-test","dispatchId":"task-test"}`)
	if code != 1 || !strings.Contains(out, "invalid dispatch identity") || string(must(os.ReadFile(file))) != "{}" {
		t.Fatal("corrupt state reset")
	}
}
