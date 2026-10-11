package manage

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
		stdout, code, _, err := e.relayHelperRun(context.Background(), argv...)
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
