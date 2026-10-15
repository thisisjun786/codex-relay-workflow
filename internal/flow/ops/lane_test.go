package ops

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/roles"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/script"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/acceptance/premerge"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dagsched"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// stubRelay answers what the operations under test ask of the scheduler and remembers it.
type stubRelay struct {
	releases []ReleaseRequest
	closes   []CloseRequest
	tip      string
	// plan is the milestone the relay reads, when a test sets one.
	plan *Milestone
	// milestoneErr, when a test sets it, is what the relay answers when it is asked for the plan.
	milestoneErr error
	// relaySlice, when a test sets it, is the slice of a node the relay releases from, where it is not the one of plan (the flow's read).
	relaySlice map[string]string
	// afterRelease, when a test sets it, runs after the relay answered a release.
	afterRelease func()
	// releaseErr, when a test sets it, is the error the relay answers a release with (CRW-1285).
	releaseErr error
	// closeErr, when a test sets it, is the error the relay answers a close with (CRW-1354).
	closeErr error
	// closeErrs, when a test sets it, are the errors the relay answers the next closes with, one each, before closeErr (CRW-1397).
	closeErrs []error
}

func (s *stubRelay) Milestone(context.Context, string, string) (Milestone, error) {
	if s.milestoneErr != nil {
		return Milestone{}, s.milestoneErr
	}
	if s.plan != nil {
		return *s.plan, nil
	}
	return Milestone{}, nil
}
func (s *stubRelay) Release(_ context.Context, r ReleaseRequest) (ReleaseResult, error) {
	s.releases = append(s.releases, r)
	if s.afterRelease != nil {
		defer s.afterRelease()
	}
	if s.releaseErr != nil {
		return ReleaseResult{}, s.releaseErr
	}
	// the relay compares the slice the flow compared with the node's slice in its release transaction (CRW-1288)
	slice, ok := s.relaySlice[r.Node]
	if !ok && s.plan != nil {
		slice = liveSlices(*s.plan)[r.Node]
	}
	if r.SliceDigest != "" && r.SliceDigest != slice {
		return ReleaseResult{Reason: ReasonPlanChanged}, nil
	}
	return ReleaseResult{Released: true, ExecutionID: "exec-" + r.Node, SliceDigest: slice}, nil
}
func (s *stubRelay) ReleaseStands(context.Context, string, string, string) (bool, error) {
	return true, nil
}
func (s *stubRelay) Accept(context.Context, AcceptRequest) (AcceptResult, error) {
	return AcceptResult{Accepted: true}, nil
}
func (s *stubRelay) AcceptState(context.Context, string) (AcceptState, bool, error) {
	return AcceptState{}, false, nil
}
func (s *stubRelay) Integrate(context.Context, IntegrateRequest) (IntegrateResult, error) {
	return IntegrateResult{Landed: true}, nil
}
func (s *stubRelay) IntegrationTip(context.Context, string) (string, error) { return s.tip, nil }
func (s *stubRelay) Close(_ context.Context, r CloseRequest) (CloseResult, error) {
	s.closes = append(s.closes, r)
	if len(s.closeErrs) > 0 {
		err := s.closeErrs[0]
		s.closeErrs = s.closeErrs[1:]
		if err != nil {
			return CloseResult{}, err
		}
		return CloseResult{Closed: true}, nil
	}
	if s.closeErr != nil {
		return CloseResult{}, s.closeErr
	}
	return CloseResult{Closed: true}, nil
}

// world is a journal with the rows the operations read, and the lane over it.
type world struct {
	t     *testing.T
	j     *engine.MemoryJournal
	relay *stubRelay
	lane  *Lane
	calls int
}

func newWorld(t *testing.T) *world {
	t.Helper()
	w := &world{t: t, j: engine.NewMemoryJournal(), relay: &stubRelay{tip: strings.Repeat("1", 40)}}
	w.lane = NewLane(Deps{RunID: "run-1", Journal: w.j, Relay: w.relay, Decisions: &MemoryDecisions{}, Actor: "parent", Now: func() time.Time { return time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC) }})
	return w
}

// op runs an operation as the engine would and writes its completed row, so the next operation reads it.
func (w *world) op(name string, argv ...any) (json.RawMessage, error) {
	w.t.Helper()
	raw, err := json.Marshal(argv)
	if err != nil {
		w.t.Fatal(err)
	}
	w.calls++
	callID := fmt.Sprintf("c_%012d", w.calls)
	op := w.lane.Ops()[name]
	res, err := op.Run(context.Background(), engine.OpRequest{RunID: "run-1", CallID: callID, RequestID: engine.RequestID("run-1", callID), Args: raw, Journal: w.j})
	if err != nil {
		return nil, err
	}
	data := map[string]any{"result": string(res.Result)}
	for k, v := range res.Data {
		data[k] = v
	}
	// the journal stores a row's data as JSON: the round trip is part of the contract the operations read
	b, _ := json.Marshal(data)
	var round map[string]any
	_ = json.Unmarshal(b, &round)
	w.j.Append(engine.Event{RunID: "run-1", Kind: engine.KindCall, CallID: callID, Op: "crw." + name, Event: engine.EvCompleted, Data: round})
	return res.Result, nil
}

func (w *world) must(name string, argv ...any) json.RawMessage {
	w.t.Helper()
	out, err := w.op(name, argv...)
	if err != nil {
		w.t.Fatalf("crw.%s: %v", name, err)
	}
	return out
}

func designOf(nodes ...string) Design {
	d := Design{Decisions: []Decision{{ID: "D1", Evidence: []string{}, Change: "change", Alternatives: []string{}, Assumptions: []string{}}}, OpenQuestions: []OpenQuestion{}, Risks: []string{}}
	for _, n := range nodes {
		d.Issues = append(d.Issues, DesignIssue{Issue: "CRW-" + n, Node: n, SizeFit: "executor", IssueSizeDecision: "ok", EditRegions: []string{n + "/"}, Approach: "approach " + n,
			TestsFirst: []string{}, DoneCondition: "done " + n, DecisionRefs: []string{"D1"}, OrderAfter: []string{}})
	}
	return d
}

func call(id, status, model string, output any) CallRef {
	var c CallRef
	c.CallID, c.Status, c.Handle = id, status, "crwh1:run-1:"+id
	c.Meta.ConfiguredPair.Model = model
	if output != nil {
		c.Output, _ = json.Marshal(output)
	}
	return c
}

// ledgered makes the rows of a milestone M whose DESIGN passed its audit and became revision r0.
func (w *world) ledgered(audit bool, d Design) (digest string) {
	w.t.Helper()
	w.must("milestone", "plan-1", "M")
	design, _ := json.Marshal(d)
	cand := Candidate{Design: design, Answers: json.RawMessage(`[]`)}
	if audit {
		out := w.must("designAudit", "M", 1, cand, call("c_arch", "completed", "claude-x", nil),
			call("c_aud", "completed", "gpt-y", map[string]any{"verdict": "approved", "findings": []any{}, "summary": "ok"}))
		var res AuditResult
		_ = json.Unmarshal(out, &res)
		if res.State != "approved" {
			w.t.Fatalf("audit %+v", res)
		}
	}
	if _, err := w.op("designLedger", "M", map[string]any{"revision": "r0", "design": json.RawMessage(design), "answers": json.RawMessage(`[]`), "architect": call("c_arch", "completed", "claude-x", nil)}); err != nil {
		if audit {
			w.t.Fatal(err)
		}
		return ""
	}
	return cand.digest()
}

// No executor starts before the audit approved the DESIGN: the ledger refuses a DESIGN with no approved audit, and a release
// refuses a node whose revision has none.
func TestLedgerAndReleaseNeedAnApprovedAudit(t *testing.T) {
	w := newWorld(t)
	if d := w.ledgered(false, designOf("A")); d != "" {
		t.Fatalf("the ledger took a DESIGN with no audit")
	}
	var rel ReleaseOutcome
	_ = json.Unmarshal(w.must("release", "plan-1", "A", "M"), &rel)
	if rel.Released || rel.Reason != "design_missing" {
		t.Errorf("release with no ledger: %+v", rel)
	}
	if len(w.relay.releases) != 0 {
		t.Errorf("the relay was asked to release")
	}

	w = newWorld(t)
	digest := w.ledgered(true, designOf("A"))
	_ = json.Unmarshal(w.must("release", "plan-1", "A", "M"), &rel)
	if !rel.Released || rel.Revision != "r0" || rel.DesignDigest != digest || rel.ExecutionID != "exec-A" {
		t.Errorf("release: %+v", rel)
	}
	if len(w.relay.releases) != 1 || w.relay.releases[0].DesignDigest != digest || w.relay.releases[0].Revision != "r0" {
		t.Errorf("the relay was asked %+v", w.relay.releases)
	}
	// a node the DESIGN does not plan is not released
	_ = json.Unmarshal(w.must("release", "plan-1", "Z", "M"), &rel)
	if rel.Released || rel.Reason != "node_not_designed" {
		t.Errorf("release of an unplanned node: %+v", rel)
	}
	// a revision after r0 is recorded only over an audit of its own, made after r0: the audit r0 passed on the same DESIGN stands in for
	// nothing
	design, _ := json.Marshal(designOf("A"))
	if _, err := w.op("designLedger", "M", map[string]any{"revision": "r1", "design": json.RawMessage(design)}); err == nil || !strings.Contains(err.Error(), "no approved audit") {
		t.Errorf("a revision after r0 over the audit of r0: %v", err)
	}
	// and a revision is named by the ledger: the next one
	if _, err := w.op("designLedger", "M", map[string]any{"revision": "r7", "design": json.RawMessage(design)}); err == nil || !strings.Contains(err.Error(), "adds r1 next") {
		t.Errorf("a revision out of order: %v", err)
	}
}

// An audit that was not made by an independent reviewer is not an approval: the model family of the auditor differs from the
// architect's, a call that did not complete is not an audit, and an approval over a P1 finding is no approval.
func TestDesignAuditIndependence(t *testing.T) {
	w := newWorld(t)
	design, _ := json.Marshal(designOf("A"))
	cand := Candidate{Design: design, Answers: json.RawMessage(`[]`)}
	approved := map[string]any{"verdict": "approved", "findings": []any{}, "summary": "ok"}
	state := func(arch, aud CallRef) AuditResult {
		var r AuditResult
		_ = json.Unmarshal(w.must("designAudit", "M", 1, cand, arch, aud), &r)
		return r
	}
	if r := state(call("a", "completed", "claude-opus", nil), call("r", "completed", "claude-sonnet", approved)); r.State != StateRefused || r.Reason != "independence_refused" {
		t.Errorf("same family: %+v", r)
	}
	if r := state(call("a", "completed", "claude-opus", nil), call("r", "completed", "", approved)); r.State != StateRefused || r.Reason != "independence_unproven" {
		t.Errorf("unread pair: %+v", r)
	}
	if r := state(call("a", "completed", "claude-opus", nil), call("r", "held", "gpt-x", nil)); r.State != StateHeld {
		t.Errorf("held: %+v", r)
	}
	if r := state(call("a", "completed", "claude-opus", nil), call("r", "completed", "gpt-x", map[string]any{"verdict": "approved", "summary": "x", "findings": []any{map[string]any{"ref": "r", "severity": "P1", "finding": "f", "evidence": []string{}}}})); r.State != StateNullModel {
		t.Errorf("an approval over a P1: %+v", r)
	}
	if r := state(call("a", "completed", "claude-opus", nil), call("r", "completed", "gpt-x", approved)); r.State != AuditApproved || r.CandidateDigest != cand.digest() {
		t.Errorf("approved: %+v", r)
	}
}

// TestDesignAuditStillRefusesAnArchitectFamilyCritic (CRW-1452): the design audit is the critic's call now, and its independence is
// still judged by the model families of the two calls' configured pairs, never by the role: a critic on the architect's family is
// refused (independence_refused), and the same critic on another family is an approval that names the critic as the auditor.
func TestDesignAuditStillRefusesAnArchitectFamilyCritic(t *testing.T) {
	w := newWorld(t)
	design, _ := json.Marshal(designOf("A"))
	cand := Candidate{Design: design, Answers: json.RawMessage(`[]`)}
	approved := map[string]any{"verdict": "approved", "findings": []any{}, "summary": "ok"}
	audit := func(archModel, criticModel string) AuditResult {
		arch := call("c_arch", "completed", archModel, nil)
		arch.Meta.Role = "architect"
		critic := call("c_crit", "completed", criticModel, approved)
		critic.Meta.Role = "critic"
		var r AuditResult
		_ = json.Unmarshal(w.must("designAudit", "M", 1, cand, arch, critic), &r)
		return r
	}
	if r := audit("claude-opus", "claude-sonnet"); r.State != StateRefused || r.Reason != "independence_refused" || r.Reviewer.Role != "critic" {
		t.Errorf("a critic of the architect's family: %+v", r)
	}
	if r := audit("claude-opus", "gpt-6.1-sol"); r.State != AuditApproved || r.Reviewer.Role != "critic" || r.Architect.Role != "architect" {
		t.Errorf("a critic of another family: %+v", r)
	}
}

// A DESIGN an earlier binary journaled has no dispositions member (it came with CRW-1452), in the architect's output and in the
// design ledger. A run recorded then resumes on this binary and goes on: the replayed audit takes the candidate, the release of a
// node reads the ledger's DESIGN (its plan and the admission regions of the others), the lane's issuePlan and the final gate's
// decisions read it too, each with the missing member read as null. A fresh answer of the architect is still held to the strict
// schema, which requires the member.
func TestADesignRecordedWithoutDispositionsResumesAndReleases(t *testing.T) {
	old := func(d Design) json.RawMessage {
		b, _ := json.Marshal(d)
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		delete(m, "dispositions")
		b, _ = json.Marshal(m)
		return b
	}
	design := old(designOf("A", "B"))
	if strings.Contains(string(design), "dispositions") {
		t.Fatalf("the earlier DESIGN still names dispositions: %s", design)
	}
	if err := Validate(SchemaDesign, design); err == nil {
		t.Fatal("the strict schema a fresh answer is held to accepts a DESIGN without dispositions")
	}
	d, err := DecodeDesign(design)
	if err != nil {
		t.Fatalf("a DESIGN recorded without dispositions does not decode: %v", err)
	}
	if d.Dispositions != nil || len(d.Issues) != 2 {
		t.Errorf("decoded %+v, want two issues and dispositions null", d)
	}

	// the first process: the milestone, the audit of the earlier DESIGN and its revision r0 as that binary wrote them, and node A
	// released; then it stops
	w := newWorld(t)
	w.must("milestone", "plan-1", "M")
	cand := Candidate{Design: design, Answers: json.RawMessage(`[]`)}
	var audit AuditResult
	out, err := w.op("designAudit", "M", 1, cand, call("c_arch", "completed", "claude-x", nil),
		call("c_aud", "completed", "gpt-y", map[string]any{"verdict": "approved", "findings": []any{}, "summary": "ok"}))
	if err != nil {
		t.Fatalf("the replayed audit of an earlier DESIGN: %v", err)
	}
	_ = json.Unmarshal(out, &audit)
	if audit.State != AuditApproved {
		t.Fatalf("audit %+v", audit)
	}
	var anyDesign any
	_ = json.Unmarshal(design, &anyDesign)
	w.j.Append(engine.Event{RunID: "run-1", Kind: engine.KindCall, CallID: "c_ledger", Op: OpDesignLedger, Event: engine.EvCompleted,
		Data: map[string]any{"milestone": "M", "revision": FirstRevision, "digest": cand.digest(), "design": anyDesign, "answers": []any{}}})
	var rel ReleaseOutcome
	_ = json.Unmarshal(w.must("release", "plan-1", "A", "M"), &rel)
	if !rel.Released || rel.Revision != FirstRevision {
		t.Fatalf("release of A before the stop: %+v", rel)
	}

	// the resumed process: a new lane over the same journal releases B and builds the packets from the earlier DESIGN
	resumed := &world{t: t, j: w.j, relay: &stubRelay{tip: w.relay.tip}, calls: 100}
	resumed.lane = NewLane(Deps{RunID: "run-1", Journal: resumed.j, Relay: resumed.relay, Decisions: &MemoryDecisions{}, Actor: "parent", Now: func() time.Time { return time.Date(2026, 10, 12, 10, 0, 0, 0, time.UTC) }})
	out, err = resumed.op("release", "plan-1", "B", "M")
	if err != nil {
		t.Fatalf("release of B after the resume: %v", err)
	}
	_ = json.Unmarshal(out, &rel)
	if !rel.Released || rel.Revision != FirstRevision || rel.DesignDigest != cand.digest() {
		t.Errorf("release of B after the resume: %+v", rel)
	}
	if len(resumed.relay.releases) != 1 || resumed.relay.releases[0].Node != "B" {
		t.Errorf("the relay was asked %+v", resumed.relay.releases)
	}
	for _, node := range []string{"A", "B"} {
		var plan IssuePlan
		out, err := resumed.op("issuePlan", node)
		if err != nil {
			t.Fatalf("issuePlan %s after the resume: %v", node, err)
		}
		_ = json.Unmarshal(out, &plan)
		if plan.Node != node || plan.Approach != "approach "+node || len(plan.Decisions) != 1 {
			t.Errorf("issuePlan %s: %+v", node, plan)
		}
	}
	if got := gateDecisions(rows(resumed.j.Events()), "M"); len(got) != 1 || got[0].ID != "D1" || !slices.Equal(got[0].Nodes, []string{"A", "B"}) {
		t.Errorf("the final gate's decisions of the earlier DESIGN: %+v", got)
	}
}

// A lane builds its packet from the revision its node was released under: issuePlan reads the release row and the ledger row of
// that revision, whatever the ledger has grown since.
func TestIssuePlanReadsTheReleaseRevision(t *testing.T) {
	w := newWorld(t)
	digest := w.ledgered(true, designOf("A", "B"))
	w.must("release", "plan-1", "A", "M")
	// a later revision appears in the journal (the re-plan issue writes it); a node already released keeps its own
	later := designOf("A", "B")
	later.Issues[0].Approach = "approach after the re-plan"
	var anyLater any
	b, _ := json.Marshal(later)
	_ = json.Unmarshal(b, &anyLater)
	w.j.Append(engine.Event{RunID: "run-1", Kind: engine.KindCall, CallID: "c_later", Op: OpDesignLedger, Event: engine.EvCompleted, Data: map[string]any{"milestone": "M", "revision": "r1", "digest": "later", "design": anyLater}})
	var plan IssuePlan
	_ = json.Unmarshal(w.must("issuePlan", "A"), &plan)
	if plan.Revision != "r0" || plan.DesignDigest != digest || plan.Approach != "approach A" || plan.Node != "A" || plan.Issue != "CRW-A" {
		t.Errorf("plan %+v", plan)
	}
	if len(plan.Decisions) != 1 || plan.Decisions[0].ID != "D1" {
		t.Errorf("decisions %+v", plan.Decisions)
	}
	if _, err := w.op("issuePlan", "B"); err == nil {
		t.Errorf("a node that was never released has a plan")
	}
	// the input snapshot of a call of the node carries the revision and the digest
	snap, _ := w.lane.InputSnapshot(context.Background(), engine.SnapshotRequest{Node: "A"})
	if !strings.Contains(snap, "design=r0:"+digest) {
		t.Errorf("snapshot %q", snap)
	}
	if snap, _ := w.lane.InputSnapshot(context.Background(), engine.SnapshotRequest{Node: ""}); snap != "" {
		t.Errorf("snapshot of a call with no node: %q", snap)
	}
}

// A wait for a decision is open until the journal has the applied row of its own question and node.
func TestDecideReadsOnlyTheAppliedRow(t *testing.T) {
	w := newWorld(t)
	decide := func() DecideOutcome {
		var d DecideOutcome
		_ = json.Unmarshal(w.must("decide", "A", Ask{ID: "esc_1"}), &d)
		return d
	}
	if d := decide(); d.Applied {
		t.Fatalf("applied with no row")
	}
	waiting := engine.Event{RunID: "run-1", Kind: engine.KindCall, CallID: "c_w", Op: OpDecide, Event: engine.EvCompleted, Data: map[string]any{"node": "A", "ask": "esc_1", "applied": false}}
	op := w.lane.Ops()[NameDecide]
	flow := func(event string, ask, node string) {
		w.j.Append(engine.Event{RunID: "run-1", Kind: engine.KindFlow, Event: event, Data: map[string]any{"ask": ask, "node": node, "decision": map[string]any{"action": "retry"}}})
	}
	flow("decision_recorded", "esc_1", "A")
	flow(EvDecisionApplied, "esc_2", "A")
	flow(EvDecisionApplied, "esc_1", "B")
	if ok, _, _ := op.Proven(context.Background(), w.j.Events(), waiting); !ok {
		t.Errorf("an open wait stopped being open for rows that do not answer it")
	}
	if d := decide(); d.Applied {
		t.Fatalf("applied by a row of another question or node")
	}
	flow(EvDecisionApplied, "esc_1", "A")
	if ok, _, _ := op.Proven(context.Background(), w.j.Events(), waiting); ok {
		t.Errorf("the wait was replayed over an applied row")
	}
	if d := decide(); !d.Applied || d.Decision["action"] != "retry" {
		t.Errorf("decide: %+v", d)
	}
}

// The questions an asker puts to explorers are routed by the thresholds of the signals, and every route is a recorded decision.
func TestExploreRouting(t *testing.T) {
	w := newWorld(t)
	files := []string{"a", "b", "c", "d"}
	var qs []Question
	for i := 0; i < 8; i++ {
		qs = append(qs, Question{ID: fmt.Sprint(i), Question: fmt.Sprintf("question %d", i), FilesNeeded: files})
	}
	qs = append(qs, Question{ID: "few", Question: "a small one", FilesNeeded: []string{"a"}}, Question{ID: "again", Question: "Question   0", FilesNeeded: files})
	var plan RoutePlan
	_ = json.Unmarshal(w.must("exploreRoute", ExploreOpen, ExploreContext{Milestone: "M"}, qs), &plan)
	var got []string
	for _, r := range plan.Routes {
		got = append(got, r.Route)
	}
	// an open question of a DESIGN is never the asker's to answer (CRW-1347): the small one is one more question for an explorer,
	// and beyond the cap of the milestone it goes to the orchestrator like the others
	want := []string{"spawn", "spawn", "spawn", "spawn", "spawn", "spawn", "orchestrator", "orchestrator", "orchestrator", "reuse"}
	if !slices.Equal(got, want) {
		t.Errorf("routes %v, want %v", got, want)
	}
	if plan.Routes[9].SameAs != 0 || plan.Routes[9].Rule != RuleInl2 || plan.Routes[8].Rule != RuleExpl1 || plan.Routes[8].Reason != "explorer_cap" {
		t.Errorf("routes %+v", plan.Routes[8:])
	}
	// the answers are kept, and the same question asked again later costs no explorer
	w.must("exploreRecord", ExploreOpen, ExploreContext{Milestone: "M"}, []Answer{{Digest: plan.Routes[0].Digest, Question: "question 0", Answer: "zero", Route: RouteSpawn}})
	_ = json.Unmarshal(w.must("exploreRoute", ExploreResearch, ExploreContext{Node: "A", Tier: "executor"}, []Question{{Question: "question 0"}}), &plan)
	if plan.Routes[0].Route != RouteReuse || plan.Routes[0].Answer != "zero" {
		t.Errorf("a question answered in the run: %+v", plan.Routes[0])
	}
	// a node gets two research continuations on a tier; the third question is a scope question
	for i := 1; i <= 3; i++ {
		_ = json.Unmarshal(w.must("exploreRoute", ExploreResearch, ExploreContext{Node: "A", Tier: "executor"}, []Question{{Question: fmt.Sprintf("research %d", i)}}), &plan)
		want := RouteSpawn
		if i == 3 {
			want = RouteEscalate
		}
		if plan.Routes[0].Route != want {
			t.Errorf("research question %d: %s, want %s", i, plan.Routes[0].Route, want)
		}
	}
	// an evidence question: two explorers for a node, then the skip
	for i := 1; i <= 3; i++ {
		_ = json.Unmarshal(w.must("exploreRoute", ExploreEvidence, ExploreContext{Node: "A", Tier: "executor"}, []Question{{Question: fmt.Sprintf("evidence %d", i), FilesNeeded: []string{"x"}}}), &plan)
		want := RouteSpawn
		if i == 3 {
			want = RouteSkip
		}
		if plan.Routes[0].Route != want {
			t.Errorf("evidence question %d: %s, want %s", i, plan.Routes[0].Route, want)
		}
	}
	// no question at all is a recorded skip
	mem := w.lane.d.Decisions.(*MemoryDecisions)
	before := len(mem.Spawn)
	w.must("exploreRoute", ExploreEvidence, ExploreContext{Node: "B", Tier: "executor"}, []Question{})
	if len(mem.Spawn) != before+1 || mem.Spawn[before].Decision != DecisionSkip || mem.Spawn[before].RuleID != RuleExpl3 {
		t.Errorf("decisions %+v", mem.Spawn[before:])
	}

	// A research question of an executor keeps SP-INL-1: inside the asker's regions, or with at most InlineFiles files, it is the
	// asker's to answer.
	_ = json.Unmarshal(w.must("exploreRoute", ExploreResearch, ExploreContext{Node: "C", Tier: "executor", Regions: []string{"c/"}}, []Question{
		{Question: "a research question on one file", FilesNeeded: []string{"x.go"}},
		{Question: "a research question inside the regions", FilesNeeded: []string{"c/1", "c/2", "c/3", "c/4"}},
	}), &plan)
	for i, rt := range plan.Routes {
		if rt.Route != RouteInline || rt.Rule != RuleInl1 || rt.Reason != "asker_answers" {
			t.Errorf("research question %d: %+v, want SP-INL-1 inline", i, rt)
		}
	}
}

// The open questions of a DESIGN (CRW-1347): none takes the inline route, whatever its files_needed; a question whose scope starts
// with one of the four notations of the out-of-authority kinds goes to the orchestrator as a non-blocking ask and to no explorer;
// a missing, unknown or longer notation goes to an explorer; the cap of the milestone counts only explorers, and the questions
// beyond it go to the orchestrator as before. Every route is recorded with its reason, in the decision rows and in the explore
// row.
func TestExploreRoutingOpenQuestions(t *testing.T) {
	w := newWorld(t)
	qs := []Question{
		{ID: "one", Question: "is the flag documented?", Scope: "docs", FilesNeeded: []string{"README.md"}},
		{ID: "none", Question: "which package owns it?", Scope: "", FilesNeeded: []string{}},
		{ID: "rule", Question: "may the change step past the README rule?", Scope: "authority:repo-rule: the README of the store", FilesNeeded: []string{"README.md"}},
		{ID: "record", Question: "does CRW-9 keep its known-defects line?", Scope: "Authority:Other-Record", FilesNeeded: []string{}},
		{ID: "policy", Question: "does the project ship it in 1.0?", Scope: "  AUTHORITY:POLICY", FilesNeeded: []string{"a", "b", "c", "d"}},
		{ID: "env", Question: "does npm ci work where the verification runs?", Scope: "authority:environment (network)", FilesNeeded: nil},
		{ID: "unknown", Question: "who pays for it?", Scope: "authority:budget", FilesNeeded: []string{"x"}},
		{ID: "longer", Question: "is it a policy at all?", Scope: "authority:policy-ish", FilesNeeded: []string{"x"}},
		{ID: "inside", Question: "is the token in the middle?", Scope: "repo, authority:policy", FilesNeeded: []string{"x"}},
	}
	var plan RoutePlan
	_ = json.Unmarshal(w.must("exploreRoute", ExploreOpen, ExploreContext{Milestone: "M"}, qs), &plan)
	type want struct{ route, reason, authority string }
	wants := map[string]want{
		"one": {RouteSpawn, "", ""}, "none": {RouteSpawn, "", ""},
		"rule": {RouteOrchestrator, "out_of_authority", ScopeRepoRule}, "record": {RouteOrchestrator, "out_of_authority", ScopeOtherRecord},
		"policy": {RouteOrchestrator, "out_of_authority", ScopePolicy}, "env": {RouteOrchestrator, "out_of_authority", ScopeEnvironment},
		"unknown": {RouteSpawn, "", ""}, "longer": {RouteSpawn, "", ""}, "inside": {RouteSpawn, "", ""},
	}
	if len(plan.Routes) != len(qs) {
		t.Fatalf("%d routes for %d questions", len(plan.Routes), len(qs))
	}
	for i, rt := range plan.Routes {
		wt := wants[qs[i].ID]
		if rt.Route != wt.route || rt.Reason != wt.reason || rt.Authority != wt.authority || rt.Rule != RuleExpl1 || rt.Scope != qs[i].Scope {
			t.Errorf("%s: route %q reason %q authority %q rule %s scope %q, want %+v under %s", qs[i].ID, rt.Route, rt.Reason, rt.Authority, rt.Rule, rt.Scope, wt, RuleExpl1)
		}
		if rt.Route == RouteInline || rt.Rule == RuleInl1 {
			t.Errorf("%s: an open question of a DESIGN took the inline route: %+v", qs[i].ID, rt)
		}
	}
	// the decision rows: one for each question, under SP-EXPL-1, with the reason and the notation
	mem := w.lane.d.Decisions.(*MemoryDecisions)
	if len(mem.Spawn) != len(qs) {
		t.Fatalf("%d decision rows, want %d", len(mem.Spawn), len(qs))
	}
	for i, row := range mem.Spawn {
		wt := wants[qs[i].ID]
		if row.RuleID != RuleExpl1 || row.Decision != wt.route || row.Reason != wt.reason {
			t.Errorf("decision row of %s: %s %s %q, want %s %s %q", qs[i].ID, row.RuleID, row.Decision, row.Reason, RuleExpl1, wt.route, wt.reason)
		}
		inputs, _ := row.Evidence["inputs"].(map[string]any)
		if got, _ := inputs["scope_notation"].(string); got != wt.authority {
			t.Errorf("decision row of %s: scope_notation %q, want %q", qs[i].ID, got, wt.authority)
		}
	}
	// the explore row records the route and the reason of each question
	var row engine.Event
	for _, ev := range w.j.Events() {
		if ev.Op == OpExploreRoute && ev.Event == engine.EvCompleted {
			row = ev
		}
	}
	list, _ := row.Data["routes"].([]any)
	if len(list) != len(qs) {
		t.Fatalf("the explore row: %v", row.Data)
	}
	for i, item := range list {
		m, _ := item.(map[string]any)
		wt := wants[qs[i].ID]
		if str(m, "route") != wt.route || str(m, "reason") != wt.reason || str(m, "scope_notation") != wt.authority {
			t.Errorf("the explore row of %s: %v, want %+v", qs[i].ID, m, wt)
		}
	}

	// The cap of the milestone counts explorers only: five were spawned above, so one more question gets an explorer and the
	// rest go to the orchestrator for the cap; an authority question still goes for its notation and spends nothing.
	_ = json.Unmarshal(w.must("exploreRoute", ExploreOpen, ExploreContext{Milestone: "M"}, []Question{
		{ID: "p2", Question: "is the policy written down?", Scope: "authority:policy", FilesNeeded: []string{}},
		{ID: "a", Question: "question a", Scope: "repo", FilesNeeded: []string{"a"}},
		{ID: "b", Question: "question b", Scope: "repo", FilesNeeded: []string{"a"}},
		{ID: "c", Question: "question c", Scope: "repo", FilesNeeded: []string{"a", "b", "c", "d", "e"}},
	}), &plan)
	var got []string
	for _, rt := range plan.Routes {
		got = append(got, rt.Route+"/"+rt.Reason)
	}
	if !slices.Equal(got, []string{"orchestrator/out_of_authority", "spawn/", "orchestrator/explorer_cap", "orchestrator/explorer_cap"}) {
		t.Errorf("routes after the cap: %v", got)
	}

	// A question answered earlier in the run still reuses its answer, an authority question among them; an authority question
	// recorded with no answer (a repeat in its round) is no answer, and goes to the orchestrator again.
	w.must("exploreRecord", ExploreOpen, ExploreContext{Milestone: "M"}, []Answer{
		{Digest: QuestionDigest("is the flag documented?"), Question: "is the flag documented?", Answer: "yes, in README.md", Route: RouteSpawn},
		{Digest: QuestionDigest("does the project ship it in 1.0?"), Question: "does the project ship it in 1.0?", Answer: "", Route: RouteReuse},
	})
	_ = json.Unmarshal(w.must("exploreRoute", ExploreOpen, ExploreContext{Milestone: "M2"}, []Question{
		{ID: "again", Question: "Is the flag   documented?", Scope: "authority:policy", FilesNeeded: []string{"README.md"}},
		{ID: "ship", Question: "does the project ship it in 1.0?", Scope: "authority:policy", FilesNeeded: []string{}},
	}), &plan)
	if rt := plan.Routes[0]; rt.Route != RouteReuse || rt.Rule != RuleInl2 || rt.Answer != "yes, in README.md" || rt.Reason != "answered_in_run" {
		t.Errorf("a question answered in the run: %+v", rt)
	}
	if rt := plan.Routes[1]; rt.Route != RouteOrchestrator || rt.Reason != "out_of_authority" {
		t.Errorf("an authority question recorded with no answer: %+v", rt)
	}
}

// The background questions of a milestone (CRW-1466): one per node, put to an explorer before the architect. Each spawns an explorer
// or reuses an answered digest, under SP-EXPL-1 with the reason spawned_background; none takes the inline route whatever its
// files_needed, none goes to the orchestrator, and none counts against the open questions' cap of the milestone: six open questions
// spawn and the seventh goes to the orchestrator (explorer_cap) whether background questions were routed before or after them.
func TestExploreRoutingBackgroundDoesNotUseTheQuestionCap(t *testing.T) {
	w := newWorld(t)
	bg := func(node string, files []string) Question {
		return Question{ID: "bg-" + node, Question: fmt.Sprintf("background of node %s (CRW-%s): the title of %s", node, node, node), FilesNeeded: files}
	}
	ctx := ExploreContext{Milestone: "M"}
	var plan RoutePlan
	checkBackground := func(when string, want []string) {
		t.Helper()
		var got []string
		for _, rt := range plan.Routes {
			got = append(got, rt.Route+"/"+rt.Reason)
			if rt.Rule != RuleExpl1 && rt.Route == RouteSpawn {
				t.Errorf("%s: background question %s spawned under %s, want %s", when, rt.ID, rt.Rule, RuleExpl1)
			}
			if rt.Authority != "" {
				t.Errorf("%s: background question %s has the scope notation %q", when, rt.ID, rt.Authority)
			}
		}
		if !slices.Equal(got, want) {
			t.Errorf("%s: background routes %v, want %v", when, got, want)
		}
	}
	// three background questions before any open question: all spawn, the one with a single file named too (never inline)
	_ = json.Unmarshal(w.must("exploreRoute", ExploreBackground, ctx, []Question{bg("A", []string{"a.go"}), bg("B", nil), bg("C", []string{})}), &plan)
	if plan.Kind != ExploreBackground {
		t.Errorf("the plan's kind %q, want %q", plan.Kind, ExploreBackground)
	}
	checkBackground("before the open questions", []string{"spawn/spawned_background", "spawn/spawned_background", "spawn/spawned_background"})

	// seven open questions: six spawn, the seventh goes to the orchestrator for the cap; the background explorers spent none of it
	var qs []Question
	for i := 0; i < 7; i++ {
		qs = append(qs, Question{ID: fmt.Sprint(i), Question: fmt.Sprintf("open question %d", i), FilesNeeded: []string{}})
	}
	_ = json.Unmarshal(w.must("exploreRoute", ExploreOpen, ctx, qs), &plan)
	var got []string
	for _, rt := range plan.Routes {
		got = append(got, rt.Route+"/"+rt.Reason)
	}
	want := []string{"spawn/", "spawn/", "spawn/", "spawn/", "spawn/", "spawn/", "orchestrator/explorer_cap"}
	if !slices.Equal(got, want) {
		t.Errorf("open routes after background ones %v, want %v", got, want)
	}

	// three more background questions after the cap is used: all still spawn, the one with files inside no region too
	_ = json.Unmarshal(w.must("exploreRoute", ExploreBackground, ctx, []Question{bg("D", []string{"d/1"}), bg("E", nil), bg("F", []string{"f.go", "g.go"})}), &plan)
	checkBackground("after the cap", []string{"spawn/spawned_background", "spawn/spawned_background", "spawn/spawned_background"})

	// and the open-question count is unchanged by them: the next open question of the milestone still goes for the cap, and in
	// another milestone an open question still spawns
	_ = json.Unmarshal(w.must("exploreRoute", ExploreOpen, ctx, []Question{{ID: "7", Question: "open question 7", FilesNeeded: []string{}}}), &plan)
	if rt := plan.Routes[0]; rt.Route != RouteOrchestrator || rt.Reason != "explorer_cap" {
		t.Errorf("an open question after the background ones: %+v, want orchestrator/explorer_cap", rt)
	}
	_ = json.Unmarshal(w.must("exploreRoute", ExploreOpen, ExploreContext{Milestone: "M2"}, []Question{{ID: "x", Question: "open question x", FilesNeeded: []string{}}}), &plan)
	if rt := plan.Routes[0]; rt.Route != RouteSpawn {
		t.Errorf("an open question of another milestone: %+v, want spawn", rt)
	}

	// the decision rows of the background questions: SP-EXPL-1 spawns with the reason and the kind in the inputs; a background route
	// reads no cap, so its spawned_in_milestone stays 0 before and after the six open explorers
	mem := w.lane.d.Decisions.(*MemoryDecisions)
	var counts []int
	for _, row := range mem.Spawn {
		inputs, _ := row.Evidence["inputs"].(map[string]any)
		if inputs["kind"] != ExploreBackground {
			continue
		}
		if row.RuleID != RuleExpl1 || row.Decision != RouteSpawn || row.Reason != "spawned_background" {
			t.Errorf("a background decision row: %s %s %q, want %s spawn spawned_background", row.RuleID, row.Decision, row.Reason, RuleExpl1)
		}
		n, _ := inputs["spawned_in_milestone"].(int)
		counts = append(counts, n)
	}
	if !slices.Equal(counts, []int{0, 0, 0, 0, 0, 0}) {
		t.Errorf("spawned_in_milestone of the background rows %v, want six zeros", counts)
	}

	// a background question answered in the run reuses the answer, and a repeat in its round reuses the first
	w.must("exploreRecord", ExploreBackground, ctx, []Answer{{Digest: QuestionDigest(bg("A", nil).Question), ID: "bg-A", Question: bg("A", nil).Question, Answer: "A reads a.go", Route: RouteSpawn}})
	_ = json.Unmarshal(w.must("exploreRoute", ExploreBackground, ctx, []Question{bg("A", []string{"a.go"}), bg("G", nil), bg("G", nil)}), &plan)
	if rt := plan.Routes[0]; rt.Route != RouteReuse || rt.Rule != RuleInl2 || rt.Answer != "A reads a.go" || rt.Reason != "answered_in_run" || rt.Origin != RouteSpawn {
		t.Errorf("an answered background question: %+v", rt)
	}
	if rt := plan.Routes[1]; rt.Route != RouteSpawn || rt.Reason != "spawned_background" {
		t.Errorf("a new background question: %+v", rt)
	}
	if rt := plan.Routes[2]; rt.Route != RouteReuse || rt.SameAs != 1 || rt.Reason != "same_question_in_round" {
		t.Errorf("a repeated background question: %+v", rt)
	}
	// a background route carries nothing to the orchestrator
	if len(plan.Carried) != 0 {
		t.Errorf("a background route carried %+v", plan.Carried)
	}
}

// CRW-1366: the router remembers the questions the run asked of the orchestrator by their digest, read from the design_question
// escalation rows (their evidence names the digests), and never rewrites a row: an asked question is not asked again until the
// orchestrator's applied answer is there, and then that answer is reused. Every reuse names the route its answer came from.
func TestExploreRoutingRemembersOrchestratorAsks(t *testing.T) {
	w := newWorld(t)
	policy := Question{ID: "policy", Question: "does the project ship it in 1.0?", Scope: "authority:policy", FilesNeeded: []string{}}
	docs := Question{ID: "docs", Question: "is the flag documented?", Scope: "docs", FilesNeeded: []string{"README.md"}}
	dup := policy
	dup.ID = "dup"
	type want struct{ route, reason, origin, answer string }
	check := func(round string, plan RoutePlan, wants ...want) {
		t.Helper()
		if len(plan.Routes) != len(wants) {
			t.Fatalf("%s: %d routes, want %d", round, len(plan.Routes), len(wants))
		}
		for i, rt := range plan.Routes {
			if got := (want{rt.Route, rt.Reason, rt.Origin, rt.Answer}); got != wants[i] {
				t.Errorf("%s: question %s routed %+v, want %+v", round, rt.ID, got, wants[i])
			}
		}
	}
	var plan RoutePlan
	_ = json.Unmarshal(w.must("exploreRoute", ExploreOpen, ExploreContext{Milestone: "M"}, []Question{policy, dup, docs}), &plan)
	check("round 1", plan, want{RouteOrchestrator, "out_of_authority", "", ""}, want{RouteReuse, "same_question_in_round", RouteOrchestrator, ""}, want{RouteSpawn, "", "", ""})
	w.must("exploreRecord", ExploreOpen, ExploreContext{Milestone: "M"}, []Answer{
		{Digest: plan.Routes[0].Digest, Question: policy.Question, Route: RouteOrchestrator, Reason: "out_of_authority"},
		{Digest: plan.Routes[1].Digest, Question: policy.Question, Route: RouteReuse, Reason: "same_question_in_round", Origin: RouteOrchestrator},
		{Digest: plan.Routes[2].Digest, Question: docs.Question, Answer: "yes, in README.md", Route: RouteSpawn},
	})
	esc := w.must("escalate", nil, EscalateInfo{Reason: "design_question", Detail: "[authority:policy] " + policy.Question, Ask: "design",
		Evidence: map[string]any{"questions": []string{QuestionDigest(policy.Question)}}})
	var out EscalateOutcome
	_ = json.Unmarshal(esc, &out)
	written := len(w.j.Events())
	before, _ := json.Marshal(w.j.Events())

	// the next round: the asked question is not asked again, the explorer's answer is reused with its route
	_ = json.Unmarshal(w.must("exploreRoute", ExploreOpen, ExploreContext{Milestone: "M"}, []Question{policy, docs}), &plan)
	check("round 2", plan, want{RouteOrchestrator, "asked_in_run", "", ""}, want{RouteReuse, "answered_in_run", RouteSpawn, "yes, in README.md"})
	if plan.Routes[0].Authority != ScopePolicy {
		t.Errorf("the asked question lost its notation: %+v", plan.Routes[0])
	}
	// an applied answer of another ask is no answer of this one
	w.j.Append(engine.Event{RunID: "run-1", Kind: engine.KindFlow, Event: EvDecisionApplied, Data: map[string]any{"ask": "esc_other", "node": "", "decision": map[string]any{"text": "not this"}}})
	_ = json.Unmarshal(w.must("exploreRoute", ExploreOpen, ExploreContext{Milestone: "M"}, []Question{policy}), &plan)
	check("round 3", plan, want{RouteOrchestrator, "asked_in_run", "", ""})
	// the orchestrator's applied answer is the answer of the question from then on
	w.j.Append(engine.Event{RunID: "run-1", Kind: engine.KindFlow, Event: EvDecisionApplied, Data: map[string]any{"ask": out.ID, "node": "", "decision": map[string]any{"text": "1.0 ships without it"}}})
	_ = json.Unmarshal(w.must("exploreRoute", ExploreOpen, ExploreContext{Milestone: "M"}, []Question{policy, dup}), &plan)
	check("round 4", plan, want{RouteReuse, "answered_by_orchestrator", RouteOrchestrator, "1.0 ships without it"}, want{RouteReuse, "same_question_in_round", RouteOrchestrator, ""})
	if plan.Routes[0].Rule != RuleInl2 {
		t.Errorf("the orchestrator's answer reused under %s", plan.Routes[0].Rule)
	}
	// the rows the router read are as they were written
	if after, _ := json.Marshal(w.j.Events()[:written]); string(after) != string(before) {
		t.Errorf("the router rewrote a row it read")
	}
	// the explore row and the decision rows record the route the answer came from
	row := w.j.Events()[len(w.j.Events())-1]
	list, _ := row.Data["routes"].([]any)
	if m, _ := list[0].(map[string]any); str(m, "origin") != RouteOrchestrator || str(m, "reason") != "answered_by_orchestrator" {
		t.Errorf("the explore row %v", row.Data)
	}
	mem := w.lane.d.Decisions.(*MemoryDecisions)
	last := mem.Spawn[len(mem.Spawn)-2]
	if inputs, _ := last.Evidence["inputs"].(map[string]any); last.Decision != RouteReuse || inputs["origin"] != RouteOrchestrator {
		t.Errorf("the decision row of the reused answer: %+v", last)
	}
}

// A re-plan pins a head (CRW-1458), and an explorer's answer reads that head: an open question asked again at a re-plan's head reuses
// only an answer an explorer gave at that head, so a repeat of a question of the first DESIGN (no head) or of a re-plan at another
// head spawns an explorer again; two asks of it in one round are one question. What goes to the orchestrator keeps the digest of the
// text alone, since the owner's answer reads no head: a question the run asked is asked_in_run and, once answered, reuses the answer
// at any head, and a question beyond the cap is asked under that digest. The explorers a re-plan spawns count against the cap of the
// milestone. A question with no head keeps the digest and the routes it had.
func TestExploreRoutingReplanHeadIsPartOfTheAnswer(t *testing.T) {
	w := newWorld(t)
	docs := Question{ID: "docs", Question: "where does the flag live?", Scope: "repo", FilesNeeded: []string{"a.go"}}
	again := docs
	again.ID = "again"
	policy := Question{ID: "policy", Question: "does the project ship it in 1.0?", Scope: "authority:policy", FilesNeeded: []string{}}
	base := QuestionDigest(docs.Question)
	at := func(head string) ExploreContext { return ExploreContext{Milestone: "M", Head: head} }
	route := func(ec ExploreContext, qs ...Question) RoutePlan {
		t.Helper()
		var plan RoutePlan
		_ = json.Unmarshal(w.must("exploreRoute", ExploreOpen, ec, qs), &plan)
		return plan
	}
	record := func(ec ExploreContext, rt Route, answer string) {
		t.Helper()
		w.must("exploreRecord", ExploreOpen, ec, []Answer{{Digest: rt.Digest, Question: rt.Question, Answer: answer, Route: RouteSpawn}})
	}

	// the first DESIGN: no head, the digest of the text
	plan := route(ExploreContext{Milestone: "M"}, docs, policy)
	if rt := plan.Routes[0]; rt.Route != RouteSpawn || rt.Digest != base {
		t.Fatalf("the first DESIGN's question: %+v", rt)
	}
	record(ExploreContext{Milestone: "M"}, plan.Routes[0], "a.go at the base")
	esc := w.must("escalate", nil, EscalateInfo{Reason: "design_question", Detail: policy.Question, Ask: "design", Evidence: map[string]any{"questions": []string{QuestionDigest(policy.Question)}}})
	var asked EscalateOutcome
	_ = json.Unmarshal(esc, &asked)

	// the re-plan at h1: the question is asked again of an explorer, under a digest of the text and the head; its repeat in the round
	// is the same question; the asked question keeps its digest and is not asked again, and nothing is carried
	plan = route(at("h1"), docs, again, policy)
	h1 := plan.Routes[0].Digest
	if rt := plan.Routes[0]; rt.Route != RouteSpawn || h1 == base {
		t.Errorf("the re-plan at h1 reused the answer of the first DESIGN: %+v", rt)
	}
	if rt := plan.Routes[1]; rt.Route != RouteReuse || rt.Reason != "same_question_in_round" || rt.SameAs != 0 || rt.Digest != h1 {
		t.Errorf("the repeat in the round: %+v", rt)
	}
	if rt := plan.Routes[2]; rt.Route != RouteOrchestrator || rt.Reason != "asked_in_run" || rt.Digest != QuestionDigest(policy.Question) {
		t.Errorf("the asked question at h1: %+v", rt)
	}
	if len(plan.Carried) != 0 {
		t.Errorf("carried %+v: the asked question is raised again under its own digest", plan.Carried)
	}
	record(at("h1"), plan.Routes[0], "a.go and b.go at h1")

	// the next round at h1 reuses the answer made at h1; a re-plan at h2 asks again; the first DESIGN's head keeps its answer
	if rt := route(at("h1"), docs).Routes[0]; rt.Route != RouteReuse || rt.Answer != "a.go and b.go at h1" || rt.Digest != h1 {
		t.Errorf("the next round at h1: %+v", rt)
	}
	if rt := route(at("h2"), docs).Routes[0]; rt.Route != RouteSpawn || rt.Digest == h1 || rt.Digest == base {
		t.Errorf("the re-plan at h2: %+v", rt)
	}
	if rt := route(ExploreContext{Milestone: "M"}, docs).Routes[0]; rt.Route != RouteReuse || rt.Answer != "a.go at the base" || rt.Digest != base {
		t.Errorf("a question with no head: %+v", rt)
	}

	// the orchestrator's answer reads no head: once applied it is the answer at any head
	w.j.Append(engine.Event{RunID: "run-1", Kind: engine.KindFlow, Event: EvDecisionApplied, Data: map[string]any{"ask": asked.ID, "node": "", "decision": map[string]any{"text": "1.0 ships without it"}}})
	if rt := route(at("h3"), policy).Routes[0]; rt.Route != RouteReuse || rt.Reason != "answered_by_orchestrator" || rt.Answer != "1.0 ships without it" || rt.Digest != QuestionDigest(policy.Question) {
		t.Errorf("the answered question at h3: %+v", rt)
	}

	// three explorers so far (the base, h1, h2): three more fill the cap of six, and the next question goes to the orchestrator
	// under the digest of its text
	plan = route(at("h3"), Question{Question: "q4", Scope: "repo"}, Question{Question: "q5", Scope: "repo"}, Question{Question: "q6", Scope: "repo"}, Question{Question: "q7", Scope: "repo"})
	var got []string
	for _, rt := range plan.Routes {
		got = append(got, rt.Route+"/"+rt.Reason)
	}
	if !slices.Equal(got, []string{"spawn/", "spawn/", "spawn/", "orchestrator/explorer_cap"}) {
		t.Errorf("routes at the cap: %v", got)
	}
	if rt := plan.Routes[3]; rt.Digest != QuestionDigest("q7") {
		t.Errorf("a question beyond the cap at a head is asked under %s, want the digest of its text %s", rt.Digest, QuestionDigest("q7"))
	}
}

// AuthorityScope reads the notation of an out-of-authority kind at the start of a scope, case-insensitively and only as a whole
// token; anything else is no notation.
func TestAuthorityScope(t *testing.T) {
	for scope, want := range map[string]string{
		"authority:repo-rule": ScopeRepoRule, "AUTHORITY:OTHER-RECORD: CRW-9": ScopeOtherRecord, " authority:policy;": ScopePolicy,
		"authority:environment\tnpm": ScopeEnvironment, "authority:environment-x": "", "authority:": "", "": "", "policy": "",
		"repo authority:policy": "", "authority:policy_x": "", "authority:policy.": ScopePolicy,
	} {
		if got := AuthorityScope(scope); got != want {
			t.Errorf("AuthorityScope(%q) = %q, want %q", scope, got, want)
		}
	}
	if !slices.Equal(AuthorityScopes, []string{ScopeRepoRule, ScopeOtherRecord, ScopePolicy, ScopeEnvironment}) {
		t.Errorf("AuthorityScopes %v", AuthorityScopes)
	}
}

// The premerge record is assembled from the verdict by code: a verified verdict passes the judgment, and a finding that needs a
// disposition is blocking - only the orchestrator separates or carries one.
func TestAssemblePremergeDefaultsToBlocking(t *testing.T) {
	in := PremergeInput{Issue: "CRW-1", Node: "A", Head: strings.Repeat("a", 40), Dev: strings.Repeat("b", 40), CriteriaDigest: strings.Repeat("c", 64),
		Grader: premerge.Grader{Model: "gpt-x", Effort: "high", PromptDigest: "pd"}, GradedAt: time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC), By: "parent",
		Verdict: ReviewVerdict{Verdict: "verified", Criteria: []Criterion{{ID: "c1", Met: true, Evidence: "read"}}, Defects: []Defect{}, Summary: "fine"}}
	rec, err := AssemblePremerge(in)
	if err != nil {
		t.Fatal(err)
	}
	if _, reason, detail, err := PremergeVerdict(rec); err != nil || reason != "" {
		t.Fatalf("a verified verdict: %q %q %v", reason, detail, err)
	}
	in.Verdict.Defects = []Defect{{Ref: "d1", Kind: "maintainability", Severity: "P3", Impact: "cosmetic", Finding: "naming"}}
	rec, _ = AssemblePremerge(in)
	if len(rec.Dispositions.Items) != 1 || rec.Dispositions.Items[0].Class != "blocking" || rec.Dispositions.Items[0].Ref != "d1" {
		t.Fatalf("dispositions %+v", rec.Dispositions)
	}
	if _, reason, _, _ := PremergeVerdict(rec); reason != "premerge_blocked" {
		t.Errorf("a finding that needs a disposition: %q", reason)
	}
	in.Verdict.Defects = []Defect{{Ref: "d2", Kind: "maintainability", Severity: "P3", Impact: "minor_separable", Finding: "naming"}}
	rec, _ = AssemblePremerge(in)
	if _, reason, _, _ := PremergeVerdict(rec); reason != "" || len(rec.Dispositions.Items) != 0 {
		t.Errorf("a minor_separable P3 needs no disposition: %q %+v", reason, rec.Dispositions)
	}
	in.Verdict.Criteria = nil
	if _, err := AssemblePremerge(in); err == nil {
		t.Errorf("a verdict with no criteria made a record")
	}
	if got := ReasonOf("crw.accept: refused:premerge_blocked: x is blocking"); got != "premerge_blocked" {
		t.Errorf("ReasonOf = %q", got)
	}
}

// The decoders hold the rules the JSON Schema cannot say.
func TestDecodersHoldTheRulesOfTheSchemas(t *testing.T) {
	good := func(kind, severity string, met bool, verdict string) []byte {
		d := map[string]any{"ref": "d", "kind": kind, "severity": severity, "introduced": false, "impact": "minor_separable", "inPromise": false, "file": "f", "line": 1, "finding": "x", "evidence_question": nil}
		if kind == "evidence_missing" {
			d["evidence_question"] = map[string]any{"question": "q?", "files_needed": []string{}}
		}
		b, _ := json.Marshal(map[string]any{"verdict": verdict, "criteria": []any{map[string]any{"id": "c", "met": met, "evidence": "e"}}, "defects": []any{d}, "summary": "s"})
		return b
	}
	if _, err := DecodeVerdict(good("evidence_missing", "P2", false, "needs_changes")); err != nil {
		t.Errorf("a valid evidence_missing verdict: %v", err)
	}
	if _, err := DecodeVerdict(good("correctness", "P2", true, "verified")); err == nil {
		t.Errorf("verified over a P2 defect")
	}
	if _, err := DecodeVerdict(good("correctness", "P3", false, "verified")); err == nil {
		t.Errorf("verified over a criterion that is not met")
	}
	if _, err := DecodeReceipt([]byte(`{"outcome":"needs_research","files":[],"verification":[],"summary":"s","gap":null,"research_question":null,"plan_mismatch":null}`)); err == nil {
		t.Errorf("needs_research without its question")
	}
	if _, err := DecodeDesignAudit([]byte(`{"verdict":"needs_changes","findings":[],"summary":"s"}`)); err == nil {
		t.Errorf("needs_changes without a finding")
	}
	if _, err := DecodeReflection([]byte(`{"verdict":"MISALIGNED","misaligned":[]}`)); err == nil {
		t.Errorf("MISALIGNED without a decision")
	}
	if err := Validate(SchemaVerdict, []byte(`{"verdict":"verified"} {}`)); err == nil {
		t.Errorf("two documents validated")
	}
	// two reviewers that find the same thing in other words and spacing find one finding
	a := Defect{Kind: "correctness", File: "f.go", Line: 3, Finding: "The  guard is MISSING"}
	b := Defect{Kind: "correctness", File: "f.go", Line: 3, Finding: " the guard is missing "}
	c := Defect{Kind: "correctness", File: "f.go", Line: 4, Finding: "the guard is missing"}
	if FindingDigest(a) != FindingDigest(b) || FindingDigest(a) == FindingDigest(c) {
		t.Errorf("finding digests %s %s %s", FindingDigest(a), FindingDigest(b), FindingDigest(c))
	}
}

// crw.verify records a passing or failing verification with the digest of its record, and a verification that could not run is
// not a failed one.
func TestVerifyRecordsTheOutcome(t *testing.T) {
	w := newWorld(t)
	head := strings.Repeat("a", 40)
	var ver fakeVerifier
	w.lane.d.Verifier = &ver
	var res VerifyResult
	ver.out = VerifyOutcome{Pass: true, Command: CILocalCommand, Digest: "sha256:ab", Record: json.RawMessage(`{"schema":"verification-record/1"}`)}
	_ = json.Unmarshal(w.must("verify", "A", head), &res)
	if !res.Ran || !res.Pass || res.Digest != "sha256:ab" || res.Error != "" {
		t.Errorf("pass: %+v", res)
	}
	row := w.j.Events()[len(w.j.Events())-1]
	if v := VerifyProof(w.j.Events(), "A", head, CILocalCommand); !v.Proven || row.Data["record"] == nil {
		t.Errorf("proof %+v, row %v", v, row.Data)
	}
	ver.out = VerifyOutcome{Pass: false, Command: CILocalCommand, Digest: "sha256:cd", Failures: []string{"validate/tests: failed"}}
	_ = json.Unmarshal(w.must("verify", "A", map[string]any{"commit": strings.Repeat("b", 40)}), &res)
	if !res.Ran || res.Pass || len(res.Failures) != 1 {
		t.Errorf("fail: %+v", res)
	}
	ver.err = fmt.Errorf("crw-dev is not installed")
	_ = json.Unmarshal(w.must("verify", "A", head), &res)
	if res.Ran || res.Pass || !strings.Contains(res.Error, "not installed") {
		t.Errorf("not run: %+v", res)
	}
}

type fakeVerifier struct {
	out VerifyOutcome
	err error
}

func (f *fakeVerifier) Verify(context.Context, VerifyRequest) (VerifyOutcome, error) {
	return f.out, f.err
}

// CILocalVerifier runs crw-dev ci local and reads the record it leaves: the record, not the exit code alone, is the verification.
func TestCILocalVerifier(t *testing.T) {
	dir := t.TempDir()
	run := func(code int, record string, ok bool) RunCommand {
		return func(_ context.Context, wt string, _ []string, name string, args ...string) (int, []byte, error) {
			if wt != "/wt" || name != "crw-dev" || !slices.Equal(args[:2], []string{"ci", "local"}) {
				t.Errorf("ran %s %v in %s", name, args, wt)
			}
			if record != "" {
				path := args[slices.Index(args, "--record")+1]
				if err := os.WriteFile(path, []byte(record), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			return code, []byte("output"), nil
		}
	}
	v := func(r RunCommand) CILocalVerifier {
		return CILocalVerifier{Run: r, RecordDir: filepath.Join(dir, "records")}
	}
	req := VerifyRequest{Node: "A", Head: strings.Repeat("a", 40), Worktree: "/wt"}
	pass := `{"schema":"verification-record/1","result":"pass","digest":"sha256:ff","jobs":[{"name":"validate","result":"pass","steps":[{"name":"tests","result":"passed"}]}]}`
	if out, err := v(run(0, pass, true)).Verify(context.Background(), req); err != nil || !out.Pass || out.Digest != "sha256:ff" || out.Command != CILocalCommand || len(out.Record) == 0 {
		t.Errorf("pass: %+v %v", out, err)
	}
	failed := `{"schema":"verification-record/1","result":"fail","digest":"sha256:ee","jobs":[{"name":"validate","result":"fail","steps":[{"name":"tests","result":"failed"},{"name":"lint","result":"passed"}]}]}`
	out, err := v(run(1, failed, true)).Verify(context.Background(), req)
	if err != nil || out.Pass || !slices.Equal(out.Failures, []string{"validate/tests: failed"}) {
		t.Errorf("fail: %+v %v", out, err)
	}
	// exit 0 with a record that says fail, and a record without a digest, are not a pass
	if out, err := v(run(0, failed, true)).Verify(context.Background(), req); err != nil || out.Pass {
		t.Errorf("exit 0 over a failed record: %+v %v", out, err)
	}
	if _, err := v(run(0, `{"schema":"verification-record/1","result":"pass"}`, true)).Verify(context.Background(), req); err == nil {
		t.Errorf("a record without a digest was taken")
	}
	// no record at all: the verification did not run
	if _, err := v(run(2, "", false)).Verify(context.Background(), req); err == nil || !strings.Contains(err.Error(), "left no record") {
		t.Errorf("no record: %v", err)
	}
}

// The decision rows are appended once: the same id again is the same decision, in memory and in the relay store.
func TestDecisionsAreAppendedOnce(t *testing.T) {
	mem := &MemoryDecisions{}
	row := SpawnRow{DecisionID: "sd_1", RunID: "run-1", Kind: KindStructural, RuleID: RuleExec1, Decision: DecisionSpawn, NodeID: "A", Evidence: map[string]any{}, DecidedAt: time.Now()}
	for i := 0; i < 2; i++ {
		_ = mem.AppendSpawn(context.Background(), row)
		_ = mem.AppendStep(context.Background(), StepRow{StepID: "st_1"})
	}
	if len(mem.Spawn) != 1 || len(mem.Step) != 1 {
		t.Errorf("memory: %d spawn, %d step", len(mem.Spawn), len(mem.Step))
	}
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "relay.sqlite3"), "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	runRow(t, st)
	db := StoreDecisions{DB: st.DB}
	row.RunID = nRun
	step := StepRow{StepID: "st_1", RunID: nRun, NodeID: "A", FromTier: "executor", ToTier: "executor-t1", RuleID: RuleTier1, Evidence: map[string]any{"letter": "a"}, CheckpointHead: "h", CheckpointTree: "t", DecidedAt: time.Now()}
	for i := 0; i < 2; i++ {
		if err := db.AppendSpawn(context.Background(), row); err != nil {
			t.Fatal(err)
		}
		if err := db.AppendStep(context.Background(), step); err != nil {
			t.Fatal(err)
		}
	}
	var spawns, steps int
	_ = st.DB.QueryRow(`SELECT COUNT(*) FROM flow_spawn_decisions`).Scan(&spawns)
	_ = st.DB.QueryRow(`SELECT COUNT(*) FROM flow_step_decisions WHERE rule_id = ? AND checkpoint_head = 'h'`, RuleTier1).Scan(&steps)
	if spawns != 1 || steps != 1 {
		t.Errorf("store: %d spawn rows, %d step rows", spawns, steps)
	}
}

// The tier evidence of crw:lib/evidence follows SP-TIER-1: the rules a, b, c and d, and what never counts.
func TestEvidenceTrackerFollowsSPTier1(t *testing.T) {
	body := `import { evidence } from 'crw:lib/evidence'
export const meta = { name: 'e' }
const out = {}
{ const ev = evidence('A')
  ev.review('executor', 'r1', 'needs_changes', ['x', 'y'])
  out.oneRound = ev.due('executor')
  ev.review('executor', 'r2', 'needs_changes', ['z'])
  out.differentFindings = ev.due('executor')
  ev.review('executor', 'r3', 'needs_changes', ['z'])
  out.repeated = ev.due('executor') }
{ const ev = evidence('A')
  ev.review('executor', 'r1', 'needs_changes', ['x'])
  ev.verification('executor', 'h1', false, 'd1')
  ev.review('executor', 'r2', 'needs_changes', ['x'])
  out.repeatedAcrossAFailure = ev.due('executor') }
{ const ev = evidence('A')
  ev.verification('executor', 'h1', false, 'd1')
  out.oneFailure = ev.due('executor')
  ev.verification('executor', 'h2', true, 'd2')
  ev.verification('executor', 'h3', false, 'd3')
  out.resetByAPass = ev.due('executor')
  ev.verification('executor', 'h4', false, 'd4')
  out.twoFailures = ev.due('executor') }
{ const ev = evidence('A')
  ev.nullModel('executor', 'c1')
  out.oneNull = ev.due('executor')
  ev.executor('executor', 'c2', 'research')
  ev.nullModel('executor', 'c3')
  out.resetByAnotherCall = ev.due('executor')
  ev.nullModel('executor', 'c4')
  out.twoNulls = ev.due('executor')
  out.otherTier = ev.due('executor-t1') }
{ const ev = evidence('A')
  for (const i of [1, 2, 3, 4]) ev.review('executor', 'r' + i, 'needs_changes', ['f' + i])
  out.fourRounds = ev.due('executor')
  out.evidence = ev.evidence('executor') }
return out`
	prog, err := script.Load(script.Source{Name: "e.js", Text: []byte(body)})
	if err != nil {
		t.Fatal(err)
	}
	res, err := engine.Run(context.Background(), prog, engine.Config{Backend: engine.NewFake(), Policy: engine.FakePolicy()})
	if err != nil || res.Status != engine.RunCompleted {
		t.Fatalf("%v %+v", err, res)
	}
	var got map[string]any
	_ = json.Unmarshal(res.Value, &got)
	want := map[string]any{"oneRound": nil, "differentFindings": nil, "repeated": "a", "repeatedAcrossAFailure": "a", "oneFailure": nil, "resetByAPass": nil, "twoFailures": "b",
		"oneNull": nil, "resetByAnotherCall": nil, "twoNulls": "c", "otherTier": nil, "fourRounds": "d"}
	for k, w := range want {
		if got[k] != w {
			t.Errorf("%s = %v, want %v", k, got[k], w)
		}
	}
	ev, _ := got["evidence"].(map[string]any)
	if ev["letter"] != "d" || ev["rounds"] != float64(4) || len(ev["reviewer_calls"].([]any)) != 4 {
		t.Errorf("evidence %v", ev)
	}
}

// The reasons the lane closes an execution and holds a node with are the closed reasons of AgentClose (R07): the node given up
// returns its slot, a recovery hold closes nothing.
func TestCloseReasonsAreTheOnesOfAgentClose(t *testing.T) {
	want := []string{dagsched.AgentEscalationTiersExhausted, dagsched.AgentEscalationBlockedScope, dagsched.AgentEscalationSizeSplit}
	var got []string
	for r := range closingReasons {
		got = append(got, r)
	}
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("closing reasons %v, AgentClose gives up a node for %v", got, want)
	}
	if holdDispatchUnknown != dagsched.AgentHoldDispatchUnknown || holdHostFailure != dagsched.AgentHoldHostFailure {
		t.Errorf("hold reasons %q %q, AgentClose holds for %q %q", holdDispatchUnknown, holdHostFailure, dagsched.AgentHoldDispatchUnknown, dagsched.AgentHoldHostFailure)
	}
}

// ---- the correction round after the evaluation of bb50b215 ----

// d2: a verification that never ran is not an effect to replay: the row is not proven, so a resume asks the verifier again.
func TestVerifyRowThatDidNotRunIsNotProven(t *testing.T) {
	w := newWorld(t)
	op := w.lane.Ops()[NameVerify]
	for _, tc := range []struct {
		ran  bool
		want bool
	}{{false, false}, {true, true}} {
		ok, why, err := op.Proven(context.Background(), nil, engine.Event{Data: map[string]any{"node": "A", "head": "h", "ran": tc.ran, "ok": false}})
		if err != nil || ok != tc.want {
			t.Errorf("ran=%v: proven %v (%s) %v, want %v", tc.ran, ok, why, err, tc.want)
		}
	}
}

// d4: the release row of a node sized for a child or a split is a decision that stands, not a release that did not happen.
func TestChildAndSplitReleaseRowsStayProven(t *testing.T) {
	w := newWorld(t)
	op := w.lane.Ops()[NameRelease]
	for kind, want := range map[string]bool{"child": true, "split": true, "executor": false} {
		ok, why, err := op.Proven(context.Background(), nil, engine.Event{Data: map[string]any{"node": "A", "plan": "p", "released": false, "kind": kind, "reason": "x"}})
		if err != nil || ok != want {
			t.Errorf("kind %s: proven %v (%s) %v, want %v", kind, ok, why, err, want)
		}
	}
}

// d5: a verdict names an id once.
func TestDecodeVerdictRefusesARepeatedCriterion(t *testing.T) {
	raw := `{"verdict":"verified","criteria":[{"id":"c1","met":true,"evidence":"a"},{"id":"c1","met":true,"evidence":"b"}],"defects":[],"summary":"s"}`
	if _, err := DecodeVerdict(json.RawMessage(raw)); err == nil {
		t.Error("a verdict that names c1 twice was taken")
	}
}

// d7: a step id that is stored with another checkpoint is a conflict, never a silent success, in memory and in the relay store;
// the lane's record then keeps both decisions under ids that say which checkpoint each describes.
func TestStepRecordKeepsTheCheckpointOfEveryRun(t *testing.T) {
	h1, h2 := strings.Repeat("a", 40), strings.Repeat("b", 40)
	mem := &MemoryDecisions{}
	one := StepRow{StepID: "step_1", RunID: "run-1", NodeID: "A", FromTier: "executor", ToTier: "executor-t1", RuleID: RuleTier1, Evidence: map[string]any{"letter": "a"}, CheckpointHead: h1, CheckpointTree: "t1"}
	two := one
	two.CheckpointHead, two.CheckpointTree = h2, "t2"
	if err := mem.AppendStep(context.Background(), one); err != nil {
		t.Fatal(err)
	}
	if err := mem.AppendStep(context.Background(), one); err != nil {
		t.Errorf("the same decision again: %v", err)
	}
	if err := mem.AppendStep(context.Background(), two); !errors.Is(err, ErrStepConflict) {
		t.Errorf("another checkpoint under the same id: %v", err)
	}
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "relay.sqlite3"), "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	runRow(t, st)
	db := StoreDecisions{DB: st.DB}
	one.RunID, two.RunID = nRun, nRun
	if err := db.AppendStep(context.Background(), one); err != nil {
		t.Fatal(err)
	}
	if err := db.AppendStep(context.Background(), one); err != nil {
		t.Errorf("the same decision again (store): %v", err)
	}
	if err := db.AppendStep(context.Background(), two); !errors.Is(err, ErrStepConflict) {
		t.Errorf("another checkpoint under the same id (store): %v", err)
	}

	// the lane: the same step position recorded twice with a different last commit
	w := newWorld(t)
	commit := func(c, tree string) {
		w.j.Append(engine.Event{RunID: "run-1", Kind: engine.KindCall, CallID: "c_commit" + c[:4], Op: OpCommit, Event: engine.EvCompleted, Data: map[string]any{"node": "A", "commit": c, "tree": tree}})
	}
	type recorded struct {
		ID         string `json:"id"`
		Checkpoint struct{ Head string }
	}
	step := map[string]any{"id": "step_same"}
	info := map[string]any{"from": "executor", "to": "executor-t1", "letter": "a", "evidence": map[string]any{"tier": "executor"}}
	commit(h1, "t1")
	var first, second recorded
	if err := json.Unmarshal(w.must("stepRecord", "A", step, info), &first); err != nil {
		t.Fatal(err)
	}
	commit(h2, "t2")
	if err := json.Unmarshal(w.must("stepRecord", "A", step, info), &second); err != nil {
		t.Fatal(err)
	}
	if first.Checkpoint.Head != h1 || second.Checkpoint.Head != h2 {
		t.Fatalf("checkpoints %q %q", first.Checkpoint.Head, second.Checkpoint.Head)
	}
	if first.ID == second.ID {
		t.Errorf("both runs answer the step id %s: the decision row stores one checkpoint and the second executor-t1 would carry an id that describes the first", first.ID)
	}
	m := w.lane.d.Decisions.(*MemoryDecisions)
	byID := map[string]string{}
	for _, r := range m.Step {
		byID[r.StepID] = r.CheckpointHead
	}
	if byID[first.ID] != h1 || byID[second.ID] != h2 {
		t.Errorf("rows %v: each answered id must stand on the checkpoint it answered with", byID)
	}
}

// d7: a spawn decision made at the same position with other inputs is another decision.
func TestSpawnDecisionIDFollowsItsContent(t *testing.T) {
	w := newWorld(t)
	op := w.lane.Ops()[NameSpawnDecision]
	run := func(inputs map[string]any) {
		raw, _ := json.Marshal([]any{"EXEC_1", "spawn", map[string]any{"node": "A", "role": "executor", "inputs": inputs}})
		if _, err := op.Run(context.Background(), engine.OpRequest{RunID: "run-1", CallID: "c_000000000001", RequestID: "r", Args: raw, Journal: w.j}); err != nil {
			t.Fatal(err)
		}
	}
	run(map[string]any{"label": "build:A:executor"})
	run(map[string]any{"label": "build:A:executor"})
	m := w.lane.d.Decisions.(*MemoryDecisions)
	if len(m.Spawn) != 1 {
		t.Fatalf("the same decision twice is %d rows", len(m.Spawn))
	}
	run(map[string]any{"label": "rebuild:A:executor:r1"})
	if len(m.Spawn) != 2 {
		t.Errorf("a decision with other inputs at the same position was dropped: %d rows", len(m.Spawn))
	}
}

// impactVerdicts are one answer of each verdict schema a reviewer writes, with one defect of the given impact.
func impactVerdicts(verdict, severity, impact string) map[string][]byte {
	d := map[string]any{"ref": "d1", "kind": "correctness", "severity": severity, "introduced": true, "impact": impact, "inPromise": false, "file": "f.go", "line": 3, "finding": "x", "evidence_question": nil}
	fields := map[string]any{"verdict": verdict, "criteria": []any{map[string]any{"id": "c1", "met": true, "evidence": "e"}}, "defects": []any{d}, "summary": "s"}
	block := map[string]any{"node": "A", "head": strings.Repeat("a", 40), "criteria_digest": strings.Repeat("c", 64)}
	for k, v := range fields {
		block[k] = v
	}
	final := map[string]any{"alignment": map[string]any{"verdict": "ALIGNED", "drift": []any{}}}
	for k, v := range fields {
		final[k] = v
	}
	one, _ := json.Marshal(fields)
	batch, _ := json.Marshal(map[string]any{"nodes": []any{block}})
	fin, _ := json.Marshal(final)
	return map[string][]byte{SchemaVerdict: one, SchemaVerdictBatch: batch, SchemaVerdictFinal: fin}
}

// CRW-1438: the impact of a defect is one closed set, held once in premerge.Impacts. Every verdict schema a reviewer answers with
// names exactly that set, so a word outside it (or an empty word) is not a verdict.
func TestImpactIsAClosedSet(t *testing.T) {
	var literal []string
	if err := json.Unmarshal([]byte(impacts), &literal); err != nil {
		t.Fatalf("the Go impact enum is not JSON: %v", err)
	}
	if !slices.Equal(literal, premerge.Impacts) {
		t.Errorf("the Go impact enum %v differs from premerge.Impacts %v", literal, premerge.Impacts)
	}
	if !slices.Contains(premerge.Impacts, premerge.MinorSeparable) {
		t.Errorf("premerge.Impacts %v leaves out %s", premerge.Impacts, premerge.MinorSeparable)
	}
	for _, bad := range []string{"cosmetic", ""} {
		for name, raw := range impactVerdicts("needs_changes", "P3", bad) {
			if err := Validate(name, raw); err == nil {
				t.Errorf("%s accepted the impact %q", name, bad)
			}
		}
	}
	for _, good := range premerge.Impacts {
		for name, raw := range impactVerdicts("needs_changes", "P3", good) {
			if err := Validate(name, raw); err != nil {
				t.Errorf("%s refused the impact %q: %v", name, good, err)
			}
		}
	}
}

// CRW-1438: a verified verdict over a defect that blocks (premerge.Blocks: P0 to P2, or an impact other than minor_separable) says
// what its own premerge record refuses, so it is no verdict (crw.review reads the error as verdict_invalid).
func TestVerifiedWithABlockingImpactIsVerdictInvalid(t *testing.T) {
	if _, err := DecodeVerdict(impactVerdicts("verified", "P3", "core_function")[SchemaVerdict]); err == nil {
		t.Errorf("verified over a P3 core_function defect decoded")
	}
	if _, err := DecodeVerdict(impactVerdicts("verified", "P3", premerge.MinorSeparable)[SchemaVerdict]); err != nil {
		t.Errorf("verified over a P3 minor_separable defect: %v", err)
	}
	if _, err := DecodeVerdict(impactVerdicts("needs_changes", "P3", "core_function")[SchemaVerdict]); err != nil {
		t.Errorf("needs_changes over a P3 core_function defect: %v", err)
	}
	if _, err := DecodeVerdictBatch(impactVerdicts("verified", "P3", "core_function")[SchemaVerdictBatch]); err == nil {
		t.Errorf("a batch block verified over a P3 core_function defect decoded")
	}
	if _, err := DecodeVerdictBatch(impactVerdicts("verified", "P3", premerge.MinorSeparable)[SchemaVerdictBatch]); err != nil {
		t.Errorf("a batch block verified over a P3 minor_separable defect: %v", err)
	}
	for _, c := range []struct {
		severity, impact string
		blocks           bool
	}{{"P0", "", true}, {"P2", premerge.MinorSeparable, true}, {"P3", "", false}, {"P3", premerge.MinorSeparable, false}, {"P3", "core_function", true}, {"P3", "cosmetic", true}} {
		if got := premerge.Blocks(c.severity, c.impact); got != c.blocks {
			t.Errorf("Blocks(%q, %q) = %v, want %v", c.severity, c.impact, got, c.blocks)
		}
	}
}

// CRW-1438: the reviewer is told the closed impact set its verdict answers with: the packet of crw:lib/packets names every value of
// premerge.Impacts, that only minor_separable does not block, and that verified over a blocking defect is refused; the reviewer
// overlay says the same under Output.
func TestReviewPacketNamesTheImpactVocabulary(t *testing.T) {
	src := "import { reviewPacket } from 'crw:lib/packets'\nexport const meta = { name: 'review-packet' }\n" +
		"return reviewPacket('A', { issue: 'CRW-1', criteria_digest: 'd', criteria: ['first'], done_condition: 'done', regions: ['a.go'] }, " +
		"{ commit: 'h1', changed: ['a.go'], out_of_scope: [] }, { pass: true, command: 'go test ./...', digest: 'v1' }, { round: 1 })\n"
	prog, err := script.Load(script.Source{Name: "review-packet.js", Text: []byte(src)})
	if err != nil {
		t.Fatal(err)
	}
	res, err := engine.Run(context.Background(), prog, engine.Config{Backend: engine.NewFake(), Policy: engine.FakePolicy()})
	if err != nil || res.Status != engine.RunCompleted {
		t.Fatalf("%v %+v", err, res)
	}
	var packet string
	if err := json.Unmarshal(res.Value, &packet); err != nil {
		t.Fatalf("the review packet is not a string: %v (%s)", err, res.Value)
	}
	overlay, err := roles.Overlay("reviewer", roles.Options{})
	if err != nil {
		t.Fatal(err)
	}
	for name, text := range map[string]string{"the review packet": packet, "the reviewer overlay": overlay} {
		for _, impact := range premerge.Impacts {
			if !strings.Contains(text, impact) {
				t.Errorf("%s does not name the impact %s:\n%s", name, impact, text)
			}
		}
		for _, rule := range []string{"only " + premerge.MinorSeparable + " does not block", "verified over a defect that blocks is refused"} {
			if !strings.Contains(text, rule) {
				t.Errorf("%s does not say %q:\n%s", name, rule, text)
			}
		}
	}
}

// acceptCapture is the relay of a test that reads what crw.accept asked of it: it keeps every AcceptRequest and accepts it.
type acceptCapture struct {
	*stubRelay
	got []AcceptRequest
}

func (c *acceptCapture) Accept(_ context.Context, r AcceptRequest) (AcceptResult, error) {
	c.got = append(c.got, r)
	return AcceptResult{Accepted: true, AcceptanceID: "acc-A"}, nil
}

// CRW-1456: crw.accept assembles the premerge record of a head from a unit-check record when no newer verified review stands on
// it. The record's grader is the unit check's pair and prompt, every criterion of the node's plan passes on the evidence of the
// passing verification and the clean check, and each finding is a defect named by its finding id. A check that is not verified,
// that gave no judgment, or that saw another tree than the head's is no basis; a node whose plan names no criterion has none.
func TestAcceptAssemblesPremergeFromAUnitCheckRecord(t *testing.T) {
	head, base := strings.Repeat("a", 40), strings.Repeat("b", 40)
	criteria, vdigest := strings.Repeat("c", 64), strings.Repeat("d", 64)
	const findingID = "0123456789abcdef"
	type world2 struct {
		*world
		relay *acceptCapture
		git   *int
	}
	setup := func(criteriaOfA []string) world2 {
		w := newWorld(t)
		w.relay.plan = planOfNodes(map[string]string{"A": "a1"}, "A")
		w.relay.plan.Nodes[0].Criteria = criteriaOfA
		w.ledgered(true, designOf("A"))
		w.must("release", "plan-1", "A", "M")
		capture := &acceptCapture{stubRelay: w.relay}
		w.lane.d.Relay = capture
		calls := 0
		w.lane.d.Git = func(context.Context, string, []string, ...string) (string, error) {
			calls++
			return "", errors.New("this test runs no git")
		}
		return world2{world: w, relay: capture, git: &calls}
	}
	appendRow := func(w world2, op, callID string, data map[string]any) {
		b, _ := json.Marshal(data)
		var round map[string]any
		_ = json.Unmarshal(b, &round)
		w.j.Append(engine.Event{RunID: "run-1", Kind: engine.KindCall, CallID: callID, Op: op, Event: engine.EvCompleted, Data: round})
	}
	commitRow := func(w world2, tree string) {
		appendRow(w, OpCommit, "c_commit", map[string]any{"node": "A", "commit": head, "parent": base, "tree": tree, "result": "{}"})
	}
	verifyRow := func(w world2) {
		appendRow(w, OpVerify, "c_verify", map[string]any{"node": "A", "head": head, "ok": true, "base": base, "digest": vdigest, "result": "{}"})
	}
	finding := func(severity, impact string) map[string]any {
		return map[string]any{"id": findingID, "severity": severity, "impact": impact, "file": "x.go", "line": 7, "text": "a nit"}
	}
	unitCheck := func(w world2, state, verdict string, findings []any, extra map[string]any) {
		res, _ := json.Marshal(map[string]any{"state": state, "detail": "", "verdict": verdict, "findings": findings, "engine_family": "grok", "model": "grok-4.7-medium",
			"effort": "medium", "guide_rev": "g1", "prompt_digest": "pd-unit", "fingerprint_before": "t-head", "fingerprint_after": "t-head", "elapsed_ms": 1})
		data := map[string]any{"role": "unit-check", "node": "A", "execution_id": "exec-A", "tree": "t-head", "criteria_digest": criteria, "result": string(res)}
		for k, v := range extra {
			data[k] = v
		}
		appendRow(w, OpUnitCheck, "c_uc", data)
	}
	clean := func() []any { return []any{finding("P3", premerge.MinorSeparable)} }
	refused := func(w world2, reason string) {
		t.Helper()
		_, err := w.op("accept", "A", head)
		if err == nil || ReasonOf(err.Error()) != reason {
			t.Errorf("crw.accept: %v, want refused %s", err, reason)
		}
		if len(w.relay.got) != 0 {
			t.Errorf("the relay was asked to accept: %+v", w.relay.got)
		}
	}

	// a pre-commit unit check of the head's tree, no review: the premerge record is the unit check's
	w := setup([]string{"first", "second"})
	commitRow(w, "t-head")
	verifyRow(w)
	unitCheck(w, "ok", "verified", clean(), nil)
	if _, err := w.op("accept", "A", head); err != nil {
		t.Fatalf("crw.accept on a unit check: %v", err)
	}
	if len(w.relay.got) != 1 {
		t.Fatalf("the relay was asked %d times", len(w.relay.got))
	}
	got := w.relay.got[0]
	if got.ReviewerCall != "c_uc" || got.CriteriaDigest != criteria || got.Head != head || got.Base != base {
		t.Errorf("accept request %+v", got)
	}
	rec, err := premerge.Decode(got.Premerge)
	if err != nil {
		t.Fatalf("premerge: %v", err)
	}
	if rec.Grader != (premerge.Grader{Model: "grok-4.7-medium", Effort: "medium", PromptDigest: "pd-unit"}) || rec.CriteriaDigest != criteria || rec.Issue != "CRW-A" {
		t.Errorf("premerge record %+v", rec)
	}
	if len(rec.Criteria) != 2 {
		t.Errorf("criteria %+v, want c1 and c2", rec.Criteria)
	}
	for _, id := range []string{"c1", "c2"} {
		c := rec.Criteria[id]
		if c.Verdict != "PASS" || !strings.Contains(c.Evidence, vdigest) || !strings.Contains(c.Evidence, "c_uc") {
			t.Errorf("criterion %s: %+v", id, c)
		}
	}
	if len(rec.Defects) != 1 || rec.Defects[0].ID != findingID || rec.Defects[0].Severity != "P3" || rec.Defects[0].Impact != premerge.MinorSeparable || len(rec.Dispositions.Items) != 0 {
		t.Errorf("defects %+v, dispositions %+v", rec.Defects, rec.Dispositions.Items)
	}

	// a verified record over a blocking finding is no verified check
	w = setup([]string{"first", "second"})
	commitRow(w, "t-head")
	verifyRow(w)
	unitCheck(w, "ok", "verified", []any{finding("P3", "core_function")}, nil)
	refused(w, "reviewer_not_verified")

	// a check that gave no judgment is no basis
	w = setup([]string{"first", "second"})
	commitRow(w, "t-head")
	verifyRow(w)
	unitCheck(w, "quota", "", []any{}, nil)
	refused(w, "reviewer_missing")

	// a unit check needs a passing verification as a review does
	w = setup([]string{"first", "second"})
	commitRow(w, "t-head")
	unitCheck(w, "ok", "verified", clean(), nil)
	refused(w, "verification_missing")

	// the head's commit row names another tree than the one the check saw
	w = setup([]string{"first", "second"})
	commitRow(w, "t-other")
	verifyRow(w)
	unitCheck(w, "ok", "verified", clean(), nil)
	refused(w, "reviewer_missing")

	// no commit row and no worktree: the tree of the head is unknown, so a pre-commit check does not match, and git is not asked
	w = setup([]string{"first", "second"})
	verifyRow(w)
	unitCheck(w, "ok", "verified", clean(), nil)
	refused(w, "reviewer_missing")
	if *w.git != 0 {
		t.Errorf("git was asked %d times with no worktree", *w.git)
	}

	// a check that names the head as its commit needs no tree
	w = setup([]string{"first", "second"})
	verifyRow(w)
	unitCheck(w, "ok", "verified", clean(), map[string]any{"commit": head})
	if _, err := w.op("accept", "A", head); err != nil || len(w.relay.got) != 1 || w.relay.got[0].ReviewerCall != "c_uc" {
		t.Errorf("a unit check on the head's commit: %v %+v", err, w.relay.got)
	}

	// a node whose plan names no criterion is never accepted on a unit check
	w = setup(nil)
	commitRow(w, "t-head")
	verifyRow(w)
	unitCheck(w, "ok", "verified", clean(), nil)
	refused(w, "premerge_missing")

	// a verified review newer than the unit check is the basis; an older one is not
	reviewRow := func(w world2) {
		verdict, _ := json.Marshal(map[string]any{"verdict": "verified", "criteria": []any{map[string]any{"id": "c1", "met": true, "evidence": "e1"}, map[string]any{"id": "c2", "met": true, "evidence": "e2"}},
			"defects": []any{}, "summary": "s"})
		w.j.Append(engine.Event{RunID: "run-1", Kind: engine.KindCall, CallID: "c_rev", Op: "agent", Event: engine.EvPrepared,
			Data: map[string]any{"role": "reviewer", "node": "A", "model": "gpt-x", "effort": "high", "prompt_digest": "pd-rev"}})
		rr, _ := json.Marshal(ReviewResult{State: StateVerdict, Verdict: verdict, Findings: []FindingRef{}, Head: head, CriteriaDigest: strings.Repeat("e", 64), Call: Brief{CallID: "c_rev"}})
		appendRow(w, OpReview, "c_review", map[string]any{"node": "A", "head": head, "criteria_digest": strings.Repeat("e", 64), "state": StateVerdict, "verdict": "verified", "call": "c_rev", "result": string(rr)})
	}
	for _, reviewLast := range []bool{true, false} {
		w = setup([]string{"first", "second"})
		commitRow(w, "t-head")
		verifyRow(w)
		if reviewLast {
			unitCheck(w, "ok", "verified", clean(), nil)
			reviewRow(w)
		} else {
			reviewRow(w)
			unitCheck(w, "ok", "verified", clean(), nil)
		}
		if _, err := w.op("accept", "A", head); err != nil || len(w.relay.got) != 1 {
			t.Fatalf("review last %v: %v %+v", reviewLast, err, w.relay.got)
		}
		want, grader := "c_uc", "pd-unit"
		if reviewLast {
			want, grader = "c_rev", "pd-rev"
		}
		rec, err := premerge.Decode(w.relay.got[0].Premerge)
		if err != nil || w.relay.got[0].ReviewerCall != want || rec.Grader.PromptDigest != grader {
			t.Errorf("review last %v: basis %s grader %+v (%v), want %s", reviewLast, w.relay.got[0].ReviewerCall, rec.Grader, err, want)
		}
	}
}
