package shadow

import (
	"math"
	"sort"
	"strings"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/trace"
)

// MeasureSchema is the format name of the measurement table.
const MeasureSchema = "crw-flow-measure/1"

// MeasureOptions are the thresholds a measurement needs from its caller.
type MeasureOptions struct {
	// OversizedTokens is the token total above which a node counts as an oversized issue (the threshold F17 decides);
	// 0 sets none and the oversized hit rate is not computed.
	OversizedTokens int64
}

// Measure is the measurement table of a trace: the nine measures F17 reads, the integration contention, and the figures the
// R2, R6, R3b, R11 and R12 decisions rest on. Every figure is computed from the trace alone.
type Measure struct {
	Schema string   `json:"schema"`
	Trace  TraceRef `json:"trace"`
	Runs   []string `json:"runs"`

	TokensPerAcceptedIssue    TokensPerIssue    `json:"tokens_per_accepted_issue"`
	TimeToFirstAcceptance     FirstAcceptance   `json:"time_to_first_acceptance_seconds"`
	TierFailureRates          TierFailures      `json:"tier_failure_rates"`
	OutputSchemaComplianceOCX SchemaCompliance  `json:"outputschema_compliance_ocx"`
	OversizedThresholdHits    OversizedHits     `json:"oversized_threshold_hits"`
	OrchestratorTurnsReceipt  OrchestratorTurns `json:"orchestrator_turns_per_receipt"`
	ExecutorThreadCache       ThreadCache       `json:"executor_thread_cache"`
	RecoveryHoldsPerRun       RecoveryHolds     `json:"recovery_holds_per_run"`
	FlowAuditResult           AuditResult       `json:"flow_audit_result"`

	Contention Contention `json:"contention"`
	R2         R2         `json:"r2"`
	R6         R6         `json:"r6"`
	R3b        R3b        `json:"r3b"`
	R11        R11        `json:"r11"`
	R12        R12        `json:"r12"`
}

// Dist is the distribution of a set of seconds. Count 0 has no minimum, percentile or maximum (they are null). Percentiles are
// nearest-rank.
type Dist struct {
	Count int      `json:"count"`
	Sum   float64  `json:"sum"`
	Min   *float64 `json:"min"`
	P50   *float64 `json:"p50"`
	P90   *float64 `json:"p90"`
	Max   *float64 `json:"max"`
}

func dist(values []float64) Dist {
	d := Dist{Count: len(values)}
	if len(values) == 0 {
		return d
	}
	v := append([]float64(nil), values...)
	sort.Float64s(v)
	at := func(p float64) *float64 {
		i := int(math.Ceil(p*float64(len(v)))) - 1
		x := v[max(0, min(i, len(v)-1))]
		return &x
	}
	for _, x := range v {
		d.Sum += x
	}
	lo, hi := v[0], v[len(v)-1]
	d.Min, d.P50, d.P90, d.Max = &lo, at(0.5), at(0.9), &hi
	return d
}

func ratio(n, d int) *float64 {
	if d == 0 {
		return nil
	}
	x := float64(n) / float64(d)
	return &x
}

// seconds is b - a in seconds, ok false when either time does not parse.
func seconds(a, b string) (float64, bool) {
	ta, err := time.Parse(time.RFC3339Nano, a)
	if err != nil {
		return 0, false
	}
	tb, err := time.Parse(time.RFC3339Nano, b)
	if err != nil {
		return 0, false
	}
	return tb.Sub(ta).Seconds(), true
}

// TokensPerIssue is the tokens the calls of the accepted nodes used, per accepted node.
type TokensPerIssue struct {
	AcceptedNodes int              `json:"accepted_nodes"`
	TokensTotal   int64            `json:"tokens_total"`
	Mean          *float64         `json:"mean"`
	ByNode        map[string]int64 `json:"by_node"`
}

// FirstAcceptance is the time from the start of a run to the first acceptance of one of its nodes.
type FirstAcceptance struct {
	PerRun  map[string]float64 `json:"per_run"`
	Seconds Dist               `json:"seconds"`
}

// TierFailures counts, per role, the calls that did not complete: a call whose status is not completed and not superseded.
type TierFailures struct {
	ByRole map[string]RoleFailures `json:"by_role"`
}

// RoleFailures is the failure count of one role.
type RoleFailures struct {
	Calls    int            `json:"calls"`
	Failed   int            `json:"failed"`
	Rate     *float64       `json:"rate"`
	ByStatus map[string]int `json:"by_status"`
}

// SchemaCompliance is how often calls with an output schema answered it: the calls that reached output validation (completed,
// or null_model after the repair turn) split into answered at the first turn, answered after the repair turn, and not
// answered. A call is counted via ocx when its model is not a Claude model (this host runs Claude models directly and the
// others through ocx).
type SchemaCompliance struct {
	All     Compliance            `json:"all"`
	ViaOCX  Compliance            `json:"via_ocx"`
	ByModel map[string]Compliance `json:"by_model"`
}

// Compliance is one row of the compliance table.
type Compliance struct {
	Calls    int      `json:"calls"`
	FirstTry int      `json:"first_try"`
	Repaired int      `json:"repaired"`
	Failed   int      `json:"failed"`
	Rate     *float64 `json:"rate"`
}

// OversizedHits compares a token threshold with what happened to the nodes: a node is predicted oversized when its calls used
// more than the threshold and struggled when it was escalated or stepped to executor-t1; a hit is a node where the two agree.
type OversizedHits struct {
	ThresholdTokens *int64          `json:"threshold_tokens"`
	Nodes           []OversizedNode `json:"nodes"`
	Hits            *int            `json:"hits"`
	Total           int             `json:"total"`
}

// OversizedNode is one node's token total and whether it struggled.
type OversizedNode struct {
	Node      string `json:"node"`
	Tokens    int64  `json:"tokens"`
	Struggled bool   `json:"struggled"`
}

// OrchestratorTurns is the turns of the architect (orchestrator) calls per executor receipt (a completed executor call).
type OrchestratorTurns struct {
	OrchestratorTurns int      `json:"orchestrator_turns"`
	Receipts          int      `json:"receipts"`
	Value             *float64 `json:"value"`
}

// ThreadCache is how the executor calls used threads: a call that continues a handle reuses a thread, any other starts one.
// The journal does not hold cached input tokens, so the reuse figures are the evidence of the cache behavior.
type ThreadCache struct {
	Threads        int      `json:"threads_started"`
	ContinuedCalls int      `json:"continued_calls"`
	Turns          int      `json:"turns"`
	ReuseRatio     *float64 `json:"reuse_ratio"`
	TurnsPerThread *float64 `json:"turns_per_thread"`
}

// RecoveryHolds counts, per run, the nodes that hold a call with an unknown outcome or that a recovery hold stopped.
type RecoveryHolds struct {
	PerRun map[string]int `json:"per_run"`
	Total  int            `json:"total"`
	Mean   *float64       `json:"mean"`
}

// AuditResult is the trace-level audit of the acceptances: every agent acceptance names a reviewer call, and that call is in
// the trace, completed, with a reviewer role. `crw flow audit` itself reads the live store and is run by F17.
type AuditResult struct {
	Source       string `json:"source"`
	Acceptances  int    `json:"acceptances"`
	Observed     int    `json:"observed"`
	Unobserved   int    `json:"unobserved"`
	Mismatch     int    `json:"mismatch"`
	OK           bool   `json:"ok"`
	LiveAuditRun bool   `json:"live_audit_run"`
}

// Contention is the integration contention: the batches retried after a failed batch of their plan that shares a node, the time
// the landed nodes waited from their first frozen intent to their mark, and the batches that moved nothing and marked nothing.
type Contention struct {
	CASRetries    int  `json:"cas_retries"`
	WaitSeconds   Dist `json:"wait_seconds"`
	FailedBatches int  `json:"failed_batches"`
}

// R2 is the evidence for the status subscription of long-lived threads: the delay from queueing a delivery to its first send.
type R2 struct {
	Deliveries      int  `json:"deliveries"`
	Undelivered     int  `json:"undelivered"`
	DeliveryLatency Dist `json:"delivery_latency_seconds"`
}

// R6 is the evidence for the report path: sends per relationship (distinct events with a counted send), and the deliveries held
// as not loaded.
type R6 struct {
	SendsPerRelationship map[string]int `json:"sends_per_relationship"`
	SendsTotal           int            `json:"sends_total"`
	NotLoadedQueued      int            `json:"not_loaded_queued"`
}

// R3b is the evidence for delivering several events in one turn: the busy backoffs and the recipients that had several events
// backing off at once.
type R3b struct {
	BusyBackoffs          int `json:"busy_backoffs"`
	EventsWithBackoff     int `json:"events_with_backoff"`
	MaxPerEvent           int `json:"max_backoffs_per_event"`
	RecipientsWithSeveral int `json:"recipients_with_several_backing_off"`
}

// R11 is the evidence for deleting the sync_outbox write: its rows by state.
type R11 struct {
	SyncOutboxRows int            `json:"sync_outbox_rows"`
	ByState        map[string]int `json:"by_state"`
}

// R12 is the evidence for retiring the merge train: its rows.
type R12 struct {
	MergeTrains trace.MergeTrains `json:"merge_trains"`
}

// BuildMeasure computes the measurement table of a trace.
func BuildMeasure(tr *trace.Trace, opts MeasureOptions) *Measure {
	m := &Measure{Schema: MeasureSchema, Trace: TraceRef{StoreDigest: tr.Source.Digest}, Runs: []string{}}
	for _, r := range tr.Runs {
		m.Runs = append(m.Runs, r.RunID)
	}
	m.tokensAndAcceptance(tr)
	m.calls(tr, opts)
	m.audit(tr)
	m.contention(tr)
	m.deliveries(tr)
	m.R11 = R11{SyncOutboxRows: tr.Aux.SyncOutbox.Rows, ByState: copyCounts(tr.Aux.SyncOutbox.ByState)}
	m.R12 = R12{MergeTrains: tr.Aux.MergeTrains}
	return m
}

func copyCounts(in map[string]int) map[string]int {
	out := map[string]int{}
	for k, v := range in {
		out[k] = v
	}
	return out
}

func (m *Measure) tokensAndAcceptance(tr *trace.Trace) {
	m.TokensPerAcceptedIssue = TokensPerIssue{ByNode: map[string]int64{}}
	m.TimeToFirstAcceptance = FirstAcceptance{PerRun: map[string]float64{}}
	var firsts []float64
	for _, r := range tr.Runs {
		tokens := map[string]int64{}
		for _, c := range r.Calls {
			tokens[c.Node] += c.Tokens.Total
		}
		first := ""
		for _, n := range r.Nodes {
			for _, t := range n.Transitions {
				if t.Stage != "accepted" {
					continue
				}
				m.TokensPerAcceptedIssue.AcceptedNodes++
				m.TokensPerAcceptedIssue.TokensTotal += tokens[n.NodeID]
				m.TokensPerAcceptedIssue.ByNode[r.RunID+"/"+n.NodeID] = tokens[n.NodeID]
				if first == "" || t.At < first {
					first = t.At
				}
				break
			}
		}
		if first != "" {
			if s, ok := seconds(r.StartedAt, first); ok {
				m.TimeToFirstAcceptance.PerRun[r.RunID] = s
				firsts = append(firsts, s)
			}
		}
	}
	m.TokensPerAcceptedIssue.Mean = nil
	if n := m.TokensPerAcceptedIssue.AcceptedNodes; n > 0 {
		x := float64(m.TokensPerAcceptedIssue.TokensTotal) / float64(n)
		m.TokensPerAcceptedIssue.Mean = &x
	}
	m.TimeToFirstAcceptance.Seconds = dist(firsts)
}

func (m *Measure) calls(tr *trace.Trace, opts MeasureOptions) {
	m.TierFailureRates = TierFailures{ByRole: map[string]RoleFailures{}}
	m.OutputSchemaComplianceOCX = SchemaCompliance{ByModel: map[string]Compliance{}}
	var threshold *int64
	if opts.OversizedTokens > 0 {
		t := opts.OversizedTokens
		threshold = &t
	}
	m.OversizedThresholdHits = OversizedHits{ThresholdTokens: threshold, Nodes: []OversizedNode{}}
	m.RecoveryHoldsPerRun = RecoveryHolds{PerRun: map[string]int{}}
	for _, r := range tr.Runs {
		tokens := map[string]int64{}
		stepped := map[string]bool{}
		held := map[string]bool{}
		for _, c := range r.Calls {
			rf := m.TierFailureRates.ByRole[c.Role]
			rf.Calls++
			if rf.ByStatus == nil {
				rf.ByStatus = map[string]int{}
			}
			rf.ByStatus[c.Status]++
			if c.Status != engine.StatusCompleted && c.Status != engine.StatusSuperseded {
				rf.Failed++
			}
			m.TierFailureRates.ByRole[c.Role] = rf

			if c.SchemaDigest != "" {
				reached := c.Status == engine.StatusCompleted || (c.Status == engine.StatusNullModel && c.Reason == engine.ReasonSchemaInvalidAfterRepair)
				if reached {
					addCompliance(&m.OutputSchemaComplianceOCX.All, c)
					bm := m.OutputSchemaComplianceOCX.ByModel[c.Model]
					addCompliance(&bm, c)
					m.OutputSchemaComplianceOCX.ByModel[c.Model] = bm
					if !strings.HasPrefix(c.Model, "claude") {
						addCompliance(&m.OutputSchemaComplianceOCX.ViaOCX, c)
					}
				}
			}
			tokens[c.Node] += c.Tokens.Total
			if c.Role == "executor-t1" {
				stepped[c.Node] = true
			}
			if c.Role == "architect" {
				m.OrchestratorTurnsReceipt.OrchestratorTurns += c.Turns
			}
			if c.Family == "executor" {
				if c.ResumeOf == "" {
					m.ExecutorThreadCache.Threads++
				} else {
					m.ExecutorThreadCache.ContinuedCalls++
				}
				m.ExecutorThreadCache.Turns += c.Turns
				if c.Status == engine.StatusCompleted {
					m.OrchestratorTurnsReceipt.Receipts++
				}
			}
			if c.Node != "" && (c.Status == engine.StatusUnknown || c.Reason == engine.ReasonRecoveryHold) {
				held[c.Node] = true
			}
		}
		m.RecoveryHoldsPerRun.PerRun[r.RunID] = len(held)
		m.RecoveryHoldsPerRun.Total += len(held)
		escalated := map[string]bool{}
		for _, n := range r.Nodes {
			for _, t := range n.Transitions {
				if t.Stage == "escalated" {
					escalated[n.NodeID] = true
				}
			}
		}
		var nodes []string
		for n := range tokens {
			if n != "" {
				nodes = append(nodes, n)
			}
		}
		sort.Strings(nodes)
		for _, n := range nodes {
			m.OversizedThresholdHits.Nodes = append(m.OversizedThresholdHits.Nodes, OversizedNode{Node: r.RunID + "/" + n, Tokens: tokens[n], Struggled: escalated[n] || stepped[n]})
		}
	}
	for role, rf := range m.TierFailureRates.ByRole {
		rf.Rate = ratio(rf.Failed, rf.Calls)
		m.TierFailureRates.ByRole[role] = rf
	}
	finishCompliance(&m.OutputSchemaComplianceOCX.All)
	finishCompliance(&m.OutputSchemaComplianceOCX.ViaOCX)
	for k, c := range m.OutputSchemaComplianceOCX.ByModel {
		finishCompliance(&c)
		m.OutputSchemaComplianceOCX.ByModel[k] = c
	}
	h := &m.OversizedThresholdHits
	h.Total = len(h.Nodes)
	if threshold != nil {
		hits := 0
		for _, n := range h.Nodes {
			if (n.Tokens > *threshold) == n.Struggled {
				hits++
			}
		}
		h.Hits = &hits
	}
	o := &m.OrchestratorTurnsReceipt
	o.Value = ratio(o.OrchestratorTurns, o.Receipts)
	c := &m.ExecutorThreadCache
	c.ReuseRatio = ratio(c.ContinuedCalls, c.Threads+c.ContinuedCalls)
	c.TurnsPerThread = ratio(c.Turns, c.Threads)
	if n := len(tr.Runs); n > 0 {
		x := float64(m.RecoveryHoldsPerRun.Total) / float64(n)
		m.RecoveryHoldsPerRun.Mean = &x
	}
}

func addCompliance(c *Compliance, call trace.Call) {
	c.Calls++
	switch {
	case call.Status != engine.StatusCompleted:
		c.Failed++
	case call.RepairUsed:
		c.Repaired++
	default:
		c.FirstTry++
	}
}

func finishCompliance(c *Compliance) { c.Rate = ratio(c.FirstTry+c.Repaired, c.Calls) }

func (m *Measure) audit(tr *trace.Trace) {
	calls := map[string]trace.Call{}
	for _, r := range tr.Runs {
		for _, c := range r.Calls {
			calls[c.CallID] = c
		}
	}
	a := AuditResult{Source: "trace"}
	for _, acc := range tr.Acceptances {
		if acc.Kind != "agent" {
			continue
		}
		a.Acceptances++
		c, ok := calls[acc.ReviewerCallID]
		switch {
		case !ok:
			a.Unobserved++
		case c.Status != engine.StatusCompleted || !strings.HasPrefix(c.Role, "reviewer"):
			a.Mismatch++
		default:
			a.Observed++
		}
	}
	a.OK = a.Unobserved == 0 && a.Mismatch == 0
	m.FlowAuditResult = a
}

func (m *Measure) contention(tr *trace.Trace) {
	byPlan := map[string][]trace.Batch{}
	var plans []string
	for _, b := range tr.Integration {
		if _, ok := byPlan[b.PlanID]; !ok {
			plans = append(plans, b.PlanID)
		}
		byPlan[b.PlanID] = append(byPlan[b.PlanID], b)
	}
	sort.Strings(plans)
	var waits []float64
	for _, plan := range plans {
		batches := byPlan[plan]
		sort.SliceStable(batches, func(i, k int) bool { return batches[i].IntentAt < batches[k].IntentAt })
		failedNodes := map[string]bool{}
		firstIntent := map[string]string{}
		for _, b := range batches {
			retry := false
			for _, n := range b.Nodes {
				if failedNodes[n.NodeID] {
					retry = true
				}
			}
			if retry {
				m.Contention.CASRetries++
			}
			if !b.Landed {
				m.Contention.FailedBatches++
				for _, n := range b.Nodes {
					failedNodes[n.NodeID] = true
				}
			}
			for _, n := range b.Nodes {
				if at, ok := firstIntent[n.NodeID]; !ok || (n.IntentAt != "" && n.IntentAt < at) {
					firstIntent[n.NodeID] = n.IntentAt
				}
				if n.MarkedAt != "" {
					if s, ok := seconds(firstIntent[n.NodeID], n.MarkedAt); ok {
						waits = append(waits, s)
					}
				}
			}
		}
	}
	m.Contention.WaitSeconds = dist(waits)
}

func (m *Measure) deliveries(tr *trace.Trace) {
	var lat []float64
	m.R6 = R6{SendsPerRelationship: map[string]int{}}
	backoffRecipients := map[string]int{}
	for _, d := range tr.Deliveries {
		m.R2.Deliveries++
		if d.FirstSentAt == "" {
			m.R2.Undelivered++
		} else {
			if s, ok := seconds(d.CreatedAt, d.FirstSentAt); ok {
				lat = append(lat, s)
			}
			m.R6.SendsPerRelationship[d.RelationshipID]++
			m.R6.SendsTotal++
		}
		if strings.Contains(d.HoldReason, "notLoaded") || strings.Contains(d.HoldReason, "not_loaded") {
			m.R6.NotLoadedQueued++
		}
		if d.BusyAnswers > 0 {
			m.R3b.BusyBackoffs += d.BusyAnswers
			m.R3b.EventsWithBackoff++
			m.R3b.MaxPerEvent = max(m.R3b.MaxPerEvent, d.BusyAnswers)
			backoffRecipients[d.RecipientTaskID]++
		}
	}
	for _, n := range backoffRecipients {
		if n >= 2 {
			m.R3b.RecipientsWithSeveral++
		}
	}
	m.R2.DeliveryLatency = dist(lat)
}
