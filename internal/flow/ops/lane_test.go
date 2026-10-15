package ops

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/reviewadapter"
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

// crw.releasedPlans lists the started nodes of the milestone, each with the plan of the revision it was released under, whatever
// revision the ledger has grown to since; a node that has not started is not listed. crw.designHead samples the integration tip of
// the run's plan once.
func TestReleasedPlansAndDesignHead(t *testing.T) {
	w := newWorld(t)
	w.relay.plan = planOfNodes(map[string]string{"A": "sa", "B": "sb"}, "A", "B")
	w.ledgered(true, designOf("A", "B"))
	w.must("release", "plan-1", "A", "M")
	later := designOf("A", "B")
	later.Issues[0].Approach = "approach after the re-plan"
	var anyLater any
	b, _ := json.Marshal(later)
	_ = json.Unmarshal(b, &anyLater)
	w.j.Append(engine.Event{RunID: "run-1", Kind: engine.KindCall, CallID: "c_later", Op: OpDesignLedger, Event: engine.EvCompleted, Data: map[string]any{"milestone": "M", "revision": "r1", "digest": "later", "design": anyLater}})
	var got []ReleasedPlan
	_ = json.Unmarshal(w.must("releasedPlans", "M"), &got)
	if len(got) != 1 {
		t.Fatalf("released plans %+v, want A only", got)
	}
	want := designOf("A", "B").Issues[0]
	if p := got[0]; p.Node != "A" || p.Revision != "r0" || !reflect.DeepEqual(p.Plan, want) || len(p.Decisions) != 1 || !reflect.DeepEqual(p.Decisions[0], designOf("A").Decisions[0]) {
		t.Errorf("the released plan of A: %+v", p)
	}
	var head DesignHead
	_ = json.Unmarshal(w.must("designHead", "M"), &head)
	if head.Milestone != "M" || head.Head != w.relay.tip {
		t.Errorf("design head %+v, want the integration tip %s", head, w.relay.tip)
	}
	if !IsRecordOp(OpDesignHead) || !IsRecordOp(OpReleasedPlans) {
		t.Error("the design head and the released plans are recorded once and never read again")
	}
}

// A node whose audited issue is sized for a split or a child has started too: its lane recorded a release row under the revision it
// was sized under (released false, kind split or child) and escalated it. crw.releasedPlans lists it with that plan, as it lists a
// node released to an executor, so a later re-plan's critic compares the DESIGN with it.
func TestReleasedPlansListTheNodesSizedForASplitOrAChild(t *testing.T) {
	w := newWorld(t)
	w.relay.plan = planOfNodes(map[string]string{"A": "sa", "B": "sb", "C": "sc"}, "A", "B", "C")
	d := designOf("A", "B", "C")
	d.Issues[1].SizeFit, d.Issues[1].SizeReason, d.Issues[1].IssueSizeDecision = "split", "two packages", "over_line"
	w.ledgered(true, d)
	for _, n := range []string{"A", "B"} {
		w.must("release", "plan-1", n, "M")
	}
	raw := w.must("releasedPlans", "M")
	var got []ReleasedPlan
	_ = json.Unmarshal(raw, &got)
	var kinds []map[string]any
	_ = json.Unmarshal(raw, &kinds)
	byNode, kind := map[string]ReleasedPlan{}, map[string]any{}
	for i, p := range got {
		byNode[p.Node] = p
		kind[p.Node] = kinds[i]["kind"]
	}
	if len(got) != 2 || !reflect.DeepEqual(byNode["B"].Plan, d.Issues[1]) || byNode["B"].Revision != "r0" {
		t.Errorf("released plans %+v, want A and B (sized for a split under r0, with its whole plan); C has not started", got)
	}
	if kind["A"] != "released" || kind["B"] != "split" {
		t.Errorf("the kinds of the released plans %v, want A released and B split", kind)
	}
}

// A node whose audited issue is sized for a child has started: its lane recorded a release row (released false, kind child,
// size_fit_child) before it escalated the node. Both readers of the one started predicate count it so: the signal path (lanes, which
// crw.signalBind records as started) does not leave it unstarted for a due signal to re-plan, and crw.releasedPlans lists it with
// the plan it was sized under.
func TestAChildSizedNodeHasStarted(t *testing.T) {
	w := newWorld(t)
	w.relay.plan = planOfNodes(map[string]string{"A": "sa", "B": "sb", "C": "sc"}, "A", "B", "C")
	d := designOf("A", "B", "C")
	d.Issues[1].SizeFit, d.Issues[1].SizeReason, d.Issues[1].IssueSizeDecision = "child", "a child of its own", "over_line"
	w.ledgered(true, d)
	var rel ReleaseOutcome
	_ = json.Unmarshal(w.must("release", "plan-1", "B", "M"), &rel)
	if rel.Released || rel.Kind != "child" || rel.Reason != "size_fit_child" {
		t.Fatalf("release of the child-sized node: %+v, want released false, kind child, size_fit_child", rel)
	}
	r := rows(w.j.Events())
	started, unstarted := lanes(r, []string{"A", "B", "C"})
	if !reflect.DeepEqual(started, []string{"B"}) || !reflect.DeepEqual(unstarted, []string{"A", "C"}) {
		t.Errorf("signal path: started %v, unstarted %v, want B started and A, C unstarted", started, unstarted)
	}
	if _, kind, ok := startRowOf(r, "B"); !ok || kind != "child" {
		t.Errorf("startRowOf(B) = %q, %v, want child, true", kind, ok)
	}
	raw := w.must("releasedPlans", "M")
	var got []ReleasedPlan
	_ = json.Unmarshal(raw, &got)
	var kinds []map[string]any
	_ = json.Unmarshal(raw, &kinds)
	if len(got) != 1 || got[0].Node != "B" || got[0].Revision != "r0" || !reflect.DeepEqual(got[0].Plan, d.Issues[1]) || kinds[0]["kind"] != "child" {
		t.Errorf("released plans %+v (kinds %v), want B only, sized for a child under r0 with its whole plan", got, kinds)
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
	// a node gets two research continuations on a tier, the reuse above being the first (CRW-1470); the third question and every
	// one after it is a scope question
	for i := 1; i <= 3; i++ {
		_ = json.Unmarshal(w.must("exploreRoute", ExploreResearch, ExploreContext{Node: "A", Tier: "executor"}, []Question{{Question: fmt.Sprintf("research %d", i)}}), &plan)
		want := RouteSpawn
		if i >= 2 {
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

// The research cap of a tier (SP-EXPL-2, CRW-1470) counts every research continuation of the node on the tier, the spawned and the
// reused alike, and only crw.exploreRoute counts it: after a spawn and a reuse the next question escalates as research_cap, and so
// does a third request whose question was answered in the run. A reuse of another node's answer counts for this node's tier, and
// the other tier keeps its own two.
func TestResearchCapCountsReuseAndEscalatesOnce(t *testing.T) {
	w := newWorld(t)
	route := func(node, tier, question string) Route {
		t.Helper()
		var plan RoutePlan
		_ = json.Unmarshal(w.must("exploreRoute", ExploreResearch, ExploreContext{Node: node, Tier: tier}, []Question{{Question: question}}), &plan)
		if len(plan.Routes) != 1 {
			t.Fatalf("routes %+v", plan.Routes)
		}
		return plan.Routes[0]
	}
	answer := func(node, tier string, rt Route, text string) {
		t.Helper()
		w.must("exploreRecord", ExploreResearch, ExploreContext{Node: node, Tier: tier}, []Answer{{Digest: rt.Digest, Question: rt.Question, Answer: text, Route: rt.Route, Origin: rt.Origin}})
	}
	// spawn, then reuse of that answer, then a new question: the third continuation escalates
	first := route("A", "executor", "which module owns the case?")
	if first.Route != RouteSpawn {
		t.Fatalf("first research question: %+v", first)
	}
	answer("A", "executor", first, "the store")
	if rt := route("A", "executor", "which module owns the case?"); rt.Route != RouteReuse || rt.Answer != "the store" {
		t.Fatalf("the answered question again: %+v", rt)
	}
	if rt := route("A", "executor", "a new question"); rt.Route != RouteEscalate || rt.Reason != "research_cap" {
		t.Errorf("a third continuation after a spawn and a reuse: %+v, want escalate research_cap", rt)
	}
	// a third request that is itself a reuse escalates too
	if rt := route("A", "executor", "which module owns the case?"); rt.Route != RouteEscalate || rt.Reason != "research_cap" {
		t.Errorf("a third continuation whose question was answered: %+v, want escalate research_cap", rt)
	}
	// node B reuses A's answer: that is its first continuation on the tier; one spawn more, then the cap
	if rt := route("B", "executor", "which module owns the case?"); rt.Route != RouteReuse {
		t.Fatalf("node B reuses the answer of A: %+v", rt)
	}
	if rt := route("B", "executor", "B's own question"); rt.Route != RouteSpawn {
		t.Errorf("node B's second continuation: %+v, want spawn", rt)
	}
	if rt := route("B", "executor", "B's third question"); rt.Route != RouteEscalate || rt.Reason != "research_cap" {
		t.Errorf("node B's third continuation: %+v, want escalate research_cap", rt)
	}
	// the next tier of A counts on its own
	if rt := route("A", "executor-t1", "which module owns the case?"); rt.Route != RouteReuse {
		t.Errorf("executor-t1 of A: %+v, want the reuse", rt)
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

// ---- crw.unitCheck (CRW-1463) ----

// checkReply is one answer of a scripted unit checker: the record it gives, or the Go error it fails with.
type checkReply struct {
	res reviewadapter.Result
	err error
}

// scriptedChecker is a UnitChecker that answers its replies in order (the last one again when they run out) and keeps every request.
type scriptedChecker struct {
	replies []checkReply
	got     []reviewadapter.Request
}

func (c *scriptedChecker) Check(_ context.Context, req reviewadapter.Request) (reviewadapter.Result, error) {
	c.got = append(c.got, req)
	r := checkReply{res: checkRecord(reviewadapter.StateOK, reviewadapter.VerdictVerified)}
	if n := len(c.replies); n > 0 {
		r = c.replies[min(len(c.got), n)-1]
	}
	return r.res, r.err
}

// checkRecord is a record of a state, with the pair and the fingerprints the lab's checker names.
func checkRecord(state reviewadapter.State, verdict string, findings ...reviewadapter.Finding) reviewadapter.Result {
	if findings == nil {
		findings = []reviewadapter.Finding{}
	}
	return reviewadapter.Result{State: state, Verdict: verdict, Findings: findings, EngineFamily: "grok", Model: "grok-4.7-medium", Effort: "medium", GuideRev: UnitCheckGuideRev,
		PromptDigest: "pd-check", FingerprintBefore: "fp", FingerprintAfter: "fp", ElapsedMS: 1}
}

// ucCriteria is the criteria digest of node A in a ucWorld.
var ucCriteria = strings.Repeat("c", 64)

// ucWorld is a lane world with node A released (execution exec-A, criteria digest ucCriteria, two criteria) and a real git worktree an
// executor call of A ran in.
type ucWorld struct {
	*world
	repo, wt, start string
	checker         *scriptedChecker
	modes           []string
}

func newUCWorld(t *testing.T) *ucWorld {
	t.Helper()
	w := newWorld(t)
	w.relay.plan = planOfNodes(map[string]string{"A": "a1"}, "A")
	w.relay.plan.Nodes[0].Criteria = []string{"the first thing holds", "the second thing holds"}
	w.relay.plan.Nodes[0].CriteriaDigest = ucCriteria
	w.ledgered(true, designOf("A"))
	w.must("release", "plan-1", "A", "M")
	repo, first := newRepo(t)
	u := &ucWorld{world: w, repo: repo, start: first, wt: addWorktree(t, repo, "attempt1", first), checker: &scriptedChecker{}}
	w.relay.tip = first
	w.lane.d.Verifier = &fakeVerifier{out: VerifyOutcome{Pass: true, Command: CILocalCommand, Digest: strings.Repeat("d", 64), Summary: "pass", Failures: []string{}}}
	w.lane.d.UnitCheck = &UnitCheck{Checker: u.checker, Limit: time.Minute, Profile: func(mode string) (reviewadapter.Profile, error) {
		u.modes = append(u.modes, mode)
		return reviewadapter.JSONProfile("lab-"+mode, []string{"lab-check"}, "grok-4.7-medium", "medium"), nil
	}}
	return u
}

// build edits the worktree and writes the rows of an executor call of A that completed with the pending tree of the worktree. The call
// is placed in the worktree at the commit its HEAD is at, as a backend places a fresh call.
func (u *ucWorld) build(callID, body string) CallRef {
	u.t.Helper()
	write(u.t, u.wt, "a.txt", body, 0o644)
	u.j.Append(engine.Event{RunID: "run-1", Kind: engine.KindCall, CallID: callID, Op: "agent", Event: engine.EvPrepared, Data: map[string]any{"role": "executor", "node": "A",
		"allocation": map[string]any{"kind": engine.PlaceFresh, "path": u.wt, "start_commit": gitIn(u.t, u.wt, "rev-parse", "HEAD"), "attempt": 1}}})
	u.j.Append(engine.Event{RunID: "run-1", Kind: engine.KindCall, CallID: callID, Op: "agent", Event: engine.EvCompleted, Data: map[string]any{"pending_tree": u.snapshot()}})
	return call(callID, "completed", "claude-x", nil)
}

func (u *ucWorld) snapshot() string {
	u.t.Helper()
	snap, err := Snapshot(context.Background(), ExecGit("git"), u.wt)
	if err != nil {
		u.t.Fatal(err)
	}
	return snap
}

// checkRows are the completed crw.unitCheck rows of the journal.
func (u *ucWorld) checkRows() rows { return rows(u.j.Events()).opRows(OpUnitCheck) }

// lastCheck is the newest crw.unitCheck row; the test fails when there is none.
func (u *ucWorld) lastCheck() engine.Event {
	u.t.Helper()
	r := u.checkRows()
	if len(r) == 0 {
		u.t.Fatal("no crw.unitCheck row was written")
	}
	return r[len(r)-1]
}

func (u *ucWorld) proven(row engine.Event) bool {
	u.t.Helper()
	ok, _, err := u.lane.Ops()[NameUnitCheck].Proven(context.Background(), u.j.Events(), row)
	if err != nil {
		u.t.Fatal(err)
	}
	return ok
}

func decodeCheck(t *testing.T, raw json.RawMessage) UnitCheckOutcome {
	t.Helper()
	var out UnitCheckOutcome
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("the answer of crw.unitCheck: %v (%s)", err, raw)
	}
	return out
}

// TestUnitCheckRecordsTheAdapterResult (CRW-1463): crw.unitCheck runs the adapter on the node's worktree and writes one completed row
// in the shape crw.accept and the relay read: role unit-check, the node, the execution and criteria digest of its release, the tree the
// check saw (in a real git worktree: the pending tree, which crw.commit commits), the judged call and the round, and the adapter's
// record as result. crw.accept takes that row as the basis of the head. With a commit and a base it is the check of a committed head.
func TestUnitCheckRecordsTheAdapterResult(t *testing.T) {
	u := newUCWorld(t)
	finding := reviewadapter.Finding{Severity: "P3", Impact: premerge.MinorSeparable, File: "a.txt", Line: 1, Text: "a nit"}
	var prompts []string
	var ranges [][2]string
	u.lane.d.UnitCheck.Checker = reviewadapter.Adapter{
		Run: func(_ context.Context, _ reviewadapter.Profile, req reviewadapter.Request) ([]byte, []byte, int, error) {
			prompts = append(prompts, req.Prompt)
			ranges = append(ranges, [2]string{req.Base, req.Head})
			b, _ := json.Marshal(map[string]any{"verdict": "verified", "findings": []any{map[string]any{"severity": finding.Severity, "impact": finding.Impact, "file": finding.File, "line": finding.Line, "text": finding.Text}}})
			return b, nil, 0, nil
		},
		Fingerprint: func(ctx context.Context, dir string) (string, error) { return Snapshot(ctx, ExecGit("git"), dir) },
	}
	ex := u.build("c_ex1", "built\n")
	pending := u.snapshot()
	out := decodeCheck(t, u.must("unitCheck", "A", ex, map[string]any{"round": 1}))
	if out.State != reviewadapter.StateOK || out.Verdict != reviewadapter.VerdictVerified || out.Hold != "" || out.Retried || out.Call != "c_ex1" || len(out.Findings) != 1 ||
		out.Findings[0].ID != reviewadapter.FindingID("a.txt", "a nit") {
		t.Errorf("answer %+v", out)
	}
	row := u.lastCheck()
	want := map[string]any{"role": "unit-check", "node": "A", "execution_id": "exec-A", "criteria_digest": ucCriteria, "tree": pending, "call": "c_ex1", "round": float64(1),
		"final": false, "retried": false, "state": "ok", "mode": "uncommitted"}
	for k, v := range want {
		if row.Data[k] != v {
			t.Errorf("row %s = %#v, want %#v", k, row.Data[k], v)
		}
	}
	for _, k := range []string{"commit", "head", "base", "hold"} {
		if _, has := row.Data[k]; has {
			t.Errorf("an uncommitted check's row has %s: %v", k, row.Data[k])
		}
	}
	var rec reviewadapter.Result
	if err := json.Unmarshal(resultOf(row), &rec); err != nil || !reflect.DeepEqual(rec, out.Result) {
		t.Errorf("the row's result %+v (%v) is not the answer's record %+v", rec, err, out.Result)
	}
	if rec.FingerprintBefore != pending || rec.Model != "grok-4.7-medium" || rec.EngineFamily != "grok" || rec.GuideRev != UnitCheckGuideRev {
		t.Errorf("record %+v", rec)
	}
	if !u.proven(row) {
		t.Error("a check that gave a judgment is not proven")
	}
	if len(prompts) != 1 || !strings.Contains(prompts[0], "Review the uncommitted changes in this working tree.") || !strings.Contains(prompts[0], "node A") ||
		!strings.Contains(prompts[0], "c1: the first thing holds") || !strings.Contains(prompts[0], "c2: the second thing holds") {
		t.Errorf("prompt %q", prompts)
	}
	if !slices.Equal(u.modes, []string{"uncommitted"}) {
		t.Errorf("profile modes %v", u.modes)
	}

	// the commit of the checked pending tree is the head the check is on: crw.accept assembles its premerge record from the check
	var commit CommitOutcome
	_ = json.Unmarshal(u.must("commit", "A", ex, []string{"a.txt"}), &commit)
	if commit.Tree != dataStr(row, "tree") {
		t.Fatalf("the commit's tree %s is not the tree the check saw %s", commit.Tree, dataStr(row, "tree"))
	}
	u.must("verify", "A", commit.Commit)
	capture := &acceptCapture{stubRelay: u.relay}
	u.lane.d.Relay = capture
	if _, err := u.op("accept", "A", commit.Commit); err != nil {
		t.Fatalf("crw.accept on the unit check: %v", err)
	}
	if len(capture.got) != 1 || capture.got[0].ReviewerCall != row.CallID || capture.got[0].CriteriaDigest != ucCriteria {
		t.Fatalf("accept requests %+v, want one on the check %s", capture.got, row.CallID)
	}

	// a committed check: no call, the commit and the base on the row, the range in the prompt
	out = decodeCheck(t, u.must("unitCheck", "A", nil, map[string]any{"commit": commit, "base": u.start, "final": true}))
	row = u.lastCheck()
	if out.State != reviewadapter.StateOK || out.Call != "" || dataStr(row, "commit") != commit.Commit || dataStr(row, "base") != u.start || dataStr(row, "mode") != "committed" ||
		dataStr(row, "role") != "unit-check" || row.Data["final"] != true || dataStr(row, "call") != "" {
		t.Errorf("committed check: answer %+v row %v", out, row.Data)
	}
	scope := fmt.Sprintf("Review the changes between %s and %s (git diff %s..%s).", u.start, commit.Commit, u.start, commit.Commit)
	if len(prompts) != 2 || !strings.Contains(prompts[1], scope) || strings.Contains(prompts[1], "uncommitted") {
		t.Errorf("committed prompt %q, want %q", prompts[len(prompts)-1], scope)
	}
	if !slices.Equal(u.modes, []string{"uncommitted", "committed"}) {
		t.Errorf("profile modes %v", u.modes)
	}
	// the request names the range a committed profile's reader takes an answer about, and an uncommitted one names none
	if len(ranges) != 2 || ranges[0] != [2]string{} || ranges[1] != [2]string{u.start, commit.Commit} {
		t.Errorf("the requests' ranges %v, want none and %s..%s", ranges, u.start, commit.Commit)
	}
	// a committed check of a checkout with edits is not run: the row is role-less, so no reader takes it as the judgment of the commit
	write(t, u.wt, "a.txt", "edited after the commit\n", 0o644)
	got := len(prompts)
	out = decodeCheck(t, u.must("unitCheck", "A", nil, map[string]any{"commit": commit, "base": u.start}))
	row = u.lastCheck()
	if out.State != StateTreeMoved || len(prompts) != got || dataStr(row, "role") != "" || dataStr(row, "commit_tree") != commit.Tree || row.Data["tree_moved"] != true {
		t.Errorf("a committed check of a dirty checkout: answer %+v row %v, %d tool calls", out, row.Data, len(prompts)-got)
	}
	write(t, u.wt, "a.txt", "built\n", 0o644)
	// a committed check needs its base
	if _, err := u.op("unitCheck", "A", nil, map[string]any{"commit": commit.Commit}); err == nil || !strings.Contains(err.Error(), "base") {
		t.Errorf("a committed check without a base: %v", err)
	}
}

// TestUnitCheckQuotaHoldsTheNode (CRW-1463): a check that gave no judgment because the tool's quota is spent or it answered nothing is
// recorded with role unit-check, its record and hold unit_check_<state>; the answer names the same hold, the row is not proven (a resume
// asks again) and the op asks the relay nothing. The lane's crw.hold with that reason and source unit_check writes the hold row and closes
// the execution, and the op run again calls the checker again.
func TestUnitCheckQuotaHoldsTheNode(t *testing.T) {
	for _, state := range []reviewadapter.State{reviewadapter.StateQuota, reviewadapter.StateNoAnswer} {
		t.Run(string(state), func(t *testing.T) {
			u := newUCWorld(t)
			u.checker.replies = []checkReply{{res: checkRecord(state, "")}}
			ex := u.build("c_ex1", "built\n")
			releases, closes := len(u.relay.releases), len(u.relay.closes)
			out := decodeCheck(t, u.must("unitCheck", "A", ex, map[string]any{"round": 1}))
			hold := "unit_check_" + string(state)
			if out.State != state || out.Hold != hold {
				t.Errorf("answer %+v, want state %s hold %s", out, state, hold)
			}
			row := u.lastCheck()
			if dataStr(row, "role") != "unit-check" || dataStr(row, "hold") != hold || dataStr(row, "state") != string(state) {
				t.Errorf("row %v", row.Data)
			}
			var rec reviewadapter.Result
			if err := json.Unmarshal(resultOf(row), &rec); err != nil || rec.State != state {
				t.Errorf("the row's result %+v %v", rec, err)
			}
			if u.proven(row) {
				t.Error("a check held for its tool is proven: a resume would not ask again")
			}
			if len(u.relay.releases) != releases || len(u.relay.closes) != closes {
				t.Errorf("the op called the relay: releases %d closes %d", len(u.relay.releases)-releases, len(u.relay.closes)-closes)
			}
			var held HoldOutcome
			_ = json.Unmarshal(u.must("hold", "A", map[string]any{"reason": out.Hold, "call": ex.CallID, "source": "unit_check"}), &held)
			hr, ok := rows(u.j.Events()).opRows(OpHold).last(func(engine.Event) bool { return true })
			if !held.Held || !ok || dataStr(hr, "source") != "unit_check" || dataStr(hr, "reason") != hold || dataStr(hr, "call") != "c_ex1" {
				t.Errorf("hold %+v row %v", held, hr.Data)
			}
			if len(u.relay.closes) != closes+1 || u.relay.closes[len(u.relay.closes)-1].Node != "A" {
				t.Errorf("the hold closed %+v", u.relay.closes)
			}
			u.checker.replies = nil
			again := decodeCheck(t, u.must("unitCheck", "A", ex, map[string]any{"round": 1}))
			if len(u.checker.got) != 2 || again.State != reviewadapter.StateOK || again.Hold != "" {
				t.Errorf("the check run again: %d checker calls, answer %+v", len(u.checker.got), again)
			}
		})
	}
}

// TestUnitCheckRetriesAnInvalidAnswerOnce (CRW-1463): an invalid answer is asked once more with the same request; the second record is
// the one the row keeps, with retried and first_state invalid. A second invalid answer is the record (proven, no hold): no third call.
func TestUnitCheckRetriesAnInvalidAnswerOnce(t *testing.T) {
	for _, second := range []reviewadapter.State{reviewadapter.StateOK, reviewadapter.StateInvalid} {
		t.Run(string(second), func(t *testing.T) {
			u := newUCWorld(t)
			verdict := ""
			if second == reviewadapter.StateOK {
				verdict = reviewadapter.VerdictVerified
			}
			u.checker.replies = []checkReply{{res: checkRecord(reviewadapter.StateInvalid, "")}, {res: checkRecord(second, verdict)}, {res: checkRecord(reviewadapter.StateOK, "verified")}}
			ex := u.build("c_ex1", "built\n")
			out := decodeCheck(t, u.must("unitCheck", "A", ex, map[string]any{"round": 2}))
			if len(u.checker.got) != 2 {
				t.Fatalf("%d checker calls, want 2", len(u.checker.got))
			}
			if a, b := u.checker.got[0], u.checker.got[1]; a.Prompt != b.Prompt || a.Worktree != b.Worktree || a.GuideRev != b.GuideRev || a.Limit != b.Limit ||
				a.Profile.Name != b.Profile.Name || !bytes.Equal(a.Schema, b.Schema) {
				t.Errorf("the retry's request %+v differs from the first %+v", b, a)
			}
			row := u.lastCheck()
			if out.State != second || !out.Retried || out.Hold != "" || row.Data["retried"] != true || dataStr(row, "first_state") != "invalid" || dataStr(row, "state") != string(second) {
				t.Errorf("answer %+v row %v", out, row.Data)
			}
			if len(u.checkRows()) != 1 || !u.proven(row) {
				t.Errorf("%d rows; proven %v", len(u.checkRows()), u.proven(row))
			}
		})
	}
}

// codexRecord is the record of a Codex review call in state, as reviewadapter.CodexReviewProfile names its pair.
func codexRecord(state reviewadapter.State, verdict string) reviewadapter.Result {
	r := checkRecord(state, verdict)
	r.EngineFamily, r.Model, r.Effort, r.PromptDigest, r.Detail = "gpt", "gpt-6-astra", "high", "pd-codex", "codex: "+string(state)
	return r
}

// codexWithBugbotFallback wires the unit check of u the way the shipped lane does (CRW-1517): the Codex review profiles, and the Bugbot
// profiles as the fallback on a Codex quota. The modes the fallback was asked for are kept in fallbacks.
func codexWithBugbotFallback(u *ucWorld, fallbacks *[]string) {
	u.lane.d.UnitCheck.Profile = func(mode string) (reviewadapter.Profile, error) {
		u.modes = append(u.modes, mode)
		if mode == UnitCheckCommitted {
			return reviewadapter.CodexReviewCommittedProfile(reviewadapter.CodexReviewConfig{}), nil
		}
		return reviewadapter.CodexReviewProfile(reviewadapter.CodexReviewConfig{}), nil
	}
	u.lane.d.UnitCheck.Fallback = func(mode string) (reviewadapter.Profile, error) {
		*fallbacks = append(*fallbacks, mode)
		if mode == UnitCheckCommitted {
			return reviewadapter.BugbotCommittedProfile(), nil
		}
		return reviewadapter.BugbotProfile(), nil
	}
}

// profileNames are the profile names of the requests the checker saw, in order.
func profileNames(got []reviewadapter.Request) []string {
	var names []string
	for _, r := range got {
		names = append(names, r.Profile.Name)
	}
	return names
}

// TestUnitCheckFallsBackToBugbotOnCodexQuota (CRW-1517): a Codex review that gave no judgment for its quota is asked once more of the
// fallback profile (Bugbot) with the same request. The one completed row keeps the fallback call's record as its result, the Codex
// record as the answer's fallback, and names the fallback in its data (fallback_from, fallback_state, fallback_model); a fallback that
// judged holds nothing and is proven. With no fallback wired a quota holds the node as before; a fallback that ends quota again holds
// unit_check_quota with both records kept; no state other than quota falls back.
func TestUnitCheckFallsBackToBugbotOnCodexQuota(t *testing.T) {
	t.Run("fallback judges", func(t *testing.T) {
		u := newUCWorld(t)
		var fallbacks []string
		codexWithBugbotFallback(u, &fallbacks)
		u.checker.replies = []checkReply{{res: codexRecord(reviewadapter.StateQuota, "")}, {res: checkRecord(reviewadapter.StateOK, reviewadapter.VerdictVerified)}}
		ex := u.build("c_ex1", "built\n")
		out := decodeCheck(t, u.must("unitCheck", "A", ex, map[string]any{"round": 1}))
		if names := profileNames(u.checker.got); !slices.Equal(names, []string{"codex-review", "bugbot"}) {
			t.Fatalf("the checker saw profiles %v, want codex-review then bugbot", names)
		}
		if a, b := u.checker.got[0], u.checker.got[1]; a.Prompt != b.Prompt || a.Worktree != b.Worktree || a.GuideRev != b.GuideRev || a.Limit != b.Limit ||
			a.Base != b.Base || a.Head != b.Head || !bytes.Equal(a.Schema, b.Schema) {
			t.Errorf("the fallback's request %+v differs from the first %+v", b, a)
		}
		if !slices.Equal(fallbacks, []string{UnitCheckUncommitted}) {
			t.Errorf("fallback modes %v", fallbacks)
		}
		if out.State != reviewadapter.StateOK || out.Verdict != reviewadapter.VerdictVerified || out.Hold != "" || out.Retried || out.Model != "grok-4.7-medium" ||
			out.Fallback == nil || out.Fallback.State != reviewadapter.StateQuota || out.Fallback.Model != "gpt-6-astra" || out.Fallback.EngineFamily != "gpt" {
			t.Errorf("answer %+v (fallback %+v)", out, out.Fallback)
		}
		row := u.lastCheck()
		want := map[string]any{"role": "unit-check", "state": "ok", "verdict": "verified", "retried": false, "fallback_from": "codex-review", "fallback_state": "quota",
			"fallback_model": "gpt-6-astra", "tree": "fp"}
		for k, v := range want {
			if row.Data[k] != v {
				t.Errorf("row %s = %#v, want %#v", k, row.Data[k], v)
			}
		}
		for _, k := range []string{"hold", "first_state", "fallback_retried"} {
			if _, has := row.Data[k]; has {
				t.Errorf("the row has %s: %v", k, row.Data[k])
			}
		}
		var rec reviewadapter.Result
		if err := json.Unmarshal(resultOf(row), &rec); err != nil || rec.State != reviewadapter.StateOK || rec.EngineFamily != "grok" || rec.Model != "grok-4.7-medium" {
			t.Errorf("the row's result %+v (%v) is not the Bugbot record", rec, err)
		}
		if got := decodeCheck(t, resultOf(row)); got.Fallback == nil || !reflect.DeepEqual(*got.Fallback, codexRecord(reviewadapter.StateQuota, "")) {
			t.Errorf("the row's fallback record %+v, want the Codex quota record", got.Fallback)
		}
		if len(u.checkRows()) != 1 || !u.proven(row) {
			t.Errorf("%d rows; proven %v", len(u.checkRows()), u.proven(row))
		}
	})

	t.Run("no fallback wired", func(t *testing.T) {
		u := newUCWorld(t)
		var fallbacks []string
		codexWithBugbotFallback(u, &fallbacks)
		u.lane.d.UnitCheck.Fallback = nil
		u.checker.replies = []checkReply{{res: codexRecord(reviewadapter.StateQuota, "")}, {res: checkRecord(reviewadapter.StateOK, reviewadapter.VerdictVerified)}}
		ex := u.build("c_ex1", "built\n")
		out := decodeCheck(t, u.must("unitCheck", "A", ex, map[string]any{"round": 1}))
		row := u.lastCheck()
		if len(u.checker.got) != 1 || out.State != reviewadapter.StateQuota || out.Hold != "unit_check_quota" || out.Fallback != nil || dataStr(row, "hold") != "unit_check_quota" ||
			u.proven(row) {
			t.Errorf("%d checker calls, answer %+v, row %v", len(u.checker.got), out, row.Data)
		}
		if _, has := row.Data["fallback_from"]; has {
			t.Errorf("a check with no fallback names one: %v", row.Data)
		}
	})

	t.Run("fallback quota too", func(t *testing.T) {
		u := newUCWorld(t)
		var fallbacks []string
		codexWithBugbotFallback(u, &fallbacks)
		u.checker.replies = []checkReply{{res: codexRecord(reviewadapter.StateQuota, "")}, {res: checkRecord(reviewadapter.StateQuota, "")}, {res: checkRecord(reviewadapter.StateOK, "verified")}}
		ex := u.build("c_ex1", "built\n")
		out := decodeCheck(t, u.must("unitCheck", "A", ex, map[string]any{"round": 1}))
		row := u.lastCheck()
		if names := profileNames(u.checker.got); !slices.Equal(names, []string{"codex-review", "bugbot"}) {
			t.Errorf("the checker saw profiles %v, want one fallback only", names)
		}
		if out.State != reviewadapter.StateQuota || out.Hold != "unit_check_quota" || out.Model != "grok-4.7-medium" || out.Fallback == nil || out.Fallback.Model != "gpt-6-astra" ||
			dataStr(row, "hold") != "unit_check_quota" || dataStr(row, "fallback_from") != "codex-review" || dataStr(row, "fallback_state") != "quota" || u.proven(row) {
			t.Errorf("answer %+v, row %v", out, row.Data)
		}
	})

	t.Run("an invalid answer, then quota", func(t *testing.T) {
		u := newUCWorld(t)
		var fallbacks []string
		codexWithBugbotFallback(u, &fallbacks)
		u.checker.replies = []checkReply{{res: codexRecord(reviewadapter.StateInvalid, "")}, {res: codexRecord(reviewadapter.StateQuota, "")}, {res: checkRecord(reviewadapter.StateOK, "verified")}}
		ex := u.build("c_ex1", "built\n")
		out := decodeCheck(t, u.must("unitCheck", "A", ex, map[string]any{"round": 1}))
		row := u.lastCheck()
		if names := profileNames(u.checker.got); !slices.Equal(names, []string{"codex-review", "codex-review", "bugbot"}) {
			t.Errorf("the checker saw profiles %v", names)
		}
		// retried and first_state are about the calls of the profile that judged; the Codex retry is fallback_retried
		if out.State != reviewadapter.StateOK || out.Retried || row.Data["retried"] != false || row.Data["fallback_retried"] != true || dataStr(row, "first_state") != "" ||
			dataStr(row, "fallback_state") != "quota" || out.Fallback == nil || out.Fallback.State != reviewadapter.StateQuota {
			t.Errorf("answer %+v, row %v", out, row.Data)
		}
	})

	for _, state := range []reviewadapter.State{reviewadapter.StateNoAnswer, reviewadapter.StateToolFailed} {
		t.Run("no fallback on "+string(state), func(t *testing.T) {
			u := newUCWorld(t)
			var fallbacks []string
			codexWithBugbotFallback(u, &fallbacks)
			u.checker.replies = []checkReply{{res: codexRecord(state, "")}, {res: checkRecord(reviewadapter.StateOK, reviewadapter.VerdictVerified)}}
			ex := u.build("c_ex1", "built\n")
			out := decodeCheck(t, u.must("unitCheck", "A", ex, map[string]any{"round": 1}))
			row := u.lastCheck()
			if len(u.checker.got) != 1 || len(fallbacks) != 0 || out.State != state || out.Fallback != nil {
				t.Errorf("%d checker calls, fallback asked %v, answer %+v", len(u.checker.got), fallbacks, out)
			}
			if _, has := row.Data["fallback_from"]; has {
				t.Errorf("the row names a fallback: %v", row.Data)
			}
		})
	}

	t.Run("a fallback that cannot be had", func(t *testing.T) {
		u := newUCWorld(t)
		var fallbacks []string
		codexWithBugbotFallback(u, &fallbacks)
		u.lane.d.UnitCheck.Fallback = func(string) (reviewadapter.Profile, error) {
			return reviewadapter.Profile{}, errors.New("no fallback for this mode")
		}
		u.checker.replies = []checkReply{{res: codexRecord(reviewadapter.StateQuota, "")}}
		ex := u.build("c_ex1", "built\n")
		if _, err := u.op("unitCheck", "A", ex, nil); err == nil || !strings.Contains(err.Error(), "no fallback for this mode") {
			t.Errorf("a fallback profile error: %v", err)
		}
		if n := len(u.checkRows()); n != 0 {
			t.Errorf("%d crw.unitCheck rows after the error", n)
		}
	})
}

// TestUnitCheckRecordsCodexFindingsOnAPlantedBug (CRW-1517, the bundle's end condition): a unit check through the real review adapter
// and the Codex review profile, on a worktree whose uncommitted change plants a bug, records the review's findings: Codex names the
// file by its absolute path under the worktree, and the row's result names it relative to the worktree, with the review's grades, the
// verdict needs_changes and the Codex model. The tool is given the bundle's argv and no prompt.
func TestUnitCheckRecordsCodexFindingsOnAPlantedBug(t *testing.T) {
	u := newUCWorld(t)
	u.lane.d.UnitCheck.Profile = func(string) (reviewadapter.Profile, error) {
		return reviewadapter.CodexReviewProfile(reviewadapter.CodexReviewConfig{}), nil
	}
	write(t, u.wt, "calc.go", "package calc\n\n// Sum adds a and b.\nfunc Sum(a, b int) int {\n\treturn a - b\n}\n", 0o644)
	var argvs [][]string
	u.lane.d.UnitCheck.Checker = reviewadapter.Adapter{
		Run: func(_ context.Context, p reviewadapter.Profile, req reviewadapter.Request) ([]byte, []byte, int, error) {
			args, err := p.Args(req)
			if err != nil {
				return nil, nil, 1, err
			}
			argvs = append(argvs, append(slices.Clone(p.Argv), args...))
			file := filepath.Join(req.Worktree, "calc.go")
			review := "The change breaks Sum.\n\nFull review comments:\n\n" +
				"- [P1] Sum subtracts instead of adding — " + file + ":5-5\n  Sum returns a - b, so every caller gets the difference.\n\n" +
				"- [P3] Name the operands — " + file + ":4\n  a and b say nothing.\n"
			return []byte(review), nil, 0, nil
		},
		Fingerprint: func(ctx context.Context, dir string) (string, error) { return Snapshot(ctx, ExecGit("git"), dir) },
	}
	ex := u.build("c_ex1", "built\n")
	pending := u.snapshot()
	out := decodeCheck(t, u.must("unitCheck", "A", ex, map[string]any{"round": 1}))
	row := u.lastCheck()
	var rec reviewadapter.Result
	if err := json.Unmarshal(resultOf(row), &rec); err != nil {
		t.Fatalf("the row's result: %v", err)
	}
	if rec.State != reviewadapter.StateOK || rec.Verdict != reviewadapter.VerdictNeedsChanges || len(rec.Findings) != 2 || rec.Model != "gpt-6-astra" || rec.EngineFamily != "gpt" ||
		rec.Effort != "high" || rec.PromptSent || rec.FingerprintBefore != pending {
		t.Fatalf("the row's result %+v", rec)
	}
	for i, want := range []struct {
		sev  string
		line int
	}{{"P1", 5}, {"P3", 4}} {
		f := rec.Findings[i]
		if f.File != "calc.go" || f.Severity != want.sev || f.Line != want.line || f.ID != reviewadapter.FindingID("calc.go", f.Text) {
			t.Errorf("finding %d %+v, want %s at calc.go:%d", i, f, want.sev, want.line)
		}
	}
	if !strings.HasPrefix(rec.Findings[0].Text, "Sum subtracts instead of adding: ") {
		t.Errorf("finding text %q", rec.Findings[0].Text)
	}
	if !reflect.DeepEqual(rec, out.Result) || dataStr(row, "verdict") != reviewadapter.VerdictNeedsChanges || row.Data["findings"] != float64(2) || dataStr(row, "tree") != pending {
		t.Errorf("answer %+v row %v", out, row.Data)
	}
	wantArgv := []string{"codex", "review", "--uncommitted", "-c", `model="gpt-6-astra"`, "-c", `model_reasoning_effort="high"`, "-c", `sandbox_mode="read-only"`}
	if len(argvs) != 1 || !slices.Equal(argvs[0], wantArgv) {
		t.Errorf("the tool's argv %q, want %q", argvs, wantArgv)
	}
}

// TestCommittedUnitCheckGivesTheBaseToTheProfile (CRW-1517): a committed check gives the profile the range's base (opts.base) with the
// request, which the Codex committed profile passes as --base; a Codex quota there falls back to the committed Bugbot profile with the
// same range.
func TestCommittedUnitCheckGivesTheBaseToTheProfile(t *testing.T) {
	u := newUCWorld(t)
	ex := u.build("c_ex1", "built\n")
	u.must("unitCheck", "A", ex, map[string]any{"round": 1})
	var commit CommitOutcome
	_ = json.Unmarshal(u.must("commit", "A", ex, []string{"a.txt"}), &commit)
	if commit.Commit == "" {
		t.Fatal("no commit")
	}

	var fallbacks []string
	codexWithBugbotFallback(u, &fallbacks)
	var argvs [][]string
	u.lane.d.UnitCheck.Checker = reviewadapter.Adapter{
		Run: func(_ context.Context, p reviewadapter.Profile, req reviewadapter.Request) ([]byte, []byte, int, error) {
			args, err := p.Args(req)
			if err != nil {
				return nil, nil, 1, err
			}
			argvs = append(argvs, append(slices.Clone(p.Argv), args...))
			return []byte("No issues were found in the change.\n"), nil, 0, nil
		},
		Fingerprint: func(ctx context.Context, dir string) (string, error) { return Snapshot(ctx, ExecGit("git"), dir) },
	}
	out := decodeCheck(t, u.must("unitCheck", "A", nil, map[string]any{"commit": commit, "base": u.start, "final": true}))
	if out.State != reviewadapter.StateOK || out.Verdict != reviewadapter.VerdictVerified || out.Fallback != nil {
		t.Errorf("committed Codex check: answer %+v", out)
	}
	if len(argvs) != 1 || !slices.Equal(argvs[0][len(argvs[0])-2:], []string{"--base", u.start}) || slices.Contains(argvs[0], "--uncommitted") {
		t.Errorf("the committed Codex argv %q, want --base %s at its end", argvs, u.start)
	}

	// a Codex quota on the committed check falls back to the committed Bugbot profile, with the same range
	u.checker.replies = []checkReply{{res: codexRecord(reviewadapter.StateQuota, "")}, {res: checkRecord(reviewadapter.StateOK, reviewadapter.VerdictVerified)}}
	u.checker.got = nil
	u.lane.d.UnitCheck.Checker = u.checker
	out = decodeCheck(t, u.must("unitCheck", "A", nil, map[string]any{"commit": commit, "base": u.start}))
	row := u.lastCheck()
	if names := profileNames(u.checker.got); !slices.Equal(names, []string{"codex-review-committed", "bugbot-committed"}) {
		t.Fatalf("the checker saw profiles %v", names)
	}
	for i, r := range u.checker.got {
		if r.Base != u.start || r.Head != commit.Commit {
			t.Errorf("request %d's range %s..%s, want %s..%s", i, r.Base, r.Head, u.start, commit.Commit)
		}
	}
	if !slices.Equal(fallbacks, []string{UnitCheckCommitted}) || out.State != reviewadapter.StateOK || out.Fallback == nil ||
		dataStr(row, "fallback_from") != "codex-review-committed" || dataStr(row, "base") != u.start || dataStr(row, "commit") != commit.Commit {
		t.Errorf("fallback modes %v, answer %+v, row %v", fallbacks, out, row.Data)
	}
}

// TestUnitCheckErrorWritesNoRow (CRW-1463): a Go error of the checker (a bad request, a fingerprint that could not be taken, the end of
// the context) is the op's error and leaves no row; so is a lane with no unit checker wired, a profile that cannot be had for the mode,
// and an uncommitted check with no judged call or no pending tree.
func TestUnitCheckErrorWritesNoRow(t *testing.T) {
	u := newUCWorld(t)
	ex := u.build("c_ex1", "built\n")
	u.checker.replies = []checkReply{{err: errors.New("reviewadapter: fingerprint before the call: boom")}}
	if _, err := u.op("unitCheck", "A", ex, map[string]any{"round": 1}); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Errorf("a checker error: %v", err)
	}
	// the invalid answer's retry that fails is the op's error too
	u.checker.replies = []checkReply{{res: checkRecord(reviewadapter.StateInvalid, "")}, {err: context.Canceled}}
	u.checker.got = nil
	if _, err := u.op("unitCheck", "A", ex, nil); !errors.Is(err, context.Canceled) || len(u.checker.got) != 2 {
		t.Errorf("a retry that failed: %v after %d calls", err, len(u.checker.got))
	}
	u.checker.replies = nil
	u.checker.got = nil
	if _, err := u.op("unitCheck", "A", nil, map[string]any{"round": 1}); err == nil || !strings.Contains(err.Error(), "call") {
		t.Errorf("an uncommitted check with no call: %v", err)
	}
	if _, err := u.op("unitCheck", "A", call("c_none", "completed", "claude-x", nil), nil); err == nil || !strings.Contains(err.Error(), "pending tree") {
		t.Errorf("a call with no pending tree: %v", err)
	}
	profile := u.lane.d.UnitCheck.Profile
	u.lane.d.UnitCheck.Profile = func(string) (reviewadapter.Profile, error) {
		return reviewadapter.Profile{}, errors.New("committed mode is not wired")
	}
	if _, err := u.op("unitCheck", "A", ex, nil); err == nil || !strings.Contains(err.Error(), "not wired") {
		t.Errorf("a profile that cannot be had: %v", err)
	}
	u.lane.d.UnitCheck.Profile = profile
	if len(u.checker.got) != 0 {
		t.Errorf("the checker was asked %d times", len(u.checker.got))
	}
	u.lane.d.UnitCheck = nil
	if _, err := u.op("unitCheck", "A", ex, nil); err == nil || !strings.Contains(err.Error(), "no unit checker is wired") {
		t.Errorf("no unit checker: %v", err)
	}
	if n := len(u.checkRows()); n != 0 {
		t.Errorf("%d crw.unitCheck rows after errors", n)
	}
}

// TestUnitCheckRefusesAMovedTree (CRW-1463): when the worktree is no longer the pending tree of the judged call, the tool is not run:
// the row records state tree_moved with both trees and no role (so neither reader takes it), it is proven, and crw.accept of the commit
// of that pending tree is refused reviewer_missing.
func TestUnitCheckRefusesAMovedTree(t *testing.T) {
	u := newUCWorld(t)
	ex := u.build("c_ex1", "built\n")
	pending := u.snapshot()
	write(t, u.wt, "src/c.txt", "an edit after the call\n", 0o644)
	moved := u.snapshot()
	out := decodeCheck(t, u.must("unitCheck", "A", ex, map[string]any{"round": 1}))
	if len(u.checker.got) != 0 {
		t.Fatalf("the checker was asked %d times on a moved tree", len(u.checker.got))
	}
	if out.State != StateTreeMoved || out.Hold != "" {
		t.Errorf("answer %+v", out)
	}
	row := u.lastCheck()
	if dataStr(row, "state") != StateTreeMoved || row.Data["tree_moved"] != true || dataStr(row, "pending_tree") != pending || dataStr(row, "tree") != moved ||
		dataStr(row, "node") != "A" || dataStr(row, "call") != "c_ex1" {
		t.Errorf("row %v", row.Data)
	}
	if _, has := row.Data["role"]; has {
		t.Errorf("a tree_moved row names a role: %v", row.Data["role"])
	}
	if !u.proven(row) {
		t.Error("a tree_moved row is not proven")
	}
	write(t, u.wt, "src/c.txt", "gamma\n", 0o644)
	var commit CommitOutcome
	_ = json.Unmarshal(u.must("commit", "A", ex, []string{"a.txt"}), &commit)
	u.must("verify", "A", commit.Commit)
	if _, err := u.op("accept", "A", commit.Commit); err == nil || ReasonOf(err.Error()) != "reviewer_missing" {
		t.Errorf("crw.accept after a tree_moved check: %v, want reviewer_missing", err)
	}
}

// TestUnitCheckRefusesAMovedHead (CRW-1463): an executor call that committed its own work (its completed row records
// executor_head_moved and HEAD is that stray commit) leaves no uncommitted change to review, while crw.commit commits the whole pending
// tree on the recorded start: the tool is not run, the row says head_moved with no role, and crw.accept of that commit is refused.
func TestUnitCheckRefusesAMovedHead(t *testing.T) {
	u := newUCWorld(t)
	write(t, u.wt, "a.txt", "built\n", 0o644)
	gitIn(t, u.wt, "add", "-A")
	gitIn(t, u.wt, "-c", "commit.gpgsign=false", "commit", "-q", "-m", "the subagent committed")
	stray := gitIn(t, u.wt, "rev-parse", "HEAD")
	u.j.Append(engine.Event{RunID: "run-1", Kind: engine.KindCall, CallID: "c_ex1", Op: "agent", Event: engine.EvPrepared, Data: map[string]any{"role": "executor", "node": "A",
		"allocation": map[string]any{"kind": engine.PlaceFresh, "path": u.wt, "start_commit": u.start, "attempt": 1}}})
	u.j.Append(engine.Event{RunID: "run-1", Kind: engine.KindCall, CallID: "c_ex1", Op: "agent", Event: engine.EvCompleted, Data: map[string]any{"pending_tree": u.snapshot(),
		"executor_head_moved": map[string]any{"start": u.start, "head": stray, "ref": engine.StrayRef("run-1", "A", 1, "c_ex1")}}})
	ex := call("c_ex1", "completed", "claude-x", nil)
	out := decodeCheck(t, u.must("unitCheck", "A", ex, map[string]any{"round": 1}))
	if len(u.checker.got) != 0 || out.State != StateHeadMoved {
		t.Fatalf("%d checker calls, answer %+v", len(u.checker.got), out)
	}
	row := u.lastCheck()
	moved, _ := row.Data["head_moved"].(map[string]any)
	if dataStr(row, "state") != StateHeadMoved || str(moved, "start") != u.start || str(moved, "head") != stray || dataStr(row, "role") != "" || !u.proven(row) {
		t.Errorf("row %v", row.Data)
	}
	var commit CommitOutcome
	_ = json.Unmarshal(u.must("commit", "A", ex, []string{"a.txt"}), &commit)
	u.must("verify", "A", commit.Commit)
	if _, err := u.op("accept", "A", commit.Commit); err == nil || ReasonOf(err.Error()) != "reviewer_missing" {
		t.Errorf("crw.accept after a head_moved check: %v, want reviewer_missing", err)
	}
}

// TestUnitCheckRefusesAHeadTheExecutorCommittedWithoutARecord (CRW-1463, DI 11c): a backend that records no executor_head_moved (the
// App Server's) still has the worktree's HEAD read against the commit the node's attempt is at: the start of its placement, or the
// node's last commit since. An executor that committed its own work leaves an uncommitted tree with nothing in it to judge, so the tool
// is not run and the row says head_moved with no role; crw.accept of the commit made on it is refused. A HEAD at the attempt's last
// recorded commit is the start of a fix and is checked.
func TestUnitCheckRefusesAHeadTheExecutorCommittedWithoutARecord(t *testing.T) {
	u := newUCWorld(t)
	u.j.Append(engine.Event{RunID: "run-1", Kind: engine.KindCall, CallID: "c_ex1", Op: "agent", Event: engine.EvPrepared, Data: map[string]any{"role": "executor", "node": "A",
		"allocation": map[string]any{"kind": engine.PlaceFresh, "path": u.wt, "start_commit": u.start, "attempt": 1}}})
	write(t, u.wt, "a.txt", "built\n", 0o644)
	gitIn(t, u.wt, "add", "-A")
	gitIn(t, u.wt, "-c", "commit.gpgsign=false", "commit", "-q", "-m", "the executor committed its own work")
	own := gitIn(t, u.wt, "rev-parse", "HEAD")
	u.j.Append(engine.Event{RunID: "run-1", Kind: engine.KindCall, CallID: "c_ex1", Op: "agent", Event: engine.EvCompleted, Data: map[string]any{"pending_tree": u.snapshot()}})
	ex := call("c_ex1", "completed", "claude-x", nil)
	out := decodeCheck(t, u.must("unitCheck", "A", ex, map[string]any{"round": 1}))
	if len(u.checker.got) != 0 || out.State != StateHeadMoved {
		t.Fatalf("%d checker calls, answer %+v: a check of an executor that committed its own work", len(u.checker.got), out)
	}
	row := u.lastCheck()
	moved, _ := row.Data["head_moved"].(map[string]any)
	if dataStr(row, "state") != StateHeadMoved || str(moved, "start") != u.start || str(moved, "head") != own || dataStr(row, "role") != "" || !u.proven(row) {
		t.Errorf("row %v", row.Data)
	}
	var commit CommitOutcome
	_ = json.Unmarshal(u.must("commit", "A", ex, []string{"a.txt"}), &commit)
	u.must("verify", "A", commit.Commit)
	if _, err := u.op("accept", "A", commit.Commit); err == nil || ReasonOf(err.Error()) != "reviewer_missing" {
		t.Errorf("crw.accept after a head_moved check: %v, want reviewer_missing", err)
	}
	// a fix that starts at the attempt's last recorded commit (the commit just made) is at the right HEAD and is checked
	u.j.Append(engine.Event{RunID: "run-1", Kind: engine.KindCall, CallID: "c_fix", Op: "agent", Event: engine.EvPrepared, Data: map[string]any{"role": "executor", "node": "A",
		"allocation": map[string]any{"kind": engine.PlaceContinued}}})
	write(t, u.wt, "a.txt", "fixed\n", 0o644)
	u.j.Append(engine.Event{RunID: "run-1", Kind: engine.KindCall, CallID: "c_fix", Op: "agent", Event: engine.EvCompleted, Data: map[string]any{"pending_tree": u.snapshot()}})
	u.checkOnTree(u.snapshot())
	if out := decodeCheck(t, u.must("unitCheck", "A", call("c_fix", "completed", "claude-x", nil), map[string]any{"round": 2})); out.State != reviewadapter.StateOK || len(u.checker.got) != 1 {
		t.Errorf("the check of a fix at the last recorded commit: %+v, %d checker calls", out, len(u.checker.got))
	}
}

// TestUnitCheckReadsTheAcceptedReceiptWhenItsCopyIsMissing (CRW-1463, x1436 item 7): an accepted receipt the backend could not copy to
// the step bus (copy_error, no copy) is read where the hook accepted it (its path) when that file still hashes to the receipt's digest:
// the check runs with it, and the row records the receipt with its path and the copy error. When the receipt cannot be read there
// either the node holds receipt_not_copied, not proven; the check run again once the file can be read (a resume) runs the tool, so the
// hold clears.
func TestUnitCheckReadsTheAcceptedReceiptWhenItsCopyIsMissing(t *testing.T) {
	body := "RED-FIRST: go test -run TestA ./a: FAIL TestA\n"
	t.Run("the original is read", func(t *testing.T) {
		u := newUCWorld(t)
		orig, digest := receiptCopy(t, body)
		ex := u.buildWithReceipt("c_ex1", "built\n", map[string]any{"assignment": "as1", "state": "accepted", "path": orig, "sha256": digest, "agent_id": "agent-1",
			"copy_error": "the step bus is full"})
		u.checkOnTree(u.snapshot())
		out := decodeCheck(t, u.must("unitCheck", "A", ex, map[string]any{"round": 1}))
		if out.Hold != "" || out.State != reviewadapter.StateOK || len(u.checker.got) != 1 {
			t.Fatalf("answer %+v, %d checker calls: the receipt the hook accepted is readable", out, len(u.checker.got))
		}
		if p := u.checker.got[0].Prompt; !strings.Contains(p, "EXECUTOR RECEIPT") || !strings.Contains(p, body) {
			t.Errorf("the prompt does not carry the receipt:\n%s", p)
		}
		row := u.lastCheck()
		rc := checkReceiptOf(row)
		if str(rc, "state") != "accepted" || str(rc, "sha256") != digest || str(rc, "path") != orig || str(rc, "copy_error") != "the step bus is full" || str(rc, "copy") != "" {
			t.Errorf("the row's receipt %v", rc)
		}
		if dataStr(row, "role") != "unit-check" || !u.proven(row) {
			t.Errorf("row %v", row.Data)
		}
	})
	t.Run("a hold that clears on a resume", func(t *testing.T) {
		u := newUCWorld(t)
		orig := filepath.Join(t.TempDir(), "receipt.md")
		sum := sha256.Sum256([]byte(body))
		digest := hex.EncodeToString(sum[:])
		ex := u.buildWithReceipt("c_ex1", "built\n", map[string]any{"assignment": "as1", "state": "accepted", "path": orig, "sha256": digest, "copy_error": "the step bus is full"})
		out := decodeCheck(t, u.must("unitCheck", "A", ex, map[string]any{"round": 1}))
		if out.Hold != "receipt_not_copied" || len(u.checker.got) != 0 || u.proven(u.lastCheck()) {
			t.Fatalf("answer %+v, %d checker calls: neither the copy nor the receipt can be read", out, len(u.checker.got))
		}
		if err := os.WriteFile(orig, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		u.checkOnTree(u.snapshot())
		out = decodeCheck(t, u.must("unitCheck", "A", ex, map[string]any{"round": 1}))
		if out.Hold != "" || out.State != reviewadapter.StateOK || len(u.checker.got) != 1 {
			t.Errorf("the check run again once the receipt can be read: %+v, %d checker calls", out, len(u.checker.got))
		}
		// the bytes that are there must still be the ones the hook accepted
		if err := os.WriteFile(orig, []byte("changed since\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if out := decodeCheck(t, u.must("unitCheck", "A", ex, map[string]any{"round": 1})); out.Hold != "receipt_unverified" || len(u.checker.got) != 1 {
			t.Errorf("a receipt whose bytes changed since: %+v", out)
		}
	})
}

// TestAcceptCarriesTheCarriedFindingsIntoThePremergeRecord (CRW-1465, x1436 item 4): the node lane carries a non-blocking finding past
// its last round trip, and the head then gets a fix (of a failed verification) whose check judges only the fix and finds nothing. The
// acceptance rests on that last check, and the carried ids the lane passes (crw.accept's opts.carried) are read from the node's recorded
// checks into the premerge record's defects (each with the check that carried it): without them the record would say nothing of the
// finding. A finding the named check did not record is an error, never a defect made up.
func TestAcceptCarriesTheCarriedFindingsIntoThePremergeRecord(t *testing.T) {
	u := newUCWorld(t)
	minor := reviewadapter.Finding{Severity: "P3", Impact: premerge.MinorSeparable, File: "a.txt", Line: 3, Text: "a name could be clearer"}
	minor.ID = reviewadapter.FindingID(minor.File, minor.Text)
	ex := u.build("c_ex1", "built\n")
	first := checkRecord(reviewadapter.StateOK, reviewadapter.VerdictVerified, minor)
	first.FingerprintBefore, first.FingerprintAfter = u.snapshot(), u.snapshot()
	u.checker.replies = []checkReply{{res: first}}
	u.must("unitCheck", "A", ex, map[string]any{"round": 3, "final": true})
	carriedBy := u.lastCheck().CallID
	var c1 CommitOutcome
	_ = json.Unmarshal(u.must("commit", "A", ex, []string{"a.txt"}), &c1)
	fix := u.build("c_fix", "built and fixed\n")
	u.checkOnTree(u.snapshot())
	u.must("unitCheck", "A", fix, map[string]any{"round": 4, "final": true})
	var c2 CommitOutcome
	_ = json.Unmarshal(u.must("commit", "A", fix, []string{"a.txt"}), &c2)
	u.must("verify", "A", c2.Commit)
	capture := &acceptCapture{stubRelay: u.relay}
	u.lane.d.Relay = capture
	if _, err := u.op("accept", "A", c2.Commit, map[string]any{"carried": []map[string]any{{"id": minor.ID, "check": carriedBy}}}); err != nil {
		t.Fatalf("crw.accept with the carried finding: %v", err)
	}
	if len(capture.got) != 1 {
		t.Fatalf("accept requests %+v", capture.got)
	}
	rec, err := premerge.Decode(capture.got[0].Premerge)
	if err != nil {
		t.Fatal(err)
	}
	var carried *premerge.Defect
	for i := range rec.Defects {
		if rec.Defects[i].ID == minor.ID {
			carried = &rec.Defects[i]
		}
	}
	if carried == nil || carried.Severity != "P3" || carried.Impact != premerge.MinorSeparable || !strings.Contains(carried.What, minor.Text) {
		t.Fatalf("the premerge defects %+v do not hold the carried finding %s", rec.Defects, minor.ID)
	}
	if len(rec.Dispositions.Items) != 0 {
		t.Errorf("a carried finding that blocks nothing asks a disposition: %+v", rec.Dispositions.Items)
	}
	for _, c := range []map[string]any{{"id": "f_no_such_finding", "check": carriedBy}, {"id": minor.ID, "check": "c_no_such_check"}} {
		if _, err := u.op("accept", "A", c2.Commit, map[string]any{"carried": []map[string]any{c}}); err == nil || !strings.Contains(err.Error(), "carried finding") {
			t.Errorf("crw.accept with a carried finding %v no check recorded: %v", c, err)
		}
	}
}

// TestAcceptReadsACarriedFindingFromTheCheckThatCarriedIt (CRW-1465, x1436 bugbot r4): a finding id names a file and a text, not a
// grade, so a later check can report the same finding at another severity: here the check that carried it graded it P3, a later check
// of the next round graded it P2 and the fix answered it. The carried finding is the one of the check that carried it (opts.carried
// names that check), a P3 that asks no disposition, not the newest check's P2, which would block an acceptance the last check and the
// verification allow.
func TestAcceptReadsACarriedFindingFromTheCheckThatCarriedIt(t *testing.T) {
	u := newUCWorld(t)
	minor := reviewadapter.Finding{Severity: "P3", Impact: premerge.MinorSeparable, File: "a.txt", Line: 3, Text: "a name could be clearer"}
	minor.ID = reviewadapter.FindingID(minor.File, minor.Text)
	graded := minor
	graded.Severity, graded.Impact = "P2", "core_function"
	ex := u.build("c_ex1", "built\n")
	first := checkRecord(reviewadapter.StateOK, reviewadapter.VerdictVerified, minor)
	first.FingerprintBefore, first.FingerprintAfter = u.snapshot(), u.snapshot()
	u.checker.replies = []checkReply{{res: first}}
	u.must("unitCheck", "A", ex, map[string]any{"round": 3, "final": true})
	carriedBy := u.lastCheck().CallID
	var c1 CommitOutcome
	_ = json.Unmarshal(u.must("commit", "A", ex, []string{"a.txt"}), &c1)
	fix := u.build("c_fix", "built, then graded again\n")
	again := checkRecord(reviewadapter.StateOK, reviewadapter.VerdictNeedsChanges, graded)
	again.FingerprintBefore, again.FingerprintAfter = u.snapshot(), u.snapshot()
	u.checker.replies = []checkReply{{res: again}}
	u.must("unitCheck", "A", fix, map[string]any{"round": 4})
	fixed := u.build("c_fix2", "built and fixed\n")
	u.checkOnTree(u.snapshot())
	u.must("unitCheck", "A", fixed, map[string]any{"round": 5})
	var c2 CommitOutcome
	_ = json.Unmarshal(u.must("commit", "A", fixed, []string{"a.txt"}), &c2)
	u.must("verify", "A", c2.Commit)
	capture := &acceptCapture{stubRelay: u.relay}
	u.lane.d.Relay = capture
	if _, err := u.op("accept", "A", c2.Commit, map[string]any{"carried": []map[string]any{{"id": minor.ID, "check": carriedBy}}}); err != nil {
		t.Fatalf("crw.accept with the finding the check %s carried: %v", carriedBy, err)
	}
	rec, err := premerge.Decode(capture.got[len(capture.got)-1].Premerge)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range rec.Defects {
		if d.ID == minor.ID && d.Severity != "P3" {
			t.Errorf("the carried defect %+v is not the P3 the carrying check graded", d)
		}
	}
	if len(rec.Dispositions.Items) != 0 {
		t.Errorf("dispositions %+v", rec.Dispositions.Items)
	}
}

// ---- the executor receipt of a unit check (CRW-1463, C12) ----

// receiptCopy writes bytes as the step-bus copy of an executor's evidence receipt under a temporary directory and answers its absolute
// path and the hex sha256 the receipt row names.
func receiptCopy(t *testing.T, body string) (path, digest string) {
	t.Helper()
	path = filepath.Join(t.TempDir(), "evidence-as1.md")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(body))
	return path, hex.EncodeToString(sum[:])
}

// buildWithReceipt is build with the evidence_receipt the session backend records on the executor call's completed row.
func (u *ucWorld) buildWithReceipt(callID, body string, receipt map[string]any) CallRef {
	u.t.Helper()
	ex := u.build(callID, body)
	evs := u.j.Events()
	last := evs[len(evs)-1]
	if last.CallID != callID || last.Event != engine.EvCompleted {
		u.t.Fatalf("build did not end with the completed row of %s", callID)
	}
	u.j.Append(engine.Event{RunID: "run-1", Kind: engine.KindCall, CallID: callID, Op: "agent", Event: engine.EvCompleted,
		Data: map[string]any{"pending_tree": dataStr(last, "pending_tree"), "evidence_receipt": receipt}})
	return ex
}

// checkOnTree has the scripted checker answer a clean record that saw tree (the pending tree crw.commit commits), so crw.accept finds
// the check on the commit's tree.
func (u *ucWorld) checkOnTree(tree string) {
	rec := checkRecord(reviewadapter.StateOK, reviewadapter.VerdictVerified)
	rec.FingerprintBefore, rec.FingerprintAfter = tree, tree
	u.checker.replies = []checkReply{{res: rec}}
}

// checkReceiptOf is the receipt a unit-check row records.
func checkReceiptOf(row engine.Event) map[string]any {
	m, _ := row.Data["receipt"].(map[string]any)
	return m
}

// TestUnitCheckCarriesTheExecutorReceiptToTheCheckAndTheAcceptance (CRW-1463, C12): an uncommitted check of a call whose completed row
// records an accepted evidence receipt with its step-bus copy reads the copy, puts its bytes in the prompt (EXECUTOR RECEIPT), and records
// the receipt (state, sha256, copy, ignored) on its row; crw.accept on that row names the executor receipt in the premerge evidence.
func TestUnitCheckCarriesTheExecutorReceiptToTheCheckAndTheAcceptance(t *testing.T) {
	u := newUCWorld(t)
	body := "RED-FIRST: go test -run TestA ./a: FAIL TestA\nMUTATION-RED: a.go:3 flipped: FAIL TestA\n"
	copyPath, digest := receiptCopy(t, body)
	ex := u.buildWithReceipt("c_ex1", "built\n", map[string]any{"assignment": "as1", "state": "accepted", "path": "/p/receipt.md", "sha256": digest,
		"agent_id": "agent-1", "turn_id": "turn-1", "turn_match": true, "copy": copyPath, "ignored": true})
	u.checkOnTree(u.snapshot())
	out := decodeCheck(t, u.must("unitCheck", "A", ex, map[string]any{"round": 1}))
	if out.State != reviewadapter.StateOK || out.Hold != "" {
		t.Fatalf("answer %+v", out)
	}
	if len(u.checker.got) != 1 {
		t.Fatalf("%d checker calls, want 1", len(u.checker.got))
	}
	prompt := u.checker.got[0].Prompt
	if !strings.Contains(prompt, "EXECUTOR RECEIPT") || !strings.Contains(prompt, body) || !strings.Contains(prompt, digest) {
		t.Errorf("the prompt does not carry the executor receipt:\n%s", prompt)
	}
	if i, j := strings.Index(prompt, "Review the uncommitted changes"), strings.Index(prompt, "EXECUTOR RECEIPT"); i != 0 || j < i {
		t.Errorf("the scope sentence is not first, before the receipt:\n%s", prompt)
	}
	row := u.lastCheck()
	rc := checkReceiptOf(row)
	if str(rc, "state") != "accepted" || str(rc, "sha256") != digest || str(rc, "copy") != copyPath || rc["ignored"] != true {
		t.Errorf("the row's receipt %v", rc)
	}
	if dataStr(row, "role") != "unit-check" || !u.proven(row) {
		t.Errorf("row %v", row.Data)
	}
	var commit CommitOutcome
	_ = json.Unmarshal(u.must("commit", "A", ex, []string{"a.txt"}), &commit)
	u.must("verify", "A", commit.Commit)
	capture := &acceptCapture{stubRelay: u.relay}
	u.lane.d.Relay = capture
	if _, err := u.op("accept", "A", commit.Commit); err != nil {
		t.Fatalf("crw.accept on the unit check: %v", err)
	}
	if len(capture.got) != 1 || capture.got[0].ReviewerCall != row.CallID {
		t.Fatalf("accept requests %+v", capture.got)
	}
	rec, err := premerge.Decode(capture.got[0].Premerge)
	if err != nil {
		t.Fatalf("premerge: %v", err)
	}
	if len(rec.Criteria) != 2 {
		t.Fatalf("criteria %+v", rec.Criteria)
	}
	for id, c := range rec.Criteria {
		if !strings.Contains(c.Evidence, "executor receipt "+digest) {
			t.Errorf("criterion %s evidence %q does not name the executor receipt", id, c.Evidence)
		}
	}

	// a receipt longer than the prompt can carry (the tool takes the prompt as one argument) is cut at a line end and names its copy
	long := strings.Repeat("RED-FIRST: go test -run TestA ./a: FAIL TestA, a line of the receipt\n", 4000)
	longPath, longDigest := receiptCopy(t, long)
	snapBefore := u.snapshot()
	u.buildWithReceipt("c_ex_long", "built\n", map[string]any{"assignment": "as2", "state": "accepted", "sha256": longDigest, "copy": longPath})
	if u.snapshot() != snapBefore {
		t.Fatal("the long-receipt call moved the tree")
	}
	u.checkOnTree(snapBefore)
	decodeCheck(t, u.must("unitCheck", "A", call("c_ex_long", "completed", "claude-x", nil), map[string]any{"round": 1}))
	lp := u.checker.got[len(u.checker.got)-1].Prompt
	if len(lp) > UnitCheckReceiptPromptLimit+4096 || !strings.Contains(lp, longPath) || !strings.Contains(lp, fmt.Sprintf("the receipt is %d bytes", len(long))) {
		t.Errorf("a long receipt's prompt is %d bytes, names its copy %v", len(lp), strings.Contains(lp, longPath))
	}
	if rc := checkReceiptOf(u.lastCheck()); str(rc, "state") != "accepted" || str(rc, "sha256") != longDigest {
		t.Errorf("the long receipt's row %v", rc)
	}
}

// TestUnitCheckHoldsWithoutAnAcceptedReceipt (CRW-1463, C12): a receipt the backend recorded as missing or unverified, an accepted one
// that was not copied to the step bus, whose copy cannot be read or whose bytes do not hash to its digest, is no receipt to hand the
// check: the tool is not run, the row carries the node, the call, the receipt as read and hold receipt_*, and no role, adapter record,
// tree or commit; it is not proven, the lane's crw.hold with that reason writes source unit_check, and crw.accept is refused
// reviewer_missing. A call with no evidence_receipt had no assignment: the check runs and records receipt not_assigned. A committed
// check has no executor: it reads no receipt and records receipt no_executor.
func TestUnitCheckHoldsWithoutAnAcceptedReceipt(t *testing.T) {
	cases := []struct {
		name, hold string
		receipt    func(t *testing.T) map[string]any
	}{
		{"missing", "receipt_missing", func(*testing.T) map[string]any {
			return map[string]any{"assignment": "as1", "state": "missing", "detail": "the stop hook never accepted a receipt for the assignment"}
		}},
		{"unverified", "receipt_unverified", func(*testing.T) map[string]any {
			return map[string]any{"assignment": "as1", "state": "unverified", "detail": "the assignment was claimed by agent-2, not by the bound subagent agent-1"}
		}},
		{"copy_error", "receipt_not_copied", func(*testing.T) map[string]any {
			return map[string]any{"assignment": "as1", "state": "accepted", "path": "/p/receipt.md", "sha256": strings.Repeat("e", 64), "agent_id": "agent-1",
				"copy_error": "the step bus is full"}
		}},
		{"unreadable_copy", "receipt_not_copied", func(t *testing.T) map[string]any {
			return map[string]any{"assignment": "as1", "state": "accepted", "sha256": strings.Repeat("e", 64), "copy": filepath.Join(t.TempDir(), "gone.md")}
		}},
		{"digest_mismatch", "receipt_unverified", func(t *testing.T) map[string]any {
			p, _ := receiptCopy(t, "the bytes on the bus\n")
			_, other := receiptCopy(t, "the bytes the hook accepted\n")
			return map[string]any{"assignment": "as1", "state": "accepted", "sha256": other, "copy": p}
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			u := newUCWorld(t)
			receipt := c.receipt(t)
			ex := u.buildWithReceipt("c_ex1", "built\n", receipt)
			out := decodeCheck(t, u.must("unitCheck", "A", ex, map[string]any{"round": 1}))
			if len(u.checker.got) != 0 {
				t.Fatalf("the checker was asked %d times without an accepted receipt", len(u.checker.got))
			}
			if out.Hold != c.hold || out.Call != "c_ex1" {
				t.Errorf("answer %+v, want hold %s", out, c.hold)
			}
			row := u.lastCheck()
			if dataStr(row, "node") != "A" || dataStr(row, "call") != "c_ex1" || dataStr(row, "hold") != c.hold {
				t.Errorf("row %v", row.Data)
			}
			if !reflect.DeepEqual(checkReceiptOf(row), receipt) {
				t.Errorf("the row's receipt %v is not the receipt as read %v", checkReceiptOf(row), receipt)
			}
			for _, k := range []string{"role", "tree", "commit"} {
				if _, has := row.Data[k]; has {
					t.Errorf("a receipt hold row has %s: %v", k, row.Data[k])
				}
			}
			var rec reviewadapter.Result
			if err := json.Unmarshal(resultOf(row), &rec); err != nil || rec.Model != "" || rec.PromptDigest != "" || rec.FingerprintBefore != "" || rec.State == reviewadapter.StateOK {
				t.Errorf("a receipt hold row carries an adapter record: %+v %v", rec, err)
			}
			if u.proven(row) {
				t.Error("a receipt hold is proven: a resume would not look at the receipt again")
			}
			var held HoldOutcome
			_ = json.Unmarshal(u.must("hold", "A", map[string]any{"reason": out.Hold, "call": ex.CallID, "source": "unit_check"}), &held)
			hr, ok := rows(u.j.Events()).opRows(OpHold).last(func(engine.Event) bool { return true })
			if !held.Held || !ok || dataStr(hr, "source") != "unit_check" || dataStr(hr, "reason") != c.hold {
				t.Errorf("hold %+v row %v", held, hr.Data)
			}
			var commit CommitOutcome
			_ = json.Unmarshal(u.must("commit", "A", ex, []string{"a.txt"}), &commit)
			u.must("verify", "A", commit.Commit)
			if _, err := u.op("accept", "A", commit.Commit); err == nil || ReasonOf(err.Error()) != "reviewer_missing" {
				t.Errorf("crw.accept after a receipt hold: %v, want reviewer_missing", err)
			}
		})
	}

	t.Run("not_assigned", func(t *testing.T) {
		u := newUCWorld(t)
		ex := u.build("c_ex1", "built\n")
		u.checkOnTree(u.snapshot())
		out := decodeCheck(t, u.must("unitCheck", "A", ex, map[string]any{"round": 1}))
		row := u.lastCheck()
		if len(u.checker.got) != 1 || out.Hold != "" || dataStr(row, "role") != "unit-check" || !reflect.DeepEqual(checkReceiptOf(row), map[string]any{"state": "not_assigned"}) {
			t.Errorf("%d checker calls, answer %+v, row %v", len(u.checker.got), out, row.Data)
		}
		if strings.Contains(u.checker.got[0].Prompt, "EXECUTOR RECEIPT") {
			t.Errorf("a check with no assignment has a receipt section:\n%s", u.checker.got[0].Prompt)
		}
		// and the acceptance on it names no executor receipt
		var commit CommitOutcome
		_ = json.Unmarshal(u.must("commit", "A", ex, []string{"a.txt"}), &commit)
		u.must("verify", "A", commit.Commit)
		capture := &acceptCapture{stubRelay: u.relay}
		u.lane.d.Relay = capture
		if _, err := u.op("accept", "A", commit.Commit); err != nil || len(capture.got) != 1 {
			t.Fatalf("crw.accept on a check with no assignment: %v %+v", err, capture.got)
		}
		rec, err := premerge.Decode(capture.got[0].Premerge)
		if err != nil {
			t.Fatal(err)
		}
		for id, c := range rec.Criteria {
			if strings.Contains(c.Evidence, "executor receipt") {
				t.Errorf("criterion %s evidence %q names a receipt the check had none of", id, c.Evidence)
			}
		}
	})

	t.Run("committed", func(t *testing.T) {
		u := newUCWorld(t)
		// the call's receipt would hold an uncommitted check; a committed check judges a range with no executor and reads none
		ex := u.buildWithReceipt("c_ex1", "built\n", map[string]any{"assignment": "as1", "state": "missing"})
		var commit CommitOutcome
		_ = json.Unmarshal(u.must("commit", "A", ex, []string{"a.txt"}), &commit)
		out := decodeCheck(t, u.must("unitCheck", "A", nil, map[string]any{"commit": commit, "base": u.start, "final": true}))
		row := u.lastCheck()
		if len(u.checker.got) != 1 || out.Hold != "" || out.State != reviewadapter.StateOK || dataStr(row, "role") != "unit-check" ||
			!reflect.DeepEqual(checkReceiptOf(row), map[string]any{"state": "no_executor"}) {
			t.Errorf("%d checker calls, answer %+v, row %v", len(u.checker.got), out, row.Data)
		}
		if strings.Contains(u.checker.got[0].Prompt, "EXECUTOR RECEIPT") {
			t.Errorf("a committed check has a receipt section:\n%s", u.checker.got[0].Prompt)
		}
	})
}
