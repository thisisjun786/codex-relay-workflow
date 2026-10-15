package agent_test

import (
	"context"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver/fakehost"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/roles"
)

// CRW-1488 (1209 trial finding 12): a thread the flow starts, forks or resumes for a call gets the run's repository tools in its
// shell. They are sent as the leaf keys shell_environment_policy.set.<NAME>, which Codex 0.154 applies key by key over the server's
// own overrides (config overrides.rs apply_toml_override), never as a shell_environment_policy table, which would replace the
// policy the server was started with. The fake host holds such a server policy (inherit none, a set entry of its own) and builds
// each loaded thread's layer the way the host does.

func toolsFor1488() *engine.RepositoryTools {
	return &engine.RepositoryTools{
		Env:    []string{"PATH=/opt/go-1.27/bin:/usr/bin", "GOROOT=/opt/go-1.27", "GOTOOLCHAIN=local"},
		Prefix: `PATH=/opt/go-1.27/bin:"$PATH" GOROOT=/opt/go-1.27 GOTOOLCHAIN=local`,
		Facts:  []string{"go: /opt/go-1.27/bin/go (verifier.go_root /opt/go-1.27 in /cfg/config.json), the go crw.verify runs"},
	}
}

// serverPolicy is the shell policy the fake server is started with: inherit none and a set entry of its own.
func serverPolicy(e *env) {
	e.threads.ServerOverrides(
		fakehost.Override{Key: "shell_environment_policy.inherit", Value: "none"},
		fakehost.Override{Key: "shell_environment_policy.set.KEEP", Value: "x"},
	)
}

// withTools is the shell policy of a thread loaded with the tools over serverPolicy, and withoutTools the server's alone.
var (
	withTools = map[string]any{"inherit": "none", "set": map[string]any{"KEEP": "x", "PATH": "/opt/go-1.27/bin:/usr/bin", "GOROOT": "/opt/go-1.27", "GOTOOLCHAIN": "local"}}
	noTools   = map[string]any{"inherit": "none", "set": map[string]any{"KEEP": "x"}}
)

func shellPolicyOf(t *testing.T, e *env, id string) any {
	t.Helper()
	th, ok := e.threads.Thread(id)
	if !ok {
		t.Fatalf("no thread %s", id)
	}
	return th.Layer["shell_environment_policy"]
}

// A started thread keeps the server's policy and gets the tools; a thread started without tools keeps the server's policy only, and
// neither prompt changes.
func TestRepositoryToolsReachThreadShell(t *testing.T) {
	e := newEnv(t)
	serverPolicy(e)
	tools := toolsFor1488()
	spec := e.policy.Roles["explorer"]
	with, err := e.backend.StartThread(context.Background(), engine.ThreadSpec{CallID: "c_tools", Role: "explorer", Pair: spec.Pair, Sandbox: spec.Sandbox,
		Cwd: filepath.Join(e.root, "tools"), Tools: tools})
	if err != nil {
		t.Fatal(err)
	}
	plain, err := e.backend.StartThread(context.Background(), engine.ThreadSpec{CallID: "c_plain", Role: "explorer", Pair: spec.Pair, Sandbox: spec.Sandbox,
		Cwd: filepath.Join(e.root, "plain")})
	if err != nil {
		t.Fatal(err)
	}
	if got := shellPolicyOf(t, e, with.ID); !reflect.DeepEqual(got, withTools) {
		t.Errorf("the thread's shell policy is %v, want the server's with the tools %v", got, withTools)
	}
	if got := shellPolicyOf(t, e, plain.ID); !reflect.DeepEqual(got, noTools) {
		t.Errorf("a thread without tools has shell policy %v, want the server's %v", got, noTools)
	}
	starts := e.requests("thread/start")
	config, _ := starts[0].Params["config"].(map[string]any)
	if _, table := config["shell_environment_policy"]; table {
		t.Errorf("the request sends a shell_environment_policy table, which replaces the server's: %v", config)
	}
	if config["model_reasoning_effort"] != spec.Pair.Effort {
		t.Errorf("the tools replaced the rest of the config: %v", config)
	}
	prompt, err := roles.Prompt("explorer", roles.Options{})
	if err != nil {
		t.Fatal(err)
	}
	for i, s := range starts {
		if dev, _ := s.Params["developerInstructions"].(string); dev != prompt {
			t.Errorf("start %d: developerInstructions is not the role prompt", i)
		}
	}
}

// A continuation's fork gets the run's tools over the server's policy as a started thread does: the engine passes them on the
// ForkSpec (call.go) and the backend sends them with the thread/fork (fork.go).
func TestRepositoryToolsReachForkedThreadShell(t *testing.T) {
	e := newEnv(t, withOwner("proc-1"))
	serverPolicy(e)
	e.tools = toolsFor1488()
	e.useClock()
	e.run(forkBuildOnly)
	e.restart("proc-2")
	res := e.resumeRun(forkContinued)
	if got := value[[]string](t, res); !slices.Equal(got, []string{"completed", "completed"}) {
		t.Fatalf("%v", got)
	}
	forked := str(e.row("second", engine.EvForkStarted).Data, "thread_id")
	if forked == "" || len(e.requests("thread/fork")) != 1 {
		t.Fatalf("no fork: thread %q, %d thread/fork requests", forked, len(e.requests("thread/fork")))
	}
	if got := shellPolicyOf(t, e, forked); !reflect.DeepEqual(got, withTools) {
		t.Errorf("the forked thread's shell policy is %v, want %v", got, withTools)
	}
}

// After an App Server restart a thread the flow goes on with is loaded again by a thread/resume, which rebuilds its configuration
// from the host's: its shell has the server's policy only, and the backend keeps no tools to send with the resume. The turn the run
// sends there names the tools in its text: the go, the setting and the prefix. Here the first process is killed after it asked for
// the call's turn and before it sent it; the host restarts; the second process proves the turn absent and sends it on the cold thread.
func TestAColdResumedTurnNamesTheRepositoryTools(t *testing.T) {
	e := newEnv(t, withOwner("proc-1"))
	serverPolicy(e)
	tools := toolsFor1488()
	e.tools = tools
	e.useClock()
	e.killAfter(engine.EvTurnRequested)
	a := calls(e.run(buildScript))["a"]
	if n := e.count("turn/start"); n != 0 {
		t.Fatalf("%d turn/start requests reached the host before the kill", n)
	}
	thread := str(e.row("a", engine.EvThreadStarted).Data, "thread_id")
	if got := shellPolicyOf(t, e, thread); !reflect.DeepEqual(got, withTools) {
		t.Fatalf("the started thread's shell policy is %v, want %v", got, withTools)
	}
	e.threads.Restart()
	e.thaw()
	e.restart("proc-2")
	res := e.resumeRun(buildScript)
	e.expectCompletedOnce(res, "a", a.CallID+"/t1")
	resumes := e.requests("thread/resume")
	if len(resumes) == 0 {
		t.Fatal("the turn on the cold thread sent no thread/resume")
	}
	for _, r := range resumes {
		if _, has := r.Params["config"]; has {
			t.Errorf("a thread/resume carries config %v: the backend keeps no tools", r.Params["config"])
		}
	}
	if got := shellPolicyOf(t, e, thread); !reflect.DeepEqual(got, noTools) {
		t.Errorf("the resumed thread's shell policy is %v, want the server's alone %v", got, noTools)
	}
	starts := e.requests("turn/start")
	if len(starts) != 1 {
		t.Fatalf("%d turn/start requests, want 1", len(starts))
	}
	text := turnText(starts[0].Params)
	for _, want := range []string{"## Repository tools", tools.Facts[0], "`" + tools.Prefix + "`"} {
		if !strings.Contains(text, want) {
			t.Errorf("the cold-resumed turn does not name %q:\n%s", want, text)
		}
	}
}
