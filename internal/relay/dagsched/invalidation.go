package dagsched

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sort"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dag"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// Invalidation (contract 3.1 stale, 8.2, 8.4: E-18, E-20, E-24, E-25). A plan revision can change what an accepted node was built from: its own slice (its spec and its incoming edges) or,
// through an edge, a predecessor's. Staleness is derived while a reading is computed and is never stored:
//
//   - a seed is an accepted node whose current slice digest is not the one the manifest its acceptance consumed recorded (the slice digest covers the node's spec and its incoming edges, so this
//     is what a revision changed: a node edited, an edge added or retired). A change of the criteria alone is a seed until the same output is re-verified against them (E-11, E-21), and
//     a node is a seed while an acceptance it consumed is no longer the active one of its node (contract 3.1, E-25), so a mark outlives the repair of the node above it;
//   - only the seeds and their descendants can be stale, and a node that already landed never is (E-20: its value is in the target branch);
//   - a descendant is judged by value: the manifest it consumed at acceptance is compared with the one BuildManifest builds now (with the base, the volatile snapshots and the rule version the
//     consumed manifest recorded, which are dispatch facts and not plan facts). Equal means current, however much changed above it; a difference counts only when it has a cause: an input
//     edge whose predecessor's accepted result is itself stale, or whose consumed value (the acceptance, the integrated head, the decision) is no longer what satisfies the edge.
//
// A stale predecessor opens no artifact edge (contract 8.2, E-25), so nothing is released onto it.

// The causes of a stale node, as its structured reading names them.
const (
	CauseSliceChanged     = "slice_changed"     // the node's own spec changed
	CauseCriteriaChanged  = "criteria_changed"  // the node's criteria changed and the same output has not been re-verified against them
	CauseEdgeAdded        = "edge_added"        // an incoming edge was added after the node consumed its inputs
	CauseEdgeRetired      = "edge_retired"      // an incoming edge was retired after the node consumed something over it
	CausePredecessorStale = "predecessor_stale" // over an incoming edge the node rests on a predecessor whose accepted result is stale
	CauseInputChanged     = "input_changed"     // over an incoming edge the value the node consumed is no longer the value that satisfies it
)

// Stale is the reading of an accepted node whose result no longer matches the plan. EdgeID and Predecessor name the incoming edge the reason rests on (empty when only the node's own slice
// changed); ConsumedAcceptance is the version of the predecessor the node consumed over it.
type Stale struct {
	Cause string
	// Seed is the node the staleness stems from: the node itself when its own slice changed.
	Seed        string
	EdgeID      string
	Predecessor string
	// ConsumedAcceptance and CurrentAcceptance are the predecessor's acceptance the node consumed over EdgeID and the one that satisfies the edge now (empty when there is none).
	ConsumedAcceptance, CurrentAcceptance string
	// ConsumedDecision and CurrentDecision are <decision id>@<revision> over a decision edge: the decision the node consumed and the one that satisfies the edge now.
	ConsumedDecision, CurrentDecision string
	// ConsumedSlice and CurrentSlice are the slice digest the consumed version rested on and the plan's now: the node's own for slice changes and added or retired edges, the predecessor's
	// for a stale predecessor.
	ConsumedSlice, CurrentSlice string
	// ConsumedManifest is the manifest digest the acceptance consumed; RebuiltManifest the digest of the manifest built now, when the node was rebuilt (a seed is not: its slice digest is part of
	// the manifest, so the two cannot be equal).
	ConsumedManifest, RebuiltManifest string
	// Text is the human sentence the reading's detail carries: it names the edge and the version, so the pass records and the reading's input digest hold them too.
	Text string
}

// Reason is the closed reason of a stale node: stale:edge:<edge_id> when the reason rests on an edge, else stale:criteria_changed or stale:slice_changed.
func (s Stale) Reason() string {
	switch {
	case s.EdgeID != "":
		return StaleEdge(s.EdgeID)
	case s.Cause == CauseCriteriaChanged:
		return StaleCriteriaChanged
	}
	return StaleSliceChanged
}

func (s Stale) object() contract.OrderedObject {
	return contract.OrderedObject{
		{Key: "cause", Value: s.Cause}, {Key: "seed_node_id", Value: optionalText(s.Seed)},
		{Key: "edge_id", Value: optionalText(s.EdgeID)}, {Key: "predecessor_node_id", Value: optionalText(s.Predecessor)},
		{Key: "consumed_acceptance_id", Value: optionalText(s.ConsumedAcceptance)}, {Key: "current_acceptance_id", Value: optionalText(s.CurrentAcceptance)},
		{Key: "consumed_decision", Value: optionalText(s.ConsumedDecision)}, {Key: "current_decision", Value: optionalText(s.CurrentDecision)},
		{Key: "consumed_slice_digest", Value: optionalText(s.ConsumedSlice)}, {Key: "current_slice_digest", Value: optionalText(s.CurrentSlice)},
		{Key: "consumed_manifest_digest", Value: optionalText(s.ConsumedManifest)}, {Key: "rebuilt_manifest_digest", Value: optionalText(s.RebuiltManifest)},
	}
}

// consumedOf is what an accepted node consumed: its active acceptance and the manifest the acceptance names.
type consumedOf struct {
	acc  Acceptance
	body map[string]any
}

// invalidation is the memo of one judgement pass: the accepted nodes with what they consumed, the seeds, the closure and the verdicts. It lives in the context of one Ready call (or of the
// outermost judgement when there is no reading around it), keyed by the plan and the state digest of the snapshot it was made for, and is never kept on the Scheduler: a verdict is a fact
// about one state of the store.
type invalidation struct {
	plan, state string
	prepared    bool
	consumed    map[string]*consumedOf
	seeds       map[string]*Stale
	closure     map[string]bool
	verdicts    map[string]*Stale
	landed      map[string]bool
	busy        map[string]bool
}

type invalidationKey struct{}

// memoFor is the memo of this plan state in ctx, or a new one in a derived context.
func memoFor(ctx context.Context, plan string, snap dag.Snapshot) (context.Context, *invalidation) {
	if m, ok := ctx.Value(invalidationKey{}).(*invalidation); ok && m.plan == plan && m.state == snap.StateDigest {
		return ctx, m
	}
	m := &invalidation{plan: plan, state: snap.StateDigest, consumed: map[string]*consumedOf{}, seeds: map[string]*Stale{}, closure: map[string]bool{},
		verdicts: map[string]*Stale{}, landed: map[string]bool{}, busy: map[string]bool{}}
	return context.WithValue(ctx, invalidationKey{}, m), m
}

func edgesInto(snap dag.Snapshot, node string) []dag.Edge {
	var out []dag.Edge
	for _, e := range incomingEdges(snap, node) {
		out = append(out, e.Edge)
	}
	return out
}

// consumedInputs are the inputs a consumed manifest recorded, by edge id.
func consumedInputs(body map[string]any) map[string]map[string]any {
	out := map[string]map[string]any{}
	items, _ := body["inputs"].([]any)
	for _, item := range items {
		if in, ok := item.(map[string]any); ok {
			if id := textOf(in["edge_id"]); id != "" {
				out[id] = in
			}
		}
	}
	return out
}

// criteriaOnly is whether the node's slice differs from the consumed one by its criteria alone: the slice digest of the current spec with the criteria the manifest consumed is the consumed
// slice digest.
func criteriaOnly(n dag.SnapNode, snap dag.Snapshot, body map[string]any) bool {
	probe := n.Node
	if criteria := textOf(body["criteria_set_digest"]); criteria != "" {
		probe.CriteriaSetDigest = criteria
	}
	return dag.SliceDigest(probe, edgesInto(snap, n.NodeID)) == textOf(body["node_slice_digest"])
}

// seedOf is the seed reading of an accepted node, or nil when it is not a seed: its own slice is not the one its consumed manifest recorded, or an acceptance it consumed is no longer the active
// one of its node. The cause of a changed slice comes from the consumed inputs' edge ids against the current incoming edges: an edge added (the first, by id), an edge retired, else the
// spec. A change of the criteria alone is a seed only until the same output is re-verified against them (the effective criteria of the acceptance are the plan's), contract E-11 and E-21.
func (s *Scheduler) seedOf(ctx context.Context, q store.Querier, plan string, n dag.SnapNode, snap dag.Snapshot, c *consumedOf) (*Stale, error) {
	consumed := textOf(c.body["node_slice_digest"])
	if consumed != "" && consumed != n.SliceDigest {
		st := &Stale{Cause: CauseSliceChanged, Seed: n.NodeID, ConsumedSlice: consumed, CurrentSlice: n.SliceDigest, ConsumedManifest: c.acc.ManifestDigest}
		resolved := false
		if criteriaOnly(n, snap, c.body) {
			effective, err := effectiveCriteria(ctx, q, c.acc)
			if err != nil {
				return nil, err
			}
			if effective != n.CriteriaSetDigest {
				st.Cause = CauseCriteriaChanged
				st.Text = fmt.Sprintf("the node's criteria changed after it consumed its inputs (its slice was %s, the plan's is %s) and the same output has not been re-verified against them", short(consumed), short(n.SliceDigest))
				return st, nil
			}
			resolved = true // re-verified against the new criteria: the criteria predicate is satisfied again and what the node consumed is unchanged
		}
		if !resolved {
			inputs := consumedInputs(c.body)
			now := map[string]dag.SnapEdge{}
			for _, e := range incomingEdges(snap, n.NodeID) {
				now[e.EdgeID] = e
			}
			var added, retired []string
			for id := range now {
				if _, had := inputs[id]; !had {
					added = append(added, id)
				}
			}
			for id := range inputs {
				if _, has := now[id]; !has {
					retired = append(retired, id)
				}
			}
			sort.Strings(added)
			sort.Strings(retired)
			switch {
			case len(added) > 0:
				st.Cause, st.EdgeID, st.Predecessor = CauseEdgeAdded, added[0], now[added[0]].FromNodeID
				st.Text = fmt.Sprintf("edge %s from %s was added after the node consumed its inputs (its slice was %s, the plan's is %s)", st.EdgeID, st.Predecessor, short(consumed), short(n.SliceDigest))
			case len(retired) > 0:
				in := inputs[retired[0]]
				st.Cause, st.EdgeID, st.Predecessor, st.ConsumedAcceptance = CauseEdgeRetired, retired[0], textOf(in["from_node_id"]), textOf(in["acceptance_id"])
				if textOf(in["kind"]) == dag.EdgeDecision {
					// a decision edge hands over a recorded decision, not an acceptance: the version consumed is the decision and its revision
					st.ConsumedDecision = textOf(in["decision_id"]) + "@" + textOf(in["decision_revision"])
					st.Text = fmt.Sprintf("edge %s from %s was retired after the node consumed decision %s over it (its slice was %s, the plan's is %s)", st.EdgeID, st.Predecessor, st.ConsumedDecision, short(consumed), short(n.SliceDigest))
				} else {
					st.Text = fmt.Sprintf("edge %s from %s was retired after the node consumed acceptance %s over it (its slice was %s, the plan's is %s)", st.EdgeID, st.Predecessor, short(st.ConsumedAcceptance), short(consumed), short(n.SliceDigest))
				}
			default:
				st.Text = fmt.Sprintf("the node's own slice changed after it consumed its inputs (its slice was %s, the plan's is %s)", short(consumed), short(n.SliceDigest))
			}
			return st, nil
		}
	}
	// an acceptance the node consumed is no longer the active acceptance of its node (a predecessor accepted again, contract 3.1 and E-25): the node rests on a version nobody accepts now. A node
	// that landed is exempt (E-20), which is also what keeps B-14 reachable behind it
	inputs := consumedInputs(c.body)
	ids := make([]string, 0, len(inputs))
	for id := range inputs {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		in := inputs[id]
		acceptance, from := textOf(in["acceptance_id"]), textOf(in["from_node_id"])
		if acceptance == "" || from == "" {
			continue // a decision input has no acceptance
		}
		var one int
		active, err := queryOne(ctx, q, "SELECT 1 FROM dag_acceptances WHERE acceptance_id = ? AND plan_id = ? AND node_id = ? AND state = 'active'", []any{acceptance, plan, from}, &one)
		if err != nil {
			return nil, err
		}
		if active {
			continue
		}
		st := &Stale{Cause: CauseInputChanged, Seed: from, EdgeID: id, Predecessor: from, ConsumedAcceptance: acceptance, ConsumedManifest: c.acc.ManifestDigest}
		if current, has, err := loadActiveAcceptance(ctx, q, plan, from); err != nil {
			return nil, err
		} else if has {
			st.CurrentAcceptance = current.AcceptanceID
		}
		st.Text = fmt.Sprintf("over edge %s it consumed acceptance %s of %s, which is no longer the active acceptance of that node (now %s)", id, short(acceptance), from, short(st.CurrentAcceptance))
		return st, nil
	}
	return nil, nil
}

// prepare reads what every accepted node consumed and finds the seeds and the closure: the seeds and everything below them in the current graph. A node that has no acceptance, or whose
// consumed manifest cannot be read (the edges out of it already report that), is not judged.
func (s *Scheduler) prepare(ctx context.Context, q store.Querier, plan string, snap dag.Snapshot, m *invalidation) error {
	if m.prepared {
		return nil
	}
	for _, n := range snap.Nodes {
		acc, has, err := loadActiveAcceptance(ctx, q, plan, n.NodeID)
		if err != nil {
			return err
		}
		if !has {
			continue
		}
		body, found, err := dag.ReadManifestOn(ctx, q, acc.ManifestDigest)
		var corrupt *dag.CorruptError
		if errors.As(err, &corrupt) || (err == nil && !found) {
			continue
		}
		if err != nil {
			return err
		}
		c := &consumedOf{acc: acc, body: body}
		m.consumed[n.NodeID] = c
		seed, err := s.seedOf(ctx, q, plan, n, snap, c)
		if err != nil {
			return err
		}
		if seed != nil {
			if landed, err := s.landedNode(ctx, q, plan, snap, n, c, m); err != nil {
				return err
			} else if !landed {
				m.seeds[n.NodeID] = seed
			}
		}
	}
	below := map[string][]string{}
	for _, e := range snap.Edges {
		below[e.FromNodeID] = append(below[e.FromNodeID], e.ToNodeID)
	}
	queue := make([]string, 0, len(m.seeds))
	for id := range m.seeds {
		queue = append(queue, id)
	}
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		if m.closure[id] {
			continue
		}
		m.closure[id] = true
		queue = append(queue, below[id]...)
	}
	m.prepared = true
	return nil
}

// landedNode is whether an accepted implementation node's head landed everywhere it has to (contract E-20: a node that landed is never invalidated, its result is in the target).
func (s *Scheduler) landedNode(ctx context.Context, q store.Querier, plan string, snap dag.Snapshot, n dag.SnapNode, c *consumedOf, m *invalidation) (bool, error) {
	if n.Kind != dag.NodeImplementation || c.acc.HeadSHA == "" {
		return false, nil
	}
	if landed, known := m.landed[n.NodeID]; known {
		return landed, nil
	}
	landed, _, err := s.nodeIntegrated(ctx, q, plan, snap, c.acc)
	if err != nil {
		return false, err
	}
	m.landed[n.NodeID] = landed
	return landed, nil
}

// staleOf is the judgement of one node's accepted result against the plan as it is now: nil when the node has no acceptance, is outside the closure of the seeds, landed, or is current. It is
// independent of how the reading shows the node (a paused or evicted seed is still stale for the edges built on it). The verdicts of one pass are memoized in the context.
func (s *Scheduler) staleOf(ctx context.Context, q store.Querier, plan string, snap dag.Snapshot, n dag.SnapNode) (*Stale, error) {
	ctx, m := memoFor(ctx, plan, snap)
	if v, done := m.verdicts[n.NodeID]; done {
		return v, nil
	}
	if m.busy[n.NodeID] {
		return nil, nil // the plan is acyclic (CRW-183 refuses a cycle); one would be cut rather than looped on
	}
	m.busy[n.NodeID] = true
	defer delete(m.busy, n.NodeID)
	if err := s.prepare(ctx, q, plan, snap, m); err != nil {
		return nil, err
	}
	var out *Stale
	if c := m.consumed[n.NodeID]; c != nil && m.closure[n.NodeID] {
		switch seed := m.seeds[n.NodeID]; {
		case seed != nil:
			out = seed
		default:
			landed, err := s.landedNode(ctx, q, plan, snap, n, c, m)
			if err != nil {
				return nil, err
			}
			if !landed {
				if out, err = s.judgeBelow(ctx, q, plan, snap, n, c); err != nil {
					return nil, err
				}
			}
		}
	}
	m.verdicts[n.NodeID] = out
	return out, nil
}

// asConsumed is the node as the consumed manifest saw it when only its criteria moved since (they are judged by the criteria predicate): its criteria and slice digest are the consumed ones,
// so the rebuild compares what the plan says about the node's inputs and not the criteria.
func asConsumed(n dag.SnapNode, snap dag.Snapshot, body map[string]any) dag.SnapNode {
	if consumed := textOf(body["node_slice_digest"]); consumed != n.SliceDigest && criteriaOnly(n, snap, body) {
		n.CriteriaSetDigest, n.SliceDigest = textOf(body["criteria_set_digest"]), consumed
	}
	return n
}

// rebuildAsConsumed builds the manifest the node would consume from the store now. The base, the volatile snapshots, the rule version and the author and time are the consumed manifest's:
// they are dispatch facts, not plan facts, so a dev that moved or a clock never changes the digest, and no file is read. complete is false when an incoming edge no longer yields an input.
func (s *Scheduler) rebuildAsConsumed(ctx context.Context, q store.Querier, plan string, snap dag.Snapshot, n dag.SnapNode, body map[string]any) (digest string, complete bool, err error) {
	rule, _ := body["rule_version"].(map[string]any)
	in := ManifestInput{CreatedByTaskID: textOf(body["created_by_task_id"]), CreatedAt: textOf(body["created_at"]),
		RuleVersion: RuleVersion{SkillsDigest: textOf(rule["skills_digest"]), Model: textOf(rule["model"]), Effort: textOf(rule["effort"]), PromptTemplate: textOf(rule["prompt_template"]), RelayBuild: textOf(rule["relay_build"])}}
	if base, ok := body["base"].(map[string]any); ok {
		in.Base = &BaseRef{Repository: textOf(base["repository"]), Ref: textOf(base["ref"]), SHA: textOf(base["sha"])}
	}
	var roots []string
	items, _ := body["volatile"].([]any)
	for _, item := range items {
		if v, ok := item.(map[string]any); ok {
			in.Volatile = append(in.Volatile, Volatile{Source: textOf(v["source"]), SnapshotURI: textOf(v["snapshot_uri"]), SHA256: textOf(v["sha256"]), CapturedAt: textOf(v["captured_at"])})
			roots = append(roots, filepath.Dir(textOf(v["snapshot_uri"])))
		}
	}
	built, _, err := s.BuildManifest(ctx, q, plan, snap, asConsumed(n, snap, body), in, VerifyOptions{SkipFileBytes: true, ArtifactRoots: roots})
	if err != nil {
		return "", false, err
	}
	// BuildManifest leaves the digest out when any finding exists (a shape finding of a manifest older than the rule version check, for one); what this judgement needs is whether every incoming
	// edge yielded its input, and the digest of those inputs.
	inputs, _ := built["inputs"].([]any)
	if len(inputs) != len(incomingEdges(snap, n.NodeID)) {
		return "", false, nil
	}
	sizesAsConsumed(inputs, consumedInputs(body))
	return dag.ManifestDigest(built), true, nil
}

// sizesAsConsumed gives every artifact the size the consumed manifest recorded for the same file. A receipt may declare an artifact without a size, and BuildManifest then reads the size of the file
// as it is now; the rebuilt digest, and what the reading prints of it, must not depend on a file's size changing, so the consumed size is kept for an artifact with the same path and digest.
func sizesAsConsumed(inputs []any, consumed map[string]map[string]any) {
	for _, item := range inputs {
		in, _ := item.(map[string]any)
		was := consumed[textOf(in["edge_id"])]
		built, _ := in["artifacts"].([]any)
		recorded, _ := was["artifacts"].([]any)
		for _, a := range built {
			artifact, _ := a.(map[string]any)
			for _, r := range recorded {
				previous, _ := r.(map[string]any)
				if previous["uri"] == artifact["uri"] && previous["sha256"] == artifact["sha256"] {
					if size, ok := previous["bytes"]; ok {
						artifact["bytes"] = size
					}
					break
				}
			}
		}
	}
}

// judgeBelow judges a node that is inside the closure but is not a seed itself. The manifest it consumed is compared with the one built now; equal means current (the early cutoff: what
// changed above did not change what the node consumes). A difference counts when explain finds the value that changed.
func (s *Scheduler) judgeBelow(ctx context.Context, q store.Querier, plan string, snap dag.Snapshot, n dag.SnapNode, c *consumedOf) (*Stale, error) {
	rebuilt, complete, err := s.rebuildAsConsumed(ctx, q, plan, snap, n, c.body)
	if err != nil {
		return nil, err
	}
	if complete && rebuilt == c.acc.ManifestDigest {
		return nil, nil
	}
	st, err := s.explain(ctx, q, plan, snap, n, c)
	if st != nil {
		st.ConsumedManifest, st.RebuiltManifest = c.acc.ManifestDigest, rebuilt
	}
	return st, err
}

// explain finds the first incoming edge (by id) over which the node's consumed value is no longer what the plan and the store give: a predecessor whose accepted result is stale (asked
// directly, so that another reason the edge shows, such as blocked:stale_criteria, hides nothing), or a satisfied edge whose consumed value differs: the acceptance, the head an integration
// landed (not the tip it was observed at: containment is monotone), the decision (id, digest and revision, as the manifest records them). A difference with no such edge (another edge fact such as a
// moved head or a cancelled predecessor) is not a stale result: the edges and the merge lane report those.
func (s *Scheduler) explain(ctx context.Context, q store.Querier, plan string, snap dag.Snapshot, n dag.SnapNode, c *consumedOf) (*Stale, error) {
	inputs := consumedInputs(c.body)
	for _, e := range incomingEdges(snap, n.NodeID) {
		in := inputs[e.EdgeID]
		consumed := textOf(in["acceptance_id"])
		if e.Kind == dag.EdgeArtifactVerified {
			if pred, ok := nodeOf(snap, e.FromNodeID); ok {
				above, err := s.staleOf(ctx, q, plan, snap, pred)
				if err != nil {
					return nil, err
				}
				if above != nil {
					return s.stalePredecessorReading(ctx, q, plan, snap, e, consumed, above)
				}
			}
		}
		status, err := s.edgeStatus(ctx, q, plan, snap, e)
		if err != nil {
			return nil, err
		}
		if !status.Satisfied {
			continue
		}
		switch e.Kind {
		case dag.EdgeDecision:
			was := textOf(in["decision_id"]) + "@" + textOf(in["decision_revision"])
			now := status.DecisionID + "@" + itoa64(status.DecisionRevision)
			if textOf(in["decision_digest"]) != e.DecisionDigest || was != now {
				st := changedInput(e, "", "")
				st.ConsumedDecision, st.CurrentDecision = was, now
				st.Text = fmt.Sprintf("over edge %s it consumed decision %s of %s, and the edge is now satisfied by decision %s", e.EdgeID, was, e.FromNodeID, now)
				return st, nil
			}
		default:
			if consumed != status.AcceptanceID {
				return changedInput(e, consumed, status.AcceptanceID), nil
			}
			if e.Kind == dag.EdgeIntegrated {
				changed, err := s.landingChanged(ctx, q, status, in)
				if err != nil {
					return nil, err
				}
				if changed {
					return changedInput(e, consumed, status.AcceptanceID), nil
				}
			}
		}
	}
	return nil, nil
}

// landingChanged is whether what an integrated edge hands over is no longer what the node consumed: another head landed, or the landed commit it recorded is not named by any observation of the
// current containment run (the satisfying observation and the positive ones after it). The tip an observation read is not a value the node consumed (ancestry is monotone, contract E-27), so an
// older record that names the tip of a later observation of the same run is unchanged; a landing no observation of the run names is another landing.
func (s *Scheduler) landingChanged(ctx context.Context, q store.Querier, status EdgeStatus, in map[string]any) (bool, error) {
	acc, err := loadAcceptanceByID(ctx, q, status.AcceptanceID)
	if err != nil {
		return false, err
	}
	if acc.HeadSHA != textOf(in["head_sha"]) {
		return true, nil
	}
	var one int
	named, err := queryOne(ctx, q, "SELECT 1 FROM dag_integration_observations base"+
		" JOIN dag_integration_observations o ON o.acceptance_id = base.acceptance_id AND o.repository = base.repository AND o.base_ref = base.base_ref"+
		"  AND o.subject_sha = base.subject_sha AND o.observed_seq >= base.observed_seq AND o.is_ancestor = 1 AND o.reverted_by IS NULL"+
		"  AND (o.merge_turn_id IS NULL OR EXISTS (SELECT 1 FROM merge_turns t WHERE t.turn_id = o.merge_turn_id AND t.state = 'landed' AND t.candidate_head = o.subject_sha"+
		"   AND t.repository = o.repository AND t.base_ref = o.base_ref))"+
		" LEFT JOIN merge_turns m ON m.turn_id = o.merge_turn_id"+
		" WHERE base.observation_id = ? AND COALESCE(NULLIF(m.landed_sha, ''), o.tip_sha) = ? LIMIT 1", []any{status.ObservationID, textOf(in["landed_sha"])}, &one)
	return !named, err
}

func changedInput(e dag.SnapEdge, consumed, current string) *Stale {
	st := &Stale{Cause: CauseInputChanged, Seed: e.FromNodeID, EdgeID: e.EdgeID, Predecessor: e.FromNodeID, ConsumedAcceptance: consumed, CurrentAcceptance: current}
	st.Text = fmt.Sprintf("over edge %s it consumed acceptance %s of %s, and the value that satisfies the edge now is acceptance %s", e.EdgeID, short(consumed), e.FromNodeID, short(current))
	return st
}

// stalePredecessorReading names the edge, the predecessor and the version of it the node rests on when the predecessor's accepted result is stale (above is its own reading).
func (s *Scheduler) stalePredecessorReading(ctx context.Context, q store.Querier, plan string, snap dag.Snapshot, e dag.SnapEdge, consumed string, above *Stale) (*Stale, error) {
	pred, ok := nodeOf(snap, e.FromNodeID)
	if !ok {
		return nil, errors.New("edge " + e.EdgeID + " starts at a node the plan does not hold")
	}
	st := &Stale{Cause: CausePredecessorStale, Seed: pred.NodeID, EdgeID: e.EdgeID, Predecessor: pred.NodeID, ConsumedAcceptance: consumed, CurrentSlice: pred.SliceDigest}
	if above.Seed != "" {
		st.Seed = above.Seed
	}
	if active, has, err := loadActiveAcceptance(ctx, q, plan, pred.NodeID); err != nil {
		return nil, err
	} else if has {
		st.CurrentAcceptance = active.AcceptanceID
	}
	if consumed != "" {
		// what the consumed version rested on: absent or unreadable records leave the digest out of the reading (the edges report them); a failure to read the store is an error
		acc, err := loadAcceptanceByID(ctx, q, consumed)
		switch {
		case errors.Is(err, errNoAcceptance):
		case err != nil:
			return nil, err
		default:
			body, found, err := dag.ReadManifestOn(ctx, q, acc.ManifestDigest)
			var corrupt *dag.CorruptError
			switch {
			case errors.As(err, &corrupt):
			case err != nil:
				return nil, err
			case found:
				st.ConsumedSlice = textOf(body["node_slice_digest"])
			}
		}
	}
	st.Text = fmt.Sprintf("over edge %s it consumed acceptance %s of %s (slice %s); %s is stale (%s), stemming from %s", e.EdgeID, short(consumed), pred.NodeID, short(st.ConsumedSlice), pred.NodeID, above.Reason(), st.Seed)
	if st.ConsumedSlice != "" && st.ConsumedSlice != pred.SliceDigest {
		st.Text += fmt.Sprintf(" and the plan's slice of %s is now %s", pred.NodeID, short(pred.SliceDigest))
	}
	return st, nil
}

// stalePredecessor is the gate on an artifact edge (contract 8.2, E-25): the accepted result of a stale predecessor opens nothing, so a node is never released onto it. It names the edge and the
// predecessor version in the detail.
func (s *Scheduler) stalePredecessor(ctx context.Context, q store.Querier, plan string, snap dag.Snapshot, e dag.SnapEdge, from dag.SnapNode) (*EdgeStatus, error) {
	st, err := s.staleOf(ctx, q, plan, snap, from)
	if err != nil || st == nil {
		return nil, err
	}
	blockedStatus := blocked(BlockedStalePredecessor, fmt.Sprintf("edge %s: predecessor %s is stale (%s): %s", e.EdgeID, from.NodeID, st.Reason(), st.Text))
	return &blockedStatus, nil
}
