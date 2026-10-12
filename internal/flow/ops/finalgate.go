package ops

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/source"
)

// This file is the final gate of a milestone close (CRW-1206, F13) and the record of reviewer-final.
//
// crw.finalGate(milestone) pins the head H the milestone close judges - the tip of the integration target now - and judges the
// evidence that stands on it: the test evidence (a verification-record/1 of H for each command of the plan's required set, made by
// crw.verify) and, when a criterion of the milestone has the surface web, tui or desktop, the QA receipt (crw.qaReceipt). Each
// piece of evidence is fresh (it stands on H), missing (there is none) or stale (it stands on another head), and the verdict is
// written to the journal as the row of the gate. The comparison of identities is the native final gate's (internal/pabcd/source
// Compare, the rule of internal/role/spawn/finalgate.go); nothing here reads the native marker or a goalplan.
//
// The gate does not make evidence. A pending gate names what is missing; the script (crw:lib/close) makes it - crw.verify at H, the
// QA calls - and evaluates again. A gate that passes is the only input reviewer-final gets, and a head that moves after the gate
// is judged again from its new head (crw.milestoneClose says the target moved).

// The states of one piece of evidence.
const (
	EvidenceFresh   = "fresh"
	EvidenceMissing = "missing"
	EvidenceStale   = "stale"
)

// The states of a gate and the reasons a gate failed.
const (
	GatePass    = "pass"
	GatePending = "pending"
	GateFailed  = "failed"

	GateTestFailed     = "test_failed"
	GateQAFailed       = "qa_failed"
	GateQAInconclusive = "qa_inconclusive"
)

// The kinds of evidence a gate judges, and the surfaces of a criterion that are driven by hand.
const (
	EvidenceKindTest = "test"
	EvidenceKindQA   = "qa"
	surfaceWeb       = "web"
	surfaceTUI       = "tui"
	surfaceDesktop   = "desktop"
)

// Evidence is one piece of evidence of a gate and its judgement.
type Evidence struct {
	Kind string `json:"kind"`
	// Command is the command of a test evidence.
	Command string `json:"command"`
	State   string `json:"state"`
	// Pass is whether the evidence is fresh and says pass.
	Pass bool `json:"pass"`
	// Head is the head the evidence stands on ("" for missing evidence).
	Head string `json:"head"`
	// Digest is the digest of the verification record, or the QA state of a receipt.
	Digest string `json:"digest"`
	// Verdict is the QA state of a receipt.
	Verdict string `json:"verdict"`
	// Call is the call whose row is the evidence.
	Call string `json:"call"`
	// Why says why a stale piece of evidence is stale.
	Why string `json:"why"`
}

// GateDecision is one decision of the approved DESIGN as the reviewer of the range sees it: its id, what it changes and the nodes
// whose issues follow it.
type GateDecision struct {
	ID     string   `json:"id"`
	Change string   `json:"change"`
	Nodes  []string `json:"nodes"`
}

// GateOutcome is the answer of crw.finalGate.
type GateOutcome struct {
	Milestone string `json:"milestone"`
	Plan      string `json:"plan"`
	// Head is the integrated head H the gate pinned, Base the tip the plan started from and Range their span.
	Head  string `json:"head"`
	Base  string `json:"base"`
	Range string `json:"range"`
	// Identity is the source identity of H, which every piece of evidence is compared with.
	Identity source.Identity `json:"identity"`
	State    string          `json:"state"`
	Reason   string          `json:"reason"`
	Grade    string          `json:"grade"`
	Surfaces []string        `json:"surfaces"`
	// QARequired is whether a criterion of the milestone has a surface that is driven (web, tui, desktop).
	QARequired bool `json:"qa_required"`
	// QAPasses is whether the QA evidence is judged by the two oracle passes (a driven surface at the work grade C3 or above); a
	// C2 milestone has the self-review of the QA call.
	QAPasses bool       `json:"qa_passes"`
	Commands []string   `json:"commands"`
	Evidence []Evidence `json:"evidence"`
	// NeedTests are the commands whose test evidence is missing or stale, NeedQA whether the QA receipt is.
	NeedTests []string `json:"need_tests"`
	NeedQA    bool     `json:"need_qa"`
	// Decisions are the decisions of the approved DESIGN revision the milestone runs under (the ledger row): the comparison source
	// of the alignment judgement of reviewer-final, which reads a checkout of the repository and not the journal.
	Decisions []GateDecision `json:"decisions"`
}

// RequiredCommands are the verification commands of the plan's required set; a plan that names none requires the verification
// of the repository's CI (CILocalCommand).
func (m Milestone) requiredCommands() []string {
	if len(m.RequiredCommands) == 0 {
		return []string{CILocalCommand}
	}
	out := slices.Clone(m.RequiredCommands)
	slices.Sort(out)
	return slices.Compact(out)
}

// needsQA says whether a criterion of the milestone has a surface a person would drive.
func (m Milestone) needsQA() bool {
	for _, s := range m.Surfaces {
		switch s {
		case surfaceWeb, surfaceTUI, surfaceDesktop:
			return true
		}
	}
	return false
}

// finalGateOp is crw.finalGate(milestone).
type finalGateOp struct{ l *Lane }

func (o *finalGateOp) Run(ctx context.Context, req engine.OpRequest) (engine.OpResult, error) {
	var ms string
	if err := args(req.Args, &ms); err != nil || ms == "" {
		return engine.OpResult{}, fmt.Errorf("crw.finalGate(milestone): a milestone is required: %v", err)
	}
	r := rows(req.Journal.Events())
	m, plan, err := milestoneOf(r, ms)
	if err != nil {
		return engine.OpResult{}, fmt.Errorf("crw.finalGate: %w", err)
	}
	head, err := o.l.d.Relay.IntegrationTip(ctx, plan)
	if err != nil {
		return engine.OpResult{}, err
	}
	if head == "" {
		return engine.OpResult{}, fmt.Errorf("crw.finalGate: the integration target of plan %s has no tip", plan)
	}
	base := ""
	if led, ok := firstLedger(r, ms); ok {
		base = dataStr(led, "base_tip")
	}
	id := source.CommitIdentity(head, o.l.d.Now)
	out := GateOutcome{Milestone: ms, Plan: plan, Head: head, Base: base, Range: head, Identity: id, Grade: m.Grade, Surfaces: nonNil(m.Surfaces),
		QARequired: m.needsQA(), QAPasses: m.needsQA() && GradeAtLeast(m.Grade, 3), Commands: m.requiredCommands(), Evidence: []Evidence{}, NeedTests: []string{}, Decisions: []GateDecision{}}
	if base != "" {
		out.Range = base + ".." + head
	}
	out.Decisions = gateDecisions(r, ms)
	for _, cmd := range out.Commands {
		ev := judgeTest(r, ms, cmd, id)
		out.Evidence = append(out.Evidence, ev)
		if ev.State != EvidenceFresh {
			out.NeedTests = append(out.NeedTests, cmd)
		}
	}
	if out.QARequired {
		ev := judgeQA(r, ms, id)
		out.Evidence = append(out.Evidence, ev)
		out.NeedQA = ev.State != EvidenceFresh
	}
	out.State, out.Reason = judgeGate(out)
	return engine.OpResult{Result: result(out), Data: map[string]any{
		"milestone": ms, "plan": plan, "head": head, "base": base, "range": out.Range, "command": strings.Join(out.Commands, ";"), "ok": out.State == GatePass,
		"state": out.State, "reason": out.Reason, "qa_required": out.QARequired, "qa_passes": out.QAPasses, "grade": out.Grade,
		"evidence": out.Evidence, "need_tests": out.NeedTests, "need_qa": out.NeedQA, "decisions": out.Decisions,
	}}, nil
}

// gateDecisions are the decisions of the DESIGN revision the milestone runs under, read from the ledger row of the journal, each
// with the nodes whose issues name it. A milestone with no ledger row (or a ledger that holds no valid DESIGN) has none.
func gateDecisions(r rows, ms string) []GateDecision {
	out := []GateDecision{}
	led, ok := currentLedger(r, ms)
	if !ok {
		return out
	}
	raw, err := json.Marshal(led.Data["design"])
	if err != nil {
		return out
	}
	d, err := DecodeDesign(raw)
	if err != nil {
		return out
	}
	for _, dec := range d.Decisions {
		gd := GateDecision{ID: dec.ID, Change: dec.Change, Nodes: []string{}}
		for _, is := range d.Issues {
			if slices.Contains(is.DecisionRefs, dec.ID) {
				gd.Nodes = append(gd.Nodes, is.Node)
			}
		}
		out = append(out, gd)
	}
	return out
}

// judgeGate: a piece of evidence that is fresh and says no fails the gate (the work is not good, however the head moves); the
// gate is pending while a piece is missing or stale, and passes when every piece is fresh and passes.
func judgeGate(g GateOutcome) (state, reason string) {
	for _, ev := range g.Evidence {
		if ev.State != EvidenceFresh || ev.Pass {
			continue
		}
		switch {
		case ev.Kind == EvidenceKindTest:
			return GateFailed, GateTestFailed
		case ev.Verdict == QAInconclusive:
			return GateFailed, GateQAInconclusive
		default:
			return GateFailed, GateQAFailed
		}
	}
	if len(g.NeedTests) > 0 || g.NeedQA {
		return GatePending, ""
	}
	return GatePass, ""
}

// judgeTest judges the test evidence of one command: the newest verification of the milestone's head with the command that ran. A
// verification that stands on the head is fresh; one that stands on another head is stale; none is missing. An identity git cannot
// resolve, on either side, proves nothing (the native gate's rule), so it is stale.
func judgeTest(r rows, ms, command string, head source.Identity) Evidence {
	ev := Evidence{Kind: EvidenceKindTest, Command: command, State: EvidenceMissing}
	rowsOfCmd := r.opRows(OpVerify).where(func(row engine.Event) bool {
		return dataStr(row, "node") == "" && dataStr(row, "milestone") == ms && dataStr(row, "command") == command && dataBool(row, "ran")
	})
	if len(rowsOfCmd) == 0 {
		return ev
	}
	fresh, ok := rowsOfCmd.last(func(row engine.Event) bool {
		return source.Compare(source.CommitIdentity(dataStr(row, "head"), nil), head).Kind == source.ComparisonSame
	})
	if ok {
		return Evidence{Kind: EvidenceKindTest, Command: command, State: EvidenceFresh, Pass: dataBool(fresh, "ok"), Head: dataStr(fresh, "head"), Digest: dataStr(fresh, "digest"), Call: fresh.CallID}
	}
	last := rowsOfCmd[len(rowsOfCmd)-1]
	cmp := source.Compare(source.CommitIdentity(dataStr(last, "head"), nil), head)
	why := cmp.Detail
	if cmp.Kind == source.ComparisonUnavailable {
		why = cmp.Reason
	}
	return Evidence{Kind: EvidenceKindTest, Command: command, State: EvidenceStale, Head: dataStr(last, "head"), Digest: dataStr(last, "digest"), Call: last.CallID, Why: why}
}

// judgeQA judges the QA evidence of the milestone the same way: the newest receipt that stands on the head is fresh, and it passes
// when its state is PASS.
func judgeQA(r rows, ms string, head source.Identity) Evidence {
	ev := Evidence{Kind: EvidenceKindQA, State: EvidenceMissing}
	receipts := r.opRows(OpQAReceipt).where(func(row engine.Event) bool { return dataStr(row, "milestone") == ms })
	if len(receipts) == 0 {
		return ev
	}
	fresh, ok := receipts.last(func(row engine.Event) bool {
		return source.Compare(source.CommitIdentity(dataStr(row, "head"), nil), head).Kind == source.ComparisonSame
	})
	if ok {
		state := dataStr(fresh, "state")
		// a PASS stands only over the tree the gate pinned: the identity the receipt rests on is compared with the head's again
		pass := state == QAPass && sourceIsHead(fresh, head)
		return Evidence{Kind: EvidenceKindQA, State: EvidenceFresh, Pass: pass, Head: dataStr(fresh, "head"), Verdict: state, Digest: dataStr(fresh, "capture_call"), Call: fresh.CallID}
	}
	last := receipts[len(receipts)-1]
	cmp := source.Compare(source.CommitIdentity(dataStr(last, "head"), nil), head)
	why := cmp.Detail
	if cmp.Kind == source.ComparisonUnavailable {
		why = cmp.Reason
	}
	return Evidence{Kind: EvidenceKindQA, State: EvidenceStale, Head: dataStr(last, "head"), Verdict: dataStr(last, "state"), Call: last.CallID, Why: why}
}

// sourceIsHead says whether the source identity a receipt row rests on is the identity of the head (the clean commit).
func sourceIsHead(row engine.Event, head source.Identity) bool {
	raw, err := json.Marshal(row.Data["source"])
	if err != nil {
		return false
	}
	var id source.Identity
	if json.Unmarshal(raw, &id) != nil || id.Kind == "" {
		return false
	}
	return source.Compare(id, head).Kind == source.ComparisonSame
}

// Proven: a gate judged the tip it pinned. While the target is still at that head the judgement stands (the evidence it read is
// the journal's, which a resume has already replayed); a target that moved on is a head the gate has not judged, and the gate is
// evaluated again from the head it has now.
func (o *finalGateOp) Proven(ctx context.Context, _ []engine.Event, row engine.Event) (bool, string, error) {
	tip, err := o.l.d.Relay.IntegrationTip(ctx, dataStr(row, "plan"))
	if err != nil {
		return false, "", err
	}
	if tip != dataStr(row, "head") {
		return false, "the integration target moved after the gate judged it", nil
	}
	return true, "", nil
}

// milestoneOf is the milestone as the journal recorded it (crw.milestone) and the plan it belongs to.
func milestoneOf(r rows, ms string) (Milestone, string, error) {
	row, ok := r.opRows(OpMilestone).last(func(ev engine.Event) bool { return dataStr(ev, "milestone") == ms })
	if !ok {
		return Milestone{}, "", fmt.Errorf("milestone %s has no crw.milestone row in the journal", ms)
	}
	var m Milestone
	if err := json.Unmarshal(resultOf(row), &m); err != nil {
		return Milestone{}, "", fmt.Errorf("the crw.milestone row of %s is unreadable: %w", ms, err)
	}
	return m, dataStr(row, "plan"), nil
}

// ---- crw.reviewerFinal ----

// reviewerFinalOp is crw.reviewerFinal(milestone, gate, call, opts): the record of the review of reviewer-final over the integrated
// range. State verdict needs a completed, valid VERDICT_FINAL of a reviewer whose model family differs from every author of the
// milestone (the architect and the executors); the other states - held, unknown, refused (independence_refused included),
// null_model, superseded - are the call's own status, as they are for a node's review. The row names the range and the criteria
// digest it judged (ReviewerFinalProof) and the head of the gate it took as its input.
type reviewerFinalOp struct{ l *Lane }

// FinalResult is the answer of crw.reviewerFinal.
type FinalResult struct {
	State     string          `json:"state"`
	Reason    string          `json:"reason"`
	Milestone string          `json:"milestone"`
	Head      string          `json:"head"`
	Range     string          `json:"range"`
	Verdict   json.RawMessage `json:"verdict"`
	Alignment string          `json:"alignment"`
	Drift     int             `json:"drift"`
	Call      Brief           `json:"call"`
	Retried   bool            `json:"retried"`
}

func (o *reviewerFinalOp) Run(_ context.Context, req engine.OpRequest) (engine.OpResult, error) {
	var ms string
	var gate struct {
		Head  string `json:"head"`
		Range string `json:"range"`
	}
	var call CallRef
	var opts struct {
		Retried bool `json:"retried"`
	}
	if err := args(req.Args, &ms, &gate, &call, &opts); err != nil {
		return engine.OpResult{}, fmt.Errorf("crw.reviewerFinal(milestone, gate, call, opts): %w", err)
	}
	r := rows(req.Journal.Events())
	m, _, err := milestoneOf(r, ms)
	if err != nil {
		return engine.OpResult{}, fmt.Errorf("crw.reviewerFinal: %w", err)
	}
	res := FinalResult{Milestone: ms, Head: gate.Head, Range: gate.Range, Verdict: json.RawMessage(`null`), Call: call.brief(), Retried: opts.Retried}
	var v VerdictFinal
	detail := ""
	switch call.Status {
	case "completed":
		var derr error
		if v, derr = DecodeVerdictFinal(call.Output); derr != nil {
			res.State, res.Reason = StateNullModel, "verdict_invalid"
			break
		}
		if why := independent(call.Meta.ConfiguredPair.Model, r.milestoneAuthorModels()); why != "" {
			res.State, res.Reason = StateRefused, why
			break
		}
		// A verified final review is the authority the close rests on, so it is held to what a node's verified review is: no
		// criterion that is not met, no defect of P0 to P2, and every criterion of the milestone evaluated once under its id (c1 to
		// cn, the labels of the packet). An answer that says verified without it is not a verdict; it is asked once more.
		if cerr := checkFinalVerified(v, m); cerr != nil {
			res.State, res.Reason = StateNullModel, "verdict_invalid"
			detail = cerr.Error()
			break
		}
		res.State, res.Verdict = StateVerdict, call.Output
		res.Alignment, res.Drift = v.Alignment.Verdict, len(v.Alignment.Drift)
	case "null_model":
		res.State, res.Reason = StateNullModel, call.Reason
	case "held":
		res.State, res.Reason = StateHeld, call.Reason
	case "unknown":
		res.State, res.Reason = StateUnknown, call.Reason
	case "refused":
		res.State, res.Reason = StateRefused, call.Reason
	default:
		res.State, res.Reason = StateSuperseded, call.Reason
	}
	return engine.OpResult{Result: result(res), Data: map[string]any{
		"milestone": ms, "head": gate.Head, "range": gate.Range, "criteria_digest": m.CriteriaDigest, "state": res.State, "reason": res.Reason, "verdict": v.Verdict,
		"alignment": res.Alignment, "call": call.CallID, "reviewer_model": call.Meta.ConfiguredPair.Model, "retried": opts.Retried, "detail": detail,
	}}, nil
}

// checkFinalVerified holds a verified VERDICT_FINAL to the rules of a verified VERDICT and to the criteria of the milestone.
func checkFinalVerified(v VerdictFinal, m Milestone) error {
	rv := ReviewVerdict{Verdict: v.Verdict, Criteria: v.Criteria, Defects: v.Defects, Summary: v.Summary}
	if err := rv.check("VERDICT_FINAL"); err != nil {
		return err
	}
	if v.Verdict == "verified" {
		return CheckCoverage(v.Criteria, CriterionIDs(len(m.Criteria)))
	}
	return nil
}

func (o *reviewerFinalOp) Proven(ctx context.Context, ev []engine.Event, row engine.Event) (bool, string, error) {
	return provenAlways(ctx, ev, row)
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
