package ops

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"sort"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
)

// This file is the continuity of the architect and the re-plan of a milestone (F09, CRW-1202):
//
//   - the design ledger holds every revision of the milestone (crw.designLedger, lane_design.go): r0, and one revision for every
//     architect delta that passed the design audit again. The handle of the architect thread moves with every completed architect
//     turn - the DESIGN, a REFLECTION (crw.reflection), a DESIGN delta (the next revision) - and crw.currentDesign reads where it
//     is now;
//   - crw.replanSignals counts the two numbers of SP-ARCH-2 from the journal and the plan, and says which reasons to re-plan stand;
//   - the signal path of crw.dag (crw.signalBind, crw.signalDone, crw.signalClose) records which terminal events a signal was made
//     of, in the journal's order, what had started and what had not when it was decided, and what it came to.
//
// A row never changes. A revision, a signal or a reflection that a resume does not replay is not overwritten: the engine ends the
// completed row of its position superseded (the suffix of the resume), and what reads the ledger reads the rows that stand.

// Op names of this file.
const (
	NameReplanSignals = "replanSignals"
	NameCurrentDesign = "currentDesign"
	NameReflection    = "reflection"
	NamePlanRead      = "planRead"
	NameSignalBind    = "signalBind"
	NameSignalDone    = "signalDone"
	NameSignalClose   = "signalClose"
)

// Ops of this file.
const (
	OpReplanSignals = "crw." + NameReplanSignals
	OpCurrentDesign = "crw." + NameCurrentDesign
	OpReflection    = "crw." + NameReflection
	OpPlanRead      = "crw." + NamePlanRead
	OpSignalBind    = "crw." + NameSignalBind
	OpSignalDone    = "crw." + NameSignalDone
	OpSignalClose   = "crw." + NameSignalClose
)

// The flow rows of a revision a resume did not keep.
const (
	// EvReleaseRebind is written for a lane that was released under a revision a resume superseded, when its slice of the plan is
	// the one the revision that stands was made against: the replay of the lane is kept, and the row says which revision it stands
	// on now.
	EvReleaseRebind = "release_rebind"
	// EvRevisionSuperseded is written for a revision whose signal a resume did not replay.
	EvRevisionSuperseded = "revision_superseded"
)

// The reasons SP-ARCH-2 re-plans for.
const (
	ReasonPlanMismatch         = "plan_mismatch"
	ReasonSliceChange          = "slice_change"
	ReasonFinalMisaligned      = "reviewer_final_misaligned"
	ReasonReflectionMisaligned = "reflection_misaligned"
)

// The thresholds of SP-ARCH-2.
const (
	// PlanMismatchLimit is the number of RECEIPTs with a plan_mismatch of the current revision at which the milestone re-plans.
	PlanMismatchLimit = 2
	// SliceChangeOverPercent is the share of the milestone's nodes, in percent, whose slice of the plan changed since the current
	// revision above which the milestone re-plans: exactly this share does not.
	SliceChangeOverPercent = 25
)

// The results of a signal.
const (
	SignalContinue = "continue"
	SignalRefresh  = "refresh"
	SignalHalt     = "halt"
)

// ---- reading the ledger and the signals ----

// supersededAfter maps a call to the sequence number of the last row that ended it superseded.
func (r rows) supersededAfter() map[string]int {
	out := map[string]int{}
	for _, ev := range r {
		if ev.Kind == engine.KindCall && ev.Event == engine.EvSuperseded && ev.Seq > out[ev.CallID] {
			out[ev.CallID] = ev.Seq
		}
	}
	return out
}

// superseded says whether a resume ended the completed row superseded: the position it was written at is in the suffix of a
// resume, and the row does not describe the run any more.
func (r rows) superseded(row engine.Event) bool {
	for _, ev := range r {
		if ev.Seq > row.Seq && ev.Kind == engine.KindCall && ev.Event == engine.EvSuperseded && ev.CallID == row.CallID {
			return true
		}
	}
	return false
}

// current returns the completed rows of an op that no resume superseded, in journal order.
func (r rows) current(op string) rows {
	sup := r.supersededAfter()
	return r.opRows(op).where(func(ev engine.Event) bool { return sup[ev.CallID] < ev.Seq })
}

// ledgerRows are the revisions of the milestone's ledger that stand, oldest first.
func ledgerRows(r rows, ms string) rows {
	return r.current(OpDesignLedger).where(func(ev engine.Event) bool { return dataStr(ev, "milestone") == ms })
}

// currentLedger is the newest revision that stands.
func currentLedger(r rows, ms string) (engine.Event, bool) {
	l := ledgerRows(r, ms)
	if len(l) == 0 {
		return engine.Event{}, false
	}
	return l[len(l)-1], true
}

// firstLedger is the oldest revision that stands: the DESIGN the milestone started from.
func firstLedger(r rows, ms string) (engine.Event, bool) {
	l := ledgerRows(r, ms)
	if len(l) == 0 {
		return engine.Event{}, false
	}
	return l[0], true
}

// nextRevision names the revision the ledger adds next. A revision id is used once: the revisions a resume superseded keep their
// ids, so a row that names a revision names one revision.
func nextRevision(r rows, ms string) string {
	n := 0
	for _, ev := range r.opRows(OpDesignLedger) {
		if dataStr(ev, "milestone") == ms {
			n++
		}
	}
	return fmt.Sprintf("r%d", n)
}

// revisionSuperseded says whether a revision's ledger row was ended by a resume. A revision the ledger does not know is not.
func revisionSuperseded(r rows, ms, revision string) bool {
	row, ok := ledgerOf(r, ms, revision)
	return ok && r.superseded(row)
}

// ledgerSlices are the slice digests of the plan's nodes a revision was made against.
func ledgerSlices(led engine.Event) map[string]string {
	out := map[string]string{}
	if m, ok := led.Data["slices"].(map[string]any); ok {
		for k, v := range m {
			if s, ok := v.(string); ok {
				out[k] = s
			}
		}
	}
	return out
}

// architectHandle is the handle of the architect thread: that of the newest completed architect turn that stands, a DESIGN of the
// ledger or a REFLECTION.
func architectHandle(r rows, ms string) (handle, call string) {
	seq := -1
	for _, op := range []string{OpDesignLedger, OpReflection} {
		for _, ev := range r.current(op) {
			if dataStr(ev, "milestone") != ms || dataStr(ev, "architect_handle") == "" || ev.Seq < seq {
				continue
			}
			seq, handle, call = ev.Seq, dataStr(ev, "architect_handle"), dataStr(ev, "architect_call")
		}
	}
	return handle, call
}

// approvedAuditAfter is an approved audit of the candidate that was made after the row with the sequence number after, and that
// no resume superseded: the audit a revision needs, which an audit of an earlier DESIGN of the same digest does not stand in for.
func approvedAuditAfter(r rows, ms, digest string, after int) (engine.Event, bool) {
	return r.current(OpDesignAudit).last(func(ev engine.Event) bool {
		return ev.Seq > after && dataStr(ev, "milestone") == ms && dataStr(ev, "candidate_digest") == digest && dataStr(ev, "state") == AuditApproved
	})
}

// acceptedAfter says whether an acceptance of the node was recorded after the row.
func acceptedAfter(r rows, node string, after engine.Event) bool {
	_, ok := r.opRows(OpAccept).last(func(ev engine.Event) bool { return ev.Seq > after.Seq && dataStr(ev, "node") == node })
	return ok
}

// releaseRevision is the revision a node stands on: the one it was released under, or the one a release_rebind row moved it to.
func releaseRevision(r rows, node string) (rev string, rel engine.Event, ok bool) {
	rel, ok = releaseRowOf(r, node)
	if !ok {
		return "", engine.Event{}, false
	}
	rev = dataStr(rel, "revision")
	for _, ev := range r.flow(EvReleaseRebind) {
		if dataStr(ev, "node") == node && dataStr(ev, "release_call") == rel.CallID && ev.Seq > rel.Seq {
			rev = dataStr(ev, "to_revision")
		}
	}
	return rev, rel, true
}

// rebind writes the release_rebind row of a lane that keeps its replay under the revision that stands (once).
func (l *Lane) rebind(r rows, rel, cur engine.Event) {
	if l.d.Journal == nil {
		return
	}
	node, to := dataStr(rel, "node"), dataStr(cur, "revision")
	for _, ev := range r.flow(EvReleaseRebind) {
		if dataStr(ev, "node") == node && dataStr(ev, "release_call") == rel.CallID && dataStr(ev, "to_revision") == to {
			return
		}
	}
	l.d.Journal.Append(engine.Event{RunID: l.d.RunID, Kind: engine.KindFlow, CallID: rel.CallID, Op: OpRelease, Event: EvReleaseRebind, Data: map[string]any{
		"milestone": dataStr(rel, "milestone"), "node": node, "release_call": rel.CallID, "from_revision": dataStr(rel, "revision"), "to_revision": to,
		"slice_digest": dataStr(rel, "slice_digest"), "reason": "slice_unchanged"}})
}

// markSuperseded writes the revision_superseded row of every revision of the milestone whose ledger row a resume ended, once.
func (l *Lane) markSuperseded(r rows, ms string) []string {
	if l.d.Journal == nil {
		return nil
	}
	var out []string
	for _, ev := range r.opRows(OpDesignLedger) {
		if dataStr(ev, "milestone") != ms || !r.superseded(ev) {
			continue
		}
		rev := dataStr(ev, "revision")
		written := false
		for _, f := range r.flow(EvRevisionSuperseded) {
			if dataStr(f, "milestone") == ms && dataStr(f, "revision") == rev {
				written = true
			}
		}
		if written {
			continue
		}
		l.d.Journal.Append(engine.Event{RunID: l.d.RunID, Kind: engine.KindFlow, CallID: ev.CallID, Op: OpDesignLedger, Event: EvRevisionSuperseded, Data: map[string]any{
			"milestone": ms, "revision": rev, "digest": dataStr(ev, "digest"), "signal": ev.Data["signal"], "reason": "signal_not_replayed"}})
		out = append(out, rev)
	}
	return out
}

// ---- the signal rows ----

// Trigger is a terminal event a signal is made of: the RECEIPT of a lane (kind receipt: the call and the digest of its result) or a
// plan revision a lane read when it was about to be released (kind revision: `plan:<occurrence>:<digest>` and the digest). The plan the signal
// itself read is recorded on its row (revision_read).
type Trigger struct {
	Kind   string `json:"kind"`
	Call   string `json:"call"`
	Node   string `json:"node"`
	Digest string `json:"digest"`
}

// signalBinds are the signal rows of the milestone that stand, oldest first.
func signalBinds(r rows, ms string) rows {
	return r.current(OpSignalBind).where(func(ev engine.Event) bool { return dataStr(ev, "milestone") == ms })
}

// triggersOf reads the triggers a row names.
func triggersOf(ev engine.Event) []Trigger {
	raw, _ := json.Marshal(ev.Data["members"])
	var out []Trigger
	_ = json.Unmarshal(raw, &out)
	return out
}

// lastPlanDigest is the digest of the plan the last signal that stands read, or the one the first revision was made against.
func lastPlanDigest(r rows, ms string) string {
	if b := signalBinds(r, ms); len(b) > 0 {
		return dataStr(b[len(b)-1], "plan_digest")
	}
	if led, ok := firstLedger(r, ms); ok {
		return dataStr(led, "milestone_digest")
	}
	return ""
}

// pinnedPlan is the plan the audited DESIGN of the milestone stands on (CRW-1202): the plan its current revision was made for - the plan
// the decision that made the revision evaluated, the milestone the run read for r0 - or, when a signal after that revision judged another
// plan and found no reason to re-plan (continue), the plan the newest such signal judged, which it kept the revision for. A release is a
// compare-and-swap on it (crw.release), and a plan read is a trigger exactly when the plan it reads is another (crw.planRead): the plan
// moves the pin only through a signal that judged it, and the revision only through a re-plan whose delta was audited for it.
func pinnedPlan(r rows, ms string) string {
	pin, _, _ := planPins(r, ms)
	return pin
}

// planPins reads the plan the audited DESIGN stands on (pin) and, when a signal after the current revision is bound and not yet decided,
// the plan that signal read and its number: a plan a signal in flight is judging is not news to a lane - the lane waits for that signal
// instead of bringing it again - but nothing is released under it before the signal decided it.
func planPins(r rows, ms string) (pin, judging string, k int) {
	led, ok := currentLedger(r, ms)
	if !ok {
		return "", "", 0
	}
	pin = dataStr(led, "milestone_digest")
	sup := r.supersededAfter()
	judged := map[int]string{}
	for _, ev := range r {
		if ev.Seq <= led.Seq || ev.Kind != engine.KindCall || ev.Event != engine.EvCompleted || sup[ev.CallID] >= ev.Seq || dataStr(ev, "milestone") != ms {
			continue
		}
		switch ev.Op {
		case OpSignalBind:
			judging, k = dataStr(ev, "plan_digest"), intOf(ev.Data["k"])
		case OpReplanSignals:
			if dataStr(ev, "phase") == "signal" {
				judged[intOf(ev.Data["signal"])] = dataStr(ev, "milestone_digest")
			}
		case OpSignalDone:
			if intOf(ev.Data["k"]) == k {
				judging, k = "", 0
			}
			if dataStr(ev, "outcome") == SignalContinue {
				if d := judged[intOf(ev.Data["k"])]; d != "" {
					pin = d
				}
			}
		}
	}
	return pin, judging, k
}

// planObserved is the plan digest a row observed: the plan a lane read before its release, the plan a signal read when it was bound,
// and the plan the decision of a signal judged.
func planObserved(ev engine.Event) string {
	switch ev.Op {
	case OpPlanRead, OpSignalBind:
		return dataStr(ev, "plan_digest")
	case OpReplanSignals:
		if dataStr(ev, "phase") == "signal" {
			return dataStr(ev, "milestone_digest")
		}
	}
	return ""
}

// planOccurrence names the occurrence of a plan digest: after the newest observation that stands of another plan. A plan that returns
// after a signal judged another is a new occurrence, so its trigger is another than the one that signal may have taken.
func planOccurrence(r rows, ms, digest string) string {
	superseded := r.supersededAfter()
	for index := len(r) - 1; index >= 0; index-- {
		observation := r[index]
		if observation.Kind != engine.KindCall || observation.Event != engine.EvCompleted ||
			dataStr(observation, "milestone") != ms || superseded[observation.CallID] >= observation.Seq {
			continue
		}
		if prior := planObserved(observation); prior != "" && prior != digest {
			return fmt.Sprintf("after:%d", observation.Seq)
		}
	}
	if first, ok := firstLedger(r, ms); ok {
		return fmt.Sprintf("after:%d", first.Seq)
	}
	return "initial"
}

func planTriggerCall(occurrence, digest string) string {
	if occurrence == "" {
		return "plan:" + digest
	}
	return "plan:" + occurrence + ":" + digest
}

// lanes says which nodes of the milestone had started and which had not: a node is started from its release on.
func lanes(r rows, nodes []string) (started, unstarted []string) {
	started, unstarted = []string{}, []string{}
	for _, n := range nodes {
		if rel, ok := releaseRowOf(r, n); ok && !r.superseded(rel) {
			started = append(started, n)
		} else {
			unstarted = append(unstarted, n)
		}
	}
	return started, unstarted
}

// milestoneNodes are the nodes of the milestone in the order the scheduler offers them, as the run read them first.
func milestoneNodes(r rows, ms string) []string {
	m, _, err := milestoneOf(r, ms)
	if err != nil {
		return nil
	}
	out := make([]string, 0, len(m.Nodes))
	for _, n := range m.Nodes {
		out = append(out, n.ID)
	}
	return out
}

// planReadOp is crw.planRead(milestone, node): the plan revision a lane reads before it asks for its release - the digest of the
// milestone as the relay has it now, and whether it is another than the plan the audited DESIGN stands on (pinnedPlan): the plan the
// current revision was made for, or the one a later signal judged and kept it for. A changed
// plan revision is a trigger of the signal path, so the release of the node waits until the signal that comes of it is decided and
// finds the revision that stands then. The row is the replay: a resume that finds the node released after this position keeps what
// it read, and so does one that finds a signal row that stands and took the revision it read as a trigger (the signal is replayed
// from its rows, so the read it was made of is too); one that finds neither reads the plan again.
type planReadOp struct{ l *Lane }

// PlanSeen is the answer of crw.planRead.
type PlanSeen struct {
	Milestone  string `json:"milestone"`
	Node       string `json:"node"`
	Digest     string `json:"digest"`
	Occurrence string `json:"occurrence"`
	// Base is the plan the audited DESIGN stands on (pinnedPlan): the plan crw.release compares the plan it releases under with.
	Base    string `json:"base"`
	Changed bool   `json:"changed"`
	// Judging is the number of the signal in flight (bound, not yet decided) that read this very plan: the read is no trigger of its
	// own, and the release of the node waits until that signal is decided.
	Judging int `json:"judging"`
	// CallID is the position of the read: crw.callFacts says by it whether this process replayed the row.
	CallID string `json:"call_id"`
}

func (o *planReadOp) Run(ctx context.Context, req engine.OpRequest) (engine.OpResult, error) {
	var ms, node string
	if err := args(req.Args, &ms, &node); err != nil {
		return engine.OpResult{}, fmt.Errorf("crw.planRead(milestone, node): %w", err)
	}
	r := rows(req.Journal.Events())
	live, err := o.l.liveMilestone(ctx, planOf(r, ms), ms)
	if err != nil {
		return engine.OpResult{}, err
	}
	pin, judging, k := planPins(r, ms)
	out := PlanSeen{Milestone: ms, Node: node, Digest: digestOf(live), Base: pin, CallID: req.CallID}
	out.Occurrence = planOccurrence(r, ms, out.Digest)
	switch {
	case k > 0 && out.Digest == judging:
		out.Judging = k
	default:
		out.Changed = out.Base != "" && out.Digest != out.Base
	}
	return engine.OpResult{Result: result(out), Data: map[string]any{"milestone": ms, "node": node, "plan_digest": out.Digest, "occurrence": out.Occurrence, "base": out.Base, "changed": out.Changed, "judging": out.Judging}}, nil
}

// Proven: the read stands while the node was released after it (the lane went on from what it read), and while a signal row that
// stands took the changed revision it read as a trigger: read again, the plan would answer the revision that signal read as its own
// base (changed=false), the trigger would not come, and the signal it was made of could not replay. A read that neither the node nor
// a signal went on from is made again.
func (o *planReadOp) Proven(_ context.Context, ev []engine.Event, row engine.Event) (bool, string, error) {
	r := rows(ev)
	node := dataStr(row, "node")
	if _, ok := r.current(OpRelease).last(func(e engine.Event) bool { return e.Seq > row.Seq && dataStr(e, "node") == node }); ok {
		return true, "", nil
	}
	if changed, _ := row.Data["changed"].(bool); changed {
		call := planTriggerCall(dataStr(row, "occurrence"), dataStr(row, "plan_digest"))
		for _, b := range signalBinds(r, dataStr(row, "milestone")) {
			if slices.ContainsFunc(triggersOf(b), func(t Trigger) bool { return t.Call == call }) {
				return true, "", nil
			}
		}
	}
	return false, "the node was not released after the plan was read and no signal took the revision it read", nil
}

// signalBindOp is crw.signalBind(milestone, k, prev, members, attempt): the signal row. The arguments are the signal's input: the number
// of the signal, the triggers as (call, digest) and the result digest of the signal before it, and attempt, which the script names
// (live:<n>, one more than the attempt of the recorded row) only for a signal the earlier process recorded and that cannot replay because
// one of its triggers ran again: the triggers of such a signal can answer the very digests they answered then, and the arguments are then
// another input for that reason alone - also when the recorded row is itself a live attempt of an earlier resume - so the position runs again and the signal and the revision it made are the suffix of the resume. A resume replays the row only when
// the script hands it the same arguments - which it does only when every trigger position replayed and answered the same result
// digest (crw.callFacts) and the signal before it replayed - so a signal one of whose triggers ran again, and every signal after
// it, are the suffix of the resume. The row records what had started and what had not at that moment, the tip of the integration
// target (the head an architect turn of this signal judges) and the plan revision read for it, and names the revisions the resume
// superseded.
type signalBindOp struct{ l *Lane }

// liveAttempt is the attempt of a live signal: live:<n>, n one more than the attempt of the row it does not replay ("live" is the
// attempt of a row bound before attempts were numbered, and counts as 1).
var liveAttempt = regexp.MustCompile(`^live(:[1-9][0-9]*)?$`)

// PlanRead is the plan revision a signal read.
type PlanRead struct {
	Digest   string `json:"digest"`
	Previous string `json:"previous"`
	// Changed is set when the plan is not the one the signal before read: a new plan revision is a trigger of the signal.
	Changed bool `json:"changed"`
}

// SignalBind is the answer of crw.signalBind.
type SignalBind struct {
	Milestone  string    `json:"milestone"`
	K          int       `json:"k"`
	Prev       string    `json:"prev"`
	Members    []Trigger `json:"members"`
	Started    []string  `json:"started"`
	Unstarted  []string  `json:"unstarted"`
	Tip        string    `json:"tip"`
	Revision   string    `json:"revision"`
	PlanRead   PlanRead  `json:"revision_read"`
	Superseded []string  `json:"superseded"`
}

func (o *signalBindOp) Run(ctx context.Context, req engine.OpRequest) (engine.OpResult, error) {
	var ms, prev, attempt string
	var k int
	var members []Trigger
	if err := args(req.Args, &ms, &k, &prev, &members, &attempt); err != nil {
		return engine.OpResult{}, fmt.Errorf("crw.signalBind(milestone, k, prev, members, attempt): %w", err)
	}
	if attempt != "" && !liveAttempt.MatchString(attempt) {
		return engine.OpResult{}, fmt.Errorf("crw.signalBind: attempt %q is not \"live:<n>\"", attempt)
	}
	if len(members) == 0 {
		return engine.OpResult{}, fmt.Errorf("crw.signalBind: signal %d of milestone %s has no trigger", k, ms)
	}
	r := rows(req.Journal.Events())
	led, ok := currentLedger(r, ms)
	if !ok {
		return engine.OpResult{}, fmt.Errorf("crw.signalBind: milestone %s has no design revision", ms)
	}
	plan := planOf(r, ms)
	tip, err := o.l.d.Relay.IntegrationTip(ctx, plan)
	if err != nil {
		return engine.OpResult{}, err
	}
	live, err := o.l.liveMilestone(ctx, plan, ms)
	if err != nil {
		return engine.OpResult{}, err
	}
	out := SignalBind{Milestone: ms, K: k, Prev: prev, Members: members, Tip: tip, Revision: dataStr(led, "revision")}
	out.PlanRead = PlanRead{Digest: digestOf(live), Previous: lastPlanDigest(r, ms)}
	out.PlanRead.Changed = out.PlanRead.Previous != "" && out.PlanRead.Digest != out.PlanRead.Previous
	out.Started, out.Unstarted = lanes(r, milestoneNodes(r, ms))
	out.Superseded = o.l.markSuperseded(r, ms)
	if out.Superseded == nil {
		out.Superseded = []string{}
	}
	return engine.OpResult{Result: result(out), Data: map[string]any{
		"milestone": ms, "k": k, "prev": prev, "members": members, "started": out.Started, "unstarted": out.Unstarted, "tip": tip, "revision": out.Revision,
		"plan_digest": out.PlanRead.Digest, "revision_read": out.PlanRead, "superseded": out.Superseded, "attempt": attempt,
	}}, nil
}

func (o *signalBindOp) Proven(ctx context.Context, ev []engine.Event, row engine.Event) (bool, string, error) {
	return provenAlways(ctx, ev, row)
}

// signalDoneOp is crw.signalDone(milestone, k, outcome): the result of a signal - continue, refresh (a new revision was added and
// the unstarted lanes are released under it) or halt (the milestone is held or escalated) - with the id of the revision it made. The
// digest of the result is the input of the signal after it.
type signalDoneOp struct{ l *Lane }

// SignalOutcome is what the signal path came to.
type SignalOutcome struct {
	Result   string   `json:"result"`
	Revision string   `json:"revision"`
	Reasons  []string `json:"reasons"`
}

// SignalDone is the answer of crw.signalDone.
type SignalDone struct {
	Milestone string `json:"milestone"`
	K         int    `json:"k"`
	Result    string `json:"result"`
	Revision  string `json:"revision"`
	Digest    string `json:"digest"`
}

func (o *signalDoneOp) Run(_ context.Context, req engine.OpRequest) (engine.OpResult, error) {
	var ms string
	var k int
	var oc SignalOutcome
	if err := args(req.Args, &ms, &k, &oc); err != nil {
		return engine.OpResult{}, fmt.Errorf("crw.signalDone(milestone, k, outcome): %w", err)
	}
	if !slices.Contains([]string{SignalContinue, SignalRefresh, SignalHalt}, oc.Result) {
		return engine.OpResult{}, fmt.Errorf("crw.signalDone: %q is no result of a signal (continue, refresh, halt)", oc.Result)
	}
	out := SignalDone{Milestone: ms, K: k, Result: oc.Result, Revision: oc.Revision}
	out.Digest = digestOf(map[string]any{"milestone": ms, "k": k, "result": oc.Result, "revision": oc.Revision})
	return engine.OpResult{Result: result(out), Data: map[string]any{"milestone": ms, "k": k, "outcome": oc.Result, "revision": oc.Revision, "reasons": oc.Reasons, "digest": out.Digest}}, nil
}

func (o *signalDoneOp) Proven(ctx context.Context, ev []engine.Event, row engine.Event) (bool, string, error) {
	return provenAlways(ctx, ev, row)
}

// signalCloseOp is crw.signalClose(milestone, summary): the end of the signal path of a milestone. A resume that does not replay a
// signal the earlier process recorded ends at this position, so the rows of the signals it did not replay are superseded, and
// the revisions they made are named.
type signalCloseOp struct{ l *Lane }

func (o *signalCloseOp) Run(_ context.Context, req engine.OpRequest) (engine.OpResult, error) {
	var ms string
	var summary struct {
		Signals int    `json:"signals"`
		Digest  string `json:"digest"`
	}
	if err := args(req.Args, &ms, &summary); err != nil {
		return engine.OpResult{}, fmt.Errorf("crw.signalClose(milestone, summary): %w", err)
	}
	r := rows(req.Journal.Events())
	sup := o.l.markSuperseded(r, ms)
	if sup == nil {
		sup = []string{}
	}
	return engine.OpResult{Result: result(map[string]any{"milestone": ms, "signals": summary.Signals, "superseded": sup}),
		Data: map[string]any{"milestone": ms, "signals": summary.Signals, "digest": summary.Digest, "superseded": sup}}, nil
}

func (o *signalCloseOp) Proven(ctx context.Context, ev []engine.Event, row engine.Event) (bool, string, error) {
	return provenAlways(ctx, ev, row)
}

// ---- crw.replanSignals ----

// replanSignalsOp is crw.replanSignals(milestone, opts): the two numbers of SP-ARCH-2, counted from the journal and the plan as
// they are now and written to the journal, and the reasons that stand.
//
//   - plan_mismatch: the RECEIPTs of the signals so far that report a plan_mismatch, of lanes released under the current revision,
//     each call once (a call a resume replays is the same call). A RECEIPT of a lane released under an older revision does not
//     count in the new revision's number.
//   - slice change: the share of the milestone's nodes whose slice digest of the plan now is not the one the current revision was
//     made against (a node the plan added or dropped is a change), read from the relay now.
//
// The reasons are plan_mismatch at PlanMismatchLimit or more, a slice change above SliceChangeOverPercent (exactly that share
// does not re-plan), and, at the close, a MISALIGNED reviewer-final and a MISALIGNED REFLECTION. A reason is enough, whatever the
// alignment says.
type replanSignalsOp struct{ l *Lane }

// ReplanOpts say where in the milestone the numbers are asked for.
type ReplanOpts struct {
	// Phase is "signal" (a signal path of crw.dag) or "close" (after the milestone close).
	Phase string `json:"phase"`
	// Signal is the number of the signal the numbers are asked for; the signals after it are not counted.
	Signal int `json:"signal"`
	// Final and Reflection are the alignments the close adds: ALIGNED, MISALIGNED or NOT_ASSESSED.
	Final      string `json:"final"`
	Reflection string `json:"reflection"`
}

// Landed is the head a node landed with.
type Landed struct {
	Node  string `json:"node"`
	Head  string `json:"head"`
	Batch string `json:"batch_commit"`
}

// SliceChange is the second number of SP-ARCH-2.
type SliceChange struct {
	Changed []string `json:"changed"`
	Total   int      `json:"total"`
	Percent float64  `json:"percent"`
}

// Due says whether the change is a reason to re-plan: more than SliceChangeOverPercent of the nodes. Exactly that share is not.
func (c SliceChange) Due() bool {
	return c.Total > 0 && len(c.Changed)*100 > SliceChangeOverPercent*c.Total
}

// ReplanState is the answer of crw.replanSignals.
type ReplanState struct {
	Milestone string   `json:"milestone"`
	Phase     string   `json:"phase"`
	Signal    int      `json:"signal"`
	Revision  string   `json:"revision"`
	Reasons   []string `json:"reasons"`
	Due       bool     `json:"due"`
	// PlanMismatch is the number of distinct RECEIPTs with a plan_mismatch of the lanes released under the current revision, and
	// Calls their ids.
	PlanMismatch int         `json:"plan_mismatch"`
	Calls        []string    `json:"plan_mismatch_calls"`
	Slices       SliceChange `json:"slice_change"`
	// Tip is the integration target as this decision read it; Base is the head the DESIGN of the current revision ran at, and
	// Landed the heads the nodes landed with.
	Tip             string   `json:"tip"`
	Base            string   `json:"base"`
	Landed          []Landed `json:"landed"`
	MilestoneDigest string   `json:"milestone_digest"`
	// Plan is the plan this decision evaluated: the architect's delta and its audit are made against it, and the ledger revision the
	// delta becomes records it (LedgerEntry.Plan), whatever the plan is by the time the revision is written.
	Plan       PlanSnapshot `json:"plan"`
	Final      string       `json:"final"`
	Reflection string       `json:"reflection"`
}

// PlanSnapshot is a plan as one decision read it: the digest of the milestone and the slice digest of each of its nodes.
type PlanSnapshot struct {
	Digest string            `json:"digest"`
	Slices map[string]string `json:"slices"`
}

// receiptOf reads the RECEIPT a call completed with: its plan_mismatch and the sequence number of its terminal row.
func receiptOf(r rows, call string) (mismatch string, seq int, ok bool) {
	term, found := r.terminal(call)
	if !found || term.Event != engine.EvCompleted {
		return "", 0, false
	}
	var rc struct {
		PlanMismatch *string `json:"plan_mismatch"`
	}
	if json.Unmarshal([]byte(str(term.Data, "result")), &rc) != nil {
		return "", 0, false
	}
	if rc.PlanMismatch != nil {
		mismatch = *rc.PlanMismatch
	}
	return mismatch, term.Seq, true
}

func (o *replanSignalsOp) Run(ctx context.Context, req engine.OpRequest) (engine.OpResult, error) {
	var ms string
	var opts ReplanOpts
	if err := args(req.Args, &ms, &opts); err != nil {
		return engine.OpResult{}, fmt.Errorf("crw.replanSignals(milestone, opts): %w", err)
	}
	if opts.Phase == "" {
		opts.Phase = "signal"
	}
	r := rows(req.Journal.Events())
	led, ok := currentLedger(r, ms)
	if !ok {
		return engine.OpResult{}, fmt.Errorf("crw.replanSignals: milestone %s has no design revision", ms)
	}
	st := ReplanState{Milestone: ms, Phase: opts.Phase, Signal: opts.Signal, Revision: dataStr(led, "revision"), Reasons: []string{}, Calls: []string{}, Landed: []Landed{},
		Final: opts.Final, Reflection: opts.Reflection, Base: dataStr(led, "base_tip")}
	seen := map[string]bool{}
	for _, b := range signalBinds(r, ms) {
		if opts.Phase == "signal" && opts.Signal > 0 && intOf(b.Data["k"]) > opts.Signal {
			continue
		}
		for _, t := range triggersOf(b) {
			if t.Kind != "receipt" || seen[t.Call] {
				continue
			}
			seen[t.Call] = true
			mismatch, seq, ok := receiptOf(r, t.Call)
			if !ok || mismatch == "" {
				continue
			}
			rev, rel, released := releaseRevision(r, t.Node)
			if !released || rev != st.Revision || seq < rel.Seq {
				continue
			}
			st.Calls = append(st.Calls, t.Call)
		}
	}
	st.PlanMismatch = len(st.Calls)
	plan := planOf(r, ms)
	live, err := o.l.liveMilestone(ctx, plan, ms)
	if err != nil {
		return engine.OpResult{}, err
	}
	st.MilestoneDigest = digestOf(live)
	st.Plan = PlanSnapshot{Digest: st.MilestoneDigest, Slices: liveSlices(live)}
	st.Slices = sliceChange(ledgerSlices(led), liveSlices(live))
	if st.Tip, err = o.l.d.Relay.IntegrationTip(ctx, plan); err != nil {
		return engine.OpResult{}, err
	}
	st.Landed = landedHeads(r)
	if st.PlanMismatch >= PlanMismatchLimit {
		st.Reasons = append(st.Reasons, ReasonPlanMismatch)
	}
	if st.Slices.Due() {
		st.Reasons = append(st.Reasons, ReasonSliceChange)
	}
	if opts.Phase == "close" {
		if opts.Final == "MISALIGNED" {
			st.Reasons = append(st.Reasons, ReasonFinalMisaligned)
		}
		if opts.Reflection == "MISALIGNED" {
			st.Reasons = append(st.Reasons, ReasonReflectionMisaligned)
		}
	}
	st.Due = len(st.Reasons) > 0
	return engine.OpResult{Result: result(st), Data: map[string]any{
		"milestone": ms, "phase": opts.Phase, "signal": opts.Signal, "revision": st.Revision, "plan_mismatch": st.PlanMismatch, "plan_mismatch_calls": st.Calls,
		"slices_changed": len(st.Slices.Changed), "slices_total": st.Slices.Total, "slice_change_percent": st.Slices.Percent, "reasons": st.Reasons, "due": st.Due,
		"tip": st.Tip, "milestone_digest": st.MilestoneDigest, "final": opts.Final, "reflection": opts.Reflection,
	}}, nil
}

func (o *replanSignalsOp) Proven(ctx context.Context, ev []engine.Event, row engine.Event) (bool, string, error) {
	return provenAlways(ctx, ev, row)
}

func intOf(v any) int {
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

// liveMilestone reads the milestone from the relay as crw.milestone records it.
func (l *Lane) liveMilestone(ctx context.Context, plan, ms string) (Milestone, error) {
	m, err := l.d.Relay.Milestone(ctx, plan, ms)
	if err != nil {
		return Milestone{}, err
	}
	normalizeMilestone(&m)
	return m, nil
}

// liveSlices are the slice digests of a milestone's nodes.
func liveSlices(m Milestone) map[string]string {
	out := map[string]string{}
	for _, n := range m.Nodes {
		out[n.ID] = n.SliceDigest
	}
	return out
}

// sliceChange compares the slice digests of a revision with those of the plan now. A node only one side has is changed.
func sliceChange(then, now map[string]string) SliceChange {
	all := map[string]bool{}
	for k := range then {
		all[k] = true
	}
	for k := range now {
		all[k] = true
	}
	sc := SliceChange{Changed: []string{}, Total: len(all)}
	for k := range all {
		if a, ok := then[k]; !ok || a != now[k] {
			sc.Changed = append(sc.Changed, k)
		}
	}
	sort.Strings(sc.Changed)
	if sc.Total > 0 {
		sc.Percent = float64(len(sc.Changed)) * 100 / float64(sc.Total)
	}
	return sc
}

// landedHeads are the heads the nodes landed with, in the order they landed.
func landedHeads(r rows) []Landed {
	out := []Landed{}
	for _, ev := range r.current(OpIntegrate) {
		out = append(out, Landed{Node: dataStr(ev, "node"), Head: dataStr(ev, "head"), Batch: dataStr(ev, "batch_commit")})
	}
	return out
}

// ---- crw.currentDesign ----

// currentDesignOp is crw.currentDesign(milestone): the design the milestone stands on now, read from the ledger - the revision that
// stands, its DESIGN and answers, the handle of the architect thread (that of the newest completed architect turn) and the heads
// the nodes landed with. A script holds none of it in a variable a lane could read: it asks.
type currentDesignOp struct{ l *Lane }

// CurrentDesign is the answer of crw.currentDesign.
type CurrentDesign struct {
	Milestone string            `json:"milestone"`
	Revision  string            `json:"revision"`
	Digest    string            `json:"digest"`
	Handle    string            `json:"handle"`
	Call      string            `json:"call"`
	BaseTip   string            `json:"base_tip"`
	Design    json.RawMessage   `json:"design"`
	Answers   json.RawMessage   `json:"answers"`
	Slices    map[string]string `json:"slices"`
	Landed    []Landed          `json:"landed"`
}

func (o *currentDesignOp) Run(_ context.Context, req engine.OpRequest) (engine.OpResult, error) {
	var ms string
	if err := args(req.Args, &ms); err != nil {
		return engine.OpResult{}, fmt.Errorf("crw.currentDesign(milestone): %w", err)
	}
	r := rows(req.Journal.Events())
	led, ok := currentLedger(r, ms)
	if !ok {
		return engine.OpResult{}, fmt.Errorf("crw.currentDesign: milestone %s has no design revision", ms)
	}
	cd := CurrentDesign{Milestone: ms, Revision: dataStr(led, "revision"), Digest: dataStr(led, "digest"), BaseTip: dataStr(led, "base_tip"), Slices: ledgerSlices(led), Landed: landedHeads(r)}
	cd.Handle, cd.Call = architectHandle(r, ms)
	cd.Design, _ = json.Marshal(led.Data["design"])
	cd.Answers, _ = json.Marshal(led.Data["answers"])
	if string(cd.Answers) == "null" {
		cd.Answers = json.RawMessage(`[]`)
	}
	return engine.OpResult{Result: result(cd), Data: map[string]any{"milestone": ms, "revision": cd.Revision, "digest": cd.Digest, "architect_handle": cd.Handle}}, nil
}

func (o *currentDesignOp) Proven(ctx context.Context, ev []engine.Event, row engine.Event) (bool, string, error) {
	return provenAlways(ctx, ev, row)
}

// ---- crw.reflection ----

// reflectionOp is crw.reflection(milestone, gate, call): the record of the architect's REFLECTION at the milestone close
// (SP-ARCH-3) - the architect checks each decision of the DESIGN against the integrated range, on its own thread. Only a completed,
// valid REFLECTION is a verdict; a call that did not complete (held, unknown, refused, null_model, superseded) is never read as
// ALIGNED. A completed reflection moves the architect's handle on to its call, so the re-plan that follows is the next turn of the
// same thread.
type reflectionOp struct{ l *Lane }

// ReflectionResult is the answer of crw.reflection.
type ReflectionResult struct {
	State      string       `json:"state"`
	Reason     string       `json:"reason"`
	Milestone  string       `json:"milestone"`
	Head       string       `json:"head"`
	Alignment  string       `json:"alignment"`
	Misaligned []Misaligned `json:"misaligned"`
	Call       Brief        `json:"call"`
}

func (o *reflectionOp) Run(_ context.Context, req engine.OpRequest) (engine.OpResult, error) {
	var ms string
	var gate struct {
		Head  string `json:"head"`
		Range string `json:"range"`
	}
	var call CallRef
	if err := args(req.Args, &ms, &gate, &call); err != nil {
		return engine.OpResult{}, fmt.Errorf("crw.reflection(milestone, gate, call): %w", err)
	}
	res := ReflectionResult{Milestone: ms, Head: gate.Head, Misaligned: []Misaligned{}, Call: call.brief()}
	handle := ""
	switch call.Status {
	case "completed":
		refl, err := DecodeReflection(call.Output)
		if err != nil {
			res.State, res.Reason = StateNullModel, "reflection_invalid"
			break
		}
		res.State, res.Alignment, handle = StateVerdict, refl.Verdict, call.Handle
		if refl.Misaligned != nil {
			res.Misaligned = refl.Misaligned
		}
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
		"milestone": ms, "head": gate.Head, "range": gate.Range, "state": res.State, "reason": res.Reason, "alignment": res.Alignment,
		"architect_call": call.CallID, "architect_handle": handle, "misaligned": len(res.Misaligned),
	}}, nil
}

func (o *reflectionOp) Proven(ctx context.Context, ev []engine.Event, row engine.Event) (bool, string, error) {
	return provenAlways(ctx, ev, row)
}
