package scenario

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/cli"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/journal"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/ops"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/script"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/workflows"
	"github.com/thisisjun786/codex-relay-workflow/internal/manage"
)

// The night batch on the scenario lab (CRW-1205): the decision's issues are the nodes of the plan's milestone, the project lane runs
// them inside the run's token budget, and the run digest the journal gives is what the morning report is made from.

// nightArgs are the arguments `crw flow start night-batch --decision D` gives the workflow for a decision.
func nightArgs(issues ...string) json.RawMessage {
	batch := cli.NightBatch{Schema: cli.NightBatchSchema, Label: "night-2026-10-12", Issues: issues, Plan: "plan-1", Milestone: "M1", TokenBudget: 100000, Deadline: "2026-10-13T06:00:00Z", Cap: 3}
	return batch.Args("ud-night-1")
}

// NightBatch runs the night-batch workflow as a new run, or as the resume of one, over the lab's journal and world.
func (l *Lab) NightBatch(args json.RawMessage, resume bool) *engine.Result {
	l.t.Helper()
	prog, err := workflows.Load(cli.NightBatchWorkflow)
	if err != nil {
		l.t.Fatal(err)
	}
	cfg := l.Config(resume)
	cfg.Args = args
	cfg.Workflows = func(name, path string) (*script.Program, error) { return workflows.Load(name) }
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	l.cancel = cancel
	defer cancel()
	res, err := engine.Run(ctx, prog, cfg)
	if err != nil {
		l.t.Fatal(err)
	}
	l.journal.flush()
	return res
}

type nightResult struct {
	State    string            `json:"state"`
	Decision string            `json:"decision"`
	Selected []string          `json:"selected"`
	Nodes    map[string]string `json:"nodes"`
	Missing  []string          `json:"missing"`
	Extra    []string          `json:"extra"`
	Lanes    []struct {
		Node  string `json:"node"`
		State string `json:"state"`
	} `json:"lanes"`
}

// digestOf is the digest of the lab's run as the journal in the store gives it.
func (l *Lab) digestOf(t *testing.T, issues []string, budget int64, live bool) cli.BatchDigest {
	t.Helper()
	events, ok, err := journal.Load(context.Background(), l.Store, l.RunID)
	if err != nil || !ok {
		t.Fatalf("the journal: %v %v", ok, err)
	}
	rec := cli.RunRecord{RunID: l.RunID, Workflow: cli.NightBatchWorkflow, Owner: "parent", StartedAt: "2026-10-12T09:00:00Z"}
	return cli.BuildDigest(cli.DigestInput{Run: rec, Events: events, Spec: cli.Spec{TokenBudget: budget, Deadline: "2026-10-13T06:00:00Z"}, Label: "night-2026-10-12", Issues: issues, Now: time.Date(2026, 10, 13, 6, 30, 0, 0, time.UTC), Live: live,
		Result: &cli.Result{Run: l.RunID, EndedAt: "2026-10-13T05:50:00Z"}})
}

// reportOf renders the morning report of a digest through the manager's own report command code, after the strict JSON round trip a
// digest file takes between `crw flow digest` and `crw manage batch report --digest`.
func reportOf(t *testing.T, d cli.BatchDigest) (string, manage.BatchDigest) {
	t.Helper()
	raw, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	var md manage.BatchDigest
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&md); err != nil {
		t.Fatalf("the digest is not the manager's crw-batch-digest/1: %v\n%s", err, raw)
	}
	return manage.BatchReport(md, time.Date(2026, 10, 13, 6, 30, 0, 0, time.UTC)), md
}

// The workflow runs the issues the decision selected, as the nodes of the milestone it names: both land, the digest says so with the
// journal's evidence, and a milestone that holds another set of issues than the selection releases nothing and asks the manager.
func TestNightBatchRunsOnlyTheSelectedIssues(t *testing.T) {
	l := NewLab(t, twoNodes()...)
	res := l.NightBatch(nightArgs("CRW-A", "CRW-B"), false)
	got := Value[nightResult](t, res)
	if got.State != "closed" || got.Decision != "ud-night-1" || got.Nodes["CRW-A"] != "A" || got.Nodes["CRW-B"] != "B" || len(got.Lanes) != 2 {
		t.Fatalf("the batch: %+v", got)
	}
	if len(l.Relay.Landed) != 2 {
		t.Fatalf("landed %v", l.Relay.Landed)
	}
	if phases := l.FlowRows("phase"); len(phases) < 5 || str(phases[0].Data, "title") != "select" {
		t.Errorf("phases %+v", phases)
	}

	// a selection the plan does not hold: nothing is released
	other := NewLab(t, twoNodes()...)
	res = other.NightBatch(nightArgs("CRW-A", "CRW-Z"), false)
	mismatch := Value[nightResult](t, res)
	if mismatch.State != "selection_mismatch" || len(mismatch.Missing) != 1 || mismatch.Missing[0] != "CRW-Z" || len(mismatch.Extra) != 1 || mismatch.Extra[0] != "CRW-B" {
		t.Fatalf("the mismatch: %+v", mismatch)
	}
	if len(other.Rows("crw.release")) != 0 || len(other.Calls()) != 0 || len(other.FlowRows(engine.EvEscalation)) != 1 {
		t.Errorf("a mismatch released %d nodes, made %d calls, escalated %d times", len(other.Rows("crw.release")), len(other.Calls()), len(other.FlowRows(engine.EvEscalation)))
	}
}

// TestNightBatchBudgetPauses: the run's token budget is the run's own. When it is spent the calls that would send end held
// (budget_exhausted), no issue is started on credit, the run completes with the lanes held, and the digest names the budget as the
// reason it stopped and carries each issue that did not land with its reason.
func TestNightBatchBudgetPauses(t *testing.T) {
	l := NewLab(t, twoNodes()...)
	// the two background explorers are 2 (CRW-1466), the architect and the audit 20 (the warning): 22, and A's build takes it over
	l.Budget = 25
	res := l.NightBatch(nightArgs("CRW-A", "CRW-B"), false)
	got := Value[nightResult](t, res)
	if got.State != "incomplete" {
		t.Fatalf("the batch under a spent budget: %+v", got)
	}
	var held, warned bool
	for _, ev := range l.Events() {
		if ev.Kind == engine.KindCall && ev.Op == "agent" && ev.Event == engine.EvHeld && str(ev.Data, "reason") == engine.ReasonBudgetExhausted {
			held = true
		}
		warned = warned || (ev.Kind == engine.KindFlow && ev.Event == "budget_warning")
	}
	if !held || !warned {
		t.Fatalf("held calls %v, budget warning %v", held, warned)
	}
	// nothing was sent on credit: a call prepared after the budget was spent is held and never asks the host for a thread or a turn
	for _, c := range l.CallsOf("executor") {
		if c.Node != "B" {
			continue
		}
		for _, ev := range l.Events() {
			if ev.Kind != engine.KindCall || ev.CallID != c.ID {
				continue
			}
			if ev.Event == engine.EvThreadRequested || ev.Event == engine.EvTurnRequested {
				t.Errorf("the executor of B asked the host for a %s on a spent budget", ev.Event)
			}
		}
		if l.Terminal(c.Label) != engine.EvHeld {
			t.Errorf("the executor of B ended %s, want held", l.Terminal(c.Label))
		}
	}
	if len(l.Relay.Landed) > 1 {
		t.Errorf("landed %v on a spent budget", l.Relay.Landed)
	}
	d := l.digestOf(t, []string{"CRW-A", "CRW-B"}, 25, false)
	if !strings.HasPrefix(d.Stopped, "token budget reached") || !strings.Contains(d.Stopped, "of 25 tokens") {
		t.Errorf("the run stopped because: %q", d.Stopped)
	}
	carried := 0
	for _, it := range d.Items {
		if it.Outcome == cli.OutcomeCarried {
			carried++
			if it.Reason == "" {
				t.Errorf("an item carried with no reason: %+v", it)
			}
		}
	}
	if carried == 0 {
		t.Errorf("nothing was carried to the next night: %+v", d.Items)
	}
	report, _ := reportOf(t, d)
	if !strings.Contains(report, "종료 사유 token budget reached") || !strings.Contains(report, "## 취소·이월") {
		t.Errorf("the report:\n%s", report)
	}
}

// TestNightBatchReportFromTheRun: the digest of a finished night batch, built as `crw flow digest` builds it, is read by the manager's report
// as it stands: the issues the journal shows integrated are under "통합됨" with the journal's evidence (commit, acceptance,
// integration), the ones it does not are not claimed, a deadline and a stop reason are on the conclusion, and a digest with an
// issue that never ran says so in the unverified part.
func TestNightBatchReportFromTheRun(t *testing.T) {
	l := NewLab(t, twoNodes()...)
	l.NightBatch(nightArgs("CRW-A", "CRW-B"), false)
	d := l.digestOf(t, []string{"CRW-A", "CRW-B"}, 100000, false)
	report, md := reportOf(t, d)
	if md.Schema != "crw-batch-digest/1" || len(md.Items) != 2 || md.Items[0].Outcome != "integrated" || md.Items[1].Outcome != "integrated" {
		t.Fatalf("the digest: %+v", md)
	}
	for _, want := range []string{
		"# 밤 배치 아침 보고: night-2026-10-12", "**결론**: 선택 2건 중 통합 2건, 취소 0건, 이월 0건, Jun 결정 필요 0건, 미확인 0건.", "기한 2026-10-13T06:00:00Z", "종료 사유 every selected issue",
		"## 통합됨", "- CRW-A issue A: 증거 run run-f08 node A: commit ", "acceptance ", "- CRW-B issue B: 증거 run run-f08 node B: commit ",
		"시작: 2026-10-12T09:00:00Z", "종료: 2026-10-13T05:50:00Z", "제공자 관측: token budget: 관측됨 (", "제공자 관측: model providers: 관측 안 됨",
	} {
		if !strings.Contains(report, want) {
			t.Errorf("the report lacks %q:\n%s", want, report)
		}
	}
	if !strings.Contains(report, "## 취소·이월\n\n- 없음") || !strings.Contains(report, "## Jun 결정이 필요한 것\n\n- 없음") {
		t.Errorf("the report claims more than the journal shows:\n%s", report)
	}
	// an issue the run never reached is carried with its reason, not integrated
	never := l.digestOf(t, []string{"CRW-A", "CRW-B", "CRW-C"}, 100000, false)
	rep2, _ := reportOf(t, never)
	if !strings.Contains(rep2, "통합 2건, 취소 0건, 이월 1건") || !strings.Contains(rep2, "CRW-C") || !strings.Contains(rep2, "the run did not reach the plan that holds the issue") {
		t.Errorf("an issue that never ran:\n%s", rep2)
	}
	// a cancelled run integrates nothing it did not land
	if cancelled := cli.BuildDigest(cli.DigestInput{Run: cli.RunRecord{RunID: "run-x"}, Events: []engine.Event{
		{Kind: engine.KindRun, Event: "started", RunID: "run-x"}, {Kind: engine.KindRun, Event: "finished", RunID: "run-x", Data: map[string]any{"status": engine.RunScriptError, "error_kind": engine.ErrKindCancelled}},
	}, Label: "x", Issues: []string{"CRW-9"}}); cancelled.Stopped != "cancelled" || cancelled.Items[0].Outcome != cli.OutcomeCancelled {
		t.Errorf("a cancelled run: %+v", cancelled)
	}
}

// TestNightBatchResumeAfterKill: the run process is killed after A landed. The journal shows A integrated and B not started (the
// digest of the dead run says so, and does not claim B); the process that takes the run again from its journal builds nothing for A,
// runs B and closes the milestone, and the digest then has both.
func TestNightBatchResumeAfterKill(t *testing.T) {
	l := NewLab(t, twoNodes()...)
	killed := false
	l.OnRow(func(ev engine.Event) {
		if !killed && ev.Op == ops.OpIntegrate && ev.Event == engine.EvCompleted && str(ev.Data, "node") == "A" {
			killed = true
			l.Kill()
		}
	})
	l.NightBatch(nightArgs("CRW-A", "CRW-B"), false)
	if !killed {
		t.Fatal("A did not land")
	}
	before := l.digestOf(t, []string{"CRW-A", "CRW-B"}, 100000, false)
	if before.Items[0].Outcome != cli.OutcomeIntegrated || before.Items[1].Outcome == cli.OutcomeIntegrated || !strings.Contains(before.Stopped, "ended without finishing") {
		t.Fatalf("the dead run: %+v", before)
	}
	if len(before.Items[1].Reason) == 0 {
		t.Errorf("B is carried with no reason: %+v", before.Items[1])
	}

	seq := l.LastSeq()
	l.NewProcess("proc-2")
	l.OnRow(nil)
	res := l.NightBatch(nightArgs("CRW-A", "CRW-B"), true)
	got := Value[nightResult](t, res)
	if got.State != "closed" {
		t.Fatalf("the resumed batch: %+v", got)
	}
	for _, c := range l.LiveCalls(seq) {
		if c.Node == "A" {
			t.Errorf("a call of A ran again: %+v", c)
		}
	}
	if turns := turnsSince(l, seq); len(turns) == 0 {
		t.Error("the resume ran nothing for B")
	}
	after := l.digestOf(t, []string{"CRW-A", "CRW-B"}, 100000, false)
	if after.Items[0].Outcome != cli.OutcomeIntegrated || after.Items[1].Outcome != cli.OutcomeIntegrated || !strings.HasPrefix(after.Stopped, "every selected issue") {
		t.Errorf("the digest after the resume: %+v", after)
	}
	if got := strings.Join(l.Relay.Landed, ","); got != "A,B" {
		t.Errorf("landed %s", got)
	}
}

// TestNightBatchMismatchIsNotCompleted: a batch whose milestone is not the selection ends with the plain facts of the journal: the
// run is held (its question waits for the manager), not completed, so it stays in `crw flow ps` and the status line, and its digest
// says nothing was released and why, never that every selected issue was worked on (CRW-1205 d6).
func TestNightBatchMismatchIsNotCompleted(t *testing.T) {
	l := NewLab(t, twoNodes()...)
	issues := []string{"CRW-A", "CRW-Z"}
	l.NightBatch(nightArgs(issues...), false)
	events, _, err := journal.Load(context.Background(), l.Store, l.RunID)
	if err != nil {
		t.Fatal(err)
	}
	info := cli.Summarize(cli.RunRecord{RunID: l.RunID, Workflow: cli.NightBatchWorkflow, Owner: "parent"}, events, false, 0)
	if info.State != cli.StateHeld || info.Waiting != 1 {
		t.Errorf("the run after a selection mismatch: state %s, %d question(s) waiting", info.State, info.Waiting)
	}
	d := l.digestOf(t, issues, 100000, false)
	if strings.Contains(d.Stopped, "every selected issue was worked on") || !strings.Contains(d.Stopped, "night_batch_selection") || !strings.Contains(d.Stopped, "CRW-Z") {
		t.Errorf("the run stopped because: %q", d.Stopped)
	}
	for _, it := range d.Items {
		if it.Outcome != cli.OutcomeDecision || !strings.Contains(it.Reason, "night_batch_selection") {
			t.Errorf("an item of a mismatched batch: %+v", it)
		}
	}
	report, _ := reportOf(t, d)
	if !strings.Contains(report, "Jun 결정 필요 2건") {
		t.Errorf("the report:\n%s", report)
	}
}
