package manage

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"sync"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dagsched"
)

// This file is CRW-690: the detachable branch candidates of a plan whose verdict is
// expand_candidate. A candidate is a bundle of live nodes no other live node joins by an edge or
// shares an edit region with, so the bundle could be taken out into a project of its own. Nothing
// here writes a plan, Linear or the relay: it reads and reports.

// branchAlwaysMemoMu guards branchAlwaysMemo, and branchAlwaysMemo is the --branches-always flag
// each Env was run with. The flag belongs to one command invocation, not to the process: Env is
// declared in another issue's file and may not gain a field here, and a library caller that runs
// Capacity twice in one process must not inherit the first call's flag. Run builds one Env per
// invocation, so the map holds one live entry per run and is bounded in practice.
var (
	branchAlwaysMemoMu sync.Mutex
	branchAlwaysMemo   = map[*Env]bool{}
)

// branchAlwaysSet records the flag for one Env.
func branchAlwaysSet(e *Env, always bool) {
	branchAlwaysMemoMu.Lock()
	branchAlwaysMemo[e] = always
	branchAlwaysMemoMu.Unlock()
}

// branchAlwaysFor is the flag one Env was run with.
func branchAlwaysFor(e *Env) bool {
	branchAlwaysMemoMu.Lock()
	defer branchAlwaysMemoMu.Unlock()
	return branchAlwaysMemo[e]
}

const (
	branchMinNodesDefault = 2
	branchMaxDefault      = 3
)

// BranchNode is one live node of a candidate: the node, the issue it implements and whether the
// plan's pass counts it as waiting (ready, or deferred for want of capacity).
type BranchNode struct {
	NodeID   string `json:"node_id"`
	IssueKey string `json:"issue_key"`
	Ready    bool   `json:"ready"`
}

// BranchCandidate is one bundle of live nodes a new project could take over: its nodes, how many
// of them the plan waits on, the places they declare and how many edges join them.
type BranchCandidate struct {
	Nodes       []BranchNode `json:"nodes"`
	ReadyCount  int          `json:"ready_count"`
	Regions     []string     `json:"regions"`
	EdgesInside int          `json:"edges_inside"`
}

// BranchCandidates is the list one plan carries. The field is nil when the plan carries no list at
// all, which is how a hold plan reads without --branches-always.
type BranchCandidates []BranchCandidate

// branchSettings is the capacity section's branch keys; the thresholds are pointers so an omitted
// key keeps its default.
type branchSettings struct {
	MinBranchNodes *int `json:"min_branch_nodes"`
	MaxBranches    *int `json:"max_branches"`
}

// branchSettingsOf reads the branch keys of the capacity section. A key this build cannot read is
// refused rather than defaulted, so a threshold nobody can parse never reads as the default and
// silently reports the wrong bundles.
func branchSettingsOf(cfg *Config) (branchSettings, error) {
	settings := branchSettings{}
	if err := cfg.Section("capacity", &settings); err != nil {
		return branchSettings{}, fmt.Errorf("capacity: the section: %w", err)
	}
	return settings, nil
}

func branchMinNodes(settings branchSettings) int {
	if settings.MinBranchNodes == nil || *settings.MinBranchNodes < 1 {
		return branchMinNodesDefault
	}
	return *settings.MinBranchNodes
}

func branchMax(settings branchSettings) int {
	if settings.MaxBranches == nil || *settings.MaxBranches < 1 {
		return branchMaxDefault
	}
	return *settings.MaxBranches
}

// branchPlanNode and branchPlanEdge are one live node and one live edge of the plan, as the plan
// itself carries them: the rows dag-plan-show reads, with the retired ones left out.
type branchPlanNode struct{ nodeID, issueKey string }
type branchPlanEdge struct{ from, to string }

// branchReadPlan reads the plan's live nodes and edges from the store read-only, which is the
// reading dag-plan-show prints (dag_nodes and dag_edges with retired_rev IS NULL).
func branchReadPlan(ctx context.Context, handle *dagReviewStore, plan string) ([]branchPlanNode, []branchPlanEdge, error) {
	nodes, err := dagReviewRows(ctx, handle.db,
		"SELECT node_id, issue_key FROM dag_nodes WHERE plan_id = ? AND retired_rev IS NULL ORDER BY node_id",
		[]any{plan}, func(rows *sql.Rows) (branchPlanNode, error) {
			var node branchPlanNode
			err := rows.Scan(&node.nodeID, &node.issueKey)
			return node, err
		})
	if err != nil {
		return nil, nil, err
	}
	edges, err := dagReviewRows(ctx, handle.db,
		"SELECT from_node_id, to_node_id FROM dag_edges WHERE plan_id = ? AND retired_rev IS NULL ORDER BY edge_id",
		[]any{plan}, func(rows *sql.Rows) (branchPlanEdge, error) {
			var edge branchPlanEdge
			err := rows.Scan(&edge.from, &edge.to)
			return edge, err
		})
	if err != nil {
		return nil, nil, err
	}
	return nodes, edges, nil
}

// branchStoreMarks reads the two marks the rule needs from the relay store, read-only: which nodes
// were released (or executed) and which nodes have an effective integration observation.
func branchStoreMarks(ctx context.Context, handle *dagReviewStore, plan string) (map[string]bool, map[string]bool, error) {
	column := func(query string) (map[string]bool, error) {
		rows, err := dagReviewRows(ctx, handle.db, query, []any{plan}, func(rows *sql.Rows) (string, error) {
			var id string
			err := rows.Scan(&id)
			return id, err
		})
		if err != nil {
			return nil, err
		}
		out := map[string]bool{}
		for _, id := range rows {
			out[id] = true
		}
		return out, nil
	}
	released, err := column("SELECT node_id FROM dag_releases WHERE plan_id = ?")
	if err != nil {
		return nil, nil, err
	}
	executed, err := column("SELECT node_id FROM dag_node_executions WHERE plan_id = ?")
	if err != nil {
		return nil, nil, err
	}
	for id := range executed {
		released[id] = true
	}
	// The integration reading mirrors the review's (dagReviewIntegratedAt): an observation of an
	// active acceptance that is an ancestor and was not reverted is an integration.
	integrated, err := column("SELECT DISTINCT a.node_id FROM dag_acceptances a" +
		" JOIN dag_integration_observations o ON o.acceptance_id = a.acceptance_id" +
		" WHERE a.plan_id = ? AND a.state = 'active' AND o.is_ancestor = 1" +
		" AND (o.reverted_by IS NULL OR o.reverted_by = '')")
	if err != nil {
		return nil, nil, err
	}
	return released, integrated, nil
}

// branchAttach computes the candidates of one plan: nil when the plan carries none at all (a hold
// plan without --branches-always), otherwise a list, empty when nothing can be detached.
func branchAttach(ctx context.Context, e *Env, cfg *Config, stateDir, plan, verdict string, waiting []string) (*BranchCandidates, error) {
	if verdict == capacityHold && !branchAlwaysFor(e) {
		return nil, nil
	}
	settings, err := branchSettingsOf(cfg)
	if err != nil {
		return nil, err
	}
	// The store is read through the review's own read-only handle: one no-sidecar open, and the
	// region reading the scheduler makes (the latest declaration per node, with its hold).
	handle, err := dagReviewOpenStore(ctx, stateDir)
	if err != nil {
		return nil, err
	}
	defer handle.Close()
	// A store that predates the DAG zone holds no plan to read, so nothing here can be proven and
	// the plan carries an empty list rather than a candidate nobody measured.
	for _, table := range []string{"dag_node_regions", "dag_releases", "dag_acceptances"} {
		present, err := handle.hasTable(ctx, table)
		if err != nil {
			return nil, err
		}
		if !present {
			empty := BranchCandidates{}
			return &empty, nil
		}
	}
	nodes, edges, err := branchReadPlan(ctx, handle, plan)
	if err != nil {
		return nil, err
	}
	var checks []Check
	declared, err := handle.dagReviewRegions(ctx, plan, &checks)
	if err != nil {
		return nil, err
	}
	released, integrated, err := branchStoreMarks(ctx, handle, plan)
	if err != nil {
		return nil, err
	}

	// A live node is one that was not retired (the plan answer holds only those) and has no
	// integration observation.
	live, issue := []string{}, map[string]string{}
	for _, node := range nodes {
		if integrated[node.nodeID] {
			continue
		}
		live = append(live, node.nodeID)
		issue[node.nodeID] = node.issueKey
	}
	regions, declaredNodes := map[string][]dagsched.Region{}, map[string]bool{}
	for _, region := range declared {
		if integrated[region.nodeID] {
			continue
		}
		declaredNodes[region.nodeID] = true
		regions[region.nodeID] = append(regions[region.nodeID], dagsched.Region{
			Repository: region.repository, Path: region.path, Kind: region.kind,
			Key: region.key, Change: region.change, Exclusive: region.exclusive,
		})
	}

	index := map[string]int{}
	for i, id := range live {
		index[id] = i
	}
	union := branchUnion{parent: make([]int, len(live))}
	for i := range union.parent {
		union.parent[i] = i
	}
	for _, edge := range edges {
		from, fromLive := index[edge.from]
		to, toLive := index[edge.to]
		if fromLive && toLive {
			union.join(from, to)
		}
	}
	for i := 0; i < len(live); i++ {
		for j := i + 1; j < len(live); j++ {
			if branchConnected(regions[live[i]], declaredNodes[live[i]], regions[live[j]], declaredNodes[live[j]]) {
				union.join(i, j)
			}
		}
	}
	groups := map[int][]string{}
	for i, id := range live {
		root := union.root(i)
		groups[root] = append(groups[root], id)
	}

	waitingSet := map[string]bool{}
	for _, key := range waiting {
		waitingSet[key] = true
	}
	candidates := BranchCandidates{}
	for _, members := range groups {
		sort.Strings(members)
		// A bundle a released node is in cannot be taken out, one below the floor is not a bundle,
		// and one that is the whole plan leaves nothing behind.
		started := false
		for _, id := range members {
			if released[id] {
				started = true
			}
		}
		if started || len(members) < branchMinNodes(settings) || len(members) >= len(live) {
			continue
		}
		inside := map[string]bool{}
		for _, id := range members {
			inside[id] = true
		}
		candidate := BranchCandidate{Nodes: []BranchNode{}, Regions: []string{}}
		paths := map[string]bool{}
		for _, id := range members {
			ready := waitingSet[issue[id]]
			candidate.Nodes = append(candidate.Nodes, BranchNode{NodeID: id, IssueKey: issue[id], Ready: ready})
			if ready {
				candidate.ReadyCount++
			}
			for _, region := range regions[id] {
				paths[region.Path] = true
			}
		}
		for path := range paths {
			candidate.Regions = append(candidate.Regions, path)
		}
		sort.Strings(candidate.Regions)
		for _, edge := range edges {
			if inside[edge.from] && inside[edge.to] {
				candidate.EdgesInside++
			}
		}
		candidates = append(candidates, candidate)
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		a, b := candidates[i], candidates[j]
		switch {
		case a.ReadyCount != b.ReadyCount:
			return a.ReadyCount > b.ReadyCount
		case len(a.Nodes) != len(b.Nodes):
			return len(a.Nodes) > len(b.Nodes)
		}
		return a.Nodes[0].NodeID < b.Nodes[0].NodeID
	})
	if max := branchMax(settings); len(candidates) > max {
		candidates = candidates[:max]
	}
	return &candidates, nil
}

// branchConnected is whether two live nodes are joined: a node that declared no region is joined to
// every live node (the scheduler's own rule), and otherwise the regions of the two nodes are judged
// with dagsched.Overlaps, every grade included.
func branchConnected(a []dagsched.Region, aDeclared bool, b []dagsched.Region, bDeclared bool) bool {
	if !aDeclared || !bDeclared {
		return true
	}
	for _, left := range a {
		for _, right := range b {
			if dagsched.Overlaps(left, right) {
				return true
			}
		}
	}
	return false
}

// branchUnion is a disjoint set over the live nodes, so the connected components are one pass of
// unions. A plan holds at most 64 nodes, so the root walk needs no ranking.
type branchUnion struct{ parent []int }

func (u *branchUnion) root(i int) int {
	for u.parent[i] != i {
		u.parent[i] = u.parent[u.parent[i]]
		i = u.parent[i]
	}
	return i
}

func (u *branchUnion) join(i, j int) {
	ri, rj := u.root(i), u.root(j)
	if ri != rj {
		u.parent[rj] = ri
	}
}
