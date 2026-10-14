package scenario

import (
	"fmt"
	"io/fs"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/execution"
	flowcli "github.com/thisisjun786/codex-relay-workflow/internal/flow/cli"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/ops"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/roles"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/workflows/lib"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/decisions"
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

// The thresholds of the explorer signals: a question repeated in the milestone costs no explorer; every other open question, one
// with at most three files among them (an open question of a DESIGN is never the asker's to answer, CRW-1347), goes to an explorer,
// at most three at a time and at most six for the milestone, and the questions beyond the sixth go to the orchestrator thread as a
// design question.
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
	want := map[string]int{"SP-EXPL-1/spawn": 6, "SP-EXPL-1/orchestrator": 4, "SP-INL-1/inline": 0, "SP-INL-2/reuse": 1}
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
	if !strings.Contains(asked, "module number 7") || !strings.Contains(asked, "module number 8") || strings.Contains(asked, "module number 6") ||
		!strings.Contains(asked, "is the flag documented?") || !strings.Contains(asked, "what does the helper return?") {
		t.Errorf("the design questions for the orchestrator: %q", asked)
	}
}

// The open questions of a DESIGN on the whole stack (CRW-1347): a question with one file goes to an explorer, not to the architect
// whose turn has ended; a question whose scope carries the notation of an out-of-authority kind goes to no explorer and is asked
// of the orchestrator in the design_question escalation, with its scope, while the milestone goes on to its close; a question
// with an unknown notation goes to an explorer. The design audit packet shows each question's route and what it got, and the
// audit of a re-plan routes its open questions the same way, reusing the answer an explorer gave in the run.
func TestOpenQuestionRoutesThroughDesignAudit(t *testing.T) {
	l := NewLab(t, twoNodes()...)
	d := l.designFor()
	d.OpenQuestions = []ops.OpenQuestion{
		{ID: "small", Question: "is the flag documented?", Scope: "docs", FilesNeeded: []string{"README.md"}},
		{ID: "policy", Question: "does the project ship the flag in 1.0?", Scope: "authority:policy: the release scope", FilesNeeded: []string{}},
		{ID: "odd", Question: "who pays for the runner?", Scope: "authority:budget", FilesNeeded: []string{"ci.yml"}},
	}
	l.Design = &d
	explained := map[string]string{"is the flag documented?": "yes, README.md says so", "who pays for the runner?": "the repository does not say"}
	l.On("^explore:", func(c *Call) *fakehostPlan {
		for q, a := range explained {
			if strings.Contains(c.Text, "QUESTION: "+q) {
				p := Say(map[string]any{"answer": a, "evidence": []string{"README.md"}, "verified": []string{}, "inferred": []string{}, "unknown": []string{}})
				return &p
			}
		}
		t.Errorf("an explorer was asked another question:\n%s", c.Text)
		return nil
	})
	l.On("^reviewer-final:M1$", func(*Call) *fakehostPlan { p := Say(VerdictFinal(false)); return &p })
	got := runReplan(t, l)
	if got.State != "replanned" {
		t.Fatalf("%+v; calls %v", got, l.Labels())
	}
	// two explorers, both before the first audit: the small question and the one with an unknown notation; none for the authority
	// question, and none in the re-plan, whose questions were answered in the run or go to the orchestrator again
	explorers := l.CallsOf("explorer")
	if len(explorers) != 2 {
		t.Errorf("%d explorer calls, want 2: %v", len(explorers), l.Labels())
	}
	for _, c := range explorers {
		if text := textOfCall(l, c.ID); strings.Contains(text, "ship the flag") {
			t.Errorf("an explorer was asked the authority question:\n%s", text)
		}
	}
	for _, r := range l.SpawnRows() {
		if r.Rule == ops.RuleInl1 {
			t.Errorf("an open question of the DESIGN took the inline route: %+v", r)
		}
	}
	type key struct{ decision, reason string }
	counts := map[key]int{}
	for _, r := range spawnRowsOf(l, ops.RuleExpl1, "spawn") {
		counts[key{r.Decision, r.Reason}]++
	}
	for _, r := range spawnRowsOf(l, ops.RuleExpl1, "orchestrator") {
		counts[key{r.Decision, r.Reason}]++
	}
	for _, r := range spawnRowsOf(l, ops.RuleInl2, "reuse") {
		counts[key{r.Decision, r.Reason}]++
	}
	if want := map[key]int{{"spawn", ""}: 2, {"orchestrator", "out_of_authority"}: 1, {"orchestrator", "asked_in_run"}: 1, {"reuse", "answered_in_run"}: 2}; !reflect.DeepEqual(counts, want) {
		t.Errorf("the routes of the open questions %v, want %v (the first audit and the re-plan, which asks nothing again, CRW-1366)", counts, want)
	}
	// each audit packet shows every question with its route and what it got
	for _, label := range []string{"design-audit:r1", "design-audit:replan-close:r1"} {
		text := textOfCall(l, callNamed(t, l, label).ID)
		answers := text[strings.Index(text, "ANSWERS TO THE OPEN QUESTIONS"):]
		reason := "out_of_authority"
		if label != "design-audit:r1" {
			reason = "asked_in_run"
		}
		for _, want := range []string{
			"policy: does the project ship the flag in 1.0?\n  route: asked of the orchestrator (" + reason + ", authority:policy)",
			"design_question escalation",
			"the milestone goes on",
		} {
			if !strings.Contains(answers, want) {
				t.Errorf("%s: the answers lack %q:\n%s", label, want, answers)
			}
		}
		explorerRoute := "route: an explorer answered it (call "
		if label != "design-audit:r1" {
			explorerRoute = "route: reused: the answer an earlier question of the run got"
		}
		for _, want := range []string{
			"small: is the flag documented?\n  " + explorerRoute, "answer: yes, README.md says so",
			"odd: who pays for the runner?\n  " + explorerRoute, "answer: the repository does not say",
		} {
			if !strings.Contains(answers, want) {
				t.Errorf("%s: the answers lack %q:\n%s", label, want, answers)
			}
		}
		if strings.Contains(answers, "inline") || strings.Contains(answers, `"answer":""`) {
			t.Errorf("%s: a question reached the audit with an empty answer and no route:\n%s", label, answers)
		}
	}
	// the authority question is asked of the orchestrator, with its scope, once in the run (the re-plan's DESIGN raises it again
	// and the run does not ask it again, CRW-1366); nothing stops the milestone, which closed and re-planned
	var asked []string
	for _, ev := range l.FlowRows(engine.EvEscalation) {
		if str(ev.Data, "reason") == "design_question" {
			asked = append(asked, str(ev.Data, "detail"))
		}
	}
	if len(asked) != 1 {
		t.Fatalf("design_question escalations %q, want one for the run", asked)
	}
	for _, detail := range asked {
		if detail != "[authority:policy: the release scope] does the project ship the flag in 1.0?" {
			t.Errorf("the design question for the orchestrator: %q", detail)
		}
	}
	if holds := l.Rows(ops.OpHold); len(holds) != 0 {
		t.Errorf("an open question made a hold: %+v", holds)
	}
}

// policyQuestion is the authority question of the CRW-1366 tests.
var policyQuestion = ops.OpenQuestion{ID: "policy", Question: "does the project ship the flag in 1.0?", Scope: "authority:policy", FilesNeeded: []string{}}

func designAuditNeedsChanges(*Call) fakehostPlan {
	return Say(map[string]any{"verdict": "needs_changes", "findings": []any{map[string]any{"ref": "D1", "severity": "P2", "finding": "state the order of the nodes", "evidence": []string{}}}, "summary": "one thing to fix"})
}

// designQuestions are the design_question escalations of the run, in order.
func designQuestions(l *Lab) []engine.Event {
	var out []engine.Event
	for _, ev := range l.FlowRows(engine.EvEscalation) {
		if str(ev.Data, "reason") == "design_question" {
			out = append(out, ev)
		}
	}
	return out
}

// answersOf is the text of the ANSWERS section of the packet of a design audit call.
func answersOf(t *testing.T, l *Lab, label string) string {
	t.Helper()
	text := textOfCall(l, callNamed(t, l, label).ID)
	i := strings.Index(text, "ANSWERS TO THE OPEN QUESTIONS")
	if i < 0 {
		t.Fatalf("%s: no answers section:\n%s", label, text)
	}
	return text[i:]
}

// CRW-1366 d1: an authority question of a DESIGN that is never approved is still asked of the orchestrator. The ask is raised in
// the round that routed it, before the audit of that round and before the design_audit_rounds escalation, and it stops nothing:
// the rounds after it run, and the later rounds do not ask the same question again.
func TestAuthorityQuestionIsAskedWhenTheAuditRoundsRunOut(t *testing.T) {
	l := NewLab(t, oneNode()...)
	d := l.designFor()
	d.OpenQuestions = []ops.OpenQuestion{policyQuestion}
	l.Design = &d
	l.Sequence("^design-audit:r\\d+$", designAuditNeedsChanges, designAuditNeedsChanges, designAuditNeedsChanges)
	got := mustRunIncomplete(t, l)
	if got.State != "escalated" {
		t.Fatalf("state %q, want the design_audit_rounds escalation; calls %v", got.State, l.Labels())
	}
	reasons := escalationReasons(l)
	if !slices.Equal(reasons, []string{"design_question", "design_audit_rounds"}) {
		t.Fatalf("escalations %v, want the design question once and then design_audit_rounds", reasons)
	}
	ask := designQuestions(l)[0]
	if detail := str(ask.Data, "detail"); detail != "[authority:policy] does the project ship the flag in 1.0?" || str(ask.Data, "ask") != "design" || str(ask.Data, "node") != "" {
		t.Errorf("the design question: %v", ask.Data)
	}
	if r1 := callNamed(t, l, "design-audit:r1"); ask.Seq > r1.Seq {
		t.Errorf("the ask is at seq %d, after the audit of its round at %d", ask.Seq, r1.Seq)
	}
	for _, label := range []string{"design-audit:r2", "design-audit:r3"} {
		if c := callNamed(t, l, label); c.Seq < ask.Seq {
			t.Errorf("%s at seq %d before the ask at %d", label, c.Seq, ask.Seq)
		}
	}
	if holds := l.Rows(ops.OpHold); len(holds) != 0 {
		t.Errorf("the ask made a hold: %+v", holds)
	}
	if answers := answersOf(t, l, "design-audit:r1"); !strings.Contains(answers, "policy: does the project ship the flag in 1.0?\n  route: asked of the orchestrator (out_of_authority, authority:policy): no answer in this audit; the run raised it in the design_question escalation of this round") {
		t.Errorf("design-audit:r1 answers:\n%s", answers)
	}
	for _, label := range []string{"design-audit:r2", "design-audit:r3"} {
		if answers := answersOf(t, l, label); !strings.Contains(answers, "policy: does the project ship the flag in 1.0?\n  route: asked of the orchestrator (asked_in_run, authority:policy)") {
			t.Errorf("%s answers:\n%s", label, answers)
		}
	}
}

// CRW-1366 d1 and d2: the orchestrator's answer to the ask of a round is the answer of the question in the next round, and a reused
// answer names the route it came from: the repeat of the question in its first round shows that the first went to the orchestrator,
// and the next round shows the orchestrator's answer with that route. The approved DESIGN asks nothing again.
func TestOrchestratorAnswerIsUsedOnTheNextAuditRound(t *testing.T) {
	l := NewLab(t, oneNode()...)
	d := l.designFor()
	again := policyQuestion
	again.ID = "again"
	d.OpenQuestions = []ops.OpenQuestion{policyQuestion, again}
	l.Design = &d
	l.On("^design-audit:r1$", func(c *Call) *fakehostPlan {
		for _, ev := range designQuestions(l) {
			l.apply(ops.EvDecisionApplied, map[string]any{"ask": str(ev.Data, "escalation_id"), "node": "", "decision": map[string]any{"id": "dec-1", "text": "1.0 ships without the flag"}})
		}
		p := designAuditNeedsChanges(c)
		return &p
	})
	if got := mustRun(t, l); got.State != "closed" {
		t.Fatalf("state %q; calls %v", got.State, l.Labels())
	}
	if asks := designQuestions(l); len(asks) != 1 {
		t.Fatalf("%d design_question escalations, want the ask of the first round only", len(asks))
	}
	r1 := answersOf(t, l, "design-audit:r1")
	for _, want := range []string{
		"policy: does the project ship the flag in 1.0?\n  route: asked of the orchestrator (out_of_authority, authority:policy)",
		"again: does the project ship the flag in 1.0?\n  route: reused: the same question as another of this DESIGN (original route: orchestrator)",
	} {
		if !strings.Contains(r1, want) {
			t.Errorf("design-audit:r1 answers lack %q:\n%s", want, r1)
		}
	}
	r2 := answersOf(t, l, "design-audit:r2")
	if want := "policy: does the project ship the flag in 1.0?\n  route: reused: the answer the orchestrator gave in the design_question escalation of the run (original route: orchestrator)\n  answer: 1.0 ships without the flag"; !strings.Contains(r2, want) {
		t.Errorf("design-audit:r2 answers lack %q:\n%s", want, r2)
	}
	// the record: each reused answer carries the route it came from
	records := l.Rows(ops.OpExploreRecord)
	if len(records) != 2 {
		t.Fatalf("%d crw.exploreRecord rows, want one for each round", len(records))
	}
	type rec struct{ route, reason, origin, answer string }
	want := [][]rec{
		{{"orchestrator", "out_of_authority", "", ""}, {"reuse", "same_question_in_round", "orchestrator", ""}},
		{{"reuse", "answered_by_orchestrator", "orchestrator", "1.0 ships without the flag"}, {"reuse", "same_question_in_round", "orchestrator", "1.0 ships without the flag"}},
	}
	for i, row := range records {
		list, _ := row.Data["answers"].([]any)
		var gotRecs []rec
		for _, item := range list {
			m, _ := item.(map[string]any)
			gotRecs = append(gotRecs, rec{str(m, "route"), str(m, "reason"), str(m, "origin"), str(m, "answer")})
		}
		if !slices.Equal(gotRecs, want[i]) {
			t.Errorf("round %d recorded %+v, want %+v", i+1, gotRecs, want[i])
		}
	}
}

// CRW-1366 d1b: a re-plan whose DESIGN raises the authority question the run asked already does not ask it again. The run knows the
// question by its digest from the escalation row it wrote (its evidence names the digests it asked), which it reads and never
// rewrites; the re-plan's route says the question was asked in the run.
func TestReplanAsksTheSameAuthorityQuestionOnce(t *testing.T) {
	l := NewLab(t, twoNodes()...)
	d := l.designFor()
	d.OpenQuestions = []ops.OpenQuestion{policyQuestion}
	l.Design = &d
	l.On("^reviewer-final:M1$", func(*Call) *fakehostPlan { p := Say(VerdictFinal(false)); return &p })
	if got := runReplan(t, l); got.State != "replanned" {
		t.Fatalf("%+v; calls %v", got, l.Labels())
	}
	asks := designQuestions(l)
	if len(asks) != 1 {
		t.Fatalf("%d design_question escalations, want one for the run", len(asks))
	}
	ev, _ := asks[0].Data["evidence"].(map[string]any)
	if qs, _ := ev["questions"].([]any); len(qs) != 1 || qs[0] != ops.QuestionDigest(policyQuestion.Question) {
		t.Errorf("the ask's evidence %v does not name the digest of the question it asked", asks[0].Data["evidence"])
	}
	var reasons []string
	for _, r := range spawnRowsOf(l, ops.RuleExpl1, "orchestrator") {
		reasons = append(reasons, r.Reason)
	}
	if !slices.Equal(reasons, []string{"out_of_authority", "asked_in_run"}) {
		t.Errorf("the routes of the authority question %v, want the first ask and the re-plan's memory of it", reasons)
	}
	if answers := answersOf(t, l, "design-audit:replan-close:r1"); !strings.Contains(answers, "policy: does the project ship the flag in 1.0?\n  route: asked of the orchestrator (asked_in_run, authority:policy): no answer in this audit; the run raised it in an earlier design_question escalation and does not ask it again") {
		t.Errorf("the re-plan's audit answers:\n%s", answers)
	}
}

// designAsks are the questions of the run's design asks as `crw flow answer` and the digest read them (flowcli.Questions).
func designAsks(l *Lab) []flowcli.Question {
	var out []flowcli.Question
	for _, q := range flowcli.Questions(l.Events()) {
		if q.Ask == "design" {
			out = append(out, q)
		}
	}
	return out
}

// judgeDesignAsk judges, as `crw flow answer` does (flowcli.Judge), a manager's answer to the design ask q of the lab's run.
func judgeDesignAsk(l *Lab, q flowcli.Question, decision, text string) *flowcli.Refusal {
	run := flowcli.RunRecord{RunID: l.RunID, Owner: "owner", PlanID: "plan-1366", PlanRevision: 1}
	b := flowcli.Binding{Source: flowcli.SourceQuestion, Run: l.RunID, Node: q.Node, Question: q.ID, Revision: q.Revision, Attempt: q.Attempt, PlanBasis: flowcli.PlanBasis(run.PlanID, run.PlanRevision)}
	rec := decisions.Record{DecisionID: decision, State: decisions.StateAnswered, Source: decisions.Source{Kind: b.Source, Ref: flowcli.BindingRef(b)},
		Blocking:  []decisions.Blocking{{Kind: decisions.BlockingPlanNode, Ref: b.Run + "/" + b.Node}},
		Authority: decisions.Authority{Kind: decisions.AuthorityDelegatedManagement}, AnsweredBy: "manager-task", AnsweredVia: decisions.ViaManagementMessage, AnswerText: text}
	_, refusal := flowcli.Judge(rec, l.Events(), run, flowcli.Claim{Decision: decision, Run: l.RunID, Node: q.Node, Question: q.ID, Revision: q.Revision, By: "owner"})
	return refusal
}

// askedIn are the question digests a design_question escalation names in its evidence.
func askedIn(ev engine.Event) []string {
	evidence, _ := ev.Data["evidence"].(map[string]any)
	list, _ := evidence["questions"].([]any)
	var out []string
	for _, d := range list {
		s, _ := d.(string)
		out = append(out, s)
	}
	return out
}

// CRW-1366 (evaluation d1): only the latest design ask of a run is current, the one `crw flow answer` accepts and the digest lists
// as waiting, so a new ask carries the questions the run asked already that no answer has reached and the DESIGN still raises.
// Round 1 asks the policy question; the revised DESIGN raises it again with a new authority question, and the ask of round 2 names
// both: the earlier ask is replaced, and the current one is answerable through the answer path and holds the first question too.
func TestANewAskCarriesTheAuthorityQuestionsStillUnanswered(t *testing.T) {
	l := NewLab(t, oneNode()...)
	d := l.designFor()
	d.OpenQuestions = []ops.OpenQuestion{policyQuestion}
	l.Design = &d
	revised := l.designFor()
	env := ops.OpenQuestion{ID: "env", Question: "may the lane use the shared runner?", Scope: "authority:environment", FilesNeeded: []string{}}
	revised.OpenQuestions = []ops.OpenQuestion{policyQuestion, env}
	l.Sequence("^design-fix:r1$", func(*Call) fakehostPlan { return Say(revised) })
	l.Sequence("^design-audit:r\\d+$", designAuditNeedsChanges, func(*Call) fakehostPlan {
		return Say(map[string]any{"verdict": "approved", "findings": []any{}, "summary": "now it holds"})
	})
	if got := mustRun(t, l); got.State != "closed" {
		t.Fatalf("state %q; calls %v", got.State, l.Labels())
	}
	rows := designQuestions(l)
	if len(rows) != 2 {
		t.Fatalf("%d design_question escalations, want the ask of each round that raised a new authority question", len(rows))
	}
	if detail := str(rows[1].Data, "detail"); detail != "[authority:policy] does the project ship the flag in 1.0? | [authority:environment] may the lane use the shared runner?" {
		t.Errorf("the ask of round 2 is %q, want the unanswered policy question carried with the new one", detail)
	}
	if got, want := askedIn(rows[1]), []string{ops.QuestionDigest(policyQuestion.Question), ops.QuestionDigest(env.Question)}; !slices.Equal(got, want) {
		t.Errorf("the ask of round 2 names the digests %v, want %v", got, want)
	}
	asks := designAsks(l)
	if len(asks) != 2 || asks[0].Current || !asks[1].Current || asks[1].Applied != "" || asks[1].ID != str(rows[1].Data, "escalation_id") {
		t.Fatalf("the design asks as the answer path reads them: %+v", asks)
	}
	if r := judgeDesignAsk(l, asks[0], "dec-old", "1.0 ships without the flag"); r == nil || r.Code != flowcli.RefusedStaleRevison {
		t.Errorf("an answer to the replaced ask: %v, want %s", r, flowcli.RefusedStaleRevison)
	}
	if r := judgeDesignAsk(l, asks[1], "dec-cur", "1.0 ships without the flag; the shared runner is fine"); r != nil {
		t.Errorf("an answer to the current ask, which holds both questions, is refused: %v", r)
	}
}

// CRW-1366 (evaluation d1): the ask of the questions beyond the explorer cap, raised when the DESIGN is approved, replaces the ask of
// the authority question of the same round, so it carries that question too.
func TestTheExplorerCapAskCarriesTheAuthorityQuestionOfItsRound(t *testing.T) {
	l := NewLab(t, oneNode()...)
	d := l.designFor()
	d.OpenQuestions = append(openQuestions(ops.ExplorersPerMilestone+1), policyQuestion)
	l.Design = &d
	if got := mustRun(t, l); got.State != "closed" {
		t.Fatalf("state %q; calls %v", got.State, l.Labels())
	}
	rows := designQuestions(l)
	if len(rows) != 2 {
		t.Fatalf("%d design_question escalations, want the authority ask of the round and the explorer-cap ask of the approval", len(rows))
	}
	capped := openQuestions(ops.ExplorersPerMilestone + 1)[ops.ExplorersPerMilestone]
	if detail := str(rows[1].Data, "detail"); detail != "[repo] "+capped.Question+" | [authority:policy] does the project ship the flag in 1.0?" {
		t.Errorf("the approval's ask is %q, want the capped question and the authority question of the round", detail)
	}
	if got, want := askedIn(rows[1]), []string{ops.QuestionDigest(capped.Question), ops.QuestionDigest(policyQuestion.Question)}; !slices.Equal(got, want) {
		t.Errorf("the approval's ask names the digests %v, want %v", got, want)
	}
	asks := designAsks(l)
	if len(asks) != 2 || !asks[1].Current {
		t.Fatalf("the design asks as the answer path reads them: %+v", asks)
	}
	if r := judgeDesignAsk(l, asks[1], "dec-cap", "module 7 is the platform team's; 1.0 ships without the flag"); r != nil {
		t.Errorf("an answer to the current ask is refused: %v", r)
	}
}

// The scope notations the router reads (ops.AuthorityScopes) are the ones the architect overlay gives the architect and the design
// audit packet gives the auditor, each beside its kind.
func TestAuthorityNotationsMatchTheRouter(t *testing.T) {
	l := NewLab(t, oneNode()...)
	mustRun(t, l)
	overlay, err := roles.Overlay("architect", roles.Options{})
	if err != nil {
		t.Fatal(err)
	}
	audit := textOfCall(l, callNamed(t, l, "design-audit:r1").ID)
	kinds := []string{"an exception to a rule of the repository", "the record of another issue", "a policy decision", "a prerequisite of the execution environment"}
	if len(kinds) != len(ops.AuthorityScopes) {
		t.Fatalf("%d notations for %d kinds", len(ops.AuthorityScopes), len(kinds))
	}
	for where, text := range map[string]string{"the architect overlay": overlay, "the design audit packet": audit} {
		for i, n := range ops.AuthorityScopes {
			line := ""
			for _, ln := range strings.Split(text, "\n") {
				if strings.Contains(ln, kinds[i]) && strings.HasPrefix(strings.TrimSpace(ln), "- ") {
					line = ln
				}
			}
			if !strings.Contains(line, "`"+n+"`") {
				t.Errorf("%s does not give the notation `%s` beside %q: %q", where, n, kinds[i], line)
			}
		}
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

// ---- the executor contract and the authority boundary of the DESIGN (CRW-1321) ----

// The architect overlay and the design audit packet both state the executor contract the run enforces, and the facts they state
// are the run's: the commit of a node is made by the run with the message `crw <node> <call>`, a blank line and the request id,
// and a RECEIPT has exactly the fields each names - no field for a commit, a commit message or an exception's reason. Both copies
// are held to ops.Receipt and to the run's commit, so neither can drift from the run or from the other.
func TestArchitectContractMatchesTheRun(t *testing.T) {
	l := NewLab(t, oneNode()...)
	mustRun(t, l)
	overlay, err := roles.Overlay("architect", roles.Options{})
	if err != nil {
		t.Fatal(err)
	}
	commits := l.Rows(ops.OpCommit)
	if len(commits) == 0 {
		t.Fatal("no commit row")
	}
	row := commits[0]
	call := str(row.Data, "call")
	msg := strings.TrimRight(l.Git("log", "-1", "--format=%B", str(row.Data, "commit")), "\n")
	head, reqID, ok := strings.Cut(msg, "\n\n")
	short := call
	if len(short) > 12 {
		short = short[:12]
	}
	if !ok || head != "crw A "+short || reqID == "" || strings.ContainsAny(reqID, "\n ") {
		t.Errorf("the run's commit message is %q, not `crw <node> <call>`, a blank line and the request id", msg)
	}
	audit := textOfCall(l, callNamed(t, l, "design-audit:r1").ID)
	for where, text := range map[string]string{"the architect overlay": overlay, "the design audit packet": audit} {
		if !strings.Contains(text, "fixed message `crw <node> <call>`") || !strings.Contains(text, "request id") {
			t.Errorf("%s does not state the run's commit message (`crw <node> <call>` and the request id)", where)
		}
	}
	// The sentence of each that lists the fields names each field of ops.Receipt and nothing the RECEIPT does not have.
	named := regexp.MustCompile("RECEIPT has exactly these fields: ([^.\n]*)").FindStringSubmatch(overlay)
	if named == nil {
		t.Fatalf("the architect overlay does not list the RECEIPT fields:\n%s", overlay)
	}
	receiptNames(t, "the architect overlay", named[1], regexp.MustCompile("`([a-z_]+)`"))
	listed := regexp.MustCompile(`reports a RECEIPT \(([^)]*)\)`).FindStringSubmatch(audit)
	if listed == nil {
		t.Fatalf("the design audit packet does not list the RECEIPT fields:\n%s", audit)
	}
	receiptNames(t, "the design audit packet", listed[1], regexp.MustCompile(`([a-z_]+)`))
}

// receiptNames holds the field list a text gives for the RECEIPT to the json fields of ops.Receipt: each named, nothing else.
func receiptNames(t *testing.T, where, list string, name *regexp.Regexp) {
	t.Helper()
	fields := reflect.TypeOf(ops.Receipt{})
	want := map[string]bool{}
	for i := 0; i < fields.NumField(); i++ {
		tag, _, _ := strings.Cut(fields.Field(i).Tag.Get("json"), ",")
		want[tag] = true
	}
	got := map[string]bool{}
	for _, m := range name.FindAllStringSubmatch(list, -1) {
		got[m[1]] = true
		if !want[m[1]] {
			t.Errorf("%s names %s, which is no RECEIPT field: %s", where, m[1], list)
		}
	}
	for tag := range want {
		if !got[tag] {
			t.Errorf("%s does not name the RECEIPT field %s: %s", where, tag, list)
		}
	}
}

// The design audit checks every done condition against the executor contract and every decision against the architect's
// authority, and either failing is a needs_changes finding: the packet of the audit says so in words the reviewer acts on.
func TestDesignAuditPacketChecksExecutabilityAndAuthority(t *testing.T) {
	l := NewLab(t, oneNode()...)
	mustRun(t, l)
	text := textOfCall(l, callNamed(t, l, "design-audit:r1").ID)
	for _, want := range []string{
		"EXECUTOR CONTRACT",
		"the executor never commits",
		"the run commits",
		"`crw <node> <call>`",
		"RECEIPT",
		"at the committed HEAD",
		"state of the worktree",
		"generation command and its verification command",
		"every done condition",
		"OUT-OF-AUTHORITY DECISIONS",
		"an exception to a rule of the repository",
		"the record of another issue",
		"a policy decision",
		"a prerequisite of the execution environment",
		"open question",
		"also when it reaches this audit unanswered",
		"needs_changes",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("the design audit packet lacks %q:\n%s", want, text)
		}
	}
	approve := regexp.MustCompile(`approved only when[^.]*every done condition[^.]*executor contract[^.]*no decision[^.]*outside the architect's authority`)
	if !approve.MatchString(text) {
		t.Errorf("approval is not conditioned on executable done conditions and no out-of-authority decision:\n%s", text)
	}
}

// libWith is the embedded helper library with one replacement in one file: the library of a binary whose packet text differs.
func libWith(t *testing.T, file, old, new string) fs.FS {
	t.Helper()
	out := fstest.MapFS{}
	err := fs.WalkDir(lib.FS, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := fs.ReadFile(lib.FS, p)
		if err != nil {
			return err
		}
		if p == file {
			if !strings.Contains(string(b), old) {
				t.Fatalf("%s has no %q", file, old)
			}
			b = []byte(strings.Replace(string(b), old, new, 1))
		}
		out[p] = &fstest.MapFile{Data: b}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// What a change of prompt text does to a run that resumes over a journal recorded with the earlier text (CRW-1321, the header of
// crw:lib/packets). A role overlay is the developer instructions of a thread and part of no input digest: a resume under other
// overlay text replays every call and runs nothing live. A packet's text is part of the input digest of its call: a resume under
// another design audit packet runs design-audit:r1 again live, and the rest of its path with it, and crw.designLedger then
// refuses to add the first revision a second time, so the resumed run ends in that refusal. That resume supersedes the recorded
// audit and the first revision with it, so the binary that started the run refuses it afterwards too: a run past its design audit
// is finished by the binary that started it only while no process of the new one has resumed it, and is otherwise started anew.
func TestResumeAfterPromptTextChange(t *testing.T) {
	l := NewLab(t, twoNodes()...)
	if got := mustRun(t, l); got.State != "closed" {
		t.Fatalf("state %q", got.State)
	}
	before := l.LastSeq()
	l.Prompt = func(role string, qa bool) (string, error) {
		p, err := roles.Prompt(role, roles.Options{QA: qa})
		return p + "\n\nAnother sentence of the overlay.", err
	}
	l.NewProcess("proc-2")
	if res := l.Resume(); res.Status != engine.RunCompleted {
		t.Fatalf("resume under another overlay: %s: %s", res.Status, res.Error)
	}
	if live := l.LiveCalls(before); len(live) != 0 {
		t.Errorf("a resume under another overlay text ran calls live: %+v", live)
	}

	before = l.LastSeq()
	l.Lib = libWith(t, "packets.js", "before any executor starts.", "before any executor starts. Another sentence of the packet.")
	l.NewProcess("proc-3")
	res := l.Resume()
	var labels []string
	for _, c := range l.LiveCalls(before) {
		labels = append(labels, c.Label)
	}
	if !slices.Equal(labels, []string{"design-audit:r1"}) {
		t.Errorf("live calls %v, want the design audit whose packet changed", labels)
	}
	refused := false
	for _, ev := range rowsAfter(l, before) {
		if ev.Kind == engine.KindCall && ev.Op == ops.OpDesignLedger && ev.Event == engine.EvRefused {
			refused = true
		}
	}
	if res.Status == engine.RunCompleted || !refused || !strings.Contains(res.Error, "crw.designLedger") {
		t.Errorf("the resume under another audit packet: %s %q (ledger refused %v), want it to end in the ledger's refusal", res.Status, res.Error, refused)
	}

	// The binary that started the run, after that resume: its audit packet is the recorded one, but the recorded audit and r0 were
	// superseded, so design-audit:r1 runs live once more and the ledger refuses again - there is no way back to the earlier binary.
	before = l.LastSeq()
	l.Lib = nil
	l.NewProcess("proc-4")
	res = l.Resume()
	labels = nil
	for _, c := range l.LiveCalls(before) {
		labels = append(labels, c.Label)
	}
	if res.Status == engine.RunCompleted || !strings.Contains(res.Error, "crw.designLedger") || !slices.Equal(labels, []string{"design-audit:r1"}) {
		t.Errorf("the starting binary after a resume by the new one: %s %q, live calls %v; want design-audit:r1 live and the ledger's refusal", res.Status, res.Error, labels)
	}
}

// A run whose open questions were routed by the binary before CRW-1347 (crw.explore handed crw.exploreRecord answers without the
// reason, the scope and the notation of their routes) and that a binary of CRW-1347 resumes (the header of crw:lib/packets): the
// crw.exploreRoute rows and the explorer calls replay as recorded, crw.exploreRecord runs again with the answers of the new
// binary, design-audit:r1 is the one agent call that runs live, and crw.designLedger refuses the first revision again.
func TestResumeAfterOpenQuestionRoutingChange(t *testing.T) {
	l := NewLab(t, oneNode()...)
	d := l.designFor()
	d.OpenQuestions = []ops.OpenQuestion{
		{ID: "small", Question: "is the flag documented?", Scope: "docs", FilesNeeded: []string{"README.md"}},
		{ID: "policy", Question: "does the project ship the flag in 1.0?", Scope: "authority:policy", FilesNeeded: []string{}},
	}
	l.Design = &d
	l.Lib = libWith(t, "lane.js", "route: r.route, reason: r.reason || '', scope: r.scope || '', authority: r.authority || '', answer,", "route: r.route, answer,")
	if got := mustRun(t, l); got.State != "closed" {
		t.Fatalf("state %q", got.State)
	}
	before := l.LastSeq()
	l.Lib = nil
	l.NewProcess("proc-2")
	res := l.Resume()
	var labels []string
	for _, c := range l.LiveCalls(before) {
		labels = append(labels, c.Label)
	}
	if !slices.Equal(labels, []string{"design-audit:r1"}) {
		t.Errorf("live calls %v, want the design audit only", labels)
	}
	ran := map[string]int{}
	refused := false
	for _, ev := range rowsAfter(l, before) {
		if ev.Kind == engine.KindCall && ev.Event == engine.EvCompleted {
			ran[ev.Op]++
		}
		if ev.Kind == engine.KindCall && ev.Op == ops.OpDesignLedger && ev.Event == engine.EvRefused {
			refused = true
		}
	}
	if ran[ops.OpExploreRoute] != 0 || ran[ops.OpExploreRecord] != 1 {
		t.Errorf("ops run again %v, want crw.exploreRecord and not crw.exploreRoute", ran)
	}
	if res.Status == engine.RunCompleted || !refused || !strings.Contains(res.Error, "crw.designLedger") {
		t.Errorf("the resume: %s %q (ledger refused %v), want the ledger's refusal", res.Status, res.Error, refused)
	}
}
