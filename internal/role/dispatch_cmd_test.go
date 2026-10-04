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
