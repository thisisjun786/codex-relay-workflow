package dagsched

import (
	"context"
	"database/sql"
	"path"
	"sort"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dag"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// The bundle candidates of a plan (CRW-810): the live implementation nodes that nobody has released, grouped where two of them share a place to edit (same_region)
// or form a chain slice (chain_slice). The reading writes nothing; the parent decides what to merge and says so by a plan revision.

// A node the candidates leave out, with the closed reason the answer gives it.
const (
	BundleExcludedNonPR     = "non_pr"
	BundleExcludedReleased  = "released"
	BundleExcludedParent    = "parent_excluded"
	BundleExcludedOtherPlan = "other_plan_exclusive"
)

// The two rules that join two candidates.
const (
	BundleSameRegion = "same_region"
	BundleChainSlice = "chain_slice"
)

// BundlePair is two candidates that a rule joins, and the rules that join them (sorted).
type BundlePair struct {
	Nodes   [2]string
	Reasons []string
}

// Bundle is a connected group of two or more candidates: its nodes (sorted), the reasons and pairs that join them, the union of the regions they
// declared, and the edges between them, which a merge of the group would absorb.
type Bundle struct {
	Nodes         []string
	Reasons       []string
	Pairs         []BundlePair
	Regions       []Region
	InternalEdges []string
}

// BundleExclusion is a live node the candidates leave out, with its reason.
type BundleExclusion struct {
	Node   string
	Reason string
}

// BundleReading is the answer of dag-bundle-candidates for the head revision of a plan.
type BundleReading struct {
	PlanID       string
	PlanRevision int64
	Bundles      []Bundle
	Excluded     []BundleExclusion
}

// ReadBundles is BundleCandidates inside one read transaction of the store.
func (s *Scheduler) ReadBundles(ctx context.Context, plan string, exclude []string) (BundleReading, error) {
	var out BundleReading
	err := s.Store.Transaction(ctx, func(txCtx context.Context, _ *sql.Conn) error {
		var err error
		out, err = s.BundleCandidates(txCtx, s.Store.Q(txCtx), plan, exclude)
		return err
	})
	return out, err
}

// ReadWithBundles is Read and the bundle candidates of the same plan in one read transaction, so dag-ready's two answers see one state of the store.
func (s *Scheduler) ReadWithBundles(ctx context.Context, plan string) (Reading, BundleReading, error) {
	var out Reading
	var bundles BundleReading
	err := s.Store.Transaction(ctx, func(txCtx context.Context, _ *sql.Conn) error {
		q := s.Store.Q(txCtx)
		var err error
		if out, err = s.Ready(txCtx, q, plan, ReadyOptions{}); err != nil {
			return err
		}
		bundles, err = s.BundleCandidates(txCtx, q, plan, nil)
		return err
	})
	return out, bundles, err
}

// RecordPassWithBundles is RecordPass and the bundle candidates of the same plan in one transaction (dag-ready --record), so the pass and the candidates
// describe one plan revision and one set of releases: nothing can commit between the two readings.
func (s *Scheduler) RecordPassWithBundles(ctx context.Context, plan, actor string) (Reading, int64, BundleReading, error) {
	var reading Reading
	var seq int64
	var bundles BundleReading
	err := s.Store.Compose(ctx, func(txCtx context.Context, _ *sql.Conn) error {
		var err error
		// RecordPass and BundleCandidates join the transaction Compose opened
		if reading, seq, err = s.RecordPass(txCtx, plan, actor, ReadyOptions{}); err != nil {
			return err
		}
		if s.testBetweenPassAndBundles != nil {
			s.testBetweenPassAndBundles()
		}
		bundles, err = s.BundleCandidates(txCtx, s.Store.Q(txCtx), plan, nil)
		return err
	})
	return reading, seq, bundles, err
}

// BundleCandidates reads the bundle candidates of a plan's head revision. exclude names live nodes the parent leaves out; a name the plan does not hold is
// refused as a usage error. Two readings over one store state are equal: nothing is written and no clock is read.
func (s *Scheduler) BundleCandidates(ctx context.Context, q store.Querier, plan string, exclude []string) (BundleReading, error) {
	snap, _, err := dag.SnapshotAt(ctx, q, plan, 0)
	if err != nil {
		return BundleReading{}, err
	}
	nodes := append([]dag.SnapNode(nil), snap.Nodes...)
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].NodeID < nodes[j].NodeID })
	live := make(map[string]bool, len(nodes))
	for _, n := range nodes {
		live[n.NodeID] = true
	}
	left := make(map[string]bool, len(exclude))
	for _, id := range exclude {
		if !live[id] {
			return BundleReading{}, usage("--exclude " + id + ": plan " + plan + " holds no live node of that name")
		}
		left[id] = true
	}
	released, err := bundleReleased(ctx, q, plan)
	if err != nil {
		return BundleReading{}, err
	}
	decls, err := loadDeclarations(ctx, q, plan)
	if err != nil {
		return BundleReading{}, err
	}
	held, err := s.bundleHeldElsewhere(ctx, q, plan)
	if err != nil {
		return BundleReading{}, err
	}

	out := BundleReading{PlanID: plan, PlanRevision: snap.Revision}
	var order []string
	candidate := make(map[string]bool)
	for _, n := range nodes {
		reason := ""
		switch {
		case n.Kind != dag.NodeImplementation:
			reason = BundleExcludedNonPR
		case released[n.NodeID]:
			reason = BundleExcludedReleased
		case left[n.NodeID]:
			reason = BundleExcludedParent
		case bundleTouches(decls[n.NodeID], held):
			reason = BundleExcludedOtherPlan
		}
		if reason != "" {
			out.Excluded = append(out.Excluded, BundleExclusion{Node: n.NodeID, Reason: reason})
			continue
		}
		candidate[n.NodeID] = true
		order = append(order, n.NodeID)
	}

	// chain_slice: an edge A->B where B has no other incoming edge and A no other outgoing edge, both of them candidates
	incoming := map[string][]dag.SnapEdge{}
	outgoing := map[string]int{}
	for _, e := range snap.Edges {
		incoming[e.ToNodeID] = append(incoming[e.ToNodeID], e)
		outgoing[e.FromNodeID]++
	}
	chain := map[[2]string]bool{}
	for _, b := range order {
		in := incoming[b]
		if len(in) != 1 {
			continue
		}
		a := in[0].FromNodeID
		if candidate[a] && outgoing[a] == 1 {
			chain[[2]string{a, b}] = true
		}
	}

	// order is sorted by node id, so every pair is listed once with its smaller id first
	joined := map[[2]string][]string{}
	for i := range order {
		for j := i + 1; j < len(order); j++ {
			a, b := order[i], order[j]
			var reasons []string
			if bundleSameRegion(decls[a], decls[b]) {
				reasons = append(reasons, BundleSameRegion)
			}
			if chain[[2]string{a, b}] || chain[[2]string{b, a}] {
				reasons = append(reasons, BundleChainSlice)
			}
			if len(reasons) > 0 {
				sort.Strings(reasons)
				joined[[2]string{a, b}] = reasons
			}
		}
	}
	pairs := make([][2]string, 0, len(joined))
	for pair := range joined {
		pairs = append(pairs, pair)
	}
	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i][0] != pairs[j][0] {
			return pairs[i][0] < pairs[j][0]
		}
		return pairs[i][1] < pairs[j][1]
	})

	// connected components of the pairs, by union-find over the candidates
	root := map[string]string{}
	var find func(string) string
	find = func(id string) string {
		if root[id] == "" || root[id] == id {
			root[id] = id
			return id
		}
		root[id] = find(root[id])
		return root[id]
	}
	for _, pair := range pairs {
		if ra, rb := find(pair[0]), find(pair[1]); ra != rb {
			root[ra] = rb
		}
	}
	members := map[string][]string{}
	for _, id := range order {
		r := find(id)
		members[r] = append(members[r], id)
	}
	for _, list := range members {
		if len(list) < 2 {
			continue
		}
		out.Bundles = append(out.Bundles, bundleComponent(snap, decls, list, pairs, joined))
	}
	sort.Slice(out.Bundles, func(i, j int) bool { return out.Bundles[i].Nodes[0] < out.Bundles[j].Nodes[0] })
	return out, nil
}

// bundleComponent assembles one component: its pairs, reasons, regions and internal edges.
func bundleComponent(snap dag.Snapshot, decls map[string][]Region, list []string, pairs [][2]string, joined map[[2]string][]string) Bundle {
	in := make(map[string]bool, len(list))
	for _, id := range list {
		in[id] = true
	}
	b := Bundle{Nodes: list}
	reasons := map[string]bool{}
	for _, pair := range pairs {
		if !in[pair[0]] {
			continue
		}
		b.Pairs = append(b.Pairs, BundlePair{Nodes: pair, Reasons: joined[pair]})
		for _, r := range joined[pair] {
			reasons[r] = true
		}
	}
	for r := range reasons {
		b.Reasons = append(b.Reasons, r)
	}
	sort.Strings(b.Reasons)
	var regions []Region
	for _, id := range list {
		regions = append(regions, decls[id]...)
	}
	b.Regions = bundleRegions(regions)
	edges := append([]dag.SnapEdge(nil), snap.Edges...)
	sort.Slice(edges, func(i, j int) bool { return edges[i].EdgeID < edges[j].EdgeID })
	for _, e := range edges {
		if in[e.FromNodeID] && in[e.ToNodeID] {
			b.InternalEdges = append(b.InternalEdges, e.EdgeID)
		}
	}
	return b
}

// bundleSameRegion is whether two declarations name one file, or two files in one directory other than the repository root. A mechanical region is
// settled by its rule and never joins two nodes, so it is left out of the comparison.
func bundleSameRegion(a, b []Region) bool {
	for _, x := range a {
		if x.Grade == "mechanical" {
			continue
		}
		for _, y := range b {
			if y.Grade == "mechanical" || x.Repository != y.Repository {
				continue
			}
			if x.Path == y.Path {
				return true
			}
			if dir := path.Dir(x.Path); dir != "." && dir == path.Dir(y.Path) {
				return true
			}
		}
	}
	return false
}

// bundleTouches is whether a node's declared regions hold a place that another plan's unintegrated node holds exclusively.
func bundleTouches(regions []Region, held map[[2]string]bool) bool {
	for _, r := range regions {
		if held[[2]string{r.Repository, r.Path}] {
			return true
		}
	}
	return false
}

// bundleHeldElsewhere is the places (repository and path) that a node of another plan holds exclusively by its latest declaration, whatever its kind
// (update_node can turn a node that declared regions into a non_pr one, which keeps the declaration), while the node is live and not yet integrated. A declaration is exclusive by its stated hold or the exclusive grade.
func (s *Scheduler) bundleHeldElsewhere(ctx context.Context, q store.Querier, plan string) (map[[2]string]bool, error) {
	rows, err := q.QueryContext(ctx, "SELECT plan_id FROM dag_plans WHERE plan_id <> ? ORDER BY plan_id", plan)
	if err != nil {
		return nil, err
	}
	var others []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		others = append(others, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	held := map[[2]string]bool{}
	for _, other := range others {
		snap, _, err := dag.SnapshotAt(ctx, q, other, 0)
		if err != nil {
			return nil, err
		}
		decls, err := loadDeclarations(ctx, q, other)
		if err != nil {
			return nil, err
		}
		for _, n := range snap.Nodes {
			if !bundleHasExclusive(decls[n.NodeID]) {
				continue
			}
			state, err := s.stateOf(ctx, q, other, snap, n)
			if err != nil {
				return nil, err
			}
			if state.State == StateIntegrated {
				continue
			}
			for _, r := range decls[n.NodeID] {
				if r.Exclusive || r.Grade == "exclusive" {
					held[[2]string{r.Repository, r.Path}] = true
				}
			}
		}
	}
	return held, nil
}

func bundleHasExclusive(regions []Region) bool {
	for _, r := range regions {
		if r.Exclusive || r.Grade == "exclusive" {
			return true
		}
	}
	return false
}

// bundleReleased is the nodes of a plan that have a release or a managed start on record.
func bundleReleased(ctx context.Context, q store.Querier, plan string) (map[string]bool, error) {
	rows, err := q.QueryContext(ctx, "SELECT node_id FROM dag_releases WHERE plan_id = ? UNION SELECT node_id FROM dag_release_requests WHERE plan_id = ?", plan, plan)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}

// bundleRegions is the union of the members' regions with one entry per place (repository, path, kind, key), sorted: the shape dag-region-declare reads, which
// refuses one place declared twice with different words (normalizeRegions). Members that declare one place differently are merged by bundleMerge, so the
// union holds every place at least as strictly as any member did.
func bundleRegions(list []Region) []Region {
	sort.SliceStable(list, func(i, j int) bool { return bundleRegionLess(list[i], list[j]) })
	var out []Region
	for _, r := range list {
		if n := len(out); n > 0 && bundleSamePlace(out[n-1], r) {
			out[n-1] = bundleMerge(out[n-1], r)
			continue
		}
		out = append(out, r)
	}
	return out
}

func bundleSamePlace(a, b Region) bool {
	return a.Repository == b.Repository && a.Path == b.Path && a.Kind == b.Kind && a.Key == b.Key
}

// bundleMerge merges two declarations of one place so that the result holds it at least as strictly as each (the order the hold is judged by: holdWeight, foldedGrade).
//   - exclusive (the whole-repository hold) when either is;
//   - change: the strictest of edit, rename, delete, in that order;
//   - grade: the stricter of mechanical, local, independent, exclusive, in that order; two mechanical grades keep their rule when it is the same one and
//     become local when the rules differ (PairGrade reads two rules as local); the rule survives only on a mechanical grade;
//   - then foldedGrade, so a rename, a delete, a hotspot, a shared contract surface or the exclusive flag holds the place exclusively, as declaring it would.
//
// The merge is commutative and associative, so any order of members gives one answer.
func bundleMerge(a, b Region) Region {
	out := a
	out.Exclusive = a.Exclusive || b.Exclusive
	if bundleChangeRank(b.Change) > bundleChangeRank(a.Change) {
		out.Change = b.Change
	}
	ga, ra := foldedGrade(a)
	gb, rb := foldedGrade(b)
	switch {
	case ga == GradeMechanical && gb == GradeMechanical && ra == rb:
		out.Grade, out.Rule = GradeMechanical, ra
	case ga == GradeMechanical && gb == GradeMechanical:
		out.Grade, out.Rule = GradeLocal, ""
	case bundleGradeRank(gb) > bundleGradeRank(ga):
		out.Grade, out.Rule = gb, rb
	default:
		out.Grade, out.Rule = ga, ra
	}
	out.Grade, out.Rule = foldedGrade(out)
	return out
}

func bundleChangeRank(change string) int {
	switch change {
	case "delete":
		return 2
	case "rename":
		return 1
	}
	return 0
}

func bundleGradeRank(grade string) int {
	switch grade {
	case GradeMechanical:
		return 1
	case GradeLocal:
		return 2
	case GradeIndependent:
		return 3
	}
	return 4
}

func bundleRegionLess(a, b Region) bool {
	switch {
	case a.Repository != b.Repository:
		return a.Repository < b.Repository
	case a.Path != b.Path:
		return a.Path < b.Path
	case a.Kind != b.Kind:
		return a.Kind < b.Kind
	}
	return a.Key < b.Key
}

// lists is the bundles and the excluded nodes, the part dag-ready carries as bundleCandidates.
func (r BundleReading) lists() contract.OrderedObject {
	bundles := make([]any, len(r.Bundles))
	for i, b := range r.Bundles {
		bundles[i] = b.object()
	}
	excluded := make([]any, len(r.Excluded))
	for i, e := range r.Excluded {
		excluded[i] = contract.OrderedObject{{Key: "node", Value: e.Node}, {Key: "reason", Value: e.Reason}}
	}
	return contract.OrderedObject{{Key: "bundles", Value: bundles}, {Key: "excluded", Value: excluded}}
}

// Object is the answer of dag-bundle-candidates.
func (r BundleReading) Object() contract.OrderedObject {
	head := contract.OrderedObject{{Key: "ok", Value: true}, {Key: "plan_id", Value: r.PlanID}, {Key: "plan_revision", Value: r.PlanRevision}}
	return append(head, r.lists()...)
}

func (b Bundle) object() contract.OrderedObject {
	pairs := make([]any, len(b.Pairs))
	for i, p := range b.Pairs {
		pairs[i] = contract.OrderedObject{{Key: "nodes", Value: []any{p.Nodes[0], p.Nodes[1]}}, {Key: "reasons", Value: bundleStrings(p.Reasons)}}
	}
	regions := make([]any, len(b.Regions))
	for i, r := range b.Regions {
		regions[i] = contract.OrderedObject{{Key: "repository", Value: r.Repository}, {Key: "path", Value: r.Path}, {Key: "kind", Value: r.Kind},
			{Key: "key", Value: optionalText(r.Key)}, {Key: "change", Value: r.Change}, {Key: "exclusive", Value: r.Exclusive},
			{Key: "grade", Value: r.Grade}, {Key: "rule", Value: optionalText(r.Rule)}}
	}
	return contract.OrderedObject{{Key: "nodes", Value: bundleStrings(b.Nodes)}, {Key: "reasons", Value: bundleStrings(b.Reasons)},
		{Key: "pairs", Value: pairs}, {Key: "regions", Value: regions}, {Key: "internalEdges", Value: bundleStrings(b.InternalEdges)}}
}

func bundleStrings(list []string) []any {
	out := make([]any, len(list))
	for i, s := range list {
		out[i] = s
	}
	return out
}
