package ops

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
	"github.com/thisisjun786/codex-relay-workflow/internal/tracking"
)

// ---- crw.mergeRequest ----

// The reasons a merge request is refused with (the result's reason; the script decides what to do).
const (
	mergeReasonNotOpen           = "pr_not_open"
	mergeReasonCrosscheckMissing = "crosscheck_missing"
	mergeReasonHeadMoved         = "head_moved"
	mergeReasonCrosscheckRefused = "crosscheck_not_accepted"
	mergeReasonChecksFailed      = "checks_failed"
	mergeReasonChecksPending     = "checks_pending"
	mergeReasonChecksNone        = "checks_none"
	mergeReasonUnresolved        = "comments_unresolved"
)

// The states a crw.mergeRequest answers.
const (
	mergeStateRefused   = "refused"
	mergeStateRequested = "requested"
)

// The values of the review argument's bots member.
const (
	reviewBotsNotConnected = "not_connected"
)

// mergeRequestOp is crw.mergeRequest(milestone, {pr: "OWNER/NAME#N", review: {record: PATH}}): the
// run asks the parent to merge the pull request of a closed milestone. It judges, in order, whether the pull request is open, whether the
// journal's closing acceptance covers its head (the closed milestone's gate and accepted closing review, reported, at the live tip),
// whether the head's checks are green and whether review comments are unresolved; a request that fails one
// is answered {state: "refused", reason, pr, head} and writes a row of its own (not a Go error: the script decides). The dispositions come
// from the crw-pr-review/1 record review.record names — the two-round file crw flow pr comments writes — bound to this pull request and
// head; a missing, malformed or stale record, or a legacy {bots, unresolved} argument, is a Go error, never an approval. A request that
// passes goes to the Reporter once per head.
type mergeRequestOp struct{ l *Lane }

type mergeRequestArg struct {
	PR     string `json:"pr"`
	Review *struct {
		Record string `json:"record"`
	} `json:"review"`
}

func (o *mergeRequestOp) Run(ctx context.Context, req engine.OpRequest) (engine.OpResult, error) {
	var ms string
	var arg mergeRequestArg
	if err := args(req.Args, &ms, &arg); err != nil {
		return engine.OpResult{}, fmt.Errorf("crw.mergeRequest(milestone, {pr, review}): %w", err)
	}
	ref, err := tracking.ParseRef(arg.PR)
	if err != nil {
		return engine.OpResult{}, fmt.Errorf("crw.mergeRequest: the pull request: %w", err)
	}
	if arg.Review == nil || arg.Review.Record == "" {
		return engine.OpResult{}, fmt.Errorf("crw.mergeRequest: the review argument ({record}) is required: a request does not skip the comments by default")
	}
	if o.l.d.PullRequests == nil {
		return engine.OpResult{}, fmt.Errorf("crw.mergeRequest: no pull request reader is wired")
	}
	requester, ok := o.l.d.Reporter.(MergeRequester)
	if !ok || requester == nil {
		return engine.OpResult{}, fmt.Errorf("crw.mergeRequest: no merge requester is wired (the reporter cannot take a merge request)")
	}
	r := rows(req.Journal.Events())
	plan := planOf(r, ms)
	pr, err := o.l.d.PullRequests.PullRequest(ctx, ref)
	if err != nil {
		return engine.OpResult{}, fmt.Errorf("crw.mergeRequest: read %s: %w", ref, err)
	}
	head := pr.HeadSHA
	// the dispositions come from the persisted record of this pull request at this head, never from the arguments
	review, err := tracking.ReadPRReviewRecord(arg.Review.Record, ref, head)
	if err != nil {
		return engine.OpResult{}, fmt.Errorf("crw.mergeRequest: the review record of %s: %w", ref, err)
	}
	unresolved, err := review.Unresolved()
	if err != nil {
		return engine.OpResult{}, fmt.Errorf("crw.mergeRequest: the review record of %s: %w", ref, err)
	}
	botsOff := review.Bots == reviewBotsNotConnected

	// once per head: a request that was sent for this pull request (by its name in any case) at this head is answered again, and nothing is sent
	if prior, found := r.opRows(OpMergeRequest).last(func(ev engine.Event) bool {
		return dataStr(ev, "state") == mergeStateRequested && strings.EqualFold(dataStr(ev, "pr"), ref.String()) && dataStr(ev, "head") == head
	}); found {
		var again map[string]any
		if json.Unmarshal(resultOf(prior), &again) != nil || again == nil {
			again = map[string]any{"state": mergeStateRequested, "pr": ref.String(), "head": head}
		}
		again["repeated"] = true
		return engine.OpResult{Result: result(again), Data: mergeRow(ms, plan, ref.String(), head, mergeStateRequested, "", botsOff)}, nil
	}

	refuse := func(reason string) (engine.OpResult, error) {
		return engine.OpResult{
			Result: result(map[string]any{"state": mergeStateRefused, "reason": reason, "pr": ref.String(), "head": head}),
			Data:   mergeRow(ms, plan, ref.String(), head, mergeStateRefused, reason, botsOff),
		}, nil
	}
	if pr.State != tracking.StateOpen || pr.Merged {
		return refuse(mergeReasonNotOpen)
	}
	// the milestone must be closed at this head: the closed outcome (gate pass + accepted closing review) the journal proves,
	// the integration target still at it and the close reported
	m, closed, reason := closedPRMilestone(req.Journal.Events(), ms, head)
	if reason != "" {
		return refuse(reason)
	}
	_ = m
	if o.l.d.Relay == nil {
		return engine.OpResult{}, errors.New("crw.mergeRequest: no relay is wired")
	}
	tip, err := o.l.d.Relay.IntegrationTip(ctx, plan)
	if err != nil {
		return engine.OpResult{}, fmt.Errorf("crw.mergeRequest: the integration tip of plan %s: %w", plan, err)
	}
	if tip != closed.Tip {
		return refuse(mergeReasonHeadMoved)
	}
	cc := closeCrosscheckOf(closed, head)
	checks, err := o.l.d.PullRequests.Checks(ctx, ref)
	if err != nil {
		return engine.OpResult{}, fmt.Errorf("crw.mergeRequest: read the checks of %s: %w", ref, err)
	}
	if checks.Head != head {
		return refuse(mergeReasonHeadMoved)
	}
	switch checks.State {
	case tracking.CheckSuccess:
	case tracking.CheckFailure:
		return refuse(mergeReasonChecksFailed)
	case tracking.CheckPending:
		return refuse(mergeReasonChecksPending)
	default:
		return refuse(mergeReasonChecksNone)
	}
	if !botsOff && unresolved > 0 {
		return refuse(mergeReasonUnresolved)
	}
	if botsOff {
		unresolved = 0
	}

	// GitHub names a repository without regard to case, so o/r#7 and O/R#7 are one pull request: the id is of the folded name
	id := "rep_" + hashParts(req.RunID, ms, strings.ToLower(ref.String()), head)[:24]
	got, err := requester.MergeRequest(ctx, MergeRequest{Plan: plan, Milestone: ms, PR: ref.String(), Head: head, Base: pr.BaseRef,
		Checks: string(checks.State), Crosscheck: cc, Unresolved: unresolved, BotsNotConnected: botsOff, RequestID: id})
	if err != nil {
		return engine.OpResult{}, err
	}
	if got != "" {
		id = got
	}
	out := map[string]any{"state": mergeStateRequested, "report_id": id, "pr": ref.String(), "head": head, "base": pr.BaseRef, "bots_not_connected": botsOff}
	return engine.OpResult{Result: result(out), Data: mergeRow(ms, plan, ref.String(), head, mergeStateRequested, "", botsOff)}, nil
}

// mergeRow is the Data of a crw.mergeRequest row: what the once-per-head rule reads.
func mergeRow(ms, plan, pr, head, state, reason string, botsOff bool) map[string]any {
	d := map[string]any{"milestone": ms, "plan": plan, "pr": pr, "head": head, "state": state, "reason": reason}
	if botsOff {
		d["bots_not_connected"] = true
	}
	return d
}

// Proven: a requested row is proven (recordOps does not run it again); a refused row is not — a resume re-judges the pull
// request so the checks and the review record are read again.
func (o *mergeRequestOp) Proven(ctx context.Context, ev []engine.Event, row engine.Event) (bool, string, error) {
	if dataStr(row, "state") == mergeStateRefused {
		return false, "the request was refused", nil
	}
	return provenAlways(ctx, ev, row)
}

// mergeCrosscheckOf judges the crosscheck of a milestone at a head from the journal's rows (R3 plan-r2 D-1): the newest completed
// crw.crosscheck row whose data names the milestone and the head. It is accepted when the row's state is verdict, its verdict object says
// verified and the object names the same head (and the same milestone when it names one). Any other state of that row (evidence included:
// a needs_changes waiting for evidence), a row without the object and a needs_changes are not accepted. With no row at the head the reason
// is head_moved when the milestone has a row at another head, else crosscheck_missing; a verified row of an older head never approves a
// head that moved. The MergeCrosscheck is filled only when the reason is empty.
func mergeCrosscheckOf(r rows, milestone, head string) (MergeCrosscheck, string) {
	anyOfMilestone := false
	cc := r.opRows(OpCrosscheck)
	for i := len(cc) - 1; i >= 0; i-- {
		e := cc[i]
		if dataStr(e, "milestone") != milestone {
			continue
		}
		anyOfMilestone = true
		if dataStr(e, "head") != head {
			continue
		}
		v, ok := e.Data["verdict"].(map[string]any)
		if !ok || dataStr(e, "state") != "verdict" || str(v, "verdict") != "verified" || str(v, "head") != head ||
			(str(v, "milestone") != "" && str(v, "milestone") != milestone) {
			return MergeCrosscheck{}, mergeReasonCrosscheckRefused
		}
		return MergeCrosscheck{Verdict: "verified", Head: head, EngineFamily: str(v, "engine_family"), Model: str(v, "model"), Round: roundOf(v["round"])}, ""
	}
	if anyOfMilestone {
		return MergeCrosscheck{}, mergeReasonHeadMoved
	}
	return MergeCrosscheck{}, mergeReasonCrosscheckMissing
}

// roundOf reads a number of a row's data: an int in memory, a float64 after the JSON round trip of the store.
func roundOf(v any) int {
	switch n := v.(type) {
	case int:
		return n
	case int64:
		return int(n)
	case float64:
		return int(n)
	}
	return 0
}

// mergeReasonNotClosed and mergeReasonFinalGate are the reasons the closed-milestone gate answers (CRW-1423): the milestone
// was not closed at the pull request's head (or the close was not reported), and the gate of the close was not accepted.
const (
	mergeReasonNotClosed = "milestone_not_closed"
	mergeReasonFinalGate = "final_gate_not_accepted"
)

// activeRows are the completed call rows of events whose call was not superseded (an EvSuperseded row for the call id kills
// the completed row — an interrupted retry must not read its answer).
func activeRows(events []engine.Event) rows {
	superseded := map[string]bool{}
	for _, ev := range events {
		if ev.Kind == engine.KindCall && ev.Event == engine.EvSuperseded {
			superseded[ev.CallID] = true
		}
	}
	var out []engine.Event
	for _, ev := range events {
		if ev.Kind == engine.KindCall && ev.Event == engine.EvCompleted && superseded[ev.CallID] {
			continue
		}
		out = append(out, ev)
	}
	return out
}

// closeCrosscheckOf fills the MergeCrosscheck a merge request states from the close's accepted closing review at head —
// the verified verdict object the close recorded, not a fresh review.
func closeCrosscheckOf(closed CloseResultOutcome, head string) MergeCrosscheck {
	var v map[string]any
	if json.Unmarshal(closed.Crosscheck, &v) == nil && str(v, "verdict") == "verified" {
		return MergeCrosscheck{Verdict: "verified", Head: head, EngineFamily: str(v, "engine_family"), Model: str(v, "model"), Round: roundOf(v["round"])}
	}
	return MergeCrosscheck{Verdict: "verified", Head: head}
}

// closedPRMilestone is the closing acceptance of a milestone at head, proven from the journal's completed rows alone, with
// every binding exact: the milestone row decodes to the milestone of this plan; the latest milestoneClose of it (not
// superseded, data and result agreeing it did not move, tip and gate_head equal and naming this milestone); the latest
// finalGate of it is that close's pass, recorded before the close and at the same tip; the closing review is the
// crosscheck's verified verdict bound to tip and milestone (or, when the close carries none, the legacy verified Final —
// only under the same exact binding); and the close was reported up after it (the latest reportUp of the milestone names
// the tip and sits after the close's row). head "" means "the close's tip" (crw.prOpen derives it); else the close must be
// of exactly head. The milestone's row comes back too (its nodes are the carried issues). The string is the refusal reason
// or "".
func closedPRMilestone(events []engine.Event, ms, head string) (Milestone, CloseResultOutcome, string) {
	r := activeRows(events)
	var m Milestone
	row, ok := r.opRows(OpMilestone).last(func(ev engine.Event) bool { return dataStr(ev, "milestone") == ms })
	if !ok {
		return m, CloseResultOutcome{}, mergeReasonNotClosed
	}
	if err := json.Unmarshal(resultOf(row), &m); err != nil || m.ID != ms || m.Plan == "" || m.Plan != dataStr(row, "plan") {
		return m, CloseResultOutcome{}, mergeReasonNotClosed
	}
	closeRow, ok := r.opRows(OpMilestoneClose).last(func(ev engine.Event) bool { return dataStr(ev, "milestone") == ms })
	if !ok {
		return m, CloseResultOutcome{}, mergeReasonNotClosed
	}
	var closed CloseResultOutcome
	if err := json.Unmarshal(resultOf(closeRow), &closed); err != nil || closed.Tip == "" || closed.Milestone != ms {
		return m, CloseResultOutcome{}, mergeReasonNotClosed
	}
	if closed.Tip != dataStr(closeRow, "tip") || closed.GateHead != dataStr(closeRow, "gate_head") || closed.GateHead != closed.Tip ||
		closed.Moved || dataBool(closeRow, "moved") {
		// a moved close, a tip that names no gate head, or a data/result disagreement is no closing acceptance
		return m, closed, mergeReasonNotClosed
	}
	if head != "" && closed.Tip != head {
		return m, closed, mergeReasonHeadMoved
	}
	// the close's gate evidence is the pass at the tip of this milestone, and the live latest gate row is that very row —
	// any gate after the close (pass or not) needs a new close
	var gate GateOutcome
	if json.Unmarshal(closed.Gate, &gate) != nil || gate.State != GatePass || gate.Head != closed.Tip || gate.Milestone != ms {
		return m, closed, mergeReasonFinalGate
	}
	gateRow, ok := r.opRows("crw." + NameFinalGate).last(func(ev engine.Event) bool { return dataStr(ev, "milestone") == ms })
	if !ok || gateRow.Seq >= closeRow.Seq {
		return m, closed, mergeReasonFinalGate
	}
	var live GateOutcome
	if err := json.Unmarshal(resultOf(gateRow), &live); err != nil || live.State != GatePass || live.Head != closed.Tip ||
		live.Milestone != ms || dataStr(gateRow, "state") != GatePass || dataStr(gateRow, "head") != closed.Tip {
		return m, closed, mergeReasonFinalGate
	}
	// the closing review: the crosscheck's verified verdict bound to the tip and milestone when the close carries one,
	// else a verified Final under the same binding
	if len(closed.Crosscheck) > 0 && string(closed.Crosscheck) != "null" {
		var v map[string]any
		if json.Unmarshal(closed.Crosscheck, &v) != nil || str(v, "verdict") != "verified" ||
			str(v, "head") != closed.Tip || str(v, "milestone") != ms {
			return m, closed, mergeReasonFinalGate
		}
	} else if !finalVerified(closed.Final) {
		return m, closed, mergeReasonFinalGate
	}
	// and the close was reported up after it: the latest report of the milestone names the tip and follows the close's row
	repRow, ok := r.opRows(OpReportUp).last(func(ev engine.Event) bool { return dataStr(ev, "milestone") == ms })
	if !ok || repRow.Seq <= closeRow.Seq || dataStr(repRow, "tip") != closed.Tip {
		return m, closed, mergeReasonNotClosed
	}
	return m, closed, ""
}

// ---- crw.prOpen (CRW-1423): the parent's pull request opener for a PR-unit milestone ----

// prOpenOp is crw.prOpen(milestone): after the milestone's close the lane asks the parent's opener to open (or reuse) the
// pull request of a bundle — a plan whose pinned handoff names a PRBase. The journal itself proves the closing acceptance
// (closedPRMilestone) before the opener is called; a target with no base, or a lane without the handoff, answers
// not_required without external calls. Refusals answer {state: refused, reason} and open nothing.
type prOpenOp struct{ l *Lane }

func (o *prOpenOp) Run(ctx context.Context, req engine.OpRequest) (engine.OpResult, error) {
	var ms string
	if err := args(req.Args, &ms); err != nil || ms == "" {
		return engine.OpResult{}, fmt.Errorf("crw.prOpen(milestone): a milestone is required")
	}
	r := activeRows(req.Journal.Events())
	plan := planOf(r, ms)
	if o.l.d.PRTarget == nil {
		return engine.OpResult{Result: result(map[string]any{"state": "not_required"}), Data: map[string]any{"milestone": ms, "state": "not_required"}}, nil
	}
	target, err := o.l.d.PRTarget(ctx, plan)
	if err != nil {
		return engine.OpResult{}, fmt.Errorf("crw.prOpen: the pull request target of plan %s: %w", plan, err)
	}
	if target.BaseRef == "" {
		return engine.OpResult{Result: result(map[string]any{"state": "not_required"}), Data: map[string]any{"milestone": ms, "plan": plan, "state": "not_required"}}, nil
	}
	if o.l.d.PROpen == nil {
		return engine.OpResult{}, fmt.Errorf("crw.prOpen: no pull request opener is wired")
	}
	if o.l.d.PullRequests == nil {
		return engine.OpResult{}, fmt.Errorf("crw.prOpen: no pull request reader is wired")
	}
	// the journal proves the close at the target's tip; the live integration tip must still be it
	m, closed, reason := closedPRMilestone(req.Journal.Events(), ms, "")
	if reason != "" {
		return engine.OpResult{Result: result(map[string]any{"state": "refused", "reason": reason}), Data: map[string]any{"milestone": ms, "plan": plan, "state": "refused", "reason": reason}}, nil
	}
	if o.l.d.Relay == nil {
		return engine.OpResult{}, errors.New("crw.prOpen: no relay is wired")
	}
	tip, err := o.l.d.Relay.IntegrationTip(ctx, plan)
	if err != nil {
		return engine.OpResult{}, fmt.Errorf("crw.prOpen: the integration tip of plan %s: %w", plan, err)
	}
	if tip != closed.Tip {
		return engine.OpResult{Result: result(map[string]any{"state": "refused", "reason": mergeReasonHeadMoved}), Data: map[string]any{"milestone": ms, "plan": plan, "state": "refused", "reason": mergeReasonHeadMoved}}, nil
	}
	out, err := o.l.d.PROpen(ctx, BundlePROpenRequest{Target: target, Milestone: m, Close: closed})
	if err != nil {
		return engine.OpResult{}, err
	}
	data := map[string]any{"milestone": ms, "plan": plan, "state": out.State}
	if out.State == "opened" {
		// the opener's answer must name the proven head and base, a pull request ref of the target's repository and a review
		// record path — a bad adapter's output is an error, never a recorded open
		ref, err := tracking.ParseRef(out.PR)
		if err != nil || !strings.EqualFold(ref.Repo, target.Repo) || out.Head != closed.Tip || out.Base != target.BaseRef || out.ReviewRecord == "" {
			return engine.OpResult{}, fmt.Errorf("crw.prOpen: the opener answered %q head %s base %s record %q, want a pull request of %s at %s onto %s",
				out.PR, out.Head, out.Base, out.ReviewRecord, target.Repo, closed.Tip, target.BaseRef)
		}
		data["pr"], data["head"], data["head_ref"], data["base"], data["review_record"] = out.PR, out.Head, target.HeadRef, out.Base, out.ReviewRecord
	} else {
		data["reason"] = out.Reason
	}
	return engine.OpResult{Result: result(out), Data: data}, nil
}

// Proven: not_required is a record (the handoff does not change); an opened row re-reads the pull request and is proven
// only while the same open or merged pull request still names the recorded head and base — a closed-unmerged one is not.
func (o *prOpenOp) Proven(ctx context.Context, ev []engine.Event, row engine.Event) (bool, string, error) {
	switch dataStr(row, "state") {
	case "not_required":
		return true, "", nil
	case "opened":
	default:
		return false, "the open was refused or did not record a state", nil
	}
	if o.l.d.PullRequests == nil {
		return false, "no pull request reader is wired", nil
	}
	ref, err := tracking.ParseRef(dataStr(row, "pr"))
	if err != nil {
		return false, "the row's pull request does not parse", nil
	}
	pr, err := o.l.d.PullRequests.PullRequest(ctx, ref)
	if err != nil {
		return false, "", err
	}
	if !strings.EqualFold(pr.Ref.Repo, ref.Repo) || pr.Ref.Number != ref.Number || !strings.EqualFold(pr.HeadRepo, ref.Repo) ||
		pr.HeadRef != dataStr(row, "head_ref") || pr.HeadSHA != dataStr(row, "head") || pr.BaseRef != dataStr(row, "base") {
		return false, "the pull request's identity changed", nil
	}
	if pr.State != tracking.StateOpen && !pr.Merged {
		return false, "the pull request was closed without a merge", nil
	}
	return true, "", nil
}
