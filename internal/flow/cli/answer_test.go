package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/journal"
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
// decision for the same question is refused. An answer to a run a process holds asks that process to stop at its next boundary
// (one SIGUSR2), waits for it to let the run go and applies once, after the process's own last rows. crw.decide reads the row.
func TestAnswerAppliesOnce(t *testing.T) {
	w := newWorld(t)
	a := newLiveAsking(t, w)
	td := w.deps()
	id := a.answered(t, "ud-apply", nil)

	// the run process holds the run: the answer drains it (the fake plays the process: its paused end, then it lets the lock go)
	held, err := TakeLock(w.state, "run-q", ProcessCommand, td.Now())
	if err != nil {
		t.Fatal(err)
	}
	fake := newDrainFake(t, w)
	fake.on = func(n, pid int) {
		fake.write(func(j *journal.Journal, _ []engine.Event) { appendDrainedEnd(j) })
		held.Release()
	}
	td.Signal = fake.signal

	code, reply, errOut := a.answer(td, id, "--wait", "5")
	if code != 0 || !reply.Applied || reply.Replayed || reply.Seq == 0 || reply.Resumed != 4242 || !reply.Drained {
		t.Fatalf("the answer: exit %d %+v %s", code, reply, errOut)
	}
	if got := fake.calls(); len(got) != 1 || got[0].pid != os.Getpid() || got[0].sig != syscall.SIGUSR2 {
		t.Fatalf("the signals: %+v", got)
	}
	if ends := drainedEnds(mustLoad(t, w)); ends.pausedRows != 1 || ends.pausedEnd < 0 || reply.Seq <= ends.pausedEnd {
		t.Fatalf("the row %d after the drained end %+v", reply.Seq, ends)
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

	// the same answer again: the row that answers it is the answer, nothing is appended, nothing is signalled, nothing is resumed
	code, again, _ := a.answer(td, id)
	if code != 0 || !again.Replayed || again.Applied || again.Drained || again.Seq != reply.Seq || len(a.appliedRows(t)) != 1 || len(td.spawned) != 1 || len(fake.calls()) != 1 {
		t.Fatalf("the same answer again: exit %d %+v; %d rows, %d processes, %d signals", code, again, len(a.appliedRows(t)), len(td.spawned), len(fake.calls()))
	}
	// another decision for a question that is answered
	other := a.answered(t, "ud-other", func(_ *Binding, r *decisions.Record, ans *decisions.Answer) { ans.Option = "stop" })
	if code, r2, _ := a.answer(td, other); code != 1 || r2.Refusal == nil || r2.Refusal.Code != RefusedApplied || len(a.appliedRows(t)) != 1 || len(fake.calls()) != 1 {
		t.Errorf("another decision for an answered question: %d %+v", code, r2)
	}
	// the run is held by nobody again after the command: the lock was released
	if LockHeld(w.state, "run-q") {
		t.Error("the run lock is still held after the answer")
	}
}

// TestAnswerAppliesOnceOnAFreeLock is the ordinary answer (the base's TestAnswerAppliesOnce): a holder that is not a run process
// (its record names another command) is never signalled and the answer is refused with run_live; a run no process holds takes the
// answer under the lock at once, with no signal, and once.
func TestAnswerAppliesOnceOnAFreeLock(t *testing.T) {
	w := newWorld(t)
	a := newAsking(t, w)
	td := w.deps()
	signals := 0
	td.Signal = func(int, syscall.Signal) error { signals++; return nil }
	if err := WriteSpec(w.state, Spec{Schema: SpecSchema, RunID: "run-q", Workflow: "project-lane", Thread: "thread-owner", CreatedAt: "2026-10-12T08:00:00Z"}); err != nil {
		t.Fatal(err)
	}
	id := a.answered(t, "ud-apply", nil)

	// a holder whose record names no run process takes no answer and no signal
	lock, err := TakeLock(w.state, "run-q", "test", td.Now())
	if err != nil {
		t.Fatal(err)
	}
	code, reply, _ := a.answer(td, id)
	lock.Release()
	if code != 1 || reply.Refusal == nil || reply.Refusal.Code != "run_live" || len(a.appliedRows(t)) != 0 || signals != 0 {
		t.Fatalf("an answer to a run held by a command that is not a run process: exit %d %+v, %d signals", code, reply, signals)
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
	if LockHeld(w.state, "run-q") || signals != 0 {
		t.Errorf("after the answers: lock held %v, %d signals", LockHeld(w.state, "run-q"), signals)
	}
}

// TestAnswerAppliesToAPausedEnd (CRW-1487): a run the operator paused ends with a run/paused row and lets its lock go; an answer to
// its open question then applies as to any run no process holds: one decision_applied row after the paused row and one resume, and
// no signal is sent (no process is there to take one).
func TestAnswerAppliesToAPausedEnd(t *testing.T) {
	w := newWorld(t)
	r := w.newRun("run-q", "thread-owner")
	r.phase("build")
	r.call("c_ex", "executor", "n1", 2, []string{engine.EvPrepared, engine.EvRefused}, map[string]map[string]any{engine.EvRefused: {"reason": "pair_refused"}})
	r.flow(engine.EvEscalation, map[string]any{"reason": "pair_refused", "class": "configuration_fault", "node": "n1", "call_id": "c_ex", "escalation_id": "esc_one", "ask": "configuration"})
	r.endPaused()
	r.close()
	a := &asking{run: r, q: "esc_one", rec: RunRecord{RunID: "run-q", Owner: "thread-owner", PlanID: "plan-1", PlanRevision: 3}}
	td := w.deps()
	signalled := 0
	td.Signal = func(int, syscall.Signal) error { signalled++; return nil }
	if err := WriteSpec(w.state, Spec{Schema: SpecSchema, RunID: "run-q", Workflow: "project-lane", Thread: "thread-owner", CreatedAt: "2026-10-12T08:00:00Z"}); err != nil {
		t.Fatal(err)
	}
	id := a.answered(t, "ud-paused", nil)
	code, reply, errOut := a.answer(td, id)
	if code != 0 || !reply.Applied || reply.Seq == 0 || reply.Resumed != 4242 {
		t.Fatalf("the answer to a paused end: exit %d %+v %s", code, reply, errOut)
	}
	events := mustLoad(t, w)
	pausedAt := -1
	for i, e := range events {
		if e.Kind == engine.KindRun && e.Event == "paused" {
			pausedAt = i
		}
	}
	rows := a.appliedRows(t)
	if pausedAt < 0 || len(rows) != 1 || rows[0].Seq <= events[pausedAt].Seq {
		t.Errorf("the applied rows %+v after the paused row at %d", rows, pausedAt)
	}
	if len(td.spawned) != 1 || !td.spawned[0].Resume || td.spawned[0].Spec.RunID != "run-q" || signalled != 0 {
		t.Errorf("the resume: %+v, %d signals", td.spawned, signalled)
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

// newLiveAsking is the run of newAsking with no end: its process still runs it (the test holds its run lock as `_run`).
func newLiveAsking(t *testing.T, w *world) *asking {
	t.Helper()
	r := w.newRun("run-q", "thread-owner")
	r.phase("build")
	r.call("c_ex", "executor", "n1", 2, []string{engine.EvPrepared, engine.EvRefused}, map[string]map[string]any{engine.EvRefused: {"reason": "pair_refused"}})
	r.flow(engine.EvEscalation, map[string]any{"reason": "pair_refused", "class": "configuration_fault", "node": "n1", "call_id": "c_ex", "escalation_id": "esc_one", "ask": "configuration"})
	r.close()
	if err := WriteSpec(w.state, Spec{Schema: SpecSchema, RunID: "run-q", Workflow: "project-lane", Thread: "thread-owner", CreatedAt: "2026-10-12T08:00:00Z"}); err != nil {
		t.Fatal(err)
	}
	return &asking{run: r, q: "esc_one", rec: RunRecord{RunID: "run-q", Owner: "thread-owner", PlanID: "plan-1", PlanRevision: 3}}
}

// drainFake is td.Signal for the drain tests (CRW-1492): it plays, synchronously inside the answer's c.Signal call (on the command's
// goroutine), the run process the answer drains and, where a test needs it, another answer and its resume. No test runs two commands
// at once: every interleaving is built inside one command. The fake opens a store of its own for each write (the world's is closed
// while td.do runs) and closes it before it returns, so its rows land before the answer opens the journal that appends its row. Its
// log is guarded: a goroutine that releases a hold later touches only that *Lock.
type drainFake struct {
	t  *testing.T
	w  *world
	mu sync.Mutex
	// log is each signal the answer sent, with the holder record read at that moment
	log []signalSent
	// on is what the fake does at the n-th signal (0-based); nil does nothing
	on func(n, pid int)
}

type signalSent struct {
	pid    int
	sig    syscall.Signal
	holder lockRecord
}

func newDrainFake(t *testing.T, w *world) *drainFake { return &drainFake{t: t, w: w} }

func (f *drainFake) signal(pid int, sig syscall.Signal) error {
	rec, _ := lockHolderRecord(f.w.state, "run-q")
	f.mu.Lock()
	n := len(f.log)
	f.log = append(f.log, signalSent{pid: pid, sig: sig, holder: rec})
	f.mu.Unlock()
	if f.on != nil {
		f.on(n, pid)
	}
	return nil
}

func (f *drainFake) calls() []signalSent {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]signalSent(nil), f.log...)
}

// write appends rows to the run's journal through a store and a journal of the fake's own, both closed before it returns.
func (f *drainFake) write(fn func(j *journal.Journal, events []engine.Event)) {
	f.t.Helper()
	st, err := store.Open(ctx, filepath.Join(f.w.state, "relay.sqlite3"), "")
	if err != nil {
		f.t.Fatal(err)
	}
	defer st.Close()
	j, err := journal.Open(ctx, st, "run-q", journal.Identity{}, journal.Options{Immediate: true})
	if err != nil {
		f.t.Fatal(err)
	}
	fn(j, j.Events())
	if err := j.Close(); err != nil || j.Err() != nil {
		f.t.Fatalf("the fake's rows: %v %v", err, j.Err())
	}
}

// decisionRecord reads a decision record through a store of the fake's own.
func (f *drainFake) decisionRecord(id string) decisions.Record {
	f.t.Helper()
	st, err := store.Open(ctx, filepath.Join(f.w.state, "relay.sqlite3"), "")
	if err != nil {
		f.t.Fatal(err)
	}
	defer st.Close()
	rec, err := st.Get(ctx, id)
	if err != nil {
		f.t.Fatal(err)
	}
	return rec
}

// withdraw withdraws a decision record through a store of the fake's own, as `crw relay decision-withdraw` does: without the run lock.
func (f *drainFake) withdraw(id, reason string) {
	f.t.Helper()
	st, err := store.Open(ctx, filepath.Join(f.w.state, "relay.sqlite3"), "")
	if err != nil {
		f.t.Fatal(err)
	}
	defer st.Close()
	if _, err := st.Update(ctx, id, func(_ context.Context, r decisions.Record) (decisions.Record, error) {
		return decisions.Withdraw(r, reason)
	}); err != nil {
		f.t.Fatal(err)
	}
}

// appendDrainedEnd appends what a drained run process writes (engine.Control.Drain): the run_paused row of the answer's ask, then
// the run/paused end by `answer`.
func appendDrainedEnd(j *journal.Journal) {
	j.Append(engine.Event{RunID: "run-q", Kind: engine.KindFlow, Event: engine.EvRunPaused, Data: map[string]any{"by": "answer", "reason": drainReason}})
	j.Append(engine.Event{RunID: "run-q", Kind: engine.KindRun, Event: "paused", Data: map[string]any{"status": engine.RunPaused, "by": "answer", "reason": drainReason}})
}

// holdByHand holds the run lock of run with a holder record written by hand (lockRecord, as control_safety_test.go writes one): a holder
// of another pid than the test's, which the fake never kills.
func holdByHand(t *testing.T, state, run string, pid int, command, since string) *Lock {
	t.Helper()
	if err := os.MkdirAll(RunDir(state, run), 0o700); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(lockPath(state, run), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		t.Fatalf("the hold of %s as %q: %v", run, command, err)
	}
	raw, _ := json.Marshal(lockRecord{PID: pid, Since: since, Command: command})
	if err := f.Truncate(0); err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt(append(raw, '\n'), 0); err != nil {
		t.Fatal(err)
	}
	return &Lock{f: f}
}

// drainEnds counts the rows of a drain in a run's rows: the run_paused rows by `answer` and the seq of the last run/paused end.
type drainEnds struct {
	pausedRows int
	pausedEnd  int
	lastSeq    int
}

func drainedEnds(events []engine.Event) drainEnds {
	out := drainEnds{pausedEnd: -1}
	for _, e := range events {
		out.lastSeq = max(out.lastSeq, e.Seq)
		switch {
		case e.Kind == engine.KindFlow && e.Event == engine.EvRunPaused && e.Data["by"] == "answer":
			out.pausedRows++
		case e.Kind == engine.KindRun && e.Event == "paused":
			out.pausedEnd = e.Seq
		}
	}
	return out
}

// TestAnswerDrainsALiveRunAndAppliesOnce (CRW-1492): an answer to a run its process holds is judged on the journal first, then asks
// that process (one SIGUSR2) to stop at its next boundary, waits (--wait) for it to let the run go - here a while after its paused
// end is stored -, and appends one decision_applied row under the lock, after the drained process's rows, and resumes the run once.
// The --json reply says the answer drained the run.
func TestAnswerDrainsALiveRunAndAppliesOnce(t *testing.T) {
	w := newWorld(t)
	a := newLiveAsking(t, w)
	td := w.deps()
	id := a.answered(t, "ud-drain", nil)
	held, err := TakeLock(w.state, "run-q", ProcessCommand, td.Now())
	if err != nil {
		t.Fatal(err)
	}
	var released sync.WaitGroup
	fake := newDrainFake(t, w)
	fake.on = func(n, pid int) {
		fake.write(func(j *journal.Journal, _ []engine.Event) { appendDrainedEnd(j) })
		// the process lets the run go a moment after its last row: the answer polls for the lock
		released.Add(1)
		go func() {
			defer released.Done()
			time.Sleep(450 * time.Millisecond)
			held.Release()
		}()
	}
	td.Signal = fake.signal
	code, out, errOut := td.do("answer", "--state", w.state, "--decision", id, "--run", "run-q", "--node", "n1", "--question", "esc_one", "--revision", "1", "--thread", "thread-owner", "--wait", "5", "--json")
	released.Wait()
	var reply AnswerReply
	// the fake spawner marks its spawn on stdout before the reply
	if err := json.Unmarshal([]byte(strings.TrimPrefix(out, "<<spawned>>\n")), &reply); err != nil {
		t.Fatalf("the reply: %v: %q", err, out)
	}
	if code != 0 || !reply.OK || !reply.Applied || !reply.DrainAsked || !reply.Drained || reply.Refusal != nil || reply.Resumed != 4242 {
		t.Fatalf("the answer to a live run: exit %d %+v %s", code, reply, errOut)
	}
	if got := fake.calls(); len(got) != 1 || got[0].sig != syscall.SIGUSR2 || got[0].pid != os.Getpid() || got[0].holder.Command != ProcessCommand {
		t.Fatalf("the signals: %+v", got)
	}
	ends := drainedEnds(mustLoad(t, w))
	rows := a.appliedRows(t)
	if ends.pausedRows != 1 || ends.pausedEnd < 0 || len(rows) != 1 || rows[0].Seq != reply.Seq || rows[0].Seq <= ends.pausedEnd || rows[0].Seq != ends.lastSeq {
		t.Fatalf("the rows: %+v, applied %+v", ends, rows)
	}
	if len(td.spawned) != 1 || !td.spawned[0].Resume {
		t.Errorf("the resume: %+v", td.spawned)
	}
	if LockHeld(w.state, "run-q") {
		t.Error("the run lock is still held after the answer")
	}
}

// TestAnswerRefusesBeforeAnySignal (CRW-1492): an answer the journal refuses (a question asked again since) is refused on a run its
// process holds before anything is asked of the process: no signal, no row.
func TestAnswerRefusesBeforeAnySignal(t *testing.T) {
	w := newWorld(t)
	a := newLiveAsking(t, w)
	td := w.deps()
	id := a.answered(t, "ud-stale", nil)
	r2, err := w.reopen("run-q")
	if err != nil {
		t.Fatal(err)
	}
	r2.call("c_ex2", "executor", "n1", 3, []string{engine.EvPrepared, engine.EvRefused}, map[string]map[string]any{engine.EvRefused: {"reason": "pair_refused"}})
	r2.flow(engine.EvEscalation, map[string]any{"reason": "pair_refused", "node": "n1", "call_id": "c_ex2", "escalation_id": "esc_two", "ask": "configuration"})
	r2.close()
	held, err := TakeLock(w.state, "run-q", ProcessCommand, td.Now())
	if err != nil {
		t.Fatal(err)
	}
	defer held.Release()
	fake := newDrainFake(t, w)
	td.Signal = fake.signal
	code, reply, _ := a.answer(td, id, "--wait", "5")
	if code != 1 || reply.Refusal == nil || reply.Refusal.Code != RefusedStaleRevison || reply.Drained || reply.DrainAsked || len(fake.calls()) != 0 || len(a.appliedRows(t)) != 0 || len(td.spawned) != 0 {
		t.Fatalf("a stale answer to a live run: exit %d %+v, %d signals", code, reply, len(fake.calls()))
	}
}

// TestAnswerDoesNotSignalAnUnknownHolder (CRW-1492): a holder whose record names no command (an older binary, a record not written
// yet) and a writer's hold (audit) are not run processes the answer may drain: run_live, nothing signalled, nothing appended.
func TestAnswerDoesNotSignalAnUnknownHolder(t *testing.T) {
	saved := writerWait
	writerWait = 100 * time.Millisecond
	t.Cleanup(func() { writerWait = saved })
	for _, command := range []string{"", "audit"} {
		t.Run("holder "+strconv.Quote(command), func(t *testing.T) {
			w := newWorld(t)
			a := newLiveAsking(t, w)
			td := w.deps()
			id := a.answered(t, "ud-unknown", nil)
			held := holdByHand(t, w.state, "run-q", 999991, command, "2026-10-12T09:00:00Z")
			defer held.Release()
			fake := newDrainFake(t, w)
			td.Signal = fake.signal
			code, reply, _ := a.answer(td, id, "--wait", "0")
			if code != 1 || reply.Refusal == nil || reply.Refusal.Code != "run_live" || reply.Drained || reply.DrainAsked || len(fake.calls()) != 0 || len(a.appliedRows(t)) != 0 {
				t.Fatalf("an answer to a run held by %q: exit %d %+v, %d signals", command, code, reply, len(fake.calls()))
			}
		})
	}
}

// TestAnswerWaitZeroAsksTheDrainAndReturns (CRW-1492): with no --wait the answer asks the drain and returns at once: one SIGUSR2,
// run_draining (exit 1), no row; the same answer once the run is paused applies it.
func TestAnswerWaitZeroAsksTheDrainAndReturns(t *testing.T) {
	w := newWorld(t)
	a := newLiveAsking(t, w)
	td := w.deps()
	id := a.answered(t, "ud-zero", nil)
	held, err := TakeLock(w.state, "run-q", ProcessCommand, td.Now())
	if err != nil {
		t.Fatal(err)
	}
	fake := newDrainFake(t, w)
	td.Signal = fake.signal
	began := time.Now()
	code, reply, _ := a.answer(td, id)
	took := time.Since(began)
	// the stop is asked, not made yet: drain_asked, and not drained (the process still holds the run)
	if code != 1 || reply.Refusal == nil || reply.Refusal.Code != "run_draining" || !reply.DrainAsked || reply.Drained || reply.Applied || took > time.Second {
		t.Fatalf("an answer with no wait to a live run: exit %d %+v in %s", code, reply, took)
	}
	if !strings.Contains(reply.Refusal.Detail, "next boundary") || !strings.Contains(reply.Refusal.Detail, "the same crw flow answer again") {
		t.Errorf("the refusal's words: %q", reply.Refusal.Detail)
	}
	if got := fake.calls(); len(got) != 1 || got[0].sig != syscall.SIGUSR2 || len(a.appliedRows(t)) != 0 || len(td.spawned) != 0 {
		t.Fatalf("the signals %+v, %d rows, %d processes", got, len(a.appliedRows(t)), len(td.spawned))
	}
	// the process ends paused by the answer and lets the run go; the same answer again applies once and resumes
	fake.write(func(j *journal.Journal, _ []engine.Event) { appendDrainedEnd(j) })
	held.Release()
	code, reply, errOut := a.answer(td, id)
	if code != 0 || !reply.Applied || reply.Drained || reply.Resumed != 4242 || len(fake.calls()) != 1 || len(a.appliedRows(t)) != 1 {
		t.Fatalf("the same answer once the run is paused: exit %d %+v %s", code, reply, errOut)
	}
}

// TestAnswerSignalsANewHolderOnce (CRW-1492): a run process that takes the run after the first drained one let it go (a resume
// started by hand: the question is still open) is drained too, once, after its own re-judge; each holder is signalled exactly once
// and the answer applies once.
func TestAnswerSignalsANewHolderOnce(t *testing.T) {
	w := newWorld(t)
	a := newLiveAsking(t, w)
	td := w.deps()
	id := a.answered(t, "ud-new-holder", nil)
	first, err := TakeLock(w.state, "run-q", ProcessCommand, td.Now())
	if err != nil {
		t.Fatal(err)
	}
	const n2 = 999992
	var second *Lock
	fake := newDrainFake(t, w)
	fake.on = func(n, pid int) {
		switch n {
		case 0:
			// the first process ends paused, and a resume someone started by hand takes the run: its own started row
			fake.write(func(j *journal.Journal, _ []engine.Event) {
				appendDrainedEnd(j)
			})
			first.Release()
			second = holdByHand(t, w.state, "run-q", n2, "resume", "2026-10-12T09:00:05Z")
			fake.write(func(j *journal.Journal, _ []engine.Event) {
				j.Append(engine.Event{RunID: "run-q", Kind: engine.KindRun, Event: "started", Data: map[string]any{"workflow": "project-lane", "script_digest": "d", "args_digest": "a"}})
			})
		case 1:
			fake.write(func(j *journal.Journal, _ []engine.Event) { appendDrainedEnd(j) })
			second.Release()
		}
	}
	td.Signal = fake.signal
	code, reply, errOut := a.answer(td, id, "--wait", "5")
	if code != 0 || !reply.Applied || !reply.Drained || reply.Resumed != 4242 {
		t.Fatalf("the answer: exit %d %+v %s", code, reply, errOut)
	}
	got := fake.calls()
	if len(got) != 2 || got[0].pid != os.Getpid() || got[1].pid != n2 || got[0].sig != syscall.SIGUSR2 || got[1].sig != syscall.SIGUSR2 {
		t.Fatalf("the signals: %+v", got)
	}
	if ends := drainedEnds(mustLoad(t, w)); ends.pausedRows != 2 || len(a.appliedRows(t)) != 1 || a.appliedRows(t)[0].Seq != ends.lastSeq || len(td.spawned) != 1 {
		t.Fatalf("the rows %+v, applied %+v, %d processes", ends, a.appliedRows(t), len(td.spawned))
	}
}

// TestAnAnswerDoesNotDrainTheResumeOfAnotherAnswer (CRW-1492, critic R1-1): answer B drains the run process P1; while it waits, answer A
// takes the free lock, applies its decision to the same question and starts its resume N2. B re-judges on the journal before it
// signals a new holder: the question is answered, so B is refused (another decision) or replayed (the same decision) and N2 - A's
// resume - is never signalled; B resumes nothing.
func TestAnAnswerDoesNotDrainTheResumeOfAnotherAnswer(t *testing.T) {
	for _, tc := range []struct {
		name string
		// aDecision is the decision answer A applies (B applies ud-b)
		aDecision string
	}{
		{"another decision", "ud-a"},
		{"the same decision", "ud-b"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newWorld(t)
			a := newLiveAsking(t, w)
			td := w.deps()
			a.answered(t, "ud-a", func(_ *Binding, _ *decisions.Record, ans *decisions.Answer) { ans.Option = "stop" })
			b := a.answered(t, "ud-b", nil)
			p1, err := TakeLock(w.state, "run-q", ProcessCommand, td.Now())
			if err != nil {
				t.Fatal(err)
			}
			const n2 = 999993
			var resumed *Lock
			seqAtResume := 0
			fake := newDrainFake(t, w)
			fake.on = func(n, pid int) {
				if n != 0 {
					return
				}
				// P1 is drained: its paused end, and it lets the run go
				fake.write(func(j *journal.Journal, _ []engine.Event) { appendDrainedEnd(j) })
				p1.Release()
				// answer A: it takes the free lock, appends the row it would append, and lets the lock go
				hold, err := TakeLock(w.state, "run-q", "answer", td.Now())
				if err != nil {
					t.Fatal(err)
				}
				recA := fake.decisionRecord(tc.aDecision)
				fake.write(func(j *journal.Journal, events []engine.Event) {
					app, refusal := Judge(recA, events, a.rec, Claim{Decision: tc.aDecision, Run: "run-q", Node: "n1", Question: "esc_one", Revision: 1, By: "thread-owner"})
					if refusal != nil || app.Replayed {
						t.Fatalf("answer A: %+v %+v", app, refusal)
					}
					ev := j.Append(engine.Event{RunID: "run-q", Kind: engine.KindFlow, Event: ops.EvDecisionApplied, Data: app.Data})
					seqAtResume = ev.Seq
				})
				hold.Release()
				// A's resume takes the run
				resumed = holdByHand(t, w.state, "run-q", n2, ProcessCommand, "2026-10-12T09:00:07Z")
			}
			td.Signal = fake.signal
			code, reply, errOut := a.answer(td, b, "--wait", "5")
			if resumed == nil {
				t.Fatalf("B never drained P1: exit %d %+v %s", code, reply, errOut)
			}
			stillHeld := LockHeld(w.state, "run-q")
			resumed.Release()
			switch tc.aDecision {
			case "ud-a":
				if code != 1 || reply.Refusal == nil || reply.Refusal.Code != RefusedApplied || reply.Applied {
					t.Fatalf("B after A applied another decision: exit %d %+v %s", code, reply, errOut)
				}
			default:
				if code != 0 || !reply.Replayed || reply.Applied || reply.Refusal != nil {
					t.Fatalf("B after A applied the same decision: exit %d %+v %s", code, reply, errOut)
				}
			}
			if got := fake.calls(); len(got) != 1 || got[0].pid != os.Getpid() || got[0].sig != syscall.SIGUSR2 {
				t.Fatalf("the signals (only P1, never A's resume %d): %+v", n2, got)
			}
			if len(td.spawned) != 0 {
				t.Errorf("B started %d processes: %+v", len(td.spawned), td.spawned)
			}
			ends := drainedEnds(mustLoad(t, w))
			rows := a.appliedRows(t)
			if ends.pausedRows != 1 || len(rows) != 1 || rows[0].Data["decision"].(map[string]any)["id"] != tc.aDecision || ends.lastSeq != seqAtResume {
				t.Fatalf("the rows: %+v, applied %+v, last row %d, A's row %d", ends, rows, ends.lastSeq, seqAtResume)
			}
			if !stillHeld {
				t.Error("A's resume did not hold the run when B returned")
			}
		})
	}
}

// TestARefusedAnswerNeverResumes (CRW-1492, the earlier lane's A2 and A3): only the answer that appends the row resumes the run. (a)
// is TestAnAnswerDoesNotDrainTheResumeOfAnotherAnswer. (b) A run drained by an answer with no wait that asks its question again before
// its paused end: the same answer on the free lock is stale_revision, nothing is resumed, and the run stays paused by `answer`. (c) An
// applying answer with --no-resume after a drain: no process is started by anyone.
func TestARefusedAnswerNeverResumes(t *testing.T) {
	t.Run("a new revision before the paused end", func(t *testing.T) {
		w := newWorld(t)
		a := newLiveAsking(t, w)
		td := w.deps()
		id := a.answered(t, "ud-old", nil)
		held, err := TakeLock(w.state, "run-q", ProcessCommand, td.Now())
		if err != nil {
			t.Fatal(err)
		}
		fake := newDrainFake(t, w)
		fake.on = func(n, pid int) {
			fake.write(func(j *journal.Journal, _ []engine.Event) {
				r := &run{w: w, id: "run-q", j: j}
				r.call("c_ex2", "executor", "n1", 3, []string{engine.EvPrepared, engine.EvRefused}, map[string]map[string]any{engine.EvRefused: {"reason": "pair_refused"}})
				r.flow(engine.EvEscalation, map[string]any{"reason": "pair_refused", "node": "n1", "call_id": "c_ex2", "escalation_id": "esc_two", "ask": "configuration"})
				appendDrainedEnd(j)
			})
		}
		td.Signal = fake.signal
		code, reply, _ := a.answer(td, id, "--wait", "0")
		if code != 1 || reply.Refusal == nil || reply.Refusal.Code != "run_draining" || !reply.DrainAsked || reply.Drained {
			t.Fatalf("the drain: exit %d %+v", code, reply)
		}
		held.Release()
		code, reply, _ = a.answer(td, id)
		if code != 1 || reply.Refusal == nil || reply.Refusal.Code != RefusedStaleRevison || reply.Applied || len(td.spawned) != 0 || len(fake.calls()) != 1 || len(a.appliedRows(t)) != 0 {
			t.Fatalf("the same answer after a new revision: exit %d %+v, %d processes, %d signals", code, reply, len(td.spawned), len(fake.calls()))
		}
		if info := Summarize(a.rec, mustLoad(t, w), false, 0); info.State != StatePaused || !strings.HasPrefix(info.Paused, "answer: ") {
			t.Errorf("the run after the refusal: %s paused %q", info.State, info.Paused)
		}
	})
	t.Run("no resume", func(t *testing.T) {
		w := newWorld(t)
		a := newLiveAsking(t, w)
		td := w.deps()
		id := a.answered(t, "ud-no-resume", nil)
		held, err := TakeLock(w.state, "run-q", ProcessCommand, td.Now())
		if err != nil {
			t.Fatal(err)
		}
		fake := newDrainFake(t, w)
		fake.on = func(n, pid int) {
			fake.write(func(j *journal.Journal, _ []engine.Event) { appendDrainedEnd(j) })
			held.Release()
		}
		td.Signal = fake.signal
		code, reply, errOut := a.answer(td, id, "--wait", "5", "--no-resume")
		if code != 0 || !reply.Applied || !reply.Drained || reply.Resumed != 0 || len(td.spawned) != 0 || len(a.appliedRows(t)) != 1 {
			t.Fatalf("an applying answer with --no-resume: exit %d %+v %s, %d processes", code, reply, errOut, len(td.spawned))
		}
	})
}

// TestAnswerRefusesADecisionWithdrawnWhileItWaits (CRW-1492, x1501 P1): `crw relay decision-withdraw` changes a record without the run
// lock (an answered record may be withdrawn), so a decision withdrawn while the answer waits for the run it drained - after the
// signal, before the answer holds the lock - is judged as the store holds it then: the answer is refused not_answered, naming the
// withdrawal and its reason; no decision_applied row is appended and nothing is resumed. (a) The drained process lets the run go: the
// record read again under the lock refuses. (b) A run process takes the run after the drained one (a resume started by hand): the
// re-judge before its signal reads the record again and refuses, and that process is never signalled.
func TestAnswerRefusesADecisionWithdrawnWhileItWaits(t *testing.T) {
	const reason = "the parent took the question back"
	for _, tc := range []struct {
		name      string
		newHolder bool
	}{
		{"the drained process lets the run go", false},
		{"a new run process takes the run", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newWorld(t)
			a := newLiveAsking(t, w)
			td := w.deps()
			id := a.answered(t, "ud-withdrawn", nil)
			held, err := TakeLock(w.state, "run-q", ProcessCommand, td.Now())
			if err != nil {
				t.Fatal(err)
			}
			const n2 = 999994
			var second *Lock
			fake := newDrainFake(t, w)
			fake.on = func(n, pid int) {
				if n != 0 {
					return
				}
				// the process ends paused for the answer; meanwhile the decision is withdrawn; the process lets the run go
				fake.write(func(j *journal.Journal, _ []engine.Event) { appendDrainedEnd(j) })
				fake.withdraw(id, reason)
				held.Release()
				if tc.newHolder {
					second = holdByHand(t, w.state, "run-q", n2, "resume", "2026-10-12T09:00:05Z")
					fake.write(func(j *journal.Journal, _ []engine.Event) {
						j.Append(engine.Event{RunID: "run-q", Kind: engine.KindRun, Event: "started", Data: map[string]any{"workflow": "project-lane", "script_digest": "d", "args_digest": "a"}})
					})
				}
			}
			td.Signal = fake.signal
			code, reply, errOut := a.answer(td, id, "--wait", "2")
			if second != nil {
				second.Release()
			}
			if code != 1 || reply.Refusal == nil || reply.Refusal.Code != RefusedNotAnswered || reply.Applied || reply.OK {
				t.Fatalf("an answer whose decision was withdrawn while it waited: exit %d %+v %s", code, reply, errOut)
			}
			if !strings.Contains(reply.Refusal.Detail, "withdrawn") || !strings.Contains(reply.Refusal.Detail, reason) {
				t.Errorf("the refusal does not name the withdrawal and its reason: %q", reply.Refusal.Detail)
			}
			if got := fake.calls(); len(got) != 1 || got[0].pid != os.Getpid() || got[0].sig != syscall.SIGUSR2 {
				t.Fatalf("the signals (only the first process, never %d): %+v", n2, got)
			}
			if rows := a.appliedRows(t); len(rows) != 0 {
				t.Fatalf("a withdrawn decision was applied: %+v", rows)
			}
			if len(td.spawned) != 0 {
				t.Fatalf("an answer of a withdrawn decision resumed the run: %+v", td.spawned)
			}
		})
	}
}

// TestAnswerSaysADrainIsAskedOfAHolderThatNeverLetsGo (CRW-1492, x1501 P2): a run process an older crw runs writes the same holder
// record (pid, since, command `_run`) and takes no SIGUSR2 (the Go runtime drops a signal no channel is notified of): the kill
// succeeds and nothing drains. The answer does not take the kill for a drain: after --wait it is refused run_draining with `drained`
// false and `drain_asked` true, its words say the stop was asked and the process still holds the run, and what to do when it does
// not stop; nothing is appended or resumed. The reply is decoded as JSON members, as a client reads it.
func TestAnswerSaysADrainIsAskedOfAHolderThatNeverLetsGo(t *testing.T) {
	w := newWorld(t)
	a := newLiveAsking(t, w)
	td := w.deps()
	id := a.answered(t, "ud-older-holder", nil)
	held, err := TakeLock(w.state, "run-q", ProcessCommand, td.Now())
	if err != nil {
		t.Fatal(err)
	}
	defer held.Release()
	// the holder takes the signal and does nothing with it: it never ends paused and never lets the run go
	fake := newDrainFake(t, w)
	td.Signal = fake.signal
	began := time.Now()
	code, out, errOut := td.do("answer", "--state", w.state, "--decision", id, "--run", "run-q", "--node", "n1", "--question", "esc_one", "--revision", "1", "--thread", "thread-owner", "--wait", "1", "--json")
	took := time.Since(began)
	var reply map[string]any
	if err := json.Unmarshal([]byte(out), &reply); err != nil {
		t.Fatalf("the reply: %v: %q %s", err, out, errOut)
	}
	refusal, _ := reply["refusal"].(map[string]any)
	if code != 1 || refusal == nil || refusal["Code"] != RefusedRunDraining || reply["applied"] == true || took < time.Second {
		t.Fatalf("an answer to a holder that never lets go: exit %d in %s, %v %s", code, took, reply, errOut)
	}
	if reply["drained"] == true || reply["drain_asked"] != true {
		t.Errorf("the reply claims a drain the holder never made (drained %v, drain_asked %v): %v", reply["drained"], reply["drain_asked"], reply)
	}
	detail, _ := refusal["Detail"].(string)
	if strings.Contains(detail, "run run-q stops at its next boundary for this answer") {
		t.Errorf("the refusal says the run stops, from the kill alone: %q", detail)
	}
	for _, want := range []string{"asked", "still holds the run", "older crw", "crw flow pause run-q"} {
		if !strings.Contains(detail, want) {
			t.Errorf("the refusal does not say %q: %q", want, detail)
		}
	}
	if got := fake.calls(); len(got) != 1 || got[0].sig != syscall.SIGUSR2 || len(a.appliedRows(t)) != 0 || len(td.spawned) != 0 {
		t.Fatalf("the signals %+v, %d rows, %d processes", got, len(a.appliedRows(t)), len(td.spawned))
	}
}
