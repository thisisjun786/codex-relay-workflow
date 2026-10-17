package scenario

import (
	"slices"
	"strings"
	"testing"

	flowcli "github.com/thisisjun786/codex-relay-workflow/internal/flow/cli"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/ops"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/decisions"
)

// The follow-ups of CRW-1366 (CRW-1381): an ask that joins several questions takes an answer for each, a question the run asked that a
// later DESIGN no longer raises stays answerable, and the optional origin is written only when it has a value.

var envQuestion = ops.OpenQuestion{ID: "env", Question: "may the lane use the shared runner?", Scope: "authority:environment", FilesNeeded: []string{}}

// judgeDesignAskFor judges, as `crw flow answer --answer-for` does, a manager's answer to the design ask q with an answer for each
// question digest, and answers the row it would append.
func judgeDesignAskFor(l *Lab, q flowcli.Question, decision, text string, parts map[string]string) (flowcli.Application, *flowcli.Refusal) {
	run := flowcli.RunRecord{RunID: l.RunID, Owner: "owner", PlanID: "plan-1381", PlanRevision: 1}
	b := flowcli.Binding{Source: flowcli.SourceQuestion, Run: l.RunID, Node: q.Node, Question: q.ID, Revision: q.Revision, Attempt: q.Attempt, PlanBasis: flowcli.PlanBasis(run.PlanID, run.PlanRevision)}
	rec := decisions.Record{DecisionID: decision, State: decisions.StateAnswered, Source: decisions.Source{Kind: b.Source, Ref: flowcli.BindingRef(b)},
		Blocking:  []decisions.Blocking{{Kind: decisions.BlockingPlanNode, Ref: b.Run + "/" + b.Node}},
		Authority: decisions.Authority{Kind: decisions.AuthorityDelegatedManagement}, AnsweredBy: "manager-task", AnsweredVia: decisions.ViaManagementMessage, AnswerText: text}
	return flowcli.Judge(rec, l.Events(), run, flowcli.Claim{Decision: decision, Run: l.RunID, Node: q.Node, Question: q.ID, Revision: q.Revision, By: "owner", Answers: parts})
}

// TestEachQuestionOfAJoinedAskGetsItsOwnAnswer (CRW-1381 d3): round 1 asks two authority questions in one ask. The answer gives each
// question its own part, by question digest: the applied row holds the parts, and from round 2 on each question reuses its own part, not
// the whole text. The answer path takes a part only for a question the ask carries and only as text of the recorded answer (input
// errors otherwise), and a single answer to a joined ask is applied to every question with a warning in the row.
func TestEachQuestionOfAJoinedAskGetsItsOwnAnswer(t *testing.T) {
	l := NewLab(t, oneNode()...)
	d := l.designFor()
	d.OpenQuestions = []ops.OpenQuestion{policyQuestion, envQuestion}
	l.Design = &d
	dp, de := ops.QuestionDigest(policyQuestion.Question), ops.QuestionDigest(envQuestion.Question)
	const whole = "1.0 ships without the flag. The shared runner is fine."
	parts := map[string]string{dp: "1.0 ships without the flag.", de: "The shared runner is fine."}
	var judged flowcli.Application
	var refusals []*flowcli.Refusal
	l.On("^design-audit:r1$", func(c *Call) *fakehostPlan {
		asks := designAsks(l)
		if len(asks) != 1 {
			t.Errorf("%d design asks before the audit of round 1, want the one joined ask", len(asks))
			p := designAuditNeedsChanges(c)
			return &p
		}
		_, r := judgeDesignAskFor(l, asks[0], "dec-unknown", whole, map[string]string{"sha256:none": "x"})
		refusals = append(refusals, r)
		_, r = judgeDesignAskFor(l, asks[0], "dec-other", whole, map[string]string{dp: "words the record never said"})
		refusals = append(refusals, r)
		single, r := judgeDesignAskFor(l, asks[0], "dec-single", whole, nil)
		if r != nil || !strings.Contains(strings.Join(rowWarnings(single.Data), " | "), "single_answer_for_joined_ask") {
			t.Errorf("a single answer to the joined ask: %v %v", r, single.Data)
		}
		var err *flowcli.Refusal
		if judged, err = judgeDesignAskFor(l, asks[0], "dec-1", whole, parts); err != nil {
			t.Errorf("the answer with a part for each question is refused: %v", err)
		}
		l.apply(ops.EvDecisionApplied, judged.Data)
		p := designAuditNeedsChanges(c)
		return &p
	})
	if got := mustRun(t, l); got.State != "closed" {
		t.Fatalf("state %q; calls %v", got.State, l.Labels())
	}
	if len(refusals) != 2 || refusals[0] == nil || refusals[1] == nil {
		t.Fatalf("the answers with a part for a question the ask does not carry, and with a part the record does not hold: %v", refusals)
	}
	dec, _ := judged.Data["decision"].(map[string]any)
	if got, _ := dec["answers"].(map[string]string); got[dp] != parts[dp] || got[de] != parts[de] {
		t.Fatalf("the applied row's parts: %v", judged.Data)
	}
	r2 := answersOf(t, l, "design-audit:r2")
	for _, want := range []string{
		"policy: does the project ship the flag in 1.0?\n  route: reused: the answer the orchestrator gave in the design_question escalation of the run (original route: orchestrator)\n  answer: 1.0 ships without the flag.\n",
		"env: may the lane use the shared runner?\n  route: reused: the answer the orchestrator gave in the design_question escalation of the run (original route: orchestrator)\n  answer: The shared runner is fine.\n",
	} {
		if !strings.Contains(r2+"\n", want) {
			t.Errorf("design-audit:r2 answers lack %q:\n%s", want, r2)
		}
	}
}

func rowWarnings(data map[string]any) []string {
	switch w := data["warnings"].(type) {
	case []string:
		return w
	case []any:
		var out []string
		for _, x := range w {
			s, _ := x.(string)
			out = append(out, s)
		}
		return out
	}
	return nil
}

// TestAQuestionTheLaterDesignNoLongerRaisesStaysAnswerable (CRW-1381 P3): round 1 asks the policy question; the revised DESIGN no
// longer raises it and raises a new authority question, whose ask replaces the first. The new ask carries the policy question too (its
// digest in the evidence, its text in the detail), so the question the run asked stays answerable through the current ask.
func TestAQuestionTheLaterDesignNoLongerRaisesStaysAnswerable(t *testing.T) {
	l := NewLab(t, oneNode()...)
	d := l.designFor()
	d.OpenQuestions = []ops.OpenQuestion{policyQuestion}
	l.Design = &d
	revised := l.designFor()
	revised.OpenQuestions = []ops.OpenQuestion{envQuestion}
	l.Sequence("^design-fix:r1$", func(*Call) fakehostPlan { return Say(revised) })
	l.Sequence("^design-audit:r\\d+$", designAuditNeedsChanges, func(*Call) fakehostPlan {
		return Say(map[string]any{"verdict": "approved", "findings": []any{}, "summary": "now it holds", "root": nil, "first_check": nil})
	})
	if got := mustRun(t, l); got.State != "closed" {
		t.Fatalf("state %q; calls %v", got.State, l.Labels())
	}
	rows := designQuestions(l)
	if len(rows) != 2 {
		t.Fatalf("%d design_question escalations, want one for each round that raised a new authority question", len(rows))
	}
	if got, want := askedIn(rows[1]), []string{ops.QuestionDigest(envQuestion.Question), ops.QuestionDigest(policyQuestion.Question)}; !slices.Equal(got, want) {
		t.Errorf("the ask of round 2 names %v, want the new question and the one the DESIGN no longer raises %v", got, want)
	}
	if detail := str(rows[1].Data, "detail"); !strings.Contains(detail, envQuestion.Question) || !strings.Contains(detail, policyQuestion.Question) {
		t.Errorf("the ask of round 2 is %q", detail)
	}
	asks := designAsks(l)
	if len(asks) != 2 || !asks[1].Current {
		t.Fatalf("the design asks: %+v", asks)
	}
	if _, r := judgeDesignAskFor(l, asks[1], "dec-2", "1.0 ships without the flag; the shared runner is fine", map[string]string{ops.QuestionDigest(policyQuestion.Question): "1.0 ships without the flag"}); r != nil {
		t.Errorf("an answer to the policy question through the current ask is refused: %v", r)
	}
}

// TestOriginIsWrittenOnlyWithAValue (CRW-1381 P3): the optional field origin is the route a reused answer came from; a route, an explore
// row and an answer of crw.exploreRecord that reuse nothing carry no origin field, not an empty one.
func TestOriginIsWrittenOnlyWithAValue(t *testing.T) {
	l := NewLab(t, oneNode()...)
	d := l.designFor()
	again := policyQuestion
	again.ID = "again"
	d.OpenQuestions = append(openQuestions(1), policyQuestion, again)
	l.Design = &d
	if got := mustRun(t, l); got.State != "closed" {
		t.Fatalf("state %q", got.State)
	}
	check := func(what string, list []any) {
		for _, item := range list {
			m, _ := item.(map[string]any)
			o, has := m["origin"]
			switch {
			case str(m, "route") == ops.RouteReuse && (!has || o == ""):
				t.Errorf("%s: a reuse without its origin: %v", what, m)
			case str(m, "route") != ops.RouteReuse && has:
				t.Errorf("%s: a %s route carries origin %q", what, str(m, "route"), o)
			}
		}
	}
	for _, row := range l.Rows(ops.OpExploreRecord) {
		list, _ := row.Data["answers"].([]any)
		check("crw.exploreRecord answers", list)
	}
	for _, row := range l.Rows(ops.OpExploreRoute) {
		list, _ := row.Data["routes"].([]any)
		check("crw.exploreRoute routes", list)
	}
}
