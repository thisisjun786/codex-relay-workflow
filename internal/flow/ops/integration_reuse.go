package ops

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/audit"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
)

// This file is the rerun of a milestone close over nodes an earlier run of the same plan already integrated (CRW-1597). A close
// stops at the final gate, the crosscheck or the close itself with every node of the milestone landed on the integration target:
// a new run of the plan must not build those nodes again - the relay still reads them as integrated - but it must not take a
// record's word for the landing either. crw.reuseIntegrated(milestone, node) asks the relay what the node is now
// (IntegratedReader), and then proves the integration out of the earlier run's own journal: its release of the node under the
// same slice and criteria, its acceptance and integration rows matching what the relay reports, the same DESIGN issue the run
// stands on, the acceptance's audit subject, and the recorded batch still on the target. A node whose proof holds is recorded
// landed with its source run and source calls; one the relay does not read as integrated is pending and runs its ordinary lane.
//
// Missing proof is never rebuilt over: an integrated node with no qualifying source, with a head the target lost, or with no
// history to prove with is refused integration_reuse_unproven, and no executor starts for it. The run's own close evidence -
// the gate, the crosscheck, the report - is made by the run itself; the reuse row carries the source's provenance, not copies
// of its release, acceptance, integration or verification rows.

// Op names of this file.
const (
	NameReuseIntegrated = "reuseIntegrated"
	OpReuseIntegrated   = "crw." + NameReuseIntegrated
)

// The reasons a reuse decision records or is refused with.
const (
	ReuseIntegratedReused     = "integrated_reused"
	ReuseIntegratedNot        = "not_integrated"
	ReuseReaderUnavailable    = "integration_reader_unavailable"
	ReasonIntegrationUnproven = "integration_reuse_unproven"
)

// IntegratedState is what the relay says about a node now (the optional IntegratedReader seam): whether the node's current
// acceptance is observed integrated on the plan's target, the acceptance it rests on, the accepted head, the integration ref
// and its tip.
type IntegratedState struct {
	Integrated     bool
	AcceptanceID   string
	Head           string
	IntegrationRef string
	Tip            string
}

// IntegratedReader is the optional relay seam crw.reuseIntegrated reads: the relay's current acceptance and integration
// observations of a node. A relay that does not implement it answers no reuse (integration_reader_unavailable): the old fakes
// keep the old behavior, every node pending.
type IntegratedReader interface {
	Integrated(context.Context, string, PlanNode) (IntegratedState, error)
}

// ReuseIntegratedResult is the answer of crw.reuseIntegrated(milestone, node): the reuse decision of one node. A reused node
// carries the provenance of the earlier run's landing: the source run, its acceptance and integration calls, the accepted head
// and batch commit, and the node's slice and criteria and the run ledger's revision and digest the decision stands on.
type ReuseIntegratedResult struct {
	Reused              bool   `json:"reused"`
	Reason              string `json:"reason"`
	Plan                string `json:"plan"`
	Milestone           string `json:"milestone"`
	Node                string `json:"node"`
	Head                string `json:"head,omitempty"`
	AcceptanceID        string `json:"acceptance_id,omitempty"`
	BatchCommit         string `json:"batch_commit,omitempty"`
	IntegrationRef      string `json:"integration_ref,omitempty"`
	Tip                 string `json:"tip,omitempty"`
	SourceRun           string `json:"source_run,omitempty"`
	SourceAcceptCall    string `json:"source_accept_call,omitempty"`
	SourceIntegrateCall string `json:"source_integrate_call,omitempty"`
	ReviewBase          string `json:"review_base,omitempty"`
	SliceDigest         string `json:"slice_digest,omitempty"`
	CriteriaDigest      string `json:"criteria_digest,omitempty"`
	Revision            string `json:"revision,omitempty"`
	DesignDigest        string `json:"design_digest,omitempty"`
}

// normalizeRef is the integration ref as both sides name it: with or without the refs/heads/ prefix.
func normalizeRef(ref string) string { return strings.TrimPrefix(ref, "refs/heads/") }

// auditSubjectOf is the audit subject of the acceptance the reuse stands on in the source run's journal: same plan, milestone,
// node, head, acceptance id and criteria digest, with the call it rests on.
func auditSubjectOf(events []engine.Event, plan, ms, node, head, acceptanceID, criteria string) (audit.Subject, bool) {
	for _, s := range audit.Subjects(events) {
		if s.Plan == plan && s.Milestone == ms && s.Node == node && s.Head == head && s.AcceptanceID == acceptanceID && s.CriteriaDigest == criteria && s.ReviewerCall != "" {
			return s, true
		}
	}
	return audit.Subject{}, false
}

type reuseIntegratedOp struct{ l *Lane }

func (o *reuseIntegratedOp) Run(ctx context.Context, req engine.OpRequest) (engine.OpResult, error) {
	var ms, node string
	if err := args(req.Args, &ms, &node); err != nil {
		return engine.OpResult{}, fmt.Errorf("crw.reuseIntegrated(milestone, node): %w", err)
	}
	r := rows(req.Journal.Events())
	_, plan, err := milestoneOf(r, ms)
	if err != nil {
		return engine.OpResult{}, fmt.Errorf("crw.reuseIntegrated: %w", err)
	}
	n, ok := planNodeOf(r, plan, ms, node)
	if !ok || n.ID == "" {
		return engine.OpResult{}, fmt.Errorf("crw.reuseIntegrated: milestone %s of plan %s has no node %s", ms, plan, node)
	}
	led, hasLedger := currentLedger(r, ms)
	if !hasLedger {
		return engine.OpResult{}, fmt.Errorf("crw.reuseIntegrated: milestone %s has no design revision", ms)
	}
	out := ReuseIntegratedResult{Plan: plan, Milestone: ms, Node: node}
	finish := func() (engine.OpResult, error) {
		data := map[string]any{}
		raw, _ := json.Marshal(out)
		_ = json.Unmarshal(raw, &data)
		return engine.OpResult{Result: result(out), Data: data}, nil
	}
	reader, ok := o.l.d.Relay.(IntegratedReader)
	if !ok {
		out.Reason = ReuseReaderUnavailable
		return finish()
	}
	state, err := reader.Integrated(ctx, plan, n)
	if err != nil {
		return engine.OpResult{}, err
	}
	if !state.Integrated {
		out.Reason = ReuseIntegratedNot
		return finish()
	}
	unproven := func(detail string) (engine.OpResult, error) {
		return engine.OpResult{}, &Refusal{ReasonIntegrationUnproven, fmt.Sprintf("node %s of milestone %s: %s", node, ms, detail)}
	}
	if state.AcceptanceID == "" || state.Head == "" || state.IntegrationRef == "" || state.Tip == "" {
		return unproven(fmt.Sprintf("the relay reads the node as integrated with acceptance %q, head %q, ref %q, tip %q: the report is incomplete", state.AcceptanceID, state.Head, state.IntegrationRef, state.Tip))
	}
	// the accepted head the relay reports must be on the target it names
	if anc, err := IsAncestor(ctx, o.l.d.Git, o.l.d.Repo, state.Head, state.Tip); err != nil {
		return engine.OpResult{}, err
	} else if !anc {
		return unproven(fmt.Sprintf("the accepted head %s is not an ancestor of the integration tip %s", state.Head, state.Tip))
	}
	if o.l.d.DesignHistory == nil {
		return unproven("no run history is wired to prove the integration with")
	}
	curIssue, found, err := designIssue(led, node)
	if err != nil {
		return engine.OpResult{}, err
	}
	if !found {
		return unproven("the current design revision holds no issue of the node")
	}
	sources, err := o.l.d.DesignHistory(ctx, plan)
	if err != nil {
		return engine.OpResult{}, err
	}
	for _, src := range sources {
		if src.Run == "" || src.Run == req.RunID || src.Reason != "" {
			continue
		}
		sr := rows(src.Events)
		// the source's release of the node stands, released under the same plan, milestone, slice and criteria
		srel, ok := releaseRowOf(sr, node)
		if !ok || sr.superseded(srel) || dataStr(srel, "plan") != plan || dataStr(srel, "milestone") != ms ||
			dataStr(srel, "slice_digest") != n.SliceDigest || dataStr(srel, "criteria_digest") != n.CriteriaDigest {
			continue
		}
		// its acceptance is the one the relay reports, made after the release
		acc, ok := sr.current(OpAccept).last(func(ev engine.Event) bool {
			return dataStr(ev, "plan") == plan && dataStr(ev, "node") == node &&
				dataStr(ev, "acceptance_id") == state.AcceptanceID && dataStr(ev, "head") == state.Head
		})
		if !ok || srel.Seq >= acc.Seq {
			continue
		}
		// its integration of that acceptance and head onto the same ref, after the acceptance, with a batch commit
		ing, ok := sr.current(OpIntegrate).last(func(ev engine.Event) bool {
			return ev.Seq > acc.Seq && dataStr(ev, "plan") == plan && dataStr(ev, "node") == node &&
				dataStr(ev, "acceptance_id") == state.AcceptanceID && dataStr(ev, "head") == state.Head &&
				normalizeRef(dataStr(ev, "integration_ref")) == normalizeRef(state.IntegrationRef)
		})
		batch := ""
		if ok {
			batch = dataStr(ing, "batch_commit")
		}
		if !ok || batch == "" {
			continue
		}
		// the DESIGN issue the source integrated under - the ledger revision its release names, still standing and still
		// the digest it released under - is canonically the one this run stands on: a changed DESIGN of integrated work
		// is not silently ignored
		sled, ok := ledgerOf(sr, ms, dataStr(srel, "revision"))
		if !ok || sr.superseded(sled) || dataStr(sled, "digest") != dataStr(srel, "design_digest") {
			continue
		}
		srcIssue, found, err := designIssue(sled, node)
		if err != nil {
			return engine.OpResult{}, err
		}
		if !found || !reflect.DeepEqual(canonicalIssue(srcIssue), canonicalIssue(curIssue)) {
			continue
		}
		// the design decisions the issue stands on must be the same ones, not only the same ids: a decision whose content
		// changed under a kept id makes the earlier landing unproven
		sourceDesign, err := ledgerDesign(sled)
		if err != nil {
			return engine.OpResult{}, err
		}
		currentDesign, err := ledgerDesign(led)
		if err != nil {
			return engine.OpResult{}, err
		}
		decisionsKept := true
		for _, ref := range srcIssue.DecisionRefs {
			srcDec := canonicalDecisions(decisionsOf(sourceDesign, ref))
			if srcDec == "[]" || srcDec != canonicalDecisions(decisionsOf(currentDesign, ref)) {
				decisionsKept = false
				break
			}
		}
		if !decisionsKept {
			continue
		}
		// the original acceptance evidence: a matching audit subject carrying the review the acceptance rests on
		if _, ok := auditSubjectOf(src.Events, plan, ms, node, state.Head, state.AcceptanceID, n.CriteriaDigest); !ok {
			continue
		}
		// the recorded batch is on the target now: a source whose landing is not there is not reusable
		anc, err := IsAncestor(ctx, o.l.d.Git, o.l.d.Repo, batch, state.Tip)
		if err != nil {
			return engine.OpResult{}, err
		}
		if !anc {
			continue
		}
		// the review base the milestone judged at the time the source landed stays with the landing: a re-planned source
		// carries its original base, and a rerun must not narrow the range it closes over
		reviewBase, err := o.l.milestoneReviewBase(ctx, sr, ms, state.Tip)
		if err != nil {
			return engine.OpResult{}, err
		}
		if reviewBase == "" {
			continue
		}
		if anc, err := IsAncestor(ctx, o.l.d.Git, o.l.d.Repo, reviewBase, batch); err != nil {
			return engine.OpResult{}, err
		} else if !anc {
			continue
		}
		out.Reused, out.Reason = true, ReuseIntegratedReused
		out.Head, out.AcceptanceID, out.BatchCommit = state.Head, state.AcceptanceID, batch
		out.IntegrationRef, out.Tip = state.IntegrationRef, state.Tip
		out.SourceRun, out.SourceAcceptCall, out.SourceIntegrateCall = src.Run, acc.CallID, ing.CallID
		out.ReviewBase = reviewBase
		out.SliceDigest, out.CriteriaDigest = n.SliceDigest, n.CriteriaDigest
		// the run's own ledger stands behind the reuse: its revision and digest, not the source's
		out.Revision, out.DesignDigest = dataStr(led, "revision"), dataStr(led, "digest")
		return finish()
	}
	return unproven("the relay reads the node as integrated, but no earlier run of the plan proves it: no matching release, acceptance, integration and audit under the same slice, criteria and DESIGN")
}

// Proven is the effect proof of a recorded decision. A nonreused decision stays recorded: the node it read may land later in
// the same run, so its row must not make a resume take a different path. A reused decision is proved again against the relay:
// the node still reads integrated, on the same acceptance, head and ref, and the recorded batch is still on the target.
func (o *reuseIntegratedOp) Proven(ctx context.Context, ev []engine.Event, row engine.Event) (bool, string, error) {
	var d ReuseIntegratedResult
	if err := json.Unmarshal(resultOf(row), &d); err != nil {
		return false, "the recorded reuse decision is unreadable", nil
	}
	if !d.Reused {
		return true, "", nil
	}
	r := rows(ev)
	_, plan, err := milestoneOf(r, d.Milestone)
	if err != nil {
		return false, "", err
	}
	n, ok := planNodeOf(r, plan, d.Milestone, d.Node)
	if !ok || n.ID == "" {
		return false, "the milestone's record has no node " + d.Node, nil
	}
	reader, ok := o.l.d.Relay.(IntegratedReader)
	if !ok {
		return false, "no integration reader is wired", nil
	}
	state, err := reader.Integrated(ctx, plan, n)
	if err != nil {
		return false, "", err
	}
	switch {
	case !state.Integrated:
		return false, "the relay no longer reads the node as integrated", nil
	case state.AcceptanceID != d.AcceptanceID || state.Head != d.Head || normalizeRef(state.IntegrationRef) != normalizeRef(d.IntegrationRef):
		return false, "the relay's integrated state of the node moved to another acceptance, head or ref", nil
	}
	anc, err := IsAncestor(ctx, o.l.d.Git, o.l.d.Repo, d.BatchCommit, state.Tip)
	if err != nil {
		return false, "", err
	}
	if !anc {
		return false, "the recorded batch commit is not on the integration target", nil
	}
	if d.ReviewBase == "" {
		return false, "the recorded reuse carries no original review base", nil
	}
	if anc, err := IsAncestor(ctx, o.l.d.Git, o.l.d.Repo, d.ReviewBase, d.BatchCommit); err != nil {
		return false, "", err
	} else if !anc {
		return false, "the recorded review base is not an ancestor of the recorded batch commit", nil
	}
	return true, "", nil
}

// milestoneReviewBase is the base the milestone's close judges the integrated range from (CRW-1597): the first ledger's
// base_tip as before, unless the run inherited landings - then the earliest base in one ancestor order of the ledger's and
// every inherited review base, so a rerun never narrows the range an earlier, re-planned run integrated under. Each inherited
// candidate must sit on the current target; bases in no single ancestor order refuse the close's base, never guess one.
func (l *Lane) milestoneReviewBase(ctx context.Context, r rows, ms, tip string) (string, error) {
	base := ""
	if led, ok := firstLedger(r, ms); ok {
		base = dataStr(led, "base_tip")
	}
	inherited := r.current(OpReuseIntegrated).where(func(ev engine.Event) bool {
		return dataStr(ev, "milestone") == ms && dataBool(ev, "reused")
	})
	if len(inherited) == 0 {
		return base, nil
	}
	refuse := func(detail string) (string, error) {
		return "", &Refusal{ReasonIntegrationUnproven, "milestone " + ms + ": " + detail}
	}
	for _, row := range inherited {
		candidate := dataStr(row, "review_base")
		if candidate == "" {
			return refuse("a reused integration has no original review base")
		}
		onTip, err := IsAncestor(ctx, l.d.Git, l.d.Repo, candidate, tip)
		if err != nil {
			return "", err
		}
		if !onTip {
			return refuse("the original review base is not on the integration target")
		}
		if base == "" || base == candidate {
			base = candidate
			continue
		}
		before, err := IsAncestor(ctx, l.d.Git, l.d.Repo, candidate, base)
		if err != nil {
			return "", err
		}
		if before {
			base = candidate
			continue
		}
		after, err := IsAncestor(ctx, l.d.Git, l.d.Repo, base, candidate)
		if err != nil {
			return "", err
		}
		if !after {
			return refuse("the inherited and current review bases have no single ancestor order")
		}
	}
	onTip, err := IsAncestor(ctx, l.d.Git, l.d.Repo, base, tip)
	if err != nil {
		return "", err
	}
	if !onTip {
		return refuse("the milestone review base is not on the integration target")
	}
	return base, nil
}

// reusedAudits are the audits of the acceptances the milestone's reused integrations rest on (CRW-1597): a close over an
// inherited landing audits the original acceptance in its source run's journal, not a fabricated call of this run. Each reused
// row names its source run and calls; a source that cannot be read, or whose accept or integrate call does not match what the
// row recorded, is an error - never a subject silently left out.
func (l *Lane) reusedAudits(ctx context.Context, r rows, ms string, reader audit.Reader) ([]audit.Result, error) {
	reused := r.current(OpReuseIntegrated).where(func(ev engine.Event) bool {
		return dataStr(ev, "milestone") == ms && dataBool(ev, "reused")
	})
	if len(reused) == 0 {
		return nil, nil
	}
	if l.d.DesignHistory == nil {
		return nil, fmt.Errorf("crw.audit: milestone %s rests on an inherited acceptance and no run history is wired to audit it", ms)
	}
	sources, err := l.d.DesignHistory(ctx, dataStr(reused[0], "plan"))
	if err != nil {
		return nil, err
	}
	byRun := map[string]DesignRun{}
	for _, s := range sources {
		if s.Run != "" && s.Reason == "" {
			byRun[s.Run] = s
		}
	}
	var out []audit.Result
	for _, row := range reused {
		var d ReuseIntegratedResult
		if err := json.Unmarshal(resultOf(row), &d); err != nil || d.SourceRun == "" {
			return nil, fmt.Errorf("crw.audit: the reuse record %s of node %s is unreadable", row.CallID, dataStr(row, "node"))
		}
		src, ok := byRun[d.SourceRun]
		if !ok {
			return nil, fmt.Errorf("crw.audit: the source run %s of the inherited acceptance of node %s is not a readable source", d.SourceRun, d.Node)
		}
		sr := rows(src.Events)
		acc, ok := sr.current(OpAccept).last(func(ev engine.Event) bool { return ev.CallID == d.SourceAcceptCall })
		if !ok || dataStr(acc, "node") != d.Node || dataStr(acc, "acceptance_id") != d.AcceptanceID || dataStr(acc, "head") != d.Head || dataStr(acc, "plan") != d.Plan {
			return nil, fmt.Errorf("crw.audit: the acceptance call %s the reuse of node %s names does not match its record in run %s", d.SourceAcceptCall, d.Node, d.SourceRun)
		}
		ing, ok := sr.current(OpIntegrate).last(func(ev engine.Event) bool { return ev.CallID == d.SourceIntegrateCall })
		if !ok || dataStr(ing, "node") != d.Node || dataStr(ing, "plan") != d.Plan || dataStr(ing, "acceptance_id") != d.AcceptanceID || dataStr(ing, "head") != d.Head ||
			dataStr(ing, "batch_commit") != d.BatchCommit || normalizeRef(dataStr(ing, "integration_ref")) != normalizeRef(d.IntegrationRef) || ing.Seq <= acc.Seq {
			return nil, fmt.Errorf("crw.audit: the integration call %s the reuse of node %s names does not match its record in run %s", d.SourceIntegrateCall, d.Node, d.SourceRun)
		}
		s, ok := auditSubjectOf(src.Events, d.Plan, ms, d.Node, d.Head, d.AcceptanceID, d.CriteriaDigest)
		if !ok {
			return nil, fmt.Errorf("crw.audit: no audit subject of the inherited acceptance %s of node %s in run %s", d.AcceptanceID, d.Node, d.SourceRun)
		}
		out = append(out, audit.Audit(ctx, reader, src.Events, s))
	}
	return out, nil
}
