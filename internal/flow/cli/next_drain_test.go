package cli

import (
	"strings"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
)

// CRW-1492 (D5): `crw flow next` of a run an answer drained. The drain stops the run at its next boundary and leaves the turns in flight
// to end: while the run's process lives, next hands out the steps the run published (they passed the gate before the drain), so the
// owner's loop goes on until the process ends; once no run process holds the run, next says paused and that the same answer resumes it.
// An operator's pause interrupts the turns: next says paused at once, also while the process lives.

// drainStep is a spawn step a session run published before the drain.
func drainStep(run string) engine.Step {
	return engine.Step{Run: run, Call: "c_000000000001", K: 1, Attempt: 1, Op: "c_000000000001/t1-a1", Role: "executor", Action: engine.StepSpawn,
		AgentType: "executor", Model: "gpt-fake-sol", ReasoningEffort: "high", Message: "crw-flow-op: c_000000000001/t1-a1\nCRW-ROLE: executor\n\nbuild it",
		Packet: "/p/packet-t1-a1.md", Workspace: "/w/call/c_000000000001"}
}

// holdAs takes the run lock of run as the command cmd does; the test releases it.
func holdAs(t *testing.T, w *world, run, cmd string) *Lock {
	t.Helper()
	lock, err := TakeLock(w.state, run, cmd, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return lock
}

// nextOf runs `crw flow next RUN --json` and decodes its answer.
func nextOf(t *testing.T, td *testDeps, run string) NextAnswer {
	t.Helper()
	code, out, errOut := td.do("next", run, "--json", "--state", td.w.state)
	if code != 0 {
		t.Fatalf("next: %d %s %s", code, out, errOut)
	}
	return decodeNext(t, out)
}

func TestNextKeepsHandingOutStepsWhileADrainedRunLives(t *testing.T) {
	// the owner's binding lives in the home's flow-bound directory: a home of the test's own (the "holds no open line" warning is expected)
	t.Setenv("HOME", t.TempDir())
	w := newWorld(t)
	td := w.deps()
	run := "run-0123456789c1"
	sessionSpec(t, w, run)
	r := w.newRun(run, sessOwner)
	r.flow(engine.EvRunPaused, map[string]any{"by": "answer", "reason": "crw flow answer: a decision waits to be applied"})
	r.close()
	step := drainStep(run)
	if err := engine.NewFileStepBus(RunDir(w.state, run)).Publish(step); err != nil {
		t.Fatal(err)
	}
	// the run's process lives (it holds the run as `crw flow _run`) and drains: next hands out the step it published before the drain
	proc := holdAs(t, w, run, "_run")
	ans := nextOf(t, td, run)
	if ans.State != NextWaitingSession || len(ans.Steps) != 1 || ans.Steps[0].Call != step.Call || ans.Steps[0].Tool.Name != "spawn_agent" ||
		ans.Resumed != 0 || len(td.spawned) != 0 {
		proc.Release()
		t.Fatalf("a drained run whose process lives: %+v %+v", ans, td.spawned)
	}
	if !strings.Contains(ans.Hint, "take every step's tool call") || !strings.Contains(ans.Hint, "--wait 120 --json once") || strings.Contains(ans.Hint, "is paused") || strings.Contains(ans.Hint, "alternate") {
		proc.Release()
		t.Fatalf("the hint of a drained live run is not the session loop: %s", ans.Hint)
	}

	// the process ended paused (its run/paused row by answer) and an answer holds the run to write: a writer's hold is not a run
	// process, so next says paused, resumes nothing and says the same answer applies and resumes the run
	j, err := openJournal(w, run)
	if err != nil {
		proc.Release()
		t.Fatal(err)
	}
	j.Append(engine.Event{RunID: run, Kind: engine.KindRun, Event: "paused", Data: map[string]any{"status": engine.RunPaused, "by": "answer", "reason": "crw flow answer: a decision waits to be applied"}})
	if err := j.Close(); err != nil {
		proc.Release()
		t.Fatal(err)
	}
	proc.Release()
	writer := holdAs(t, w, run, "answer")
	ans = nextOf(t, td, run)
	writer.Release()
	wantHint := "run " + run + " ended paused for an answer: run the same crw flow answer again (it applies and resumes the run); crw flow resume " + run + " goes on without it"
	if ans.State != NextPaused || ans.Resumed != 0 || len(td.spawned) != 0 || !strings.Contains(bare(ans.Hint), wantHint) {
		t.Fatalf("a drained run an answer holds: %+v %+v", ans, td.spawned)
	}
	// with no holder at all: the same
	ans = nextOf(t, td, run)
	if ans.State != NextPaused || ans.Resumed != 0 || len(td.spawned) != 0 || !strings.Contains(bare(ans.Hint), wantHint) {
		t.Fatalf("a drained run no process holds: %+v %+v", ans, td.spawned)
	}
}

// TestNextSaysPausedAtOnceForAnOperatorPause (guard): an operator's pause interrupts the turns, so next says paused while the run's
// process lives, as at the base; an operator pause after an answer's drain names the end, and next says paused too.
func TestNextSaysPausedAtOnceForAnOperatorPause(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	for _, c := range []struct {
		name string
		by   []string
	}{
		{"operator", []string{"operator"}},
		{"answer then operator", []string{"answer", "operator"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			w := newWorld(t)
			td := w.deps()
			run := "run-0123456789c2"
			sessionSpec(t, w, run)
			r := w.newRun(run, sessOwner)
			for _, by := range c.by {
				r.flow(engine.EvRunPaused, map[string]any{"by": by, "reason": "crw flow " + by})
			}
			r.close()
			if err := engine.NewFileStepBus(RunDir(w.state, run)).Publish(drainStep(run)); err != nil {
				t.Fatal(err)
			}
			proc := holdAs(t, w, run, "_run")
			defer proc.Release()
			ans := nextOf(t, td, run)
			if ans.State != NextPaused || len(td.spawned) != 0 || !strings.Contains(ans.Hint, "run "+run+" is paused (operator: crw flow operator)") ||
				strings.Contains(ans.Hint, "for an answer") {
				t.Fatalf("an operator pause with its process alive: %+v", ans)
			}
		})
	}
}
