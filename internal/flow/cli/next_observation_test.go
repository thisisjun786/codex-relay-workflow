package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
)

func TestNextObservationPath(t *testing.T) {
	for _, mode := range []string{"", "active-observation", "event-driven-idle"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			w := newWorld(t)
			td := w.deps()
			run := "run-0123456789d5"
			sessionSpec(t, w, run)
			r := w.newRun(run, sessOwner)
			r.close()
			lock := holdAs(t, w, run, "_run")
			defer lock.Release()
			args := []string{"next", run, "--wait", "0", "--json", "--state", w.state}
			if mode != "" {
				args = append(args, "--observation-path", mode)
			}
			code, out, stderr := td.do(args...)
			if code != 0 {
				t.Fatalf("next: %d %s %s", code, out, stderr)
			}
			ans := decodeNext(t, out)
			if ans.State != NextRunning || len(ans.Steps) != 0 {
				t.Fatalf("want a live run with no steps: %+v", ans)
			}
			if mode == "event-driven-idle" {
				if !strings.Contains(ans.Hint, "end this turn") || !strings.Contains(ans.Hint, "relay") || strings.Contains(ans.Hint, "wait_agent(") || strings.Contains(ans.Hint, "--wait 600") {
					t.Fatalf("idle owner must yield to the run: %s", ans.Hint)
				}
			} else if strings.Contains(ans.Hint, "wait_agent(") || !strings.Contains(ans.Hint, "--wait 600 --json") {
				t.Fatalf("with no agents active observation waits on next alone: %s", ans.Hint)
			}
			// A delivered spawn must lead to native waiting, in either mode.
			if err := engine.NewFileStepBus(RunDir(w.state, run)).Publish(drainStep(run)); err != nil {
				t.Fatal(err)
			}
			code, out, stderr = td.do(args...)
			if code != 0 {
				t.Fatalf("next with spawn: %d %s %s", code, out, stderr)
			}
			ans = decodeNext(t, out)
			if len(ans.Steps) != 1 || ans.Steps[0].Then == "" {
				t.Fatalf("spawn and dispatch must still be handed out: %+v", ans)
			}
			if !strings.Contains(ans.Hint, "wait_agent(") || !strings.Contains(ans.Hint, "--wait 120 --json") || strings.Contains(ans.Hint, "alternate") {
				t.Fatalf("spawn must lead to native waiting and one completion read: %s", ans.Hint)
			}
		})
	}
}

func TestNextOpenAgentsWaitForCompletion(t *testing.T) {
	for _, mode := range []string{observationActive, observationEventIdle} {
		t.Run(mode, func(t *testing.T) {
			ans := NextAnswer{Run: "run-0123456789d5", State: NextRunning, observationPath: mode,
				Slots: NextSlots{Open: 2, Agents: []string{"agent-1", "agent-2"}}}
			hint := nextHint(ans, RunInfo{}, nil)
			for _, want := range []string{`wait_agent(targets=["agent-1","agent-2"]`, "longest timeout", "repeat the same wait without calling next", "When wait reports completion", "dispatched", "--wait 120 --json once"} {
				if !strings.Contains(hint, want) {
					t.Errorf("open agents lack %q: %s", want, hint)
				}
			}
			for _, forbidden := range []string{"end this turn", "relay", "alternate", "--wait 600"} {
				if strings.Contains(hint, forbidden) {
					t.Errorf("native wait must not suggest %q: %s", forbidden, hint)
				}
			}
			if mode == observationEventIdle && !strings.Contains(hint, "--observation-path event-driven-idle") {
				t.Fatalf("completion read lost the recorded mode: %s", hint)
			}
		})
	}
}

func TestNextBatchWaitUsesAgentsAfterItsTools(t *testing.T) {
	ans := NextAnswer{Run: "run-0123456789d5", State: NextWaitingSession,
		Slots: NextSlots{Limit: 6, Open: 1, Agents: []string{"finished-agent"}},
		Steps: []NextStep{{Step: engine.Step{Action: engine.StepClose, AgentID: "finished-agent"}}, {Step: drainStep("run-0123456789d5")}}}
	hint := nextHint(ans, RunInfo{}, []string{"finished-agent"})
	if !strings.Contains(hint, "wait_agent(targets=[<open agents>]") || strings.Contains(hint, `wait_agent(targets=["finished-agent"]`) {
		t.Fatalf("a close/spawn batch must wait on its resulting agents: %s", hint)
	}
}

func TestNextRejectsUnknownObservationPath(t *testing.T) {
	w := newWorld(t)
	td := w.deps()
	for _, mode := range []string{"relay", "blocked", ""} {
		code, _, stderr := td.do("next", "run-0123456789d5", "--observation-path", mode, "--state", w.state)
		if code != 2 || !strings.Contains(stderr, "event-driven-idle or active-observation") || len(td.spawned) != 0 {
			t.Fatalf("unknown path %q: %d %s, spawned %+v", mode, code, stderr, td.spawned)
		}
	}
}

func TestIdleHintKeepsOutstandingOwnerActions(t *testing.T) {
	for _, state := range []string{NextWaitingDecision, NextPaused, NextStarting, NextEnded} {
		ans := NextAnswer{Run: "run-0123456789d5", State: state, observationPath: observationEventIdle}
		hint := nextHint(ans, RunInfo{State: StatePaused, Paused: "operator"}, nil)
		if strings.Contains(hint, "end this turn") || strings.Contains(hint, "wait_agent(") {
			t.Errorf("%s still needs its owner action: %s", state, hint)
		}
	}
}

// This is a documentation contract, not evidence of a live relay wake.
func TestRunSkillSeparatesObservationPaths(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "plugins", "crw", "skills", "crw-run", "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	_, steps, ok := strings.Cut(string(raw), "**Take the steps.**")
	if !ok {
		t.Fatal("missing Take the steps")
	}
	steps, _, _ = strings.Cut(steps, "**What this thread handles.**")
	for _, want := range []string{"**Open subagents.**", "**Subagent completion.**", "**No open subagents and no step.**", "event-driven-idle", "active-observation", "--observation-path event-driven-idle", "--wait 120 --json", "end this turn", "relay", "longest timeout", "repeat the same wait", "without calling `flow next`", "Keep at most one `next` command in flight"} {
		if !strings.Contains(steps, want) {
			t.Errorf("Take the steps lacks %q", want)
		}
	}
	for _, forbidden := range []string{"alternate", "even while subagents work", "normal wait/next sequence"} {
		if strings.Contains(steps, forbidden) {
			t.Errorf("Take the steps still prescribes %q", forbidden)
		}
	}
}
