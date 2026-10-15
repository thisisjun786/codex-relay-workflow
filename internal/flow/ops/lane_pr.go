package ops

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
	"github.com/thisisjun786/codex-relay-workflow/internal/tracking"
)

// ---- crw.mergeRequest ----

// opCrosscheck is the op of the crosscheck call (R3, CRW-1442): its completed rows are the crosscheck this op reads. The name is local so
// the package compiles on a dev that does not hold the op yet; flow/pullrequest/run.go keeps the same literal.
const opCrosscheck = "crw.crosscheck"

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
	reviewBotsConnected    = "connected"
	reviewBotsNotConnected = "not_connected"
)

// mergeRequestOp is crw.mergeRequest(milestone, {pr: "OWNER/NAME#N", review: {bots: "connected"|"not_connected", unresolved: N}}): the
// run asks the parent to merge the pull request of a closed milestone. It judges, in order, whether the pull request is open, whether the
// crosscheck accepted its head, whether the head's checks are green and whether review comments are unresolved; a request that fails one
// is answered {state: "refused", reason, pr, head} and writes a row of its own (not a Go error: the script decides). The unresolved count
// is the script's argument: the tracking adapter reads no resolved flag. A request that passes goes to the Reporter once per head.
type mergeRequestOp struct{ l *Lane }

type mergeRequestArg struct {
	PR     string `json:"pr"`
	Review *struct {
		Bots       string `json:"bots"`
		Unresolved *int   `json:"unresolved"`
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
	if arg.Review == nil {
		return engine.OpResult{}, fmt.Errorf("crw.mergeRequest: the review argument ({bots, unresolved}) is required: a request does not skip the comments by default")
	}
	if arg.Review.Bots != reviewBotsConnected && arg.Review.Bots != reviewBotsNotConnected {
		return engine.OpResult{}, fmt.Errorf("crw.mergeRequest: review.bots is %q, want %q or %q", arg.Review.Bots, reviewBotsConnected, reviewBotsNotConnected)
	}
	// connected bots: the count must be given (an absent or null one is not a zero); not_connected: there is nothing to count
	unresolved := 0
	if arg.Review.Unresolved != nil {
		unresolved = *arg.Review.Unresolved
	} else if arg.Review.Bots == reviewBotsConnected {
		return engine.OpResult{}, fmt.Errorf("crw.mergeRequest: review.unresolved is required when the review bots are connected")
	}
	if unresolved < 0 {
		return engine.OpResult{}, fmt.Errorf("crw.mergeRequest: review.unresolved is %d", unresolved)
	}
	if o.l.d.PullRequests == nil {
		return engine.OpResult{}, fmt.Errorf("crw.mergeRequest: no pull request reader is wired")
	}
	requester, ok := o.l.d.Reporter.(MergeRequester)
	if !ok || requester == nil {
		return engine.OpResult{}, fmt.Errorf("crw.mergeRequest: no merge requester is wired (the reporter cannot take a merge request)")
	}
	botsOff := arg.Review.Bots == reviewBotsNotConnected
	r := rows(req.Journal.Events())
	plan := planOf(r, ms)
	pr, err := o.l.d.PullRequests.PullRequest(ctx, ref)
	if err != nil {
		return engine.OpResult{}, fmt.Errorf("crw.mergeRequest: read %s: %w", ref, err)
	}
	head := pr.HeadSHA

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
	cc, reason := mergeCrosscheckOf(r, ms, head)
	if reason != "" {
		return refuse(reason)
	}
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

// Proven: a request is a record, and a recorded one is never sent again (recordOps).
func (o *mergeRequestOp) Proven(ctx context.Context, ev []engine.Event, row engine.Event) (bool, string, error) {
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
	cc := r.opRows(opCrosscheck)
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
