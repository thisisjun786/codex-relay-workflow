package scenario

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/execution"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/ops"
)

// seqOf is the sequence number of the first row the function accepts.
func seqOf(t *testing.T, l *Lab, what string, keep func(engine.Event) bool) int {
	t.Helper()
	for _, ev := range l.Events() {
		if keep(ev) {
			return ev.Seq
		}
	}
	t.Fatalf("no row for %s", what)
	return 0
}

// The first executor starts after an approved audit of the DESIGN, by a reviewer of another model family than the architect: the
// audit is recorded as a design_audit row before the first release, and the release before the first executor call.
func TestDesignAuditBeforeFirstExecutor(t *testing.T) {
	l := NewLab(t, twoNodes()...)
	mustRun(t, l)
	audit := seqOf(t, l, "approved audit", func(ev engine.Event) bool {
		return ev.Op == ops.OpDesignAudit && ev.Event == engine.EvCompleted && str(ev.Data, "state") == "approved"
	})
	release := seqOf(t, l, "release", func(ev engine.Event) bool { return ev.Op == ops.OpRelease && ev.Event == engine.EvCompleted })
	executor := seqOf(t, l, "executor call", func(ev engine.Event) bool {
		return ev.Kind == engine.KindCall && ev.Event == engine.EvPrepared && str(ev.Data, "role") == "executor"
	})
	ledger := seqOf(t, l, "ledger", func(ev engine.Event) bool { return ev.Op == ops.OpDesignLedger && ev.Event == engine.EvCompleted })
	if !(audit < ledger && ledger < release && release < executor) {
		t.Fatalf("order: audit %d, ledger %d, release %d, executor %d", audit, ledger, release, executor)
	}
	row := l.Rows(ops.OpDesignAudit)[0]
	arch, rev := str(row.Data, "architect_model"), str(row.Data, "reviewer_model")
	if arch == "" || rev == "" || execution.ModelFamily(arch) == execution.ModelFamily(rev) {
		t.Errorf("the audit was not independent: architect %q, auditor %q", arch, rev)
	}
	if l.Labels()[1] != "design-audit:r1" || l.Roles()[1] != "reviewer" {
		t.Errorf("calls %v", l.Labels())
	}
}

// An audit that finds the DESIGN wanting sends it back to the architect on its own thread; the revised DESIGN is audited again,
// and only its approval lets an executor start. The packet of the executor is built from the revised DESIGN.
func TestDesignAuditNeedsChangesReturnsToArchitect(t *testing.T) {
	l := NewLab(t, oneNode()...)
	revised := l.designFor()
	revised.Issues[0].Approach = "the approach after the audit"
	l.Sequence("^architect$", func(*Call) fakehostPlan { return Say(l.designFor()) })
	l.Sequence("^design-fix:r1$", func(*Call) fakehostPlan { return Say(revised) })
	l.Sequence("^design-audit:r\\d+$",
		func(*Call) fakehostPlan {
			return Say(map[string]any{"verdict": "needs_changes", "findings": []any{map[string]any{"ref": "CRW-A", "severity": "P1", "finding": "the approach names no test", "evidence": []string{}}}, "summary": "not yet"})
		},
		func(*Call) fakehostPlan {
			return Say(map[string]any{"verdict": "approved", "findings": []any{}, "summary": "now it holds"})
		})
	got := mustRun(t, l)
	if got.State != "closed" {
		t.Fatalf("state %q", got.State)
	}
	want := []string{"architect", "design-audit:r1", "design-fix:r1", "design-audit:r2", "build:A:executor", "review:A:r1", "reviewer-final:M1", "reflection:M1"}
	if labels := l.Labels(); !slices.Equal(labels, want) {
		t.Fatalf("calls %v, want %v", labels, want)
	}
	arch, fix := callNamed(t, l, "architect"), callNamed(t, l, "design-fix:r1")
	if fix.Resume != arch.ID || fix.Role != "architect" {
		t.Errorf("the revision continues %q (%s), want the architect's call %s", fix.Resume, fix.Role, arch.ID)
	}
	if text := textOfCall(l, fix.ID); !strings.Contains(text, "the approach names no test") {
		t.Errorf("the architect was not given the finding:\n%s", text)
	}
	states := []string{}
	for _, r := range l.Rows(ops.OpDesignAudit) {
		states = append(states, str(r.Data, "state"))
	}
	if !slices.Equal(states, []string{"needs_changes", "approved"}) {
		t.Errorf("audit rows %v", states)
	}
	if text := textOfCall(l, callNamed(t, l, "build:A:executor").ID); !strings.Contains(text, "the approach after the audit") {
		t.Errorf("the executor packet is not built from the revised DESIGN:\n%s", text)
	}
	// no executor before the approval
	approved := seqOf(t, l, "approved audit", func(ev engine.Event) bool {
		return ev.Op == ops.OpDesignAudit && ev.Event == engine.EvCompleted && str(ev.Data, "state") == "approved"
	})
	first := seqOf(t, l, "executor", func(ev engine.Event) bool {
		return ev.Kind == engine.KindCall && ev.Event == engine.EvPrepared && str(ev.Data, "role") == "executor"
	})
	if first < approved {
		t.Errorf("an executor started (row %d) before the audit approved (row %d)", first, approved)
	}
}

// The release of a lane is recorded with the DESIGN revision and digest it was made under; the executor's call carries them in its
// input snapshot and its packet is built from that revision, and a later revision of the ledger would not change it.
func TestLanePacketFromReleaseRevision(t *testing.T) {
	l := NewLab(t, twoNodes()...)
	mustRun(t, l)
	ledger := l.Rows(ops.OpDesignLedger)
	if len(ledger) != 1 {
		t.Fatalf("%d ledger rows", len(ledger))
	}
	digest := str(ledger[0].Data, "digest")
	for _, node := range []string{"A", "B"} {
		var rel engine.Event
		for _, r := range l.Rows(ops.OpRelease) {
			if str(r.Data, "node") == node {
				rel = r
			}
		}
		if str(rel.Data, "revision") != "r0" || str(rel.Data, "design_digest") != digest {
			t.Errorf("release of %s: %v", node, rel.Data)
		}
		build := callNamed(t, l, "build:"+node+":executor")
		prep := l.Prepared(build.ID)
		snap := str(prep.Data, "input_snapshot")
		if !strings.Contains(snap, "design=r0:"+digest) {
			t.Errorf("the input snapshot of %s is %q, want the design revision of the release", node, snap)
		}
		if text := textOfCall(l, build.ID); !strings.Contains(text, "DESIGN REVISION: r0 "+digest) || !strings.Contains(text, "approach of "+node) {
			t.Errorf("the packet of %s is not built from the release revision:\n%s", node, text)
		}
	}
}

// ---- explorers ----

func openQuestions(n int) []ops.OpenQuestion {
	var out []ops.OpenQuestion
	for i := 1; i <= n; i++ {
		out = append(out, ops.OpenQuestion{ID: fmt.Sprintf("q%d", i), Question: fmt.Sprintf("who owns module number %d?", i), Scope: "repo", FilesNeeded: []string{"a.go", "b.go", "c.go", "d.go", "e.go"}})
	}
	return out
}

// maxOpen is the most calls of a role that were open at once (prepared and not yet ended).
func maxOpen(l *Lab, role string) int {
	open, most := map[string]bool{}, 0
	for _, ev := range l.Events() {
		if ev.Kind != engine.KindCall || ev.Op != "agent" {
			continue
		}
		switch ev.Event {
		case engine.EvPrepared:
			if str(ev.Data, "role") == role {
				open[ev.CallID] = true
			}
		case engine.EvCompleted, engine.EvNullModel, engine.EvHeld, engine.EvRefused, engine.EvOutcomeUnknown, engine.EvSuperseded:
			delete(open, ev.CallID)
		}
		most = max(most, len(open))
	}
	return most
}

// The thresholds of the explorer signals: a question with at most three files, or one repeated in the milestone, costs no
// explorer; the rest run at most three at a time, at most six for the milestone, and the questions beyond the sixth go to the
// orchestrator thread as a design question.
func TestExplorerSignalAndInlineThresholds(t *testing.T) {
	l := NewLab(t, oneNode()...)
	d := l.designFor()
	d.OpenQuestions = append(openQuestions(8),
		ops.OpenQuestion{ID: "small1", Question: "is the flag documented?", Scope: "docs", FilesNeeded: []string{"README.md"}},
		ops.OpenQuestion{ID: "small2", Question: "what does the helper return?", Scope: "pkg", FilesNeeded: []string{"x.go", "y.go", "z.go"}},
		ops.OpenQuestion{ID: "dup", Question: "Who owns   module number 1?", Scope: "repo", FilesNeeded: []string{"a.go", "b.go", "c.go", "d.go", "e.go"}})
	l.Design = &d
	l.On("^explore:", func(c *Call) *fakehostPlan {
		p := Say(map[string]any{"answer": "answer of the explorer", "evidence": []string{"README.md"}, "verified": []string{}, "inferred": []string{}, "unknown": []string{}})
		p.Delay = 40_000_000
		return &p
	})
	got := mustRun(t, l)
	if got.State != "closed" {
		t.Fatalf("state %q", got.State)
	}
	explorers := l.CallsOf("explorer")
	if len(explorers) != ops.ExplorersPerMilestone {
		t.Errorf("%d explorer calls, want %d (the cap of the milestone)", len(explorers), ops.ExplorersPerMilestone)
	}
	if most := maxOpen(l, "explorer"); most > ops.ExplorerConcurrency || most < 2 {
		t.Errorf("%d explorers were open at once, want 2 to %d", most, ops.ExplorerConcurrency)
	}
	counts := map[string]int{}
	for _, r := range l.SpawnRows() {
		if strings.HasPrefix(r.Rule, "SP-EXPL-1") || r.Rule == ops.RuleInl1 || r.Rule == ops.RuleInl2 {
			counts[r.Rule+"/"+r.Decision]++
		}
	}
	want := map[string]int{"SP-EXPL-1/spawn": 6, "SP-EXPL-1/orchestrator": 2, "SP-INL-1/inline": 2, "SP-INL-2/reuse": 1}
	for k, n := range want {
		if counts[k] != n {
			t.Errorf("decisions %s = %d, want %d (all: %v)", k, counts[k], n, counts)
		}
	}
	var asked string
	for _, ev := range l.FlowRows(engine.EvEscalation) {
		if str(ev.Data, "reason") == "design_question" {
			asked = str(ev.Data, "detail")
		}
	}
	if !strings.Contains(asked, "module number 7") || !strings.Contains(asked, "module number 8") || strings.Contains(asked, "module number 6") {
		t.Errorf("the design questions for the orchestrator: %q", asked)
	}
}

// A verdict with a defect of kind evidence_missing sends its evidence_question to an explorer before the node's next round; the
// answer goes into the fix packet and into the next reviewer packet, and no model interprets the finding again.
func TestEvidenceMissingSpawnsExplorerThenReviewerPacket(t *testing.T) {
	l := NewLab(t, oneNode()...)
	reviews(l, "A", Verdict(false, EvidenceDefect("e1", "does the retry loop stop at the deadline?", "retry.go", "deadline.go")), Verdict(true))
	l.On("^explore:A:", func(*Call) *fakehostPlan {
		p := Say(map[string]any{"answer": "the loop stops at the deadline in retry.go:44", "evidence": []string{"retry.go:44"}, "verified": []string{"retry.go"}, "inferred": []string{}, "unknown": []string{}})
		return &p
	})
	got := mustRun(t, l)
	if got.State != "closed" {
		t.Fatalf("state %q", got.State)
	}
	labels := l.Labels()
	if len(labels) < 7 || !strings.HasPrefix(labels[4], "explore:A:") || labels[5] != "fix:A:executor:r1" || !strings.HasPrefix(labels[6], "review:A:") {
		t.Fatalf("calls %v: want the explorer between the review and the fix, then the next review", labels)
	}
	answer := "the loop stops at the deadline in retry.go:44"
	fix, next := callNamed(t, l, "fix:A:executor:r1"), callNamed(t, l, labels[6])
	if text := textOfCall(l, fix.ID); !strings.Contains(text, answer) {
		t.Errorf("the fix packet does not carry the explorer's answer:\n%s", text)
	}
	if text := textOfCall(l, next.ID); !strings.Contains(text, answer) {
		t.Errorf("the next reviewer packet does not carry the explorer's answer:\n%s", text)
	}
	if text := textOfCall(l, callNamed(t, l, labels[4]).ID); !strings.Contains(text, "does the retry loop stop at the deadline?") || !strings.Contains(text, "retry.go") {
		t.Errorf("the explorer was not given the evidence question and its files:\n%s", text)
	}
	found := false
	for _, r := range l.SpawnRows() {
		if r.Rule == ops.RuleExpl3 && r.Decision == "spawn" && r.Node == "A" {
			found = true
		}
	}
	if !found {
		t.Errorf("no SP-EXPL-3 spawn decision: %+v", l.SpawnRows())
	}
	noTierStep(t, l)
}
