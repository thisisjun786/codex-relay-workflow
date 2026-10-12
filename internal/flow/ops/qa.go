package ops

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/source"
)

// This file is the QA evidence of a milestone's final gate (CRW-1206, F13): the answers of the three calls the run makes for it and
// the record the run keeps of them (crw.qaReceipt).
//
// The QA capture call (SP-QA-1) is one executor call in a scratch worktree at the pinned head. It drives the surface, captures
// artifacts and judges nothing. For a C2 milestone the same call's self-review, one more turn on its thread, is the receipt. For a
// milestone of C3 and above the run starts the two oracle passes itself (SP-QA-2, read-only explorer calls over the same
// captures: Pass A the design and functional integrity, Pass B the visual fidelity and CJK precision), and the synthesis is one
// more turn on the thread of the QA call. Why the run starts the passes: the crw-qa skill (section 5) asks for two read-only
// reviews in parallel, and a native thread of the flow may spawn one explorer per call (SP-NAT-1); a call the run owns satisfies
// both rules, and the QA call is bound to spawn none (nativeAllowance 0).

// The QA states a receipt can be in. INCONCLUSIVE is a QA that did not reach a verdict: a call that did not complete, an answer
// that is no verdict, a pass that is missing. It is never a PASS, and the gate does not pass on it.
const (
	QAPass         = "PASS"
	QARevise       = "REVISE"
	QAFail         = "FAIL"
	QAInconclusive = "INCONCLUSIVE"
)

// OpQAReceipt is the Op of the rows crw.qaReceipt writes: milestone, head, state, grade.
const OpQAReceipt = "crw.qaReceipt"

// QAFinding is one finding of a QA pass or receipt.
type QAFinding struct {
	Ref      string   `json:"ref"`
	Severity string   `json:"severity"`
	Finding  string   `json:"finding"`
	Evidence []string `json:"evidence"`
}

// QACaptureItem is one capture of the QA call: where it is, what kind of artifact it is and what it shows.
type QACaptureItem struct {
	Path  string `json:"path"`
	Kind  string `json:"kind"`
	Shows string `json:"shows"`
}

// QACapture is the answer of the QA capture call (QA_CAPTURE).
type QACapture struct {
	Captures []QACaptureItem `json:"captures"`
	Matrix   []string        `json:"matrix"`
	Teardown []string        `json:"teardown"`
	Summary  string          `json:"summary"`
}

// QAPassAnswer is the answer of one oracle pass (QA_PASS).
type QAPassAnswer struct {
	Pass     string      `json:"pass"`
	Verdict  string      `json:"verdict"`
	Findings []QAFinding `json:"findings"`
	Summary  string      `json:"summary"`
}

// QAReceiptAnswer is the answer of the turn that gives the verdict (QA_RECEIPT): the self-review of a C2 milestone or the synthesis
// of the two passes.
type QAReceiptAnswer struct {
	Verdict  string      `json:"verdict"`
	Findings []QAFinding `json:"findings"`
	Summary  string      `json:"summary"`
}

// checkQA holds the rule the JSON Schema cannot say: a PASS stands over no finding of P0 or P1.
func checkQA(what, verdict string, findings []QAFinding) error {
	if verdict != QAPass {
		return nil
	}
	for _, f := range findings {
		if f.Severity == "P0" || f.Severity == "P1" {
			return fmt.Errorf("%s: PASS, and finding %q is %s", what, f.Ref, f.Severity)
		}
	}
	return nil
}

// DecodeQACapture reads a QA_CAPTURE.
func DecodeQACapture(raw []byte) (QACapture, error) { return decode[QACapture](SchemaQACapture, raw) }

// DecodeQAPass reads a QA_PASS.
func DecodeQAPass(raw []byte) (QAPassAnswer, error) {
	p, err := decode[QAPassAnswer](SchemaQAPass, raw)
	if err != nil {
		return p, err
	}
	return p, checkQA("QA_PASS", p.Verdict, p.Findings)
}

// DecodeQAReceipt reads a QA_RECEIPT.
func DecodeQAReceipt(raw []byte) (QAReceiptAnswer, error) {
	r, err := decode[QAReceiptAnswer](SchemaQAReceipt, raw)
	if err != nil {
		return r, err
	}
	return r, checkQA("QA_RECEIPT", r.Verdict, r.Findings)
}

// GradeAtLeast says whether a work grade (C0 to C4) is at least n. A grade the plan did not give, or one that is no grade, is
// read as the stricter reading: the oracle passes are asked for, not skipped.
func GradeAtLeast(grade string, n int) bool {
	g, ok := strings.CutPrefix(strings.ToUpper(strings.TrimSpace(grade)), "C")
	if !ok {
		return true
	}
	level, err := strconv.Atoi(g)
	if err != nil {
		return true
	}
	return level >= n
}

// qaOrder ranks the verdicts: a receipt is as bad as the worst verdict it rests on.
var qaOrder = map[string]int{QAPass: 0, QARevise: 1, QAFail: 2}

func worseQA(a, b string) string {
	if qaOrder[b] > qaOrder[a] {
		return b
	}
	return a
}

// ---- crw.qaReceipt ----

// qaReceiptOp is crw.qaReceipt(milestone, gate, capture, passes, receipt): the record of the QA evidence of a final gate. It
// judges what the run's calls answered and writes the receipt the gate reads (finalgate.go): the state, the head it stands on,
// and for each call its role, its pair as the thread read it back and its thread. It decides nothing a model says: a call that did
// not complete, an answer the schema or a rule refuses, a capture list that is empty, a pass that is not a read-only explorer
// call, a synthesis that is not a continuation of the QA call - each makes the receipt INCONCLUSIVE, and an INCONCLUSIVE
// receipt is never a pass.
type qaReceiptOp struct{ l *Lane }

// GateRef is the part of a gate outcome a later call of the close names: the head it judged and the grade of the work.
type GateRef struct {
	Head   string `json:"head"`
	Grade  string `json:"grade"`
	Passes bool   `json:"qa_passes"`
}

// QACallRecord is what the receipt keeps of one call.
type QACallRecord struct {
	Call   string `json:"call"`
	Thread string `json:"thread"`
	Status string `json:"status"`
	Reason string `json:"reason"`
	Role   string `json:"role"`
	Model  string `json:"model"`
	Effort string `json:"effort"`
}

func qaCallRecord(c CallRef) QACallRecord {
	return QACallRecord{Call: c.CallID, Thread: c.Meta.ThreadID, Status: c.Status, Reason: c.Reason, Role: c.Meta.Role, Model: c.Meta.ConfiguredPair.Model, Effort: c.Meta.ConfiguredPair.Effort}
}

// QAPassRecord is one oracle pass in a receipt.
type QAPassRecord struct {
	Pass    string       `json:"pass"`
	Call    QACallRecord `json:"call"`
	Verdict string       `json:"verdict"`
}

// QAResult is the answer of crw.qaReceipt.
type QAResult struct {
	State     string         `json:"state"`
	Reason    string         `json:"reason"`
	Milestone string         `json:"milestone"`
	Head      string         `json:"head"`
	Grade     string         `json:"grade"`
	Captures  int            `json:"captures"`
	Capture   QACallRecord   `json:"capture"`
	Passes    []QAPassRecord `json:"passes"`
	// Synthesis is the turn on the thread of the QA call that gave the verdict: the self-review (C2) or the synthesis of the passes.
	Synthesis QACallRecord `json:"synthesis"`
	Findings  []QAFinding  `json:"findings"`
	Summary   string       `json:"summary"`
	// Source is the identity of the tree the capture call ended in: what the receipt rests on. It is the clean head of the gate when
	// the receipt is a PASS.
	Source *source.Identity `json:"source"`
}

func (o *qaReceiptOp) Run(_ context.Context, req engine.OpRequest) (engine.OpResult, error) {
	var ms string
	var gate GateRef
	var capture, receipt CallRef
	var passes []CallRef
	if err := args(req.Args, &ms, &gate, &capture, &passes, &receipt); err != nil {
		return engine.OpResult{}, fmt.Errorf("crw.qaReceipt(milestone, gate, capture, passes, receipt): %w", err)
	}
	if ms == "" || gate.Head == "" || capture.CallID == "" {
		return engine.OpResult{}, fmt.Errorf("crw.qaReceipt: a milestone, the head the gate judged and the QA call are required")
	}
	res := QAResult{State: QAInconclusive, Milestone: ms, Head: gate.Head, Grade: gate.Grade, Capture: qaCallRecord(capture), Passes: []QAPassRecord{}, Findings: []QAFinding{}}
	inconclusive := func(reason string) engine.OpResult {
		res.State, res.Reason = QAInconclusive, reason
		return o.done(res)
	}
	if receipt.CallID != "" {
		res.Synthesis = qaCallRecord(receipt)
	}
	view := captureView(rows(req.Journal.Events()), gate, capture)
	res.Captures = len(view.Captures)
	if view.Source != nil {
		res.Source = view.Source
	}
	if !view.OK {
		return inconclusive(view.Reason), nil
	}
	verdict := QAPass
	if gate.Passes {
		if len(passes) != 2 {
			return inconclusive("passes_missing"), nil
		}
		for i, p := range passes {
			label := string('A' + rune(i))
			rec := QAPassRecord{Pass: label, Call: qaCallRecord(p)}
			res.Passes = append(res.Passes, rec)
			if p.Status != "completed" {
				return inconclusive("pass_" + label + "_" + p.Status), nil
			}
			if p.Meta.Role != "explorer" {
				return inconclusive("pass_" + label + "_not_explorer"), nil
			}
			ans, err := DecodeQAPass(p.Output)
			if err != nil || ans.Pass != label {
				return inconclusive("pass_" + label + "_invalid"), nil
			}
			res.Passes[i].Verdict = ans.Verdict
			verdict = worseQA(verdict, ans.Verdict)
			res.Findings = append(res.Findings, ans.Findings...)
		}
	} else if len(passes) != 0 {
		return inconclusive("passes_not_asked"), nil
	}
	if receipt.CallID == "" {
		return inconclusive("synthesis_missing"), nil
	}
	if receipt.Status != "completed" {
		return inconclusive("synthesis_" + receipt.Status), nil
	}
	// the next turn of the QA call's thread: the journal says the call continues the QA call. After a resume the continuation runs on
	// a fork of the thread (its history through the QA call's last turn), so the thread id is not the test.
	if !continues(rows(req.Journal.Events()), receipt.CallID, capture.CallID) {
		return inconclusive("synthesis_not_continuation"), nil
	}
	// a turn of the QA thread that edited the tree it works in did not judge the head the gate pinned either
	if src, ok := callSource(rows(req.Journal.Events()), receipt.CallID); ok && source.Compare(src, source.CommitIdentity(gate.Head, nil)).Kind != source.ComparisonSame {
		return inconclusive("synthesis_source_modified"), nil
	}
	ans, err := DecodeQAReceipt(receipt.Output)
	if err != nil {
		return inconclusive("synthesis_invalid"), nil
	}
	res.State, res.Summary = worseQA(verdict, ans.Verdict), ans.Summary
	res.Findings = append(res.Findings, ans.Findings...)
	return o.done(res), nil
}

func (o *qaReceiptOp) done(res QAResult) engine.OpResult {
	return engine.OpResult{Result: result(res), Data: map[string]any{
		"milestone": res.Milestone, "head": res.Head, "grade": res.Grade, "state": res.State, "reason": res.Reason, "captures": res.Captures,
		"capture_call": res.Capture.Call, "synthesis_call": res.Synthesis.Call, "passes": len(res.Passes), "source": res.Source,
	}}
}

func (o *qaReceiptOp) Proven(ctx context.Context, ev []engine.Event, row engine.Event) (bool, string, error) {
	return provenAlways(ctx, ev, row)
}

// continues says whether the journal records the call as a continuation of the other (agent.call with resume: its handle).
func continues(r rows, call, of string) bool {
	prep, ok := r.prepared().last(func(ev engine.Event) bool { return ev.CallID == call })
	return ok && str(prep.Data, "resume_of") == of
}

// qaReceiptOf decodes the receipt a completed crw.qaReceipt row holds.
func qaReceiptOf(row engine.Event) (QAResult, bool) {
	var r QAResult
	if err := json.Unmarshal(resultOf(row), &r); err != nil {
		return QAResult{}, false
	}
	return r, true
}

// ---- crw.qaCapture ----

// qaCaptureOp is crw.qaCapture(milestone, gate, capture): the capture of the QA call as the later calls of the close may rely on it.
// The capture call ran in a scratch worktree of its own, and its artifacts are files there; the passes and the self-review read
// them from checkouts of their own. The answer is the list of captures with the path of each resolved against the directory the
// capture ran in, and the identity of the tree the call exercised. A capture that cannot be handed on - not completed, not a
// QA_CAPTURE, none listed, a path that cannot be resolved or that leaves the worktree, a tree that is not the head the gate pinned -
// is not ok, with the reason the receipt will give; the close then asks nothing more of the QA call.
type qaCaptureOp struct{ l *Lane }

// QACaptureEntry is one capture with the path it can be read at.
type QACaptureEntry struct {
	Path  string `json:"path"`
	Abs   string `json:"abs"`
	Kind  string `json:"kind"`
	Shows string `json:"shows"`
}

// QACaptureView is the answer of crw.qaCapture.
type QACaptureView struct {
	OK     bool   `json:"ok"`
	Reason string `json:"reason"`
	// Root is the working directory of the capture call.
	Root     string           `json:"root"`
	Captures []QACaptureEntry `json:"captures"`
	Matrix   []string         `json:"matrix"`
	Teardown []string         `json:"teardown"`
	// Source is the identity of the tree the call ended in (nil when none was recorded).
	Source *source.Identity `json:"source"`
}

func (o *qaCaptureOp) Run(_ context.Context, req engine.OpRequest) (engine.OpResult, error) {
	var ms string
	var gate GateRef
	var capture CallRef
	if err := args(req.Args, &ms, &gate, &capture); err != nil {
		return engine.OpResult{}, fmt.Errorf("crw.qaCapture(milestone, gate, capture): %w", err)
	}
	if ms == "" || gate.Head == "" || capture.CallID == "" {
		return engine.OpResult{}, fmt.Errorf("crw.qaCapture: a milestone, the head the gate judged and the QA call are required")
	}
	v := captureView(rows(req.Journal.Events()), gate, capture)
	return engine.OpResult{Result: result(v), Data: map[string]any{
		"milestone": ms, "head": gate.Head, "capture_call": capture.CallID, "ok": v.OK, "reason": v.Reason, "captures": len(v.Captures), "root": v.Root,
	}}, nil
}

func (o *qaCaptureOp) Proven(ctx context.Context, ev []engine.Event, row engine.Event) (bool, string, error) {
	return provenAlways(ctx, ev, row)
}

// captureView judges the capture call and resolves what it listed.
func captureView(r rows, gate GateRef, capture CallRef) QACaptureView {
	v := QACaptureView{Captures: []QACaptureEntry{}, Matrix: []string{}, Teardown: []string{}}
	fail := func(reason string) QACaptureView {
		v.OK, v.Reason = false, reason
		return v
	}
	if capture.Status != "completed" {
		return fail("capture_" + capture.Status)
	}
	captured, err := DecodeQACapture(capture.Output)
	if err != nil {
		return fail("capture_invalid")
	}
	if len(captured.Captures) == 0 {
		// a PASS points at a non-empty artifact the call captured this round (the crw-qa skill, section 1): a capture that is not
		// listed does not exist for the gate
		return fail("no_captures")
	}
	v.Matrix, v.Teardown = nonNil(captured.Matrix), nonNil(captured.Teardown)
	v.Root = callRoot(r, capture.CallID)
	// what the call exercised is the tree it ended in; the gate pinned the clean head
	src, ok := callSource(r, capture.CallID)
	if !ok {
		return fail("capture_source_unknown")
	}
	v.Source = &src
	switch source.Compare(src, source.CommitIdentity(gate.Head, nil)).Kind {
	case source.ComparisonUnavailable:
		return fail("capture_source_unknown")
	case source.ComparisonDifferent:
		return fail("capture_source_modified")
	}
	for _, c := range captured.Captures {
		abs := c.Path
		if !filepath.IsAbs(abs) {
			if v.Root == "" {
				return fail("capture_root_unknown")
			}
			abs = filepath.Join(v.Root, c.Path)
			if rel, err := filepath.Rel(v.Root, abs); err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				return fail("capture_path_outside")
			}
		}
		v.Captures = append(v.Captures, QACaptureEntry{Path: c.Path, Abs: filepath.Clean(abs), Kind: c.Kind, Shows: c.Shows})
	}
	v.OK = true
	return v
}

// callRoot is the working directory the newest attempt of a call was placed in ("" when the backend placed it nowhere).
func callRoot(r rows, call string) string {
	prep, ok := r.prepared().last(func(ev engine.Event) bool { return ev.CallID == call })
	if !ok {
		return ""
	}
	alloc, _ := prep.Data["allocation"].(map[string]any)
	return str(alloc, "path")
}

// callSource is the source identity the engine recorded on the newest completed row of a QA call (qa_source): false when the row
// records none, or the error of reading it.
func callSource(r rows, call string) (source.Identity, bool) {
	done, found := r.terminal(call)
	if !found || done.Event != engine.EvCompleted {
		return source.Identity{}, false
	}
	raw, ok := done.Data["qa_source"]
	if !ok || raw == nil {
		return source.Identity{}, false
	}
	b, err := json.Marshal(raw)
	if err != nil {
		return source.Identity{}, false
	}
	var id source.Identity
	if json.Unmarshal(b, &id) != nil || id.Kind == "" {
		return source.Identity{}, false
	}
	return id, true
}
