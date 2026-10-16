package ops

import (
	"bytes"
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
		// Repair, when the close corrected the failed crosscheck in the run (CRW-1490), is the failed gate's head and the committed
		// candidate over it: {base, head}.
		Repair *struct {
			Base string `json:"base"`
			Head string `json:"head"`
		} `json:"repair"`
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
	var tip string
	var err error
	repairMoved := false
	if evidence.Repair != nil {
		// A close that repaired the crosscheck in the run publishes the candidate (CRW-1490): the journal's own records are the
		// proof, not the script's copies - the synthetic node's commit row for the candidate, the recorded gate that passed on it
		// (the very outcome the evidence names) and the recorded round-2 recheck's verified V of it. Then the relay moves the
		// target by a fenced compare-and-swap from the failed head, so a target that moved on keeps what it has.
		rep := evidence.Repair
		bad := func(detail string) (engine.OpResult, error) {
			return engine.OpResult{}, &Refusal{"close_repair_unverified", "milestone " + ms + ": " + detail}
		}
		if rep.Base == "" || rep.Head == "" || rep.Head != gateHead {
			return bad(fmt.Sprintf("the repair %v does not name the gate's head %q", rep, gateHead))
		}
		if _, ok := r.opRows(OpCommit).last(func(ev engine.Event) bool {
			return dataStr(ev, "node") == "milestone:"+ms && dataStr(ev, "commit") == rep.Head
		}); !ok {
			return bad("no recorded commit of the candidate " + rep.Head + " for the close's repair")
		}
		gateRow, ok := r.opRows(OpGate).last(func(ev engine.Event) bool {
			return dataStr(ev, "milestone") == ms && dataStr(ev, "head") == rep.Head && dataStr(ev, "state") == GatePass
		})
		if !ok || !jsonSame(resultOf(gateRow), evidence.Gate) {
			return bad("no recorded passing gate of the candidate " + rep.Head + ", or the gate in the evidence is not it")
		}
		// the digest of a fresh passing test verification of the candidate, which the publication records so the official push
		// path reads the repaired close as a verified move (dagsched.VerifiedMoveOnto)
		var passedGate GateOutcome
		if err := json.Unmarshal(evidence.Gate, &passedGate); err != nil {
			return bad("the gate of the evidence is not a gate: " + err.Error())
		}
		verificationDigest := ""
		for _, e := range passedGate.Evidence {
			if e.Kind == EvidenceKindTest && e.State == EvidenceFresh && e.Pass && e.Digest != "" {
				verificationDigest = e.Digest
				break
			}
		}
		if verificationDigest == "" {
			return bad("the recorded passing gate of the candidate has no verified test digest")
		}
		ccRow, ok := r.opRows(OpCrosscheck).last(func(ev engine.Event) bool {
			return dataStr(ev, "milestone") == ms && intOf(ev.Data["round"]) == 2 && dataStr(ev, "state") == CrosscheckStateVerdict
		})
		var cc CrosscheckOutcome
		if !ok || json.Unmarshal(resultOf(ccRow), &cc) != nil || cc.Verdict == nil || cc.Verdict.Verdict != reviewadapter.VerdictVerified ||
			cc.Verdict.Head != rep.Head || cc.Verdict.PriorHead != rep.Base {
			return bad("no recorded round-2 crosscheck that verified the candidate " + rep.Head + " over the failed head " + rep.Base)
		}
		var named CrosscheckVerdictRecord
		if json.Unmarshal(evidence.Crosscheck, &named) != nil {
			return bad("the crosscheck of the evidence is not a verdict: " + cc.State)
		}
		a, _ := json.Marshal(*cc.Verdict)
		b, _ := json.Marshal(named)
		if !bytes.Equal(a, b) {
			return bad("the crosscheck in the evidence is not the recorded recheck's verdict")
		}
		if anc, aerr := IsAncestor(ctx, o.l.d.Git, o.l.d.Repo, rep.Base, rep.Head); aerr != nil {
			return engine.OpResult{}, aerr
		} else if !anc {
			return bad("the candidate " + rep.Head + " does not descend from the failed head " + rep.Base)
		}
		integrator, ok := o.l.d.Relay.(MilestoneRepairIntegrator)
		if !ok {
			return engine.OpResult{}, &Refusal{"close_repair_unavailable", "the relay of this run cannot publish a close repair"}
		}
		res, rerr := integrator.IntegrateMilestoneRepair(ctx, MilestoneRepairRequest{Plan: plan, Milestone: ms, Base: rep.Base, Head: rep.Head, VerificationDigest: verificationDigest})
		if rerr != nil {
			return engine.OpResult{}, rerr
		}
		if res.Tip == "" {
			return engine.OpResult{}, fmt.Errorf("crw.milestoneClose: the repair publisher returned no integration tip")
		}
		tip, repairMoved = res.Tip, res.Moved
	} else {
		if tip, err = o.l.d.Relay.IntegrationTip(ctx, plan); err != nil {
			return engine.OpResult{}, err
		}
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
	out.Moved = out.Moved || repairMoved
	if len(evidence.Final) > 0 {
		out.Final = evidence.Final
	}
	if len(evidence.Crosscheck) > 0 {
		out.Crosscheck = evidence.Crosscheck
	}
	data := map[string]any{"milestone": ms, "plan": plan, "base": base, "tip": tip, "gate_head": out.GateHead, "moved": out.Moved}
	if evidence.Repair != nil {
		data["repair_base"], data["repair_head"] = evidence.Repair.Base, evidence.Repair.Head
	}
	return engine.OpResult{Result: result(out), Data: data}, nil
}

// ReasonCloseUnverified is the refusal of a milestone close whose evidence holds no verified review of the gate's head.
const ReasonCloseUnverified = "close_unverified"

// jsonSame says whether two JSON values are the same, by their canonical encoding.
func jsonSame(a, b json.RawMessage) bool {
	var x, y any
	if json.Unmarshal(a, &x) != nil || json.Unmarshal(b, &y) != nil {
		return false
	}
	am, _ := json.Marshal(x)
	bm, _ := json.Marshal(y)
	return bytes.Equal(am, bm)
}

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
