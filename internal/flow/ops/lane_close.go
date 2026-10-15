package ops

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/reviewadapter"
)

// ---- crw.milestoneClose ----

// milestoneCloseOp is crw.milestoneClose(milestone, evidence): the close of a milestone once every lane has landed, over the
// evidence the close made: the final gate that passed on the head H it pinned (finalgate.go) and the review that took that gate as
// its input - the crosscheck's verdict V (crosscheck.go, evidence member crosscheck) or, for a close made before the crosscheck, the
// review of reviewer-final (final). The op carries both into its answer as they are. It closes only over a verified review
// (CRW-1473): a crosscheck must be a V of verified, of the milestone and of the gate's head; evidence with no crosscheck and a final
// review (a journal of the close before the crosscheck) must hold a verified final verdict; evidence with neither, or with one that
// does not verify, is refused (close_unverified) and nothing is recorded. The row names the range, the tip the close judged and
// whether that tip is still the head of the gate: a target that moved on after the gate - another actor, or a lane that integrated a
// new head after a resume - makes the gate a judgement of a head that is gone, and the answer says so (moved), so the close judges
// the new head again. Proven holds only while the integration target is still the tip the row names.
type milestoneCloseOp struct{ l *Lane }

// CloseResultOutcome is the answer of crw.milestoneClose.
type CloseResultOutcome struct {
	Milestone string          `json:"milestone"`
	Base      string          `json:"base"`
	Tip       string          `json:"tip"`
	Gate      json.RawMessage `json:"gate"`
	Final     json.RawMessage `json:"final"`
	Audit     json.RawMessage `json:"audit"`
	// Crosscheck is the crosscheck's verdict V the close rests on (null for a close without one); Reflection the alignment the
	// reflection of the close answered (ALIGNED, MISALIGNED, NOT_ASSESSED, NOT_RUN), which the script adds when it reports the close
	// ("" from crw.milestoneClose itself).
	Crosscheck json.RawMessage `json:"crosscheck"`
	Reflection string          `json:"reflection"`
	// GateHead is the head the gate in the evidence judged; Moved is whether the target is not at it any more.
	GateHead string `json:"gate_head"`
	Moved    bool   `json:"moved"`
}

func (o *milestoneCloseOp) Run(ctx context.Context, req engine.OpRequest) (engine.OpResult, error) {
	var ms string
	var evidence struct {
		Gate       json.RawMessage `json:"gate"`
		Final      json.RawMessage `json:"final"`
		Crosscheck json.RawMessage `json:"crosscheck"`
	}
	if err := args(req.Args, &ms, &evidence); err != nil {
		var obj struct {
			ID string `json:"id"`
		}
		if err2 := args(req.Args, &obj); err2 != nil || obj.ID == "" {
			return engine.OpResult{}, fmt.Errorf("crw.milestoneClose(milestone, evidence): %w", err)
		}
		ms = obj.ID
	}
	var gateHead string
	if present(evidence.Gate) {
		var g struct {
			Head string `json:"head"`
		}
		_ = json.Unmarshal(evidence.Gate, &g)
		gateHead = g.Head
	}
	if why := closeUnverified(ms, gateHead, evidence.Crosscheck, evidence.Final); why != "" {
		return engine.OpResult{}, &Refusal{ReasonCloseUnverified, "milestone " + ms + ": " + why}
	}
	r := rows(req.Journal.Events())
	plan := planOf(r, ms)
	tip, err := o.l.d.Relay.IntegrationTip(ctx, plan)
	if err != nil {
		return engine.OpResult{}, err
	}
	base := ""
	if led, ok := firstLedger(r, ms); ok {
		base = dataStr(led, "base_tip")
	}
	out := CloseResultOutcome{Milestone: ms, Base: base, Tip: tip, Gate: json.RawMessage(`null`), Final: json.RawMessage(`null`), Audit: json.RawMessage(`null`),
		Crosscheck: json.RawMessage(`null`)}
	if len(evidence.Gate) > 0 {
		out.Gate = evidence.Gate
		var g struct {
			Head string `json:"head"`
		}
		_ = json.Unmarshal(evidence.Gate, &g)
		out.GateHead = g.Head
		out.Moved = g.Head != "" && g.Head != tip
	}
	if len(evidence.Final) > 0 {
		out.Final = evidence.Final
	}
	if len(evidence.Crosscheck) > 0 {
		out.Crosscheck = evidence.Crosscheck
	}
	return engine.OpResult{Result: result(out), Data: map[string]any{"milestone": ms, "plan": plan, "base": base, "tip": tip, "gate_head": out.GateHead, "moved": out.Moved}}, nil
}

// ReasonCloseUnverified is the refusal of a milestone close whose evidence holds no verified review of the gate's head.
const ReasonCloseUnverified = "close_unverified"

// closeUnverified says why the evidence of a close does not let the milestone close ("" when it does): a crosscheck V that is not
// verified, or is of another milestone or another head than the gate's; with no crosscheck, a final review that is not a verified
// verdict; with neither, no review at all.
func closeUnverified(ms, gateHead string, crosscheck, final json.RawMessage) string {
	if present(crosscheck) {
		var v struct {
			Verdict   string `json:"verdict"`
			Milestone string `json:"milestone"`
			Head      string `json:"head"`
		}
		if err := json.Unmarshal(crosscheck, &v); err != nil {
			return "the crosscheck of the evidence is not a verdict: " + err.Error()
		}
		switch {
		case v.Verdict != reviewadapter.VerdictVerified:
			return fmt.Sprintf("the crosscheck's verdict is %q, not verified", v.Verdict)
		case v.Milestone != ms:
			return fmt.Sprintf("the crosscheck is of milestone %q, not of %s", v.Milestone, ms)
		case gateHead == "" || v.Head != gateHead:
			return fmt.Sprintf("the crosscheck is of the head %q, not of the gate's head %q", v.Head, gateHead)
		}
		return ""
	}
	if present(final) {
		if finalVerified(final) {
			return ""
		}
		return "the final review of the evidence is not a verified verdict"
	}
	return "the evidence holds neither a crosscheck nor a final review"
}

// finalVerified says whether a final review of a close's evidence is a verified verdict: the result of crw.reviewerFinal (state
// verdict, the VERDICT_FINAL answer at verdict) or a bare answer whose word is verified.
func finalVerified(final json.RawMessage) bool {
	var f struct {
		State   string          `json:"state"`
		Verdict json.RawMessage `json:"verdict"`
	}
	if json.Unmarshal(final, &f) != nil {
		return false
	}
	var word string
	if json.Unmarshal(f.Verdict, &word) == nil {
		return word == reviewadapter.VerdictVerified && (f.State == "" || f.State == StateVerdict)
	}
	var answer struct {
		Verdict string `json:"verdict"`
	}
	if json.Unmarshal(f.Verdict, &answer) != nil {
		return false
	}
	return answer.Verdict == reviewadapter.VerdictVerified && (f.State == "" || f.State == StateVerdict)
}

func (o *milestoneCloseOp) Proven(ctx context.Context, _ []engine.Event, row engine.Event) (bool, string, error) {
	tip, err := o.l.d.Relay.IntegrationTip(ctx, dataStr(row, "plan"))
	if err != nil {
		return false, "", err
	}
	if tip != dataStr(row, "tip") {
		return false, "the integration target moved after the close judged it", nil
	}
	return true, "", nil
}

// ---- crw.reportUp ----

// reportUpOp is crw.reportUp(milestone, close): the report of a closed milestone goes up to the initiative lead. The report row is
// the run's; its delivery is F11's and is asked of the Reporter when one is wired, with the close's gate, final review, audit,
// crosscheck and reflection as the close argument carries them.
type reportUpOp struct{ l *Lane }

func (o *reportUpOp) Run(ctx context.Context, req engine.OpRequest) (engine.OpResult, error) {
	var ms string
	var closed CloseResultOutcome
	if err := args(req.Args, &ms, &closed); err != nil {
		return engine.OpResult{}, fmt.Errorf("crw.reportUp(milestone, close): %w", err)
	}
	r := rows(req.Journal.Events())
	plan := planOf(r, ms)
	id := "rep_" + hashParts(req.RunID, ms, closed.Tip)[:24]
	delivered := false
	if o.l.d.Reporter != nil {
		got, err := o.l.d.Reporter.ReportUp(ctx, Report{Plan: plan, Milestone: ms, Range: CloseRange{Base: closed.Base, Tip: closed.Tip},
			Close: CloseOutcome{Gate: closed.Gate, Final: closed.Final, Audit: closed.Audit, Crosscheck: closed.Crosscheck, Reflection: closed.Reflection}, RequestID: req.RequestID})
		if err != nil {
			return engine.OpResult{}, err
		}
		if got != "" {
			id = got
		}
		delivered = true
	}
	return engine.OpResult{Result: result(map[string]any{"report_id": id, "milestone": ms, "tip": closed.Tip, "delivered": delivered}),
		Data: map[string]any{"milestone": ms, "report_id": id, "tip": closed.Tip, "delivered": delivered}}, nil
}

func (o *reportUpOp) Proven(ctx context.Context, ev []engine.Event, row engine.Event) (bool, string, error) {
	return provenAlways(ctx, ev, row)
}
