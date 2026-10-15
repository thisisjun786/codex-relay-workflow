package scenario

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/ops"
)

// stepEvidence is the evidence a step row was recorded with.
func stepEvidence(t *testing.T, row StepRow) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(row.Evidence), &m); err != nil {
		t.Fatalf("evidence %q: %v", row.Evidence, err)
	}
	return m
}

// onlyStep asserts the run took exactly one tier step of the node, by the letter of the rule, and that the executor-t1 call carries
// its id, starts in a new worktree at the checkpoint the row records, and is not a continuation of an executor thread.
func onlyStep(t *testing.T, l *Lab, node, letter string) (StepRow, CallInfo) {
	t.Helper()
	steps := l.StepRows()
	if len(steps) != 1 {
		t.Fatalf("step rows %+v, want one", steps)
	}
	s := steps[0]
	if s.Node != node || s.From != "executor" || s.To != "executor-t1" || s.Rule != ops.RuleTier1 {
		t.Errorf("step %+v", s)
	}
	if got := stepEvidence(t, s)["letter"]; got != letter {
		t.Errorf("step evidence letter %v, want %s: %s", got, letter, s.Evidence)
	}
	t1 := l.CallsOf("executor-t1")
	if len(t1) != 1 {
		t.Fatalf("executor-t1 calls %+v, want one", t1)
	}
	if t1[0].Step != s.ID || s.ID == "" {
		t.Errorf("the executor-t1 call carries step %q, the row is %q", t1[0].Step, s.ID)
	}
	if t1[0].Resume != "" {
		t.Errorf("executor-t1 continues %q: a tier step starts a fresh thread", t1[0].Resume)
	}
	prep := l.Prepared(t1[0].ID)
	alloc, _ := prep.Data["allocation"].(map[string]any)
	// the new worktree starts at the last commit of the node, or at the base when the node has none yet
	start := s.Head
	if start == "" {
		start = l.Base
	}
	if str(alloc, "kind") != "fresh" || str(alloc, "start_commit") != start {
		t.Errorf("executor-t1 allocation %v, checkpoint head %q", alloc, s.Head)
	}
	if rows := l.Rows("crw.tierStep"); len(rows) != 1 {
		t.Errorf("%d crw.tierStep rows", len(rows))
	}
	return s, t1[0]
}

// Two consecutive needs_changes with one finding in both: SP-TIER-1 (a). The step is recorded, and only then does an executor-t1
// thread start, on the last commit.
func TestTierStepOnRepeatedFinding(t *testing.T) {
	l := NewLab(t, oneNode()...)
	same := Defect("d1", "correctness", "P1", "the guard is missing")
	reviews(l, "A", Verdict(false, same), Verdict(false, same), Verdict(true))
	got := mustRun(t, l)
	if got.State != "closed" {
		t.Fatalf("state %q: %+v", got.State, got)
	}
	want := []string{"architect", "design-audit:r1", "build:A:executor", "review:A:r1", "fix:A:executor:r1", "review:A:r2", "build:A:executor-t1", "review:A:r3", "reviewer-final:M1", "reflection:M1"}
	if labels := l.Labels(); !slices.Equal(labels, want) {
		t.Fatalf("calls %v, want %v", labels, want)
	}
	step, _ := onlyStep(t, l, "A", ops.TierEvidenceFinding)
	if reviewers, ok := stepEvidence(t, step)["reviewer_calls"].([]any); !ok || len(reviewers) != 2 {
		t.Errorf("the step names its reviewer calls: %s", step.Evidence)
	}
	// the checkpoint is the last commit the node had when the step was taken: the one before the executor-t1's own
	commits := l.Rows("crw.commit")
	if len(commits) != 3 {
		t.Fatalf("%d commits, want three", len(commits))
	}
	if before := str(commits[1].Data, "commit"); step.Head != before {
		t.Errorf("checkpoint head %s, the last commit before the step is %s", step.Head, before)
	}
	if rows := l.Rows("crw.stepRecord"); len(rows) != 1 {
		t.Errorf("%d crw.stepRecord rows", len(rows))
	}
}

// The required verification fails on two consecutive committed heads: SP-TIER-1 (b). The heads are fixed on the executor's thread
// first; no reviewer sees a head that failed its verification.
func TestTierStepOnTwoFailingHeads(t *testing.T) {
	l := NewLab(t, oneNode()...)
	l.Verify.Fail = map[string][]int{"A": {1, 2}}
	got := mustRun(t, l)
	if got.State != "closed" {
		t.Fatalf("state %q: %+v", got.State, got)
	}
	want := []string{"architect", "design-audit:r1", "build:A:executor", "fix:A:executor:r1", "build:A:executor-t1", "review:A:r3", "reviewer-final:M1", "reflection:M1"}
	if labels := l.Labels(); !slices.Equal(labels, want) {
		t.Fatalf("calls %v, want %v", labels, want)
	}
	onlyStep(t, l, "A", ops.TierEvidenceVerify)
	if n := len(l.CallsOf("reviewer")); n != 1 {
		t.Errorf("%d reviewer calls (one review; the audit is the critic's)", n)
	}
}

// Four reviewed rounds on the tier without a verified verdict, each with a finding of its own: SP-TIER-1 (d).
func TestRoundCapStepsTier(t *testing.T) {
	l := NewLab(t, oneNode()...)
	reviews(l, "A",
		Verdict(false, Defect("d1", "correctness", "P2", "first finding")),
		Verdict(false, Defect("d2", "test_gap", "P2", "second finding")),
		Verdict(false, Defect("d3", "scope", "P2", "third finding")),
		Verdict(false, Defect("d4", "maintainability", "P2", "fourth finding")),
		Verdict(true))
	got := mustRun(t, l)
	if got.State != "closed" {
		t.Fatalf("state %q: %+v", got.State, got)
	}
	want := []string{"architect", "design-audit:r1", "build:A:executor", "review:A:r1", "fix:A:executor:r1", "review:A:r2", "fix:A:executor:r2", "review:A:r3", "fix:A:executor:r3", "review:A:r4", "build:A:executor-t1", "review:A:r5", "reviewer-final:M1", "reflection:M1"}
	if labels := l.Labels(); !slices.Equal(labels, want) {
		t.Fatalf("calls %v, want %v", labels, want)
	}
	onlyStep(t, l, "A", ops.TierEvidenceRounds)
}

// The same evidence on executor-t1 ends the tiers: no second step, no third executor, the node escalates as tiers_exhausted, and
// the escalation closes the node's execution at the relay (R07), which returns its slot.
func exhausted(t *testing.T) *Lab {
	t.Helper()
	l := NewLab(t, oneNode()...)
	l.Verify.Fail = map[string][]int{"A": {1, 2, 3, 4}}
	return l
}

func TestTiersExhaustedEscalates(t *testing.T) {
	l := exhausted(t)
	got := mustRun(t, l)
	if got.State != "incomplete" {
		t.Fatalf("state %q", got.State)
	}
	if state, reason := laneOf(t, got, "A"); state != "escalated" || reason != "tiers_exhausted" {
		t.Fatalf("lane A: %s %s", state, reason)
	}
	want := []string{"architect", "design-audit:r1", "build:A:executor", "fix:A:executor:r1", "build:A:executor-t1", "fix:A:executor-t1:r3"}
	if labels := l.Labels(); !slices.Equal(labels, want) {
		t.Fatalf("calls %v, want %v", labels, want)
	}
	if steps := l.StepRows(); len(steps) != 1 {
		t.Errorf("step rows %+v: one step for a node", steps)
	}
	if n := len(l.CallsOf("executor-t1")); n != 2 {
		t.Errorf("%d executor-t1 calls (the build and its fix), no third tier", n)
	}
	var reasons []string
	for _, ev := range l.FlowRows(engine.EvEscalation) {
		reasons = append(reasons, str(ev.Data, "reason"))
	}
	if !slices.Equal(reasons, []string{"tiers_exhausted"}) {
		t.Errorf("escalations %v", reasons)
	}
	if len(l.Relay.Landed) != 0 {
		t.Errorf("landed %v", l.Relay.Landed)
	}
	// the end of the tiers is a decision with its rule: SP-TIER-2 refuses a third tier, and the escalation follows it
	var tier2 []SpawnRow
	for _, r := range l.SpawnRows() {
		if r.Rule == ops.RuleTier2 {
			tier2 = append(tier2, r)
		}
	}
	if len(tier2) != 1 || tier2[0].Node != "A" || tier2[0].Decision != "refused" || tier2[0].Kind != "signal" {
		t.Errorf("SP-TIER-2 decision rows %+v: one refusal of a third tier for A", tier2)
	}
}

// A tiers_exhausted escalation is the end of the node's execution: the relay is asked to close it (AgentClose, reason
// tiers_exhausted), and the slot the release reserved goes back with it. The slot was held until then.
func TestAgentCloseOnEscalationReleasesSlot(t *testing.T) {
	l := exhausted(t)
	held := false
	l.OnRow(func(ev engine.Event) {
		if ev.Op == ops.OpEscalate && ev.Event == engine.EvCompleted {
			return
		}
		if ev.Kind == engine.KindCall && ev.Event == engine.EvPrepared && ev.Label == "fix:A:executor-t1:r3" {
			held = l.Relay.SlotHeld("A")
		}
	})
	mustRunIncomplete(t, l)
	if !held {
		t.Errorf("the node did not hold its slot while it was running")
	}
	if got := l.Relay.CloseReasons(); !slices.Equal(got, []string{"A:tiers_exhausted"}) {
		t.Fatalf("closes %v", got)
	}
	if l.Relay.SlotHeld("A") {
		t.Errorf("the slot of the escalated node was not returned")
	}
	rows := l.Rows("crw.escalate")
	if len(rows) != 1 || !dataBoolOf(rows[0], "closed") || !dataBoolOf(rows[0], "slot_released") {
		t.Fatalf("escalate rows %+v", rows)
	}
}

func dataBoolOf(ev engine.Event, key string) bool {
	b, _ := ev.Data[key].(bool)
	return b
}

func mustRunIncomplete(t *testing.T, l *Lab) laneResult {
	t.Helper()
	res := l.Run()
	if res.Status != engine.RunCompleted {
		t.Fatalf("run %s: %s: %s", res.Status, res.ErrorKind, res.Error)
	}
	return Value[laneResult](t, res)
}

// ---- what is not tier evidence ----

// null_model once is not evidence: the next executor is a fresh thread of the same tier, in a new worktree.
func TestNoTierStepOnSingleNull(t *testing.T) {
	l := NewLab(t, oneNode()...)
	l.Sequence("^build:A:executor$", func(*Call) fakehostPlan { return Garbage() })
	got := mustRun(t, l)
	if got.State != "closed" {
		t.Fatalf("state %q", got.State)
	}
	want := []string{"architect", "design-audit:r1", "build:A:executor", "rebuild:A:executor:r1", "review:A:r2", "reviewer-final:M1", "reflection:M1"}
	if labels := l.Labels(); !slices.Equal(labels, want) {
		t.Fatalf("calls %v, want %v", labels, want)
	}
	if term := l.Terminal("build:A:executor"); term != engine.EvNullModel {
		t.Errorf("the first build ended %s", term)
	}
	rebuild := callNamed(t, l, "rebuild:A:executor:r1")
	if rebuild.Resume != "" {
		t.Errorf("the rebuild continues %q: a null_model thread is not continued", rebuild.Resume)
	}
	alloc, _ := l.Prepared(rebuild.ID).Data["allocation"].(map[string]any)
	if str(alloc, "kind") != "fresh" {
		t.Errorf("rebuild allocation %v", alloc)
	}
	noTierStep(t, l)
}

// Two null_model in a row on the tier are evidence: SP-TIER-1 (c).
func TestSecondConsecutiveNullModelStepsTier(t *testing.T) {
	l := NewLab(t, oneNode()...)
	l.Sequence("^(build|rebuild):A:executor:?r?\\d*$", func(*Call) fakehostPlan { return Garbage() })
	got := mustRun(t, l)
	if got.State != "closed" {
		t.Fatalf("state %q: %+v", got.State, got)
	}
	want := []string{"architect", "design-audit:r1", "build:A:executor", "rebuild:A:executor:r1", "build:A:executor-t1", "review:A:r3", "reviewer-final:M1", "reflection:M1"}
	if labels := l.Labels(); !slices.Equal(labels, want) {
		t.Fatalf("calls %v, want %v", labels, want)
	}
	onlyStep(t, l, "A", ops.TierEvidenceNull)
}

// An executor that reports failed or budget_exhausted about itself is not evidence: the run commits and verifies the worktree
// anyway, reviews what verification passes, and takes no step.
func selfReported(t *testing.T, outcome string) {
	t.Helper()
	l := NewLab(t, oneNode()...)
	l.Sequence("^build:A:executor$", func(c *Call) fakehostPlan {
		c.Edit()
		return Say(ReceiptWith(outcome, map[string]any{"gap": "I could not finish"}))
	})
	got := mustRun(t, l)
	if got.State != "closed" {
		t.Fatalf("state %q: %+v", got.State, got)
	}
	want := []string{"architect", "design-audit:r1", "build:A:executor", "review:A:r1", "reviewer-final:M1", "reflection:M1"}
	if labels := l.Labels(); !slices.Equal(labels, want) {
		t.Fatalf("calls %v, want %v", labels, want)
	}
	nodeVerifies := 0
	for _, r := range l.Rows("crw.verify") {
		if str(r.Data, "node") == "A" {
			nodeVerifies++
		}
	}
	if len(l.Rows("crw.commit")) != 1 || nodeVerifies != 1 {
		t.Errorf("the run did not commit and verify the worktree of a %s report", outcome)
	}
	noTierStep(t, l)
}

func TestNoTierStepOnSelfReportedFailed(t *testing.T) { selfReported(t, "failed") }

func TestNoTierStepOnBudgetExhausted(t *testing.T) { selfReported(t, "budget_exhausted") }

// A receipt that needs research is answered by an explorer and the SAME executor thread goes on with the answer: no new thread, no
// step.
func TestNeedsResearchResumesSameExecutor(t *testing.T) {
	l := NewLab(t, oneNode()...)
	l.Sequence("^build:A:executor$", func(c *Call) fakehostPlan {
		return Say(ReceiptWith("needs_research", map[string]any{"research_question": "which package owns the retry policy?"}))
	})
	got := mustRun(t, l)
	if got.State != "closed" {
		t.Fatalf("state %q: %+v", got.State, got)
	}
	labels := l.Labels()
	if len(labels) != 8 || labels[6] != "reviewer-final:M1" || labels[7] != "reflection:M1" || labels[2] != "build:A:executor" || !strings.HasPrefix(labels[3], "explore:A:") || labels[4] != "research:A:executor:r1" || labels[5] != "review:A:r2" {
		t.Fatalf("calls %v", labels)
	}
	build, research := callNamed(t, l, "build:A:executor"), callNamed(t, l, "research:A:executor:r1")
	if research.Resume != build.ID {
		t.Errorf("the research turn continues %q, want the executor's call %s", research.Resume, build.ID)
	}
	if text := textOfCall(l, research.ID); !strings.Contains(text, "answer of the explorer") {
		t.Errorf("the executor was not given the answer:\n%s", text)
	}
	found := false
	for _, r := range l.SpawnRows() {
		if r.Rule == ops.RuleExpl2 && r.Decision == "spawn" && r.Node == "A" {
			found = true
		}
	}
	if !found {
		t.Errorf("no SP-EXPL-2 spawn decision: %+v", l.SpawnRows())
	}
	noTierStep(t, l)
}

// blocked_scope is a decision for the orchestrator, not a failure of the tier: the node escalates, the relay closes its execution
// and takes its slot back, and nothing else runs.
func TestBlockedScopeEscalatesNoTierStep(t *testing.T) {
	l := NewLab(t, oneNode()...)
	l.Sequence("^build:A:executor$", func(c *Call) fakehostPlan {
		return Say(ReceiptWith("blocked_scope", map[string]any{"gap": "the change needs the other package"}))
	})
	got := mustRunIncomplete(t, l)
	if state, reason := laneOf(t, got, "A"); state != "escalated" || reason != "blocked_scope" {
		t.Fatalf("lane A: %s %s", state, reason)
	}
	if labels := l.Labels(); !slices.Equal(labels, []string{"architect", "design-audit:r1", "build:A:executor"}) {
		t.Fatalf("calls %v", labels)
	}
	if got := l.Relay.CloseReasons(); !slices.Equal(got, []string{"A:blocked_scope"}) {
		t.Errorf("closes %v", got)
	}
	noTierStep(t, l)
}

// A request that got no answer leaves a call whose outcome is unknown: the node is in recovery hold, nothing is sent again and
// nothing steps the tier.
func TestUnknownDispatchHoldsNoTierStep(t *testing.T) {
	l := NewLab(t, oneNode()...)
	l.Threads.DropAck("turn/start", func(_ int, params map[string]any) bool { return strings.Contains(inputText(params), "ROLE: executor") })
	got := mustRunIncomplete(t, l)
	if state, reason := laneOf(t, got, "A"); state != "held" || reason != "dispatch_unknown" {
		t.Fatalf("lane A: %s %s", state, reason)
	}
	if labels := l.Labels(); !slices.Equal(labels, []string{"architect", "design-audit:r1", "build:A:executor"}) {
		t.Fatalf("calls %v", labels)
	}
	if rows := l.Rows("crw.hold"); len(rows) != 1 || str(rows[0].Data, "reason") != "dispatch_unknown" {
		t.Fatalf("hold rows %+v", rows)
	}
	if got := l.Relay.CloseReasons(); !slices.Equal(got, []string{"A:dispatch_unknown"}) || l.Relay.Closes[0].Reason != "dispatch_unknown" {
		t.Errorf("closes %v", got)
	}
	if !l.Relay.SlotHeld("A") {
		t.Errorf("a recovery hold returned the slot")
	}
	noTierStep(t, l)
}

// An infrastructure failure of the host is a held call: the node is held, the packet is not sent to a new thread, and the
// failure is not evidence against the tier.
func TestInfraFailureHoldsNoTierStep(t *testing.T) {
	l := NewLab(t, oneNode()...)
	l.Sequence("^build:A:executor$", func(*Call) fakehostPlan { return Fail("serverOverloaded") })
	got := mustRunIncomplete(t, l)
	if state, reason := laneOf(t, got, "A"); state != "held" || reason != "host_infra_serverOverloaded" {
		t.Fatalf("lane A: %s %s", state, reason)
	}
	if labels := l.Labels(); !slices.Equal(labels, []string{"architect", "design-audit:r1", "build:A:executor"}) {
		t.Fatalf("calls %v", labels)
	}
	if rows := l.Rows("crw.hold"); len(rows) != 1 || str(rows[0].Data, "reason") != "host_infra_serverOverloaded" {
		t.Fatalf("hold rows %+v", rows)
	}
	if got := l.Relay.CloseReasons(); !slices.Equal(got, []string{"A:host_failure"}) {
		t.Errorf("closes %v", got)
	}
	noTierStep(t, l)
}

// A fresh executor-t1 call needs the id crw.tierStep returned: without one it is refused, and no thread starts.
func TestExecutorT1WithoutDecisionRefused(t *testing.T) {
	l := NewLab(t, oneNode()...)
	res := l.Script(`const r = await agent.call('take over', { agentType: 'executor-t1', node: 'A', label: 'takeover' })
return [r.status, r.reason]`, false)
	if got := Value[[]string](t, res); !slices.Equal(got, []string{"refused", "missing_step_decision"}) {
		t.Fatalf("%v", got)
	}
	if n := len(l.Requests("thread/start")); n != 0 {
		t.Errorf("%d thread/start requests: a refused call sends nothing", n)
	}
	for _, c := range l.CallsOf("executor-t1") {
		t.Logf("call %+v", c)
	}
}
