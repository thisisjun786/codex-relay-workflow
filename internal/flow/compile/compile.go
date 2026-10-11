package compile

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dag"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dagsched"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
	"github.com/thisisjun786/codex-relay-workflow/internal/skill"
)

// Options are what a compile is made against.
type Options struct {
	// Bases is the plan each project already has, by plan id, as a fold of its log with its history (BaseFromLog). A project without
	// one starts a new plan at revision 0.
	Bases map[string]dag.State
	// DryRun leaves the revision documents out of the result; the previews are the same.
	DryRun bool
}

// Compile turns the handoff into one plan result per project, or refuses all of it with a *Refused. A refused handoff
// compiles nothing: the planner fixes it and compiles again.
func Compile(in Input, opts Options) (*Result, error) {
	var refusals []Refusal
	if in.Schema != InputSchema {
		refusals = append(refusals, Refusal{Rule: "input", Path: "$.schema", Detail: fmt.Sprintf("must be %q", InputSchema)})
	}
	if len(in.Projects) == 0 {
		refusals = append(refusals, Refusal{Rule: "input", Path: "$.projects", Detail: "the handoff holds no project"})
	}
	owner := map[string]string{} // issue key -> the project that holds it
	projects, plans := map[string]bool{}, map[string]bool{}
	for i, p := range in.Projects {
		path := fmt.Sprintf("$.projects[%d]", i)
		if p.Key == "" {
			refusals = append(refusals, Refusal{Rule: "input", Path: path + ".key", Detail: "a project needs its key"})
			continue
		}
		if projects[p.Key] {
			refusals = append(refusals, Refusal{Project: p.Key, Rule: "input", Path: path + ".key", Detail: "two projects have this key"})
		}
		projects[p.Key] = true
		if id := planID(p); plans[id] {
			refusals = append(refusals, Refusal{Project: p.Key, Rule: "input", Path: path + ".plan_id", Detail: fmt.Sprintf("two projects write plan %s", id)})
		} else {
			plans[id] = true
		}
		for j, is := range p.Issues {
			first, dup := owner[is.Key]
			switch {
			case !dup:
				owner[is.Key] = p.Key
			case first != p.Key:
				// one issue is one executor's work in one project: a key held twice makes the owner of an external prerequisite ambiguous
				refusals = append(refusals, Refusal{Project: p.Key, Rule: "input", Path: fmt.Sprintf("%s.issues[%d].key", path, j),
					Detail: fmt.Sprintf("issue %s is already held by project %s: an issue belongs to one project", is.Key, first)})
			}
		}
	}
	refusals = append(refusals, crossProjectCycle(in, owner)...)
	res := &Result{Schema: OutputSchema, Initiative: in.Initiative}
	for i, p := range in.Projects {
		if p.Key == "" {
			continue
		}
		plan, rs := compileProject(in, i, owner, opts)
		refusals = append(refusals, rs...)
		res.Plans = append(res.Plans, plan)
	}
	if len(refusals) > 0 {
		return nil, &Refused{Refusals: refusals}
	}
	return res, nil
}

// crossProjectCycle refuses a handoff whose prerequisites form a cycle through more than one project: every project compiles a plan of
// its own, so the cycle would pass each plan's check and leave the projects waiting for each other. A cycle inside one project is
// that project's own refusal.
func crossProjectCycle(in Input, owner map[string]string) []Refusal {
	var keys []string
	var edges []pair
	seen := map[pair]bool{}
	add := func(from, to string) {
		if e := (pair{from, to}); from != to && !seen[e] {
			seen[e] = true
			edges = append(edges, e)
		}
	}
	for _, p := range in.Projects {
		members := map[string][]string{}
		var groups []string
		for _, is := range p.Issues {
			if owner[is.Key] != p.Key {
				continue // a duplicate: refused already
			}
			keys = append(keys, is.Key)
			for _, a := range is.After {
				if owner[a] != "" {
					add(a, is.Key)
				}
			}
			if is.Group != "" {
				if _, ok := members[is.Group]; !ok {
					groups = append(groups, is.Group)
				}
				members[is.Group] = append(members[is.Group], is.Key)
			}
		}
		for _, g := range groups {
			for k := 1; k < len(members[g]); k++ {
				add(members[g][k-1], members[g][k])
			}
		}
	}
	cycle := findCycle(keys, edges)
	if cycle == "" {
		return nil
	}
	projects := map[string]bool{}
	for _, k := range strings.Split(cycle, " -> ") {
		projects[owner[k]] = true
	}
	if len(projects) < 2 {
		return nil
	}
	return []Refusal{{Rule: "cycle", Path: "$.projects", Detail: fmt.Sprintf("issues %s form a cycle across the projects of the handoff; a plan is a DAG and so is the handoff", cycle)}}
}

func planID(p Project) string {
	if p.PlanID != "" {
		return p.PlanID
	}
	return p.Key
}

// unit is an issue after its input was read.
type unit struct {
	Issue
	nodeID   string
	digest   string
	regions  []Region
	kind     string
	size     [3]string // size_fit, size_reason, issue_size as carried
	external []string
	op       string
}

type pair struct{ from, to string } // issue keys

func compileProject(in Input, at int, owner map[string]string, opts Options) (Plan, []Refusal) {
	p := in.Projects[at]
	id := planID(p)
	plan := Plan{PlanID: id, ProjectKey: p.Key, Status: StatusChanges}
	var refusals []Refusal
	refuse := func(rule, path, format string, args ...any) {
		refusals = append(refusals, Refusal{Project: p.Key, Rule: rule, Path: path, Detail: fmt.Sprintf(format, args...)})
	}
	base := opts.Bases[id]
	plan.BaseRevision = base.Revision

	if len(p.Issues) == 0 {
		refuse("input", "issues", "the project holds no issue")
		return plan, refusals
	}
	if len(p.Issues) > dag.MaxNodes {
		refuse("over_64", "issues", "the project holds %d issues; a plan holds at most %d (split the project)", len(p.Issues), dag.MaxNodes)
		return plan, refusals
	}
	if p.CoordinatorEpoch < 0 {
		refuse("input", "coordinator_epoch", "coordinator_epoch %d: an epoch is 0 (a plan nobody has claimed) or the epoch of the writing session's claim", p.CoordinatorEpoch)
	}
	if t := p.Target; t != nil && (t.Repository == "" || t.BaseRef == "") {
		refuse("input", "target", "target needs both repository and base_ref")
	}

	// --- the issues
	units := make([]*unit, len(p.Issues))
	byKey := map[string]*unit{}
	for i, is := range p.Issues {
		path := fmt.Sprintf("issues[%d]", i)
		u := &unit{Issue: is, kind: is.Kind}
		units[i] = u
		switch {
		case is.Key == "":
			refuse("input", path+".key", "an issue needs its key")
			continue
		case byKey[is.Key] != nil:
			refuse("input", path+".key", "issue %s is listed twice", is.Key)
			continue
		}
		byKey[is.Key] = u
		if is.Title == "" {
			refuse("input", path+".title", "issue %s needs its title", is.Key)
		}
		switch u.kind {
		case "":
			u.kind = dag.NodeImplementation
		case dag.NodeImplementation, dag.NodeNonPR:
		default:
			refuse("input", path+".kind", "kind %q is not implementation or non_pr", is.Kind)
		}
		switch {
		case len(is.Criteria) > 0 && is.CriteriaSetDigest != "":
			refuse("input", path+".criteria", "give the criteria or their digest, not both")
		case len(is.Criteria) > 0:
			for j, c := range is.Criteria {
				if strings.TrimSpace(c) == "" {
					refuse("input", fmt.Sprintf("%s.criteria[%d]", path, j), "a criterion needs its text: the relay registers no blank title")
				}
			}
			u.digest = criteriaDigest(is.Criteria)
		case is.CriteriaSetDigest != "":
			u.digest = is.CriteriaSetDigest
		default:
			refuse("input", path+".criteria", "issue %s has no criteria: the node's criteria_set_digest is made from them", is.Key)
		}
		for j, r := range is.Regions {
			c, err := cleanRegion(r)
			if err != nil {
				refuse("input", fmt.Sprintf("%s.regions[%d]", path, j), "%v", err)
				continue
			}
			u.regions = append(u.regions, c)
		}
		u.size = [3]string{is.SizeFit, is.SizeReason, is.IssueSize}
		for k, set := range []struct {
			name   string
			values []string
			def    string
		}{{"size_fit", skill.SizeFits, "executor"}, {"size_reason", skill.SizeReasons, "fits_executor"}, {"issue_size", skill.IssueSizeAnswers, ""}} {
			switch v := u.size[k]; {
			case v == "" && set.def != "":
				u.size[k] = set.def
			case v != "" && !slices.Contains(set.values, v):
				refuse("size_value", path+"."+set.name, "%s %q is not one of %s", set.name, v, strings.Join(set.values, ", "))
			}
		}
	}

	// --- prerequisites and groups
	var order []pair // declared prerequisites, then group chains
	seen := map[pair]bool{}
	addOrder := func(from, to string) {
		if e := (pair{from, to}); !seen[e] {
			seen[e] = true
			order = append(order, e)
		}
	}
	for i, u := range units {
		for _, a := range u.After {
			path := fmt.Sprintf("issues[%d].after", i)
			switch {
			case a == u.Key:
				refuse("self_reference", path, "issue %s is its own prerequisite", a)
			case byKey[a] != nil:
				addOrder(a, u.Key)
			case owner[a] != "" && owner[a] != p.Key:
				u.external = append(u.external, a)
			default:
				refuse("after_unknown", path, "issue %s names the prerequisite %s, which no project of the handoff holds", u.Key, a)
			}
		}
	}
	var groupNames []string
	members := map[string][]string{}
	for _, u := range units {
		if u.Group == "" {
			continue
		}
		if _, ok := members[u.Group]; !ok {
			groupNames = append(groupNames, u.Group)
		}
		members[u.Group] = append(members[u.Group], u.Key)
	}
	for _, g := range groupNames {
		m := members[g]
		if len(m) > MaxGroup {
			refuse("group_cap", "group:"+g, "group %s names %d issues (%s); a group names at most %d", g, len(m), strings.Join(m, ", "), MaxGroup)
		}
		for k := 1; k < len(m); k++ {
			addOrder(m[k-1], m[k])
		}
	}
	if len(refusals) > 0 {
		return plan, refusals
	}

	// --- the shape of the plan
	keys := make([]string, len(units))
	for i, u := range units {
		keys[i] = u.Key
	}
	if cycle := findCycle(keys, order); cycle != "" {
		refuse("cycle", "plan", "issues %s form a cycle; a plan is a DAG", cycle)
		return plan, refusals
	}
	reach := closure(keys, order)
	refusals = append(refusals, regionOrder(p.Key, p.Target, units, reach)...)

	// --- the plan revision
	h := readBase(base)
	assign(units, h)
	changes, counts, gates := diff(units, order, p.Target, h, &refusals, p.Key)
	// The plan the revision leaves holds the decision edges that stay as well as the order edges: the preview reads that graph.
	live := append(append([]pair{}, order...), gates...)
	if cycle := findCycle(keys, live); cycle != "" {
		refuse("cycle", "plan", "issues %s form a cycle with the decision edges the plan already holds; a plan is a DAG", cycle)
		return plan, refusals
	}
	plan.Preview = preview(units, live, len(order)+len(gates), keys, counts, h, groupNames, members, base.Revision, owner)
	if len(refusals) > 0 {
		return plan, refusals
	}
	if len(changes) == 0 {
		plan.Status = StatusUnchanged
		return plan, nil
	}
	rev := dag.Revision{PlanID: id, ProjectKey: p.Key, RequestID: p.RequestID, ExpectedParent: base.Revision, CoordinatorEpoch: p.CoordinatorEpoch, AuthorTaskID: in.AuthorTaskID, Changes: changes}
	if rev.AuthorTaskID == "" {
		rev.AuthorTaskID = DefaultAuthor
	}
	if rev.RequestID == "" {
		rev.RequestID = "flow-compile-" + dag.RequestDigest(rev)[:16]
	}
	plan.Preview.RequestID = rev.RequestID
	checked, err := dag.Checked(rev)
	if err != nil {
		return plan, append(refusals, dagRefusals(p.Key, err)...)
	}
	if base.PlanID == "" {
		base.PlanID = id
	}
	if _, _, vs := dag.Apply(base, checked); len(vs) > 0 {
		for _, v := range vs {
			refusals = append(refusals, Refusal{Project: p.Key, Rule: v.Rule, Path: v.Path, Detail: v.Detail})
		}
		return plan, refusals
	}
	plan.Change = &checked
	if !opts.DryRun {
		plan.Revision, err = document(checked)
		if err != nil {
			return plan, append(refusals, Refusal{Project: p.Key, Rule: "input", Detail: err.Error()})
		}
	}
	return plan, nil
}

func dagRefusals(project string, err error) []Refusal {
	var rejected *dag.PlanRejected
	if errors.As(err, &rejected) {
		out := make([]Refusal, len(rejected.Violations))
		for i, v := range rejected.Violations {
			out[i] = Refusal{Project: project, Rule: v.Rule, Path: v.Path, Detail: v.Detail}
		}
		return out
	}
	return []Refusal{{Project: project, Rule: "input", Detail: err.Error()}}
}

// criteriaDigest is the node's criteria_set_digest, the digest dag-release recomputes from the canonical criteria
// (delivery.SetDigest of {id, title, required}): the criteria of the handoff, in the order they are listed, are the canonical
// criteria c1, c2, ... each required, with the title trimmed as the release trims it.
func criteriaDigest(criteria []string) string {
	set := make([]delivery.Criterion, len(criteria))
	for i, c := range criteria {
		set[i] = delivery.Criterion{ID: fmt.Sprintf("c%d", i+1), Title: strings.TrimSpace(c), Required: true}
	}
	return delivery.SetDigest(set)
}

// document is the revision as the dag-plan-revision/1 document dag-plan-put reads (canonical JSON).
func document(rev dag.Revision) (json.RawMessage, error) {
	var changes []any
	if err := json.Unmarshal([]byte(dag.ChangesJSON(rev.Changes)), &changes); err != nil {
		return nil, err
	}
	doc := map[string]any{
		"schema": dag.SchemaRevision, "plan_id": rev.PlanID, "project_key": rev.ProjectKey, "request_id": rev.RequestID,
		"expected_parent_revision": rev.ExpectedParent, "author_task_id": rev.AuthorTaskID, "changes": changes,
	}
	if rev.CoordinatorEpoch != 0 {
		doc["coordinator_epoch"] = rev.CoordinatorEpoch
	}
	raw := []byte(dag.Canonical(doc))
	if _, err := dag.DecodeRevision(raw); err != nil {
		return nil, err
	}
	return raw, nil
}

// --- graph

// findCycle returns a cycle of the order edges as "a -> b -> a", or "".
func findCycle(keys []string, edges []pair) string {
	succ := map[string][]string{}
	indeg := map[string]int{}
	for _, k := range keys {
		indeg[k] = 0
	}
	for _, e := range edges {
		succ[e.from] = append(succ[e.from], e.to)
		indeg[e.to]++
	}
	var queue []string
	for _, k := range keys {
		if indeg[k] == 0 {
			queue = append(queue, k)
		}
	}
	removed := map[string]bool{}
	for len(queue) > 0 {
		k := queue[0]
		queue = queue[1:]
		removed[k] = true
		for _, to := range succ[k] {
			if indeg[to]--; indeg[to] == 0 {
				queue = append(queue, to)
			}
		}
	}
	var left []string
	for _, k := range keys {
		if !removed[k] {
			left = append(left, k)
		}
	}
	if len(left) == 0 {
		return ""
	}
	sort.Strings(left)
	inLeft := map[string]bool{}
	for _, k := range left {
		inLeft[k] = true
	}
	// from the smallest key, follow successors that are left until a key repeats
	at := map[string]int{}
	var walk []string
	cur := left[0]
	for {
		if i, ok := at[cur]; ok {
			cycle := append(append([]string{}, walk[i:]...), cur)
			// start at the smallest key
			cycle = cycle[:len(cycle)-1]
			low := 0
			for j, k := range cycle {
				if k < cycle[low] {
					low = j
				}
			}
			cycle = append(cycle[low:], cycle[:low]...)
			return strings.Join(append(cycle, cycle[0]), " -> ")
		}
		at[cur] = len(walk)
		walk = append(walk, cur)
		next := ""
		nexts := append([]string(nil), succ[cur]...)
		sort.Strings(nexts)
		for _, s := range nexts {
			if inLeft[s] {
				next = s
				break
			}
		}
		if next == "" {
			return strings.Join(left, " -> ")
		}
		cur = next
	}
}

// closure is the transitive closure of the order edges: reach[a][b] when b can only start after a.
func closure(keys []string, edges []pair) map[string]map[string]bool {
	succ := map[string][]string{}
	for _, e := range edges {
		succ[e.from] = append(succ[e.from], e.to)
	}
	reach := map[string]map[string]bool{}
	for _, k := range keys {
		seen := map[string]bool{}
		stack := append([]string(nil), succ[k]...)
		for len(stack) > 0 {
			n := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			if seen[n] {
				continue
			}
			seen[n] = true
			stack = append(stack, succ[n]...)
		}
		reach[k] = seen
	}
	return reach
}

// regionOrder refuses every pair of issues whose edit regions overlap (by the scheduler's rule) with no order between them.
func regionOrder(project string, target *Target, units []*unit, reach map[string]map[string]bool) []Refusal {
	repo := ""
	if target != nil {
		repo = target.Repository
	}
	scheduler := func(r Region) dagsched.Region {
		return dagsched.Region{Repository: repo, Path: r.Path, Kind: r.Kind, Key: r.Key, Change: r.Change, Exclusive: r.Exclusive}
	}
	var out []Refusal
	for i, a := range units {
		for _, b := range units[i+1:] {
			if reach[a.Key][b.Key] || reach[b.Key][a.Key] {
				continue
			}
			var shared []string
			for _, ra := range a.regions {
				for _, rb := range b.regions {
					if dagsched.Overlaps(scheduler(ra), scheduler(rb)) {
						shared = append(shared, ra.String()+" ~ "+rb.String())
					}
				}
			}
			if len(shared) > 0 {
				out = append(out, Refusal{Project: project, Rule: "region_order_missing", Path: a.Key + "," + b.Key,
					Detail: fmt.Sprintf("issues %s and %s share an edit region (%s) and nothing orders them: name one as the other's prerequisite (after) or put both in one group", a.Key, b.Key, strings.Join(shared, "; "))})
			}
		}
	}
	return out
}

// --- the base plan and the changes

type baseView struct {
	liveByKey map[string]dag.NodeVersion
	liveByID  map[string]dag.NodeVersion
	usedNode  map[string]bool
	liveEdges []dag.EdgeRow // every live edge by edge id: a plan may hold several between one pair of nodes
	usedEdge  map[string]bool
}

func readBase(s dag.State) baseView {
	h := baseView{liveByKey: map[string]dag.NodeVersion{}, liveByID: map[string]dag.NodeVersion{}, usedNode: map[string]bool{}, usedEdge: map[string]bool{}}
	for _, n := range s.Nodes {
		h.usedNode[n.NodeID] = true
		if n.RetiredRev == 0 {
			h.liveByKey[n.IssueKey] = n
			h.liveByID[n.NodeID] = n
		}
	}
	for _, e := range s.Edges {
		h.usedEdge[e.EdgeID] = true
		if e.RetiredRev == 0 {
			h.liveEdges = append(h.liveEdges, e)
		}
	}
	sort.Slice(h.liveEdges, func(i, j int) bool { return h.liveEdges[i].EdgeID < h.liveEdges[j].EdgeID })
	return h
}

// assign gives every issue its node id: the id of the live node it already has, else its key, else (the key was used by a
// node that left, or by another issue) the key with a suffix no node of the plan ever had.
func assign(units []*unit, h baseView) {
	taken := map[string]bool{}
	desired := map[string]bool{}
	for _, u := range units {
		desired[u.Key] = true
	}
	for _, u := range units {
		if n, ok := h.liveByKey[u.Key]; ok {
			u.nodeID = n.NodeID
			taken[u.nodeID] = true
		}
	}
	for _, u := range units {
		if u.nodeID != "" {
			continue
		}
		id := u.Key
		for n := 2; h.usedNode[id] || taken[id] || (id != u.Key && desired[id]); n++ {
			id = fmt.Sprintf("%s.%d", u.Key, n)
		}
		u.nodeID = id
		taken[id] = true
	}
}

// edgeSpec is what an order edge from an issue of kind k carries: an implementation node's work is integrated before the next one
// starts, a non_pr node's artifact is verified.
func edgeSpec(from *unit, t *Target) dag.Edge {
	if from.kind == dag.NodeImplementation {
		e := dag.Edge{Kind: dag.EdgeIntegrated}
		if t != nil {
			e.TargetRepository, e.TargetBaseRef = t.Repository, t.BaseRef
		}
		return e
	}
	return dag.Edge{Kind: dag.EdgeArtifactVerified}
}

func newEdgeID(from, to string, h baseView, used map[string]bool) string {
	id := "e:" + from + ":" + to
	for n := 2; h.usedEdge[id] || used[id]; n++ {
		id = fmt.Sprintf("e:%s:%s.%d", from, to, n)
	}
	used[id] = true
	return id
}

// diff is the changes that take the base plan to the handoff, in an order that reads: edges and nodes that leave, nodes that change,
// nodes that come, edges that come. It also returns the decision edges that stay, as pairs of issue keys.
func diff(units []*unit, order []pair, t *Target, h baseView, refusals *[]Refusal, project string) ([]dag.Change, ChangeCounts, []pair) {
	var counts ChangeCounts
	byKey := map[string]*unit{}
	for _, u := range units {
		byKey[u.Key] = u
	}
	var retireEdges, retireNodes, updates, adds, addEdges []dag.Change
	var gates []pair
	keyOf := map[string]string{} // node id -> issue key
	for _, u := range units {
		keyOf[u.nodeID] = u.Key
	}
	want := map[pair]dag.Edge{} // by node ids
	edgeIDs := map[pair]string{}

	desiredIDs := map[string]bool{}
	for _, u := range units {
		desiredIDs[u.nodeID] = true
	}
	for _, o := range order {
		from, to := byKey[o.from], byKey[o.to]
		spec := edgeSpec(from, t)
		if spec.Kind == dag.EdgeIntegrated && t == nil {
			*refusals = append(*refusals, Refusal{Project: project, Rule: "input", Path: "target", Detail: fmt.Sprintf("the order edge %s -> %s integrates code: the project needs its target (repository and base_ref)", o.from, o.to)})
			continue
		}
		spec.FromNodeID, spec.ToNodeID = from.nodeID, to.nodeID
		want[pair{from.nodeID, to.nodeID}] = spec
	}
	// nodes
	for _, u := range units {
		node := dag.Node{NodeID: u.nodeID, IssueKey: u.Key, Kind: u.kind, Title: u.Title, CriteriaSetDigest: u.digest}
		live, ok := h.liveByID[u.nodeID]
		switch {
		case !ok:
			u.op = "add"
			adds = append(adds, dag.Change{Op: dag.OpAddNode, Node: &node})
			counts.AddNode++
		case live.Node != node:
			u.op = "update"
			updates = append(updates, dag.Change{Op: dag.OpUpdateNode, Node: &node})
			counts.UpdateNode++
		default:
			u.op = "keep"
		}
	}
	var gone []string
	for id := range h.liveByID {
		if !desiredIDs[id] {
			gone = append(gone, id)
		}
	}
	sort.Strings(gone)
	for _, id := range gone {
		retireNodes = append(retireNodes, dag.Change{Op: dag.OpRetireNode, NodeID: id})
		counts.RetireNode++
	}
	// edges
	for _, row := range h.liveEdges { // by edge id; the first edge of a pair that is what the handoff wants stays, any other is retired
		e := pair{row.FromNodeID, row.ToNodeID}
		// A decision edge carries an approval (subject, digest, required authority), which the handoff has no way to state: it is
		// neither an order edge compile owns nor something to drop in silence, so it stays while both of its nodes do.
		if row.Kind == dag.EdgeDecision && desiredIDs[row.FromNodeID] && desiredIDs[row.ToNodeID] {
			counts.KeepDecisionEdge++
			gates = append(gates, pair{keyOf[row.FromNodeID], keyOf[row.ToNodeID]})
			continue
		}
		if w, ok := want[e]; ok && edgeIDs[e] == "" && sameEdge(row.Edge, w) {
			edgeIDs[e] = row.EdgeID
			continue
		}
		retireEdges = append(retireEdges, dag.Change{Op: dag.OpRetireEdge, EdgeID: row.EdgeID})
		counts.RetireEdge++
	}
	used := map[string]bool{}
	var wanted []pair
	for e := range want {
		wanted = append(wanted, e)
	}
	sort.Slice(wanted, func(i, j int) bool {
		if wanted[i].from != wanted[j].from {
			return wanted[i].from < wanted[j].from
		}
		return wanted[i].to < wanted[j].to
	})
	for _, e := range wanted {
		if edgeIDs[e] != "" {
			continue
		}
		spec := want[e]
		spec.EdgeID = newEdgeID(e.from, e.to, h, used)
		edgeIDs[e] = spec.EdgeID
		addEdges = append(addEdges, dag.Change{Op: dag.OpAddEdge, Edge: &spec})
		counts.AddEdge++
	}
	var all []dag.Change
	for _, part := range [][]dag.Change{retireEdges, retireNodes, updates, adds, addEdges} {
		all = append(all, part...)
	}
	return all, counts, gates
}

func sameEdge(a, b dag.Edge) bool {
	return a.Kind == b.Kind && a.TargetRepository == b.TargetRepository && a.TargetBaseRef == b.TargetBaseRef && a.PinsCodeHead == b.PinsCodeHead &&
		a.DecisionSubject == b.DecisionSubject && a.DecisionDigest == b.DecisionDigest && slices.Equal(a.RequiredAuthority, b.RequiredAuthority)
}

// --- the preview

// graph is the order edges and the kept decision edges, edges the live edge count of the plan the revision leaves.
func preview(units []*unit, graph []pair, edges int, keys []string, counts ChangeCounts, h baseView,
	groupNames []string, members map[string][]string, baseRev int64, owner map[string]string) Preview {
	pv := Preview{Nodes: len(units), Edges: edges, Changes: counts, ExpectedParentRev: baseRev,
		Issues: []IssuePreview{}, Groups: []GroupPreview{}, Milestones: []MilestoneCount{}, SizeUnreviewed: []string{}, ExternalPrereqs: []ExternalAfter{}, RetiredIssues: []string{}}
	desired := map[string]bool{}
	for _, u := range units {
		desired[u.Key] = true
	}
	var retired []string
	for key := range h.liveByKey {
		if !desired[key] {
			retired = append(retired, key)
		}
	}
	sort.Strings(retired)
	pv.RetiredIssues = append(pv.RetiredIssues, retired...)

	milestones := map[string]int{}
	var milestoneOrder []string
	for _, u := range units {
		ip := IssuePreview{
			Key: u.Key, NodeID: u.nodeID, Op: u.op, Kind: u.kind, Milestone: u.Milestone, Regions: []string{}, After: []string{}, Group: u.Group,
			SizeFit: u.size[0], SizeReason: u.size[1], IssueSize: u.size[2], BundleReason: u.BundleReason, ExceptionRecord: u.ExceptionRecord,
		}
		for _, r := range u.regions {
			ip.Regions = append(ip.Regions, r.String())
		}
		for _, a := range u.After {
			if !slices.Contains(u.external, a) {
				ip.After = append(ip.After, a)
			}
		}
		// over_line with neither a bundle reason nor a split: shown for the planner, never refused
		if u.size[2] == "over_line" && u.BundleReason == "" && u.size[0] != "split" {
			ip.SizeUnreviewed = true
			pv.SizeUnreviewed = append(pv.SizeUnreviewed, u.Key)
		}
		pv.Issues = append(pv.Issues, ip)
		for _, a := range u.external {
			pv.ExternalPrereqs = append(pv.ExternalPrereqs, ExternalAfter{Issue: u.Key, After: a, Project: owner[a]})
		}
		if u.Milestone != "" {
			if _, ok := milestones[u.Milestone]; !ok {
				milestoneOrder = append(milestoneOrder, u.Milestone)
			}
			milestones[u.Milestone]++
		}
	}
	for _, m := range milestoneOrder {
		pv.Milestones = append(pv.Milestones, MilestoneCount{Milestone: m, Issues: milestones[m]})
	}
	for _, g := range groupNames {
		pv.Groups = append(pv.Groups, GroupPreview{Group: g, Order: append([]string{}, members[g]...)})
	}
	pv.ParallelWidth = width(keys, graph)
	pv.CriticalPath = criticalPath(keys, graph)
	pv.CriticalPathLen = len(pv.CriticalPath)
	pv.Waves = waves(keys, graph)
	return pv
}

// width is the most issues that can run at once: the widest set of issues none of which has to wait for another (Dilworth: the issues
// less the largest matching of the order's transitive closure).
func width(keys []string, order []pair) int {
	reach := closure(keys, order)
	matchOf := map[string]string{} // right side -> left side
	var try func(u string, seen map[string]bool) bool
	try = func(u string, seen map[string]bool) bool {
		for _, v := range keys {
			if !reach[u][v] || seen[v] {
				continue
			}
			seen[v] = true
			if w, taken := matchOf[v]; !taken || try(w, seen) {
				matchOf[v] = u
				return true
			}
		}
		return false
	}
	matching := 0
	for _, u := range keys {
		if try(u, map[string]bool{}) {
			matching++
		}
	}
	return len(keys) - matching
}

// criticalPath is the longest chain of issues; among equal chains, the one that reads first by key.
func criticalPath(keys []string, order []pair) []string {
	succ := map[string][]string{}
	for _, e := range order {
		succ[e.from] = append(succ[e.from], e.to)
	}
	longest := map[string]int{}
	var depth func(k string) int
	depth = func(k string) int {
		if n, ok := longest[k]; ok {
			return n
		}
		best := 0
		for _, s := range succ[k] {
			if d := depth(s); d > best {
				best = d
			}
		}
		longest[k] = best + 1
		return best + 1
	}
	start, top := "", 0
	for _, k := range keys {
		if d := depth(k); d > top || (d == top && k < start) {
			start, top = k, d
		}
	}
	var path []string
	for cur := start; cur != ""; {
		path = append(path, cur)
		next := ""
		for _, s := range succ[cur] {
			if longest[s] == longest[cur]-1 && (next == "" || s < next) {
				next = s
			}
		}
		cur = next
	}
	return path
}

// waves groups the issues by the earliest wave each can start in: wave 0 waits for nothing.
func waves(keys []string, order []pair) [][]string {
	pred := map[string][]string{}
	for _, e := range order {
		pred[e.to] = append(pred[e.to], e.from)
	}
	level := map[string]int{}
	var of func(k string) int
	of = func(k string) int {
		if n, ok := level[k]; ok {
			return n
		}
		n := 0
		for _, p := range pred[k] {
			if l := of(p) + 1; l > n {
				n = l
			}
		}
		level[k] = n
		return n
	}
	var out [][]string
	for _, k := range keys {
		l := of(k)
		for len(out) <= l {
			out = append(out, []string{})
		}
		out[l] = append(out[l], k)
	}
	return out
}

// --- output

// Marshal is the result as the bytes `crw flow compile` prints: indented JSON, a trailing newline.
func Marshal(res *Result) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(res); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// BaseFromLog reads the plan a dag-plan-log/1 document (the whole log, from revision 1, as `crw relay dag-plan-log` prints it) folds
// to, history included: the retired ids a recompile must not use again.
func BaseFromLog(raw []byte, projectKey string) (dag.State, error) {
	var doc struct {
		Schema string `json:"schema"`
		PlanID string `json:"plan_id"`
		After  int64  `json:"after"`
		Head   int64  `json:"head_revision_no"`
		Events []struct {
			RevisionNo       int64           `json:"revision_no"`
			ParentRevisionNo int64           `json:"parent_revision_no"`
			RequestID        string          `json:"request_id"`
			AuthorTaskID     string          `json:"author_task_id"`
			CoordinatorEpoch int64           `json:"coordinator_epoch"`
			Changes          json.RawMessage `json:"changes"`
		} `json:"events"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return dag.State{}, err
	}
	switch {
	case doc.Schema != "dag-plan-log/1":
		return dag.State{}, fmt.Errorf("the document is not a dag-plan-log/1 (schema %q)", doc.Schema)
	case projectKey == "":
		return dag.State{}, errors.New("the project key of the plan is needed")
	case doc.After != 0:
		return dag.State{}, fmt.Errorf("the log starts after revision %d: a base is the whole log, from revision 1 (--after 0)", doc.After)
	case int64(len(doc.Events)) != doc.Head:
		return dag.State{}, fmt.Errorf("the log holds %d events of the %d revisions the plan has: read the whole log (every page)", len(doc.Events), doc.Head)
	}
	state := dag.State{PlanID: doc.PlanID}
	for i, ev := range doc.Events {
		if ev.RevisionNo != int64(i+1) || ev.ParentRevisionNo != int64(i) {
			return dag.State{}, fmt.Errorf("event %d is revision %d (parent %d): the log is missing, repeats or reorders an event", i+1, ev.RevisionNo, ev.ParentRevisionNo)
		}
		changes, err := dag.DecodeChanges(string(ev.Changes))
		if err != nil {
			return dag.State{}, fmt.Errorf("event %d: %w", ev.RevisionNo, err)
		}
		next, _, vs := dag.Apply(state, dag.Revision{PlanID: doc.PlanID, ProjectKey: projectKey, RequestID: ev.RequestID, ExpectedParent: ev.ParentRevisionNo,
			CoordinatorEpoch: ev.CoordinatorEpoch, AuthorTaskID: ev.AuthorTaskID, Changes: changes})
		if len(vs) > 0 {
			return dag.State{}, fmt.Errorf("event %d does not apply: %s", ev.RevisionNo, vs[0])
		}
		state = next
	}
	return state, nil
}
