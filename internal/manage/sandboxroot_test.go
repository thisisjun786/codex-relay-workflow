package manage

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/execution"
	"github.com/thisisjun786/codex-relay-workflow/internal/crwconfig"
	"github.com/thisisjun786/codex-relay-workflow/internal/sandboxroot"
	"github.com/thisisjun786/codex-relay-workflow/internal/sandboxroot/sandboxroottest"
)

// CRW-1265. child-resume resumes the child at the cwd its record carries, so a child recorded in the
// account home before the rule existed is not run there again: the run is refused before the App
// Server is contacted, the dry run included.
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
				host := resumeHost(t, "idle")
				exe, _ := resumeRelayScript(t, resumeTestAssignment, c.settings, 0)
				e, _, _ := resumeEnv(t, exe)
				_, err := resumeRun(context.Background(), e, resumeConfig(host, "alpha", "beta"), resumeOptions{relationship: "rel-1", message: "go", dryRun: dry})
				var failure *resumeFailure
				if !errors.As(err, &failure) || failure.Reason != sandboxroot.Code || !strings.Contains(failure.Detail, c.rule) || resumeExit(failure) != usageExit {
					t.Fatalf("dry=%v: want a %s refusal, got %v", dry, c.rule, err)
				}
				if methods := resumeHostMethods(host); len(methods) != 0 || resumeHostConnections(host) != 0 {
					t.Fatalf("dry=%v: the App Server was contacted: %v", dry, methods)
				}
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
