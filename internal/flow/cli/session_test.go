package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/bound"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
)

// CRW-1348 (CRW-1316 S3): the commands of a session run over its FileStepBus, the start's backend, binding and facts, and the
// backend a resume takes.

const sessOwner = "01a30000-0000-7000-8000-00000000000a"

// sessionSpec writes the spec of a session run of the world.
func sessionSpec(t *testing.T, w *world, run string) Spec {
	t.Helper()
	spec := Spec{Schema: SpecSchema, RunID: run, Workflow: "hello", Script: helloScript(t), Thread: sessOwner, Backend: BackendSession, CreatedAt: "2026-10-14T00:00:00Z"}
	if err := WriteSpec(w.state, spec); err != nil {
		t.Fatal(err)
	}
	return spec
}

// answerOf is a command's answer without the marker the test spawner writes into the same output.
func answerOf(out string) string { return strings.ReplaceAll(out, "<<spawned>>\n", "") }

func decodeNext(t *testing.T, out string) NextAnswer {
	t.Helper()
	out = answerOf(out)
	var ans NextAnswer
	if err := json.Unmarshal([]byte(answerOf(out)), &ans); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	return ans
}

func TestNextHandsOutTheStepsOfTheBus(t *testing.T) {
	w := newWorld(t)
	td := w.deps()
	run := "run-0123456789ab"
	sessionSpec(t, w, run)
	lock, err := TakeLock(w.state, run, "_run", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Release()
	bus := engine.NewFileStepBus(RunDir(w.state, run))
	spawnStep := engine.Step{Run: run, Call: "c_000000000001", K: 1, Attempt: 2, Op: "c_000000000001/t1-a2", Role: "reviewer-final", Action: engine.StepSpawn,
		AgentType: "reviewer", Model: "gpt-fake-sol", ReasoningEffort: "xhigh", Message: "crw-flow-op: c_000000000001/t1-a2\nCRW-ROLE: reviewer-final\n\nread it",
		Packet: "/p/packet-t1-a2.md", Workspace: "/w/call/c_000000000001"}
	inputStep := engine.Step{Run: run, Call: "c_000000000002", K: 2, Attempt: 1, Op: "c_000000000002/t2-a1", Role: "executor", Action: engine.StepSendInput,
		AgentType: "executor", AgentID: "agent-7", Message: "crw-flow-op: c_000000000002/t2-a1\n\nrepair"}
	closeStep := engine.Step{Run: run, Call: "c_000000000003", Attempt: 1, Role: "explorer", Action: engine.StepClose, AgentID: "agent-3"}
	for _, st := range []engine.Step{spawnStep, inputStep, closeStep} {
		if err := bus.Publish(st); err != nil {
			t.Fatal(err)
		}
	}
	code, out, errOut := td.do("next", run, "--json", "--state", w.state)
	if code != 0 {
		t.Fatalf("next: %d %s %s", code, out, errOut)
	}
	ans := decodeNext(t, out)
	if ans.State != NextWaitingSession || len(ans.Steps) != 3 || ans.Slots.Limit != DefaultMaxThreads || ans.Slots.Open != 1 || len(ans.Spawns) != 6 {
		t.Fatalf("answer %+v", ans)
	}
	byAction := map[string]NextStep{}
	for _, st := range ans.Steps {
		byAction[st.Action] = st
	}
	sp := byAction[engine.StepSpawn]
	if sp.Tool.Name != "spawn_agent" || sp.Tool.Arguments["agent_type"] != "reviewer" || sp.Tool.Arguments["model"] != "gpt-fake-sol" || sp.Tool.Arguments["reasoning_effort"] != "xhigh" ||
		sp.Tool.Arguments["fork_context"] != false || sp.Tool.Arguments["message"] != spawnStep.Message || sp.Packet != spawnStep.Packet || sp.Workspace != spawnStep.Workspace ||
		sp.Then != "crw flow dispatched "+run+" c_000000000001 --agent <the id spawn_agent returned> --k 1 --attempt 2" {
		t.Fatalf("spawn step %+v", sp)
	}
	// the live tool arguments: send_input and close_agent take target
	if in := byAction[engine.StepSendInput]; in.Tool.Name != "send_input" || in.Tool.Arguments["target"] != "agent-7" || in.Tool.Arguments["message"] != inputStep.Message ||
		!strings.Contains(in.Then, "--agent agent-7 --k 2 --attempt 1") {
		t.Fatalf("send_input step %+v", in)
	}
	if cl := byAction[engine.StepClose]; cl.Tool.Name != "close_agent" || cl.Tool.Arguments["target"] != "agent-3" || cl.Then != "" {
		t.Fatalf("close step %+v", cl)
	}
	for _, want := range []string{"close_agent(target=...) agent-3", "wait_agent(targets=", "timeout_ms=600000", "send_input(target=<agent_id>"} {
		if !strings.Contains(ans.Hint, want) {
			t.Errorf("the hint does not say %q: %s", want, ans.Hint)
		}
	}
	// the binding of the owner was missing: next wrote it again, with a warning
	if len(ans.Warnings) != 1 || !strings.Contains(ans.Warnings[0], "missing and is written again") || !strings.Contains(errOut, "warning") {
		t.Fatalf("warnings %v / %s", ans.Warnings, errOut)
	}
	if got := bound.Lookup(sessOwner); got.State != bound.Orchestrator || got.Line.RunID != run || len(got.Line.HelperPairs) != 6 {
		t.Fatalf("the owner's binding: %+v", got)
	}
	// the close step was handed out once: its file is gone, the others stay until the run binds or retracts them
	steps, _ := bus.Steps()
	if len(steps) != 2 {
		t.Fatalf("steps left %+v", steps)
	}
	_, out, _ = td.do("next", run, "--json", "--state", w.state)
	if again := decodeNext(t, out); len(again.Steps) != 2 || len(again.Warnings) != 0 {
		t.Fatalf("the second next: %+v", again)
	}
	// with the slots full the hint says the spawn waits for a close
	cfg := filepath.Join(t.TempDir(), "codex")
	_ = os.MkdirAll(cfg, 0o700)
	_ = os.WriteFile(filepath.Join(cfg, "config.toml"), []byte("[agents]\nmax_threads = 1\n"), 0o600)
	td.Getenv = func(k string) (string, bool) {
		if k == "CODEX_HOME" {
			return cfg, true
		}
		return "", false
	}
	_, out, _ = td.do("next", run, "--json", "--state", w.state)
	if full := decodeNext(t, out); full.Slots.Limit != 1 || !strings.Contains(full.Hint, "agent thread limit reached") {
		t.Fatalf("full slots: %+v", full)
	}
}

func TestNextResumesARunWithNoProcess(t *testing.T) {
	w := newWorld(t)
	td := w.deps()
	r := w.newRun("run-0123456789ac", sessOwner)
	r.close()
	spec := sessionSpec(t, w, r.id)
	code, out, errOut := td.do("next", r.id, "--json", "--state", w.state)
	if code != 0 {
		t.Fatalf("next: %d %s %s", code, out, errOut)
	}
	ans := decodeNext(t, out)
	if len(td.spawned) != 1 || !td.spawned[0].Resume || td.spawned[0].Spec.RunID != spec.RunID || ans.Resumed != 4242 ||
		!strings.Contains(ans.Hint, "crw flow resume "+r.id+" --detach") {
		t.Fatalf("no resume: %+v %+v", ans, td.spawned)
	}
	// a paused run is not resumed
	w2 := newWorld(t)
	td2 := w2.deps()
	p := w2.newRun("run-0123456789ad", sessOwner)
	p.flow(engine.EvRunPaused, map[string]any{"by": "operator", "reason": "crw flow pause"})
	p.close()
	sessionSpec(t, w2, p.id)
	_, out, _ = td2.do("next", p.id, "--json", "--state", w2.state)
	if ans := decodeNext(t, out); ans.State != NextPaused || len(td2.spawned) != 0 || !strings.Contains(ans.Hint, "paused") {
		t.Fatalf("a paused run: %+v %+v", ans, td2.spawned)
	}
	// a run that is over says how it ended and is not resumed
	w3 := newWorld(t)
	td3 := w3.deps()
	f := w3.newRun("run-0123456789ae", sessOwner)
	f.finish(engine.RunCompleted, "")
	f.close()
	sessionSpec(t, w3, f.id)
	_, out, _ = td3.do("next", f.id, "--json", "--state", w3.state)
	if ans := decodeNext(t, out); ans.State != NextEnded || ans.Ended == nil || ans.Ended.Status != engine.RunCompleted || len(td3.spawned) != 0 {
		t.Fatalf("an ended run: %+v", ans)
	}
	// a run of the App Server backend publishes no steps
	w4 := newWorld(t)
	td4 := w4.deps()
	if err := WriteSpec(w4.state, Spec{Schema: SpecSchema, RunID: "run-0123456789af", Workflow: "hello", Thread: sessOwner, Backend: BackendAppServer}); err != nil {
		t.Fatal(err)
	}
	if code, _, errOut := td4.do("next", "run-0123456789af", "--state", w4.state); code != 2 || !strings.Contains(errOut, "app-server") {
		t.Fatalf("an App Server run: %d %s", code, errOut)
	}
}

func TestDispatchedWritesTheInboxAndPrintsTheReply(t *testing.T) {
	w := newWorld(t)
	td := w.deps()
	run := "run-0123456789b0"
	sessionSpec(t, w, run)
	bus := engine.NewFileStepBus(RunDir(w.state, run))
	// no process: the report waits in the inbox for the resume
	code, out, _ := td.do("dispatched", run, "c_000000000001", "--agent", "agent-1", "--k", "1", "--attempt", "1", "--json", "--state", w.state)
	var queued map[string]any
	_ = json.Unmarshal([]byte(out), &queued)
	if code != 0 || queued["status"] != "queued" || !strings.Contains(queued["detail"].(string), "waits in its inbox") {
		t.Fatalf("no process: %d %s", code, out)
	}
	reps, _ := bus.Reports()
	if len(reps) != 1 || reps[0].Call != "c_000000000001" || reps[0].Agent != "agent-1" || reps[0].K != 1 || reps[0].Attempt != 1 || reps[0].Run != run {
		t.Fatalf("inbox %+v", reps)
	}
	// with a process: it answers, and dispatched prints the answer
	lock, err := TakeLock(w.state, run, "_run", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Release()
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		for {
			select {
			case <-stop:
				return
			case <-time.After(10 * time.Millisecond):
			}
			reps, _ := bus.Reports()
			for _, rep := range reps {
				reply := engine.DispatchReply{Status: engine.ReplyBound}
				if rep.Agent == "agent-x" {
					reply = engine.DispatchReply{Status: engine.ReplyPairMismatch, Close: "agent-x", Detail: "pair_mismatch: close agent-x and spawn again with m/e"}
				}
				_ = bus.Reply(rep, reply)
			}
		}
	}()
	code, out, _ = td.do("dispatched", run, "c_000000000002", "--agent", "agent-2", "--k", "1", "--attempt", "1", "--json", "--state", w.state)
	var bound map[string]any
	_ = json.Unmarshal([]byte(out), &bound)
	if code != 0 || bound["status"] != engine.ReplyBound {
		t.Fatalf("bound: %d %s", code, out)
	}
	code, out, _ = td.do("dispatched", run, "c_000000000002", "--agent", "agent-x", "--k", "1", "--attempt", "1", "--state", w.state)
	if code != 1 || !strings.Contains(out, "pair_mismatch: close agent-x") {
		t.Fatalf("pair mismatch: %d %s", code, out)
	}
	for _, args := range [][]string{
		{"dispatched", run, "c_1", "--k", "1", "--attempt", "1"},
		{"dispatched", run, "c_1", "--agent", "a", "--k", "0", "--attempt", "1"},
		{"dispatched", run, "--agent", "a", "--k", "1", "--attempt", "1"},
	} {
		if code, _, _ := td.do(append(args, "--state", w.state)...); code != 2 {
			t.Errorf("%v: exit %d, want a usage error", args, code)
		}
	}
}

func helloScript(t *testing.T) string {
	t.Helper()
	script := filepath.Join(t.TempDir(), "hello.js")
	if err := os.WriteFile(script, []byte("export const meta = { name: 'hello' }\nconst a = await agent.call('hi', { agentType: 'explorer', label: 'hi' })\nreturn a.status\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return script
}

func TestStartDefaultsToASessionRunAndBindsItsOwner(t *testing.T) {
	isolateGit(t)
	w := newWorld(t)
	td := w.deps()
	script := helloScript(t)
	cfg := filepath.Join(t.TempDir(), "codex")
	_ = os.MkdirAll(cfg, 0o700)
	_ = os.WriteFile(filepath.Join(cfg, "config.toml"), []byte("[agents]\nmax_threads = 4\n"), 0o600)
	td.Getenv = func(k string) (string, bool) {
		if k == "CODEX_HOME" {
			return cfg, true
		}
		return "", false
	}
	code, out, errOut := td.do("start", script, "--thread", sessOwner, "--state", w.state, "--json")
	if code != 0 || len(td.spawned) != 1 {
		t.Fatalf("start: %d %s %s", code, out, errOut)
	}
	var ans StartAnswer
	_ = json.Unmarshal([]byte(answerOf(out)), &ans)
	spec := td.spawned[0].Spec
	if spec.Backend != BackendSession || ans.Plan.Backend != BackendSession || spec.Pool != 3 || ans.Plan.Pool != 3 || !ans.Bound || ans.BindWarning != "" ||
		ans.Next != "crw flow next "+spec.RunID+" --wait 600 --json" || len(ans.Plan.Spawns) != 6 || !strings.Contains(ans.Plan.Loop, "close_agent(target=") {
		t.Fatalf("the start: %+v (spec %+v)", ans, spec)
	}
	names := map[string]bool{}
	for _, f := range ans.Plan.Facts {
		names[f.Name] = true
	}
	for _, name := range SessionFacts {
		if !names[name] {
			t.Errorf("no %s fact: %+v", name, ans.Plan.Facts)
		}
	}
	got := bound.Lookup(sessOwner)
	if got.State != bound.Orchestrator || got.Line.RunID != spec.RunID || len(got.Line.HelperPairs) != 6 ||
		got.Line.HelperPairs["executor-t1"].Model != engine.FakePolicy().Roles["executor-t1"].Pair.Model {
		t.Fatalf("the owner's binding: %+v", got)
	}
	// a second start while the first run's process holds its lock: it starts, warns, and is not bound
	lock, err := TakeLock(w.state, spec.RunID, "_run", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	code, out, errOut = td.do("start", script, "--thread", sessOwner, "--state", w.state, "--json")
	ans = StartAnswer{}
	_ = json.Unmarshal([]byte(answerOf(out)), &ans)
	if code != 0 || len(td.spawned) != 2 || ans.Bound || !strings.Contains(ans.BindWarning, "one thread binds one live run") || !strings.Contains(errOut, "warning") {
		t.Fatalf("a second live start: %d %+v %s", code, ans, errOut)
	}
	if got := bound.Lookup(sessOwner); got.Line.RunID != spec.RunID {
		t.Fatalf("the second start took the thread: %+v", got.Line)
	}
	// the first run's process is gone: a third start closes its line and binds
	lock.Release()
	code, out, _ = td.do("start", script, "--thread", sessOwner, "--state", w.state, "--json")
	ans = StartAnswer{}
	_ = json.Unmarshal([]byte(answerOf(out)), &ans)
	if code != 0 || !ans.Bound || bound.Lookup(sessOwner).Line.RunID != ans.Run {
		t.Fatalf("a start over a stale line: %d %+v", code, ans)
	}
	// the App Server backend stays reachable: no binding, no session fact
	before := bound.Lookup(sessOwner).Line
	code, out, _ = td.do("start", script, "--thread", sessOwner, "--state", w.state, "--json", "--backend", "app-server")
	ans = StartAnswer{}
	_ = json.Unmarshal([]byte(answerOf(out)), &ans)
	if code != 0 || ans.Plan.Backend != BackendAppServer || td.spawned[len(td.spawned)-1].Spec.Backend != BackendAppServer || ans.Bound {
		t.Fatalf("an App Server start: %d %+v", code, ans)
	}
	for _, f := range ans.Plan.Facts {
		if f.Name == FactOwnerBound {
			t.Fatalf("an App Server start judged %s", f.Name)
		}
	}
	if after := bound.Lookup(sessOwner).Line; after.RunID != before.RunID {
		t.Fatalf("an App Server start bound the owner: %+v", after)
	}
	if code, _, errOut := td.do("start", script, "--thread", sessOwner, "--state", w.state, "--backend", "threads"); code != 2 || !strings.Contains(errOut, "--backend") {
		t.Fatalf("an unknown backend: %d %s", code, errOut)
	}
}

// The session facts are warnings with their reasons, and a session run whose facts fail still starts (CRW-1318).
func TestSessionFactsWarnAndNeverBlockTheStart(t *testing.T) {
	isolateGit(t)
	w := newWorld(t)
	td := w.deps()
	cfg := filepath.Join(t.TempDir(), "codex")
	_ = os.MkdirAll(cfg, 0o700)
	_ = os.WriteFile(filepath.Join(cfg, "config.toml"), []byte("[agents]\nmax_threads = 1\n\n[features]\nmulti_agent_v2 = true\n"), 0o600)
	td.Getenv = func(k string) (string, bool) {
		switch k {
		case "CODEX_HOME":
			return cfg, true
		case "PATH":
			return t.TempDir(), true
		}
		return "", false
	}
	code, out, errOut := td.do("start", helloScript(t), "--thread", "thread-owner", "--state", w.state, "--socket", filepath.Join(t.TempDir(), "no.sock"), "--json")
	if code != 0 || len(td.spawned) != 1 {
		t.Fatalf("the start was blocked: %d %s %s", code, out, errOut)
	}
	var ans StartAnswer
	_ = json.Unmarshal([]byte(answerOf(out)), &ans)
	levels := map[string]Fact{}
	for _, f := range ans.Plan.Facts {
		levels[f.Name] = f
	}
	for _, name := range []string{FactOwnerBound, FactOwnerWritable, FactSubagentSlots, FactMultiAgentV1, FactVerifierPrereqs} {
		if f := levels[name]; f.Level != LevelWarn || f.Reason == "" {
			t.Errorf("%s: %+v", name, f)
		}
	}
	if f := levels[FactRoleModels]; f.Level != LevelPass {
		t.Errorf("role_models of the fake table: %+v", f)
	}
	if !strings.Contains(levels[FactMultiAgentV1].Reason, "stops") || !strings.Contains(levels[FactVerifierPrereqs].Reading, "crw-dev") {
		t.Errorf("readings %+v %+v", levels[FactMultiAgentV1], levels[FactVerifierPrereqs])
	}
	if spec := td.spawned[0].Spec; spec.Pool != 1 || !strings.Contains(factNames(spec.Preflight), FactSubagentSlots) {
		t.Errorf("the spec: pool %d, preflight %s", spec.Pool, factNames(spec.Preflight))
	}
	if !strings.Contains(errOut, "warning") {
		t.Errorf("no warning printed: %s", errOut)
	}
}

// A resume takes the backend the run's started row records; a spec that names another is a mixed resume, an input error.
func TestResumeTakesTheRecordedBackend(t *testing.T) {
	startedOf := func(run, backend string) []engine.Event {
		data := map[string]any{"workflow": "w"}
		if backend != "" {
			data["backend"] = backend
		}
		return []engine.Event{{RunID: run, Kind: engine.KindRun, Event: "started", Data: data}}
	}
	started := func(backend string) []engine.Event { return startedOf("run-1", backend) }
	for _, c := range []struct {
		name, spec string
		events     []engine.Event
		resume     bool
		want       string
		refused    bool
	}{
		{"a new run takes the spec's", BackendSession, nil, false, BackendSession, false},
		{"an old spec is the App Server's", "", nil, false, BackendAppServer, false},
		{"a resume takes the recorded session backend", BackendSession, started(BackendSession), true, BackendSession, false},
		{"a resume of a run with no started row takes the spec's", BackendSession, nil, true, BackendSession, false},
		{"an old run resumes on the App Server", "", started(""), true, BackendAppServer, false},
		{"a mixed resume is refused", BackendSession, started(BackendAppServer), true, "", true},
	} {
		got, err := worldBackend(Spec{RunID: "run-1", Backend: c.spec}, c.events, c.resume)
		if c.refused {
			if err == nil || !strings.Contains(err.Error(), "mixed resume") {
				t.Errorf("%s: %q %v", c.name, got, err)
			}
			continue
		}
		if err != nil || got != c.want {
			t.Errorf("%s: %q %v, want %q", c.name, got, err, c.want)
		}
	}
	// the run process refuses it as an input error, with the reason
	w := newWorld(t)
	td := w.deps()
	r := w.newRun("run-0123456789b1", sessOwner)
	r.close()
	sessionSpec(t, w, r.id)
	td.World = func(ctx context.Context, req WorldRequest) (World, error) {
		_, err := worldBackend(req.Spec, startedOf(req.Spec.RunID, BackendAppServer), req.Resume)
		return World{}, err
	}
	if code, _, errOut := td.do(ProcessCommand, "--resume", r.id, "--state", w.state); code != 2 || !strings.Contains(errOut, "mixed resume") {
		t.Fatalf("the process: %d %s", code, errOut)
	}
}
