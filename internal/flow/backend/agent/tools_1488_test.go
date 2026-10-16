package agent_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

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
// from the host's and that request's config only. The backend resumes it with the call's run tools as leaf keys (CRW-1559), taken from
// the turn's spec and kept nowhere, so its shell has the server's policy plus the tools again, and the turn the run sends there still
// names them in its text: the go, the setting and the prefix. Here the first process is killed after it asked for the call's turn and
// before it sent it; the host restarts; the second process proves the turn absent and sends it on the cold thread.
func TestAColdResumedThreadGetsTheRepositoryTools(t *testing.T) {
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
		if got := r.Params["config"]; !jsonEqualAny(t, got, tools.ShellOverrides()) {
			t.Errorf("a thread/resume carries config %v, want the run's tools as leaf keys %v", got, tools.ShellOverrides())
		}
	}
	if got := shellPolicyOf(t, e, thread); !reflect.DeepEqual(got, withTools) {
		t.Errorf("the resumed thread's shell policy is %v, want the server's with the tools %v", got, withTools)
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

// otherTools1559 are another run's tools: another go.
func otherTools1559() *engine.RepositoryTools {
	return &engine.RepositoryTools{
		Env:    []string{"PATH=/opt/go-1.26/bin:/usr/bin", "GOROOT=/opt/go-1.26"},
		Prefix: `PATH=/opt/go-1.26/bin:"$PATH" GOROOT=/opt/go-1.26`,
		Facts:  []string{"go: /opt/go-1.26/bin/go (verifier.go_root /opt/go-1.26 in /cfg/config.json), the go crw.verify runs"},
	}
}

// resumeConfigs are the config of each thread/resume the host received for thread, in order (nil for one with none).
func resumeConfigs(e *env, thread string) []any {
	var out []any
	for _, r := range e.requests("thread/resume") {
		if str(r.Params, "threadId") == thread {
			out = append(out, r.Params["config"])
		}
	}
	return out
}

// hostRestart restarts the App Server under the backend's connection and waits until the client has seen its socket close, as it
// does when a real host goes away; the next request connects again.
func hostRestart(t *testing.T, e *env) {
	t.Helper()
	e.threads.Restart()
	deadline := time.Now().Add(10 * time.Second)
	for e.client.ConnectionID() != 0 {
		if time.Now().After(deadline) {
			t.Fatal("the client never saw the restarted host's socket close")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// CRW-1559: the backend keeps no run's tools. Three runs share one backend and one host: run A with tools, run B with other tools
// and run C with none. After an App Server restart each thread is loaded cold again by the hold of its call's next turn, and each
// resume carries only its own run's leaf keys (C's none), so each shell is the server's policy plus that run's tools. A cold
// AwaitTurn, as the first hold after a restart, carries its run's keys too, and the RunTurn after it does not resume again.
func TestTwoRunsColdResumeEachWithItsOwnTools(t *testing.T) {
	e := newEnv(t)
	serverPolicy(e)
	type run struct {
		name   string
		tools  *engine.RepositoryTools
		policy map[string]any
		thread engine.ThreadInfo
		last   engine.TurnOutcome
	}
	runs := []*run{
		{name: "a", tools: toolsFor1488(), policy: withTools},
		{name: "b", tools: otherTools1559(), policy: map[string]any{"inherit": "none", "set": map[string]any{"KEEP": "x", "PATH": "/opt/go-1.26/bin:/usr/bin", "GOROOT": "/opt/go-1.26"}}},
		{name: "c", tools: nil, policy: noTools},
	}
	role := "explorer"
	spec := e.policy.Roles[role]
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	turn := func(r *run, k int) {
		t.Helper()
		call := "c_" + r.name
		op := fmt.Sprintf("%s/t%d", call, k)
		out, err := e.backend.RunTurn(ctx, engine.TurnSpec{CallID: call, Op: op, K: k, ThreadID: r.thread.ID, Role: role, Pair: spec.Pair,
			Input: "crw-flow-op: " + op + "\n\nwork", Tools: r.tools})
		if err != nil || out.Failed != nil {
			t.Fatalf("run %s turn %d: %v %+v", r.name, k, err, out.Failed)
		}
		r.last = out
	}
	for _, r := range runs {
		cwd := filepath.Join(e.root, "run-"+r.name)
		if err := os.MkdirAll(cwd, 0o755); err != nil {
			t.Fatal(err)
		}
		info, err := e.backend.StartThread(ctx, engine.ThreadSpec{CallID: "c_" + r.name, Role: role, Pair: spec.Pair, Sandbox: spec.Sandbox, Cwd: cwd, Tools: r.tools})
		if err != nil {
			t.Fatal(err)
		}
		r.thread = info
		turn(r, 1)
	}
	hostRestart(t, e)
	for _, r := range runs {
		turn(r, 2)
	}
	for _, r := range runs {
		configs := resumeConfigs(e, r.thread.ID)
		if len(configs) != 1 {
			t.Fatalf("run %s: %d thread/resume requests after the restart, want 1: %v", r.name, len(configs), configs)
		}
		want := r.tools.ShellOverrides()
		if want == nil {
			if configs[0] != nil {
				t.Errorf("run %s has no tools but its thread/resume carries config %v", r.name, configs[0])
			}
		} else if !jsonEqualAny(t, configs[0], want) {
			t.Errorf("run %s: its thread/resume carries config %v, want only its own leaf keys %v", r.name, configs[0], want)
		}
		if got := shellPolicyOf(t, e, r.thread.ID); !reflect.DeepEqual(got, r.policy) {
			t.Errorf("run %s: the resumed thread's shell policy is %v, want %v", r.name, got, r.policy)
		}
	}

	// A cold AwaitTurn as the first hold: run A follows its last turn after a second restart, then runs another turn.
	a := runs[0]
	hostRestart(t, e)
	if _, err := e.backend.AwaitTurn(ctx, a.thread.ID, a.last.TurnID, "c_a/t2", a.tools); err != nil {
		t.Fatal(err)
	}
	configs := resumeConfigs(e, a.thread.ID)
	if len(configs) != 2 || !jsonEqualAny(t, configs[1], a.tools.ShellOverrides()) {
		t.Fatalf("the cold AwaitTurn's thread/resume configs are %v, want a second one with run a's leaf keys", configs)
	}
	if got := shellPolicyOf(t, e, a.thread.ID); !reflect.DeepEqual(got, a.policy) {
		t.Errorf("after the cold AwaitTurn the thread's shell policy is %v, want %v", got, a.policy)
	}
	turn(a, 3)
	if configs := resumeConfigs(e, a.thread.ID); len(configs) != 2 {
		t.Errorf("the RunTurn after the AwaitTurn resumed the thread again: %d thread/resume requests, want 2", len(configs))
	}
	if got := shellPolicyOf(t, e, a.thread.ID); !reflect.DeepEqual(got, a.policy) {
		t.Errorf("after the next turn the thread's shell policy is %v, want %v", got, a.policy)
	}
}
