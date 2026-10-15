package ops

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/reviewadapter"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/roles"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/script"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/acceptance/premerge"
)

// The tests of the final gate of a milestone close (CRW-1206, F13). They run the real close - crw:lib/close over the operations of
// the lane - on the engine with the fake backend, a journal in memory and a relay whose integration target the test moves: what
// the close judges, what it makes, which calls it makes and which it does not. The close's crosscheck (CRW-1473) runs on a test
// crosschecker in checkouts the fake Checkout makes under the world's own absolute run directory (closeWorld.landRoot).

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
	// testPackages is the count of test packages the verifications ran (nil: unknown, CRW-1328)
	testPackages *int
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
	out := VerifyOutcome{Pass: !v.fail, Command: cmd, Digest: "sha256:" + req.Head[:8], Summary: "pass", Failures: []string{}, TestPackagesRun: v.testPackages}
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

	// landRoot is the run's directory of the close (Deps.LandRoot), the test's own; the crosscheck writes its receipts file and makes
	// its checkouts under it. cc is the crosschecker of the close and checkout the fake Checkout of its passes.
	landRoot string
	cc       *closeCrosschecker
	checkout *ccCheckout
}

// closeCrosschecker is the crosschecker of a close under test: it answers each pass (read from the name of its checkout,
// <ms>-r<round>-e<evidence round>-<pass><attempt>) with the scripted answers in order, then a verified answer with no finding.
type closeCrosschecker struct {
	mu      sync.Mutex
	answers map[string][]ccAnswer
	calls   []ccCall
	// onCheck runs when a pass is asked (a test moves the target while the crosscheck is running).
	onCheck func(pass string)
}

var closeCheckoutPass = regexp.MustCompile(`-([AB])\d+(-[0-9a-f]+)?$`)

func (c *closeCrosschecker) script(pass string, answers ...ccAnswer) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.answers[pass] = append(c.answers[pass], answers...)
}

func (c *closeCrosschecker) requests() []ccCall {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.calls)
}

func (c *closeCrosschecker) Check(_ context.Context, req reviewadapter.Request) (reviewadapter.Result, error) {
	pass := "?"
	if m := closeCheckoutPass.FindStringSubmatch(filepath.Base(req.Worktree)); m != nil {
		pass = m[1]
	}
	_, statErr := os.Stat(req.Worktree)
	c.mu.Lock()
	c.calls = append(c.calls, ccCall{Pass: pass, Request: req, Existed: statErr == nil})
	a := ccAnswer{State: reviewadapter.StateOK, Verdict: reviewadapter.VerdictVerified}
	if q := c.answers[pass]; len(q) > 0 {
		a, c.answers[pass] = q[0], q[1:]
	}
	hook := c.onCheck
	c.mu.Unlock()
	if hook != nil {
		hook(pass)
	}
	if a.State == "" {
		a.State = reviewadapter.StateOK
	}
	findings := []reviewadapter.Finding{}
	verdict := ""
	if a.State == reviewadapter.StateOK {
		verdict = a.Verdict
		if verdict == "" {
			verdict = reviewadapter.VerdictVerified
		}
		for _, f := range a.Findings {
			if f.ID == "" {
				f.ID = reviewadapter.FindingID(f.File, f.Text)
			}
			findings = append(findings, f)
		}
	}
	sum := sha256.Sum256([]byte(req.Prompt))
	return reviewadapter.Result{State: a.State, Verdict: verdict, Findings: findings, EngineFamily: profileFamily(req.Profile), Model: req.Profile.Model, Effort: req.Profile.Effort,
		GuideRev: req.GuideRev, PromptDigest: hex.EncodeToString(sum[:]), ElapsedMS: 7}, nil
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
	// the run's directory is the test's own and absolute; the crosscheck's authors are the run's role pairs (FakePolicy: claude)
	w.landRoot = t.TempDir()
	w.cc = &closeCrosschecker{answers: map[string][]ccAnswer{}}
	w.checkout = &ccCheckout{root: w.landRoot}
	authors := map[string]string{}
	for _, role := range []string{"architect", "executor", "executor-t1"} {
		authors[role] = w.policy.Roles[role].Pair.Model
	}
	cc := &Crosscheck{Checker: w.cc, Limit: time.Minute, Authors: authors, Checkout: w.checkout.checkout,
		Profiles: func(runDir string) ([]reviewadapter.Profile, error) {
			return []reviewadapter.Profile{reviewadapter.AstraProfile(filepath.Join(runDir, "crosscheck", "answer.strict.json")), reviewadapter.GrokProfile(reviewadapter.AnswerSchema)}, nil
		}}
	w.lane = NewLane(Deps{RunID: "run-1", Journal: w.j, Relay: w.relay, Verifier: w.verifier, Decisions: w.decide, Actor: "parent", Repo: "/repo",
		LandRoot: w.landRoot, Crosscheck: cc, Now: func() time.Time { return time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC) }})
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

// crosschecks are the passes the close's crosschecker was asked, in order.
func (w *closeWorld) crosschecks() []ccCall { return w.cc.requests() }

// checkClosedUnderTheRunDir checks what a close over the crosscheck leaves under the world's run directory: the receipts file of
// round 1 of M1, and every checkout of a pass made under it (CRW-1473, R4-F2).
func (w *closeWorld) checkClosedUnderTheRunDir() {
	w.t.Helper()
	if _, err := os.Stat(filepath.Join(w.landRoot, "crosscheck", "M1-r1-receipts.md")); err != nil {
		w.t.Errorf("the receipts file of the close is not under the run's directory: %v", err)
	}
	w.checkout.mu.Lock()
	made := slices.Clone(w.checkout.made)
	w.checkout.mu.Unlock()
	if len(made) == 0 {
		w.t.Errorf("no checkout of a pass was made")
	}
	for _, dir := range made {
		if rel, err := filepath.Rel(w.landRoot, dir); err != nil || strings.HasPrefix(rel, "..") {
			w.t.Errorf("the checkout %s is not under the run's directory %s", dir, w.landRoot)
		}
	}
	for _, c := range w.crosschecks() {
		if !strings.HasPrefix(c.Request.Worktree, w.landRoot+string(filepath.Separator)) || !c.Existed {
			w.t.Errorf("a pass ran in %s (existed %v), not in a checkout under %s", c.Request.Worktree, c.Existed, w.landRoot)
		}
	}
}

// the milestone has an architect and an executor of another model family than the crosscheck's engine: the authors it must be
// independent of
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
	return w.runSource(closeHeader + body)
}

// runSource runs a whole script on the world and decodes what it returned.
func (w *closeWorld) runSource(src string) map[string]any {
	w.t.Helper()
	prog, err := script.Load(script.Source{Name: "close.js", Text: []byte(src)})
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

// final is the direct harness of reviewer-final (CRW-1505): the gate of the close (closeGate) and then the reviewer-final call over
// its range (finalReview), both of crw:lib/close, with no milestone close after them. It answers the stop of a gate that did not
// pass, or the result of crw.reviewerFinal. The reviewer-final op and its packet are tested here, whatever the close calls.
func (w *closeWorld) final() map[string]any {
	return w.runSource("import { closeGate, finalReview } from 'crw:lib/close'\n" + closeHeader + `const g = await closeGate(ms)
if (g.state !== 'pass') return g
return await finalReview(ms, g)
`)
}

// finalPackets are the packets of the reviewer-final calls the world was asked, in order.
func (w *closeWorld) finalPackets() []string {
	var out []string
	for _, req := range w.fake.Requests() {
		if kindOf(req.Prompt) == "final" {
			out = append(out, req.Prompt)
		}
	}
	return out
}

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
// journal, and the crosscheck runs once (its two passes), with the passed gate as its input; reviewer-final is not called.
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
	if n := w.count("final"); n != 0 || len(w.opRows(OpReviewerFinal)) != 0 {
		t.Fatalf("%d reviewer-final calls: the close calls the crosscheck", n)
	}
	// the crosscheck takes the passed gate as its input: both passes read the head and the range of the gate
	passes := w.crosschecks()
	if len(passes) != 2 {
		t.Fatalf("%d crosscheck passes, want A and B", len(passes))
	}
	for _, c := range passes {
		for _, want := range []string{"HEAD: " + headOne, "Review the changes between", CrosscheckRule, "EXECUTOR RECEIPTS"} {
			if !strings.Contains(c.Request.Prompt, want) {
				t.Errorf("the crosscheck pass %s lacks %q:\n%s", c.Pass, want, c.Request.Prompt)
			}
		}
	}
	if cc := w.opRows(OpCrosscheck); len(cc) != 1 || !CrosscheckProof(w.j.Events(), "M1", headOne, 1, 0).Proven {
		t.Errorf("crosscheck rows %d", len(cc))
	}
	if v, _ := closed["crosscheck"].(map[string]any); v["verdict"] != "verified" || v["head"] != headOne {
		t.Errorf("the closed result's crosscheck %v", closed["crosscheck"])
	}
	w.checkClosedUnderTheRunDir()
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
	// one executor call marked qa, its self-review on the same thread, nothing else: no oracle pass at C2, and the crosscheck is no call
	if got := w.askedKinds(); !slices.Equal(got, []string{"qa-capture", "qa-review"}) {
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
	if rules := w.spawnRules(); !slices.Equal(rules, []string{RuleQA1}) {
		t.Errorf("spawn decisions %v", rules)
	}
	receipts := w.opRows(OpQAReceipt)
	if len(receipts) != 1 || dataStr(receipts[0], "state") != QAPass || dataStr(receipts[0], "head") != headOne {
		t.Errorf("receipt rows %+v", receipts)
	}
	w.checkClosedUnderTheRunDir()
}

// A receipt and a verification made for a head are stale for the head after it: the target moves while the crosscheck reads the
// first one, the close finds the target is not the head the gate judged, judges the new head again from nothing, makes the
// evidence at it, crosschecks it and closes over it.
func TestFinalGateStaleQAAfterHeadMove(t *testing.T) {
	w := newCloseWorld(t, web("C2"))
	moved := false
	w.cc.onCheck = func(string) {
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
	cc := w.opRows(OpCrosscheck)
	if len(cc) != 2 || dataStr(cc[0], "head") != headOne || dataStr(cc[1], "head") != headTwo {
		t.Errorf("crosscheck rows %+v, want one round for each head", cc)
	}
	if n := len(w.crosschecks()); n != 4 {
		t.Errorf("%d crosscheck passes, want two for each head", n)
	}
	w.checkClosedUnderTheRunDir()
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
		if got := w.askedKinds(); len(got) != 0 {
			t.Errorf("surfaces %v: calls %v", surfaces, got)
		}
		if n := len(w.crosschecks()); n != 2 {
			t.Errorf("surfaces %v: %d crosscheck passes", surfaces, n)
		}
		w.checkClosedUnderTheRunDir()
		if len(w.opRows(OpQAReceipt)) != 0 {
			t.Errorf("surfaces %v: a QA receipt", surfaces)
		}
	}
}

// The gate never reads missing evidence as a pass and never makes it itself: with nothing recorded it is pending, with a
// verification that cannot run the milestone waits for the verifier, and the crosscheck does not run.
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
	if n := len(w.crosschecks()); n != 0 || len(w.opRows(OpCrosscheck)) != 0 {
		t.Errorf("the crosscheck ran with no verification")
	}
	for _, g := range w.gates() {
		if g.State == GatePass {
			t.Errorf("a gate passed with no verification: %+v", g)
		}
	}
}

// TestCrosscheckNotRunOnFailedGate: a gate that fails is the end of the close: a verification that ran and failed, a QA receipt that
// says FAIL or REVISE. Nothing reaches the crosscheck (no crw.crosscheck row, no pass, no checkout, no receipts file), reviewer-final
// is not called either, and the stop names the evidence that failed.
func TestCrosscheckNotRunOnFailedGate(t *testing.T) {
	w := newCloseWorld(t, Milestone{})
	w.verifier.fail = true
	got := w.close()
	if got["state"] != "stop" || got["source"] != "final_gate" || got["status"] != "test_failed" {
		t.Fatalf("failed verification: %v", got)
	}
	w.noCrosscheck("a failed verification")
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
		w.noCrosscheck("a QA that says " + verdict)
	}
}

// noCrosscheck checks that the close made no crosscheck and called no reviewer-final.
func (w *closeWorld) noCrosscheck(over string) {
	w.t.Helper()
	if n := len(w.crosschecks()); n != 0 || len(w.opRows(OpCrosscheck)) != 0 || w.checkout.count() != 0 {
		w.t.Errorf("the crosscheck ran over %s: %d passes", over, n)
	}
	if _, err := os.Stat(filepath.Join(w.landRoot, "crosscheck")); err == nil {
		w.t.Errorf("the crosscheck wrote under the run's directory over %s", over)
	}
	if n := w.count("final"); n != 0 || len(w.opRows(OpReviewerFinal)) != 0 {
		w.t.Errorf("reviewer-final ran over %s", over)
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
	if len(kinds) != 4 || kinds[0] != "qa-capture" || kinds[3] != "qa-synthesis" || !slices.Contains(kinds[1:3], "qa-pass-A") || !slices.Contains(kinds[1:3], "qa-pass-B") {
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
	if rules := w.spawnRules(); !slices.Equal(rules, []string{RuleQA1, RuleQA2, RuleQA2}) {
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
// gate does not pass and the close stops with an escalation. The crosscheck does not run.
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
	w.noCrosscheck("an INCONCLUSIVE QA")
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

// ---- the final review as the authority of the close (CRW-1206 correction) ----

func twoCriteria() Milestone {
	return Milestone{ID: "M1", Plan: "plan-1", Title: "milestone one", CriteriaDigest: "crit-digest-1", Criteria: []string{"first", "second"}}
}

func finalAnswer(verdict string, criteria []map[string]any, defects ...map[string]any) engine.FakeReply {
	cs := []any{}
	for _, c := range criteria {
		cs = append(cs, c)
	}
	ds := []any{}
	for _, d := range defects {
		ds = append(ds, d)
	}
	return engine.FakeReply{Text: jsonOf(map[string]any{"verdict": verdict, "criteria": cs, "defects": ds,
		"alignment": map[string]any{"verdict": "ALIGNED", "drift": []any{}}, "summary": "reviewed"})}
}

func crit(id string, met bool) map[string]any {
	return map[string]any{"id": id, "met": met, "evidence": "read the range"}
}

// A verified final review evaluated every criterion of the milestone once and found each met, and names no defect that blocks: a
// schema-valid answer that says verified over criteria it never evaluated, over a criterion it did not meet, or over a defect of
// P1 is no verdict. It is asked once more, as an answer that is no verdict is, and reviewer-final answers verdict_invalid when the
// second answer is as bad. The test runs on the direct harness final() (CRW-1505), so it judges the reviewer-final op whatever the
// close calls.
func TestReviewerFinalVerifiedMustBeComplete(t *testing.T) {
	p1 := map[string]any{"ref": "d1", "kind": "correctness", "severity": "P1", "introduced": true, "impact": "core_function", "inPromise": true, "file": "a.go", "line": 1, "finding": "broken", "evidence_question": nil}
	for name, c := range map[string]struct {
		answer   engine.FakeReply
		verified bool
	}{
		"no criterion evaluated":    {finalAnswer("verified", nil), false},
		"one criterion left out":    {finalAnswer("verified", []map[string]any{crit("c1", true)}), false},
		"a criterion named twice":   {finalAnswer("verified", []map[string]any{crit("c1", true), crit("c1", true)}), false},
		"a criterion of no plan":    {finalAnswer("verified", []map[string]any{crit("c1", true), crit("c9", true)}), false},
		"a criterion not met":       {finalAnswer("verified", []map[string]any{crit("c1", true), crit("c2", false)}), false},
		"a P1 defect stands":        {finalAnswer("verified", []map[string]any{crit("c1", true), crit("c2", true)}, p1), false},
		"every criterion met":       {finalAnswer("verified", []map[string]any{crit("c1", true), crit("c2", true)}), true},
		"criteria in another order": {finalAnswer("verified", []map[string]any{crit("c2", true), crit("c1", true)}), true},
	} {
		t.Run(name, func(t *testing.T) {
			w := newCloseWorld(t, twoCriteria())
			w.answers["final"] = c.answer
			got := w.final()
			if verdict := got["state"] == StateVerdict; verdict != c.verified {
				t.Fatalf("a verdict = %v, want %v: %v", verdict, c.verified, got)
			}
			rf := w.opRows(OpReviewerFinal)
			if len(rf) == 0 || (dataStr(rf[len(rf)-1], "state") == StateVerdict) != c.verified {
				t.Errorf("the last reviewer-final row is a verdict: want %v, rows %+v", c.verified, rf)
			}
			if c.verified {
				return
			}
			if got["state"] != StateNullModel || got["reason"] != "verdict_invalid" || got["retried"] != true {
				t.Errorf("reviewer-final %v", got)
			}
			if n := w.count("final"); n != 2 {
				t.Errorf("reviewer-final asked %d times, want the call and one retry", n)
			}
		})
	}
}

// reviewer-final judges the integrated range against the decisions of the approved DESIGN, so its packet carries them: each
// decision with its id (the drift names it), what it changes and the nodes that follow it. The DESIGN is the journal's ledger
// row, not a file of the checkout the reviewer reads. The test runs on the direct harness final() (CRW-1505).
func TestReviewerFinalPacketCarriesTheDesignDecisions(t *testing.T) {
	w := newCloseWorld(t, Milestone{})
	design := designOf("A", "B")
	design.Decisions = []Decision{
		{ID: "D1", Evidence: []string{}, Change: "the ledger row is the only source of the approved decisions", Alternatives: []string{}, Assumptions: []string{}},
		{ID: "D7", Evidence: []string{}, Change: "every close judges the head it pinned", Alternatives: []string{}, Assumptions: []string{}},
	}
	design.Issues[0].DecisionRefs = []string{"D1"}
	design.Issues[1].DecisionRefs = []string{"D1", "D7"}
	raw, err := json.Marshal(design)
	if err != nil {
		t.Fatal(err)
	}
	var asAny any
	_ = json.Unmarshal(raw, &asAny)
	w.j.Append(engine.Event{RunID: "run-1", Kind: engine.KindCall, CallID: "c_ledger", Op: OpDesignLedger, Event: engine.EvCompleted,
		Data: map[string]any{"milestone": "M1", "revision": FirstRevision, "digest": "sha256:design", "base_tip": headOne, "design": asAny}})
	if got := w.final(); got["state"] != StateVerdict {
		t.Fatalf("%v", got)
	}
	packets := w.finalPackets()
	if len(packets) != 1 {
		t.Fatalf("reviewer-final asked %d times", len(packets))
	}
	packet := packets[0]
	for _, want := range []string{"DESIGN DECISIONS", "D1", "the ledger row is the only source of the approved decisions", "D7", "every close judges the head it pinned", "nodes A, B", "nodes B"} {
		if !strings.Contains(packet, want) {
			t.Errorf("the reviewer-final packet lacks %q:\n%s", want, packet)
		}
	}
	if g := w.gates(); len(g) == 0 || len(g[len(g)-1].Decisions) != 2 {
		t.Errorf("the gate row names its decisions: %+v", g)
	}
}

// CRW-1505 item 1: reviewer-final is told the rule its check applies. checkFinalVerified refuses verified over a defect that
// premerge.Blocks, so the packet names the closed impact set and says that a defect of P0 to P2 blocks whatever its impact and that
// of the impacts only minor_separable does not block, and the reviewer-final overlay says the same. The check is pinned with the
// text: verified over a P3 core_function defect is no verdict (asked once more, then verdict_invalid), and verified over a P3
// minor_separable defect is a verdict.
func TestReviewerFinalPacketSaysTheRuleTheCheckApplies(t *testing.T) {
	const rule = "a defect of P0 to P2 blocks whatever its impact, and of the impacts only minor_separable does not block"
	p3 := func(impact string) map[string]any {
		return map[string]any{"ref": "d1", "kind": "correctness", "severity": "P3", "introduced": true, "impact": impact, "inPromise": true, "file": "a.go", "line": 1, "finding": "a nit", "evidence_question": nil}
	}
	met := []map[string]any{crit("c1", true), crit("c2", true)}
	for name, c := range map[string]struct {
		answer engine.FakeReply
		state  string
		reason string
		asked  int
	}{
		"verified over a P3 core_function defect":   {finalAnswer("verified", met, p3("core_function")), StateNullModel, "verdict_invalid", 2},
		"verified over a P3 minor_separable defect": {finalAnswer("verified", met, p3(premerge.MinorSeparable)), StateVerdict, "", 1},
	} {
		t.Run(name, func(t *testing.T) {
			w := newCloseWorld(t, twoCriteria())
			w.answers["final"] = c.answer
			got := w.final()
			if got["state"] != c.state || got["reason"] != c.reason {
				t.Fatalf("reviewer-final %v, want state %s reason %q", got, c.state, c.reason)
			}
			packets := w.finalPackets()
			if len(packets) != c.asked {
				t.Fatalf("reviewer-final asked %d times, want %d", len(packets), c.asked)
			}
			for _, packet := range packets {
				for _, impact := range premerge.Impacts {
					if !strings.Contains(packet, impact) {
						t.Errorf("the reviewer-final packet does not name the impact %s:\n%s", impact, packet)
					}
				}
				for _, want := range []string{rule, "verified over a defect that blocks is refused"} {
					if !strings.Contains(packet, want) {
						t.Errorf("the reviewer-final packet does not say %q:\n%s", want, packet)
					}
				}
			}
		})
	}
	overlay, err := roles.Overlay("reviewer-final", roles.Options{})
	if err != nil {
		t.Fatal(err)
	}
	for _, impact := range premerge.Impacts {
		if !strings.Contains(overlay, impact) {
			t.Errorf("the reviewer-final overlay does not name the impact %s:\n%s", impact, overlay)
		}
	}
	for _, want := range []string{"A defect of P0 to P2 blocks whatever its impact, and of the impacts only minor_separable does not block", "verified over a defect that blocks is refused"} {
		if !strings.Contains(overlay, want) {
			t.Errorf("the reviewer-final overlay does not say %q:\n%s", want, overlay)
		}
	}
}

// ---- the crosscheck as the authority of the close (CRW-1473) ----

// TestCloseFollowsTheCrosscheckRule is the close part of TestCrosscheckRuleIsOneTableForPacketOpAndClose (workflows/packets_test.go):
// for each severity P0-P3 crossed with every impact and none, a close whose crosscheck answers that one finding (the verdict word
// following premerge.Blocks, as the adapter holds the engine's word to it) closes exactly when the finding does not block, which is
// exactly when V says verified. A finding that blocks keeps the milestone open: the close stops crosscheck_pending with ask
// crosscheck_pending, V in its evidence and the fix packet naming the finding, and no crw.milestoneClose row is written; a finding that
// does not block is carried in V as a follow-up. Every close leaves the receipts file and its checkouts under the world's run
// directory.
func TestCloseFollowsTheCrosscheckRule(t *testing.T) {
	for _, sev := range []string{"P0", "P1", "P2", "P3"} {
		for _, impact := range append(slices.Clone(premerge.Impacts), "") {
			blocks := premerge.Blocks(sev, impact)
			t.Run(sev+"/"+impact, func(t *testing.T) {
				w := newCloseWorld(t, Milestone{})
				verdict := reviewadapter.VerdictVerified
				if blocks {
					verdict = reviewadapter.VerdictNeedsChanges
				}
				f := reviewadapter.Finding{Severity: sev, Impact: impact, File: "a.go", Line: 3, Text: "the finding of " + sev + "/" + impact}
				w.cc.script("A", ccAnswer{Verdict: verdict, Findings: []reviewadapter.Finding{f}})
				got := w.close()
				rows := w.opRows(OpCrosscheck)
				if len(rows) != 1 {
					t.Fatalf("%d crosscheck rows", len(rows))
				}
				// V as the op answered it (the row's result; the memory journal keeps the row's data as the op built it)
				var x struct {
					Verdict map[string]any `json:"verdict"`
				}
				if err := json.Unmarshal(resultOf(rows[0]), &x); err != nil {
					t.Fatal(err)
				}
				v := x.Verdict
				if verified := v["verdict"] == reviewadapter.VerdictVerified; verified == blocks {
					t.Fatalf("V says %v over a finding that blocks: %v; the row %v", v["verdict"], blocks, rows[0].Data)
				}
				closes := w.opRows(OpMilestoneClose)
				if blocks {
					ev, _ := got["evidence"].(map[string]any)
					fix, _ := got["fix"].(string)
					id := reviewadapter.FindingID(f.File, f.Text)
					if got["state"] != "stop" || got["source"] != "crosscheck" || got["status"] != "pending" || got["reason"] != "crosscheck_pending" || got["ask"] != "crosscheck_pending" ||
						!jsonEqual(t, ev["crosscheck"], v) || !strings.Contains(fix, id) || len(closes) != 0 {
						t.Errorf("a blocking finding: the close %v, %d close rows", got, len(closes))
					}
				} else {
					carried, _ := v["carried"].([]any)
					if got["state"] != "closed" || len(closes) != 1 || len(carried) != 1 || !jsonEqual(t, got["crosscheck"], v) {
						t.Errorf("a finding that does not block: the close %v, %d close rows, carried %v", got, len(closes), carried)
					}
				}
				w.checkClosedUnderTheRunDir()
			})
		}
	}
}
