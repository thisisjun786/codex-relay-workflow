package spawn

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/bound"
)

// CRW-1207 correction round: what the pre-merge evaluation found in the first build.

// d1: under native multi-agent V2 the task is ciphertext, so the hook cannot read a CRW-ROLE header in it. The orchestrator's role
// is then the one the plaintext task_name names (one of the six roles, or a role and an underscore suffix). Since CRW-1348 a name
// that names no role is no refusal: the spawn gets its agent_type's pair, with the ledger's unrecorded warning.
func TestOrchestratorCiphertextRoleFromTaskName(t *testing.T) {
	r := newFBRig(t)
	r.bindOrchestrator(fbThreadA)
	cipher := strconv.Quote(spawnV2Fernet())
	input := func(taskName string) string {
		name := ""
		if taskName != "-" {
			name = `"task_name":` + strconv.Quote(taskName) + `,`
		}
		return `{"agent_type":"explorer",` + name + `"fork_turns":"none","message":` + cipher + `}`
	}
	cases := []struct {
		name, taskName string
		want           bound.Pair
		role           string
	}{
		{"reviewer", "reviewer", fbReviewer, "reviewer"},
		{"reviewer with a suffix", "reviewer_diff_2", fbReviewer, "reviewer"},
		{"explorer", "explorer", fbExplorer, "explorer"},
		{"explorer with a suffix", "explorer_parser", fbExplorer, "explorer"},
		{"a name that says no role", "helper", fbExplorer, "explorer"},
		{"no task_name", "-", fbExplorer, "explorer"},
		{"role glued to a word", "reviewers", fbExplorer, "explorer"},
		{"role in capitals", "REVIEWER", fbExplorer, "explorer"},
		{"architect", "architect", fbArchitect, "architect"},
		{"architect with a suffix", "architect_plan", fbArchitect, "architect"},
		{"reviewer-final", "reviewer-final_gate", fbReviewerFinal, "reviewer-final"},
	}
	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out := r.spawn(fbThreadA, fmt.Sprintf("tool-%d", i), input(c.taskName))
			updated := r.expectAllowed(out)
			if got := fbPairOf(updated); got != c.want {
				t.Fatalf("pair %+v, want %+v", got, c.want)
			}
			if updated.Get("message") != spawnV2Fernet() {
				t.Fatalf("the hook changed the ciphertext: %v", updated.Get("message"))
			}
		})
	}
	// the decisions say which role each spawn was
	var roles []string
	for _, d := range r.decisions(fbThreadA) {
		if d.Decision == bound.DecisionAllowed {
			roles = append(roles, d.Role)
		}
	}
	if strings.Join(roles, ",") != "reviewer,reviewer,explorer,explorer,explorer,explorer,explorer,explorer,architect,architect,reviewer-final" {
		t.Fatalf("allowed roles %v", roles)
	}
	// an engine thread ignores the name: its helper is the explorer pair whatever it is called
	r.bindEngine(fbThreadB, fbCall1, "reviewer", 1)
	if got := fbPairOf(r.expectAllowed(r.spawn(fbThreadB, "tool-e", input("reviewer")))); got != fbExplorer {
		t.Fatalf("engine thread: %+v", got)
	}
}

// d3: the claim and everything the hook needs to find it again is durable before the spawn is allowed. Every directory entry the
// hook makes is flushed (the parent of a directory it made, the claim, its answer), and a flush that fails refuses the spawn; the
// same event delivered again once the fault is gone gets the allow.
func TestFlowBoundClaimEntriesDurableBeforeAllow(t *testing.T) {
	var seen []string
	old := bound.SyncEntry
	bound.SyncEntry = func(label string, d *os.File) error { seen = append(seen, label); return d.Sync() }
	t.Cleanup(func() { bound.SyncEntry = old })
	input := `{"agent_type":"explorer","message":"TASK: q"}`
	r := newFBRig(t)
	r.bindEngine(fbThreadA, fbCall1, "executor", 1)
	r.expectAllowed(r.spawn(fbThreadA, "tool-1", input))
	for _, want := range []string{"claims", fbThreadA, fbCall1, fbCall1 + ".answer", "decisions", fbThreadA + bound.FileSuffix} {
		if !strings.Contains(strings.Join(seen, " "), "entry:"+want) {
			t.Errorf("no flush of the entry %q; flushed %v", want, seen)
		}
	}
	if len(seen) == 0 {
		t.Fatal("nothing flushed")
	}
	labels := map[string]bool{}
	for _, l := range seen {
		labels[l] = true
	}
	for label := range labels {
		t.Run(label, func(t *testing.T) {
			r := newFBRig(t)
			r.bindEngine(fbThreadA, fbCall1, "executor", 1)
			failing := true
			bound.SyncEntry = func(l string, d *os.File) error {
				if failing && l == label {
					return fmt.Errorf("injected: %s", l)
				}
				return d.Sync()
			}
			out := r.spawn(fbThreadA, "tool-1", input)
			reason := r.expectRefused(out, bound.ReasonFlowBoundUnavailable)
			if !strings.Contains(reason, "injected") {
				t.Errorf("the refusal hides the failure: %q", reason)
			}
			for _, d := range r.decisions(fbThreadA) {
				if d.Decision == bound.DecisionAllowed {
					t.Fatalf("an allow was recorded after a failed flush: %+v", d)
				}
			}
			failing = false
			r.expectAllowed(r.spawn(fbThreadA, "tool-1", input)) // the same event, delivered again
			if got := r.spawn(fbThreadA, "tool-2", input); !strings.Contains(got, bound.ReasonNativeLimitRefused) {
				t.Errorf("a second spawn after the recovery: %q", got)
			}
		})
	}
}

// d4: a ledger cut short by a crash or a full disk keeps working. The cut tail was never a decision the hook answered (it answers
// after the whole line is written), so the next decision starts a clean line and the run's import reads the ledger whole; a
// line that is whole but is no decision is a ledger nobody can trust, and the spawn is refused rather than allowed unrecorded.
func TestFlowBoundCutLedgerTailRecovers(t *testing.T) {
	input := `{"agent_type":"explorer","message":"TASK: q"}`
	r := newFBRig(t)
	r.bindEngine(fbThreadA, fbCall1, "executor", 1)
	ledger := filepath.Join(r.dir, bound.DecisionsDir, fbThreadA+bound.FileSuffix)
	spawnHookMust(t, os.MkdirAll(filepath.Dir(ledger), 0o700))
	spawnHookMust(t, os.WriteFile(ledger, []byte(`{"run_id":"run-fb","thread_id":"`+fbThreadA+`","tool_use_id":"tool-0","decis`), 0o600))
	r.expectAllowed(r.spawn(fbThreadA, "tool-1", input))
	d := r.decisions(fbThreadA)
	if len(d) != 1 || d[0].ToolUseID != "tool-1" || d[0].Decision != bound.DecisionAllowed {
		t.Fatalf("the ledger after the cut tail: %+v", d)
	}
	raw, _ := os.ReadFile(ledger)
	if strings.Contains(string(raw), "decis\n") || !strings.HasSuffix(string(raw), "\n") || strings.Count(string(raw), "\n") != 1 {
		t.Fatalf("ledger bytes %q", raw)
	}

	r2 := newFBRig(t)
	r2.bindEngine(fbThreadA, fbCall1, "executor", 1)
	ledger2 := filepath.Join(r2.dir, bound.DecisionsDir, fbThreadA+bound.FileSuffix)
	spawnHookMust(t, os.MkdirAll(filepath.Dir(ledger2), 0o700))
	spawnHookMust(t, os.WriteFile(ledger2, []byte("not a decision\n"), 0o600))
	out := r2.spawn(fbThreadA, "tool-1", input)
	r2.expectRefused(out, bound.ReasonFlowBoundUnavailable)
	if raw, _ := os.ReadFile(ledger2); string(raw) != "not a decision\n" {
		t.Fatalf("the unreadable ledger was changed: %q", raw)
	}
}
