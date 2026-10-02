package dagsched

import (
	"context"
	"database/sql"
	"sort"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dag"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// StateWaiting and StateReady are the derived states of a node nobody owns yet (contract 3.1): its edges are not all satisfied (or its inputs fail
// their checks), or it is a candidate for release.
const (
	StateWaiting = "waiting"
	StateReady   = "ready"
)

// ReadyOptions tune one reading. SkipArtifactBytes leaves the declared artifact files unread, which is what the release path wants under its lock
// (the files were hashed before it; the store half is what can change between).
type ReadyOptions struct{ SkipArtifactBytes bool }

// Read is Ready inside one read transaction of the store: every query of the reading shares that one connection and so sees one state of the store.
// The store has one connection, so a reading that opened a second would wait for itself forever; Read never does.
func (s *Scheduler) Read(ctx context.Context, plan string, opts ReadyOptions) (Reading, error) {
	var out Reading
	err := s.Store.Transaction(ctx, func(txCtx context.Context, _ *sql.Conn) error {
		var err error
		out, err = s.Ready(txCtx, s.Store.Q(txCtx), plan, opts)
		return err
	})
	return out, err
}

// candidate is a node whose edges are satisfied, whose inputs verified and that nobody else owns: it competes for a slot.
type candidate struct {
	node  dag.SnapNode
	rank  Rank
	index int // position in the node list
}

// Ready computes the ready set of a plan from the stored plan and the relay's execution records (contract 7). It writes nothing, reads no clock and
// opens no connection of its own: two calls over one store state return equal readings. Every live node is either ready or carries one closed reason.
func (s *Scheduler) Ready(ctx context.Context, q store.Querier, plan string, opts ReadyOptions) (Reading, error) {
	snap, _, err := dag.SnapshotAt(ctx, q, plan, 0)
	if err != nil {
		return Reading{}, err
	}
	// one memo of the invalidation judgement for this reading (invalidation.go): it lives in a context derived here and ends with the call, so no verdict outlives the state it was made for
	ctx, _ = memoFor(ctx, plan, snap)
	nodes := append([]dag.SnapNode(nil), snap.Nodes...)
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].NodeID < nodes[j].NodeID })
	incoming := map[string][]dag.SnapEdge{}
	outgoing := map[string][]string{}
	for _, e := range snap.Edges {
		incoming[e.ToNodeID] = append(incoming[e.ToNodeID], e)
		outgoing[e.FromNodeID] = append(outgoing[e.FromNodeID], e.ToNodeID)
	}
	for _, list := range incoming {
		sort.Slice(list, func(i, j int) bool { return list[i].EdgeID < list[j].EdgeID })
	}
	critical, descendants := graphMetrics(nodes, outgoing)
	capacity, err := s.capacity(ctx, q, snap.ProjectKey)
	if err != nil {
		return Reading{}, err
	}
	declarations, err := loadDeclarations(ctx, q, plan)
	if err != nil {
		return Reading{}, err
	}

	readings := make(map[string]*NodeReading, len(nodes))
	var holders []holder
	var candidates []candidate
	var hashes []string
	for i, n := range nodes {
		state, err := s.stateOf(ctx, q, plan, snap, n)
		if err != nil {
			return Reading{}, err
		}
		reading := &NodeReading{NodeID: n.NodeID, IssueKey: n.IssueKey, Kind: n.Kind, State: state.State}
		readings[n.NodeID] = reading
		if state.Owned {
			reading.Disposition, reading.Reason, reading.Detail = state.Disp, state.Reason, state.Detail
			if state.State == StateStale {
				if reading.Stale, err = s.staleOf(ctx, q, plan, snap, n); err != nil {
					return Reading{}, err
				}
				// what is done about it (revalidation.go)
				if reading.Stale != nil {
					if reading.Stale, err = s.withRoute(ctx, q, plan, snap, n, reading.Stale); err != nil {
						return Reading{}, err
					}
				}
			}
			if state.Holds && n.Kind == dag.NodeImplementation {
				regions, declared := declarations[n.NodeID]
				holders = append(holders, holder{NodeID: n.NodeID, Regions: regions, Unknown: !declared})
			}
			continue
		}
		reading.State = StateWaiting
		statuses := map[string]EdgeStatus{}
		since := ""
		var first *EdgeStatus
		for _, e := range incoming[n.NodeID] {
			status, err := s.edgeStatus(ctx, q, plan, snap, e)
			if err != nil {
				return Reading{}, err
			}
			statuses[e.EdgeID] = status
			if !status.Satisfied {
				first = &status
				break
			}
			if status.Since > since {
				since = status.Since
			}
		}
		if first != nil {
			reading.Disposition, reading.Reason, reading.Detail = dispositionOf(first.Reason), first.Reason, first.Detail
			continue
		}
		verdict, err := s.checkInputs(ctx, q, plan, incoming[n.NodeID], statuses, opts.SkipArtifactBytes)
		if err != nil {
			return Reading{}, err
		}
		hashes = append(hashes, verdict.Hashes...)
		if verdict.Finding != nil {
			reading.Disposition, reading.Reason, reading.Detail = DispBlocked, verdict.Finding.Reason, verdict.Finding.Code+": "+verdict.Finding.Detail
			continue
		}
		reason, detail, err := s.resourceHold(ctx, q, plan, snap, n, incoming[n.NodeID])
		if err != nil {
			return Reading{}, err
		}
		reading.State = StateReady
		if reason != "" {
			reading.Disposition, reading.Reason, reading.Detail = dispositionOf(reason), reason, detail
			continue
		}
		if len(incoming[n.NodeID]) == 0 {
			if since, err = introducedAt(ctx, q, plan, n.IntroducedRev); err != nil {
				return Reading{}, err
			}
		}
		candidates = append(candidates, candidate{node: n, index: i, rank: Rank{CriticalPath: critical[n.NodeID], Descendants: descendants[n.NodeID], ReadySince: since}})
	}

	sort.SliceStable(candidates, func(i, j int) bool {
		a, b := candidates[i], candidates[j]
		switch {
		case a.rank.CriticalPath != b.rank.CriticalPath:
			return a.rank.CriticalPath > b.rank.CriticalPath
		case a.rank.Descendants != b.rank.Descendants:
			return a.rank.Descendants > b.rank.Descendants
		case a.rank.ReadySince != b.rank.ReadySince:
			return a.rank.ReadySince < b.rank.ReadySince
		}
		return a.node.NodeID < b.node.NodeID
	})

	pass := PassSummary{FreeSlots: capacity.Free, Ceiling: capacity.Ceiling, Held: capacity.Held, CeilingSource: capacity.Source, DecidingLimit: LimitNone}
	var ready []NodeReading
	selected := 0
	for _, c := range candidates {
		reading := readings[c.node.NodeID]
		rank := c.rank
		reading.Rank = &rank
		limit := ""
		if c.node.Kind == dag.NodeImplementation {
			regions, declared := declarations[c.node.NodeID]
			if overlapsAny(holder{NodeID: c.node.NodeID, Regions: regions, Unknown: !declared}, holders) {
				limit = LimitEditOverlap
				reading.Disposition, reading.Reason = DispDefer, DeferEditOverlap
				reading.Detail = "its edit regions overlap a node that is running or accepted and not yet landed, or a region is undeclared"
			}
		}
		if limit == "" && selected >= capacity.Free {
			limit, reading.Disposition = LimitNoCapacity, DispDefer
			reading.Reason, reading.Detail = DeferNoCapacity, "no slot is free in the scope that binds: "+itoa64(int64(capacity.Held))+" held of a ceiling of "+itoa64(int64(capacity.Ceiling))
			if capacity.Unmeasured {
				limit, reading.Reason = LimitCapacityUnmeasured, DeferCapacityUnmeasured
				reading.Detail = "an enforced ceiling on a dimension nobody measured leaves no basis to say a slot is free"
			}
		}
		if limit != "" {
			if pass.DecidingLimit == LimitNone {
				pass.DecidingLimit = limit
			}
			continue
		}
		reading.Disposition, reading.Reason, reading.Detail = DispReady, "", ""
		selected++
		if c.node.Kind == dag.NodeImplementation {
			regions, declared := declarations[c.node.NodeID]
			holders = append(holders, holder{NodeID: c.node.NodeID, Regions: regions, Unknown: !declared})
		}
		ready = append(ready, *reading)
	}
	pass.ReadyCount = selected

	out := Reading{PlanID: plan, PlanRevision: snap.Revision, StateDigest: snap.StateDigest, Pass: pass, Ready: ready}
	for _, n := range nodes {
		out.Nodes = append(out.Nodes, *readings[n.NodeID])
	}
	for i := range out.Ready {
		out.Ready[i] = *readings[out.Ready[i].NodeID]
	}
	out.InputDigest = inputDigest(out, capacity, hashes)
	return out, nil
}

// introducedAt is the stored time of the revision that introduced a node: the moment a node with no incoming edge became ready.
func introducedAt(ctx context.Context, q store.Querier, plan string, revision int64) (string, error) {
	var at string
	_, err := queryOne(ctx, q, "SELECT recorded_at FROM dag_plan_revisions WHERE plan_id = ? AND revision_no = ?", []any{plan, revision}, &at)
	return at, err
}

// resourceHold applies the checks a candidate meets outside the plan: nobody else owns the issue (an open relationship, or a managed start still
// pending: an attached start is the relationship's, and a finished relationship frees the issue), the project has exactly one registered parent to
// reserve under, and no merge is in flight on a branch the node builds on. A non-empty reason is a closed reason.
func (s *Scheduler) resourceHold(ctx context.Context, q store.Querier, plan string, snap dag.Snapshot, n dag.SnapNode, edges []dag.SnapEdge) (string, string, error) {
	var open int
	if _, err := queryOne(ctx, q, "SELECT COUNT(*) FROM relationships WHERE issue_key = ? AND status IN ('active','paused') AND superseded_by IS NULL", []any{n.IssueKey}, &open); err != nil {
		return "", "", err
	}
	if open > 0 {
		return SkipAlreadyOwned, n.IssueKey + " already has an open relationship that is not an execution of this node", nil
	}
	var pending int
	if _, err := queryOne(ctx, q, "SELECT COUNT(*) FROM managed_start_requests WHERE issue_key = ? AND state IN ('reserved','create_armed')", []any{n.IssueKey}, &pending); err != nil {
		return "", "", err
	}
	if pending > 0 {
		return SkipAlreadyOwned, n.IssueKey + " has a managed start in flight that no release of this node made", nil
	}
	parents, err := projectParents(ctx, q, snap.ProjectKey)
	if err != nil {
		return "", "", err
	}
	if len(parents) != 1 {
		return DeferOwnershipUnverified, "project " + snap.ProjectKey + " has " + itoa64(int64(len(parents))) + " registered parents; a release needs exactly one to reserve under", nil
	}
	for _, e := range edges {
		if e.TargetRepository == "" || e.TargetBaseRef == "" {
			continue
		}
		var one int
		moving, err := queryOne(ctx, q, "SELECT 1 FROM merge_turns WHERE repository = ? AND base_ref = ? AND state IN ('merging','unknown') LIMIT 1", []any{e.TargetRepository, e.TargetBaseRef}, &one)
		if err != nil {
			return "", "", err
		}
		if moving {
			return DeferMergeWindow, "a merge turn is moving " + e.TargetRepository + " " + e.TargetBaseRef + "; the tip the node would build on is not settled", nil
		}
	}
	return "", "", nil
}

// graphMetrics are, for every node, the number of nodes on the longest chain that starts at it (hop-count critical path) and the number of nodes
// reachable below it. The plan is acyclic (CRW-183 refuses a cycle); a cycle would be cut rather than looped on.
func graphMetrics(nodes []dag.SnapNode, outgoing map[string][]string) (critical, descendants map[string]int) {
	critical, descendants = map[string]int{}, map[string]int{}
	visiting := map[string]bool{}
	var depth func(id string) int
	depth = func(id string) int {
		if v, ok := critical[id]; ok {
			return v
		}
		if visiting[id] {
			return 0
		}
		visiting[id] = true
		best := 0
		for _, next := range outgoing[id] {
			best = max(best, depth(next))
		}
		visiting[id] = false
		critical[id] = best + 1
		return best + 1
	}
	for _, n := range nodes {
		depth(n.NodeID)
		reach := map[string]bool{}
		var walk func(id string)
		walk = func(id string) {
			for _, next := range outgoing[id] {
				if !reach[next] {
					reach[next] = true
					walk(next)
				}
			}
		}
		walk(n.NodeID)
		descendants[n.NodeID] = len(reach)
	}
	return critical, descendants
}

// inputDigest is the digest of everything a reading depended on: the plan state, each node's state and reason, the capacity figures, the order and the
// hashes of the files read. Two readings with one digest saw one world.
func inputDigest(r Reading, c Capacity, hashes []string) string {
	nodes := make([]any, len(r.Nodes))
	for i, n := range r.Nodes {
		nodes[i] = map[string]any{"node_id": n.NodeID, "state": n.State, "disposition": n.Disposition, "reason": n.Reason, "detail": n.Detail}
	}
	order := make([]any, len(r.Ready))
	for i, n := range r.Ready {
		order[i] = n.NodeID
	}
	files := make([]any, len(hashes))
	for i, h := range hashes {
		files[i] = h
	}
	sort.Slice(files, func(i, j int) bool { return files[i].(string) < files[j].(string) })
	return digestOf(map[string]any{
		"plan_id": r.PlanID, "plan_revision": r.PlanRevision, "state_digest": r.StateDigest, "nodes": nodes, "order": order, "files": files,
		"free": c.Free, "ceiling": c.Ceiling, "held": c.Held, "source": c.Source, "unmeasured": c.Unmeasured, "deciding_limit": r.Pass.DecidingLimit,
	})
}
