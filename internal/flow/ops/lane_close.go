package ops

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
)

// ---- crw.milestoneClose ----

// milestoneCloseOp is crw.milestoneClose(milestone): the close of a milestone once every lane has landed. The final gate over the
// integrated range and the review of reviewer-final are F13's; this issue records the call, the range it judges and the tip it
// judged, and asks the Closer when one is wired. The row names the tip, and Proven holds only while the integration target is still
// that tip: a lane that integrates a new head after the close (a resume that reran one lane's suffix) makes the recorded close a
// close of a range that is gone, and the close runs live again.
type milestoneCloseOp struct{ l *Lane }

// CloseResultOutcome is the answer of crw.milestoneClose.
type CloseResultOutcome struct {
	Milestone string          `json:"milestone"`
	Base      string          `json:"base"`
	Tip       string          `json:"tip"`
	Wired     bool            `json:"wired"`
	Gate      json.RawMessage `json:"gate"`
	Final     json.RawMessage `json:"final"`
}

func (o *milestoneCloseOp) Run(ctx context.Context, req engine.OpRequest) (engine.OpResult, error) {
	var ms string
	if err := args(req.Args, &ms); err != nil {
		var obj struct {
			ID string `json:"id"`
		}
		if err2 := args(req.Args, &obj); err2 != nil || obj.ID == "" {
			return engine.OpResult{}, fmt.Errorf("crw.milestoneClose(milestone): %w", err)
		}
		ms = obj.ID
	}
	r := rows(req.Journal.Events())
	plan := planOf(r, ms)
	tip, err := o.l.d.Relay.IntegrationTip(ctx, plan)
	if err != nil {
		return engine.OpResult{}, err
	}
	base := ""
	if led, ok := ledgerOf(r, ms, FirstRevision); ok {
		base = dataStr(led, "base_tip")
	}
	out := CloseResultOutcome{Milestone: ms, Base: base, Tip: tip, Gate: json.RawMessage(`null`), Final: json.RawMessage(`null`)}
	if o.l.d.Closer != nil {
		var m Milestone
		if row, ok := r.opRows(OpMilestone).last(func(ev engine.Event) bool { return dataStr(ev, "milestone") == ms }); ok {
			_ = json.Unmarshal(resultOf(row), &m)
		}
		res, err := o.l.d.Closer.Close(ctx, m, CloseRange{Base: base, Tip: tip})
		if err != nil {
			return engine.OpResult{}, err
		}
		out.Wired = res.Wired
		if len(res.Gate) > 0 {
			out.Gate = res.Gate
		}
		if len(res.Final) > 0 {
			out.Final = res.Final
		}
	}
	return engine.OpResult{Result: result(out), Data: map[string]any{"milestone": ms, "plan": plan, "base": base, "tip": tip, "wired": out.Wired}}, nil
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
// the run's; its delivery is F11's and is asked of the Reporter when one is wired.
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
			Close: CloseOutcome{Wired: closed.Wired, Gate: closed.Gate, Final: closed.Final}, RequestID: req.RequestID})
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
