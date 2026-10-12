package manage

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The pump's signal sources are driven through injected readings, the injected clock and temporary
// state; no test opens a relay store of the host or reaches the network.

// pumpSignalTestCapacityState reads the capacity state file of a fixture.
func pumpSignalTestCapacityState(t *testing.T, f *capacityFixture) capacityState {
	t.Helper()
	return capacityReadState(filepath.Join(f.stateDir, capacityStateFile))
}

// The evaluation is pure: the alert it reports and the alerted state it would write are returned,
// and the state file is left as it was until Commit.
func TestPumpSignalCapacityEvaluateWritesNothingUntilCommit(t *testing.T) {
	f := capacityTestReady(t, capacityTestPlans)
	statePath := filepath.Join(f.stateDir, capacityStateFile)
	before, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	ev, err := capacityEvaluate(context.Background(), f.env, f.cfg)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if len(ev.Report.Plans) != 1 || !ev.Report.Plans[0].Alert || ev.Report.Plans[0].AlertEpoch != 1 {
		t.Fatalf("plans = %+v, want one alerting plan of epoch 1", ev.Report.Plans)
	}
	after, err := os.ReadFile(statePath)
	if err != nil || string(after) != string(before) {
		t.Fatalf("the evaluation changed the state file: %v\nbefore %s\nafter  %s", err, before, after)
	}
	// Two evaluations of the same uncommitted state judge the same alert under the same epoch.
	again, err := capacityEvaluate(context.Background(), f.env, f.cfg)
	if err != nil || !again.Report.Plans[0].Alert || again.Report.Plans[0].AlertEpoch != 1 {
		t.Fatalf("second evaluation = %+v, %v; want the same alert of epoch 1", again.Report.Plans, err)
	}
	if err := ev.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	stored := pumpSignalTestCapacityState(t, f).Plans["p-crw-129"]
	if len(stored.Alerted) != 2 || stored.AlertedAt == 0 || stored.AlertEpoch != 1 {
		t.Fatalf("stored = %+v, want the alerted set and epoch 1", stored)
	}
	third, err := capacityEvaluate(context.Background(), f.env, f.cfg)
	if err != nil || third.Report.Plans[0].Alert {
		t.Fatalf("after the commit the same set alerts again: %+v, %v", third.Report.Plans, err)
	}
}

// A realert_hours re-alert of the same waiting set is a new epoch.
func TestPumpSignalCapacityRealertIsANewEpoch(t *testing.T) {
	f := capacityTestReady(t, capacityTestPlans)
	ev, err := capacityEvaluate(context.Background(), f.env, f.cfg)
	if err != nil || ev.Commit() != nil {
		t.Fatalf("first alert: %v", err)
	}
	later := capacityTestNow.Add(4 * time.Hour)
	f.env.Now = func() time.Time { return later }
	// The set has waited the whole time: keep the persistence clock as the first run left it.
	again, err := capacityEvaluate(context.Background(), f.env, f.cfg)
	if err != nil {
		t.Fatal(err)
	}
	plan := again.Report.Plans[0]
	if !plan.Alert || plan.AlertEpoch != 2 {
		t.Fatalf("plan = alert %v epoch %d, want a re-alert of epoch 2", plan.Alert, plan.AlertEpoch)
	}
}

// Capacity itself keeps its contract: it writes the state unless dry.
func TestPumpSignalCapacityStillWritesUnlessDry(t *testing.T) {
	f := capacityTestReady(t, capacityTestPlans)
	capacityTestRun(t, f, true)
	if got := pumpSignalTestCapacityState(t, f).Plans["p-crw-129"]; len(got.Alerted) != 0 {
		t.Fatalf("a dry run wrote the alerted state: %+v", got)
	}
	capacityTestRun(t, f, false)
	if got := pumpSignalTestCapacityState(t, f).Plans["p-crw-129"]; len(got.Alerted) != 2 || got.AlertEpoch != 1 {
		t.Fatalf("a real run left %+v, want the alerted set at epoch 1", got)
	}
}

// pumpSignalFixture is a pump over a temporary state directory, an injected clock and no management
// thread, so a round collects and saves its pending events and delivers nothing.
type pumpSignalFixture struct {
	t   *testing.T
	now time.Time
	e   *Env
	cfg *Config
}

func pumpSignalTestFixture(t *testing.T, sources ...pumpSource) *pumpSignalFixture {
	t.Helper()
	f := &pumpSignalFixture{t: t, now: pumpTestNow}
	f.e = pumpTestEnv(t, &f.now)
	f.cfg = pumpTestConfig(t, "")
	f.cfg.ManagementThread = ""
	if len(sources) == 0 {
		sources = []pumpSource{pumpSignalDagSource{}, pumpSignalCapacitySource{}, pumpSignalAuditSource{}}
	}
	pumpTestSwapSources(t, sources...)
	return f
}

// settings sets the pump section of the configuration.
func (f *pumpSignalFixture) settings(section map[string]any) {
	f.t.Helper()
	raw, err := json.Marshal(section)
	if err != nil {
		f.t.Fatal(err)
	}
	f.cfg.raw = map[string]json.RawMessage{"pump": raw}
}

func (f *pumpSignalFixture) advance(d time.Duration) { f.now = f.now.Add(d) }

// round runs one pump round and returns the saved state.
func (f *pumpSignalFixture) round() pumpState {
	f.t.Helper()
	if _, err := pumpRound(context.Background(), f.e, f.cfg, pumpSettingsFrom(f.cfg), false); err != nil {
		f.t.Fatalf("round: %v", err)
	}
	return pumpTestReadState(f.t, f.cfg)
}

// deliverAll marks the pending events delivered, as an accepted batch does.
func (f *pumpSignalFixture) deliverAll() {
	f.t.Helper()
	st := pumpTestReadState(f.t, f.cfg)
	for _, ev := range st.Pending {
		st.Sent[ev.ID] = true
	}
	st.Pending, st.FirstAt = nil, nil
	if err := st.pumpSave(f.cfg); err != nil {
		f.t.Fatal(err)
	}
}

func pumpSignalIDs(events []pumpEvent) []string {
	ids := []string{}
	for _, ev := range events {
		ids = append(ids, ev.ID)
	}
	return ids
}

func pumpSignalWant(t *testing.T, what string, got []pumpEvent, ids ...string) {
	t.Helper()
	if want := strings.Join(ids, ","); strings.Join(pumpSignalIDs(got), ",") != want {
		t.Fatalf("%s: event ids = %v, want [%s]", what, pumpSignalIDs(got), want)
	}
}

// pumpSignalDagFake stands in for the DAG review: no relay store is opened.
type pumpSignalDagFake struct {
	review Review
	err    error
	calls  int
	// pumpStore records, for each read, whether the review was given the pump's own offset store.
	pumpStore []bool
}

func pumpSignalTestDag(t *testing.T) *pumpSignalDagFake {
	t.Helper()
	fake := &pumpSignalDagFake{}
	saved := pumpSignalDagReview
	pumpSignalDagReview = func(ctx context.Context, _ *Env, _ *Config) (Review, error) {
		fake.calls++
		_, own := dagHostOffsetStoreOf(ctx).(pumpSignalDagOffsetStore)
		fake.pumpStore = append(fake.pumpStore, own)
		return fake.review, fake.err
	}
	t.Cleanup(func() { pumpSignalDagReview = saved })
	return fake
}

func pumpSignalAnomaly(kind, plan, node, issue, detail string) DagReviewAnomaly {
	return DagReviewAnomaly{Kind: kind, Plan: plan, Node: node, Issue: issue, Detail: detail}
}

func pumpSignalReview(checks []Check, anomalies ...DagReviewAnomaly) Review {
	return Review{Plans: []DagReviewPlan{}, Anomalies: anomalies, Checks: checks}
}

var pumpSignalUnmeasuredCheck = []Check{{Name: "dag_node_region_grades", State: dagReviewUnmeasured, Detail: "the store predates the table"}}

// The DAG source reads every dag_review_seconds (600 by default) and says nothing while the
// anomaly set is unchanged.
func TestPumpSignalDagIntervalAndSilence(t *testing.T) {
	f := pumpSignalTestFixture(t, pumpSignalDagSource{})
	dag := pumpSignalTestDag(t)
	dag.review = pumpSignalReview(nil, pumpSignalAnomaly("child_without_receipt", "p1", "n1", "CRW-1", "ended"))
	st := f.round()
	if dag.calls != 1 {
		t.Fatalf("calls = %d, want the first round to read", dag.calls)
	}
	pumpSignalWant(t, "first", st.Pending, "signal_dag#1")
	if st.Pending[0].Kind != pumpKindDag || !strings.Contains(st.Pending[0].Text, "CRW-1") {
		t.Fatalf("event = %+v, want an urgent dag event naming the issue", st.Pending[0])
	}
	f.advance(599 * time.Second)
	f.round()
	if dag.calls != 1 {
		t.Fatalf("calls = %d at 599s, want no read before the interval", dag.calls)
	}
	f.advance(time.Second)
	st = f.round()
	if dag.calls != 2 {
		t.Fatalf("calls = %d at 600s, want the second read", dag.calls)
	}
	pumpSignalWant(t, "same set", st.Pending, "signal_dag#1")
	if got := dag.pumpStore; len(got) != 2 || !got[0] || !got[1] {
		t.Fatalf("pumpStore = %v, want every read to keep its offsets in the pump state", got)
	}
}

// dag_review_seconds overrides the default.
func TestPumpSignalDagSettingOverridesInterval(t *testing.T) {
	f := pumpSignalTestFixture(t, pumpSignalDagSource{})
	f.settings(map[string]any{"dag_review_seconds": 60})
	dag := pumpSignalTestDag(t)
	f.round()
	f.advance(59 * time.Second)
	f.round()
	f.advance(time.Second)
	f.round()
	if dag.calls != 2 {
		t.Fatalf("calls = %d, want reads at 0 and 60 seconds", dag.calls)
	}
}

// C5: the key is the anomaly set, so the same anomaly with other words is not sent again.
func TestPumpSignalDagKeyIsTheSetNotTheText(t *testing.T) {
	f := pumpSignalTestFixture(t, pumpSignalDagSource{})
	dag := pumpSignalTestDag(t)
	dag.review = pumpSignalReview(nil, pumpSignalAnomaly("k", "p1", "n1", "CRW-1", "first wording"))
	f.round()
	f.advance(10 * time.Minute)
	dag.review = pumpSignalReview(nil, pumpSignalAnomaly("k", "p1", "n1", "CRW-1", "second wording"))
	st := f.round()
	if dag.calls != 2 {
		t.Fatalf("calls = %d, want two reads", dag.calls)
	}
	pumpSignalWant(t, "reworded", st.Pending, "signal_dag#1")
}

// A -> clear -> A is reported again, the clear is an info event, and the counter survives a restart
// (every round reloads the state from disk) with the earlier ids already delivered.
func TestPumpSignalDagAppearClearAppearReportsAgain(t *testing.T) {
	f := pumpSignalTestFixture(t, pumpSignalDagSource{})
	dag := pumpSignalTestDag(t)
	a := pumpSignalAnomaly("k", "p1", "n1", "CRW-1", "d")
	dag.review = pumpSignalReview(nil, a)
	f.round()
	f.deliverAll()
	f.advance(10 * time.Minute)
	dag.review = pumpSignalReview(nil)
	st := f.round()
	pumpSignalWant(t, "clear", st.Pending, "signal_dag#2")
	if st.Pending[0].Kind != pumpKindInfo {
		t.Fatalf("clear kind = %q, want info", st.Pending[0].Kind)
	}
	f.deliverAll()
	f.advance(10 * time.Minute)
	dag.review = pumpSignalReview(nil, a)
	st = f.round()
	pumpSignalWant(t, "reappear", st.Pending, "signal_dag#3")
	if st.Pending[0].Kind != pumpKindDag {
		t.Fatalf("reappear kind = %q, want dag", st.Pending[0].Kind)
	}
	if st.Cursors[pumpSignalDagChangesKey] != "3" {
		t.Fatalf("change counter = %q, want 3", st.Cursors[pumpSignalDagChangesKey])
	}
}

// The first measured read with no anomaly says nothing: there was nothing to clear.
func TestPumpSignalDagFirstCleanReadIsSilent(t *testing.T) {
	f := pumpSignalTestFixture(t, pumpSignalDagSource{})
	pumpSignalTestDag(t)
	if st := f.round(); len(st.Pending) != 0 {
		t.Fatalf("pending = %+v, want none", st.Pending)
	}
}

// C4: an error round and a round with an unmeasured check never clear a prior alert, and the next
// measured read is compared with the last measured set.
func TestPumpSignalDagUnmeasuredNeverClears(t *testing.T) {
	f := pumpSignalTestFixture(t, pumpSignalDagSource{})
	dag := pumpSignalTestDag(t)
	a := pumpSignalAnomaly("k", "p1", "n1", "CRW-1", "d")
	dag.review = pumpSignalReview(nil, a)
	f.round()
	f.deliverAll()

	// An error round: unmeasured, no event, no clear, and the read is retried on the next round.
	f.advance(10 * time.Minute)
	dag.review, dag.err = pumpSignalReview(nil), errors.New("store unreadable")
	st := f.round()
	if len(st.Pending) != 0 || st.Sources["signal_dag"] != pumpSourceUnmeasured {
		t.Fatalf("error round: pending %+v, source %q; want none and unmeasured", st.Pending, st.Sources["signal_dag"])
	}
	f.advance(30 * time.Second)
	dag.err = nil
	dag.review = pumpSignalReview(pumpSignalUnmeasuredCheck)
	callsBefore := dag.calls
	st = f.round()
	if dag.calls != callsBefore+1 {
		t.Fatalf("a failed read was not retried on the next round")
	}
	if len(st.Pending) != 0 {
		t.Fatalf("a review with an unmeasured check cleared: %+v", st.Pending)
	}

	// The same anomaly still present after the error round is not a new event.
	f.advance(10 * time.Minute)
	dag.review = pumpSignalReview(nil, a)
	if st = f.round(); len(st.Pending) != 0 {
		t.Fatalf("the unchanged set after the error round was sent again: %+v", st.Pending)
	}

	// A measured clear after all of that is the one clear event.
	f.advance(10 * time.Minute)
	dag.review = pumpSignalReview(nil)
	st = f.round()
	pumpSignalWant(t, "measured clear", st.Pending, "signal_dag#2")
}

// A review with an unmeasured check can add an anomaly but never drops one it cannot measure.
func TestPumpSignalDagPartialReviewAddsButNeverDrops(t *testing.T) {
	f := pumpSignalTestFixture(t, pumpSignalDagSource{})
	dag := pumpSignalTestDag(t)
	a := pumpSignalAnomaly("k", "p1", "n1", "CRW-1", "d")
	b := pumpSignalAnomaly("k", "p1", "n2", "CRW-2", "d")
	dag.review = pumpSignalReview(nil, a)
	f.round()
	f.deliverAll()
	f.advance(10 * time.Minute)
	dag.review = pumpSignalReview(pumpSignalUnmeasuredCheck, b)
	st := f.round()
	pumpSignalWant(t, "partial add", st.Pending, "signal_dag#2")
	if !strings.Contains(st.Pending[0].Text, "CRW-1") || !strings.Contains(st.Pending[0].Text, "CRW-2") {
		t.Fatalf("text = %q, want both anomalies kept", st.Pending[0].Text)
	}
	f.deliverAll()
	f.advance(10 * time.Minute)
	dag.review = pumpSignalReview(nil, b)
	st = f.round()
	pumpSignalWant(t, "measured shrink", st.Pending, "signal_dag#3")
}

// pumpSignalTestQuiet replaces the DAG review and the capacity judgement with readings that find
// nothing, so a test that runs the registered sources never opens a relay store or starts a helper.
// A test of the signal sources installs its own fake over it.
func pumpSignalTestQuiet(t *testing.T) {
	t.Helper()
	savedDag, savedCapacity, savedFlow := pumpSignalDagReview, pumpSignalCapacity, pumpFlowRead
	pumpFlowRead = func(context.Context, *Env, *Config) (pumpFlowData, error) { return pumpFlowData{}, nil }
	pumpSignalDagReview = func(context.Context, *Env, *Config) (Review, error) {
		return Review{Plans: []DagReviewPlan{}, Anomalies: []DagReviewAnomaly{}, Checks: []Check{}}, nil
	}
	pumpSignalCapacity = func(context.Context, *Env, *Config, bool) (CapacityReport, func() error, error) {
		return CapacityReport{Plans: []CapacityPlan{}}, nil, nil
	}
	t.Cleanup(func() { pumpSignalDagReview, pumpSignalCapacity, pumpFlowRead = savedDag, savedCapacity, savedFlow })
}

// pumpSignalCapFake stands in for the capacity judgement: it records the order of evaluate and
// commit, and what the pump had made durable when the commit ran.
type pumpSignalCapFake struct {
	report    CapacityReport
	err       error
	commitErr error
	dryCalls  []bool
	commits   int
	onCommit  func()
}

func pumpSignalTestCapacity(t *testing.T) *pumpSignalCapFake {
	t.Helper()
	fake := &pumpSignalCapFake{}
	saved := pumpSignalCapacity
	pumpSignalCapacity = func(_ context.Context, _ *Env, _ *Config, dry bool) (CapacityReport, func() error, error) {
		fake.dryCalls = append(fake.dryCalls, dry)
		if fake.err != nil {
			return CapacityReport{}, nil, fake.err
		}
		if dry {
			return fake.report, nil, nil
		}
		return fake.report, func() error {
			fake.commits++
			if fake.onCommit != nil {
				fake.onCommit()
			}
			return fake.commitErr
		}, nil
	}
	t.Cleanup(func() { pumpSignalCapacity = saved })
	return fake
}

func pumpSignalPlan(plan string, epoch int, waiting []string, issues ...string) CapacityPlan {
	candidate := BranchCandidate{}
	for _, issue := range issues {
		candidate.Nodes = append(candidate.Nodes, BranchNode{NodeID: "n-" + issue, IssueKey: issue})
	}
	branches := BranchCandidates{candidate}
	return CapacityPlan{Plan: plan, Verdict: capacityExpand, Alert: true, AlertEpoch: epoch, Waiting: waiting, Branches: &branches}
}

// The capacity source reads every capacity_seconds (1800 by default), sends one low event per
// alerting plan with the candidate issue keys, and says nothing when no plan alerts.
func TestPumpSignalCapacityIntervalAndEventPerAlertPlan(t *testing.T) {
	f := pumpSignalTestFixture(t, pumpSignalCapacitySource{})
	capFake := pumpSignalTestCapacity(t)
	quiet := CapacityPlan{Plan: "p3", Verdict: capacityHold, Alert: false}
	capFake.report = CapacityReport{Plans: []CapacityPlan{
		pumpSignalPlan("p1", 1, []string{"CRW-1", "CRW-2"}, "CRW-1", "CRW-2"),
		quiet,
		pumpSignalPlan("p2", 4, []string{"CRW-9", "CRW-8"}, "CRW-9"),
	}}
	st := f.round()
	if len(st.Pending) != 2 {
		t.Fatalf("pending = %+v, want one event per alerting plan", st.Pending)
	}
	for _, ev := range st.Pending {
		if ev.Kind != pumpKindLow {
			t.Fatalf("event %+v is not low", ev)
		}
	}
	if !strings.Contains(st.Pending[0].Text, "CRW-1") || !strings.Contains(st.Pending[0].Text, "CRW-2") || !strings.Contains(st.Pending[1].Text, "CRW-9") {
		t.Fatalf("texts = %q / %q, want the candidate issue keys", st.Pending[0].Text, st.Pending[1].Text)
	}
	if st.Pending[0].ID == st.Pending[1].ID || !strings.Contains(st.Pending[0].ID, "p1") || !strings.Contains(st.Pending[1].ID, "p2") {
		t.Fatalf("ids = %v, want one id per plan", pumpSignalIDs(st.Pending))
	}
	f.advance(1799 * time.Second)
	f.round()
	if len(capFake.dryCalls) != 1 {
		t.Fatalf("calls = %d at 1799s, want no read before the interval", len(capFake.dryCalls))
	}
	f.advance(time.Second)
	capFake.report = CapacityReport{Plans: []CapacityPlan{quiet}}
	st = f.round()
	if len(capFake.dryCalls) != 2 || len(st.Pending) != 2 {
		t.Fatalf("calls %d, pending %d; want a second read that adds nothing", len(capFake.dryCalls), len(st.Pending))
	}
}

func TestPumpSignalCapacitySettingOverridesInterval(t *testing.T) {
	f := pumpSignalTestFixture(t, pumpSignalCapacitySource{})
	f.settings(map[string]any{"capacity_seconds": 120})
	capFake := pumpSignalTestCapacity(t)
	f.round()
	f.advance(119 * time.Second)
	f.round()
	f.advance(time.Second)
	f.round()
	if len(capFake.dryCalls) != 2 {
		t.Fatalf("calls = %d, want reads at 0 and 120 seconds", len(capFake.dryCalls))
	}
}

// The order is evaluate, durably append the pending events, then commit the alerted state: when the
// commit runs, the event is already in the saved pump state.
func TestPumpSignalCapacityCommitsAfterPendingIsDurable(t *testing.T) {
	f := pumpSignalTestFixture(t, pumpSignalCapacitySource{})
	capFake := pumpSignalTestCapacity(t)
	capFake.report = CapacityReport{Plans: []CapacityPlan{pumpSignalPlan("p1", 1, []string{"CRW-1"}, "CRW-1")}}
	var durable []string
	capFake.onCommit = func() { durable = pumpSignalIDs(pumpTestReadState(t, f.cfg).Pending) }
	f.round()
	if capFake.commits != 1 || len(durable) != 1 {
		t.Fatalf("commits %d, durable at commit %v; want one commit after the event was saved", capFake.commits, durable)
	}
}

// A pending save that fails never reaches the commit, so the alert cannot be lost.
func TestPumpSignalCapacityPendingSaveFailureSkipsCommit(t *testing.T) {
	f := pumpSignalTestFixture(t, pumpSignalCapacitySource{})
	capFake := pumpSignalTestCapacity(t)
	capFake.report = CapacityReport{Plans: []CapacityPlan{pumpSignalPlan("p1", 1, []string{"CRW-1"}, "CRW-1")}}
	target := filepath.Join(f.cfg.StateDir, "elsewhere.json")
	if err := os.Symlink(target, filepath.Join(f.cfg.StateDir, pumpStateFile)); err != nil {
		t.Fatal(err)
	}
	if _, err := pumpRound(context.Background(), f.e, f.cfg, pumpSettingsFrom(f.cfg), false); err == nil {
		t.Fatal("round succeeded with an unwritable state")
	}
	if capFake.commits != 0 {
		t.Fatalf("commits = %d, want none when the pending events were not saved", capFake.commits)
	}
}

// A commit that fails is logged and isolated: the round succeeds with the event pending, and the
// next read converges on the same id.
func TestPumpSignalCapacityCommitFailureConvergesOnTheSameEvent(t *testing.T) {
	f := pumpSignalTestFixture(t, pumpSignalCapacitySource{})
	capFake := pumpSignalTestCapacity(t)
	capFake.report = CapacityReport{Plans: []CapacityPlan{pumpSignalPlan("p1", 1, []string{"CRW-1"}, "CRW-1")}}
	capFake.commitErr = errors.New("disk full")
	st := f.round()
	if len(st.Pending) != 1 {
		t.Fatalf("pending = %+v, want the alert recorded despite the failed commit", st.Pending)
	}
	first := st.Pending[0].ID
	logged, err := os.ReadFile(filepath.Join(f.cfg.StateDir, pumpLogFile))
	if err != nil || !strings.Contains(string(logged), "disk full") {
		t.Fatalf("pump.log = %q, %v; want the commit failure logged", logged, err)
	}
	f.advance(30 * time.Minute)
	capFake.commitErr = nil
	st = f.round()
	pumpSignalWant(t, "converged", st.Pending, first)
	if capFake.commits != 2 {
		t.Fatalf("commits = %d, want the retry to commit", capFake.commits)
	}
}

// An error from the judgement is unmeasured: logged, no event, retried on the next round.
func TestPumpSignalCapacityErrorIsUnmeasuredAndRetried(t *testing.T) {
	f := pumpSignalTestFixture(t, pumpSignalCapacitySource{})
	capFake := pumpSignalTestCapacity(t)
	capFake.err = errors.New("relay unreadable")
	st := f.round()
	if len(st.Pending) != 0 || st.Sources["signal_capacity"] != pumpSourceUnmeasured {
		t.Fatalf("pending %+v, source %q", st.Pending, st.Sources["signal_capacity"])
	}
	f.advance(30 * time.Second)
	f.round()
	if len(capFake.dryCalls) != 2 {
		t.Fatalf("calls = %d, want the failed read retried on the next round", len(capFake.dryCalls))
	}
}

// pumpSignalRealCapacity runs the pump against the real judgement over the capacity test fixture
// (a temporary relay store), with an optional crash between the pending save and the commit.
func pumpSignalRealCapacity(t *testing.T) (*pumpSignalFixture, *capacityFixture, *bool) {
	t.Helper()
	cf := capacityTestReady(t, capacityTestPlans)
	f := &pumpSignalFixture{t: t, now: capacityTestNow, e: cf.env, cfg: cf.cfg}
	cf.cfg.ManagementThread = ""
	cf.env.Now = func() time.Time { return f.now }
	pumpTestSwapSources(t, pumpSignalCapacitySource{})
	crash := false
	saved := pumpSignalCapacity
	pumpSignalCapacity = func(ctx context.Context, e *Env, cfg *Config, dry bool) (CapacityReport, func() error, error) {
		report, commit, err := pumpSignalCapacityRead(ctx, e, cfg, dry)
		if crash && commit != nil {
			return report, func() error { return errors.New("process ended before the commit") }, err
		}
		return report, commit, err
	}
	t.Cleanup(func() { pumpSignalCapacity = saved })
	return f, cf, &crash
}

// A crash between the pending write and the commit converges: the next read judges the same alert
// under the same epoch, the event id is the same, and the commit then lands.
func TestPumpSignalCapacityCrashBetweenPendingAndCommit(t *testing.T) {
	f, cf, crash := pumpSignalRealCapacity(t)
	*crash = true
	st := f.round()
	if len(st.Pending) != 1 {
		t.Fatalf("pending = %+v, want the alert recorded", st.Pending)
	}
	first := st.Pending[0].ID
	if got := pumpSignalTestCapacityState(t, cf).Plans["p-crw-129"]; len(got.Alerted) != 0 {
		t.Fatalf("the alerted state was committed despite the crash: %+v", got)
	}
	*crash = false
	f.advance(31 * time.Minute)
	st = f.round()
	pumpSignalWant(t, "after the crash", st.Pending, first)
	if got := pumpSignalTestCapacityState(t, cf).Plans["p-crw-129"]; len(got.Alerted) != 2 || got.AlertEpoch != 1 {
		t.Fatalf("stored = %+v, want the alerted set at epoch 1", got)
	}
	// The event was delivered meanwhile: the converged id is already sent and is not added again.
	f.deliverAll()
	f.advance(31 * time.Minute)
	if st = f.round(); len(st.Pending) != 0 {
		t.Fatalf("a committed alert alerted again: %+v", st.Pending)
	}
}

// A crash after the event was delivered but before the commit still adds nothing the second time.
func TestPumpSignalCapacityDeliveredEventIsNotAddedAfterACrash(t *testing.T) {
	f, _, crash := pumpSignalRealCapacity(t)
	*crash = true
	f.round()
	f.deliverAll()
	*crash = false
	f.advance(31 * time.Minute)
	if st := f.round(); len(st.Pending) != 0 {
		t.Fatalf("pending = %+v, want the delivered id to dedupe", st.Pending)
	}
}

// A periodic re-alert of the same waiting set is a new epoch and so a new event.
func TestPumpSignalCapacityPeriodicRealertIsANewEvent(t *testing.T) {
	f, cf, _ := pumpSignalRealCapacity(t)
	st := f.round()
	first := st.Pending[0].ID
	f.deliverAll()
	f.advance(2 * time.Hour)
	if st = f.round(); len(st.Pending) != 0 {
		t.Fatalf("inside realert_hours: %+v", st.Pending)
	}
	f.advance(2 * time.Hour)
	st = f.round()
	if len(st.Pending) != 1 || st.Pending[0].ID == first {
		t.Fatalf("pending = %v after realert_hours, want a new id besides %s", pumpSignalIDs(st.Pending), first)
	}
	if got := pumpSignalTestCapacityState(t, cf).Plans["p-crw-129"]; got.AlertEpoch != 2 {
		t.Fatalf("epoch = %d, want 2", got.AlertEpoch)
	}
}

// A dry run reads with dry=true, saves nothing of the pump and commits nothing of the capacity.
func TestPumpSignalDryRunWritesNothing(t *testing.T) {
	f, cf, _ := pumpSignalRealCapacity(t)
	pumpTestSwapSources(t, pumpSignalCapacitySource{}, pumpSignalAuditSource{})
	pumpSignalTestWriteAlerts(t, f, "{\"issue\":\"CRW-1\"}\n")
	if _, err := pumpRound(context.Background(), f.e, f.cfg, pumpSettingsFrom(f.cfg), true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(f.cfg.StateDir, pumpStateFile)); !os.IsNotExist(err) {
		t.Fatalf("a dry run wrote the pump state: %v", err)
	}
	if got := pumpSignalTestCapacityState(t, cf).Plans["p-crw-129"]; len(got.Alerted) != 0 || got.AlertEpoch != 0 {
		t.Fatalf("a dry run committed the alerted state: %+v", got)
	}
}

func TestPumpSignalDryRunPassesDryToCapacity(t *testing.T) {
	f := pumpSignalTestFixture(t, pumpSignalCapacitySource{})
	capFake := pumpSignalTestCapacity(t)
	if _, err := pumpRound(context.Background(), f.e, f.cfg, pumpSettingsFrom(f.cfg), true); err != nil {
		t.Fatal(err)
	}
	if len(capFake.dryCalls) != 1 || !capFake.dryCalls[0] || capFake.commits != 0 {
		t.Fatalf("dry calls %v, commits %d; want one dry read and no commit", capFake.dryCalls, capFake.commits)
	}
}

func pumpSignalTestAlertsPath(f *pumpSignalFixture) string {
	return filepath.Join(f.cfg.StateDir, "audit", auditAlertFile)
}

func pumpSignalTestWriteAlerts(t *testing.T, f *pumpSignalFixture, content string) {
	t.Helper()
	path := pumpSignalTestAlertsPath(f)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func pumpSignalTestAppendAlerts(t *testing.T, f *pumpSignalFixture, content string) {
	t.Helper()
	file, err := os.OpenFile(pumpSignalTestAlertsPath(f), os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString(content); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}

const pumpSignalTestAlertA = `{"mode":"pr","subject":"s1","issue":"CRW-1","pair":"p","phase":"ph","round":"1","score":3,"defects":[{"severity":"P1","what":"broken","where":"a.go:1"}]}`

// A missing alert file is skipped.
func TestPumpSignalAuditMissingFileIsSkipped(t *testing.T) {
	f := pumpSignalTestFixture(t, pumpSignalAuditSource{})
	st := f.round()
	if len(st.Pending) != 0 || st.Sources["signal_audit"] != pumpSourceOK {
		t.Fatalf("pending %+v, source %q; want nothing and ok", st.Pending, st.Sources["signal_audit"])
	}
}

// One low event per complete line, the offset persists, and two identical lines are two events.
func TestPumpSignalAuditOneEventPerLineAndOffsetPersists(t *testing.T) {
	f := pumpSignalTestFixture(t, pumpSignalAuditSource{})
	pumpSignalTestWriteAlerts(t, f, pumpSignalTestAlertA+"\n"+pumpSignalTestAlertA+"\n")
	st := f.round()
	if len(st.Pending) != 2 || st.Pending[0].ID == st.Pending[1].ID {
		t.Fatalf("pending = %+v, want two events with distinct ids for identical lines", st.Pending)
	}
	for _, ev := range st.Pending {
		if ev.Kind != pumpKindLow || !strings.Contains(ev.Text, "CRW-1") || !strings.Contains(ev.Text, "broken") {
			t.Fatalf("event = %+v", ev)
		}
	}
	if st.Offsets[pumpSignalAuditOffsetKey] != int64(2*(len(pumpSignalTestAlertA)+1)) {
		t.Fatalf("offset = %d", st.Offsets[pumpSignalAuditOffsetKey])
	}
	f.deliverAll()
	if st = f.round(); len(st.Pending) != 0 {
		t.Fatalf("a restart re-read the file: %+v", st.Pending)
	}
	pumpSignalTestAppendAlerts(t, f, pumpSignalTestAlertA+"\n")
	st = f.round()
	if len(st.Pending) != 1 || st.Pending[0].ID != "signal_audit#"+strconv.Itoa(2*(len(pumpSignalTestAlertA)+1)) {
		t.Fatalf("pending = %v, want the appended line at its byte offset", pumpSignalIDs(st.Pending))
	}
}

// A half-written last line is skipped and keeps its start offset until it is complete.
func TestPumpSignalAuditPartialLastLineWaits(t *testing.T) {
	f := pumpSignalTestFixture(t, pumpSignalAuditSource{})
	half := pumpSignalTestAlertA[:40]
	pumpSignalTestWriteAlerts(t, f, pumpSignalTestAlertA+"\n"+half)
	st := f.round()
	if len(st.Pending) != 1 {
		t.Fatalf("pending = %+v, want only the complete line", st.Pending)
	}
	start := int64(len(pumpSignalTestAlertA) + 1)
	if st.Offsets[pumpSignalAuditOffsetKey] != start {
		t.Fatalf("offset = %d, want the partial line's start %d", st.Offsets[pumpSignalAuditOffsetKey], start)
	}
	f.deliverAll()
	if st = f.round(); len(st.Pending) != 0 {
		t.Fatalf("the partial line was read: %+v", st.Pending)
	}
	pumpSignalTestAppendAlerts(t, f, pumpSignalTestAlertA[40:]+"\n")
	st = f.round()
	if len(st.Pending) != 1 || st.Pending[0].ID != "signal_audit#"+strconv.FormatInt(start, 10) {
		t.Fatalf("pending = %v, want the completed line at its start offset", pumpSignalIDs(st.Pending))
	}
}

// A blank or malformed complete line is passed over and the offset moves beyond it.
func TestPumpSignalAuditMalformedLineIsSkipped(t *testing.T) {
	f := pumpSignalTestFixture(t, pumpSignalAuditSource{})
	pumpSignalTestWriteAlerts(t, f, "\nnot json\n"+pumpSignalTestAlertA+"\n")
	st := f.round()
	if len(st.Pending) != 1 {
		t.Fatalf("pending = %+v, want the one valid line", st.Pending)
	}
}

// A file that shrank is a new file: the offset restarts and the ids stay distinct from the old ones.
func TestPumpSignalAuditTruncatedFileRestartsWithNewIDs(t *testing.T) {
	f := pumpSignalTestFixture(t, pumpSignalAuditSource{})
	pumpSignalTestWriteAlerts(t, f, pumpSignalTestAlertA+"\n"+pumpSignalTestAlertA+"\n")
	first := f.round().Pending
	f.deliverAll()
	pumpSignalTestWriteAlerts(t, f, pumpSignalTestAlertA+"\n")
	st := f.round()
	if len(st.Pending) != 1 || st.Pending[0].ID == first[0].ID {
		t.Fatalf("pending = %v after a rewrite, want a new id besides %v", pumpSignalIDs(st.Pending), pumpSignalIDs(first))
	}
}

// One source failing is logged and isolated: the others' events are still saved.
func TestPumpSignalOneSourceFailingDoesNotBlockTheOthers(t *testing.T) {
	f := pumpSignalTestFixture(t)
	dag := pumpSignalTestDag(t)
	dag.err = errors.New("store unreadable")
	capFake := pumpSignalTestCapacity(t)
	capFake.report = CapacityReport{Plans: []CapacityPlan{pumpSignalPlan("p1", 1, []string{"CRW-1"}, "CRW-1")}}
	pumpSignalTestWriteAlerts(t, f, pumpSignalTestAlertA+"\n")
	st := f.round()
	if len(st.Pending) != 2 || st.Sources["signal_dag"] != pumpSourceUnmeasured ||
		st.Sources["signal_capacity"] != pumpSourceOK || st.Sources["signal_audit"] != pumpSourceOK {
		t.Fatalf("pending %v, sources %v", pumpSignalIDs(st.Pending), st.Sources)
	}
	logged, err := os.ReadFile(filepath.Join(f.cfg.StateDir, pumpLogFile))
	if err != nil || !strings.Contains(string(logged), "store unreadable") {
		t.Fatalf("pump.log = %q, %v", logged, err)
	}
}

// Capacity failing, with DAG and audit healthy, is the same isolation the other way round.
func TestPumpSignalCapacityFailingDoesNotBlockDagAndAudit(t *testing.T) {
	f := pumpSignalTestFixture(t)
	dag := pumpSignalTestDag(t)
	dag.review = pumpSignalReview(nil, pumpSignalAnomaly("k", "p1", "n1", "CRW-1", "d"))
	capFake := pumpSignalTestCapacity(t)
	capFake.err = errors.New("relay unreadable")
	pumpSignalTestWriteAlerts(t, f, pumpSignalTestAlertA+"\n")
	if st := f.round(); len(st.Pending) != 2 {
		t.Fatalf("pending = %v, want the dag and audit events", pumpSignalIDs(st.Pending))
	}
}

// The three signal sources are in the real registry, after the two the pump already had, and the two flow sources of CRW-1204 follow.
func TestPumpSignalSourcesAreRegistered(t *testing.T) {
	var names []string
	for _, source := range pumpSources {
		names = append(names, source.Name())
	}
	want := "rollout,pr,signal_dag,signal_capacity,signal_audit,flow_runs,flow_reports"
	if strings.Join(names, ",") != want {
		t.Fatalf("sources = %v, want %s", names, want)
	}
}

// The two settings are read from the pump section; absent or non-positive keeps the default.
func TestPumpSignalSettings(t *testing.T) {
	f := pumpSignalTestFixture(t)
	s := pumpSettingsFrom(f.cfg)
	if got := pumpSettingInt(s.DagReviewSeconds, pumpDefaultDagReviewSeconds); got != 600 {
		t.Fatalf("dag_review_seconds default = %d", got)
	}
	if got := pumpSettingInt(s.CapacitySeconds, pumpDefaultCapacitySeconds); got != 1800 {
		t.Fatalf("capacity_seconds default = %d", got)
	}
	f.settings(map[string]any{"dag_review_seconds": 5, "capacity_seconds": 7})
	s = pumpSettingsFrom(f.cfg)
	if pumpSettingInt(s.DagReviewSeconds, 600) != 5 || pumpSettingInt(s.CapacitySeconds, 1800) != 7 {
		t.Fatalf("settings = %+v", s)
	}
}

// An alert names its first three defects, each cut short, and says how many more the file holds.
func TestPumpSignalAuditTextCapsDefects(t *testing.T) {
	row := auditAlertRow{Issue: "CRW-1"}
	for i := 0; i < 5; i++ {
		row.Defects = append(row.Defects, auditAlertDefect{Severity: "P1", What: strings.Repeat("x", 400), Where: "f.go"})
	}
	text := pumpSignalAuditText(row)
	if strings.Count(text, " - P1 ") != 3 || !strings.Contains(text, "+2 more defects") || strings.Contains(text, strings.Repeat("x", 201)) {
		t.Fatalf("text = %q", text)
	}
}

// Two alerting plans and two audit lines in one run are four events with four distinct ids, and the
// capacity commit runs once after all of them are saved.
func TestPumpSignalTwoPlansAndTwoAuditLinesInOneRun(t *testing.T) {
	f := pumpSignalTestFixture(t, pumpSignalCapacitySource{}, pumpSignalAuditSource{})
	capFake := pumpSignalTestCapacity(t)
	capFake.report = CapacityReport{Plans: []CapacityPlan{
		pumpSignalPlan("p1", 1, []string{"CRW-1"}, "CRW-1"),
		pumpSignalPlan("p2", 1, []string{"CRW-2"}, "CRW-2"),
	}}
	pumpSignalTestWriteAlerts(t, f, pumpSignalTestAlertA+"\n"+pumpSignalTestAlertA+"\n")
	var durable int
	capFake.onCommit = func() { durable = len(pumpTestReadState(t, f.cfg).Pending) }
	st := f.round()
	ids := map[string]bool{}
	for _, ev := range st.Pending {
		ids[ev.ID] = true
	}
	if len(st.Pending) != 4 || len(ids) != 4 || durable != 4 || capFake.commits != 1 {
		t.Fatalf("pending %v, durable at commit %d, commits %d; want four distinct events saved before one commit",
			pumpSignalIDs(st.Pending), durable, capFake.commits)
	}
}
