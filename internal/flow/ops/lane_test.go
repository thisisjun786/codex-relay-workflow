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
}

func (s *stubRelay) Milestone(context.Context, string, string) (Milestone, error) {
	return Milestone{}, nil
}
func (s *stubRelay) Release(_ context.Context, r ReleaseRequest) (ReleaseResult, error) {
	s.releases = append(s.releases, r)
	return ReleaseResult{Released: true, ExecutionID: "exec-" + r.Node}, nil
}
func (s *stubRelay) ReleaseStands(context.Context, string, string, string) (bool, error) {
	return true, nil
}
func (s *stubRelay) ReleaseChild(context.Context, ChildRequest) (ChildResult, error) {
	return ChildResult{Started: true}, nil
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
	// a second revision is not this build's: the ledger holds r0 only
	if _, err := w.op("designLedger", "M", map[string]any{"revision": "r1", "design": json.RawMessage(`{}`)}); err == nil || !strings.Contains(err.Error(), "F09") {
		t.Errorf("a revision after r0: %v", err)
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
	want := []string{"spawn", "spawn", "spawn", "spawn", "spawn", "spawn", "orchestrator", "orchestrator", "inline", "reuse"}
	if !slices.Equal(got, want) {
		t.Errorf("routes %v, want %v", got, want)
	}
	if plan.Routes[9].SameAs != 0 || plan.Routes[9].Rule != RuleInl2 || plan.Routes[8].Rule != RuleInl1 {
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
		d := map[string]any{"ref": "d", "kind": kind, "severity": severity, "introduced": false, "impact": "", "inPromise": false, "file": "f", "line": 1, "finding": "x", "evidence_question": nil}
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
		return func(_ context.Context, wt, name string, args ...string) (int, []byte, error) {
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
