package manage

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/coder/websocket"
	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver/fakehost"
)

// resumeFakeCallMarker separates the argument blocks the fake crw records, one per invocation.
const resumeFakeCallMarker = "CALL"

// resumeRelayScript writes a fake crw that records every argument it is given and answers the
// doctor, assignment-show and settings-show reads child-resume makes. Nothing here reaches the
// real relay: every answer comes from a file in the test's own temporary directory.
func resumeRelayScript(t *testing.T, assignment, settings string, exit int) (exe, record string) {
	t.Helper()
	dir := t.TempDir()
	record = filepath.Join(dir, "argv.txt")
	assignmentPath := filepath.Join(dir, "assignment.json")
	settingsPath := filepath.Join(dir, "settings.json")
	if err := os.WriteFile(assignmentPath, []byte(assignment), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(settingsPath, []byte(settings), 0o600); err != nil {
		t.Fatal(err)
	}
	exe = filepath.Join(dir, "crw")
	script := "#!/bin/sh\n" +
		"{ printf '%s\\n' " + resumeFakeCallMarker + "; for a in \"$@\"; do printf '%s\\n' \"$a\"; done; } >> " + resumeShellQuote(record) + "\n" +
		"case \"$*\" in\n" +
		"  *doctor*) printf '%s\\n' '{\"stateSelection\":{\"path\":\"/tmp/relay-store\"}}' ;;\n" +
		"  *assignment-show*) cat " + resumeShellQuote(assignmentPath) + " ;;\n" +
		"  *settings-show*) cat " + resumeShellQuote(settingsPath) + " ;;\n" +
		"esac\n" +
		"exit " + fmt.Sprint(exit) + "\n"
	if err := os.WriteFile(exe, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return exe, record
}

func resumeShellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'" }

// resumeCalls reads the fake crw's record back as one slice of arguments per invocation.
func resumeCalls(t *testing.T, record string) [][]string {
	t.Helper()
	data, err := os.ReadFile(record)
	if err != nil {
		t.Fatal(err)
	}
	var calls [][]string
	for _, line := range strings.Split(strings.TrimRight(string(data), "\n"), "\n") {
		switch {
		case line == "":
		case line == resumeFakeCallMarker:
			calls = append(calls, nil)
		default:
			calls[len(calls)-1] = append(calls[len(calls)-1], line)
		}
	}
	return calls
}

// resumeRelayCommands is the relay subcommand of every recorded call, in order.
func resumeRelayCommands(t *testing.T, calls [][]string) []string {
	t.Helper()
	var out []string
	for _, call := range calls {
		// The relay subcommand is the first token after the leading "relay" and the helper's own
		// flags with their values.
		for i := 1; i < len(call); i++ {
			if strings.HasPrefix(call[i], "--") {
				i++
				continue
			}
			out = append(out, call[i])
			break
		}
	}
	return out
}

const (
	resumeTestAssignment = `{"relationshipId":"rel-1","issueKey":"CRW-1","parentTaskId":"01parent","childTaskId":"01child","executionGeneration":3}`
	resumeTestSettings   = `{"task":"01child","usable":true,"settings":{"sandbox":{"type":"readOnly","networkAccess":false},"approvalPolicy":"never","cwd":"/w","runtimeWorkspaceRoots":["/w"],"model":"m","reasoningEffort":"xhigh"}}`
)

// resumeHost scripts a fake App Server whose thread is idle, whose resume keeps the recorded
// settings and whose two listed servers are disabled.
func resumeHost(t *testing.T, status string) *fakehost.Server {
	t.Helper()
	host := fakehost.Start(t)
	host.Respond("thread/read", fakehost.Reply{Result: map[string]any{"thread": map[string]any{
		"model": "m", "reasoningEffort": "xhigh", "status": map[string]any{"type": status},
	}}})
	host.Respond("thread/resume", fakehost.Reply{Result: map[string]any{"model": "m", "reasoningEffort": "xhigh"}})
	host.Respond("mcpServerStatus/list", fakehost.Reply{Result: map[string]any{"data": []any{
		map[string]any{"name": "alpha", "runtimeStatus": "disabled"},
		map[string]any{"name": "beta", "runtimeStatus": "disabled"},
	}, "nextCursor": nil}})
	host.Respond("turn/start", fakehost.Reply{Result: map[string]any{"turn": map[string]any{"id": "turn-1"}}})
	return host
}

// resumeHostMethods is the App Server methods a run called, without the connection's handshake and
// without the subscription release. The handshake and thread/unsubscribe belong to the client and
// the watch it admits rather than to the resume's own steps, and the release is not deterministic
// here: it is asked for when the watch finishes, while the run closes its client on the way out.
func resumeHostMethods(host *fakehost.Server) []string {
	var methods []string
	for _, request := range host.Requests() {
		if request.Method != "initialize" && request.Method != "initialized" && request.Method != "thread/unsubscribe" {
			methods = append(methods, request.Method)
		}
	}
	return methods
}

// resumeHostConnections is how many App Server connections a run opened, counted by the
// handshake the client performs once per connection.
func resumeHostConnections(host *fakehost.Server) int { return host.Count("initialize") }

// resumeHostRequests is those requests themselves, in order.
func resumeHostRequests(host *fakehost.Server) []fakehost.Request {
	var requests []fakehost.Request
	for _, request := range host.Requests() {
		if request.Method != "initialize" && request.Method != "initialized" {
			requests = append(requests, request)
		}
	}
	return requests
}

// resumeConfig is the configuration a run is driven with: the fake crw's socket and the
// child_check section naming the servers to stop.
func resumeConfig(host *fakehost.Server, disabled ...string) *Config {
	cfg := hostReadConfig(host.SocketPath)
	cfg.Relay.State = "/tmp/relay-store"
	if len(disabled) > 0 {
		list, err := json.Marshal(map[string]any{"disabled_servers": disabled})
		if err != nil {
			panic(err)
		}
		cfg.raw["child_check"] = list
	}
	return cfg
}

// resumeEnv points HOME, CODEX_HOME and XDG_STATE_HOME at a fresh temporary tree and returns the
// Env a run gets: the fake crw, the process environment and streams a test can read.
func resumeEnv(t *testing.T, exe string) (*Env, *strings.Builder, *strings.Builder) {
	t.Helper()
	hostReadEnv(t, "")
	var out, errOut strings.Builder
	return &Env{Stdout: &out, Stderr: &errOut, Getenv: os.Getenv, Executable: exe}, &out, &errOut
}

func resumeStrings(v any) []string {
	list, _ := v.([]any)
	out := make([]string, 0, len(list))
	for _, one := range list {
		text, _ := one.(string)
		out = append(out, text)
	}
	return out
}

// C2: the happy path calls read, resume, status and turn/start once each, in that order, with
// the recorded settings, the servers switched off and the message file's bytes verbatim.
func TestResumeHappyPathFixesTheOrderAndArguments(t *testing.T) {
	host := resumeHost(t, "idle")
	exe, record := resumeRelayScript(t, resumeTestAssignment, resumeTestSettings, 0)
	e, out, _ := resumeEnv(t, exe)
	message := "continue where you stopped\nsecond line\n"
	report, err := resumeRun(context.Background(), e, resumeConfig(host, "alpha", "beta"), resumeOptions{relationship: "rel-1", message: message})
	if err != nil {
		t.Fatal(err)
	}
	if methods := resumeHostMethods(host); !slices.Equal(methods, []string{"thread/read", "thread/resume", "mcpServerStatus/list", "turn/start"}) {
		t.Fatalf("the host saw %q", methods)
	}
	if n := resumeHostConnections(host); n != 1 {
		t.Fatalf("the four steps used %d connections, want 1", n)
	}
	requests := resumeHostRequests(host)
	var read struct{ ThreadID string }
	if err := json.Unmarshal(requests[0].Params, &read); err != nil || read.ThreadID != "01child" {
		t.Fatalf("thread/read params %s (%v)", requests[0].Params, err)
	}
	var resume map[string]any
	if err := json.Unmarshal(requests[1].Params, &resume); err != nil {
		t.Fatal(err)
	}
	if resume["threadId"] != "01child" || resume["excludeTurns"] != true || resume["model"] != "m" ||
		resume["cwd"] != "/w" || resume["sandbox"] != "read-only" || resume["approvalPolicy"] != "never" ||
		!slices.Equal(resumeStrings(resume["runtimeWorkspaceRoots"]), []string{"/w"}) {
		t.Fatalf("resume params %v", resume)
	}
	config, _ := resume["config"].(map[string]any)
	if config["model_reasoning_effort"] != "xhigh" {
		t.Fatalf("resume config %v", config)
	}
	servers, _ := config["mcp_servers"].(map[string]any)
	for _, name := range []string{"alpha", "beta"} {
		one, _ := servers[name].(map[string]any)
		if one["enabled"] != false {
			t.Fatalf("server %s was not switched off: %v", name, servers)
		}
	}
	var status struct{ ThreadID string }
	if err := json.Unmarshal(requests[2].Params, &status); err != nil || status.ThreadID != "01child" {
		t.Fatalf("status params %s (%v)", requests[2].Params, err)
	}
	var turn struct {
		ThreadID string
		Input    []struct{ Type, Text string }
	}
	if err := json.Unmarshal(requests[3].Params, &turn); err != nil {
		t.Fatal(err)
	}
	if turn.ThreadID != "01child" || len(turn.Input) != 1 || turn.Input[0].Type != "text" || turn.Input[0].Text != message {
		t.Fatalf("turn/start params %s", requests[3].Params)
	}
	if !report.OK || report.Thread != "01child" || report.TurnID != "turn-1" || report.Generation != 3 ||
		report.Model != "m" || report.Effort != "xhigh" || report.DryRun {
		t.Fatalf("report %+v", report)
	}
	if report.Servers["alpha"] != "disabled" || report.Servers["beta"] != "disabled" {
		t.Fatalf("servers %v", report.Servers)
	}
	for _, want := range []string{"admit-turn", "--relationship", "rel-1", "--generation", "3", "--turn", "turn-1", "--actor", "01parent"} {
		if !strings.Contains(report.AdmitTurn, want) {
			t.Errorf("the admit-turn line %q is missing %q", report.AdmitTurn, want)
		}
	}
	if out.Len() != 0 {
		t.Errorf("resumeRun wrote %q to stdout", out.String())
	}
	if calls := resumeRelayCommands(t, resumeCalls(t, record)); !slices.Equal(calls, []string{"assignment-show", "settings-show"}) {
		t.Errorf("the relay saw %q", calls)
	}
}

// C1: an active thread, a resume that answers with other settings, and one server left enabled
// each refuse without calling turn/start.
func TestResumeRefusesWithoutStartingATurn(t *testing.T) {
	cases := []struct {
		name     string
		script   func(host *fakehost.Server)
		wantCode int
		want     string
	}{
		{"active", func(host *fakehost.Server) {
			host.Respond("thread/read", fakehost.Reply{Result: map[string]any{"thread": map[string]any{
				"model": "m", "reasoningEffort": "xhigh", "status": map[string]any{"type": "active"}}}})
		}, 3, resumeThreadActive},
		{"settings differ", func(host *fakehost.Server) {
			host.Respond("thread/resume", fakehost.Reply{Result: map[string]any{"model": "other", "reasoningEffort": "xhigh"}})
		}, 4, resumeSettingsMismatch},
		{"server enabled", func(host *fakehost.Server) {
			host.Respond("mcpServerStatus/list", fakehost.Reply{Result: map[string]any{"data": []any{
				map[string]any{"name": "alpha", "runtimeStatus": "disabled"},
				map[string]any{"name": "beta", "runtimeStatus": "connected"},
			}, "nextCursor": nil}})
		}, 4, resumeMCPNotDisabled},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			host := resumeHost(t, "idle")
			test.script(host)
			exe, record := resumeRelayScript(t, resumeTestAssignment, resumeTestSettings, 0)
			e, _, _ := resumeEnv(t, exe)
			_, err := resumeRun(context.Background(), e, resumeConfig(host, "alpha", "beta"), resumeOptions{relationship: "rel-1", message: "m"})
			failure, ok := err.(*resumeFailure)
			if !ok || failure.Reason != test.want {
				t.Fatalf("err = %v, want %s", err, test.want)
			}
			if code := resumeExit(failure); code != test.wantCode {
				t.Errorf("%s exits %d, want %d", test.want, code, test.wantCode)
			}
			if n := host.Count("turn/start"); n != 0 {
				t.Errorf("turn/start was called %d times", n)
			}
			if failure.Detail == "" {
				t.Errorf("the refusal carries no detail")
			}
			if calls := resumeRelayCommands(t, resumeCalls(t, record)); !slices.Equal(calls, []string{"assignment-show", "settings-show"}) {
				t.Errorf("the relay saw %q", calls)
			}
		})
	}
}

// C3: every relay call is a read, with the state the doctor named and the configured socket; no
// write command is issued.
func TestResumeCallsOnlyRelayReads(t *testing.T) {
	host := resumeHost(t, "idle")
	exe, record := resumeRelayScript(t, resumeTestAssignment, resumeTestSettings, 0)
	e, _, _ := resumeEnv(t, exe)
	if _, err := resumeRun(context.Background(), e, resumeConfig(host, "alpha"), resumeOptions{relationship: "rel-1", message: "m"}); err != nil {
		t.Fatal(err)
	}
	calls := resumeCalls(t, record)
	if len(calls) != 2 {
		t.Fatalf("the relay saw %d calls: %q", len(calls), calls)
	}
	for _, call := range calls {
		if len(call) < 4 || call[0] != "relay" || call[1] != "--state" || call[3] != "--socket" {
			t.Fatalf("a relay call is not the relay helper's: %q", call)
		}
		command := call[5:]
		if len(command) == 0 {
			t.Fatalf("a relay call names no command: %q", call)
		}
		if command[0] != "assignment-show" && command[0] != "settings-show" {
			t.Errorf("the relay was asked to run %q", command[0])
		}
	}
}

// --dry-run reads the thread, compares the settings with the record and changes nothing on the
// host: no resume, no server list, no turn.
func TestResumeDryRunSendsNothing(t *testing.T) {
	host := resumeHost(t, "notLoaded")
	exe, record := resumeRelayScript(t, resumeTestAssignment, resumeTestSettings, 0)
	e, _, _ := resumeEnv(t, exe)
	report, err := resumeRun(context.Background(), e, resumeConfig(host, "alpha"), resumeOptions{relationship: "rel-1", message: "m", dryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if !report.DryRun || report.TurnID != "" || report.AdmitTurn != "" {
		t.Fatalf("report %+v", report)
	}
	if methods := resumeHostMethods(host); !slices.Equal(methods, []string{"thread/read"}) {
		t.Fatalf("dry-run sent %q", methods)
	}
	if n := resumeHostConnections(host); n != 1 {
		t.Fatalf("the dry run used %d connections, want 1", n)
	}
	if calls := resumeRelayCommands(t, resumeCalls(t, record)); !slices.Equal(calls, []string{"assignment-show", "settings-show"}) {
		t.Errorf("dry-run asked the relay for %q", calls)
	}
	// A read-only run must not mutate the host. thread/read never subscribed this connection, so the
	// finished watch must retain the root rather than queue a thread/unsubscribe. Before the fix the
	// watch was finished with retain=false, making it eligible for release: the subscription
	// worker raced client.Close and occasionally sent the unsubscribe anyway.
	if n := host.Count("thread/unsubscribe"); n != 0 {
		t.Errorf("the dry run sent thread/unsubscribe %d times; a read-only run changes nothing on the host", n)
	}
}

// The command line: a missing option and an unreadable message file are usage errors, -h prints
// the usage, and each refusal is one JSON object on stdout with the status it belongs to.
func TestResumeCommandLineAndRefusals(t *testing.T) {
	host := resumeHost(t, "idle")
	exe, _ := resumeRelayScript(t, resumeTestAssignment, resumeTestSettings, 0)
	messageFile := filepath.Join(t.TempDir(), "message.txt")
	if err := os.WriteFile(messageFile, []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{}, {"--relationship", "rel-1"}, {"--message-file", messageFile},
		{"--relationship", "rel-1", "--message-file", filepath.Join(t.TempDir(), "absent")},
		{"--relationship", "rel-1", "--message-file", messageFile, "--nope", "x"},
	} {
		e, _, _ := resumeEnv(t, exe)
		if code := resumeRunCommand(context.Background(), e, args); code != usageExit {
			t.Errorf("%q: exit %d, want %d", args, code, usageExit)
		}
	}
	e, out, _ := resumeEnv(t, exe)
	if code := resumeRunCommand(context.Background(), e, []string{"-h"}); code != 0 || !strings.Contains(out.String(), "usage: crw manage child-resume") {
		t.Fatalf("-h: exit %d, output %q", code, out.String())
	}
	// disabled_servers_unset: the config section names no servers. This is what the command line
	// answers until a configuration file is loaded (the same seam child-check has).
	e, out, _ = resumeEnv(t, exe)
	if code := resumeRunCommand(context.Background(), e, []string{"--relationship", "rel-1", "--message-file", messageFile}); code != usageExit ||
		!strings.Contains(out.String(), resumeDisabledServersUnset) {
		t.Fatalf("no disabled servers: exit %d, output %q", code, out.String())
	}
	// A relay that fails is assignment_unavailable.
	exe3, _ := resumeRelayScript(t, resumeTestAssignment, resumeTestSettings, 1)
	e, out, _ = resumeEnv(t, exe3)
	if code := resumeRunCommand(context.Background(), e, []string{"--relationship", "rel-1", "--message-file", messageFile}); code != 3 ||
		!strings.Contains(out.String(), resumeAssignmentUnavailable) {
		t.Fatalf("a failing relay: exit %d, output %q", code, out.String())
	}
	// settings_unavailable: a recorded block missing a setting a resume carries. The settings are
	// read before the server list, so this is what a record with a field missing reports.
	missing := `{"settings":{"sandbox":{"type":"readOnly","networkAccess":false},"approvalPolicy":"never","cwd":"/w","runtimeWorkspaceRoots":["/w"],"model":"m"}}`
	missingExe, _ := resumeRelayScript(t, resumeTestAssignment, missing, 0)
	e, _, _ = resumeEnv(t, missingExe)
	_, err := resumeRun(context.Background(), e, resumeConfig(host, "alpha"), resumeOptions{relationship: "rel-1", message: "m"})
	if failure, ok := err.(*resumeFailure); !ok || failure.Reason != resumeSettingsUnavailable || resumeExit(failure) != usageExit ||
		!strings.Contains(failure.Detail, "reasoningEffort") {
		t.Fatalf("missing reasoningEffort: err = %v", err)
	}
	// A socket nobody listens on is host_unreachable, and the failure names it on stderr too.
	hostReadEnv(t, "")
	var failed strings.Builder
	var errOut strings.Builder
	e = &Env{Stdout: &failed, Stderr: &errOut, Getenv: os.Getenv, Executable: exe}
	cfg := resumeConfig(host, "alpha")
	cfg.Relay.Socket = filepath.Join(t.TempDir(), "absent.sock")
	if _, err := resumeRun(context.Background(), e, cfg, resumeOptions{relationship: "rel-1", message: "m"}); err == nil {
		t.Fatal("a socket nobody listens on was accepted")
	} else if failure, ok := err.(*resumeFailure); !ok || failure.Reason != "host_unreachable" || resumeExit(failure) != 3 {
		t.Fatalf("err = %v", err)
	}
}

// --dry-run on a thread whose reported settings disagree with the record reports the mismatch and
// starts nothing.
func TestResumeDryRunReportsASettingsMismatch(t *testing.T) {
	host := resumeHost(t, "notLoaded")
	host.Respond("thread/read", fakehost.Reply{Result: map[string]any{"thread": map[string]any{
		"model": "other", "reasoningEffort": "xhigh", "status": map[string]any{"type": "notLoaded"}}}})
	exe, _ := resumeRelayScript(t, resumeTestAssignment, resumeTestSettings, 0)
	e, _, _ := resumeEnv(t, exe)
	_, err := resumeRun(context.Background(), e, resumeConfig(host, "alpha"), resumeOptions{relationship: "rel-1", message: "m", dryRun: true})
	failure, ok := err.(*resumeFailure)
	if !ok || failure.Reason != resumeSettingsMismatch || resumeExit(failure) != 4 {
		t.Fatalf("err = %v", err)
	}
	if methods := resumeHostMethods(host); !slices.Equal(methods, []string{"thread/read"}) {
		t.Fatalf("the dry run sent %q", methods)
	}
}

// An assignment that names no parent is refused: the admit-turn line it would print could not be
// run by anybody.
func TestResumeRefusesAnAssignmentWithoutAParent(t *testing.T) {
	host := resumeHost(t, "idle")
	exe, _ := resumeRelayScript(t, `{"childTaskId":"01child","executionGeneration":3}`, resumeTestSettings, 0)
	e, _, _ := resumeEnv(t, exe)
	_, err := resumeRun(context.Background(), e, resumeConfig(host, "alpha"), resumeOptions{relationship: "rel-1", message: "m"})
	failure, ok := err.(*resumeFailure)
	if !ok || failure.Reason != resumeAssignmentUnavailable || resumeExit(failure) != 3 {
		t.Fatalf("err = %v", err)
	}
	if n := host.Count("thread/read"); n != 0 {
		t.Errorf("the host was contacted %d times before the assignment was refused", n)
	}
}

// A recorded workspace-write policy is carried in full: the resume's config names the writable
// roots and the three flags, so the host cannot apply its own defaults instead.
func TestResumeCarriesTheRecordedWorkspaceWritePolicy(t *testing.T) {
	host := resumeHost(t, "notLoaded")
	write := `{"settings":{"sandbox":{"type":"workspaceWrite","writableRoots":["/w"],"networkAccess":true,"excludeTmpdirEnvVar":false,"excludeSlashTmp":true},"approvalPolicy":"never","cwd":"/w","runtimeWorkspaceRoots":["/w"],"model":"m","reasoningEffort":"xhigh"}}`
	exe, _ := resumeRelayScript(t, resumeTestAssignment, write, 0)
	e, _, _ := resumeEnv(t, exe)
	if _, err := resumeRun(context.Background(), e, resumeConfig(host, "alpha"), resumeOptions{relationship: "rel-1", message: "m"}); err != nil {
		t.Fatal(err)
	}
	var resume map[string]any
	if err := json.Unmarshal(resumeHostRequests(host)[1].Params, &resume); err != nil {
		t.Fatal(err)
	}
	if resume["sandbox"] != "workspace-write" {
		t.Fatalf("sandbox mode %v", resume["sandbox"])
	}
	config, _ := resume["config"].(map[string]any)
	section, _ := config["sandbox_workspace_write"].(map[string]any)
	if !slices.Equal(resumeStrings(section["writable_roots"]), []string{"/w"}) || section["network_access"] != true ||
		section["exclude_tmpdir_env_var"] != false || section["exclude_slash_tmp"] != true {
		t.Fatalf("sandbox_workspace_write %v", section)
	}
}

// A record the relay calls not deliverable is refused: resuming with settings the relay itself
// will not send with would carry an authorization nobody checked.
func TestResumeRefusesANonDeliverableRecord(t *testing.T) {
	host := resumeHost(t, "notLoaded")
	undeliverable := `{"settings":{"sandbox":{"type":"readOnly","networkAccess":false},"approvalPolicy":"never","cwd":"/w","runtimeWorkspaceRoots":["/w"],"model":"m","reasoningEffort":"xhigh"},"deliverable":false,"roleFinding":{"code":"role_binding_mismatch","detail":"two live bindings"}}`
	exe, _ := resumeRelayScript(t, resumeTestAssignment, undeliverable, 0)
	e, _, _ := resumeEnv(t, exe)
	_, err := resumeRun(context.Background(), e, resumeConfig(host, "alpha"), resumeOptions{relationship: "rel-1", message: "m"})
	failure, ok := err.(*resumeFailure)
	if !ok || failure.Reason != resumeSettingsUnavailable || resumeExit(failure) != usageExit ||
		!strings.Contains(failure.Detail, "role_binding_mismatch") {
		t.Fatalf("err = %v", err)
	}
	if n := host.Count("thread/read"); n != 0 {
		t.Errorf("the host was contacted %d times before the record was refused", n)
	}
}

// A turn/start whose answer is lost is an unknown outcome, not an ordinary host error: the turn
// may already have started, so the operator is told to read the thread before resending.
func TestResumeReportsALostTurnStartAsUncertain(t *testing.T) {
	host := resumeHost(t, "notLoaded")
	host.Respond("turn/start", fakehost.Reply{Close: &fakehost.CloseFrame{Code: 1011, Reason: "lost"}})
	exe, _ := resumeRelayScript(t, resumeTestAssignment, resumeTestSettings, 0)
	e, _, _ := resumeEnv(t, exe)
	_, err := resumeRun(context.Background(), e, resumeConfig(host, "alpha"), resumeOptions{relationship: "rel-1", message: "m"})
	failure, ok := err.(*resumeFailure)
	if !ok || failure.Reason != resumeTurnStartUncertain || resumeExit(failure) != 3 {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(failure.Detail, "read the thread") {
		t.Errorf("the refusal does not tell the operator to read the thread: %q", failure.Detail)
	}
}

// A dry run against a thread that reports no settings says it cannot verify them, rather than
// reporting a disagreement the record would not have.
func TestResumeDryRunReportsUnverifiedSettings(t *testing.T) {
	host := resumeHost(t, "notLoaded")
	host.Respond("thread/read", fakehost.Reply{Result: map[string]any{"thread": map[string]any{
		"status": map[string]any{"type": "notLoaded"}}}})
	exe, _ := resumeRelayScript(t, resumeTestAssignment, resumeTestSettings, 0)
	e, _, _ := resumeEnv(t, exe)
	_, err := resumeRun(context.Background(), e, resumeConfig(host, "alpha"), resumeOptions{relationship: "rel-1", message: "m", dryRun: true})
	failure, ok := err.(*resumeFailure)
	if !ok || failure.Reason != resumeSettingsUnverified || resumeExit(failure) != 3 {
		t.Fatalf("err = %v", err)
	}
	if methods := resumeHostMethods(host); !slices.Equal(methods, []string{"thread/read"}) {
		t.Fatalf("the dry run sent %q", methods)
	}
}

// The admit-turn line names the program this runtime is reached by, not a bare crw that an
// installation does not put on PATH.
func TestResumeAdmitTurnNamesTheResolvedProgram(t *testing.T) {
	line := resumeAdmitTurn(resumeProgram(&Env{Executable: "/opt/crw/bin/crw"}), &Config{Relay: coreRelay{State: "/s", Socket: "/k"}}, "rel-1", 3, "turn-1", "01parent")
	if strings.HasPrefix(line, "crw ") || !strings.Contains(line, "/opt/crw/bin/crw relay --state /s --socket /k admit-turn") {
		t.Fatalf("the line does not name the resolved runtime: %q", line)
	}
	if got := resumeProgram(&Env{}); len(got) != 1 || got[0] != "codex-session-relay" {
		t.Fatalf("an unnamed executable falls back to %q", got)
	}
}

// The command registers itself, so crw manage lists it.
func TestResumeIsRegistered(t *testing.T) {
	if !slices.Contains(coreNames(), "child-resume") {
		t.Fatalf("child-resume is not registered: %q", coreNames())
	}
}

// resumeHostLog records the App Server methods a fake host answered, across every connection it
// accepted, so a test can count handshakes and prove a step was never sent.
type resumeHostLog struct {
	mu      sync.Mutex
	methods []string
}

func (l *resumeHostLog) add(method string) {
	l.mu.Lock()
	l.methods = append(l.methods, method)
	l.mu.Unlock()
}

func (l *resumeHostLog) count(method string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, one := range l.methods {
		if one == method {
			n++
		}
	}
	return n
}

// resumeDroppingHost is a fake App Server that answers the handshake, thread/read and
// thread/resume and then drops the connection immediately after the thread/resume answer. A
// later step on that socket fails; a client that reconnects is answered again on a second
// connection, which is exactly what the one-connection fence must prevent.
func resumeDroppingHost(t *testing.T) (string, *resumeHostLog) {
	t.Helper()
	log := &resumeHostLog{}
	socket := fakehost.SocketPath(t, "app.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{CompressionMode: websocket.CompressionDisabled})
		if err != nil {
			return
		}
		defer conn.CloseNow()
		for {
			_, raw, err := conn.Read(r.Context())
			if err != nil {
				return
			}
			var message struct {
				ID     json.RawMessage `json:"id"`
				Method string          `json:"method"`
			}
			if json.Unmarshal(raw, &message) != nil || message.Method == "" {
				continue
			}
			log.add(message.Method)
			answer := func(result map[string]any) {
				frame, _ := json.Marshal(map[string]any{"id": message.ID, "result": result})
				_ = conn.Write(r.Context(), websocket.MessageText, frame)
			}
			switch message.Method {
			case "initialize":
				answer(map[string]any{"userAgent": "dropping-host"})
			case "initialized":
			case "thread/read":
				answer(map[string]any{"thread": map[string]any{"model": "m", "reasoningEffort": "xhigh", "status": map[string]any{"type": "idle"}}})
			case "thread/resume":
				answer(map[string]any{"model": "m", "reasoningEffort": "xhigh"})
				return // the peer drops the connection after the resume answer
			case "mcpServerStatus/list":
				answer(map[string]any{"data": []any{map[string]any{"name": "alpha", "runtimeStatus": "disabled"}}, "nextCursor": nil})
			case "turn/start":
				answer(map[string]any{"turn": map[string]any{"id": "turn-1"}})
			default:
				frame, _ := json.Marshal(map[string]any{"id": message.ID, "error": map[string]any{"code": -32601, "message": message.Method}})
				_ = conn.Write(r.Context(), websocket.MessageText, frame)
			}
		}
	})}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close(); _ = listener.Close() })
	return socket, log
}

// C1: a connection that drops after the thread/resume answer ends the run as host_error naming
// the step it failed at, sends no turn/start on any connection and initializes once. Before the
// fence the client silently dialed a second connection and started the turn there.
func TestResumeFailsWhenTheConnectionDropsAfterResume(t *testing.T) {
	socket, log := resumeDroppingHost(t)
	exe, _ := resumeRelayScript(t, resumeTestAssignment, resumeTestSettings, 0)
	e, _, _ := resumeEnv(t, exe)
	cfg := hostReadConfig(socket)
	cfg.Relay.State = "/tmp/relay-store"
	list, err := json.Marshal(map[string]any{"disabled_servers": []string{"alpha"}})
	if err != nil {
		t.Fatal(err)
	}
	cfg.raw["child_check"] = list
	_, err = resumeRun(context.Background(), e, cfg, resumeOptions{relationship: "rel-1", message: "m"})
	failure, ok := err.(*resumeFailure)
	if !ok || failure.Reason != string(hostReadHostError) {
		t.Fatalf("err = %v, want host_error", err)
	}
	if !strings.Contains(failure.Detail, "mcpServerStatus/list") {
		t.Errorf("the refusal does not name the step it failed at: %q", failure.Detail)
	}
	if n := log.count("initialize"); n != 1 {
		t.Errorf("initialize ran %d times, want 1", n)
	}
	if n := log.count("turn/start"); n != 0 {
		t.Errorf("turn/start was sent %d times, want 0", n)
	}
}

// C2: the normal path runs the four steps on one connection, in order, and returns with the turn
// the watch followed, so the run finishes the watch it admitted instead of leaving it holding the
// root. The thread/unsubscribe a finished watch asks for is not asserted here: the run closes its
// client on the way out, and that close cancels an in-flight release before it can be observed
// (internal/bridge/appserver/subscription.go, the release worker and Close). The release contract
// is the bridge's own test; the turn/completed notification below is what the watch would follow.
func TestResumeRunsTheFourStepsOnOneConnection(t *testing.T) {
	host := resumeHost(t, "idle")
	started := make(chan struct{})
	host.Handle("turn/start", func(json.RawMessage) fakehost.Reply {
		close(started)
		return fakehost.Reply{Result: map[string]any{"turn": map[string]any{"id": "turn-1"}},
			Before: []fakehost.Notification{{Method: "turn/completed", Params: map[string]any{
				"threadId": "01child", "turn": map[string]any{"id": "turn-1", "status": "completed"}}}}}
	})
	exe, _ := resumeRelayScript(t, resumeTestAssignment, resumeTestSettings, 0)
	e, _, _ := resumeEnv(t, exe)
	report, err := resumeRun(context.Background(), e, resumeConfig(host, "alpha"), resumeOptions{relationship: "rel-1", message: "m"})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	default:
		t.Fatal("turn/start never reached the host")
	}
	if report.TurnID != "turn-1" {
		t.Fatalf("report %+v", report)
	}
	if methods := resumeHostMethods(host); !slices.Equal(methods, []string{"thread/read", "thread/resume", "mcpServerStatus/list", "turn/start"}) {
		t.Fatalf("the host saw %q", methods)
	}
	if n := resumeHostConnections(host); n != 1 {
		t.Fatalf("the four steps used %d connections, want 1", n)
	}
}

// C3: a recorded runtimeWorkspaceRoots of [] is a value, not a missing setting: the resume
// proceeds and the thread/resume parameters carry the empty array rather than omitting it.
func TestResumeSendsAnEmptyRuntimeWorkspaceRoots(t *testing.T) {
	host := resumeHost(t, "idle")
	empty := `{"settings":{"sandbox":{"type":"readOnly","networkAccess":false},"approvalPolicy":"never","cwd":"/w","runtimeWorkspaceRoots":[],"model":"m","reasoningEffort":"xhigh"}}`
	exe, _ := resumeRelayScript(t, resumeTestAssignment, empty, 0)
	e, _, _ := resumeEnv(t, exe)
	report, err := resumeRun(context.Background(), e, resumeConfig(host, "alpha"), resumeOptions{relationship: "rel-1", message: "m"})
	if err != nil {
		t.Fatalf("an empty runtimeWorkspaceRoots was refused: %v", err)
	}
	if !report.OK || report.TurnID != "turn-1" {
		t.Fatalf("report %+v", report)
	}
	raw := string(resumeHostRequests(host)[1].Params)
	if !strings.Contains(raw, `"runtimeWorkspaceRoots":[]`) {
		t.Fatalf("the resume params did not carry an empty array: %s", raw)
	}
	var resume map[string]any
	if err := json.Unmarshal([]byte(raw), &resume); err != nil {
		t.Fatal(err)
	}
	roots, ok := resume["runtimeWorkspaceRoots"].([]any)
	if !ok || len(roots) != 0 {
		t.Fatalf("runtimeWorkspaceRoots = %#v, want an empty array", resume["runtimeWorkspaceRoots"])
	}
}

// C3 control: a runtimeWorkspaceRoots key that is absent or null supplied nothing and is still
// missing, so the resume is refused rather than sending a host default.
func TestResumeRefusesMissingOrNullRuntimeWorkspaceRoots(t *testing.T) {
	host := resumeHost(t, "idle")
	for name, document := range map[string]string{
		"absent": `{"settings":{"sandbox":{"type":"readOnly","networkAccess":false},"approvalPolicy":"never","cwd":"/w","model":"m","reasoningEffort":"xhigh"}}`,
		"null":   `{"settings":{"sandbox":{"type":"readOnly","networkAccess":false},"approvalPolicy":"never","cwd":"/w","runtimeWorkspaceRoots":null,"model":"m","reasoningEffort":"xhigh"}}`,
	} {
		t.Run(name, func(t *testing.T) {
			exe, _ := resumeRelayScript(t, resumeTestAssignment, document, 0)
			e, _, _ := resumeEnv(t, exe)
			_, err := resumeRun(context.Background(), e, resumeConfig(host, "alpha"), resumeOptions{relationship: "rel-1", message: "m"})
			failure, ok := err.(*resumeFailure)
			if !ok || failure.Reason != resumeSettingsUnavailable || !strings.Contains(failure.Detail, "runtimeWorkspaceRoots") {
				t.Fatalf("err = %v", err)
			}
			if n := host.Count("thread/read"); n != 0 {
				t.Errorf("the host was contacted %d times before the record was refused", n)
			}
		})
	}
}

// C4: the crw configuration file supplies the run: its child_check.disabled_servers names the
// servers to stop and its relay.socket names the App Server, and resumeRunCommand runs the four
// steps against the fake host from those values alone.
func TestResumeCommandRunsTheFourStepsFromTheConfigFile(t *testing.T) {
	host := resumeHost(t, "idle")
	exe, _ := resumeRelayScript(t, resumeTestAssignment, resumeTestSettings, 0)
	e, out, errOut := resumeEnv(t, exe)
	configHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", configHome)
	t.Setenv("CRW_CONFIG", "")
	document := coreConfigMarshal(t, map[string]any{
		"schema": "crw-config/1",
		"manage": map[string]any{
			"relay":       map[string]string{"socket": host.SocketPath},
			"child_check": map[string]any{"disabled_servers": []string{"alpha", "beta"}},
		},
	})
	coreConfigWrite(t, filepath.Join(configHome, "crw", "config.json"), document)
	messageFile := filepath.Join(t.TempDir(), "message.txt")
	if err := os.WriteFile(messageFile, []byte("go on"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code := resumeRunCommand(context.Background(), e, []string{"--relationship", "rel-1", "--message-file", messageFile}); code != 0 {
		t.Fatalf("exit %d: %s", code, errOut.String())
	}
	if methods := resumeHostMethods(host); !slices.Equal(methods, []string{"thread/read", "thread/resume", "mcpServerStatus/list", "turn/start"}) {
		t.Fatalf("the host saw %q", methods)
	}
	if n := resumeHostConnections(host); n != 1 {
		t.Fatalf("the four steps used %d connections, want 1", n)
	}
	if !strings.Contains(out.String(), `"turnId":"turn-1"`) {
		t.Fatalf("the command reported %q", out.String())
	}
}
