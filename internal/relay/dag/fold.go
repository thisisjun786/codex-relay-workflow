package dag

import (
	"fmt"
	"sort"
	"strings"
)

// Diff is what a fold changes in the materialized rows, in the order a writer applies it: old versions
// are retired before new ones are inserted (the partial unique index of live rows is checked per statement).
type Diff struct {
	RetireNodes []NodeVersion // rows now retired (their RetiredRev is set)
	InsertNodes []NodeVersion
	RetireEdges []EdgeRow
	InsertEdges []EdgeRow
}

type liveNode struct {
	Node
	Supersedes string
}

// Apply folds a revision onto prev and returns the state one revision later, strictly: every rule of
// the plan, the graph-size limits and the project binding. It writes nothing; the caller stores the
// Diff only when there are no violations.
func Apply(prev State, rev Revision) (State, Diff, []Violation) {
	var vs []Violation
	if prev.ProjectKey != "" && rev.ProjectKey != prev.ProjectKey {
		vs = append(vs, Violation{Rule: RuleProjectMismatch, Path: "$.project_key",
			Detail: fmt.Sprintf("plan %s belongs to project %s, not %s", prev.PlanID, prev.ProjectKey, rev.ProjectKey)})
	}
	if prev.ProjectKey == "" {
		prev.ProjectKey = rev.ProjectKey
	}
	next, diff, more := applyChanges(prev, rev.Changes, true)
	return next, diff, append(vs, more...)
}

// applyChanges is the fold. strict adds the graph-size limits: a replay of committed events does not
// re-judge them, since the limits are a rule of the writer and a log written under an older limit is
// still the log.
func applyChanges(prev State, changes []Change, strict bool) (State, Diff, []Violation) {
	var vs []Violation
	add := func(rule, path, format string, args ...any) {
		vs = append(vs, Violation{Rule: rule, Path: path, Detail: fmt.Sprintf(format, args...)})
	}

	parentNodes := map[string]NodeVersion{} // live in the parent plan
	live := map[string]liveNode{}           // live after the changes so far
	taken := map[string]string{}            // node ids that may not be introduced again, and why
	retiredAt := map[string]int64{}
	for _, row := range prev.Nodes {
		if row.RetiredRev == 0 {
			parentNodes[row.NodeID] = row
			live[row.NodeID] = liveNode{row.Node, row.SupersedesNodeID}
			taken[row.NodeID] = "is in the plan"
		} else if row.RetiredRev > retiredAt[row.NodeID] {
			retiredAt[row.NodeID] = row.RetiredRev
		}
	}
	for id, rev := range retiredAt {
		if _, ok := taken[id]; !ok {
			taken[id] = fmt.Sprintf("was retired at revision %d", rev)
		}
	}
	parentEdges := map[string]EdgeRow{}
	liveEdges := map[string]Edge{}
	takenEdge := map[string]string{}
	retiredEdgeAt := map[string]int64{}
	for _, row := range prev.Edges {
		if row.RetiredRev == 0 {
			parentEdges[row.EdgeID] = row
			liveEdges[row.EdgeID] = row.Edge
			takenEdge[row.EdgeID] = "is in the plan"
		} else if row.RetiredRev > retiredEdgeAt[row.EdgeID] {
			retiredEdgeAt[row.EdgeID] = row.RetiredRev
		}
	}
	for id, rev := range retiredEdgeAt {
		if _, ok := takenEdge[id]; !ok {
			takenEdge[id] = fmt.Sprintf("was retired at revision %d", rev)
		}
	}

	changedNode := map[string]int{} // parent node id -> index of the change that touched it
	changedEdge := map[string]int{}
	addedAt := map[string]int{} // edge id -> index of the change that added it
	addedNode := map[string]int{}

	// target checks that a change addresses a node the parent plan holds and that no other change of
	// this revision already addressed.
	targetNode := func(i int, path, id string) bool {
		if j, ok := addedNode[id]; ok {
			add(RuleUnknownNode, path, "node %s is added by changes[%d] in this revision; add it with its final spec instead", id, j)
			return false
		}
		if _, ok := parentNodes[id]; !ok {
			add(RuleUnknownNode, path, "node %s is not in the plan (%s)", id, describe(taken, id))
			return false
		}
		if j, ok := changedNode[id]; ok {
			add(RuleConflictingChanges, path, "node %s is already changed by changes[%d] in this revision", id, j)
			return false
		}
		changedNode[id] = i
		return true
	}
	introduce := func(i int, path string, n Node, supersedes string) {
		if why, ok := taken[n.NodeID]; ok {
			add(RuleDuplicateNodeID, path, "node id %s %s; node ids are never reused within a plan", n.NodeID, why)
			return
		}
		taken[n.NodeID] = fmt.Sprintf("is added by changes[%d] in this revision", i)
		addedNode[n.NodeID] = i
		live[n.NodeID] = liveNode{n, supersedes}
	}

	for i, c := range changes {
		p := fmt.Sprintf("changes[%d]", i)
		switch c.Op {
		case OpAddNode:
			introduce(i, p+".node.node_id", *c.Node, "")
		case OpUpdateNode:
			if targetNode(i, p+".node.node_id", c.Node.NodeID) {
				live[c.Node.NodeID] = liveNode{*c.Node, live[c.Node.NodeID].Supersedes}
			}
		case OpReplaceNode:
			if targetNode(i, p+".supersedes_node_id", c.SupersedesNodeID) {
				delete(live, c.SupersedesNodeID)
			}
			introduce(i, p+".node.node_id", *c.Node, c.SupersedesNodeID)
		case OpRetireNode:
			if targetNode(i, p+".node_id", c.NodeID) {
				delete(live, c.NodeID)
			}
		case OpAddEdge:
			e := *c.Edge
			if why, ok := takenEdge[e.EdgeID]; ok {
				add(RuleDuplicateEdgeID, p+".edge.edge_id", "edge id %s %s; edge ids are never reused within a plan", e.EdgeID, why)
				continue
			}
			takenEdge[e.EdgeID] = fmt.Sprintf("is added by changes[%d] in this revision", i)
			addedAt[e.EdgeID] = i
			liveEdges[e.EdgeID] = e
		case OpRetireEdge:
			path := p + ".edge_id"
			if j, ok := addedAt[c.EdgeID]; ok {
				add(RuleUnknownEdge, path, "edge %s is added by changes[%d] in this revision; add only the edges the plan keeps", c.EdgeID, j)
			} else if _, ok := parentEdges[c.EdgeID]; !ok {
				add(RuleUnknownEdge, path, "edge %s is not in the plan (%s)", c.EdgeID, describe(takenEdge, c.EdgeID))
			} else if j, ok := changedEdge[c.EdgeID]; ok {
				add(RuleConflictingChanges, path, "edge %s is already retired by changes[%d] in this revision", c.EdgeID, j)
			} else {
				changedEdge[c.EdgeID] = i
				delete(liveEdges, c.EdgeID)
			}
		}
	}

	// The rules of the plan the changes produce.
	if strict && len(live) > MaxNodes {
		add(RuleLimitExceeded, "plan", "the plan would hold %d nodes; the limit is %d", len(live), MaxNodes)
	}
	if strict && len(liveEdges) > MaxEdges {
		add(RuleLimitExceeded, "plan", "the plan would hold %d edges; the limit is %d", len(liveEdges), MaxEdges)
	}
	if len(live) == 0 {
		add(RuleEmptyGraph, "plan", "the plan would hold no node")
	}
	edgeIDs := make([]string, 0, len(liveEdges))
	for id := range liveEdges {
		edgeIDs = append(edgeIDs, id)
	}
	sort.Strings(edgeIDs)
	edgePath := func(id string) string {
		if i, ok := addedAt[id]; ok {
			return fmt.Sprintf("changes[%d].edge", i)
		}
		return "edges." + id
	}
	for _, id := range edgeIDs {
		vs = append(vs, checkEdge(liveEdges[id], live, edgePath(id))...)
	}
	vs = append(vs, findCycle(live, liveEdges)...)
	if len(vs) > 0 {
		return State{}, Diff{}, vs
	}

	return materialize(prev, live, liveEdges)
}

func describe(taken map[string]string, id string) string {
	if why, ok := taken[id]; ok {
		return "it " + why
	}
	return "no such id was ever used"
}

// output matrix: what each kind of node offers (contract 1.2) and what each kind of edge needs (2.4).
// The first three rules are stated by the contract; the others are its reading (docs/relay/dag-plans.md).
func checkEdge(e Edge, nodes map[string]liveNode, path string) []Violation {
	var vs []Violation
	add := func(rule, format string, args ...any) {
		vs = append(vs, Violation{Rule: rule, Path: path, Detail: fmt.Sprintf(format, args...)})
	}
	if e.FromNodeID == e.ToNodeID {
		add(RuleSelfReference, "edge %s points from node %s to itself", e.EdgeID, e.FromNodeID)
		return vs
	}
	from, fromOK := nodes[e.FromNodeID]
	_, toOK := nodes[e.ToNodeID]
	if !fromOK {
		add(RuleMissingPredecessor, "edge %s starts at node %s, which is not in the plan", e.EdgeID, e.FromNodeID)
	}
	if !toOK {
		add(RuleMissingSuccessor, "edge %s ends at node %s, which is not in the plan", e.EdgeID, e.ToNodeID)
	}
	if !fromOK || !toOK {
		return vs
	}

	allowed := map[string]bool{}
	switch e.Kind {
	case EdgeArtifactVerified:
		allowed["pins_code_head"], allowed["target_repository"], allowed["target_base_ref"] = true, true, true
	case EdgeIntegrated:
		allowed["target_repository"], allowed["target_base_ref"] = true, true
	case EdgeDecision:
		allowed["decision_subject"], allowed["decision_digest"], allowed["required_authority"] = true, true, true
	}
	present := map[string]bool{
		"pins_code_head":     e.PinsCodeHead,
		"target_repository":  e.TargetRepository != "",
		"target_base_ref":    e.TargetBaseRef != "",
		"decision_subject":   e.DecisionSubject != "",
		"decision_digest":    e.DecisionDigest != "",
		"required_authority": len(e.RequiredAuthority) > 0,
	}
	var missing []string
	need := func(fields ...string) {
		for _, f := range fields {
			if !present[f] {
				missing = append(missing, f)
			}
		}
	}
	switch e.Kind {
	case EdgeArtifactVerified:
		if from.Kind == NodeImplementation {
			// A code artifact: the edge pins the verified head of the PR and names where it lives.
			need("pins_code_head", "target_repository", "target_base_ref")
			if len(missing) > 0 {
				add(RuleEdgeFieldMissing, "artifact_verified edge %s leaves implementation node %s, so it consumes a code artifact and must set %s", e.EdgeID, e.FromNodeID, strings.Join(missing, ", "))
			}
		} else {
			// A non-code artifact has no head to pin and no repository to name.
			for _, f := range []string{"pins_code_head", "target_repository", "target_base_ref"} {
				if present[f] {
					missing = append(missing, f)
				}
			}
			if len(missing) > 0 {
				add(RuleEdgeFieldNotApplicable, "artifact_verified edge %s leaves non_pr node %s, whose artifact is not code, so it cannot set %s", e.EdgeID, e.FromNodeID, strings.Join(missing, ", "))
			}
		}
		allowed = map[string]bool{} // the fields above were judged by the source kind
		for _, f := range []string{"pins_code_head", "target_repository", "target_base_ref"} {
			allowed[f] = true
		}
	case EdgeIntegrated:
		if from.Kind != NodeImplementation {
			add(RuleOutputIntegratedNeedsImpl, "integrated edge %s leaves %s node %s; only an implementation node (one PR) can be integrated", e.EdgeID, from.Kind, e.FromNodeID)
		}
		need("target_repository", "target_base_ref")
		if len(missing) > 0 {
			add(RuleEdgeFieldMissing, "integrated edge %s must set %s", e.EdgeID, strings.Join(missing, ", "))
		}
	case EdgeDecision:
		if from.Kind == NodeImplementation {
			add(RuleOutputDecisionNeedsNonPR, "decision edge %s leaves implementation node %s; a decision follows a non_pr node", e.EdgeID, e.FromNodeID)
		}
		need("decision_subject", "decision_digest", "required_authority")
		if len(missing) > 0 {
			add(RuleEdgeFieldMissing, "decision edge %s must set %s (required_authority must name at least one authority)", e.EdgeID, strings.Join(missing, ", "))
		}
	}
	var stray []string
	for _, f := range []string{"pins_code_head", "target_repository", "target_base_ref", "decision_subject", "decision_digest", "required_authority"} {
		if present[f] && !allowed[f] {
			stray = append(stray, f)
		}
	}
	if len(stray) > 0 {
		add(RuleEdgeFieldNotApplicable, "%s edge %s cannot set %s", e.Kind, e.EdgeID, strings.Join(stray, ", "))
	}
	return vs
}

// findCycle reports one cycle, deterministically: Kahn's algorithm leaves the nodes on cycles and
// those downstream of them, every one with a predecessor left, so walking predecessors from the
// smallest id must repeat a node.
func findCycle(nodes map[string]liveNode, edges map[string]Edge) []Violation {
	indegree := map[string]int{}
	out := map[string][]string{}
	in := map[string][]string{}
	for id := range nodes {
		indegree[id] = 0
	}
	eids := make([]string, 0, len(edges))
	for id := range edges {
		eids = append(eids, id)
	}
	sort.Strings(eids)
	for _, id := range eids {
		e := edges[id]
		if e.FromNodeID == e.ToNodeID {
			continue
		}
		if _, ok := nodes[e.FromNodeID]; !ok {
			continue
		}
		if _, ok := nodes[e.ToNodeID]; !ok {
			continue
		}
		out[e.FromNodeID] = append(out[e.FromNodeID], e.ToNodeID)
		in[e.ToNodeID] = append(in[e.ToNodeID], e.FromNodeID)
		indegree[e.ToNodeID]++
	}
	var queue []string
	for id, n := range indegree {
		if n == 0 {
			queue = append(queue, id)
		}
	}
	removed := map[string]bool{}
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		removed[id] = true
		for _, to := range out[id] {
			indegree[to]--
			if indegree[to] == 0 {
				queue = append(queue, to)
			}
		}
	}
	var rest []string
	for id := range nodes {
		if !removed[id] {
			rest = append(rest, id)
		}
	}
	if len(rest) == 0 {
		return nil
	}
	sort.Strings(rest)
	left := map[string]bool{}
	for _, id := range rest {
		left[id] = true
	}
	at := map[string]int{}
	var path []string
	cur := rest[0]
	for {
		if k, seen := at[cur]; seen {
			cycle := append([]string(nil), path[k:]...)
			// the walk went against the edges: reverse it to read along them, and start at the smallest id
			for i, j := 0, len(cycle)-1; i < j; i, j = i+1, j-1 {
				cycle[i], cycle[j] = cycle[j], cycle[i]
			}
			low := 0
			for i, id := range cycle {
				if id < cycle[low] {
					low = i
				}
			}
			cycle = append(cycle[low:], cycle[:low]...)
			cycle = append(cycle, cycle[0])
			return []Violation{{Rule: RuleCycle, Path: "plan", Detail: "nodes " + strings.Join(cycle, " -> ") + " form a cycle; a plan is a DAG"}}
		}
		at[cur] = len(path)
		path = append(path, cur)
		preds := append([]string(nil), in[cur]...)
		sort.Strings(preds)
		next := ""
		for _, p := range preds {
			if left[p] {
				next = p
				break
			}
		}
		if next == "" {
			// unreachable: every node Kahn's algorithm left has a predecessor that was left too
			return []Violation{{Rule: RuleCycle, Path: "plan", Detail: "the plan holds a cycle"}}
		}
		cur = next
	}
}

// materialize builds the state one revision later and the rows that change: a node whose spec or
// incoming edges changed is retired and introduced again as a new version; a node that left the plan
// is retired; an edge that left is retired; what is new is inserted.
func materialize(prev State, live map[string]liveNode, liveEdges map[string]Edge) (State, Diff, []Violation) {
	rev := prev.Revision + 1
	next := State{PlanID: prev.PlanID, ProjectKey: prev.ProjectKey, Revision: rev}
	var diff Diff

	incoming := map[string][]Edge{}
	for _, e := range liveEdges {
		incoming[e.ToNodeID] = append(incoming[e.ToNodeID], e)
	}
	slice := map[string]string{}
	for id, n := range live {
		slice[id] = SliceDigest(n.Node, incoming[id])
	}

	next.Nodes = append([]NodeVersion(nil), prev.Nodes...)
	had := map[string]bool{}
	for i := range next.Nodes {
		row := &next.Nodes[i]
		if row.RetiredRev != 0 {
			continue
		}
		had[row.NodeID] = true
		cur, ok := live[row.NodeID]
		if ok && cur.Node == row.Node && slice[row.NodeID] == row.SliceDigest && cur.Supersedes == row.SupersedesNodeID {
			continue
		}
		row.RetiredRev = rev
		diff.RetireNodes = append(diff.RetireNodes, *row)
		if ok {
			diff.InsertNodes = append(diff.InsertNodes, NodeVersion{Node: cur.Node, SliceDigest: slice[row.NodeID], SupersedesNodeID: cur.Supersedes, IntroducedRev: rev})
		}
	}
	var fresh []string
	for id := range live {
		if !had[id] {
			fresh = append(fresh, id)
		}
	}
	sort.Strings(fresh)
	for _, id := range fresh {
		n := live[id]
		diff.InsertNodes = append(diff.InsertNodes, NodeVersion{Node: n.Node, SliceDigest: slice[id], SupersedesNodeID: n.Supersedes, IntroducedRev: rev})
	}
	sort.SliceStable(diff.InsertNodes, func(i, j int) bool { return diff.InsertNodes[i].NodeID < diff.InsertNodes[j].NodeID })
	next.Nodes = append(next.Nodes, diff.InsertNodes...)

	next.Edges = append([]EdgeRow(nil), prev.Edges...)
	hadEdge := map[string]bool{}
	for i := range next.Edges {
		row := &next.Edges[i]
		if row.RetiredRev != 0 {
			continue
		}
		hadEdge[row.EdgeID] = true
		if _, ok := liveEdges[row.EdgeID]; !ok {
			row.RetiredRev = rev
			diff.RetireEdges = append(diff.RetireEdges, *row)
		}
	}
	var freshEdges []string
	for id := range liveEdges {
		if !hadEdge[id] {
			freshEdges = append(freshEdges, id)
		}
	}
	sort.Strings(freshEdges)
	for _, id := range freshEdges {
		row := EdgeRow{Edge: liveEdges[id], IntroducedRev: rev}
		diff.InsertEdges = append(diff.InsertEdges, row)
		next.Edges = append(next.Edges, row)
	}
	return next, diff, nil
}

// At is the plan as of revision rev: the rows live then, sorted by id, with the digest of their content.
func (s State) At(rev int64) Snapshot {
	snap := Snapshot{PlanID: s.PlanID, ProjectKey: s.ProjectKey, Revision: rev, Nodes: []SnapNode{}, Edges: []SnapEdge{}}
	for _, row := range s.Nodes {
		if row.IntroducedRev <= rev && (row.RetiredRev == 0 || row.RetiredRev > rev) {
			snap.Nodes = append(snap.Nodes, SnapNode{Node: row.Node, SliceDigest: row.SliceDigest, SupersedesNodeID: row.SupersedesNodeID, IntroducedRev: row.IntroducedRev})
		}
	}
	for _, row := range s.Edges {
		if row.IntroducedRev <= rev && (row.RetiredRev == 0 || row.RetiredRev > rev) {
			snap.Edges = append(snap.Edges, SnapEdge{Edge: row.Edge, IntroducedRev: row.IntroducedRev})
		}
	}
	sort.Slice(snap.Nodes, func(i, j int) bool { return snap.Nodes[i].NodeID < snap.Nodes[j].NodeID })
	sort.Slice(snap.Edges, func(i, j int) bool { return snap.Edges[i].EdgeID < snap.Edges[j].EdgeID })
	snap.StateDigest = stateDigest(snap.PlanID, snap.ProjectKey, snap.Nodes, snap.Edges)
	return snap
}

// StateFromSnapshot is the state a snapshot stands for: its live rows and no history.
func StateFromSnapshot(s Snapshot) State {
	st := State{PlanID: s.PlanID, ProjectKey: s.ProjectKey, Revision: s.Revision}
	for _, n := range s.Nodes {
		st.Nodes = append(st.Nodes, NodeVersion{Node: n.Node, SliceDigest: n.SliceDigest, SupersedesNodeID: n.SupersedesNodeID, IntroducedRev: n.IntroducedRev})
	}
	for _, e := range s.Edges {
		st.Edges = append(st.Edges, EdgeRow{Edge: e.Edge, IntroducedRev: e.IntroducedRev})
	}
	return st
}

// Replay applies committed events, in order, to a snapshot and returns the snapshot at the last
// event. It is mechanical: the events already passed the writer's rules, so it re-applies them and
// refuses only what a log cannot contain: an event that does not follow the one before, a change that
// does not apply, a state whose digest is not the one the event recorded. Replaying from the empty
// snapshot, or from any earlier snapshot and its cursor, reaches the same plan.
func Replay(base Snapshot, events []Event) (Snapshot, error) {
	st := StateFromSnapshot(base)
	for _, ev := range events {
		if ev.ParentRevisionNo != st.Revision || ev.RevisionNo != st.Revision+1 {
			return Snapshot{}, fmt.Errorf("event %d (parent %d) does not follow revision %d: the log is missing, repeats or reorders an event", ev.RevisionNo, ev.ParentRevisionNo, st.Revision)
		}
		next, _, vs := applyChanges(st, ev.Changes, false)
		if len(vs) > 0 {
			return Snapshot{}, fmt.Errorf("event %d does not apply: %s", ev.RevisionNo, (&PlanRejected{Violations: vs}).refusal().Detail)
		}
		if got := next.At(next.Revision).StateDigest; got != ev.StateDigest {
			return Snapshot{}, fmt.Errorf("event %d produces state digest %s; the event recorded %s", ev.RevisionNo, got, ev.StateDigest)
		}
		st = next
	}
	return st.At(st.Revision), nil
}
