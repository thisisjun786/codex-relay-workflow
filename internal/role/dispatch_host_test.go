package role

import (
	"bytes"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/host"
)

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
			got, err := DispatchSocket(env)
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("socket %q, want %q", got, tc.want)
			}
		})
	}
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

// dispatchHostEnv points CODEX_HOME at native and leaves HOME and CRW_HOME temporary.
func dispatchHostEnv(t *testing.T, native string) host.LookupEnv {
	t.Helper()
	base, _ := home(t)
	return func(key string) (string, bool) {
		if key == "CODEX_HOME" {
			return native, true
		}
		return base(key)
	}
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

// dispatchHostNative is a temporary CODEX_HOME holding the child-a row the created report reads.
func dispatchHostNative(t *testing.T) string {
	t.Helper()
	native := t.TempDir()
	createdCheckSeed(t, native, "child-a", "session-test")
	return native
}

// dispatchHostRestoreOpen makes the test the owner of OpenDispatchHost and restores it after.
func dispatchHostRestoreOpen(t *testing.T) {
	t.Helper()
	was := OpenDispatchHost
	t.Cleanup(func() { OpenDispatchHost = was })
}

// With no opener installed the CLI is exactly as it was before the host existed: the stopped close
// goes through, reads no turn and says the runtime was not confirmed. This is also the path the
// library default (a nil OpenDispatchHost) takes.
func TestDispatchCommandWithoutAnOpenerClosesAsOnDev(t *testing.T) {
	dispatchHostRestoreOpen(t)
	OpenDispatchHost = nil
	ws := t.TempDir()
	t.Chdir(ws)
	env := dispatchHostEnv(t, dispatchHostNative(t))
	attempt, file := dispatchHostStarted(t, ws, env)
	out, err := dispatchHostCommand(t, env, dispatchHostStopped(attempt))
	check(t, err)
	if out.Action != "stop" || out.Reason != createdRuntimeClosed+"; "+createdRuntimeNote {
		t.Fatalf("close without an opener = %q %q", out.Action, out.Reason)
	}
	if stored := must(dispatchRead(file, "session-test", "task-test")); stored.Status != "stopped" {
		t.Fatalf("closure not durable: %+v", stored)
	}
}

// The opener is asked for only a stopped close: a start, a claim, a created report and a status
// never dial. The counting opener here proves which inputs reach it.
func TestDispatchCommandOpensTheHostOnlyForAStoppedClose(t *testing.T) {
	dispatchHostRestoreOpen(t)
	ws := t.TempDir()
	t.Chdir(ws)
	env := dispatchHostEnv(t, dispatchHostNative(t))
	calls := 0
	OpenDispatchHost = func(host.LookupEnv) (DispatchHost, func(), error) {
		calls++
		return nil, func() {}, nil
	}
	start, err := dispatchHostCommand(t, env, map[string]any{"action": "start", "role": "executor"})
	check(t, err)
	if _, err := dispatchHostCommand(t, env, map[string]any{"action": "claim", "attemptId": start.AttemptID}); err != nil {
		t.Fatal(err)
	}
	if _, err := dispatchHostCommand(t, env, map[string]any{"action": "report", "attemptId": start.AttemptID, "outcome": "created", "agentId": "child-a"}); err != nil {
		t.Fatal(err)
	}
	if _, err := dispatchHostCommand(t, env, map[string]any{"action": "status"}); err != nil {
		t.Fatal(err)
	}
	if calls != 0 {
		t.Fatalf("the opener was asked for %d times before a stopped close", calls)
	}
	if _, err := dispatchHostCommand(t, env, dispatchHostStopped(start.AttemptID)); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("the opener was asked for %d times, want 1 for the stopped close", calls)
	}
}

// A stopped close whose opener fails still runs on the native database, as it does with no host.
func TestDispatchCommandFallsBackWhenTheOpenerFails(t *testing.T) {
	dispatchHostRestoreOpen(t)
	ws := t.TempDir()
	t.Chdir(ws)
	env := dispatchHostEnv(t, dispatchHostNative(t))
	OpenDispatchHost = func(host.LookupEnv) (DispatchHost, func(), error) {
		return nil, nil, errors.New("dial failed")
	}
	attempt, _ := dispatchHostStarted(t, ws, env)
	out, err := dispatchHostCommand(t, env, dispatchHostStopped(attempt))
	check(t, err)
	if out.Action != "stop" || out.Reason != createdRuntimeClosed+"; "+createdRuntimeNote {
		t.Fatalf("close with a failing opener = %q %q", out.Action, out.Reason)
	}
}

// An input that cannot be read as a dispatch record never dials either: the opener is reached only
// for a record that already reads as a stopped report, so malformed or non-object input takes the
// same nil-host path as on dev and CheckedDispatch produces its canonical refusal.
func TestDispatchCommandNeverDialsForUnreadableInput(t *testing.T) {
	dispatchHostRestoreOpen(t)
	ws := t.TempDir()
	t.Chdir(ws)
	env := dispatchHostEnv(t, dispatchHostNative(t))
	calls := 0
	OpenDispatchHost = func(host.LookupEnv) (DispatchHost, func(), error) {
		calls++
		return nil, func() {}, nil
	}
	for _, input := range []string{"[]", "null", "{}", `{"action":"report"}`, `{"action":"report","outcome":"stopped"}`} {
		var out bytes.Buffer
		_ = DispatchCommand(nil, strings.NewReader(input), &out, env)
	}
	if calls != 0 {
		t.Fatalf("the opener was asked for %d times for unreadable or incomplete input", calls)
	}
}
