package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/bound"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/journal"
)

// deadSessionRun starts a session run of a script that fans b and c out after a, under agents.max_threads 2, and lets its process
// die right after a's completed row: the shipped journal.ResumeEnv is the engine's, as the binary installs it.
func deadSessionRun(t *testing.T) (k *sessionE2E, run, script string) {
	t.Helper()
	k = newSessionE2E(t)
	prev := engine.ResumeEnv
	t.Cleanup(func() { engine.ResumeEnv = prev })
	engine.ResumeEnv = journal.ResumeEnv
	// agents.max_threads 2: a session run keeps one call in flight
	codex := filepath.Join(t.TempDir(), "codex")
	_ = os.MkdirAll(codex, 0o700)
	if err := os.WriteFile(filepath.Join(codex, "config.toml"), []byte("[agents]\nmax_threads = 2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	k.td.Getenv = func(key string) (string, bool) {
		if key == "CODEX_HOME" {
			return codex, true
		}
		return "", false
	}
	script = filepath.Join(t.TempDir(), "fan.js")
	if err := os.WriteFile(script, []byte(`export const meta = { name: 'fan', phases: ['one'] }
phase('one')
const a = await agent.call('look at a', { agentType: 'explorer', label: 'a' })
const rest = await parallel([
  () => agent.call('look at b', { agentType: 'explorer', label: 'b' }),
  () => agent.call('look at c', { agentType: 'explorer', label: 'c' }),
])
return [a.status, rest[0].status, rest[1].status]
`), 0o644); err != nil {
		t.Fatal(err)
	}
	// the first process dies right after the completed row of a, and its exit closes the owner's line
	k.killWhen = func(e engine.Event) bool {
		return e.Kind == engine.KindCall && e.Label == "a" && e.Event == engine.EvCompleted
	}
	code, out, errOut := k.cmd("start", script, "--thread", k.owner, "--repo", k.repo, "--json")
	if code != 0 {
		t.Fatalf("start: exit %d\n%s\n%s", code, out, errOut)
	}
	var started StartAnswer
	if err := json.Unmarshal([]byte(out), &started); err != nil {
		t.Fatal(err)
	}
	run = started.Run
	_, stA := k.nextSpawn(run)
	k.dispatched(run, stA, k.spawnStep(stA, "the answer of a"))
	k.waitProcess()
	if LockHeld(k.w.state, run) {
		t.Fatal("the dead process still holds the run lock")
	}
	if got := bound.Lookup(k.owner); got.State == bound.Orchestrator && got.Line.RunID == run {
		t.Fatalf("the dead process left the owner's line open: %+v", got)
	}
	return k, run, script
}

// TestForegroundResumeOfASessionRunBindsTheOwnerAndCapsThePool (CRW-1348, evaluation d1): the foreground `crw flow resume RUN SCRIPT` of a session run goes through the engine's command and the
// shipped journal.ResumeEnv. It holds the run the way the run process does: the owner thread gets the run's orchestrator line again
// for as long as the resume runs (the dead process closed it), the pool is held to agents.max_threads - 1 whatever --pool says,
// and the line is closed when the run ends.
func TestForegroundResumeOfASessionRunBindsTheOwnerAndCapsThePool(t *testing.T) {
	k, run, script := deadSessionRun(t)
	// the foreground resume, with a pool above the slots
	type result struct {
		code        int
		out, errOut string
	}
	done := make(chan result, 1)
	go func() {
		code, out, errOut := k.cmd("resume", run, script, "--repo", k.repo, "--pool", "6")
		done <- result{code, out, errOut}
	}()
	bus := engine.NewFileStepBus(RunDir(k.w.state, run))
	spawns := func() map[string]bool {
		steps, _ := bus.Steps()
		calls := map[string]bool{}
		for _, st := range steps {
			if st.Action == engine.StepSpawn {
				calls[st.Call] = true
			}
		}
		return calls
	}
	waitFor(t, "a step of the resumed run", func() bool { return len(spawns()) > 0 })
	// read before any `crw flow next`, which would write a missing line itself
	if got := bound.Lookup(k.owner); got.State != bound.Orchestrator || got.Line.RunID != run || len(got.Line.HelperPairs) != 6 {
		t.Fatalf("the owner's binding while the foreground resume runs: %+v", got)
	}
	time.Sleep(time.Second) // the engine publishes the steps of a batch together
	if n := len(spawns()); n != 1 {
		t.Fatalf("%d spawn steps in flight with agents.max_threads 2, want 1 (the pool is max_threads - 1)", n)
	}
	seen := map[string]bool{}
	for len(seen) < 2 {
		_, st := k.nextSpawn(run)
		if seen[st.Call] {
			continue
		}
		seen[st.Call] = true
		if n := len(spawns()); n != 1 {
			t.Fatalf("%d spawn steps in flight with agents.max_threads 2, want 1", n)
		}
		k.dispatched(run, st, k.spawnStep(st, "the answer of "+st.Call))
	}
	var res result
	select {
	case res = <-done:
	case <-time.After(120 * time.Second):
		t.Fatal("the foreground resume did not end")
	}
	if res.code != 0 || !strings.Contains(res.out, `"completed"`) {
		t.Fatalf("the foreground resume: exit %d\n%s\n%s", res.code, res.out, res.errOut)
	}
	if got := bound.Lookup(k.owner); got.State != bound.Standalone {
		t.Fatalf("the owner's binding after the run ended: %+v", got)
	}
}

// TestForegroundResumeRefusesAMixedResume: a spec that names the App Server backend for a run whose started row records the session
// backend is a mixed resume. The foreground command refuses it as the run process does (exit 2), before the run is taken or the
// owner bound.
func TestForegroundResumeRefusesAMixedResume(t *testing.T) {
	k, run, script := deadSessionRun(t)
	specFile := specPath(k.w.state, run)
	orig, err := os.ReadFile(specFile)
	if err != nil {
		t.Fatal(err)
	}
	var spec map[string]any
	_ = json.Unmarshal(orig, &spec)
	spec["backend"] = BackendAppServer
	mixed, _ := json.Marshal(spec)
	if err := os.WriteFile(specFile, mixed, 0o600); err != nil {
		t.Fatal(err)
	}
	type result struct {
		code        int
		out, errOut string
	}
	done := make(chan result, 1)
	go func() {
		code, out, errOut := k.cmd("resume", run, script, "--repo", k.repo)
		done <- result{code, out, errOut}
	}()
	select {
	case res := <-done:
		if res.code != 2 || !strings.Contains(res.errOut, "mixed resume") {
			t.Fatalf("a mixed foreground resume: exit %d\n%s\n%s", res.code, res.out, res.errOut)
		}
	case <-time.After(60 * time.Second):
		t.Fatalf("the mixed foreground resume took the run and ran it (owner binding %+v)", bound.Lookup(k.owner))
	}
	if got := bound.Lookup(k.owner); got.State == bound.Orchestrator && got.Line.RunID == run {
		t.Fatalf("a refused resume bound the owner: %+v", got)
	}
}
