package mcp

import (
	"context"
	"io"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver/fakehost"
)

type forwarded struct {
	state, program string
	argv           []string
	calls          int
}

// recordForward gives report_child a daemon client that records what the bridge sends and answers code and stdout, and a
// state resolver that answers state whatever socket it is asked about, as the relay's resolver does from the bridge's own
// environment.
func recordForward(t *testing.T, state string, code int, out string) *forwarded {
	t.Helper()
	got := &forwarded{}
	previousForward, previousState := forwardChild, childState
	RegisterChildReports(func(_ context.Context, state, program string, argv []string, stdout, _ io.Writer) (int, error) {
		got.calls++
		got.state, got.program, got.argv = state, program, slices.Clone(argv)
		_, _ = io.WriteString(stdout, out)
		return code, nil
	}, func(string) (string, error) { return state, nil })
	t.Cleanup(func() { RegisterChildReports(previousForward, previousState) })
	return got
}

func resultText(result *sdk.CallToolResult) string {
	text := ""
	for _, content := range result.Content {
		if raw, ok := content.(*sdk.TextContent); ok {
			text += raw.Text
		}
	}
	return text
}

// the options each child command needs, as a child gives them after the command name
var childCommandArguments = map[string][]string{
	"emit":               {"--relationship", "rel-1", "--generation", "1", "--outcome", "blocked_needs_input", "--turn-thread", "01child-task", "--turn-id", "turn-1"},
	"intent-claim":       {"--workspace", "/work", "--assignment", "dispatch-1", "--session", "01child-task", "--dispatch-request-id", "dispatch-1"},
	"intent-disposition": {"--workspace", "/work", "--assignment", "dispatch-1", "--session", "01child-task", "--turn", "turn-1", "--outcome", "blocked_needs_input"},
}

// CRW-1297: report_child hands a child's command to the relay daemon and nothing else: the program is crw, the command
// is the one named (emit, intent-claim or intent-disposition) followed by the child's own options, and the state the
// daemon is found in is the bridge's own configuration, never the call's arguments.
func TestReportChildForwardsToTheDaemonOnTheBridgesOwnState(t *testing.T) {
	home, env := isolated(t)
	state := filepath.Join(home, "relay-state")
	got := recordForward(t, state, 0, `{"stage":"final"}`)
	s := serve(t, []string{"--socket", fakehost.SocketPath(t, "absent.sock"), "--state-dir", filepath.Join(home, "ledger")}, env)
	arguments := childCommandArguments["emit"]
	result := call(t, s, "report_child", map[string]any{"command": "emit", "arguments": arguments})
	s.finish(t)
	if result.IsError || got.calls != 1 {
		t.Fatalf("report_child: error=%v calls=%d %s", result.IsError, got.calls, resultText(result))
	}
	if got.state != state || got.program != "crw" || !slices.Equal(got.argv, append([]string{"emit"}, arguments...)) {
		t.Fatalf("forwarded %+v, want state %s and argv emit + the arguments", got, state)
	}
	reply := structured(t, result)
	if reply["exitCode"] != float64(0) || reply["report"].(map[string]any)["stage"] != "final" {
		t.Fatalf("reply %v", reply)
	}
}

// CRW-1297: nothing the call says moves where the report goes. A --state, --socket or --kind-module is no option of a child
// command, so the bridge refuses the call, naming it, and asks the daemon nothing; the state the daemon is found in is the
// bridge's configuration (CODEX_SESSION_RELAY_STATE of its own environment) whatever the arguments hold.
func TestReportChildNeverTakesTheStateFromTheCall(t *testing.T) {
	home, env := isolated(t)
	state := filepath.Join(home, "relay-state")
	got := recordForward(t, state, 0, `{}`)
	s := serve(t, []string{"--socket", fakehost.SocketPath(t, "absent.sock"), "--state-dir", filepath.Join(home, "ledger")}, env)
	elsewhere := filepath.Join(home, "elsewhere")
	for _, extra := range [][]string{{"--state", elsewhere}, {"--state=" + elsewhere}, {"--socket", elsewhere}, {"--kind-module", "codex_session_relay.projects"}, {"--json"}, {"--help"}} {
		arguments := append(slices.Clone(extra), childCommandArguments["emit"]...)
		result := call(t, s, "report_child", map[string]any{"command": "emit", "arguments": arguments})
		if !result.IsError || !strings.Contains(resultText(result), "arguments are not the options of `emit`") {
			t.Errorf("%v: error=%v %s", extra, result.IsError, resultText(result))
		}
	}
	// an option the command lacks, and one it needs and is not given
	for _, arguments := range [][]string{append(slices.Clone(childCommandArguments["emit"]), "--state", elsewhere), {"--outcome", "blocked_needs_input"}} {
		if result := call(t, s, "report_child", map[string]any{"command": "emit", "arguments": arguments}); !result.IsError {
			t.Errorf("%v was forwarded", arguments)
		}
	}
	s.finish(t)
	if got.calls != 0 {
		t.Fatalf("the daemon was asked %d times about a call that named a state", got.calls)
	}
}

// CRW-1297: only the three child commands are reportable; any other command is refused by the tool's schema before
// the daemon is asked, and so is a missing argument list.
func TestReportChildAcceptsOnlyTheChildCommands(t *testing.T) {
	home, env := isolated(t)
	got := recordForward(t, filepath.Join(home, "relay-state"), 0, `{}`)
	s := serve(t, []string{"--socket", fakehost.SocketPath(t, "absent.sock"), "--state-dir", filepath.Join(home, "ledger")}, env)
	for _, command := range []string{"register", "deliver", "daemon", "--state", ""} {
		result := call(t, s, "report_child", map[string]any{"command": command, "arguments": []string{}})
		if !result.IsError || !strings.Contains(resultText(result), "command must be one of") {
			t.Errorf("command %q: error=%v %s", command, result.IsError, resultText(result))
		}
	}
	if result := call(t, s, "report_child", map[string]any{"command": "emit"}); !result.IsError || !strings.Contains(resultText(result), "arguments is required") {
		t.Errorf("no arguments: %v", resultText(result))
	}
	for command, arguments := range childCommandArguments {
		if result := call(t, s, "report_child", map[string]any{"command": command, "arguments": arguments}); result.IsError {
			t.Errorf("%s was refused: %s", command, resultText(result))
		}
	}
	s.finish(t)
	if got.calls != 3 {
		t.Fatalf("the daemon was asked %d times, want 3 (the child commands only)", got.calls)
	}
}

// CRW-1297: a daemon that refuses the command (a nonzero exit) is a tool error carrying the daemon's own answer, so the
// child sees why its report was not recorded.
func TestReportChildSaysWhyTheDaemonRefused(t *testing.T) {
	home, env := isolated(t)
	recordForward(t, filepath.Join(home, "relay-state"), 2, `{"error":"refused","reason":"scope_escape"}`)
	s := serve(t, []string{"--socket", fakehost.SocketPath(t, "absent.sock"), "--state-dir", filepath.Join(home, "ledger")}, env)
	result := call(t, s, "report_child", map[string]any{"command": "emit", "arguments": childCommandArguments["emit"]})
	s.finish(t)
	if text := resultText(result); !result.IsError || !strings.Contains(text, "exit 2") || !strings.Contains(text, "scope_escape") {
		t.Fatalf("expected the daemon's refusal, got error=%v %s", result.IsError, text)
	}
}
