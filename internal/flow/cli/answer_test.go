package cli

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/ops"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/decisions"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// asking is a run that asked the orchestrator a question: the executor of n1 was refused (a configuration fault) in its second
// attempt, and the lane escalated with an ask and waits for crw.decide.
type asking struct {
	*run
	q   string
	rec RunRecord
}

func newAsking(t *testing.T, w *world) *asking {
	t.Helper()
	r := w.newRun("run-q", "thread-owner")
	r.phase("build")
	r.call("c_ex", "executor", "n1", 2, []string{engine.EvPrepared, engine.EvRefused}, map[string]map[string]any{engine.EvRefused: {"reason": "pair_refused"}})
	esc := r.flow(engine.EvEscalation, map[string]any{"reason": "pair_refused", "class": "configuration_fault", "node": "n1", "call_id": "c_ex", "escalation_id": "esc_one", "ask": "configuration"})
	_ = esc
	r.finish(engine.RunCompleted, "")
	r.close()
	return &asking{run: r, q: "esc_one", rec: RunRecord{RunID: "run-q", Owner: "thread-owner", PlanID: "plan-1", PlanRevision: 3}}
}

// answered stores a decision record that answers the question of the run, as the manager records it, and returns its id.
func (a *asking) answered(t *testing.T, id string, mutate func(*Binding, *decisions.Record, *decisions.Answer)) string {
	t.Helper()
	b := Binding{Source: SourceQuestion, Run: "run-q", Node: "n1", Question: "esc_one", Revision: 1, Attempt: 2, PlanBasis: PlanBasis("plan-1", 3)}
	rec := decisions.Record{
		Schema: decisions.Schema, DecisionID: id, Kind: decisions.KindBlockedEscalation, Context: "the executor of n1 was refused: " + id,
		Options:  []decisions.Option{{ID: "retry", Label: "retry on a fresh thread", Effect: "the lane builds n1 again"}, {ID: "stop", Label: "stop n1", Effect: "n1 is given up"}},
		Blocking: []decisions.Blocking{{Kind: decisions.BlockingPlanNode}}, Origin: decisions.Origin{Project: "P"},
		Authority: decisions.Authority{Kind: decisions.AuthorityDelegatedManagement}, State: decisions.StateRaised, RaisedAt: "2026-10-12T08:00:00Z", Seen: []decisions.Seen{{At: "2026-10-12T08:00:00Z", Source: "test"}},
	}
	ans := decisions.Answer{Option: "retry", By: "manager-task", Via: decisions.ViaManagementMessage, Authority: decisions.Authority{Kind: decisions.AuthorityDelegatedManagement}}
	if mutate != nil {
		mutate(&b, &rec, &ans)
	}
	rec.Blocking = []decisions.Blocking{{Kind: decisions.BlockingPlanNode, Ref: b.Run + "/" + b.Node}}
	rec.Source = decisions.Source{Kind: b.Source, Ref: BindingRef(b)}
	rec.Fingerprint = decisions.Fingerprint(rec.Context, rec.Blocking, rec.Options)
	st := a.w.st
	if _, _, err := st.Raise(context.Background(), rec); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Update(context.Background(), id, func(_ context.Context, r decisions.Record) (decisions.Record, error) {
		return decisions.ValidateAnswer(r, ans)
	}); err != nil {
		t.Fatal(err)
	}
	return id
}

func (a *asking) answer(td *testDeps, decision string, extra ...string) (int, AnswerReply, string) {
	args := append([]string{"answer", "--state", a.w.state, "--decision", decision, "--run", "run-q", "--node", "n1", "--question", a.q, "--revision", "1", "--thread", "thread-owner", "--json"}, extra...)
	code, out, errOut := td.do(args...)
	var reply AnswerReply
	if err := json.Unmarshal([]byte(strings.TrimPrefix(out, "<<spawned>>\n")), &reply); err != nil && out != "" {
		a.w.t.Fatalf("%v: %s", err, out)
	}
	return code, reply, errOut
}

func (a *asking) appliedRows(t *testing.T) []engine.Event {
	t.Helper()
	rows, _, err := loadRun(a.w, "run-q")
	if err != nil {
		t.Fatal(err)
	}
	var out []engine.Event
	for _, e := range rows {
		if e.Kind == engine.KindFlow && e.Event == ops.EvDecisionApplied {
			out = append(out, e)
		}
	}
	return out
}

// TestAnswerRefusesStaleRevision: a record that answers revision 1 of a question the lane has since asked again (revision 2), a
// claim of another revision than the record's, and a record of an earlier attempt or another plan basis are refused with their
// own code, and nothing is appended to the journal.
func TestAnswerRefusesStaleRevision(t *testing.T) {
	w := newWorld(t)
	a := newAsking(t, w)
	td := w.deps()
	id := a.answered(t, "ud-1", nil)

	// the lane asks the same question again on a new attempt: the first is replaced
	r2, err := w.reopen("run-q")
	if err != nil {
		t.Fatal(err)
	}
	r2.call("c_ex2", "executor", "n1", 3, []string{engine.EvPrepared, engine.EvRefused}, map[string]map[string]any{engine.EvRefused: {"reason": "pair_refused"}})
	r2.flow(engine.EvEscalation, map[string]any{"reason": "pair_refused", "node": "n1", "call_id": "c_ex2", "escalation_id": "esc_two", "ask": "configuration"})
	r2.close()

	code, reply, _ := a.answer(td, id)
	if code != 1 || reply.Refusal == nil || reply.Refusal.Code != RefusedStaleRevison || !strings.Contains(reply.Refusal.Detail, "replaced by a later question") || reply.Applied {
		t.Fatalf("an answer to a replaced question: exit %d %+v", code, reply)
	}
	if len(td.spawned) != 0 || len(a.appliedRows(t)) != 0 {
		t.Fatalf("a refused answer resumed %d processes and appended %d rows", len(td.spawned), len(a.appliedRows(t)))
	}

	// the current question: the record's revision is 1 and the question's is 2
	a.q = "esc_two"
	id2 := a.answered(t, "ud-2", func(b *Binding, _ *decisions.Record, _ *decisions.Answer) {
		b.Question, b.Attempt, b.Revision = "esc_two", 3, 1
	})
	if code, reply, _ := a.answer(td, id2); code != 1 || reply.Refusal == nil || reply.Refusal.Code != RefusedStaleRevison || !strings.Contains(reply.Refusal.Detail, "revision 2") {
		t.Errorf("a record of revision 1 for revision 2: %d %+v", code, reply)
	}
	// the claim names another revision than the record
	id3 := a.answered(t, "ud-3", func(b *Binding, _ *decisions.Record, _ *decisions.Answer) {
		b.Question, b.Attempt, b.Revision = "esc_two", 3, 2
	})
	if code, reply, _ := a.answer(td, id3); code != 1 || reply.Refusal == nil || reply.Refusal.Code != RefusedStaleRevison {
		t.Errorf("a claim of revision 1 for a record of revision 2: %d %+v", code, reply)
	}
	// an older attempt of the node and a plan revision that moved
	id4 := a.answered(t, "ud-4", func(b *Binding, _ *decisions.Record, _ *decisions.Answer) {
		b.Question, b.Attempt, b.Revision = "esc_two", 2, 2
	})
	if code, reply, _ := a.answerRev(td, id4, "2"); code != 1 || reply.Refusal == nil || reply.Refusal.Code != RefusedStaleAttempt {
		t.Errorf("a record of attempt 2 for a question of attempt 3: %d %+v", code, reply)
	}
	id5 := a.answered(t, "ud-5", func(b *Binding, _ *decisions.Record, _ *decisions.Answer) {
		b.Question, b.Attempt, b.Revision, b.PlanBasis = "esc_two", 3, 2, PlanBasis("plan-1", 2)
	})
	if code, reply, _ := a.answerRev(td, id5, "2"); code != 1 || reply.Refusal == nil || reply.Refusal.Code != RefusedStalePlan {
		t.Errorf("a record answered under plan revision 2 for a run pinned to 3: %d %+v", code, reply)
	}
	if n := len(a.appliedRows(t)); n != 0 {
		t.Errorf("%d rows appended by refused answers", n)
	}
}

func (a *asking) answerRev(td *testDeps, decision, rev string) (int, AnswerReply, string) {
	return a.answer(td, decision, "--revision", rev)
}

// TestAnswerRefusesOtherNode: a record that binds another node than the question's, another node than the claim names, another
// run, and a question the run never asked are refused before anything is appended.
func TestAnswerRefusesOtherNode(t *testing.T) {
	w := newWorld(t)
	a := newAsking(t, w)
	td := w.deps()
	cases := []struct {
		name   string
		mutate func(*Binding, *decisions.Record, *decisions.Answer)
		extra  []string
		code   string
	}{
		{"the record binds the node n2 and the claim names n1", func(b *Binding, _ *decisions.Record, _ *decisions.Answer) { b.Node = "n2" }, nil, RefusedOtherNode},
		{"the claim names n2 and the record n1", nil, []string{"--node", "n2"}, RefusedOtherNode},
		{"another run", func(b *Binding, _ *decisions.Record, _ *decisions.Answer) { b.Run = "run-z" }, nil, RefusedOtherRun},
		{"a question the run never asked", func(b *Binding, _ *decisions.Record, _ *decisions.Answer) { b.Question = "esc_ghost" }, []string{"--question", "esc_ghost"}, RefusedNoQuestion},
		{"a record that binds no question", func(b *Binding, r *decisions.Record, _ *decisions.Answer) { b.Source = "supervisor_obligation" }, nil, RefusedBadBinding},
	}
	for i, tc := range cases {
		id := a.answered(t, "ud-node-"+itoa(i), tc.mutate)
		code, reply, errOut := a.answer(td, id, tc.extra...)
		if tc.name == "a record that binds no question" {
			// the binding cannot be read: the command says so
			if code != 1 || reply.Refusal == nil || reply.Refusal.Code != tc.code {
				t.Errorf("%s: exit %d %+v %s", tc.name, code, reply, errOut)
			}
			continue
		}
		if code != 1 || reply.Refusal == nil || reply.Refusal.Code != tc.code {
			t.Errorf("%s: exit %d %+v %s", tc.name, code, reply, errOut)
		}
	}
	if n := len(a.appliedRows(t)); n != 0 || len(td.spawned) != 0 {
		t.Errorf("%d rows appended, %d processes started by refused answers", n, len(td.spawned))
	}
	// a record the manager has not answered applies nothing
	rec := decisions.Record{Schema: decisions.Schema, DecisionID: "ud-open", Kind: decisions.KindBlockedEscalation, Context: "open",
		Options:   []decisions.Option{{ID: "a", Label: "a", Effect: "a"}, {ID: "b", Label: "b", Effect: "b"}},
		Blocking:  []decisions.Blocking{{Kind: decisions.BlockingPlanNode, Ref: "run-q/n1"}},
		Origin:    decisions.Origin{Project: "P"},
		Source:    decisions.Source{Kind: SourceQuestion, Ref: BindingRef(Binding{Question: "esc_one", Revision: 1, Attempt: 2, PlanBasis: PlanBasis("plan-1", 3)})},
		Authority: decisions.Authority{Kind: decisions.AuthorityDelegatedManagement}, State: decisions.StateRaised, RaisedAt: "2026-10-12T08:00:00Z", Seen: []decisions.Seen{{At: "2026-10-12T08:00:00Z", Source: "t"}}}
	rec.Fingerprint = decisions.Fingerprint(rec.Context, rec.Blocking, rec.Options)
	if _, _, err := w.st.Raise(ctx, rec); err != nil {
		t.Fatal(err)
	}
	if code, reply, _ := a.answer(td, "ud-open"); code != 1 || reply.Refusal == nil || reply.Refusal.Code != RefusedNotAnswered {
		t.Errorf("an unanswered record: %d %+v", code, reply)
	}
}

// TestAnswerRefusesOutOfScopeAuthority: an escalation of a run is answered by the orchestrator that holds the run, the manager or
// Jun; a decision only Jun takes is answered at the user class by one of the ways the user's answer arrives. A parent-class answer
// of another orchestrator, a decision only Jun takes answered below the user class, and one that arrives as the parent's report
// read back are refused.
func TestAnswerRefusesOutOfScopeAuthority(t *testing.T) {
	w := newWorld(t)
	a := newAsking(t, w)
	td := w.deps()
	parent := func(ref string) func(*Binding, *decisions.Record, *decisions.Answer) {
		return func(_ *Binding, r *decisions.Record, ans *decisions.Answer) {
			r.Authority = decisions.Authority{Kind: decisions.AuthorityParent, Ref: ref}
			ans.Authority = r.Authority
		}
	}
	jun := func(kind, via string) func(*Binding, *decisions.Record, *decisions.Answer) {
		return func(b *Binding, r *decisions.Record, ans *decisions.Answer) {
			b.Source = SourceJunDecision
			r.Authority = decisions.Authority{Kind: kind, Ref: "ref"}
			ans.Authority, ans.Via = r.Authority, via
		}
	}
	cases := []struct {
		name   string
		mutate func(*Binding, *decisions.Record, *decisions.Answer)
		want   string
	}{
		{"a parent-class answer of another orchestrator", parent("thread-intruder"), "orchestrator that holds the run"},
		{"a decision only Jun takes, answered at the management class", jun(decisions.AuthorityDelegatedManagement, decisions.ViaManagementMessage), "needs a user-class answer"},
		{"a decision only Jun takes, answered at the parent class", jun(decisions.AuthorityParent, decisions.ViaDirectAsk), "needs a user-class answer"},
		{"a decision only Jun takes, read back from the parent's report", jun(decisions.AuthorityUser, decisions.ViaSupervisorReadback), "arrives as the user's answer"},
	}
	for i, tc := range cases {
		id := a.answered(t, "ud-auth-"+itoa(i), tc.mutate)
		code, reply, _ := a.answer(td, id)
		if code != 1 || reply.Refusal == nil || reply.Refusal.Code != RefusedAuthority || !strings.Contains(reply.Refusal.Detail, tc.want) {
			t.Errorf("%s: exit %d %+v", tc.name, code, reply)
		}
	}
	if n := len(a.appliedRows(t)); n != 0 {
		t.Errorf("%d rows appended by refused answers", n)
	}
	// the answers in scope apply: the orchestrator that holds the run, the manager, and Jun's own through a way the user answers by
	for i, tc := range []struct {
		mutate func(*Binding, *decisions.Record, *decisions.Answer)
		class  string
	}{
		{parent("thread-owner"), "escalation"},
		{nil, "escalation"},
		{jun(decisions.AuthorityUser, decisions.ViaDirectAsk), "jun_decision"},
	} {
		w2 := newWorld(t)
		a2 := newAsking(t, w2)
		td2 := w2.deps()
		id := a2.answered(t, "ud-ok-"+itoa(i), tc.mutate)
		code, reply, errOut := a2.answer(td2, id, "--no-resume")
		if code != 0 || !reply.Applied || reply.Refusal != nil {
			t.Fatalf("case %d: exit %d %+v %s", i, code, reply, errOut)
		}
		if got := a2.appliedRows(t); len(got) != 1 || got[0].Data["class"] != tc.class {
			t.Errorf("case %d: rows %+v", i, got)
		}
	}
}

// TestAnswerAppliesOnce: an answer that holds appends one decision_applied row (the question, the node, the attempt, the plan
// basis, the decision and who gave it) and resumes the run once; the same answer again appends nothing and resumes nothing, another
// decision for the same question is refused, and a run a process holds takes no answer. crw.decide reads the row.
func TestAnswerAppliesOnce(t *testing.T) {
	w := newWorld(t)
	a := newAsking(t, w)
	td := w.deps()
	if err := WriteSpec(w.state, Spec{Schema: SpecSchema, RunID: "run-q", Workflow: "project-lane", Thread: "thread-owner", CreatedAt: "2026-10-12T08:00:00Z"}); err != nil {
		t.Fatal(err)
	}
	id := a.answered(t, "ud-apply", nil)

	// a process that holds the run takes no answer
	lock, err := TakeLock(w.state, "run-q", "test", td.Now())
	if err != nil {
		t.Fatal(err)
	}
	code, reply, _ := a.answer(td, id)
	lock.Release()
	if code != 1 || reply.Refusal == nil || reply.Refusal.Code != "run_live" || len(a.appliedRows(t)) != 0 {
		t.Fatalf("an answer to a live run: exit %d %+v", code, reply)
	}

	code, reply, errOut := a.answer(td, id)
	if code != 0 || !reply.Applied || reply.Replayed || reply.Seq == 0 || reply.Resumed != 4242 {
		t.Fatalf("the answer: exit %d %+v %s", code, reply, errOut)
	}
	rows := a.appliedRows(t)
	if len(rows) != 1 || rows[0].Seq != reply.Seq {
		t.Fatalf("rows %+v", rows)
	}
	d := rows[0].Data
	dec, _ := d["decision"].(map[string]any)
	if d["ask"] != "esc_one" || d["node"] != "n1" || d["run"] != "run-q" || d["attempt"] != float64(2) || d["revision"] != float64(1) || d["plan_basis"] != PlanBasis("plan-1", 3) ||
		d["applied_by"] != "thread-owner" || d["action"] != "retry" || dec["id"] != id || dec["by"] != "manager-task" || dec["via"] != decisions.ViaManagementMessage {
		t.Errorf("the row: %+v", d)
	}
	if len(td.spawned) != 1 || !td.spawned[0].Resume || td.spawned[0].Spec.RunID != "run-q" {
		t.Errorf("the resume: %+v", td.spawned)
	}
	// the lane's crw.decide reads exactly this row: the question's id and the node
	if q := Questions(mustLoad(t, w)); len(q) != 1 || q[0].Applied != id {
		t.Errorf("the question after the answer: %+v", q)
	}

	// the same answer again: the row that answers it is the answer, nothing is appended and nothing is resumed
	code, again, _ := a.answer(td, id)
	if code != 0 || !again.Replayed || again.Applied || again.Seq != reply.Seq || len(a.appliedRows(t)) != 1 || len(td.spawned) != 1 {
		t.Fatalf("the same answer again: exit %d %+v; %d rows, %d processes", code, again, len(a.appliedRows(t)), len(td.spawned))
	}
	// another decision for a question that is answered
	other := a.answered(t, "ud-other", func(_ *Binding, r *decisions.Record, ans *decisions.Answer) { ans.Option = "stop" })
	if code, r2, _ := a.answer(td, other); code != 1 || r2.Refusal == nil || r2.Refusal.Code != RefusedApplied || len(a.appliedRows(t)) != 1 {
		t.Errorf("another decision for an answered question: %d %+v", code, r2)
	}
	// the run is held by nobody again after the command: the lock was released
	if LockHeld(w.state, "run-q") {
		t.Error("the run lock is still held after the answer")
	}
}

// TestAnswerDoesNotResumeAnExhaustedBudget (CRW-1278, Argon F5 on 2a082733): the answer's resume starts the run process the way
// `crw flow resume RUN --detach` does, and a run whose spec's budget it has spent is refused before a process is started
// (resumeDetached asks budgetLeft): the answer is applied - its row is in the journal -, nothing is spawned, and the command says the
// run is not resumed with budget_exhausted, the tokens spent and the decision, exiting 1.
func TestAnswerDoesNotResumeAnExhaustedBudget(t *testing.T) {
	w := newWorld(t)
	r := w.newRun("run-q", "thread-owner")
	r.phase("build")
	r.completed("c_arch", "architect", "", "thread-arch", 60)
	r.call("c_ex", "executor", "n1", 2, []string{engine.EvPrepared, engine.EvRefused}, map[string]map[string]any{engine.EvRefused: {"reason": "pair_refused"}})
	r.flow(engine.EvEscalation, map[string]any{"reason": "pair_refused", "class": "configuration_fault", "node": "n1", "call_id": "c_ex", "escalation_id": "esc_one", "ask": "configuration"})
	r.finish(engine.RunCompleted, "")
	r.close()
	a := &asking{run: r, q: "esc_one", rec: RunRecord{RunID: "run-q", Owner: "thread-owner", PlanID: "plan-1", PlanRevision: 3}}
	td := w.deps()
	if err := WriteSpec(w.state, Spec{Schema: SpecSchema, RunID: "run-q", Workflow: "project-lane", Thread: "thread-owner", CreatedAt: "2026-10-12T08:00:00Z", Decision: "ud-night", TokenBudget: 50}); err != nil {
		t.Fatal(err)
	}
	id := a.answered(t, "ud-spent", nil)
	code, reply, errOut := a.answer(td, id)
	if code != 1 || !reply.Applied || reply.Refusal != nil || reply.Resumed != 0 {
		t.Errorf("the answer to a run with no budget left: exit %d %+v %q", code, reply, errOut)
	}
	if !strings.Contains(errOut, "not resumed") || !strings.Contains(errOut, engine.ReasonBudgetExhausted) || !strings.Contains(errOut, "60 of the 50 tokens") || !strings.Contains(errOut, "ud-night") {
		t.Errorf("the refusal of the resume: %q", errOut)
	}
	if len(td.spawned) != 0 {
		t.Errorf("an answer to a run with no budget left started %d processes: %+v", len(td.spawned), td.spawned)
	}
	if rows := a.appliedRows(t); len(rows) != 1 {
		t.Errorf("the answer appended %d decision_applied rows, want 1", len(rows))
	}
}

func mustLoad(t *testing.T, w *world) []engine.Event {
	t.Helper()
	rows, _, err := loadRun(w, "run-q")
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

var _ = store.ErrUserDecisionAbsent

// TestAnswerForGivesEachQuestionOfAJoinedAskItsPart (CRW-1381 d3): `crw flow answer --answer-for DIGEST=TEXT` gives each question an ask
// joins its own part of the recorded answer: the row's decision holds the parts by digest. A digest the ask does not carry, a part that is
// not text of the recorded answer and a flag that is not DIGEST=TEXT are refused (input errors, nothing appended); an answer with no part
// is every question's, with the warning single_answer_for_joined_ask in the row and on stderr.
func TestAnswerForGivesEachQuestionOfAJoinedAskItsPart(t *testing.T) {
	const d1, d2 = "1111111111111111111111111111111111111111111111111111111111111111", "2222222222222222222222222222222222222222222222222222222222222222"
	setup := func(t *testing.T) (*world, *asking) {
		w := newWorld(t)
		r := w.newRun("run-q", "thread-owner")
		r.phase("design")
		r.flow(engine.EvEscalation, map[string]any{"reason": "design_question", "class": "decision", "node": "n1", "escalation_id": "esc_one", "ask": "design",
			"detail": "[authority:policy] q one | [authority:environment] q two", "evidence": map[string]any{"questions": []any{d1, d2}}})
		r.finish(engine.RunCompleted, "")
		r.close()
		return w, &asking{run: r, q: "esc_one", rec: RunRecord{RunID: "run-q", Owner: "thread-owner", PlanID: "plan-1", PlanRevision: 3}}
	}
	text := func(_ *Binding, _ *decisions.Record, ans *decisions.Answer) {
		ans.Option, ans.Text = "", "No flag in 1.0. The shared runner is fine."
	}
	attempt0 := func(b *Binding, r *decisions.Record, ans *decisions.Answer) { b.Attempt = 0; text(b, r, ans) }

	w, a := setup(t)
	td := w.deps()
	for name, flags := range map[string][]string{
		"a digest the ask does not carry": {"--answer-for", strings.Repeat("3", 64) + "=No flag in 1.0."},
		"a part the record does not hold": {"--answer-for", d1 + "=words nobody recorded"},
		"no DIGEST=TEXT":                  {"--answer-for", "no-equals-sign"},
	} {
		id := a.answered(t, "ud-"+strings.ReplaceAll(name, " ", "-"), attempt0)
		if code, reply, errOut := a.answer(td, id, flags...); code == 0 || reply.Applied {
			t.Errorf("%s: exit %d %+v %s", name, code, reply, errOut)
		}
	}
	if n := len(a.appliedRows(t)); n != 0 {
		t.Fatalf("a refused answer appended %d rows", n)
	}
	id := a.answered(t, "ud-parts", attempt0)
	code, reply, errOut := a.answer(td, id, "--no-resume", "--answer-for", d1+"=No flag in 1.0.", "--answer-for", d2+"=The shared runner is fine.")
	if code != 0 || !reply.Applied {
		t.Fatalf("the answer with a part for each question: exit %d %+v %s", code, reply, errOut)
	}
	rows := a.appliedRows(t)
	dec, _ := rows[0].Data["decision"].(map[string]any)
	parts, _ := dec["answers"].(map[string]any)
	if len(rows) != 1 || parts[d1] != "No flag in 1.0." || parts[d2] != "The shared runner is fine." || len(rowWarningsOf(rows[0].Data)) != 0 {
		t.Errorf("the applied row: %v", rows[0].Data)
	}

	w2, a2 := setup(t)
	td2 := w2.deps()
	single := a2.answered(t, "ud-single", attempt0)
	code, reply, errOut = a2.answer(td2, single, "--no-resume")
	if code != 0 || !reply.Applied || !strings.Contains(strings.Join(rowWarningsOf(reply.Row), " "), "single_answer_for_joined_ask") || !strings.Contains(errOut, "single_answer_for_joined_ask") {
		t.Errorf("a single answer to a joined ask: exit %d %+v %s", code, reply, errOut)
	}
}

func rowWarningsOf(data map[string]any) []string {
	var out []string
	switch w := data["warnings"].(type) {
	case []string:
		out = w
	case []any:
		for _, x := range w {
			s, _ := x.(string)
			out = append(out, s)
		}
	}
	return out
}
