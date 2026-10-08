package manage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"sync"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/acceptance"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dag"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dagsched"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// This file is CRW-690: the detachable branch candidates of a plan whose verdict is
// expand_candidate. A candidate is a bundle of live nodes no other live node joins by an edge or
// shares an edit region with, so the bundle could be taken out into a project of its own. Nothing
// here writes a plan, Linear or the relay: it reads and reports.
//
// CRW-865 makes that reading the relay's own: the whole reading (the plan, the declared regions, the
// release and execution marks and the integration judgement) runs inside one ReadSnapshot of one
// read-only handle, the plan comes from dag.SnapshotAt, and a node counts as landed only where
// dagsched's own integration judgement says so. A store that predates the DAG zone is then reported
// as unmeasured rather than as a plan nothing can be detached from.

// branchAlwaysMemoMu guards branchAlwaysMemo, and branchAlwaysMemo is the --branches-always flag
// each Env is currently run with. The flag belongs to one command invocation, not to the process:
// Env is declared in another issue's file and may not gain a field here, and a library caller that
// runs Capacity twice in one process must not inherit the first call's flag. capacityRun forgets
// its entry on every exit, so a process that runs the command many times retains nothing.
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

// branchAlwaysForget drops one Env's flag, so the map holds only the invocation in flight and no
// Env, with the streams and closures it carries, is kept reachable past its run.
func branchAlwaysForget(e *Env) {
	branchAlwaysMemoMu.Lock()
	delete(branchAlwaysMemo, e)
	branchAlwaysMemoMu.Unlock()
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

// branchThresholds reads the two thresholds: an omitted key takes its default, an explicit zero is
// honored (a floor of zero admits a one-node bundle, a cap of zero reports none), and a negative
// count is refused rather than clamped or defaulted. The sibling keys of this section keep an
// explicit value too (capacityPick), and a count below zero is a mistake the report must not hide.
func branchThresholds(settings branchSettings) (int, int, error) {
	minNodes, maxBranches := branchMinNodesDefault, branchMaxDefault
	if settings.MinBranchNodes != nil {
		if *settings.MinBranchNodes < 0 {
			return 0, 0, fmt.Errorf("capacity: min_branch_nodes is a count of nodes, not %d", *settings.MinBranchNodes)
		}
		minNodes = *settings.MinBranchNodes
	}
	if settings.MaxBranches != nil {
		if *settings.MaxBranches < 0 {
			return 0, 0, fmt.Errorf("capacity: max_branches is a count of candidates, not %d", *settings.MaxBranches)
		}
		maxBranches = *settings.MaxBranches
	}
	return minNodes, maxBranches, nil
}

// branchZoneTables are the DAG zone tables one branch reading needs. The zone is additive, so a
// store written before it (or one an interrupted install left partial) holds no plan this reading
// could measure: the plan then reports why its branches were not measured instead of the empty list
// of a plan nothing can be detached from.
var branchZoneTables = []string{"dag_plans", "dag_nodes", "dag_edges", "dag_plan_revisions", "dag_node_regions", "dag_releases", "dag_acceptances"}

// branchReadSeam runs once inside one plan's branch reading, right after the plan has been read and
// before the readings beside it. It is nil in production; a test replaces it to commit a revision
// while the reading is in flight, which is how the reading's one snapshot is pinned.
var branchReadSeam func()

// branchPassSeam runs once at the start of one plan's branch reading, after the plan's ready pass has
// been read and before the reading takes its snapshot. It is nil in production; a test replaces it to
// commit a revision between the pass and the snapshot, which is how the readiness re-read is pinned:
// the reading must report the readiness of the revision its snapshot holds.
var branchPassSeam func()

// branchPlanNode and branchPlanEdge are one live node and one live edge of the plan, as the relay's
// own reading of the plan carries them. A node the plan cancelled or archived is not live; a paused
// one is.
type branchPlanNode struct{ nodeID, issueKey string }
type branchPlanEdge struct{ from, to string }

// branchReadPlan reads the plan through the relay canon: the live nodes and the live edges as of one
// revision, from dag.SnapshotAt over the reading's own snapshot querier, at the head the snapshot
// holds. The canon recomputes every node's slice digest and the plan's state digest from the rows it
// read, so a plan that does not agree with itself is refused rather than reported as a bundle.
func branchReadPlan(ctx context.Context, q store.Querier, plan string) ([]branchPlanNode, []branchPlanEdge, int64, error) {
	snap, _, err := dag.SnapshotAt(ctx, q, plan, 0)
	if err != nil {
		return nil, nil, 0, err
	}
	nodes := make([]branchPlanNode, 0, len(snap.Nodes))
	for _, node := range snap.Nodes {
		if node.Lifecycle == dag.LifeCancelled || node.Lifecycle == dag.LifeArchived {
			continue
		}
		nodes = append(nodes, branchPlanNode{nodeID: node.NodeID, issueKey: node.IssueKey})
	}
	edges := make([]branchPlanEdge, 0, len(snap.Edges))
	for _, edge := range snap.Edges {
		edges = append(edges, branchPlanEdge{from: edge.FromNodeID, to: edge.ToNodeID})
	}
	return nodes, edges, snap.Revision, nil
}

// branchReadReleased reads the nodes the relay released or executed. A bundle that holds one of them
// cannot be taken out, so it is not a candidate.
func branchReadReleased(ctx context.Context, q store.Querier, plan string) (map[string]bool, error) {
	out := map[string]bool{}
	for _, query := range []string{
		"SELECT node_id FROM dag_releases WHERE plan_id = ?",
		"SELECT node_id FROM dag_node_executions WHERE plan_id = ?",
	} {
		rows, err := dagReviewRows(ctx, q, query, []any{plan}, func(rows *sql.Rows) (string, error) {
			var id string
			err := rows.Scan(&id)
			return id, err
		})
		if err != nil {
			return nil, err
		}
		for _, id := range rows {
			out[id] = true
		}
	}
	return out, nil
}

// branchActiveAcceptance is one node's active acceptance: the row whose stand the integration
// judgement is asked about, and the row a recorded base refresh is keyed by.
type branchActiveAcceptance struct {
	acceptanceID, relationshipID, eventID, revisionHash, headSHA string
	generation                                                   int64
}

// branchAcceptanceOf reads one node's active acceptance; found is false when it has none.
func branchAcceptanceOf(ctx context.Context, q store.Querier, plan, node string) (branchActiveAcceptance, bool, error) {
	var row branchActiveAcceptance
	err := q.QueryRowContext(ctx, "SELECT acceptance_id, relationship_id, execution_generation, event_id, revision_hash, COALESCE(head_sha, '')"+
		" FROM dag_acceptances WHERE plan_id = ? AND node_id = ? AND state = 'active'", plan, node).
		Scan(&row.acceptanceID, &row.relationshipID, &row.generation, &row.eventID, &row.revisionHash, &row.headSHA)
	if errors.Is(err, sql.ErrNoRows) {
		return branchActiveAcceptance{}, false, nil
	}
	if err != nil {
		return branchActiveAcceptance{}, false, err
	}
	return row, true, nil
}

// branchIntegratedNodes is the set of the plan's nodes the relay's own integration judgement counts
// as integrated, so the connectivity graph drops exactly the nodes the scheduler calls landed and
// keeps every node it does not. The judgement is dagsched's ExecutionIntegrated, so a node the
// parent never marked merged, whose accepted head was not observed in every target it has to land
// on, or whose observed head is not the head it stands on stays live. What an acceptance stands on
// is resolved with acceptance.StandOf over the same snapshot's querier, so an acceptance a recorded
// base refresh moved to a later generation is judged at the stand it holds now rather than at the
// generation it was accepted in.
func branchIntegratedNodes(ctx context.Context, st *store.Store, plan string, nodes []branchPlanNode) (map[string]bool, error) {
	scheduler := &dagsched.Scheduler{Store: st}
	q := st.Q(ctx)
	out := map[string]bool{}
	for _, node := range nodes {
		row, found, err := branchAcceptanceOf(ctx, q, plan, node.nodeID)
		if err != nil {
			return nil, err
		}
		if !found {
			continue
		}
		stand, err := acceptance.StandOf(ctx, q, row.acceptanceID, row.relationshipID,
			row.generation, row.eventID, row.revisionHash, row.headSHA)
		if err != nil {
			return nil, err
		}
		applicable, integrated, err := scheduler.ExecutionIntegrated(ctx, stand.RelationshipID, stand.EventID, stand.Generation, stand.RevisionHash)
		if err != nil {
			return nil, err
		}
		if applicable && integrated {
			out[node.nodeID] = true
		}
	}
	return out, nil
}

// branchAttach computes the candidates of one plan: nil when the plan carries none at all (a hold
// plan without --branches-always), otherwise a list, empty when nothing can be detached. Readiness is
// the relay's own dag-ready reading (dagsched Scheduler.Ready, which is what dag-ready runs) taken on
// this reading's snapshot querier, judged by node id because a plan may hold two nodes with one issue
// key (a redefinition) and their states must not mix. The second result is why the reading is
// unmeasured: a store that predates the DAG zone holds no plan to read, so its candidates are unknown
// rather than none.
func branchAttach(ctx context.Context, e *Env, cfg *Config, stateDir, plan, verdict, zoneReason string) (*BranchCandidates, string, error) {
	// The thresholds are read and validated before the hold shortcut: a malformed section is a
	// refusal whether or not this plan happens to carry candidates, so a configuration mistake never
	// hides behind a transient verdict.
	settings, err := branchSettingsOf(cfg)
	if err != nil {
		return nil, "", err
	}
	minNodes, maxBranches, err := branchThresholds(settings)
	if err != nil {
		return nil, "", err
	}
	// A missing DAG zone is an unmeasured reading whatever the verdict: the plan carries the null
	// list beside the reason rather than the absent key of a reading nobody asked for, so a hold that
	// happens to fall out of an unreadable store still says why nothing could be measured.
	if zoneReason != "" {
		var unknown BranchCandidates
		return &unknown, zoneReason, nil
	}
	if verdict == capacityHold && !branchAlwaysFor(e) {
		return nil, "", nil
	}
	if branchPassSeam != nil {
		branchPassSeam()
	}
	// The whole reading is one snapshot of the review's own read-only handle: the plan (through the
	// canon's own dag.SnapshotAt), the declared regions, the release and execution marks and the
	// integration judgement all run on that snapshot's querier, so a revision that commits while the
	// reading is in flight cannot mix two revisions into one bundle.
	handle, err := dagReviewOpenStore(ctx, stateDir)
	if err != nil {
		return nil, "", err
	}
	defer handle.Close()
	var candidates *BranchCandidates
	unmeasured := ""
	err = handle.dagReviewSnapshot(ctx, stateDir, func(ctx context.Context, st *store.Store) error {
		q := st.Q(ctx)
		nodes, edges, _, err := branchReadPlan(ctx, q, plan)
		if err != nil {
			return err
		}
		// Readiness comes from this snapshot, never from the earlier dag-ready pass, so the whole
		// reading is the answer of one revision. The pass is a transaction of its own, and a plan
		// revision does not move when an integration observation or a merged mark changes a
		// downstream node's readiness: judging from the pass then would drop a predecessor from this
		// graph while the successor's Ready and ReadyCount still came from a graph that held it. The
		// re-read carries the same host memory bound dag-ready judges with, so a node the pass would
		// defer for host memory is not read as waiting here. The pass still supplies the waiting
		// issue_key list the report prints, so the reported shape does not change.
		scheduler := &dagsched.Scheduler{Store: st}
		if bound, err := dagsched.HostMemoryFromEnvironment(e.Getenv); err == nil {
			scheduler.Host = bound
		}
		reading, err := scheduler.Ready(ctx, q, plan, dagsched.ReadyOptions{})
		if err != nil {
			return err
		}
		waitingNodes := capacityWaitingNodeIDs(reading)
		if branchReadSeam != nil {
			branchReadSeam()
		}
		var checks []Check
		declared, err := handle.dagReviewRegions(ctx, plan, &checks)
		if err != nil {
			return err
		}
		released, err := branchReadReleased(ctx, q, plan)
		if err != nil {
			return err
		}
		integrated, err := branchIntegratedNodes(ctx, st, plan, nodes)
		if err != nil {
			return err
		}
		candidates = branchCandidates(nodes, edges, declared, released, integrated, waitingNodes, minNodes, maxBranches)
		return nil
	})
	if err != nil {
		return nil, "", err
	}
	if unmeasured != "" {
		// branches stays null beside the reason: a pointer to a nil list, which is how a reader tells
		// "nobody could look" from the empty list of a plan nothing can be detached from.
		var unknown BranchCandidates
		return &unknown, unmeasured, nil
	}
	return candidates, unmeasured, nil
}

// branchCandidates is the detachable bundles of one plan's live nodes: the nodes no other live node
// joins by an edge or shares a declared place with, ranked as the report prints them.
func branchCandidates(nodes []branchPlanNode, edges []branchPlanEdge, declared []dagReviewRegion, released, integrated map[string]bool, waitingNodes []string, minNodes, maxBranches int) *BranchCandidates {
	// A live node is one the plan still holds (a cancelled or archived node was left out of the
	// reading) and that the relay does not count as integrated.
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
	for _, id := range waitingNodes {
		waitingSet[id] = true
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
		if started || len(members) < minNodes || len(members) >= len(live) {
			continue
		}
		inside := map[string]bool{}
		for _, id := range members {
			inside[id] = true
		}
		candidate := BranchCandidate{Nodes: []BranchNode{}, Regions: []string{}}
		paths := map[string]bool{}
		for _, id := range members {
			ready := waitingSet[id]
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
	if len(candidates) > maxBranches {
		candidates = candidates[:maxBranches]
	}
	return &candidates
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
