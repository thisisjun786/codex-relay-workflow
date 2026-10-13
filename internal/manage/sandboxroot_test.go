package manage

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver/fakehost"
	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/execution"
	"github.com/thisisjun786/codex-relay-workflow/internal/crwconfig"
	"github.com/thisisjun786/codex-relay-workflow/internal/sandboxroot"
	"github.com/thisisjun786/codex-relay-workflow/internal/sandboxroot/sandboxroottest"
)

// CRW-1265. child-resume resumes the child at the cwd its record carries. CRW-1265 refused a child
// recorded in the account home before the App Server was contacted; since CRW-1308 the rule's verdict
// is a recorded warning and the run goes on, the dry run included.
func TestChildResumeRefusesRecordedHomeCwd(t *testing.T) {
	w := sandboxroottest.Fake(t)
	run := w.Mkdir(t, w.Sandbox, "run")
	record := func(cwd string, roots string, sandbox string) string {
		return `{"task":"01child","usable":true,"settings":{"sandbox":` + sandbox + `,"approvalPolicy":"never","cwd":"` + cwd + `","runtimeWorkspaceRoots":` + roots + `,"model":"m","reasoningEffort":"xhigh"}}`
	}
	readOnly := `{"type":"readOnly","networkAccess":false}`
	for _, c := range []struct {
		name, settings, rule string
	}{
		{"the recorded cwd is the account home", record(w.Home, `["`+w.Home+`"]`, readOnly), sandboxroot.RuleAccountHome},
		{"the recorded cwd is above the home", record(w.Root+"/home", `["`+run+`"]`, readOnly), sandboxroot.RuleHomeAncestor},
		{"a recorded root is the home", record(run, `["`+run+`","`+w.Home+`"]`, readOnly), sandboxroot.RuleAccountHome},
		{"the policy's writable root is the home", record(run, `["`+run+`"]`, `{"type":"workspaceWrite","writableRoots":["`+w.Home+`"],"networkAccess":false}`), sandboxroot.RuleAccountHome},
	} {
		for _, dry := range []bool{false, true} {
			t.Run(c.name, func(t *testing.T) {
				w.ClearWarnings(t)
				host := resumeHost(t, "idle")
				exe, _ := resumeRelayScript(t, resumeTestAssignment, c.settings, 0)
				e, _, _ := resumeEnv(t, exe)
				_, err := resumeRun(context.Background(), e, resumeConfig(host, "alpha", "beta"), resumeOptions{relationship: "rel-1", message: "go", dryRun: dry})
				var failure *resumeFailure
				if errors.As(err, &failure) && failure.Reason == sandboxroot.Code {
					t.Fatalf("dry=%v: the rule refused: %v", dry, err)
				}
				if methods := resumeHostMethods(host); len(methods) == 0 || methods[0] != "thread/read" || resumeHostConnections(host) == 0 {
					t.Fatalf("dry=%v: the App Server was not contacted: %v (err %v)", dry, methods, err)
				}
				w.Warned(t, "manage/child-resume", c.rule, "")
			})
		}
	}
	// A child recorded without a sandbox the rule applies to is run as before.
	host := resumeHost(t, "idle")
	exe, _ := resumeRelayScript(t, resumeTestAssignment, record(w.Home, `["`+w.Home+`"]`, `{"type":"dangerFullAccess"}`), 0)
	e, _, _ := resumeEnv(t, exe)
	if _, err := resumeRun(context.Background(), e, resumeConfig(host, "alpha", "beta"), resumeOptions{relationship: "rel-1", message: "go"}); err != nil {
		t.Fatalf("a danger-full-access child: %v", err)
	}
	if host.Count("thread/resume") != 1 {
		t.Fatalf("the resume was not sent: %v", resumeHostMethods(host))
	}
}

// CRW-1265. A relay command the manage command runs does not take the cwd of whoever started manage
// (often the account home): it runs in the state directory its --state names. The doctor line, which
// names none, and a state directory that does not exist keep the caller's cwd.
func TestRelayHelperRunsInTheStateDirectory(t *testing.T) {
	start := t.TempDir()
	state := t.TempDir()
	script := filepath.Join(t.TempDir(), "crw")
	if err := os.WriteFile(script, []byte("#!/bin/sh\npwd -P\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Chdir(start)
	e := &Env{Executable: script}
	run := func(argv ...string) string {
		stdout, code, _, err := e.relayHelperRun(context.Background(), nil, argv...)
		if err != nil || code != 0 {
			t.Fatalf("%v: code %d err %v", argv, code, err)
		}
		return strings.TrimSpace(string(stdout))
	}
	wantState, _ := filepath.EvalSymlinks(state)
	wantStart, _ := filepath.EvalSymlinks(start)
	if got := run("relay", "--state", state, "--socket", "s", "service", "status"); got != wantState {
		t.Fatalf("a --state line ran in %s, want %s", got, wantState)
	}
	if got := run("relay", "--socket", "s", "doctor"); got != wantStart {
		t.Fatalf("the doctor line ran in %s, want the caller's %s", got, wantStart)
	}
	if got := run("relay", "--state", filepath.Join(state, "absent"), "--socket", "s", "x"); got != wantStart {
		t.Fatalf("a missing state directory ran in %s, want the caller's %s", got, wantStart)
	}
}

// CRW-1265. The command runs in the state directory, so the paths the caller named relative to its own
// directory reach it absolute and keep naming the same files.
func TestRelayHelperKeepsRelativePathsOfTheCaller(t *testing.T) {
	start := t.TempDir()
	state := t.TempDir()
	script := filepath.Join(t.TempDir(), "crw")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nfor a in \"$@\"; do echo \"$a\"; done\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Chdir(start)
	e := &Env{Executable: script}
	stdout, code, _, err := e.relayHelperRun(context.Background(), nil, "relay", "--state", state, "--socket", "./app.sock", "settings-record", "--cwd", "work", "--settings", "@s.json", "--task", "t")
	if err != nil || code != 0 {
		t.Fatalf("code %d err %v", code, err)
	}
	want, _ := os.Getwd()
	got := strings.Fields(string(stdout))
	expect := []string{"relay", "--state", state, "--socket", want + "/./app.sock", "settings-record", "--cwd", want + "/work", "--settings", "@" + want + "/s.json", "--task", "t"}
	if strings.Join(got, "\n") != strings.Join(expect, "\n") {
		t.Fatalf("argv %q, want %q", got, expect)
	}
	// The line that stays in the caller's directory is passed as it was.
	stdout, _, _, _ = e.relayHelperRun(context.Background(), nil, "relay", "--socket", "./app.sock", "doctor")
	if strings.Join(strings.Fields(string(stdout)), " ") != "relay --socket ./app.sock doctor" {
		t.Fatalf("the doctor line was rewritten: %q", stdout)
	}
}

// CRW-1265. The relay command runs in the state directory, so an execution policy (or an isolated scope
// directory) the environment names relative to the caller's directory reaches it as a path that names the
// same file, a link followed by ".." included; a policy the relay reads from the state directory instead
// would be another file or none. Absolute values, a scope under "~" and the line that stays in the
// caller's directory keep the value as it was.
func TestRelayHelperKeepsTheCallersRelativePolicyEnvironment(t *testing.T) {
	root := t.TempDir()
	start, sub, state := filepath.Join(root, "start"), filepath.Join(root, "other", "sub"), filepath.Join(root, "state")
	for _, dir := range []string{start, sub, state} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(sub, filepath.Join(start, "link")); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{filepath.Join(root, "other", "p.json"), filepath.Join(start, "p.json")} {
		if err := os.WriteFile(f, []byte(f), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	script := filepath.Join(t.TempDir(), "crw")
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho \"$CODEX_THREAD_BRIDGE_EXECUTION_POLICY\"\necho \"$CODEX_SESSION_RELAY_SCOPE_DIR\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Chdir(start)
	e := &Env{Executable: script}
	run := func(argv ...string) []string {
		stdout, code, _, err := e.relayHelperRun(context.Background(), nil, argv...)
		if err != nil || code != 0 {
			t.Fatalf("%v: code %d err %v", argv, code, err)
		}
		return strings.Split(strings.TrimSuffix(string(stdout), "\n"), "\n")
	}
	for _, policy := range []string{"p.json", " link/../p.json ", "./link/../p.json"} {
		meant, err := os.Stat(strings.TrimSpace(policy))
		if err != nil {
			t.Fatal(err)
		}
		t.Setenv(execution.EnvPolicy, policy)
		t.Setenv("CODEX_SESSION_RELAY_SCOPE_DIR", "scopes")
		got := run("relay", "--state", state, "--socket", "s", "service", "status")
		if !filepath.IsAbs(got[0]) {
			t.Fatalf("policy %q reached the relay command as %q", policy, got[0])
		}
		if named, err := os.Stat(got[0]); err != nil || !os.SameFile(meant, named) {
			t.Fatalf("policy %q reached the relay command as %q, another file", policy, got[0])
		}
		if wd, _ := os.Getwd(); got[1] != wd+"/scopes" {
			t.Fatalf("the scope directory reached the relay command as %q", got[1])
		}
	}
	for _, kept := range []string{"/abs/p.json", ""} {
		t.Setenv(execution.EnvPolicy, kept)
		t.Setenv("CODEX_SESSION_RELAY_SCOPE_DIR", "~/scopes")
		if got := run("relay", "--state", state, "--socket", "s", "service", "status"); got[0] != kept || got[1] != "~/scopes" {
			t.Fatalf("kept values were rewritten: %q", got)
		}
	}
	t.Setenv(execution.EnvPolicy, "p.json")
	t.Setenv("CODEX_SESSION_RELAY_SCOPE_DIR", "scopes")
	if got := run("relay", "--socket", "s", "doctor"); got[0] != "p.json" || got[1] != "scopes" {
		t.Fatalf("the doctor line's environment was rewritten: %q", got)
	}
}

// CRW-1265 with CRW-958. A relay command that runs in the state directory still gets the configuration
// file this tool was started with (CRW_CONFIG) and the caller's relative execution policy made absolute:
// the two environment changes compose on one environment, neither drops the other.
func TestRelayHelperInStateDirectoryKeepsConfigAndPolicyEnvironment(t *testing.T) {
	start := t.TempDir()
	state := t.TempDir()
	if err := os.WriteFile(filepath.Join(start, "p.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(t.TempDir(), "crw")
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho \"$CRW_CONFIG\"\necho \"$CODEX_THREAD_BRIDGE_EXECUTION_POLICY\"\npwd -P\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Chdir(start)
	t.Setenv("CODEX_THREAD_BRIDGE_EXECUTION_POLICY", "p.json")
	cfg := &Config{source: crwconfig.SourceConfig, path: filepath.Join(start, "c.json")}
	e := &Env{Executable: script}
	stdout, code, _, err := e.relayHelperRun(context.Background(), cfg, "relay", "--state", state, "--socket", "s", "service", "status")
	if err != nil || code != 0 {
		t.Fatalf("code %d err %v", code, err)
	}
	wd, _ := os.Getwd()
	wantState, _ := filepath.EvalSymlinks(state)
	got := strings.Split(strings.TrimSuffix(string(stdout), "\n"), "\n")
	want := []string{filepath.Join(start, "c.json"), filepath.Join(wd, "p.json"), wantState}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("child saw %q, want %q", got, want)
	}
}

// CRW-1265 (pre-merge evaluation of f0691fdd, d3). The record is safe, but the loaded child runs where a
// sandboxed Codex may not (the account home as its cwd, a home writable root kept, a sandbox policy or a
// config root that names the home): the settings thread/read reports, and the ones thread/resume answers
// with, are judged too. CRW-1265 refused at either answer; since CRW-1308 each verdict is a recorded warning
// and the run goes on.
func TestChildResumeRefusesLiveHomeSettings(t *testing.T) {
	w := sandboxroottest.Fake(t)
	run := w.Mkdir(t, w.Sandbox, "run")
	settings := `{"task":"01child","usable":true,"settings":{"sandbox":{"type":"workspaceWrite","writableRoots":["` + run + `"],"networkAccess":false},"approvalPolicy":"never","cwd":"` + run + `","runtimeWorkspaceRoots":["` + run + `"],"model":"m","reasoningEffort":"xhigh"}}`
	thread := func(extra map[string]any) map[string]any {
		out := map[string]any{"model": "m", "reasoningEffort": "xhigh", "status": map[string]any{"type": "idle"}}
		for k, v := range extra {
			out[k] = v
		}
		return out
	}
	home := []any{w.Home}
	for _, c := range []struct {
		name         string
		read, resume map[string]any // the extra members of the thread/read thread and of the thread/resume result
		methods      []string       // the App Server methods that ran up to the verdict, which no longer stops the run
		rule         string
		caller       string
	}{
		{"the thread runs in the account home", map[string]any{"cwd": w.Home, "sandbox": map[string]any{"type": "workspaceWrite"}}, nil, []string{"thread/read"}, sandboxroot.RuleAccountHome, "manage/child-resume:thread/read"},
		{"the thread keeps a home writable root", map[string]any{"cwd": run, "sandbox": map[string]any{"type": "workspaceWrite", "writableRoots": home}}, nil, []string{"thread/read"}, sandboxroot.RuleAccountHome, "manage/child-resume:thread/read"},
		{"the thread reports a home runtime root and no sandbox", map[string]any{"cwd": run, "runtimeWorkspaceRoots": home}, nil, []string{"thread/read"}, sandboxroot.RuleAccountHome, "manage/child-resume:thread/read"},
		{"the resume answers with a home writable root", nil, map[string]any{"cwd": run, "sandbox": map[string]any{"type": "workspaceWrite", "writableRoots": home}}, []string{"thread/read", "thread/resume"}, sandboxroot.RuleAccountHome, "manage/child-resume:thread/resume"},
		{"the resume answers with the home as its cwd", nil, map[string]any{"cwd": w.Home, "sandbox": map[string]any{"type": "workspaceWrite"}}, []string{"thread/read", "thread/resume"}, sandboxroot.RuleAccountHome, "manage/child-resume:thread/resume"},
	} {
		for _, dry := range []bool{false, true} {
			if dry && c.resume != nil {
				continue // the dry run stops after thread/read
			}
			t.Run(c.name, func(t *testing.T) {
				w.ClearWarnings(t)
				host := resumeHost(t, "idle")
				host.Respond("thread/read", fakehost.Reply{Result: map[string]any{"thread": thread(c.read)}})
				answer := map[string]any{"model": "m", "reasoningEffort": "xhigh"}
				for k, v := range c.resume {
					answer[k] = v
				}
				host.Respond("thread/resume", fakehost.Reply{Result: answer})
				exe, _ := resumeRelayScript(t, resumeTestAssignment, settings, 0)
				e, _, _ := resumeEnv(t, exe)
				_, err := resumeRun(context.Background(), e, resumeConfig(host, "alpha", "beta"), resumeOptions{relationship: "rel-1", message: "go", dryRun: dry})
				var failure *resumeFailure
				if errors.As(err, &failure) && failure.Reason == sandboxroot.Code {
					t.Fatalf("dry=%v: the rule refused: %v", dry, err)
				}
				methods := resumeHostMethods(host)
				if len(methods) < len(c.methods) || strings.Join(methods[:len(c.methods)], ",") != strings.Join(c.methods, ",") {
					t.Fatalf("dry=%v: the App Server ran %v, want %v first (err %v)", dry, methods, c.methods, err)
				}
				w.Warned(t, c.caller, c.rule, "")
			})
		}
	}
	// A thread that runs in a dedicated root with safe roots is resumed and its turn started as before.
	host := resumeHost(t, "idle")
	safe := map[string]any{"cwd": run, "sandbox": map[string]any{"type": "workspaceWrite", "writableRoots": []any{run}}, "runtimeWorkspaceRoots": []any{run}}
	host.Respond("thread/read", fakehost.Reply{Result: map[string]any{"thread": thread(safe)}})
	host.Respond("thread/resume", fakehost.Reply{Result: map[string]any{"model": "m", "reasoningEffort": "xhigh", "cwd": run, "sandbox": safe["sandbox"], "runtimeWorkspaceRoots": []any{run}}})
	exe, _ := resumeRelayScript(t, resumeTestAssignment, settings, 0)
	e, _, _ := resumeEnv(t, exe)
	if _, err := resumeRun(context.Background(), e, resumeConfig(host, "alpha", "beta"), resumeOptions{relationship: "rel-1", message: "go"}); err != nil || host.Count("turn/start") != 1 {
		t.Fatalf("a safe thread: %v, calls %v", err, resumeHostMethods(host))
	}
}
