package shadow_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/shadow"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/trace"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/trace/tracetest"
)

// recordAndCompare records src as one run on a fake backend, exports the store copy and replays the same script over it.
func recordAndCompare(t *testing.T, b *tracetest.Builder, fake *engine.Fake, src string) (*shadow.Report, *trace.Trace) {
	t.Helper()
	b.Run("run-f", "plan-f", src, fake, nil)
	tr, err := trace.Export(tracetest.Context(t), trace.Options{Path: b.Copy()})
	if err != nil {
		t.Fatal(err)
	}
	rep, err := shadow.Compare(tracetest.Context(t), tr, shadow.Config{Script: []byte(src), Name: "f.js", Policy: engine.FakePolicy()})
	if err != nil {
		t.Fatal(err)
	}
	return rep, tr
}

func wantAllSame(t *testing.T, rep *shadow.Report, same int) {
	t.Helper()
	run := rep.Runs[0]
	if got := counts(rep); !mapsEqual(got, map[string]int{"same": same}) || run.Replay.Invented != 0 || run.Replay.Status != engine.RunCompleted {
		t.Errorf("counts %v, replay %+v\n%s", got, run.Replay, dump(rep))
	}
}

// TestShadowReplaysTheRecordedAnswerVerbatim (CRW-1208 after the evaluation, d2): a call without a schema answers the script
// with the text the recorded call answered, not with that text encoded as JSON a second time, so a workflow that branches on the
// answer takes the branch it took when it was recorded.
func TestShadowReplaysTheRecordedAnswerVerbatim(t *testing.T) {
	fake := engine.NewFake()
	fake.Handler = func(r engine.FakeRequest) engine.FakeReply {
		if r.Kind == "turn" && r.Prompt != "" && contains(r.Prompt, "judge") {
			return engine.FakeReply{Text: "go"}
		}
		return engine.FakeReply{}
	}
	src := `export const meta = { name: 'f' }
const r = await agent.call('judge', { agentType: 'executor', node: 'n1', label: 'judge' })
if (r.output === 'go') {
  await agent.call('review', { agentType: 'reviewer', node: 'n1', label: 'review' })
}
return r.output
`
	rep, _ := recordAndCompare(t, tracetest.New(t), fake, src)
	wantAllSame(t, rep, 2)
}

// TestShadowReplaysARepairedCallAsRepaired (d3): a call that was answered wrong first and right on the repair turn replays the
// same way, so the script sees meta.repairUsed and the two turns the recorded call used.
func TestShadowReplaysARepairedCallAsRepaired(t *testing.T) {
	fake := engine.NewFake()
	fake.Handler = func(r engine.FakeRequest) engine.FakeReply {
		if r.Kind != "turn" {
			return engine.FakeReply{}
		}
		if r.Repair {
			return engine.FakeReply{Text: `{"ok":true}`}
		}
		return engine.FakeReply{Text: "this is not json"}
	}
	src := `export const meta = { name: 'f' }
const r = await agent.call('check', { agentType: 'executor', node: 'n1', label: 'check', schema: { type: 'object', properties: { ok: { type: 'boolean' } }, required: ['ok'], additionalProperties: false } })
if (r.meta.repairUsed) {
  await agent.call('recheck', { agentType: 'reviewer', node: 'n1', label: 'recheck' })
}
return r.meta.turnIds.length
`
	rep, tr := recordAndCompare(t, tracetest.New(t), fake, src)
	var check trace.Call
	for _, c := range tr.Runs[0].Calls {
		if c.Label == "check" {
			check = c
		}
	}
	if !check.RepairUsed || check.Turns != 2 || check.Status != engine.StatusCompleted {
		t.Fatalf("recorded call = %+v", check)
	}
	wantAllSame(t, rep, 2)
	if rep.Runs[0].Replay.Replayed != 2 {
		t.Errorf("replayed = %d, want both recorded calls", rep.Runs[0].Replay.Replayed)
	}
}

// TestShadowReplaysTheRecordedClock (d4): crw.clock answers with the times the recorded run read, so a decision that depends on
// the time is computed from the recorded run and repeats.
func TestShadowReplaysTheRecordedClock(t *testing.T) {
	b := tracetest.New(t)
	b.SetClock(time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC))
	src := `export const meta = { name: 'f' }
const first = Date.parse(await crw.clock())
if (first < Date.parse('2020-06-01T00:00:00Z')) {
  await agent.call('early', { agentType: 'reviewer', node: 'n1', label: 'early' })
}
const second = await crw.clock()
if (Date.parse(second) < Date.parse('2020-06-01T00:00:00Z')) {
  await agent.call('late', { agentType: 'explorer', node: 'n2', label: 'late' })
}
return second
`
	rep, _ := recordAndCompare(t, b, engine.NewFake(), src)
	wantAllSame(t, rep, 2)
	again, err := shadow.Compare(tracetest.Context(t), mustTrace(t, b), shadow.Config{Script: []byte(src), Name: "f.js", Policy: engine.FakePolicy()})
	if err != nil {
		t.Fatal(err)
	}
	a, _ := json.Marshal(rep)
	c, _ := json.Marshal(again)
	if string(a) != string(c) {
		t.Errorf("two comparisons of one trace differ")
	}
}

func mustTrace(t *testing.T, b *tracetest.Builder) *trace.Trace {
	t.Helper()
	tr, err := trace.Export(tracetest.Context(t), trace.Options{Path: b.Copy()})
	if err != nil {
		t.Fatal(err)
	}
	return tr
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// twoRuns records two independent runs, each with a completed reviewer and an agent acceptance on it, one integration batch each,
// and a delivery.
func twoRuns(t *testing.T) *tracetest.Builder {
	t.Helper()
	b := tracetest.New(t)
	src := `export const meta = { name: 'f' }
await agent.call('build', { agentType: 'executor', node: 'n1', label: 'build' })
await agent.call('review', { agentType: 'reviewer', node: 'n1', label: 'review' })
return 1
`
	for i, id := range []string{"a", "b"} {
		run, plan := "run-"+id, "plan-"+id
		res := b.Run(run, plan, src, nil, nil)
		var reviewer string
		for _, c := range res.Calls {
			if c.Label == "review" {
				reviewer = c.CallID
			}
		}
		x := "x-" + id
		b.Execution(x, plan, "n1", "initial", "executor", "t0", run, tracetest.At(2+i))
		b.Acceptance("acc-"+id, plan, "n1", x, reviewer, tracetest.At(100+i))
		batch := "batch-" + id
		b.IntegrationStage(batch, plan, "intent", "", "", "head", tracetest.At(110+i))
		b.AgentIntegrationStage(batch, plan, "intent", "n1", "acc-"+id, x, tracetest.At(110+i))
		b.IntegrationStage(batch, plan, "ref_moved", "", "", "head", tracetest.At(115+i))
		b.IntegrationBatch(batch, plan, "refs/heads/integration", tracetest.At(115+i))
		b.AgentIntegrationStage(batch, plan, "marked", "n1", "acc-"+id, x, tracetest.At(116+i))
	}
	return b
}

// TestRunSelectionKeepsOnlyTheRunsAcceptancesAndBatches (d5): measuring one run of a store that holds two audits that run against
// its own reviewers and integrates its own batches; the other run's acceptance is not an unobserved reviewer, and exporting with
// --run gives the same selection as selecting from the whole export.
func TestRunSelectionKeepsOnlyTheRunsAcceptancesAndBatches(t *testing.T) {
	b := twoRuns(t)
	path := b.Copy()
	full, err := trace.Export(tracetest.Context(t), trace.Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	if m := shadow.BuildMeasure(full, shadow.MeasureOptions{}); m.FlowAuditResult.Acceptances != 2 || !m.FlowAuditResult.OK {
		t.Fatalf("the audit of both runs = %+v", m.FlowAuditResult)
	}
	selected, err := full.Select("run-a")
	if err != nil {
		t.Fatal(err)
	}
	exported, err := trace.Export(tracetest.Context(t), trace.Options{Path: path, Run: "run-a"})
	if err != nil {
		t.Fatal(err)
	}
	for name, tr := range map[string]*trace.Trace{"Select": selected, "Export --run": exported} {
		m := shadow.BuildMeasure(tr, shadow.MeasureOptions{})
		if a := m.FlowAuditResult; a.Acceptances != 1 || a.Observed != 1 || a.Unobserved != 0 || !a.OK {
			t.Errorf("%s: audit = %+v, want the one acceptance of run-a observed", name, a)
		}
		if len(tr.Acceptances) != 1 || tr.Acceptances[0].AcceptanceID != "acc-a" {
			t.Errorf("%s: acceptances = %+v", name, tr.Acceptances)
		}
		if len(tr.Integration) != 1 || tr.Integration[0].BatchID != "batch-a" || len(tr.Integration[0].Nodes) != 1 {
			t.Errorf("%s: integration = %+v", name, tr.Integration)
		}
	}
	a, _ := json.Marshal(selected)
	c, _ := json.Marshal(exported)
	if string(a) != string(c) {
		t.Errorf("Select and Export --run disagree:\n%s\n%s", a, c)
	}
	t.Run("runs of one plan", runSelectionOfRunsOfOnePlan)
}

// runSelectionOfRunsOfOnePlan (a sub-test of TestRunSelectionKeepsOnlyTheRunsAcceptancesAndBatches): two runs of one plan, the acceptances on different nodes standing on different runs'
// reviewers. run-a calls executor n1, reviewer n1 and executor n2; run-b calls reviewer n2. acc-a (n1) stands on run-a's
// reviewer and acc-b (n2) stands on run-b's reviewer, though run-a touches n2 too. Selecting run-a, from the whole export or by
// export --run, keeps acc-a only: acc-b stands on a call of another run, so it is not an unobserved reviewer of run-a.
func runSelectionOfRunsOfOnePlan(t *testing.T) {
	b := tracetest.New(t)
	reviewerOf := func(res *engine.Result) string {
		for _, c := range res.Calls {
			if c.Label == "review" {
				return c.CallID
			}
		}
		t.Fatal("the run has no review call")
		return ""
	}
	const plan = "plan-x"
	resA := b.Run("run-a", plan, `export const meta = { name: 'f' }
await agent.call('build', { agentType: 'executor', node: 'n1', label: 'build' })
await agent.call('review', { agentType: 'reviewer', node: 'n1', label: 'review' })
await agent.call('build2', { agentType: 'executor', node: 'n2', label: 'build2' })
return 1
`, nil, nil)
	resB := b.Run("run-b", plan, `export const meta = { name: 'f' }
await agent.call('review', { agentType: 'reviewer', node: 'n2', label: 'review' })
return 1
`, nil, nil)
	b.Execution("x-a", plan, "n1", "initial", "executor", "t0", "run-a", tracetest.At(2))
	b.Execution("x-b", plan, "n2", "initial", "executor", "t0", "run-b", tracetest.At(3))
	b.Acceptance("acc-a", plan, "n1", "x-a", reviewerOf(resA), tracetest.At(100))
	b.Acceptance("acc-b", plan, "n2", "x-b", reviewerOf(resB), tracetest.At(101))
	path := b.Copy()
	full, err := trace.Export(tracetest.Context(t), trace.Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	selected, err := full.Select("run-a")
	if err != nil {
		t.Fatal(err)
	}
	exported, err := trace.Export(tracetest.Context(t), trace.Options{Path: path, Run: "run-a"})
	if err != nil {
		t.Fatal(err)
	}
	for name, tr := range map[string]*trace.Trace{"Select": selected, "Export --run": exported} {
		m := shadow.BuildMeasure(tr, shadow.MeasureOptions{})
		if a := m.FlowAuditResult; a.Acceptances != 1 || a.Observed != 1 || a.Unobserved != 0 || !a.OK {
			t.Errorf("%s: audit = %+v, want the one acceptance of run-a observed", name, a)
		}
		if len(tr.Acceptances) != 1 || tr.Acceptances[0].AcceptanceID != "acc-a" {
			t.Errorf("%s: acceptances = %+v", name, tr.Acceptances)
		}
	}
	a, _ := json.Marshal(selected)
	c, _ := json.Marshal(exported)
	if string(a) != string(c) {
		t.Errorf("Select and Export --run disagree:\n%s\n%s", a, c)
	}
	if _, err := trace.Export(tracetest.Context(t), trace.Options{Path: path, Run: "run-none"}); err == nil {
		t.Errorf("export --run of a run the store does not hold succeeded")
	}
}
