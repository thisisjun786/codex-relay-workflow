package agent_test

import (
	"context"
	"path/filepath"
	"reflect"
	"slices"
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

// After an App Server restart a held call's thread is loaded again by the backend's thread/resume, which reloads the configuration:
// the resume carries the same leaf keys, so the thread still has the tools, and the server's policy stays.
func TestRepositoryToolsSurviveAColdResume(t *testing.T) {
	e := newEnv(t, withOwner("proc-1"))
	serverPolicy(e)
	tools := toolsFor1488()
	spec := e.policy.Roles["explorer"]
	th, err := e.backend.StartThread(context.Background(), engine.ThreadSpec{CallID: "c_held", Role: "explorer", Pair: spec.Pair, Sandbox: spec.Sandbox,
		Cwd: filepath.Join(e.root, "held"), Tools: tools})
	if err != nil {
		t.Fatal(err)
	}
	e.threads.Restart()
	if got := shellPolicyOf(t, e, th.ID); !reflect.DeepEqual(got, withTools) {
		t.Fatalf("before the resume: %v", got)
	}
	// the process that goes on with the held call has a backend of its own, which the engine gives the run's tools (engine.Run)
	e.restart("proc-2")
	e.backend.SetRepositoryTools(tools)
	op := "c_held/t2"
	if _, err := e.backend.RunTurn(context.Background(), engine.TurnSpec{CallID: "c_held", Op: op, K: 2, ThreadID: th.ID, Role: "explorer", Pair: spec.Pair,
		Input: "crw-flow-op: " + op + "\n\ngo on"}); err != nil {
		t.Fatal(err)
	}
	if n := len(e.requests("thread/resume")); n == 0 {
		t.Fatal("the turn after the restart sent no thread/resume")
	}
	if got := shellPolicyOf(t, e, th.ID); !reflect.DeepEqual(got, withTools) {
		t.Errorf("the resumed thread's shell policy is %v, want %v", got, withTools)
	}
}
