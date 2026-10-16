package ops

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/execution"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
)

// ---- crw.milestone ----

// milestoneOp is crw.milestone(plan, milestone): the milestone's nodes in the order the scheduler offers them, with the edges and
// the edit regions of each.
type milestoneOp struct{ l *Lane }

func (o *milestoneOp) Run(ctx context.Context, req engine.OpRequest) (engine.OpResult, error) {
	var plan, id string
	if err := args(req.Args, &plan, &id); err != nil {
		return engine.OpResult{}, fmt.Errorf("crw.milestone(plan, milestone): %w", err)
	}
	if plan == "" || id == "" {
		return engine.OpResult{}, fmt.Errorf("crw.milestone(plan, milestone): a plan and a milestone are required")
	}
	ms, err := o.l.liveMilestone(ctx, plan, id)
	if err != nil {
		return engine.OpResult{}, err
	}
	return engine.OpResult{Result: result(ms), Data: map[string]any{"plan": plan, "milestone": id, "nodes": len(ms.Nodes), "digest": digestOf(ms)}}, nil
}

// normalizeMilestone gives every list of a milestone its empty value, so the JSON a script reads and the digest of the plan do not depend
// on whether the relay answered null or an empty list.
func normalizeMilestone(ms *Milestone) {
	for i := range ms.Nodes {
		n := &ms.Nodes[i]
		if n.After == nil {
			n.After = []string{}
		}
		if n.Regions == nil {
			n.Regions = []string{}
		}
		if n.Criteria == nil {
			n.Criteria = []string{}
		}
	}
	if ms.Nodes == nil {
		ms.Nodes = []PlanNode{}
	}
}

func (o *milestoneOp) Proven(ctx context.Context, ev []engine.Event, row engine.Event) (bool, string, error) {
	return provenAlways(ctx, ev, row)
}

// ---- crw.designAudit ----

// The states of a design audit and of a review: the one state a result is in, and the only state that moves the lane on.
const (
	AuditApproved     = "approved"
	AuditNeedsChanges = "needs_changes"
	StateVerdict      = "verdict"
	StateHeld         = "held"
	StateUnknown      = "unknown"
	StateRefused      = "refused"
	StateNullModel    = "null_model"
	StateSuperseded   = "superseded"
)

// Candidate is a DESIGN with the answers of its open questions: what the audit judges and the ledger records.
type Candidate struct {
	Design  json.RawMessage `json:"design"`
	Answers json.RawMessage `json:"answers"`
}

func (c Candidate) digest() string {
	var design, answers any
	_ = json.Unmarshal(c.Design, &design)
	if len(c.Answers) > 0 {
		_ = json.Unmarshal(c.Answers, &answers)
	}
	if answers == nil {
		answers = []any{}
	}
	return digestOf(map[string]any{"design": design, "answers": answers})
}

// independent reports why a call of the family of critic is not an independent second opinion of the models: "" when its
// model family differs from every one of theirs, else the closed reason. A pair that was never read back proves nothing.
func independent(critic string, authors []string) string {
	if critic == "" || len(authors) == 0 {
		return "independence_unproven"
	}
	for _, a := range authors {
		if a == "" || execution.ModelFamily(a) == execution.ModelFamily(critic) {
			return engine.ReasonIndependenceRefused
		}
	}
	return ""
}

// designAuditOp is crw.designAudit(milestone, round, candidate, architectCall, auditCall): the record of the independent audit of
// a DESIGN. The auditor is the critic, of another model family than the architect; only an approved audit lets the DESIGN into the
// ledger, and the executors start only on a ledger revision. A call that did not complete is not an audit: held and unknown hold
// the milestone, refused escalates, null_model asks the auditor again once, superseded stops.
type designAuditOp struct{ l *Lane }

// AuditResult is the answer of crw.designAudit. Critic is the auditor's call; an answer an earlier binary journaled named it
// reviewer, and no reader reads that member.
type AuditResult struct {
	State           string         `json:"state"`
	Reason          string         `json:"reason"`
	Round           int            `json:"round"`
	CandidateDigest string         `json:"candidate_digest"`
	Findings        []AuditFinding `json:"findings"`
	Summary         string         `json:"summary"`
	Critic          Brief          `json:"critic"`
	Architect       Brief          `json:"architect"`
}

func (o *designAuditOp) Run(_ context.Context, req engine.OpRequest) (engine.OpResult, error) {
	var ms string
	var round int
	var cand Candidate
	var arch, call CallRef
	if err := args(req.Args, &ms, &round, &cand, &arch, &call); err != nil {
		return engine.OpResult{}, fmt.Errorf("crw.designAudit(milestone, round, candidate, architect, call): %w", err)
	}
	if _, err := DecodeDesign(cand.Design); err != nil {
		return engine.OpResult{}, fmt.Errorf("crw.designAudit: the candidate is no DESIGN: %w", err)
	}
	res := AuditResult{Round: round, CandidateDigest: cand.digest(), Findings: []AuditFinding{}, Critic: call.brief(), Architect: arch.brief()}
	switch call.Status {
	case "completed":
		audit, err := DecodeDesignAudit(call.Output)
		if err != nil {
			res.State, res.Reason = StateNullModel, "audit_invalid"
			break
		}
		res.Findings, res.Summary = audit.Findings, audit.Summary
		if audit.Findings == nil {
			res.Findings = []AuditFinding{}
		}
		if why := independent(call.Meta.ConfiguredPair.Model, []string{arch.Meta.ConfiguredPair.Model}); why != "" {
			res.State, res.Reason = StateRefused, why
			break
		}
		res.State = audit.Verdict
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
		"milestone": ms, "round": round, "candidate_digest": res.CandidateDigest, "state": res.State, "reason": res.Reason,
		"critic_call": call.CallID, "architect_call": arch.CallID, "critic_model": call.Meta.ConfiguredPair.Model, "architect_model": arch.Meta.ConfiguredPair.Model,
	}}, nil
}

func (o *designAuditOp) Proven(ctx context.Context, ev []engine.Event, row engine.Event) (bool, string, error) {
	return provenAlways(ctx, ev, row)
}

// approvedAudit is the approved audit row of the milestone for a candidate digest.
func approvedAudit(r rows, ms, digest string) (engine.Event, bool) {
	return r.opRows(OpDesignAudit).last(func(ev engine.Event) bool {
		return dataStr(ev, "milestone") == ms && dataStr(ev, "candidate_digest") == digest && dataStr(ev, "state") == AuditApproved
	})
}

// ---- crw.designLedger ----

// FirstRevision is the first revision of a milestone's ledger. Revisions after it are made by a re-plan (replan.go): the ledger adds
// one for every architect delta that passed the design audit again.
const FirstRevision = "r0"

// designLedgerOp is crw.designLedger(milestone, entry): it journals a revision of the milestone's design ledger - the DESIGN that
// passed the audit with the answers of its open questions, the digest the packets and the input snapshots carry, the handle of the
// architect thread (the thread goes on with every completed architect turn) and the slice digest of every node of the plan the
// revision was made against. The first revision is r0; a revision after it is the architect's delta after a re-plan signal, and is
// recorded only over an audit of its own that was approved after the revision before it.
type designLedgerOp struct{ l *Lane }

// LedgerEntry is what a script hands crw.designLedger.
type LedgerEntry struct {
	// Revision is the id of the revision the entry becomes; empty is the next one. A script that names one names the next.
	Revision  string          `json:"revision"`
	Design    json.RawMessage `json:"design"`
	Answers   json.RawMessage `json:"answers"`
	Architect CallRef         `json:"architect"`
	// Signal is the number of the signal that made the revision (0 for the first design); Head is the head of the integration
	// target the architect judged for a revision after the first.
	Signal int    `json:"signal"`
	Head   string `json:"head"`
	// Plan is the plan the architect and the auditor evaluated for a revision after the first (the plan of crw.replanSignals): the
	// revision records its slices, so that a plan revision made while the delta was architected or audited is a change against the
	// revision, counted by the next signal, and not part of its baseline. Without it the revision records the plan as it is now.
	Plan *PlanSnapshot `json:"plan"`
}

// Ledger is the answer of crw.designLedger and the shape of the revision a release reads back.
type Ledger struct {
	Milestone string   `json:"milestone"`
	Revision  string   `json:"revision"`
	Digest    string   `json:"digest"`
	Handle    string   `json:"handle"`
	Nodes     []string `json:"nodes"`
	BaseTip   string   `json:"base_tip"`
}

func (o *designLedgerOp) Run(ctx context.Context, req engine.OpRequest) (engine.OpResult, error) {
	var ms string
	var e LedgerEntry
	if err := args(req.Args, &ms, &e); err != nil {
		return engine.OpResult{}, fmt.Errorf("crw.designLedger(milestone, entry): %w", err)
	}
	r := rows(req.Journal.Events())
	next := nextRevision(r, ms)
	if e.Revision == "" {
		e.Revision = next
	}
	if e.Revision != next {
		return engine.OpResult{}, fmt.Errorf("crw.designLedger: revision %s: the ledger of milestone %s adds %s next", e.Revision, ms, next)
	}
	prev, hasPrev := currentLedger(r, ms)
	if e.Revision == FirstRevision && hasPrev {
		return engine.OpResult{}, fmt.Errorf("crw.designLedger: milestone %s has revision %s already", ms, dataStr(prev, "revision"))
	}
	if e.Revision != FirstRevision && !hasPrev {
		return engine.OpResult{}, fmt.Errorf("crw.designLedger: revision %s: milestone %s has no revision it could follow", e.Revision, ms)
	}
	design, err := DecodeDesign(e.Design)
	if err != nil {
		return engine.OpResult{}, fmt.Errorf("crw.designLedger: %w", err)
	}
	cand := Candidate{Design: e.Design, Answers: e.Answers}
	digest := cand.digest()
	// the audit of a revision after the first is an audit of its own, made after the revision before it: the audit of an earlier
	// DESIGN of the same digest stands in for nothing
	after := 0
	if hasPrev {
		after = prev.Seq
	}
	if _, ok := approvedAuditAfter(r, ms, digest, after); !ok {
		return engine.OpResult{}, fmt.Errorf("crw.designLedger: the DESIGN of milestone %s (%s) has no approved audit: executors start only on an audited revision", ms, short(digest))
	}
	plan := planOf(r, ms)
	// the slices are those of the plan the architect saw: the milestone as the run read it for the first revision, the plan the
	// decision evaluated for the ones after (a re-plan after a plan revision starts counting the change from that revision, and a
	// change made after the evaluation is counted by the next signal)
	var mile Milestone
	var sl map[string]string
	var mileDigest string
	switch {
	case hasPrev && e.Plan != nil:
		sl, mileDigest = e.Plan.Slices, e.Plan.Digest
		if sl == nil {
			sl = map[string]string{}
		}
	case hasPrev:
		if mile, err = o.l.liveMilestone(ctx, plan, ms); err != nil {
			return engine.OpResult{}, err
		}
		sl, mileDigest = liveSlices(mile), digestOf(mile)
	default:
		if mile, _, err = milestoneOf(r, ms); err != nil {
			return engine.OpResult{}, err
		}
		sl, mileDigest = liveSlices(mile), digestOf(mile)
	}
	// the head the DESIGN ran at: for a revision after the first the head H the re-plan pinned, for the first the commit the
	// architect's checkout was made at, as its prepared row records it - never the integration target sampled now, after the audit,
	// which a lane that landed while the DESIGN was audited has moved past it
	base := ""
	switch {
	case hasPrev && e.Head != "":
		base = e.Head
	case hasPrev:
		if base, err = o.l.d.Relay.IntegrationTip(ctx, plan); err != nil {
			return engine.OpResult{}, err
		}
	default:
		base = designHead(r, e.Architect.CallID)
	}
	led := Ledger{Milestone: ms, Revision: e.Revision, Digest: digest, Handle: e.Architect.Handle, Nodes: []string{}, BaseTip: base}
	for _, is := range design.Issues {
		led.Nodes = append(led.Nodes, is.Node)
	}
	var answers any
	if len(e.Answers) > 0 {
		_ = json.Unmarshal(e.Answers, &answers)
	}
	var designAny any
	_ = json.Unmarshal(e.Design, &designAny)
	return engine.OpResult{Result: result(led), Data: map[string]any{
		"milestone": ms, "revision": e.Revision, "digest": digest, "architect_call": e.Architect.CallID, "architect_handle": e.Architect.Handle,
		"base_tip": base, "design": designAny, "answers": answers, "signal": e.Signal, "slices": sl, "milestone_digest": mileDigest,
	}}, nil
}

func (o *designLedgerOp) Proven(ctx context.Context, ev []engine.Event, row engine.Event) (bool, string, error) {
	return provenAlways(ctx, ev, row)
}

// designHead is the commit the DESIGN a call answered was written at: the start commit of the call-private checkout the architect's
// thread worked in, read from the prepared row of the call or, for a continuation that made no directory of its own (a design fix
// of the first audit goes on in the thread's directory), of the call it continued. "" when no placement recorded one (a backend
// that places nothing): the packets then say the head is unknown rather than name a tip read later.
func designHead(r rows, callID string) string {
	seen := map[string]bool{}
	for callID != "" && !seen[callID] {
		seen[callID] = true
		prep, ok := r.last(func(ev engine.Event) bool {
			return ev.Kind == engine.KindCall && ev.Event == engine.EvPrepared && ev.CallID == callID
		})
		if !ok {
			return ""
		}
		if m, ok := prep.Data["allocation"].(map[string]any); ok && str(m, "kind") == engine.PlaceCall {
			if start := str(m, "start_commit"); start != "" {
				return start
			}
		}
		if head := dataStr(prep, "head"); head != "" {
			return head
		}
		callID = dataStr(prep, "resume_of")
	}
	return ""
}

// ledgerOf is the ledger row of a milestone's revision.
func ledgerOf(r rows, ms, revision string) (engine.Event, bool) {
	return r.opRows(OpDesignLedger).last(func(ev engine.Event) bool {
		return dataStr(ev, "milestone") == ms && dataStr(ev, "revision") == revision
	})
}

// ---- crw.release ----

// releaseOp is crw.release(plan, node, milestone, opts): the release of an issue node to an executor of the run, under the milestone's
// ledger revision. It refuses to release a node whose DESIGN has no approved audit - the structural guarantee that no executor
// starts before the audit passed - and a node whose issue the architect sized for a child or a split is not released to an
// executor at all (the lane releases the child or escalates). The row it writes names the revision and the digest: the packet of
// the lane is built from them, and the input snapshot of every call of the node carries them.
//
// With opts.pinned (crw.dag passes it on every release of a lane, CRW-1202) the release checks the plan the audited DESIGN stands on
// (pinnedPlan): the plan the relay has now is compared with it in the flow, before the relay is asked and before the node's size is
// read, and a node whose plan is another is not released (reason plan_changed). The lane then reads the plan again - a trigger of the
// signal path - and asks again once the signal that comes of it re-planned and audited a delta for that plan (or judged it and kept
// the revision). The release is a compare-and-swap on the plan (CRW-1288): Relay.Release carries the node's slice of the compared plan,
// and the relay compares it with the node's slice in the transaction that reserves the slot and records the release, so a plan that
// changes between the flow's comparison and that commit releases nothing (plan_changed) and the row records the slice the relay
// committed.
type releaseOp struct{ l *Lane }

// ReleaseOpts are the options of crw.release.
type ReleaseOpts struct {
	// Pinned makes the release compare the live plan with the plan the audited DESIGN stands on in the flow, before Relay.Release, whose
	// release commit compares the node's slice of that plan again (CRW-1288).
	Pinned bool `json:"pinned"`
	// Land is the head made outside a project-lane run that the gate-only workflow land-head lands (CRW-1354, lane_land_head.go): the node
	// is released with no DESIGN, at the plan the run read, and the head is checked out for the gates. Absent for every other release.
	Land *LandOpts `json:"land,omitempty"`
}

// AdmissionRegions are the regions the lane admits a node by, as crw:lib/lane admissionRegions makes them: the plan's declared regions
// and then the DESIGN's edit_regions the plan does not name. A node with no regions on either side is undeclared (none), and an
// undeclared node overlaps every other. An edit region is compared by its normal form (RegionOf, CRW-1404), so one that names a region
// already admitted in another spelling (blanks around it, ./path) is not admitted again: the relay would refuse the declaration of a
// planned region and a second spelling of it whose change or exclusive flag differ.
func AdmissionRegions(plan, edit []string) []string {
	out := []string{}
	if len(plan) == 0 || len(edit) == 0 {
		return out
	}
	out = append(out, plan...)
	seen := map[RegionForm]bool{}
	for _, r := range plan {
		seen[RegionOf(r)] = true
	}
	for _, r := range edit {
		if form := RegionOf(r); !seen[form] {
			seen[form] = true
			out = append(out, r)
		}
	}
	return out
}

// RegionForm is the normal form of a region as the lane writes it (a path, a tree when it ends in "/", a symbol when it names one after
// "#"): its kind, the path trimmed and cleaned as the relay's DeclareRegions cleans it, and the symbol's key.
type RegionForm struct{ Path, Kind, Key string }

// RegionOf is the normal form of a region's text (CRW-1404). A path the relay refuses (empty, absolute, outside the repository) keeps
// its cleaned form, which the relay refuses as it would the text.
func RegionOf(text string) RegionForm {
	p := strings.TrimSpace(text)
	form := RegionForm{Path: p, Kind: "file"}
	if at := strings.LastIndex(p, "#"); at > 0 {
		form.Path, form.Kind, form.Key = p[:at], "symbol", p[at+1:]
	} else if strings.HasSuffix(p, "/") {
		form.Path, form.Kind = strings.TrimSuffix(p, "/"), "tree"
	}
	form.Path = path.Clean(form.Path)
	return form
}

// RegionFormOf is the normal form of a structured region (a plan's compile.Region) taken field by field (CRW-1408): its path cleaned as
// compile.cleanRegion and the relay's DeclareRegions clean it, its kind (a file when none is named) and its symbol's key. Its text is
// never parsed, so a path that holds "#" stays the file or tree it is.
func RegionFormOf(p, kind, key string) RegionForm {
	if kind == "" {
		kind = "file"
	}
	return RegionForm{Path: path.Clean(strings.TrimSuffix(p, "/")), Kind: kind, Key: key}
}

// Text is the region as the plan's manifest writes it (compile.Region.String of the cleaned region): the path, the path and "/" for a
// tree, the path, "#" and the key for a symbol.
func (f RegionForm) Text() string {
	switch f.Kind {
	case "tree":
		return f.Path + "/"
	case "symbol":
		return f.Path + "#" + f.Key
	}
	return f.Path
}

// WarnRegionsNotDeclared is the warning of a release whose admission regions the relay did not declare (CRW-1391): the release goes on
// as for an undeclared node, which overlaps every other.
const WarnRegionsNotDeclared = "regions_not_declared"

// ReasonPlanChanged is the reason of a pinned release whose plan is not the one the audited DESIGN stands on.
const ReasonPlanChanged = "plan_changed"

// ReasonRelayPlanChanged is the reason of a pinned release the relay refused as plan_changed while the plan the flow reads again is
// still the one the audited DESIGN stands on: the relay releases from a plan the flow does not read (a run reads its plan at the
// revision it pinned), so no signal comes of the change and the lane does not ask again. The release raises the run-level hold
// plan_superseded (CRW-1291): the run does not follow the relay's new revision.
const ReasonRelayPlanChanged = "relay_plan_changed"

// ReasonPlanSuperseded is the run-level hold of CRW-1291, raised by a release answered relay_plan_changed, and the reason of every
// release while the hold stands.
const ReasonPlanSuperseded = "plan_superseded"

// ReleaseOutcome is the answer of crw.release.
type ReleaseOutcome struct {
	Released     bool   `json:"released"`
	Reason       string `json:"reason"`
	Node         string `json:"node"`
	Kind         string `json:"kind"`
	Revision     string `json:"revision"`
	DesignDigest string `json:"design_digest"`
	ExecutionID  string `json:"execution_id"`
	// Escalation is the hold this release raised, the question its node waits on; Hold the hold that stands (plan_superseded).
	Escalation string `json:"escalation,omitempty"`
	Hold       string `json:"hold,omitempty"`
	// Detail is the relay's message of a release refused epoch_lost (CRW-1382): the lane ends unreleased with it.
	Detail string `json:"detail,omitempty"`
	// Land is the head a land release checked out for the gates (CRW-1354); absent for every other release.
	Land *LandHead `json:"land,omitempty"`
	// Warnings are what the release went on despite (CRW-1391: the node's admission regions the relay did not declare); absent when none.
	Warnings []string `json:"warnings,omitempty"`
}

func (o *releaseOp) Run(ctx context.Context, req engine.OpRequest) (engine.OpResult, error) {
	var plan, node, ms string
	var opts ReleaseOpts
	if err := args(req.Args, &plan, &node, &ms, &opts); err != nil {
		return engine.OpResult{}, fmt.Errorf("crw.release(plan, node, milestone, opts): %w", err)
	}
	r := rows(req.Journal.Events())
	led, ok := currentLedger(r, ms)
	out := ReleaseOutcome{Node: node, Kind: "executor"}
	data := map[string]any{"plan": plan, "node": node, "milestone": ms}
	finish := func() (engine.OpResult, error) {
		data["released"], data["reason"], data["kind"] = out.Released, out.Reason, out.Kind
		if out.Hold != "" {
			data["escalation"], data["hold"] = out.Escalation, out.Hold
		}
		return engine.OpResult{Result: result(out), Data: data}, nil
	}
	// a run under the hold plan_superseded releases nothing and asks the relay nothing until a decision lifts it (CRW-1291); the call
	// that raised the hold and runs again answers it as its own
	if hold, ok := standingHold(r, req.CallID); ok {
		out.Reason, out.Hold = ReasonPlanSuperseded, dataStr(hold, "escalation_id")
		if dataStr(hold, "release_call") == req.CallID {
			out.Reason, out.Escalation = ReasonRelayPlanChanged, out.Hold
			if err := o.returnSlot(ctx, req, hold, data); err != nil {
				return engine.OpResult{}, err
			}
		}
		return finish()
	}
	// a head made outside a run lands with no DESIGN (CRW-1354): its release is the land's
	if opts.Land != nil {
		return o.landRelease(ctx, req, plan, node, ms, opts)
	}
	if !ok {
		out.Reason = "design_missing"
		return finish()
	}
	out.Revision, out.DesignDigest = dataStr(led, "revision"), dataStr(led, "digest")
	if _, audited := approvedAudit(r, ms, out.DesignDigest); !audited {
		out.Reason = "design_not_audited"
		return finish()
	}
	entry, found, err := designIssue(led, node)
	if err != nil {
		return engine.OpResult{}, err
	}
	if !found {
		out.Reason = "node_not_designed"
		return finish()
	}
	// the plan as the relay has it now: the comparison of a pinned release, and the slice the release records
	var now *Milestone
	readNow := func() (Milestone, error) {
		if now == nil {
			m, err := o.l.liveMilestone(ctx, plan, ms)
			if err != nil {
				return Milestone{}, err
			}
			now = &m
		}
		return *now, nil
	}
	// The release of a lane (crw:lib/lane, crw.dag) is the only release path of a node to an executor, a child or a split, and it always
	// passes pinned: this comparison is therefore the one check every release goes through (the workflows' call sites are held to it by
	// TestEveryWorkflowReleasePassesPinned). It happens here, in the flow, before Relay.Release below, and the relay's release commit
	// compares the node's slice of this plan again (CRW-1288), so a plan change between this comparison and that commit releases nothing.
	// A call without pinned is the operation's own unit tests.
	if opts.Pinned {
		m, err := readNow()
		if err != nil {
			return engine.OpResult{}, err
		}
		live, pinned := digestOf(m), pinnedPlan(r, ms)
		data["plan_digest"], data["pinned_plan"] = live, pinned
		if live != pinned {
			out.Reason = ReasonPlanChanged
			return finish()
		}
	}
	// A node that was released under a revision a resume superseded, whose slice of the plan is not the one the revision that stands
	// was made against, and that a landed acceptance rests on, is not released again: the route is a plan revision with update_node
	// or dag-correct, as for any accepted node whose spec changed.
	if old, was := releaseRowOf(r, node); was && revisionSuperseded(r, ms, dataStr(old, "revision")) && ledgerSlices(led)[node] != dataStr(old, "slice_digest") && acceptedAfter(r, node, old) {
		return engine.OpResult{}, &Refusal{"accepted_node_changed", fmt.Sprintf("node %s was accepted under revision %s, which a resume superseded, and its slice of the plan is not the one revision %s was made against: record a plan revision with update_node (the acceptance becomes stale) and run again, or correct an oversized child with dag-correct", node, dataStr(old, "revision"), out.Revision)}
	}
	data["revision"], data["design_digest"] = out.Revision, out.DesignDigest
	switch entry.SizeFit {
	case "child":
		// the child start carries the node's slice of the plan compared here, which the relay's managed release compares again (CRW-1382),
		// as the release of an executor carries it below
		m, err := readNow()
		if err != nil {
			return engine.OpResult{}, err
		}
		data["slice_digest"] = liveSlices(m)[node]
		out.Kind, out.Reason = "child", "size_fit_child"
		return finish()
	case "split":
		out.Kind, out.Reason = "split", "size_split"
		return finish()
	}
	// the criteria are those of the plan the run read; the slice is the node's slice of the plan as it is when the node is released,
	// the one a later revision of the ledger is compared with (a node whose slice another revision was not made against is not kept
	// under it)
	mnode, planned := planNodeOf(r, plan, ms, node)
	m, err := readNow()
	if err != nil {
		return engine.OpResult{}, err
	}
	slice, live := liveSlices(m)[node]
	if !live && planned {
		// the plan the relay reads has dropped the node: nothing is released, and no slice of the plan the run read first stands in for
		// the one the node no longer has
		out.Reason = "node_not_in_plan"
		return finish()
	}
	// the release carries the slice it compared (CRW-1288), which the relay also compares with the slice a replayed release was
	// committed under; a released row records the slice the relay committed, an unreleased one the slice the flow asked for
	data["slice_digest"], data["criteria_digest"] = slice, mnode.CriteriaDigest
	// the regions the lane admitted the node by (crw:lib/lane admissionRegions) are the ones the relay declares for it before the release
	// (CRW-1391): the relay's own overlap judgment reads only declarations, and an undeclared node overlaps every other
	regions := AdmissionRegions(mnode.Regions, entry.EditRegions)
	data["regions"] = regions
	admitted, err := admittedOthers(r, led, plan, ms, node)
	if err != nil {
		return engine.OpResult{}, err
	}
	res, err := o.l.d.Relay.Release(ctx, ReleaseRequest{Plan: plan, Node: node, RunID: req.RunID, Revision: out.Revision, DesignDigest: out.DesignDigest, RequestID: req.RequestID,
		SliceDigest: slice, PlanDigest: digestOf(m), Regions: regions, Admitted: admitted})
	if errors.Is(err, ErrEpochLost) {
		// the fenced write of a run whose plan another session claimed (CRW-1285): the node is not released, the row names the reason,
		// and the lane ends on it; a resume asks again, and the relay refuses again while the other session holds the plan
		out.Reason, out.Detail, data["detail"] = ReasonEpochLost, err.Error(), err.Error()
		return finish()
	}
	if err != nil {
		return engine.OpResult{}, err
	}
	out.Released, out.Reason, out.ExecutionID = res.Released, res.Reason, res.ExecutionID
	if res.Released {
		data["slice_digest"] = res.SliceDigest
	}
	if res.RegionsDeclared != "" {
		data["regions_declared"] = res.RegionsDeclared
	}
	if res.RegionsWarning != "" {
		// fail open: the release went on as for an undeclared node, and the row and the answer say what was not declared and why
		out.Warnings = []string{fmt.Sprintf("%s: the admission regions %s of node %s were not declared to the relay (%s), so the relay judges the node as undeclared, overlapping every other",
			WarnRegionsNotDeclared, strings.Join(regions, ", "), node, res.RegionsWarning)}
		data["warnings"] = out.Warnings
	}
	if opts.Pinned && !res.Released && res.Reason == ReasonPlanChanged {
		// the relay's plan gave the node another slice at its commit: a plan the flow reads again as the pinned one is a change the
		// flow cannot see, and the lane must not ask again for it
		again, err := o.l.liveMilestone(ctx, plan, ms)
		if err != nil {
			return engine.OpResult{}, err
		}
		if digestOf(again) == pinnedPlan(r, ms) {
			out.Reason = ReasonRelayPlanChanged
			if err := o.hold(ctx, req, plan, node, res, &out, data); err != nil {
				return engine.OpResult{}, err
			}
		}
	}
	data["execution_id"], data["slot_id"], data["request_id"] = res.ExecutionID, res.SlotID, req.RequestID
	return finish()
}

// hold raises the run-level hold plan_superseded (CRW-1291): the relay refused the node for a plan revision past the one the run pinned,
// which the run does not follow (CRW-1277 c). The hold is an escalation of the node that asks the parent (the run tells it, with the
// pinned revision, the head revision, the node and the actions), written once for the call; while it stands no node of the run is
// released and nothing restarts on its own. The execution the relay still holds for the node (a release the run never recorded) is
// closed when it is this run's, which returns its slot, and recorded with its run when it is another run's.
func (o *releaseOp) hold(ctx context.Context, req engine.OpRequest, plan, node string, res ReleaseResult, out *ReleaseOutcome, data map[string]any) error {
	id := "esc_" + hashParts(req.RunID, req.CallID, ReasonPlanSuperseded)[:24]
	out.Escalation, out.Hold = id, id
	row, written := rows(req.Journal.Events()).flow(engine.EvEscalation).last(func(ev engine.Event) bool { return dataStr(ev, "escalation_id") == id })
	if !written {
		slot := "none"
		switch {
		case res.HeldExecution != "" && res.HeldRun == req.RunID:
			slot = "returned"
		case res.HeldExecution != "":
			slot = "owned_by_run"
		}
		detail := fmt.Sprintf("plan_superseded: run %s pinned revision %d of plan %s; the relay's head revision %d gives node %s another slice, so the relay "+
			"refused its release (plan_revision_conflict). The run does not follow the new revision and holds: no node of it is released while the hold stands, "+
			"and nothing restarts on its own. Actions: end or cancel the run (crw flow cancel %s) and start a new run on revision %d, or answer this question "+
			"to close node %s by escalation (crw flow answer, then crw flow resume).", req.RunID, res.PinnedRevision, plan, res.HeadRevision, node, req.RunID, res.HeadRevision, node)
		evidence := map[string]any{"plan": plan, "node": node, "pinned_revision": res.PinnedRevision, "head_revision": res.HeadRevision, "held_execution": res.HeldExecution,
			"held_run": res.HeldRun, "slot": slot}
		row = req.Journal.Append(engine.Event{RunID: req.RunID, Kind: engine.KindFlow, CallID: req.CallID, Op: OpRelease, Event: engine.EvEscalation, Data: map[string]any{
			"reason": ReasonPlanSuperseded, "class": escalationClass(ReasonPlanSuperseded), "node": node, "escalation_id": id, "detail": detail, "evidence": evidence,
			"ask": ReasonPlanSuperseded, "release_call": req.CallID}})
	}
	return o.returnSlot(ctx, req, row, data)
}

// returnSlot closes the execution a hold recorded as the run's (plan_superseded, which returns its slot in the transaction that closes
// it); the same close again is the relay's replay, so a call that raised the hold and runs again closes it again.
func (o *releaseOp) returnSlot(ctx context.Context, req engine.OpRequest, hold engine.Event, data map[string]any) error {
	evidence, _ := hold.Data["evidence"].(map[string]any)
	held := func(k string) string { s, _ := evidence[k].(string); return s }
	data["held_execution"], data["held_run"], data["slot"] = held("held_execution"), held("held_run"), held("slot")
	if held("slot") != "returned" {
		return nil
	}
	closed, err := o.l.d.Relay.Close(ctx, CloseRequest{Plan: held("plan"), Node: dataStr(hold, "node"), Actor: o.l.d.Actor, Reason: ReasonPlanSuperseded, RequestID: req.RequestID})
	data["slot_released"] = closed.SlotReleased
	return err
}

// standingHold is a plan_superseded hold of the run that no applied decision answered: the one the call raised (two releases refused at
// once raise one each), else the newest.
func standingHold(r rows, call string) (engine.Event, bool) {
	stands := func(ev engine.Event) bool {
		if dataStr(ev, "reason") != ReasonPlanSuperseded || dataStr(ev, "ask") == "" {
			return false
		}
		_, applied := appliedRow(r, dataStr(ev, "node"), dataStr(ev, "escalation_id"))
		return !applied
	}
	if own, ok := r.flow(engine.EvEscalation).last(func(ev engine.Event) bool { return dataStr(ev, "release_call") == call && stands(ev) }); ok {
		return own, true
	}
	return r.flow(engine.EvEscalation).last(stands)
}

// Proven: a release is proven while the relay says it stands. It is also proven when the run itself ended the execution after the
// release: it gave the node up (a completed crw.escalate row of the node that closed the execution) or landed it (a completed
// crw.integrate row of the node). A closed execution does not stand at the relay, and the lane that closed it must not be
// released and built again on a resume - the escalation is the node's decision until crw.decide answers it.
//
// A release made under a revision a resume superseded (the signal that made the revision was not replayed) stands only for a node
// whose slice of the plan is the one the revision that stands was made against: the replay of the lane is kept and a release_rebind
// row says so. For any other node the position runs again under the revision that stands - a node that had not started gets the new
// packet, and a node that had started and was not accepted runs again from its first position (the position is live, so is every
// later one of the lane); one that was accepted is refused by Run.
func (o *releaseOp) Proven(ctx context.Context, ev []engine.Event, row engine.Event) (bool, string, error) {
	if !dataBool(row, "released") {
		// a node the architect sized for a child or a split was not released to an executor on purpose: the row is the decision
		// of the ledger revision it names, which does not change, and the child's start has a row of its own (crw.releaseChild).
		// Asking again would start the child again.
		if k := dataStr(row, "kind"); k == "child" || k == "split" {
			return true, "", nil
		}
		// a pinned release whose plan was not the one the audited DESIGN stood on is the decision of that moment: the lane read the
		// plan again after it, and the positions it went on with are what a resume replays
		if dataStr(row, "reason") == ReasonPlanChanged {
			return true, "", nil
		}
		// the release that raised the hold plan_superseded is its node's for good (the decision on the hold closes the node by
		// escalation); a release the hold answered asks again once a decision lifted it - nothing restarts while it stands
		if dataStr(row, "escalation") != "" {
			return true, "", nil
		}
		if dataStr(row, "reason") == ReasonPlanSuperseded {
			if _, standing := standingHold(rows(ev), ""); standing {
				return true, "", nil
			}
			return false, "the hold plan_superseded was lifted", nil
		}
		// a node the scheduler held back may be releasable now: the position asks again
		return false, "the release did not happen", nil
	}
	r := rows(ev)
	ms, rev, node := dataStr(row, "milestone"), dataStr(row, "revision"), dataStr(row, "node")
	if revisionSuperseded(r, ms, rev) {
		cur, ok := currentLedger(r, ms)
		if !ok || ledgerSlices(cur)[node] == "" || ledgerSlices(cur)[node] != dataStr(row, "slice_digest") {
			return false, "the release stands under revision " + rev + ", which a resume superseded, and the node's slice is not the one the revision that stands was made against", nil
		}
		o.l.rebind(r, row, cur)
	}
	if ended, how := endedByRun(r, row); ended {
		return true, how, nil
	}
	ok, err := o.l.d.Relay.ReleaseStands(ctx, dataStr(row, "plan"), dataStr(row, "node"), dataStr(row, "execution_id"))
	if err != nil {
		return false, "", err
	}
	if !ok {
		return false, "the execution of the release is closed", nil
	}
	return true, "", nil
}

// endedByRun says whether the run ended the execution of a release: a later completed escalation of the node that closed it, or a
// later completed integration of the node.
func endedByRun(r rows, release engine.Event) (bool, string) {
	node := dataStr(release, "node")
	if _, ok := r.opRows(OpEscalate).last(func(ev engine.Event) bool {
		return ev.Seq > release.Seq && dataStr(ev, "node") == node && dataBool(ev, "closed")
	}); ok {
		return true, "the run closed the execution when it gave the node up"
	}
	if _, ok := r.opRows(OpIntegrate).last(func(ev engine.Event) bool { return ev.Seq > release.Seq && dataStr(ev, "node") == node }); ok {
		return true, "the run landed the node"
	}
	return false, ""
}

// planOf is the plan a milestone was read from.
func planOf(r rows, ms string) string {
	row, ok := r.opRows(OpMilestone).last(func(ev engine.Event) bool { return dataStr(ev, "milestone") == ms })
	if !ok {
		return ""
	}
	return dataStr(row, "plan")
}

// planNodeOf is the milestone's node as the milestone row of the journal recorded it.
func planNodeOf(r rows, plan, ms, node string) (PlanNode, bool) {
	row, ok := r.opRows(OpMilestone).last(func(ev engine.Event) bool { return dataStr(ev, "plan") == plan && dataStr(ev, "milestone") == ms })
	if !ok {
		return PlanNode{}, false
	}
	var m Milestone
	if json.Unmarshal(resultOf(row), &m) != nil {
		return PlanNode{}, false
	}
	for _, n := range m.Nodes {
		if n.ID == node {
			return n, true
		}
	}
	return PlanNode{}, false
}

// admittedOthers are the admission regions of the milestone's other nodes the DESIGN of the ledger row plans, by node (ReleaseRequest.Admitted,
// CRW-1391): the plan of the run's crw.milestone row and the DESIGN's edit_regions, as the lane admits them; a node with none is left out.
func admittedOthers(r rows, led engine.Event, plan, ms, node string) (map[string][]string, error) {
	raw, err := json.Marshal(led.Data["design"])
	if err != nil {
		return nil, err
	}
	d, err := DecodeDesign(raw)
	if err != nil {
		return nil, fmt.Errorf("the ledger holds no valid DESIGN: %w", err)
	}
	out := map[string][]string{}
	for _, is := range d.Issues {
		if is.Node == node {
			continue
		}
		if n, ok := planNodeOf(r, plan, ms, is.Node); ok {
			if regions := AdmissionRegions(n.Regions, is.EditRegions); len(regions) > 0 {
				out[is.Node] = regions
			}
		}
	}
	return out, nil
}

// designIssue is the plan of a node in the DESIGN a ledger row holds.
func designIssue(led engine.Event, node string) (DesignIssue, bool, error) {
	raw, err := json.Marshal(led.Data["design"])
	if err != nil {
		return DesignIssue{}, false, err
	}
	d, err := DecodeDesign(raw)
	if err != nil {
		return DesignIssue{}, false, fmt.Errorf("the ledger holds no valid DESIGN: %w", err)
	}
	for _, is := range d.Issues {
		if is.Node == node {
			return is, true, nil
		}
	}
	return DesignIssue{}, false, nil
}

// releaseRowOf is the node's newest release row that released it.
func releaseRowOf(r rows, node string) (engine.Event, bool) {
	return r.opRows(OpRelease).last(func(ev engine.Event) bool { return dataStr(ev, "node") == node && dataBool(ev, "released") })
}

// designRowOf is the node's newest release row that names the design revision it was made under, whether it released the node to
// an executor or found it sized for a child or a split.
func designRowOf(r rows, node string) (engine.Event, bool) {
	return r.opRows(OpRelease).last(func(ev engine.Event) bool { return dataStr(ev, "node") == node && dataStr(ev, "revision") != "" })
}

// ---- crw.issuePlan ----

// issuePlanOp is crw.issuePlan(node): the plan of the node's issue from the ledger revision the node was released under. It reads
// the journal and nothing the script holds: a lane that reads the plan from a variable would build its packet from whatever
// revision the script saw last, and a replayed lane must build the packet it built.
type issuePlanOp struct{ l *Lane }

// IssuePlan is the plan of one issue, as a lane builds its packet from it.
type IssuePlan struct {
	DesignIssue
	Revision     string          `json:"revision"`
	DesignDigest string          `json:"design_digest"`
	Milestone    string          `json:"milestone"`
	Regions      []string        `json:"regions"`
	Decisions    []Decision      `json:"decisions"`
	Answers      json.RawMessage `json:"answers"`
	Title        string          `json:"title"`
	Criteria     []string        `json:"criteria"`
	// CriteriaDigest is the digest of the node's criteria set the plan fixed when the node was released: the digest a review and its
	// premerge record carry.
	CriteriaDigest string `json:"criteria_digest"`
}

func (o *issuePlanOp) Run(_ context.Context, req engine.OpRequest) (engine.OpResult, error) {
	var node string
	if err := args(req.Args, &node); err != nil {
		return engine.OpResult{}, fmt.Errorf("crw.issuePlan(node): %w", err)
	}
	r := rows(req.Journal.Events())
	rel, ok := designRowOf(r, node)
	if !ok {
		return engine.OpResult{}, fmt.Errorf("crw.issuePlan: node %s has no release in the journal", node)
	}
	ms, revision := dataStr(rel, "milestone"), dataStr(rel, "revision")
	led, ok := ledgerOf(r, ms, revision)
	if !ok {
		return engine.OpResult{}, fmt.Errorf("crw.issuePlan: the ledger has no revision %s of milestone %s", revision, ms)
	}
	if dataStr(led, "digest") != dataStr(rel, "design_digest") {
		return engine.OpResult{}, fmt.Errorf("crw.issuePlan: revision %s of milestone %s is not the one node %s was released under", revision, ms, node)
	}
	entry, found, err := designIssue(led, node)
	if err != nil {
		return engine.OpResult{}, err
	}
	if !found {
		return engine.OpResult{}, fmt.Errorf("crw.issuePlan: revision %s plans no issue for node %s", revision, node)
	}
	raw, _ := json.Marshal(led.Data["design"])
	design, _ := DecodeDesign(raw)
	plan := IssuePlan{DesignIssue: entry, Revision: revision, DesignDigest: dataStr(rel, "design_digest"), Milestone: ms, Regions: entry.EditRegions, Decisions: []Decision{}, Criteria: []string{},
		CriteriaDigest: dataStr(rel, "criteria_digest")}
	for _, ref := range entry.DecisionRefs {
		for _, d := range design.Decisions {
			if d.ID == ref {
				plan.Decisions = append(plan.Decisions, d)
			}
		}
	}
	if answers, err := json.Marshal(led.Data["answers"]); err == nil && string(answers) != "null" {
		plan.Answers = answers
	} else {
		plan.Answers = json.RawMessage(`[]`)
	}
	if n, ok := planNodeOf(r, dataStr(rel, "plan"), ms, node); ok {
		plan.Title, plan.Criteria = n.Title, n.Criteria
	}
	if plan.Regions == nil {
		plan.Regions = []string{}
	}
	return engine.OpResult{Result: result(plan), Data: map[string]any{"node": node, "milestone": ms, "revision": revision, "design_digest": plan.DesignDigest}}, nil
}

func (o *issuePlanOp) Proven(ctx context.Context, ev []engine.Event, row engine.Event) (bool, string, error) {
	return provenAlways(ctx, ev, row)
}

// ---- the input snapshot ----

// InputSnapshot is engine.Config.InputSnapshot: the facts only an external act changes, journaled with every call of a node and
// compared when the run resumes - the revision and digest of the DESIGN the node was released under, the node's slice of the
// plan revision and its criteria digest. A call that belongs to no released node has none. The facts are read from the release
// row, which is written once when the node is released, so a ledger that later grows a revision changes nothing for a node that
// started under r0.
func (l *Lane) InputSnapshot(_ context.Context, req engine.SnapshotRequest) (string, error) {
	if req.Node == "" || l.d.Journal == nil {
		return "", nil
	}
	rel, ok := releaseRowOf(rows(l.d.Journal.Events()), req.Node)
	if !ok {
		return "", nil
	}
	return fmt.Sprintf("design=%s:%s;slice=%s;criteria=%s", dataStr(rel, "revision"), dataStr(rel, "design_digest"), dataStr(rel, "slice_digest"), dataStr(rel, "criteria_digest")), nil
}
