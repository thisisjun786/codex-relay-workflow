package shadow_test

import (
	"encoding/json"
	"slices"
	"sort"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/shadow"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/trace"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/trace/tracetest"
)

func exportCopy(t *testing.T, s *tracetest.Scenario) *trace.Trace {
	t.Helper()
	tr, err := trace.Export(tracetest.Context(t), trace.Options{Path: s.Copy()})
	if err != nil {
		t.Fatal(err)
	}
	return tr
}

func f(v *float64) float64 {
	if v == nil {
		return -1
	}
	return *v
}

// TestIntegrationContentionReport reads the integration batches of the copy: the first batch of plan-1 moved nothing and
// marked nothing (a failed batch), the batch after it landed the same two nodes (one CAS retry), and a batch of plan-2 landed
// at once; the nodes' waits run from their first frozen intent to their mark.
func TestIntegrationContentionReport(t *testing.T) {
	s := tracetest.NewScenario(tracetest.New(t))
	s.IntegrationStage("batch-3", "plan-2", "intent", "", "", "head-3", tracetest.At(300))
	s.IntegrationStage("batch-3", "plan-2", "intent", "n9", "acc-9", "", tracetest.At(300))
	s.IntegrationStage("batch-3", "plan-2", "ref_moved", "", "", "head-3", tracetest.At(300))
	s.IntegrationStage("batch-3", "plan-2", "marked", "n9", "acc-9", "", tracetest.At(301))
	m := shadow.BuildMeasure(exportCopy(t, s), shadow.MeasureOptions{})
	c := m.Contention
	if c.CASRetries != 1 || c.FailedBatches != 1 {
		t.Errorf("contention = %+v, want 1 retry and 1 failed batch", c)
	}
	w := c.WaitSeconds
	if w.Count != 3 || w.Sum != 34 || f(w.Min) != 1 || f(w.P50) != 16 || f(w.P90) != 17 || f(w.Max) != 17 {
		t.Errorf("wait = %+v, want 3 waits of 16, 17 and 1 s", w)
	}
	// a store with no batches reports zeros, never nulls where a count belongs
	empty := shadow.BuildMeasure(&trace.Trace{Schema: trace.Schema}, shadow.MeasureOptions{})
	if empty.Contention.CASRetries != 0 || empty.Contention.FailedBatches != 0 || empty.Contention.WaitSeconds.Count != 0 || empty.Contention.WaitSeconds.Min != nil {
		t.Errorf("empty contention = %+v", empty.Contention)
	}
}

// TestDeliveryLatencyReport reads the delivery rows: the delay from queueing to the first counted send, the deliveries never
// sent, the busy backoffs (journal rows and busy-deferred attempts, as the delivery policy counts them), and the sends per
// relationship (distinct events with a counted send).
func TestDeliveryLatencyReport(t *testing.T) {
	s := tracetest.NewScenario(tracetest.New(t))
	s.Delivery("ev-4", "rel-3", "task-b", "queued", "notLoaded", tracetest.At(220), nil, 0, 0)
	m := shadow.BuildMeasure(exportCopy(t, s), shadow.MeasureOptions{})
	lat := m.R2.DeliveryLatency
	if m.R2.Deliveries != 4 || m.R2.Undelivered != 2 || lat.Count != 2 || lat.Sum != 14 || f(lat.Min) != 4 || f(lat.P50) != 4 || f(lat.P90) != 10 || f(lat.Max) != 10 {
		t.Errorf("r2 = %+v", m.R2)
	}
	if m.R6.SendsTotal != 2 || m.R6.SendsPerRelationship["rel-1"] != 2 || len(m.R6.SendsPerRelationship) != 1 || m.R6.NotLoadedQueued != 1 {
		t.Errorf("r6 = %+v", m.R6)
	}
	if m.R3b.BusyBackoffs != 5 || m.R3b.EventsWithBackoff != 2 || m.R3b.MaxPerEvent != 3 || m.R3b.RecipientsWithSeveral != 1 {
		t.Errorf("r3b = %+v", m.R3b)
	}
}

// TestMeasureReportHasNineKeys writes the measurement table of the synthetic copy and checks what F17 reads: the nine
// measures, contention with its three values, and the r2, r6, r3b, r11 and r12 evidence; and the figures of the scenario.
func TestMeasureReportHasNineKeys(t *testing.T) {
	s := tracetest.NewScenario(tracetest.New(t))
	m := shadow.BuildMeasure(exportCopy(t, s), shadow.MeasureOptions{OversizedTokens: 1500})
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		t.Fatal(err)
	}
	var keys []string
	for k := range top {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	nine := []string{"tokens_per_accepted_issue", "time_to_first_acceptance_seconds", "tier_failure_rates", "outputschema_compliance_ocx", "oversized_threshold_hits", "orchestrator_turns_per_receipt", "executor_thread_cache", "recovery_holds_per_run", "flow_audit_result"}
	for _, k := range append(append([]string{}, nine...), "contention", "r2", "r6", "r3b", "r11", "r12") {
		if !slices.Contains(keys, k) {
			t.Errorf("the table has no key %s (keys: %v)", k, keys)
		}
	}
	var contention map[string]json.RawMessage
	if err := json.Unmarshal(top["contention"], &contention); err != nil {
		t.Fatal(err)
	}
	var ck []string
	for k := range contention {
		ck = append(ck, k)
	}
	sort.Strings(ck)
	if want := []string{"cas_retries", "failed_batches", "wait_seconds"}; !slices.Equal(ck, want) {
		t.Errorf("contention keys = %v, want %v", ck, want)
	}

	if tk := m.TokensPerAcceptedIssue; tk.AcceptedNodes != 2 || tk.TokensTotal != 3000 || f(tk.Mean) != 1500 || tk.ByNode["run-a/n1"] != 1400 || tk.ByNode["run-a/n2"] != 1600 {
		t.Errorf("tokens per accepted issue = %+v", tk)
	}
	if ta := m.TimeToFirstAcceptance; ta.PerRun["run-a"] != 99 || ta.Seconds.Count != 1 {
		t.Errorf("time to first acceptance = %+v", ta)
	}
	if ex := m.TierFailureRates.ByRole["executor"]; ex.Calls != 3 || ex.Failed != 1 || f(ex.Rate) != 1.0/3 || ex.ByStatus["unknown"] != 1 {
		t.Errorf("executor failures = %+v", ex)
	}
	if rv := m.TierFailureRates.ByRole["reviewer"]; rv.Calls != 1 || rv.Failed != 0 || f(rv.Rate) != 0 {
		t.Errorf("reviewer failures = %+v", rv)
	}
	if oc := m.OutputSchemaComplianceOCX; oc.All.Calls != 2 || oc.All.FirstTry != 2 || f(oc.All.Rate) != 1 || oc.ViaOCX.Calls != 1 || oc.ByModel["gpt-fake-sol"].Calls != 1 {
		t.Errorf("schema compliance = %+v", oc)
	}
	if ov := m.OversizedThresholdHits; ov.ThresholdTokens == nil || *ov.ThresholdTokens != 1500 || ov.Total != 3 || ov.Hits == nil || *ov.Hits != 2 {
		t.Errorf("oversized hits = %+v", ov)
	}
	if ot := m.OrchestratorTurnsReceipt; ot.OrchestratorTurns != 0 || ot.Receipts != 3 || f(ot.Value) != 0 {
		t.Errorf("orchestrator turns = %+v", ot)
	}
	if tc := m.ExecutorThreadCache; tc.Threads != 4 || tc.ContinuedCalls != 0 || tc.Turns != 3 || f(tc.ReuseRatio) != 0 || f(tc.TurnsPerThread) != 0.75 {
		t.Errorf("thread cache = %+v", tc)
	}
	if rh := m.RecoveryHoldsPerRun; rh.Total != 1 || rh.PerRun["run-a"] != 1 || f(rh.Mean) != 1 {
		t.Errorf("recovery holds = %+v", rh)
	}
	if au := m.FlowAuditResult; au.Acceptances != 2 || au.Observed != 2 || !au.OK || au.LiveAuditRun {
		t.Errorf("audit = %+v", au)
	}
	if m.R11.SyncOutboxRows != 2 || m.R11.ByState["pending"] != 1 || m.R12.MergeTrains.Trains != 1 {
		t.Errorf("r11 / r12 = %+v / %+v", m.R11, m.R12)
	}
	// without a threshold the hit rate is not computed
	if ov := shadow.BuildMeasure(exportCopy(t, s), shadow.MeasureOptions{}).OversizedThresholdHits; ov.ThresholdTokens != nil || ov.Hits != nil || ov.Total != 3 {
		t.Errorf("oversized hits without a threshold = %+v", ov)
	}
	// an acceptance whose reviewer call is not in the trace is unobserved
	s.Acceptance("acc-x", "plan-1", "n9", "x-n9", "c_missing", tracetest.At(400))
	au := shadow.BuildMeasure(exportCopy(t, s), shadow.MeasureOptions{}).FlowAuditResult
	if au.Unobserved != 1 || au.OK {
		t.Errorf("audit with an unobserved acceptance = %+v", au)
	}
}
