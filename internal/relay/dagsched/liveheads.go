package dagsched

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"sort"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dag"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// sweepHead is a live node and the head a sweep measures it at, or the reason it has none.
type sweepHead struct{ Node, Head, Source, Reason string }

// sweepHeads names, for every live implementation node of the plan (one that holds its edit regions) and every node the caller named a head for, the head to measure, in node order. The head is the one the
// parent named; else the head of the node's current accepted result; else the HEAD of the checkout its child works in. A work report is not a source: this build has no writer of one (progress.go).
func (s *Scheduler) sweepHeads(ctx context.Context, q store.Querier, plan string, snap dag.Snapshot, checkout string, explicit map[string]string) ([]sweepHead, error) {
	nodes := append([]dag.SnapNode(nil), snap.Nodes...)
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].NodeID < nodes[j].NodeID })
	var out []sweepHead
	for _, n := range nodes {
		if n.Kind != dag.NodeImplementation {
			continue
		}
		st, err := s.stateOf(ctx, q, plan, snap, n)
		if err != nil {
			return nil, err
		}
		head, named := explicit[n.NodeID]
		if !named && !(st.Owned && st.Holds) {
			continue
		}
		h := sweepHead{Node: n.NodeID}
		if named {
			h.Head, h.Source = head, HeadExplicit
			out = append(out, h)
			continue
		}
		rel, relFound, err := currentRelationshipOf(ctx, q, plan, n.NodeID)
		if err != nil {
			return nil, err
		}
		accepted, err := s.currentAcceptanceHead(ctx, q, st.Acc, st.HasAcc, rel, relFound)
		if err != nil {
			return nil, err
		}
		if accepted != "" {
			h.Head, h.Source = accepted, HeadAcceptance
			out = append(out, h)
			continue
		}
		if h.Head, h.Reason, err = s.childCheckoutHead(ctx, q, rel, relFound, checkout); err != nil {
			return nil, err
		}
		if h.Reason == "" {
			h.Source = HeadChildCheckout
		}
		out = append(out, h)
	}
	return out, nil
}

// currentAcceptanceHead is the head the relay read from the forge when it accepted the node's result, when that result is still the node's current one: the acceptance stands on the relationship the node
// runs on now, on that relationship's current generation and on the generation's current head event. A correction generation, or a newer report of the same generation, makes it history (the acceptance
// stays active until the new result is accepted, so the node's reading does not say so itself). After a recorded base refresh (baserefresh.go) what the acceptance stands on is the later generation, its head
// event and its head, and that head is the one returned. "" is no current accepted head.
func (s *Scheduler) currentAcceptanceHead(ctx context.Context, q store.Querier, acc Acceptance, hasAcc bool, rel relRow, relFound bool) (string, error) {
	if !hasAcc || acc.HeadSHA == "" || !relFound || rel.ID != acc.RelationshipID {
		return "", nil
	}
	stand, err := s.standOf(ctx, q, acc)
	if err != nil || rel.Generation != stand.Generation {
		return "", err
	}
	head, err := delivery.HeadRevisionFrom(ctx, q, stand.RelationshipID, stand.Generation)
	if err != nil {
		return "", err
	}
	if id, _ := objString(head, "eventId"); id != stand.EventID {
		return "", nil
	}
	return stand.Head, nil
}

// childCheckoutHead is the HEAD of the checkout the node's current relationship says its child works in (child_cwd, which managed start records as the child's workspace). It counts only when that checkout
// is a worktree of the sweep's own repository (one git common directory) and not the sweep's own working tree, whose HEAD would be the parent's. Otherwise the reason says why there is none.
func (s *Scheduler) childCheckoutHead(ctx context.Context, q store.Querier, rel relRow, relFound bool, checkout string) (head, reason string, err error) {
	if !relFound {
		return "", ReasonHeadUnknown, nil
	}
	var cwd sql.NullString
	if _, err := queryOne(ctx, q, "SELECT child_cwd FROM relationships WHERE relationship_id = ?", []any{rel.ID}, &cwd); err != nil {
		return "", "", err
	}
	if !cwd.Valid || !isGitCheckout(ctx, cwd.String) {
		return "", ReasonHeadUnknown, nil
	}
	childCommon, childErr := gitCommonDir(ctx, cwd.String)
	sweepCommon, sweepErr := gitCommonDir(ctx, checkout)
	if childErr != nil || sweepErr != nil || childCommon != sweepCommon {
		return "", ReasonCheckoutMismatch, nil
	}
	if top := gitTopLevel(ctx, cwd.String); top != "" && top == gitTopLevel(ctx, checkout) {
		return "", ReasonCheckoutMismatch, nil
	}
	out, err := runGit(ctx, cwd.String, nil, "rev-parse", "HEAD")
	if err != nil || !commitPattern.MatchString(strings.TrimSpace(out)) {
		return "", ReasonHeadUnknown, nil
	}
	return strings.TrimSpace(out), "", nil
}

// gitCommonDir is the directory the objects of a checkout live in, links resolved: two worktrees of one repository share it.
func gitCommonDir(ctx context.Context, checkout string) (string, error) {
	out, err := runGit(ctx, checkout, nil, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return "", err
	}
	dir := strings.TrimSpace(out)
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		dir = resolved
	}
	return dir, nil
}

// gitTopLevel is the root of a working tree, links resolved; "" for a bare repository.
func gitTopLevel(ctx context.Context, checkout string) string {
	out, err := runGit(ctx, checkout, nil, "rev-parse", "--path-format=absolute", "--show-toplevel")
	if err != nil {
		return ""
	}
	top := strings.TrimSpace(out)
	if resolved, err := filepath.EvalSymlinks(top); err == nil {
		top = resolved
	}
	return top
}

// measureSweep asks git about every unordered pair of the heads and every head against every tip. The heads arrive in node order, so the nodes of a pair are in the sorted order the observation rows keep.
// A node with no head, a commit the checkout lacks and a tip that could not be read are members with a reason, in the same order, so a sweep always accounts for every pair and tip.
func measureSweep(ctx context.Context, iso *isolated, heads []sweepHead, tips []SweepTip) ([]SweepMember, error) {
	var out []SweepMember
	for i := range heads {
		for j := i + 1; j < len(heads); j++ {
			l, r := heads[i], heads[j]
			m := SweepMember{Kind: MemberPair, LeftNode: l.Node, RightNode: r.Node, LeftHead: l.Head, RightHead: r.Head, LeftSource: l.Source, RightSource: r.Source}
			switch {
			case l.Reason != "":
				m.Status, m.Reason = MemberUnmeasured, l.Reason
			case r.Reason != "":
				m.Status, m.Reason = MemberUnmeasured, r.Reason
			default:
				pm, err := iso.measurePair(ctx, l.Head, r.Head)
				if err != nil {
					return nil, err
				}
				m.applyPair(pm, false)
			}
			out = append(out, m)
		}
	}
	for _, h := range heads {
		for _, t := range tips {
			m := SweepMember{Kind: MemberTip, LeftNode: h.Node, LeftHead: h.Head, RightHead: t.SHA, LeftSource: h.Source, tipRef: t.label()}
			switch {
			case h.Reason != "":
				m.Status, m.Reason = MemberUnmeasured, h.Reason
			case t.SHA == "":
				m.Status, m.Reason = MemberUnmeasured, ReasonTipUnreadable
			default:
				pm, err := iso.measurePair(ctx, h.Head, t.SHA)
				if err != nil {
					return nil, err
				}
				m.applyPair(pm, true)
			}
			out = append(out, m)
		}
	}
	return out, nil
}

// applyPair gives a member what git said: the merge base and files of a measurement, or the reason there is none. For a tip the second commit is the tip, and its absence is tip_unreadable.
func (m *SweepMember) applyPair(pm pairMeasure, tip bool) {
	if pm.Reason == "" {
		m.Status, m.Files, m.Conflicts, m.base = MemberObserved, pm.Files, len(pm.Files), pm.Base
		return
	}
	m.Status, m.Reason, m.Detail = MemberUnmeasured, pm.Reason, pm.Detail
	if tip && pm.Missing == 2 {
		m.Reason = ReasonTipUnreadable
	}
}

// sweepTips are the tips an explicit sweep measures against: each target read through the tip reader (one that cannot be read is a tip with no commit, which the sweep records as unmeasured), or, with no
// target, the base frozen in the manifest of the node's current execution. A manifest that is only stored (a prepared correction, another plan's node of the same name) is not the node's: the execution links
// it. With neither a target nor a base there is one tip nobody could read.
func (s *Scheduler) sweepTips(ctx context.Context, plan, node string, targets []Target) ([]SweepTip, error) {
	if len(targets) == 0 {
		repository, ref, found, err := s.manifestBase(ctx, s.Store.Q(ctx), plan, node)
		if err != nil {
			return nil, err
		}
		if !found {
			return []SweepTip{{}}, nil
		}
		targets = []Target{{Repository: repository, BaseRef: ref}}
	}
	out := make([]SweepTip, len(targets))
	for i, t := range targets {
		out[i] = SweepTip{Repository: t.Repository, Ref: t.BaseRef}
		if s.Tips == nil {
			continue
		}
		if tip, err := s.Tips.Tip(ctx, t.Repository, t.BaseRef); err == nil {
			out[i].SHA = tip.SHA
		}
	}
	return out, nil
}

// manifestBase is the base (repository and ref) frozen in the input manifest the node's current execution is linked to: the manifest of its current relationship and generation, else of its open release
// intent. found is false when the node has no such manifest or the manifest froze no base.
func (s *Scheduler) manifestBase(ctx context.Context, q store.Querier, plan, node string) (repository, ref string, found bool, err error) {
	if node == "" {
		return "", "", false, nil
	}
	var digest string
	rel, relFound, err := currentRelationshipOf(ctx, q, plan, node)
	if err != nil {
		return "", "", false, err
	}
	if relFound {
		if _, err := queryOne(ctx, q, "SELECT manifest_digest FROM dag_node_executions WHERE plan_id = ? AND node_id = ? AND relationship_id = ? AND execution_generation = ?", []any{plan, node, rel.ID, rel.Generation}, &digest); err != nil {
			return "", "", false, err
		}
	}
	if digest == "" {
		open, ok, err := latestRelease(ctx, q, plan, node)
		if err != nil {
			return "", "", false, err
		}
		if ok {
			digest = open.Digest
		}
	}
	var body string
	if digest == "" {
		return "", "", false, nil
	}
	if ok, err := queryOne(ctx, q, "SELECT body_json FROM dag_input_manifests WHERE manifest_digest = ?", []any{digest}, &body); err != nil || !ok {
		return "", "", false, err
	}
	var manifest struct {
		Base *struct {
			Repository string `json:"repository"`
			Ref        string `json:"ref"`
		} `json:"base"`
	}
	if err := json.Unmarshal([]byte(body), &manifest); err != nil || manifest.Base == nil || manifest.Base.Repository == "" || manifest.Base.Ref == "" {
		return "", "", false, nil
	}
	return manifest.Base.Repository, manifest.Base.Ref, true, nil
}
