package agent_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver"
	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver/fakehost"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/roles"
	"github.com/thisisjun786/codex-relay-workflow/internal/tmploc"
)

func q(s string) string { return fmt.Sprintf("%q", s) }

func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return line
}

// inputText is the text of the first input element of a turn/start request.
func inputText(t *testing.T, r request) string {
	t.Helper()
	input, _ := r.Params["input"].([]any)
	if len(input) == 0 {
		t.Fatalf("turn/start without input: %v", r.Params)
	}
	return str(input[0].(map[string]any), "text")
}

func jsonEqual(t *testing.T, got any, want string) bool {
	t.Helper()
	var w any
	if err := json.Unmarshal([]byte(want), &w); err != nil {
		t.Fatalf("%s: %v", want, err)
	}
	return reflect.DeepEqual(got, w)
}

// An executor and a reviewer call start persistent threads with their role's pair, sandbox and prompt, read the pair back
// with thread/read, and run every turn on that pair with the call's schema and operation marker.
func TestAgentStartsWithRolePair(t *testing.T) {
	e := newEnv(t)
	res := e.run(`const S = ` + verdictSchema + `
const b = await agent.call('build it', { agentType: 'executor', node: 'n1', schema: S, label: 'build' })
const r = await agent.call('review it', { agentType: 'ocx-gpt-6-1-sol', node: 'n1', head: ` + q(e.base) + `, schema: S, label: 'review' })
return [b.status, r.status]`)
	if got := value[[]string](t, res); !slices.Equal(got, []string{"completed", "completed"}) {
		t.Fatalf("%v", got)
	}
	by := calls(res)
	want := map[string]struct {
		role, model, effort, sandbox string
	}{
		"build":  {"executor", "claude-fake-haiku", "xhigh", "workspace-write"},
		"review": {"reviewer", "gpt-fake-sol", "high", "read-only"},
	}
	starts := e.requests("thread/start")
	if len(starts) != 2 {
		t.Fatalf("%d thread/start requests, want 2", len(starts))
	}
	for i, label := range []string{"build", "review"} {
		w, call, start := want[label], by[label], starts[i].Params
		if call.Meta.Role != w.role || call.Meta.ConfiguredPair != (engine.Pair{Model: w.model, Effort: w.effort}) {
			t.Errorf("%s: meta %+v", label, call.Meta)
		}
		if start["model"] != w.model || start["sandbox"] != w.sandbox || start["approvalPolicy"] != "never" {
			t.Errorf("%s: thread/start %v", label, start)
		}
		if config, _ := start["config"].(map[string]any); config["model_reasoning_effort"] != w.effort {
			t.Errorf("%s: config %v", label, start["config"])
		}
		if eph, present := start["ephemeral"]; !present || eph != false {
			t.Errorf("%s: ephemeral = %v (present %v): an engine thread is persistent", label, eph, present)
		}
		prompt, err := roles.Prompt(w.role, roles.Options{})
		if err != nil {
			t.Fatal(err)
		}
		dev, _ := start["developerInstructions"].(string)
		if dev != prompt {
			t.Errorf("%s: developerInstructions is not the role prompt", label)
		}
		for _, identity := range []string{call.CallID, "crw-flow-op", e.runID} {
			if strings.Contains(dev, identity) {
				t.Errorf("%s: developerInstructions carry the identity %q", label, identity)
			}
		}
	}
	reads := e.requests("thread/read")
	if len(reads) != 2 {
		t.Fatalf("%d thread/read requests, want one readback per thread", len(reads))
	}
	for _, r := range reads {
		if r.Params["includeTurns"] != false {
			t.Errorf("thread/read %v: includeTurns must be false", r.Params)
		}
	}
	turns := e.requests("turn/start")
	if len(turns) != 2 {
		t.Fatalf("%d turn/start requests", len(turns))
	}
	for i, label := range []string{"build", "review"} {
		w, call, turn := want[label], by[label], turns[i]
		op := call.CallID + "/t1"
		if turn.Params["model"] != w.model || turn.Params["effort"] != w.effort {
			t.Errorf("%s: turn pair %v/%v", label, turn.Params["model"], turn.Params["effort"])
		}
		if turn.Params["clientUserMessageId"] != op || firstLine(inputText(t, turn)) != "crw-flow-op: "+op {
			t.Errorf("%s: marker %v / %q, want %s", label, turn.Params["clientUserMessageId"], firstLine(inputText(t, turn)), op)
		}
		if !jsonEqual(t, turn.Params["outputSchema"], `{"type":"object","properties":{"verdict":{"type":"string"}},"required":["verdict"],"additionalProperties":false}`) {
			t.Errorf("%s: outputSchema %v", label, turn.Params["outputSchema"])
		}
	}
	// the pair the host reports back is what the call records as configured
	if row := e.row("build", engine.EvThreadStarted); str(row.Data, "model") != "claude-fake-haiku" || str(row.Data, "effort") != "xhigh" {
		t.Errorf("thread_started %v", row.Data)
	}
}

// A thread that reads back another pair than its role's, or a null one, is refused (pair_refused) and recorded; no turn
// is sent to it.
func TestPairReadbackMismatchRefused(t *testing.T) {
	for name, readback := range map[string]func(fakehost.ThreadView) (*string, *string){
		"another effort": func(v fakehost.ThreadView) (*string, *string) { low := "low"; return &v.Model, &low },
		"another model":  func(v fakehost.ThreadView) (*string, *string) { m := "some-other-model"; return &m, &v.Effort },
		"null model":     func(v fakehost.ThreadView) (*string, *string) { return nil, &v.Effort },
		"null effort":    func(v fakehost.ThreadView) (*string, *string) { return &v.Model, nil },
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			e.threads.Readback(readback)
			res := e.run(`const r = await agent.call('x', { agentType: 'explorer', label: 'x' }); return [r.status, r.reason, r.handle !== null]`)
			if got := value[[]any](t, res); fmt.Sprint(got) != "[refused pair_refused true]" {
				t.Fatalf("%v", got)
			}
			if n := len(e.requests("turn/start")); n != 0 {
				t.Errorf("%d turns were sent to a thread with the wrong pair", n)
			}
			started := e.row("x", engine.EvThreadStarted)
			if started.Data["thread_id"] == "" {
				t.Errorf("the refusal must keep the thread it names: %v", started.Data)
			}
			if got := kinds(e.rows("x")); !slices.Equal(got, []string{"prepared", "thread_requested", "thread_started", "refused"}) {
				t.Errorf("rows %v", got)
			}
			refused := e.row("x", engine.EvRefused)
			if refused.Data["reason"] != "pair_refused" || refused.Data["detail"] == nil {
				t.Errorf("refusal record %v", refused.Data)
			}
		})
	}
}

// A result that does not satisfy the schema gets one repair turn on the same thread with its own marker; a second miss is
// null_model and no third turn is sent.
func TestOutputSchemaRepairOnce(t *testing.T) {
	t.Run("repaired", func(t *testing.T) {
		e := newEnv(t)
		e.plan(func(r fakehost.TurnRequest) fakehost.TurnPlan {
			if r.N == 1 {
				return fakehost.TurnPlan{Message: `{"nope":1}`}
			}
			return fakehost.TurnPlan{Message: passMessage}
		})
		res := e.run(`const S = ` + verdictSchema + `
const r = await agent.call('x', { agentType: 'explorer', schema: S, label: 'x' })
return [r.status, r.output, r.meta.repairUsed]`)
		if got := value[[]any](t, res); fmt.Sprint(got) != "[completed map[verdict:pass] true]" {
			t.Fatalf("%v", got)
		}
		turns := e.requests("turn/start")
		if len(turns) != 2 || len(e.requests("thread/start")) != 1 {
			t.Fatalf("%d turns, %d threads", len(turns), len(e.requests("thread/start")))
		}
		if turns[0].Params["threadId"] != turns[1].Params["threadId"] {
			t.Errorf("the repair turn must run on the same thread")
		}
		id := calls(res)["x"].CallID
		for k, turn := range turns {
			op := fmt.Sprintf("%s/t%d", id, k+1)
			if turn.Params["clientUserMessageId"] != op || firstLine(inputText(t, turn)) != "crw-flow-op: "+op {
				t.Errorf("turn %d marker %v", k+1, turn.Params["clientUserMessageId"])
			}
			if turn.Params["model"] != "claude-fake-haiku" || turn.Params["effort"] != "high" {
				t.Errorf("turn %d pair %v/%v", k+1, turn.Params["model"], turn.Params["effort"])
			}
		}
	})
	t.Run("still invalid", func(t *testing.T) {
		e := newEnv(t)
		e.plan(func(fakehost.TurnRequest) fakehost.TurnPlan { return fakehost.TurnPlan{Message: `{"verdict":3}`} })
		res := e.run(`const S = ` + verdictSchema + `
const r = await agent.call('x', { agentType: 'explorer', schema: S, label: 'x' })
return [r.status, r.reason, r.output]`)
		if got := value[[]any](t, res); fmt.Sprint(got) != "[null_model schema_invalid_after_repair <nil>]" {
			t.Fatalf("%v", got)
		}
		if n := len(e.requests("turn/start")); n != 2 {
			t.Errorf("%d turns were sent, want the first and one repair", n)
		}
	})
}

// A continuation runs on the pair its thread has: a differing effort or model is refused before any request, and the
// turns it sends repeat the thread's pair.
func TestNoEffortChangeOnResume(t *testing.T) {
	e := newEnv(t)
	res := e.run(`const a = await agent.call('one', { agentType: 'executor', node: 'n1', label: 'a' })
const bad = await agent.call('two', { resume: a, effort: 'low', label: 'bad' })
const wrongModel = await agent.call('two', { resume: a, model: 'claude-other', label: 'wrong' })
const ok = await agent.call('three', { resume: a, label: 'ok' })
return [a.status, bad.status, bad.reason, wrongModel.reason, ok.status, ok.handle !== null]`)
	if got := value[[]any](t, res); fmt.Sprint(got) != "[completed refused pair_refused pair_refused completed true]" {
		t.Fatalf("%v", got)
	}
	if n := len(e.requests("thread/start")); n != 1 {
		t.Errorf("%d threads were started; a continuation runs on its own thread", n)
	}
	turns := e.requests("turn/start")
	if len(turns) != 2 {
		t.Fatalf("%d turns: a refused continuation must send nothing", len(turns))
	}
	for i, turn := range turns {
		if turn.Params["model"] != "claude-fake-haiku" || turn.Params["effort"] != "xhigh" {
			t.Errorf("turn %d pair %v/%v", i+1, turn.Params["model"], turn.Params["effort"])
		}
		if turn.Params["threadId"] != turns[0].Params["threadId"] {
			t.Errorf("turn %d is on another thread", i+1)
		}
	}
	// a fresh call that asks for another effort runs on its role's pair; the request is only recorded
	e2 := newEnv(t)
	res = e2.run(`const r = await agent.call('x', { agentType: 'explorer', effort: 'low', label: 'x' }); return [r.status, r.meta.requestedPair.effort, r.meta.configuredPair.effort]`)
	if got := value[[]string](t, res); !slices.Equal(got, []string{"completed", "low", "high"}) {
		t.Errorf("%v", got)
	}
	if turn := e2.requests("turn/start")[0]; turn.Params["effort"] != "high" {
		t.Errorf("a requested effort must not reach the host: %v", turn.Params["effort"])
	}
}

// A repository whose worktrees an executor and a reviewer use.
func registered(t *testing.T, repo string) []string {
	t.Helper()
	var paths []string
	for _, line := range strings.Split(git(t, repo, "worktree", "list", "--porcelain"), "\n") {
		if p, ok := strings.CutPrefix(line, "worktree "); ok {
			real, err := filepath.EvalSymlinks(p)
			if err != nil {
				real = p
			}
			paths = append(paths, real)
		}
	}
	return paths
}

func realPath(t *testing.T, p string) string {
	t.Helper()
	real, err := filepath.EvalSymlinks(p)
	if err != nil {
		t.Fatal(err)
	}
	return real
}

// An executor's thread runs in the attempt worktree of its node, a detached worktree of the repository at the node's
// start commit, and the call records the allocation.
func TestExecutorCwdIsIssueWorktree(t *testing.T) {
	e := newEnv(t)
	res := e.run(`const r = await agent.call('build', { agentType: 'executor', node: 'issue-7', label: 'build' }); return r.status`)
	if got := value[string](t, res); got != "completed" {
		t.Fatalf("%s", got)
	}
	want := filepath.Join(e.state, "flow", e.runID, "wt", "issue-7", "a1-l0")
	start := e.requests("thread/start")[0]
	if start.Params["cwd"] != want {
		t.Fatalf("cwd = %v, want %s", start.Params["cwd"], want)
	}
	if head := git(t, want, "rev-parse", "HEAD"); head != e.base {
		t.Errorf("worktree HEAD %s, want the base %s", head, e.base)
	}
	if !gitFails(want, "symbolic-ref", "-q", "HEAD") {
		t.Errorf("the worktree HEAD must be detached")
	}
	if !slices.Contains(registered(t, e.repo), realPath(t, want)) {
		t.Errorf("%s is not a worktree of the repository: %v", want, registered(t, e.repo))
	}
	alloc, _ := e.row("build", engine.EvPrepared).Data["allocation"].(map[string]any)
	if alloc["kind"] != "fresh" || alloc["path"] != want || alloc["start_commit"] != e.base || fmt.Sprint(alloc["attempt"]) != "1" {
		t.Errorf("prepared allocation %v", alloc)
	}
	if got := e.row("build", engine.EvThreadStarted).Data["cwd"]; got != want {
		t.Errorf("thread_started cwd %v: the host's Thread.cwd is what the call records", got)
	}
	// the turns do not name it again
	if _, has := e.requests("turn/start")[0].Params["cwd"]; has {
		t.Errorf("an executor turn must not carry a cwd")
	}
}

// The run commits and reads the head from git; the model never does. Every git command the allocator runs targets a
// directory under the test's own temporary tree.
func TestRunCommitsAndReadsHead(t *testing.T) {
	e := newEnv(t)
	res := e.run(`const r = await agent.call('build', { agentType: 'executor', node: 'n1', label: 'build' }); return r.status`)
	if got := value[string](t, res); got != "completed" {
		t.Fatalf("%s", got)
	}
	wt := filepath.Join(e.state, "flow", e.runID, "wt", "n1", "a1-l0")
	writeFile(t, filepath.Join(wt, "feature.txt"), "work\n")
	ctx := context.Background()
	commit, err := e.alloc.Commit(ctx, wt, "CRW: the work", false)
	if err != nil {
		t.Fatal(err)
	}
	head, err := e.alloc.Head(ctx, wt)
	if err != nil {
		t.Fatal(err)
	}
	if commit != head || head != git(t, wt, "rev-parse", "HEAD") {
		t.Errorf("commit %s head %s git %s", commit, head, git(t, wt, "rev-parse", "HEAD"))
	}
	if head == e.base || git(t, wt, "rev-parse", "HEAD^") != e.base {
		t.Errorf("the commit must sit on the start commit")
	}
	if git(t, wt, "show", "--name-only", "--format=", "HEAD") != "feature.txt" {
		t.Errorf("the commit holds %q", git(t, wt, "show", "--name-only", "--format=", "HEAD"))
	}
	if _, err := e.alloc.Commit(ctx, wt, "empty", false); err == nil {
		t.Errorf("a commit of nothing must fail unless it is allowed")
	}
	if _, err := e.alloc.Commit(ctx, wt, "", true); err == nil {
		t.Errorf("a commit needs a message")
	}
	// no commit came from the host: the fake host never ran a command, and the repository's main branch is untouched
	if git(t, e.repo, "rev-parse", "main") != e.base {
		t.Errorf("main moved")
	}
	// every git command targets the test's temporary tree
	root := realPath(t, e.root)
	for _, c := range e.gitCalls() {
		dir, err := filepath.EvalSymlinks(c.Dir)
		if err != nil {
			dir = c.Dir
		}
		if !strings.HasPrefix(dir+string(filepath.Separator), root+string(filepath.Separator)) {
			t.Errorf("git -C %s %v targets a directory outside the test's temporary tree %s", c.Dir, c.Args, root)
		}
	}
	if len(e.gitCalls()) == 0 {
		t.Errorf("no git command was recorded")
	}
	tmpRoot, _ := tmploc.Lookup(os.Getenv)
	if home := realPath(t, os.Getenv("HOME")); !strings.HasPrefix(home, realPath(t, tmpRoot)) {
		t.Errorf("HOME %s is not a temporary directory", home)
	}
}

// A reviewer reads a detached, read-only worktree at the head it names; its thread is read-only and never gets a cwd on a turn.
func TestReviewerReadOnlyDetachedHead(t *testing.T) {
	e := newEnv(t)
	writeFile(t, filepath.Join(e.repo, "b.txt"), "two\n")
	git(t, e.repo, "add", "-A")
	git(t, e.repo, "commit", "-q", "-m", "second")
	head := git(t, e.repo, "rev-parse", "HEAD")
	git(t, e.repo, "checkout", "-q", "--detach", e.base)
	res := e.run(`const r = await agent.call('review', { agentType: 'ocx-gpt-6-1-sol', node: 'n1', head: ` + q(head) + `, label: 'review' }); return [r.status, r.meta.role]`)
	if got := value[[]string](t, res); !slices.Equal(got, []string{"completed", "reviewer"}) {
		t.Fatalf("%v", got)
	}
	id := calls(res)["review"].CallID
	want := filepath.Join(e.state, "flow", e.runID, "calls", id, "wt")
	start := e.requests("thread/start")[0].Params
	if start["cwd"] != want || start["sandbox"] != "read-only" {
		t.Fatalf("thread/start %v", start)
	}
	if got := git(t, want, "rev-parse", "HEAD"); got != head {
		t.Errorf("the reviewer's worktree is at %s, want the named head %s (the repository itself is at %s)", got, head, e.base)
	}
	if !gitFails(want, "symbolic-ref", "-q", "HEAD") {
		t.Errorf("the reviewer's worktree must be detached")
	}
	if _, err := os.Stat(filepath.Join(want, "b.txt")); err != nil {
		t.Errorf("the head's file is missing: %v", err)
	}
	if _, has := e.requests("turn/start")[0].Params["cwd"]; has {
		t.Errorf("a reviewer turn must not carry a cwd")
	}
	if alloc, _ := e.row("review", engine.EvPrepared).Data["allocation"].(map[string]any); alloc["kind"] != "call" || alloc["start_commit"] != head {
		t.Errorf("prepared allocation %v", alloc)
	}
	// no executor worktree was made for a reviewer
	for _, p := range registered(t, e.repo) {
		if strings.Contains(p, string(filepath.Separator)+"wt"+string(filepath.Separator)+"n1"+string(filepath.Separator)) {
			t.Errorf("a reviewer call made the executor worktree %s", p)
		}
	}
}

// No engine thread is ephemeral: every thread/start and thread/fork says ephemeral false, the model holds none, and the
// backend never asks for history an ephemeral thread would refuse.
func TestEngineNeverStartsEphemeral(t *testing.T) {
	e := newEnv(t)
	res := e.run(`const S = ` + verdictSchema + `
const a = await agent.call('a', { agentType: 'executor', node: 'n1', schema: S, label: 'a' })
const b = await agent.call('b', { agentType: 'architect', label: 'b' })
const c = await agent.call('c', { agentType: 'explorer', label: 'c' })
const d = await agent.call('d', { agentType: 'ocx-gpt-6-1-sol', node: 'n1', label: 'd' })
const f = await agent.call('f', { resume: a, label: 'f' })
return [a.status, b.status, c.status, d.status, f.status]`)
	if got := value[[]string](t, res); !slices.Equal(got, []string{"completed", "completed", "completed", "completed", "completed"}) {
		t.Fatalf("%v", got)
	}
	starts := e.requests("thread/start", "thread/fork")
	if len(starts) != 4 {
		t.Fatalf("%d thread creations", len(starts))
	}
	for _, r := range starts {
		if eph, present := r.Params["ephemeral"]; !present || eph != false {
			t.Errorf("%s: ephemeral = %v (present %v)", r.Method, eph, present)
		}
	}
	for _, v := range e.threads.All() {
		if v.Ephemeral {
			t.Errorf("thread %s is ephemeral", v.ID)
		}
	}
	for _, r := range e.requests("thread/read") {
		if r.Params["includeTurns"] != false {
			t.Errorf("thread/read %v hydrates history", r.Params)
		}
	}
	for _, r := range e.requests() {
		if r.Params["ephemeral"] == true || r.Params["includeTurns"] == true {
			t.Errorf("%s asks for %v", r.Method, r.Params)
		}
	}
}

// The real answers codex-cli 0.154.0 gives for the history of an ephemeral thread are fixtures, and the fake serves them.
func TestEphemeralHistoryRefusedFixture(t *testing.T) {
	read := func(name string) map[string]any {
		raw, err := os.ReadFile(filepath.Join("..", "..", "..", "bridge", "appserver", "testdata", "fixtures", name))
		if err != nil {
			t.Fatal(err)
		}
		var frame map[string]any
		if err := json.Unmarshal(raw, &frame); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		return frame
	}
	includeTurns := read("thread-read-ephemeral-include-turns.json")["error"]
	turnsList := read("thread-turns-list-ephemeral.json")["error"]
	if !jsonEqualAny(t, includeTurns, fakehost.EphemeralIncludeTurnsRefusal) {
		t.Errorf("the fake's includeTurns refusal %v is not the fixture %v", fakehost.EphemeralIncludeTurnsRefusal, includeTurns)
	}
	if !jsonEqualAny(t, turnsList, fakehost.EphemeralTurnsListRefusal) {
		t.Errorf("the fake's thread/turns/list refusal %v is not the fixture %v", fakehost.EphemeralTurnsListRefusal, turnsList)
	}

	e := newEnv(t)
	ctx := context.Background()
	raw, err := e.client.Call(ctx, "thread/start", map[string]any{"cwd": "/work", "ephemeral": true, "model": "m"})
	if err != nil {
		t.Fatal(err)
	}
	var started struct {
		Thread struct {
			ID        string `json:"id"`
			Ephemeral bool   `json:"ephemeral"`
		} `json:"thread"`
	}
	if err := json.Unmarshal(raw, &started); err != nil || !started.Thread.Ephemeral {
		t.Fatalf("%s %v", raw, err)
	}
	for method, params := range map[string]map[string]any{
		"thread/read":       {"threadId": started.Thread.ID, "includeTurns": true},
		"thread/turns/list": {"threadId": started.Thread.ID, "itemsView": "full"},
	} {
		_, err := e.client.Call(ctx, method, params)
		var rpc *appserver.RPCError
		if !asRPC(err, &rpc) {
			t.Fatalf("%s: %v, want the host's refusal", method, err)
		}
		fixture := includeTurns
		if method == "thread/turns/list" {
			fixture = turnsList
		}
		if !jsonEqualAny(t, rpc.Object, fixture) {
			t.Errorf("%s: %v, want the fixture %v", method, rpc.Object, fixture)
		}
	}
	// an ephemeral thread still reads without turns, which is why a backend must check the flag it reads back
	if _, err := e.client.Call(ctx, "thread/read", map[string]any{"threadId": started.Thread.ID, "includeTurns": false}); err != nil {
		t.Errorf("thread/read without turns: %v", err)
	}
}

// A pair's auto-compaction limit reaches the thread's config, and the threads a backend starts are the ones the flow's
// server-request responder owns.
func TestAutoCompactLimitReachesThreadStart(t *testing.T) {
	e := newEnv(t)
	limit := int64(180000)
	spec := e.policy.Roles["executor"]
	info, err := e.backend.StartThread(context.Background(), engine.ThreadSpec{CallID: "c_x", Role: "executor", Pair: spec.Pair, Sandbox: spec.Sandbox, Cwd: filepath.Join(e.root, "direct"), AutoCompactTokenLimit: &limit})
	if err != nil {
		t.Fatal(err)
	}
	config, _ := e.requests("thread/start")[0].Params["config"].(map[string]any)
	if fmt.Sprint(config["model_auto_compact_token_limit"]) != "180000" || config["model_reasoning_effort"] != "xhigh" {
		t.Errorf("config %v", config)
	}
	if info.Pair != spec.Pair || info.Cwd != filepath.Join(e.root, "direct") {
		t.Errorf("%+v", info)
	}
	if !e.owned.Owns(info.ID) || e.owned.Owns("thr-not-mine") {
		t.Errorf("the threads the backend started are the ones its responder owns: %v", e.owned.IDs())
	}
	// a relative directory is refused before anything is sent
	if _, err := e.backend.StartThread(context.Background(), engine.ThreadSpec{CallID: "c_y", Role: "executor", Pair: spec.Pair, Sandbox: spec.Sandbox, Cwd: "relative"}); err == nil {
		t.Errorf("a relative cwd was accepted")
	}
	if n := len(e.requests("thread/start")); n != 1 {
		t.Errorf("%d thread/start requests", n)
	}
}

// TestCommitScrubsPublicMessageAndStopsSecrets (CRW-1549): the commit message is public text — a
// private path or the host's name is replaced before git writes it, and a token shape stops the
// commit before anything is staged, leaving HEAD and the index untouched.
func TestCommitScrubsPublicMessageAndStopsSecrets(t *testing.T) {
	e := newEnv(t)
	res := e.run(`const r = await agent.call('build', { agentType: 'executor', node: 'n1', label: 'build' }); return r.status`)
	if got := value[string](t, res); got != "completed" {
		t.Fatalf("%s", got)
	}
	wt := filepath.Join(e.state, "flow", e.runID, "wt", "n1", "a1-l0")
	ctx := context.Background()
	// a message holding a private path is masked in the commit git writes
	writeFile(t, filepath.Join(wt, "feature.txt"), "work\n")
	priv := e.repo
	if _, err := e.alloc.Commit(ctx, wt, "CRW: work in "+priv, false); err != nil {
		t.Fatal(err)
	}
	msg := git(t, wt, "log", "-1", "--format=%B")
	if strings.Contains(msg, priv) || !strings.Contains(msg, "[path]") {
		t.Errorf("the committed message holds the private path: %q", msg)
	}
	// a token stops the commit before the index: nothing staged, nothing committed
	writeFile(t, filepath.Join(wt, "secret.txt"), "x\n")
	token := "ghp_" + strings.Repeat("A", 24)
	before := git(t, wt, "rev-parse", "HEAD")
	if _, err := e.alloc.Commit(ctx, wt, "key "+token, false); err == nil {
		t.Fatal("a commit with a token shape succeeded")
	} else if strings.Contains(err.Error(), token) {
		t.Errorf("the error echoes the token: %v", err)
	}
	if git(t, wt, "rev-parse", "HEAD") != before {
		t.Error("HEAD moved on a blocked commit")
	}
	if staged := git(t, wt, "diff", "--cached", "--name-only"); staged != "" {
		t.Errorf("a blocked commit staged %q", staged)
	}
}
