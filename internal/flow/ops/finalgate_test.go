package ops

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/script"
)

// The tests of the final gate of a milestone close (CRW-1206, F13). They run the real close - crw:lib/close over the operations of
// the lane - on the engine with the fake backend, a journal in memory and a relay whose integration target the test moves: what
// the close judges, what it makes, which calls it makes and which it does not.

const (
	headOne = "1111111111111111111111111111111111111111"
	headTwo = "2222222222222222222222222222222222222222"
)

// closeRelay is the relay of a close: one milestone and an integration target that a test moves.
type closeRelay struct {
	*stubRelay
	mu sync.Mutex
	ms Milestone
}

func (r *closeRelay) Milestone(context.Context, string, string) (Milestone, error) { return r.ms, nil }

func (r *closeRelay) IntegrationTip(context.Context, string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.stubRelay.tip, nil
}

func (r *closeRelay) moveTo(tip string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.stubRelay.tip = tip
}

// closeVerifier verifies the heads it is asked for: a pass unless the test says otherwise.
type closeVerifier struct {
	mu          sync.Mutex
	calls       []VerifyRequest
	fail        bool
	unavailable bool
}

func (v *closeVerifier) Verify(_ context.Context, req VerifyRequest) (VerifyOutcome, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.calls = append(v.calls, req)
	if v.unavailable {
		return VerifyOutcome{}, fmt.Errorf("crw-dev is not installed")
	}
	cmd := req.Command
	if cmd == "" {
		cmd = CILocalCommand
	}
	out := VerifyOutcome{Pass: !v.fail, Command: cmd, Digest: "sha256:" + req.Head[:8], Summary: "pass", Failures: []string{}}
	if v.fail {
		out.Summary, out.Failures = "validate/tests: failed", []string{"validate/tests: failed"}
	}
	return out, nil
}

func (v *closeVerifier) requests() []VerifyRequest {
	v.mu.Lock()
	defer v.mu.Unlock()
	return slices.Clone(v.calls)
}

// closeWorld is one close under test.
type closeWorld struct {
	t        *testing.T
	j        *engine.MemoryJournal
	relay    *closeRelay
	verifier *closeVerifier
	fake     *engine.Fake
	decide   *MemoryDecisions
	lane     *Lane
	policy   *engine.Policy

	mu sync.Mutex
	// answers decide what a call says: the key is the kind of the packet (qa-capture, qa-pass-A, qa-pass-B, qa-synthesis, qa-review,
	// final); a missing key is the default answer.
	answers map[string]engine.FakeReply
	asked   []string
	// onAsk runs when a kind of call is asked (a test moves the target while the final review is running).
	onAsk map[string]func()
	// both passes are asked at once: the first one waits for the second for a while, so a run that does them in turn is seen.
	passesTogether bool
	arrived        int
	arrivedCh      chan struct{}
}

func newCloseWorld(t *testing.T, ms Milestone) *closeWorld {
	t.Helper()
	w := &closeWorld{t: t, j: engine.NewMemoryJournal(), fake: engine.NewFake(), decide: &MemoryDecisions{}, policy: engine.FakePolicy(),
		answers: map[string]engine.FakeReply{}, onAsk: map[string]func(){}, arrivedCh: make(chan struct{})}
	if ms.ID == "" {
		ms.ID, ms.Plan, ms.Title, ms.CriteriaDigest = "M1", "plan-1", "milestone one", "crit-digest-1"
		ms.Criteria = []string{"the milestone is done"}
	}
	w.relay = &closeRelay{stubRelay: &stubRelay{tip: headOne}, ms: ms}
	w.verifier = &closeVerifier{}
	w.lane = NewLane(Deps{RunID: "run-1", Journal: w.j, Relay: w.relay, Verifier: w.verifier, Decisions: w.decide, Actor: "parent", Repo: "/repo",
		Now: func() time.Time { return time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC) }})
	w.fake.Handler = w.handle
	return w
}

// kindOf names the call a packet belongs to.
func kindOf(prompt string) string {
	switch {
	case strings.HasPrefix(prompt, "ROLE: executor. QA capture"):
		return "qa-capture"
	case strings.HasPrefix(prompt, "ROLE: explorer. QA pass A"):
		return "qa-pass-A"
	case strings.HasPrefix(prompt, "ROLE: explorer. QA pass B"):
		return "qa-pass-B"
	case strings.HasPrefix(prompt, "The two reviewer passes"):
		return "qa-synthesis"
	case strings.HasPrefix(prompt, "Review your own captures"):
		return "qa-review"
	case strings.HasPrefix(prompt, "ROLE: reviewer-final."):
		return "final"
	}
	return "other"
}

func (w *closeWorld) handle(req engine.FakeRequest) engine.FakeReply {
	kind := kindOf(req.Prompt)
	w.mu.Lock()
	w.asked = append(w.asked, kind)
	hook := w.onAsk[kind]
	reply, scripted := w.answers[kind]
	together := w.passesTogether && strings.HasPrefix(kind, "qa-pass-")
	if together {
		w.arrived++
		if w.arrived == 2 {
			close(w.arrivedCh)
		}
	}
	w.mu.Unlock()
	if hook != nil {
		hook()
	}
	if together {
		select {
		case <-w.arrivedCh:
		case <-time.After(5 * time.Second):
			return engine.FakeReply{Text: `{"not":"in parallel"}`}
		}
	}
	if scripted {
		return reply
	}
	switch kind {
	case "qa-capture":
		return engine.FakeReply{Text: jsonOf(map[string]any{"captures": []any{map[string]any{"path": "/tmp/qa/shot.png", "kind": "screenshot", "shows": "the home screen"}},
			"matrix": []string{"home"}, "teardown": []string{"no resources spawned"}, "summary": "drove it"})}
	case "qa-pass-A":
		return engine.FakeReply{Text: jsonOf(map[string]any{"pass": "A", "verdict": "PASS", "findings": []any{}, "summary": "A reads the captures"})}
	case "qa-pass-B":
		return engine.FakeReply{Text: jsonOf(map[string]any{"pass": "B", "verdict": "PASS", "findings": []any{}, "summary": "B reads the captures"})}
	case "qa-synthesis", "qa-review":
		return engine.FakeReply{Text: jsonOf(map[string]any{"verdict": "PASS", "findings": []any{}, "summary": "receipt"})}
	case "final":
		return engine.FakeReply{Text: jsonOf(map[string]any{"verdict": "verified", "criteria": []any{map[string]any{"id": "c1", "met": true, "evidence": "read the range"}},
			"defects": []any{}, "alignment": map[string]any{"verdict": "ALIGNED", "drift": []any{}}, "summary": "aligned"})}
	}
	return engine.FakeReply{}
}

func jsonOf(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(b)
}

// askedKinds are the calls of the close, in the order they were asked (the architect and the executor of the header are not).
func (w *closeWorld) askedKinds() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	var out []string
	for _, k := range w.asked {
		if k != "other" {
			out = append(out, k)
		}
	}
	return out
}

func (w *closeWorld) count(kind string) int {
	n := 0
	for _, k := range w.askedKinds() {
		if k == kind {
			n++
		}
	}
	return n
}

// the milestone has an architect and an executor of another model family than reviewer-final: the authors it must be independent of
const closeHeader = `import { install } from 'crw:lib/lane'
export const meta = { name: 'close' }
install(crw)
const ms = await crw.milestone('plan-1', 'M1')
await agent.call('ROLE: architect. plan the milestone', { agentType: 'architect', label: 'architect' })
await agent.call('ROLE: executor. build the node', { agentType: 'executor', node: 'A', label: 'build:A' })
`

// run runs the script after the header (the milestone M1 read and the composites installed) and decodes what it returned.
func (w *closeWorld) run(body string) map[string]any {
	w.t.Helper()
	prog, err := script.Load(script.Source{Name: "close.js", Text: []byte(closeHeader + body)})
	if err != nil {
		w.t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	res, err := engine.Run(ctx, prog, engine.Config{Backend: w.fake, Policy: w.policy, Journal: w.j, RunID: "run-1", Ops: w.lane.Ops()})
	if err != nil {
		w.t.Fatal(err)
	}
	if res.Status != engine.RunCompleted {
		w.t.Fatalf("run %s: %s: %s", res.Status, res.ErrorKind, res.Error)
	}
	var out map[string]any
	if err := json.Unmarshal(res.Value, &out); err != nil {
		w.t.Fatalf("value %s: %v", res.Value, err)
	}
	return out
}

func (w *closeWorld) close() map[string]any { return w.run("return await crw.closeMilestone(ms)\n") }

// opRows are the completed rows of an op, in order.
func (w *closeWorld) opRows(op string) []engine.Event { return rows(w.j.Events()).opRows(op) }

func (w *closeWorld) gates() []GateOutcome {
	var out []GateOutcome
	for _, row := range w.opRows(OpGate) {
		var g GateOutcome
		if err := json.Unmarshal(resultOf(row), &g); err != nil {
			w.t.Fatal(err)
		}
		out = append(out, g)
	}
	return out
}

func evidenceOf(g GateOutcome, kind string) Evidence {
	for _, e := range g.Evidence {
		if e.Kind == kind {
			return e
		}
	}
	return Evidence{}
}

// agentRows are the prepared rows of the agent calls, in order.
func (w *closeWorld) agentCalls() []engine.Event {
	return rows(w.j.Events()).prepared()
}

func (w *closeWorld) spawnRules() []string {
	w.decide.mu.Lock()
	defer w.decide.mu.Unlock()
	var out []string
	for _, r := range w.decide.Spawn {
		out = append(out, r.RuleID)
	}
	return out
}

func web(grade string) Milestone {
	return Milestone{ID: "M1", Plan: "plan-1", Title: "milestone one", CriteriaDigest: "crit-digest-1", Criteria: []string{"the screen works"}, Surfaces: []string{"web"}, Grade: grade}
}

// ---- the gate ----

// Evidence that stands on the pinned head is fresh: the gate passes without making any, writes its judgement of each piece to the
// journal, and reviewer-final runs once, with the passed gate as its input.
func TestFinalGateFreshPass(t *testing.T) {
	w := newCloseWorld(t, Milestone{})
	got := w.run(`
const earlier = await crw.verify('', '` + headOne + `', { milestone: 'M1', command: 'crw-dev ci local' })
const closed = await crw.closeMilestone(ms)
return { earlier: earlier.pass, closed }
`)
	closed, _ := got["closed"].(map[string]any)
	if got["earlier"] != true || closed["state"] != "closed" || closed["tip"] != headOne {
		t.Fatalf("%v", got)
	}
	if n := len(w.verifier.requests()); n != 1 {
		t.Errorf("%d verifications: fresh evidence is not made again", n)
	}
	gates := w.gates()
	if len(gates) != 1 {
		t.Fatalf("%d gate rows", len(gates))
	}
	g := gates[0]
	if g.State != GatePass || g.Head != headOne || len(g.Evidence) != 1 || g.Evidence[0].State != EvidenceFresh || !g.Evidence[0].Pass || g.QARequired {
		t.Errorf("gate %+v", g)
	}
	if rowOK := dataBool(w.opRows(OpGate)[0], "ok"); !rowOK {
		t.Errorf("the gate row does not say ok")
	}
	if !GateProof(w.j.Events(), headOne, CILocalCommand, headOne).Proven {
		t.Errorf("the gate row is not a passing record of the head, the command set and the range")
	}
	if n := w.count("final"); n != 1 {
		t.Fatalf("%d reviewer-final calls", n)
	}
	// reviewer-final takes the passed gate as its input: the head and the evidence it stood on are in its packet
	for _, req := range w.fake.Requests() {
		if kindOf(req.Prompt) == "final" {
			for _, want := range []string{"HEAD: " + headOne, "FINAL GATE (passed)", "test crw-dev ci local: fresh pass"} {
				if !strings.Contains(req.Prompt, want) {
					t.Errorf("the reviewer-final packet lacks %q:\n%s", want, req.Prompt)
				}
			}
		}
	}
	if rf := w.opRows(OpReviewerFinal); len(rf) != 1 || !ReviewerFinalProof(w.j.Events(), headOne, "crit-digest-1").Proven {
		t.Errorf("reviewer-final rows %d", len(rf))
	}
}

// QA evidence that is missing for a milestone with a driven surface is made by the QA call, not by the gate: the gate names it,
// the run starts one executor call marked qa, the C2 self-review of that call is the receipt, and the gate is judged again.
func TestFinalGateMissingQARoutesQACall(t *testing.T) {
	w := newCloseWorld(t, web("C2"))
	got := w.close()
	if got["state"] != "closed" {
		t.Fatalf("%v", got)
	}
	gates := w.gates()
	if len(gates) != 2 {
		t.Fatalf("%d gate rows, want the pending one and the one that passed", len(gates))
	}
	first, second := gates[0], gates[1]
	if first.State != GatePending || !first.NeedQA || !slices.Equal(first.NeedTests, []string{CILocalCommand}) || evidenceOf(first, "qa").State != EvidenceMissing || evidenceOf(first, "test").State != EvidenceMissing {
		t.Errorf("first gate %+v", first)
	}
	if second.State != GatePass || evidenceOf(second, "qa").State != EvidenceFresh || evidenceOf(second, "test").State != EvidenceFresh {
		t.Errorf("second gate %+v", second)
	}
	// one executor call marked qa, its self-review on the same thread, nothing else: no oracle pass at C2
	if got := w.askedKinds(); !slices.Equal(got, []string{"qa-capture", "qa-review", "final"}) {
		t.Fatalf("calls %v", got)
	}
	var qa, review engine.Event
	for _, c := range w.agentCalls() {
		switch c.Label {
		case "qa:M1":
			qa = c
		case "qa-review:M1":
			review = c
		}
	}
	if str(qa.Data, "role") != "executor" || qa.Data["qa"] != true || str(review.Data, "resume_of") != qa.CallID || review.Data["qa"] != true {
		t.Errorf("qa call %+v, review %+v", qa.Data, review.Data)
	}
	if th, ok := w.fake.Thread("thr_" + qa.CallID); !ok || !th.QA || th.Role != "executor" || len(th.Turns) != 2 {
		t.Errorf("the thread of the qa call: %+v %v", th, ok)
	}
	if rules := w.spawnRules(); !slices.Equal(rules, []string{RuleQA1, RuleRevF1}) {
		t.Errorf("spawn decisions %v", rules)
	}
	receipts := w.opRows(OpQAReceipt)
	if len(receipts) != 1 || dataStr(receipts[0], "state") != QAPass || dataStr(receipts[0], "head") != headOne {
		t.Errorf("receipt rows %+v", receipts)
	}
}

// A receipt and a verification made for a head are stale for the head after it: the target moves while reviewer-final reads the
// first one, the close finds the target is not the head the gate judged, judges the new head again from nothing, makes the
// evidence at it, and closes over it.
func TestFinalGateStaleQAAfterHeadMove(t *testing.T) {
	w := newCloseWorld(t, web("C2"))
	moved := false
	w.onAsk["final"] = func() {
		w.mu.Lock()
		first := !moved
		moved = true
		w.mu.Unlock()
		if first {
			w.relay.moveTo(headTwo)
		}
	}
	got := w.close()
	if got["state"] != "closed" || got["tip"] != headTwo {
		t.Fatalf("%v", got)
	}
	gates := w.gates()
	if len(gates) != 4 {
		t.Fatalf("%d gate rows: %+v", len(gates), gates)
	}
	// rows 0 and 1 judged the first head; row 2 is the first judgement of the second head, over the evidence of the first
	if gates[1].Head != headOne || gates[1].State != GatePass || gates[2].Head != headTwo || gates[2].State != GatePending {
		t.Fatalf("gate heads and states: %s %s, %s %s", gates[1].Head, gates[1].State, gates[2].Head, gates[2].State)
	}
	for _, kind := range []string{"qa", "test"} {
		e := evidenceOf(gates[2], kind)
		if e.State != EvidenceStale || e.Head != headOne || !strings.Contains(e.Why, "commit") {
			t.Errorf("%s evidence at the second head: %+v", kind, e)
		}
	}
	if gates[3].Head != headTwo || gates[3].State != GatePass {
		t.Errorf("last gate %+v", gates[3])
	}
	if n := w.count("qa-capture"); n != 2 {
		t.Errorf("%d QA capture calls, want one for each head", n)
	}
	if reqs := w.verifier.requests(); len(reqs) != 2 || reqs[0].Head != headOne || reqs[1].Head != headTwo {
		t.Errorf("verifications %+v", reqs)
	}
	closes := w.opRows(OpMilestoneClose)
	if len(closes) != 2 || closes[0].Data["moved"] != true || closes[1].Data["moved"] != false || dataStr(closes[1], "tip") != headTwo {
		t.Errorf("close rows %+v", closes)
	}
	if n := w.count("final"); n != 2 {
		t.Errorf("%d reviewer-final calls, want one for each head", n)
	}
}

// A milestone whose criteria have no surface that is driven needs no QA: the gate lists no QA evidence and the close makes no QA
// call, whatever the grade of the work.
func TestFinalGateNoUISurfaceNeedsNoQA(t *testing.T) {
	for _, surfaces := range [][]string{nil, {"logic"}} {
		w := newCloseWorld(t, Milestone{ID: "M1", Plan: "plan-1", Title: "m", CriteriaDigest: "crit-digest-1", Criteria: []string{"c"}, Surfaces: surfaces, Grade: "C4"})
		got := w.close()
		if got["state"] != "closed" {
			t.Fatalf("%v: %v", surfaces, got)
		}
		for _, g := range w.gates() {
			if g.QARequired || g.NeedQA || evidenceOf(g, "qa").Kind != "" {
				t.Errorf("surfaces %v: gate %+v", surfaces, g)
			}
		}
		if got := w.askedKinds(); !slices.Equal(got, []string{"final"}) {
			t.Errorf("surfaces %v: calls %v", surfaces, got)
		}
		if len(w.opRows(OpQAReceipt)) != 0 {
			t.Errorf("surfaces %v: a QA receipt", surfaces)
		}
	}
}

// The gate never reads missing evidence as a pass and never makes it itself: with nothing recorded it is pending, with a
// verification that cannot run the milestone waits for the verifier, and reviewer-final does not run.
func TestFinalGateNeverVerifiedOnMissing(t *testing.T) {
	w := newCloseWorld(t, Milestone{})
	got := w.run(`const g = await crw.finalGate('M1')
return { g }`)
	g, _ := got["g"].(map[string]any)
	if g["state"] != "pending" || g["need_tests"].([]any)[0] != CILocalCommand {
		t.Fatalf("%v", g)
	}
	if rowOK := dataBool(w.opRows(OpGate)[0], "ok"); rowOK {
		t.Errorf("a gate over missing evidence is ok")
	}
	if n := len(w.verifier.requests()); n != 0 {
		t.Errorf("the gate made a verification itself")
	}
	if GateProof(w.j.Events(), headOne, CILocalCommand, headOne).Proven {
		t.Errorf("a pending gate proves a gate")
	}

	w = newCloseWorld(t, Milestone{})
	w.verifier.unavailable = true
	got = w.close()
	if got["state"] != "stop" || got["source"] != "final_gate" || got["status"] != "held" || got["reason"] != "verify_unavailable" {
		t.Fatalf("%v", got)
	}
	if n := w.count("final"); n != 0 {
		t.Errorf("reviewer-final ran with no verification")
	}
	for _, g := range w.gates() {
		if g.State == GatePass {
			t.Errorf("a gate passed with no verification: %+v", g)
		}
	}
}

// A gate that fails is the end of the close: a verification that ran and failed, a QA receipt that says FAIL or REVISE. Nothing
// reaches reviewer-final, and the stop names the evidence that failed.
func TestReviewerFinalNotRunOnFailedGate(t *testing.T) {
	w := newCloseWorld(t, Milestone{})
	w.verifier.fail = true
	got := w.close()
	if got["state"] != "stop" || got["source"] != "final_gate" || got["status"] != "test_failed" {
		t.Fatalf("failed verification: %v", got)
	}
	if n := w.count("final"); n != 0 || len(w.opRows(OpReviewerFinal)) != 0 {
		t.Errorf("reviewer-final ran over a failed verification")
	}
	if g := w.gates(); len(g) != 2 || g[1].State != GateFailed || g[1].Reason != GateTestFailed || g[1].Evidence[0].State != EvidenceFresh || g[1].Evidence[0].Pass {
		t.Errorf("gates %+v", g)
	}

	for _, verdict := range []string{"REVISE", "FAIL"} {
		w = newCloseWorld(t, web("C2"))
		w.answers["qa-review"] = engine.FakeReply{Text: jsonOf(map[string]any{"verdict": verdict, "findings": []any{map[string]any{"ref": "q1", "severity": "P2", "finding": "the border drifts", "evidence": []string{"shot.png"}}}, "summary": "no"})}
		got = w.close()
		if got["state"] != "stop" || got["source"] != "qa" || got["status"] != "failed" || got["reason"] != GateQAFailed {
			t.Fatalf("%s: %v", verdict, got)
		}
		if n := w.count("final"); n != 0 {
			t.Errorf("%s: reviewer-final ran over a QA that says %s", verdict, verdict)
		}
	}
}

// ---- the oracle passes ----

// For a driven surface at C3 or above the run starts the two oracle passes itself: the QA call (an executor call marked qa)
// captures, both passes (explorer calls, with the explorer pair) read the same capture packet at the same time, and the synthesis
// is the next turn of the QA call's own thread. The receipt names the passes, their pair and the synthesis turn, and the three
// spawns are decisions of SP-QA-1 and SP-QA-2.
func TestQAOracleTwoRunOwnedPasses(t *testing.T) {
	w := newCloseWorld(t, web("C3"))
	w.passesTogether = true
	got := w.close()
	if got["state"] != "closed" {
		t.Fatalf("%v", got)
	}
	kinds := w.askedKinds()
	if len(kinds) != 5 || kinds[0] != "qa-capture" || kinds[3] != "qa-synthesis" || kinds[4] != "final" || !slices.Contains(kinds[1:3], "qa-pass-A") || !slices.Contains(kinds[1:3], "qa-pass-B") {
		t.Fatalf("calls %v", kinds)
	}
	receipts := w.opRows(OpQAReceipt)
	if len(receipts) != 1 {
		t.Fatalf("%d receipts", len(receipts))
	}
	r, ok := qaReceiptOf(receipts[0])
	if !ok || r.State != QAPass || r.Head != headOne || r.Captures != 1 || len(r.Passes) != 2 {
		t.Fatalf("receipt %+v", r)
	}
	explorer := w.policy.Roles["explorer"].Pair
	for i, label := range []string{"A", "B"} {
		p := r.Passes[i]
		if p.Pass != label || p.Verdict != "PASS" || p.Call.Role != "explorer" || p.Call.Model != explorer.Model || p.Call.Effort != explorer.Effort || p.Call.Thread == r.Capture.Thread {
			t.Errorf("pass %s: %+v (explorer pair %+v)", label, p, explorer)
		}
		if th, found := w.fake.Thread(p.Call.Thread); !found || th.QA || th.Role != "explorer" {
			t.Errorf("pass %s thread: %+v", label, th)
		}
	}
	// the QA call is the executor's pair, marked qa, and the synthesis is the second turn of that thread
	executor := w.policy.Roles["executor"].Pair
	if r.Capture.Role != "executor" || r.Capture.Model != executor.Model || r.Synthesis.Thread != r.Capture.Thread || r.Synthesis.Call == r.Capture.Call {
		t.Errorf("capture %+v, synthesis %+v", r.Capture, r.Synthesis)
	}
	if th, found := w.fake.Thread(r.Capture.Thread); !found || !th.QA || len(th.Turns) != 2 {
		t.Errorf("the thread of the QA call: %+v", th)
	}
	if rules := w.spawnRules(); !slices.Equal(rules, []string{RuleQA1, RuleQA2, RuleQA2, RuleRevF1}) {
		t.Errorf("spawn decisions %v", rules)
	}
	// the passes read the same capture packet
	var packets []string
	for _, req := range w.fake.Requests() {
		if strings.HasPrefix(kindOf(req.Prompt), "qa-pass-") {
			packets = append(packets, req.Prompt)
		}
	}
	if len(packets) != 2 || !strings.Contains(packets[0], "/tmp/qa/shot.png") || !strings.Contains(packets[1], "/tmp/qa/shot.png") {
		t.Errorf("pass packets: %q", packets)
	}
}

// A pass that did not complete is not a pass that found nothing: the receipt is INCONCLUSIVE, the synthesis is not asked for, the
// gate does not pass and the close stops with an escalation. reviewer-final does not run.
func TestQAOracleMissingPassInconclusive(t *testing.T) {
	w := newCloseWorld(t, web("C3"))
	w.answers["qa-pass-B"] = engine.FakeReply{Fail: &engine.TurnFailure{CodexErrorInfo: "serverOverloaded", Message: "the host is overloaded"}}
	got := w.close()
	if got["state"] != "stop" || got["source"] != "qa" || got["status"] != "inconclusive" || got["reason"] != GateQAInconclusive {
		t.Fatalf("%v", got)
	}
	if kinds := w.askedKinds(); slices.Contains(kinds, "qa-synthesis") || slices.Contains(kinds, "final") {
		t.Errorf("calls %v: no synthesis over a missing pass, and no final review", kinds)
	}
	receipts := w.opRows(OpQAReceipt)
	if len(receipts) != 1 {
		t.Fatalf("%d receipts", len(receipts))
	}
	r, _ := qaReceiptOf(receipts[0])
	if r.State != QAInconclusive || r.Reason != "pass_B_held" || r.Passes[0].Verdict != "PASS" {
		t.Errorf("receipt %+v", r)
	}
	last := w.gates()[len(w.gates())-1]
	if last.State != GateFailed || last.Reason != GateQAInconclusive || evidenceOf(last, "qa").Verdict != QAInconclusive {
		t.Errorf("gate %+v", last)
	}
}
