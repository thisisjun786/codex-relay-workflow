package compile

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dag"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
)

const fixtureDir = "../../../testdata/flow/compile/"

func loadFixture(t testing.TB) Input {
	t.Helper()
	raw, err := os.ReadFile(fixtureDir + "initiative.input.json")
	if err != nil {
		t.Fatal(err)
	}
	in, err := Decode(raw)
	if err != nil {
		t.Fatal(err)
	}
	return in
}

// fold applies a plan's revision to base the way dag-plan-put does and fails the test on a violation.
func fold(t testing.TB, base dag.State, plan Plan) dag.State {
	t.Helper()
	if plan.Change == nil {
		t.Fatalf("plan %s has no revision (status %s)", plan.PlanID, plan.Status)
	}
	if base.PlanID == "" {
		base.PlanID = plan.PlanID
	}
	next, _, vs := dag.Apply(base, *plan.Change)
	if len(vs) > 0 {
		t.Fatalf("dag refuses the revision of %s: %v", plan.PlanID, vs)
	}
	return next
}

func compileOne(t testing.TB, in Input, bases map[string]dag.State) Plan {
	t.Helper()
	res, err := Compile(in, Options{Bases: bases})
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if len(res.Plans) != 1 {
		t.Fatalf("%d plans", len(res.Plans))
	}
	return res.Plans[0]
}

func mustRefuse(t testing.TB, in Input, bases map[string]dag.State) *Refused {
	t.Helper()
	res, err := Compile(in, Options{Bases: bases})
	var refused *Refused
	if !errors.As(err, &refused) {
		t.Fatalf("want a refusal, got result %+v, error %v", res, err)
	}
	if res != nil {
		t.Errorf("a refused handoff returned a result")
	}
	return refused
}

// mini handoffs --------------------------------------------------------------------------------------------

var target = &Target{Repository: "acme/shop", BaseRef: "refs/heads/dev"}

func iss(key string, with ...func(*Issue)) Issue {
	i := Issue{Key: key, Title: "issue " + key, Criteria: []string{"criterion of " + key}}
	for _, w := range with {
		w(&i)
	}
	return i
}

func after(keys ...string) func(*Issue) { return func(i *Issue) { i.After = keys } }
func group(name string) func(*Issue)    { return func(i *Issue) { i.Group = name } }
func regions(rs ...string) func(*Issue) {
	return func(i *Issue) {
		for _, r := range rs {
			var rg Region
			if err := rg.UnmarshalJSON([]byte(fmt.Sprintf("%q", r))); err != nil {
				panic(err)
			}
			i.Regions = append(i.Regions, rg)
		}
	}
}
func title(s string) func(*Issue)       { return func(i *Issue) { i.Title = s } }
func criteria(c ...string) func(*Issue) { return func(i *Issue) { i.Criteria = c } }
func nonPR() func(*Issue)               { return func(i *Issue) { i.Kind = "non_pr" } }
func sizes(fit, reason, answer, bundle string) func(*Issue) {
	return func(i *Issue) { i.SizeFit, i.SizeReason, i.IssueSize, i.BundleReason = fit, reason, answer, bundle }
}

func project(issues ...Issue) Input {
	return Input{Schema: InputSchema, Projects: []Project{{Key: "PX", PlanID: "plan-px", Target: target, Issues: issues}}}
}

func changeOps(p Plan) map[string][]string {
	out := map[string][]string{}
	for _, c := range p.Change.Changes {
		id := c.NodeID
		if c.Node != nil {
			id = c.Node.NodeID
		}
		if c.Edge != nil {
			id = c.Edge.EdgeID
		}
		if c.EdgeID != "" {
			id = c.EdgeID
		}
		out[c.Op] = append(out[c.Op], id)
	}
	return out
}

func edgesOf(p Plan) map[[2]string]dag.Edge {
	out := map[[2]string]dag.Edge{}
	for _, c := range p.Change.Changes {
		if c.Op == dag.OpAddEdge {
			out[[2]string{c.Edge.FromNodeID, c.Edge.ToNodeID}] = *c.Edge
		}
	}
	return out
}

// TestCompileTargetFormWarnsAndNormalizes (CRW-1317): a target repository that is not an absolute path (OWNER/NAME) is a warning,
// because a run integrates on a local checkout; a base_ref with the refs/heads/ prefix is the short branch name in the plan's edges,
// with a warning, because the integration path adds the prefix itself. Neither refuses the handoff; the run-ready form warns nothing.
func TestCompileTargetFormWarnsAndNormalizes(t *testing.T) {
	in := project(iss("A"), iss("B", after("A")))
	p := compileOne(t, in, nil)
	rules := map[string]Warning{}
	for _, w := range p.Warnings {
		rules[w.Rule] = w
	}
	if w := rules["target_repository"]; len(p.Warnings) != 2 || w.Project != "PX" || w.Path != "target.repository" || !strings.Contains(w.Detail, `"acme/shop"`) || !strings.Contains(w.Detail, "absolute") {
		t.Errorf("an OWNER/NAME repository: %+v", p.Warnings)
	}
	if w := rules["target_base_ref"]; w.Path != "target.base_ref" || !strings.Contains(w.Detail, `"refs/heads/dev"`) || !strings.Contains(w.Detail, `"dev"`) {
		t.Errorf("a refs/heads/ base_ref: %+v", p.Warnings)
	}
	if e := edgesOf(p)[[2]string{"A", "B"}]; e.TargetBaseRef != "dev" || e.TargetRepository != "acme/shop" {
		t.Errorf("the edge: %+v", e)
	}
	if target.BaseRef != "refs/heads/dev" {
		t.Errorf("compile changed its input: %+v", target)
	}

	ready := Input{Schema: InputSchema, Projects: []Project{{Key: "PX", PlanID: "plan-px", Target: &Target{Repository: "/srv/acme/shop", BaseRef: "dev"}, Issues: []Issue{iss("A"), iss("B", after("A"))}}}}
	if p := compileOne(t, ready, nil); len(p.Warnings) != 0 || edgesOf(p)[[2]string{"A", "B"}].TargetBaseRef != "dev" {
		t.Errorf("the run-ready target: %+v", p.Warnings)
	}

	// the command prints the warnings on stderr and compiles
	raw, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	code, out, errOut := runCLI(t, string(raw), "-")
	if code != 0 || !strings.Contains(out, `"warnings"`) || !strings.Contains(errOut, "crw flow compile: warning: [target_repository] PX target.repository: ") || !strings.Contains(errOut, "crw flow compile: warning: [target_base_ref] PX target.base_ref: ") {
		t.Errorf("exit %d\n%s\n%s", code, errOut, out)
	}
}

// TestCompileGoldenInitiativeFixture compiles an initiative of four projects and twenty issues, as a
// planner's handoff holds it, to the plan revisions dag-plan-put takes. The bytes are held in a golden file;
// CRW_UPDATE_GOLDEN=1 rewrites it.
func TestCompileGoldenInitiativeFixture(t *testing.T) {
	in := loadFixture(t)
	res, err := Compile(in, Options{})
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	got, err := Marshal(res)
	if err != nil {
		t.Fatal(err)
	}
	golden := fixtureDir + "initiative.golden.json"
	if os.Getenv("CRW_UPDATE_GOLDEN") == "1" {
		if err := os.WriteFile(golden, got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Errorf("the compiled initiative differs from %s (CRW_UPDATE_GOLDEN=1 rewrites it)\n%s", golden, firstDifference(string(want), string(got)))
	}

	// the same handoff compiles to the same bytes
	again, err := Compile(loadFixture(t), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := Marshal(again); string(b) != string(got) {
		t.Errorf("a second compile of the same handoff printed other bytes")
	}

	if len(res.Plans) != 4 {
		t.Fatalf("%d plans, want 4", len(res.Plans))
	}
	issues := 0
	for i, plan := range res.Plans {
		issues += len(in.Projects[i].Issues)
		if plan.Status != StatusChanges || plan.Change == nil || len(plan.Revision) == 0 {
			t.Fatalf("plan %s: status %q", plan.PlanID, plan.Status)
		}
		// the document is what dag-plan-put reads, and the plan it makes has one node per issue
		rev, err := dag.DecodeRevision(plan.Revision)
		if err != nil {
			t.Fatalf("plan %s: dag-plan-put would refuse the document: %v", plan.PlanID, err)
		}
		state := fold(t, dag.State{PlanID: plan.PlanID}, Plan{PlanID: plan.PlanID, Change: &rev})
		if live := state.At(state.Revision); len(live.Nodes) != len(in.Projects[i].Issues) {
			t.Errorf("plan %s: %d nodes for %d issues", plan.PlanID, len(live.Nodes), len(in.Projects[i].Issues))
		}
		if plan.Preview.Changes.AddNode != len(in.Projects[i].Issues) || plan.Preview.Changes.UpdateNode+plan.Preview.Changes.RetireNode != 0 {
			t.Errorf("plan %s: changes %+v", plan.PlanID, plan.Preview.Changes)
		}
	}
	if issues != 20 {
		t.Fatalf("the fixture holds %d issues", issues)
	}

	papi, pweb := res.Plans[0].Preview, res.Plans[1].Preview
	if got := fmt.Sprint(papi.Groups); got != "[{g-cart [API-2 API-3 API-4]}]" {
		t.Errorf("groups %s", got)
	}
	if len(pweb.SizeUnreviewed) != 1 || pweb.SizeUnreviewed[0] != "WEB-4" || len(papi.SizeUnreviewed) != 0 {
		t.Errorf("size_unreviewed: %v / %v", papi.SizeUnreviewed, pweb.SizeUnreviewed)
	}
	if len(pweb.ExternalPrereqs) != 1 || pweb.ExternalPrereqs[0] != (ExternalAfter{Issue: "WEB-2", After: "API-3", Project: "PAPI"}) {
		t.Errorf("external prerequisites %v", pweb.ExternalPrereqs)
	}
	if papi.ParallelWidth < 2 || papi.CriticalPathLen < 4 {
		t.Errorf("shape: width %d, critical path %v", papi.ParallelWidth, papi.CriticalPath)
	}

	// compile makes no date: the only dates in the output are the ones the handoff carried
	date := regexp.MustCompile(`\d{4}-\d{2}-\d{2}`)
	raw, _ := os.ReadFile(fixtureDir + "initiative.input.json")
	if n, m := len(date.FindAllString(string(got), -1)), len(date.FindAllString(string(raw), -1)); n != m {
		t.Errorf("the output holds %d dates, the handoff %d", n, m)
	}
	for _, banned := range []string{"recorded_at", "created", "deadline", "due"} {
		if strings.Contains(string(got), banned) {
			t.Errorf("the output mentions %q", banned)
		}
	}

	// a dry run shows the same previews and no revision document
	dry, err := Compile(loadFixture(t), Options{DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	for i := range dry.Plans {
		if len(dry.Plans[i].Revision) != 0 {
			t.Errorf("dry run printed the revision of %s", dry.Plans[i].PlanID)
		}
		a, _ := json.Marshal(dry.Plans[i].Preview)
		b, _ := json.Marshal(res.Plans[i].Preview)
		if string(a) != string(b) {
			t.Errorf("dry run preview of %s differs", dry.Plans[i].PlanID)
		}
	}
}

func firstDifference(want, got string) string {
	w, g := strings.Split(want, "\n"), strings.Split(got, "\n")
	for i := 0; i < len(w) || i < len(g); i++ {
		var a, b string
		if i < len(w) {
			a = w[i]
		}
		if i < len(g) {
			b = g[i]
		}
		if a != b {
			return fmt.Sprintf("line %d\n  want: %s\n  got:  %s", i+1, a, b)
		}
	}
	return ""
}

// TestRecompileUsesUpdateNode: a changed issue is an update_node, a new one an add_node, a missing one a retire_node
// (with the retire_edge of its edges); an issue that did not change is no change at all.
func TestRecompileUsesUpdateNode(t *testing.T) {
	v1 := project(iss("A"), iss("B", after("A")), iss("C", after("B")), iss("D"), iss("F", after("D")))
	p1 := compileOne(t, v1, nil)
	state1 := fold(t, dag.State{PlanID: "plan-px"}, p1)
	bases := map[string]dag.State{"plan-px": state1}

	// unchanged against its own plan
	same := compileOne(t, v1, bases)
	if same.Status != StatusUnchanged || same.Change != nil || len(same.Revision) != 0 || same.BaseRevision != 1 {
		t.Fatalf("recompile of the same handoff: %+v", same)
	}

	// D is gone, and F no longer waits for it
	v2 := project(iss("A"), iss("B", after("A"), title("issue B, retitled")), iss("C", after("B"), criteria("a new criterion")), iss("E", after("A")), iss("F"))
	p2 := compileOne(t, v2, bases)
	if p2.Status != StatusChanges || p2.Change.ExpectedParent != 1 || p2.BaseRevision != 1 {
		t.Fatalf("status %s, expected parent %d", p2.Status, p2.Change.ExpectedParent)
	}
	ops := changeOps(p2)
	want := map[string][]string{
		dag.OpUpdateNode: {"B", "C"},
		dag.OpAddNode:    {"E"},
		dag.OpRetireNode: {"D"},
		dag.OpAddEdge:    {"e:A:E"},
		dag.OpRetireEdge: {"e:D:F"},
	}
	if fmt.Sprint(ops) != fmt.Sprint(want) {
		t.Errorf("changes %v, want %v", ops, want)
	}
	if _, replaced := ops[dag.OpReplaceNode]; replaced {
		t.Error("a recompile used replace_node")
	}
	if c := p2.Preview.Changes; c != (ChangeCounts{AddNode: 1, UpdateNode: 2, RetireNode: 1, AddEdge: 1, RetireEdge: 1}) {
		t.Errorf("counts %+v", c)
	}
	state2 := fold(t, state1, p2)
	live := state2.At(state2.Revision)
	var ids []string
	for _, n := range live.Nodes {
		ids = append(ids, n.NodeID)
	}
	if fmt.Sprint(ids) != "[A B C E F]" {
		t.Errorf("live nodes %v", ids)
	}
	for _, n := range live.Nodes {
		if n.NodeID == "B" && n.Title != "issue B, retitled" {
			t.Errorf("B title %q", n.Title)
		}
	}
	if again := compileOne(t, v2, map[string]dag.State{"plan-px": state2}); again.Status != StatusUnchanged {
		t.Errorf("recompile after the update: %s", again.Status)
	}
	if p2.Preview.RetiredIssues[0] != "D" {
		t.Errorf("retired %v", p2.Preview.RetiredIssues)
	}
}

// TestRetiredIdNeverReused: an issue that left the plan and comes back gets a new node id, and an edge that was
// retired comes back under a new edge id; no revision ever introduces an id the plan used before.
func TestRetiredIdNeverReused(t *testing.T) {
	v1 := project(iss("A"), iss("B", after("A")))
	v2 := project(iss("A"))
	v3 := project(iss("A"), iss("B", after("A")))

	p1 := compileOne(t, v1, nil)
	s1 := fold(t, dag.State{PlanID: "plan-px"}, p1)
	p2 := compileOne(t, v2, map[string]dag.State{"plan-px": s1})
	if ops := changeOps(p2); fmt.Sprint(ops) != "map[retire_edge:[e:A:B] retire_node:[B]]" {
		t.Fatalf("v2 changes %v", ops)
	}
	s2 := fold(t, s1, p2)
	p3 := compileOne(t, v3, map[string]dag.State{"plan-px": s2})
	s3 := fold(t, s2, p3)

	var back *dag.Node
	for _, c := range p3.Change.Changes {
		if c.Op == dag.OpAddNode {
			back = c.Node
		}
		if c.Op == dag.OpReplaceNode || c.Op == dag.OpUpdateNode {
			t.Errorf("a returning issue used %s", c.Op)
		}
	}
	if back == nil || back.NodeID == "B" || back.IssueKey != "B" {
		t.Fatalf("the issue that came back: %+v", back)
	}
	if got := edgesOf(p3); len(got) != 1 || got[[2]string{"A", back.NodeID}].EdgeID == "e:A:B" || got[[2]string{"A", back.NodeID}].EdgeID == "" {
		t.Errorf("edges of the return %+v", got)
	}

	// the node ids and edge ids every revision introduced are all different, and never one that had been retired
	seenNode, seenEdge := map[string]bool{}, map[string]bool{}
	for _, p := range []Plan{p1, p2, p3} {
		for _, c := range p.Change.Changes {
			switch c.Op {
			case dag.OpAddNode:
				if seenNode[c.Node.NodeID] {
					t.Errorf("node id %s introduced twice", c.Node.NodeID)
				}
				seenNode[c.Node.NodeID] = true
			case dag.OpAddEdge:
				if seenEdge[c.Edge.EdgeID] {
					t.Errorf("edge id %s introduced twice", c.Edge.EdgeID)
				}
				seenEdge[c.Edge.EdgeID] = true
			}
		}
	}
	if again := compileOne(t, v3, map[string]dag.State{"plan-px": s3}); again.Status != StatusUnchanged {
		t.Errorf("recompile of the returned plan: %s", again.Status)
	}

	// what compile avoids is what the DAG refuses: the old id back is a duplicate_node_id
	reuse := dag.Revision{PlanID: "plan-px", ProjectKey: "PX", RequestID: "reuse", ExpectedParent: s2.Revision, AuthorTaskID: "t", Changes: []dag.Change{{Op: dag.OpAddNode, Node: &dag.Node{
		NodeID: "B", IssueKey: "B", Kind: dag.NodeImplementation, Title: "b", CriteriaSetDigest: strings.Repeat("a", 64)}}}}
	if _, _, vs := dag.Apply(s2, reuse); len(vs) == 0 || vs[0].Rule != dag.RuleDuplicateNodeID {
		t.Errorf("the DAG accepted a reused id: %v", vs)
	}

	// an edge retired while both nodes stay: the pair comes back under a new edge id
	w1 := project(iss("A"), iss("B", after("A")))
	w2 := project(iss("A"), iss("B"))
	w3 := project(iss("A"), iss("B", after("A")))
	q1 := compileOne(t, w1, nil)
	t1 := fold(t, dag.State{PlanID: "plan-px"}, q1)
	q2 := compileOne(t, w2, map[string]dag.State{"plan-px": t1})
	t2 := fold(t, t1, q2)
	q3 := compileOne(t, w3, map[string]dag.State{"plan-px": t2})
	fold(t, t2, q3)
	if e := edgesOf(q3)[[2]string{"A", "B"}]; e.EdgeID == "" || e.EdgeID == "e:A:B" {
		t.Errorf("the edge came back as %q", e.EdgeID)
	}
}

// TestOver64Refused: 64 issues are a plan, 65 are refused, and nothing is compiled for a refused handoff.
func TestOver64Refused(t *testing.T) {
	many := func(n int) Input {
		var issues []Issue
		for i := 0; i < n; i++ {
			issues = append(issues, iss(fmt.Sprintf("N-%02d", i), regions(fmt.Sprintf("pkg/p%02d/", i))))
		}
		return project(issues...)
	}
	p := compileOne(t, many(64), nil)
	if p.Preview.Nodes != 64 || p.Preview.ParallelWidth != 64 || p.Preview.CriticalPathLen != 1 {
		t.Errorf("64 issues: %+v", p.Preview)
	}
	fold(t, dag.State{PlanID: "plan-px"}, p)

	refused := mustRefuse(t, many(65), nil)
	if !refused.Has("over_64") || !strings.Contains(refused.Error(), "65") {
		t.Errorf("refusal %v", refused)
	}
	// 65 issues are also refused when a second project of the handoff is small and fine
	in := many(65)
	in.Projects = append(in.Projects, Project{Key: "PY", Target: target, Issues: []Issue{iss("Y-1")}})
	if r := mustRefuse(t, in, nil); !r.Has("over_64") || len(r.Refusals) != 1 {
		t.Errorf("refusals %v", r.Refusals)
	}
}

func TestCycleAndRegionOrderRefused(t *testing.T) {
	t.Run("a cycle of prerequisites", func(t *testing.T) {
		r := mustRefuse(t, project(iss("A", after("C")), iss("B", after("A")), iss("C", after("B"))), nil)
		if !r.Has("cycle") || !strings.Contains(r.Error(), "A -> B -> C -> A") && !strings.Contains(r.Error(), "A -> C -> B -> A") {
			t.Errorf("refusal %v", r)
		}
	})
	t.Run("a group order against a prerequisite is a cycle", func(t *testing.T) {
		r := mustRefuse(t, project(iss("A", group("g"), after("B")), iss("B", group("g"))), nil)
		if !r.Has("cycle") {
			t.Errorf("refusal %v", r)
		}
	})
	t.Run("two issues on one region with no order", func(t *testing.T) {
		r := mustRefuse(t, project(iss("A", regions("internal/x/")), iss("B", regions("internal/x/y.go")), iss("C", regions("internal/z/"))), nil)
		if !r.Has("region_order_missing") || len(r.Refusals) != 1 || !strings.Contains(r.Refusals[0].Detail, "A") || !strings.Contains(r.Refusals[0].Detail, "B") {
			t.Fatalf("refusals %+v", r.Refusals)
		}
	})
	orderedOK := map[string]Input{
		"a prerequisite orders them":             project(iss("A", regions("internal/x/")), iss("B", regions("internal/x/y.go"), after("A"))),
		"a group orders them":                    project(iss("A", regions("internal/x/"), group("g")), iss("B", regions("internal/x/y.go"), group("g"))),
		"a third issue orders them transitively": project(iss("A", regions("internal/x/")), iss("M", after("A")), iss("B", regions("internal/x/y.go"), after("M"))),
		"disjoint regions need no order":         project(iss("A", regions("internal/x/a.go")), iss("B", regions("internal/x/b.go"))),
		"two symbols of one file are apart": project(
			func() Issue {
				i := iss("A")
				i.Regions = []Region{{Path: "f.go", Kind: "symbol", Key: "One"}}
				return i
			}(),
			func() Issue {
				i := iss("B")
				i.Regions = []Region{{Path: "f.go", Kind: "symbol", Key: "Two"}}
				return i
			}()),
	}
	for name, in := range orderedOK {
		t.Run(name, func(t *testing.T) {
			compileOne(t, in, nil)
		})
	}
	t.Run("a whole-repository hold overlaps everything", func(t *testing.T) {
		a := iss("A")
		a.Regions = []Region{{Path: "go.mod", Kind: "file", Exclusive: true}}
		r := mustRefuse(t, project(a, iss("B", regions("internal/q/q.go"))), nil)
		if !r.Has("region_order_missing") {
			t.Errorf("refusal %v", r)
		}
	})
	t.Run("regions of issues in two projects are not compared", func(t *testing.T) {
		in := Input{Schema: InputSchema, Projects: []Project{
			{Key: "P1", Target: target, Issues: []Issue{iss("A", regions("internal/x/"))}},
			{Key: "P2", Target: target, Issues: []Issue{iss("B", regions("internal/x/"))}},
		}}
		if res, err := Compile(in, Options{}); err != nil || len(res.Plans) != 2 {
			t.Errorf("%v", err)
		}
	})
}

func TestGroupCapRefused(t *testing.T) {
	members := func(n int) Input {
		var issues []Issue
		for i := 0; i < n; i++ {
			issues = append(issues, iss(fmt.Sprintf("G-%d", i), group("g-big"), regions("internal/shared/")))
		}
		return project(issues...)
	}
	if p := compileOne(t, members(4), nil); len(p.Preview.Groups) != 1 || len(p.Preview.Groups[0].Order) != 4 {
		t.Errorf("a group of four: %+v", p.Preview.Groups)
	}
	r := mustRefuse(t, members(5), nil)
	if !r.Has("group_cap") || !strings.Contains(r.Error(), "g-big") || !strings.Contains(r.Error(), "5") {
		t.Errorf("refusal %v", r)
	}
	// the cap is per group: two groups of four are fine
	var issues []Issue
	for _, g := range []string{"ga", "gb"} {
		for i := 0; i < 4; i++ {
			issues = append(issues, iss(fmt.Sprintf("%s-%d", g, i), group(g), regions("internal/"+g+"/")))
		}
	}
	compileOne(t, project(issues...), nil)
}

// TestCompileShowsSizeUnreviewed: an issue that answers over_line with neither a bundle reason nor a split (`split` or
// `bundle_split`) is shown as size_unreviewed; it is not refused, and the size fields are carried as they were given. `child` is
// out of the closed set (a run starts no `child` session) and is refused like any made-up value.
func TestCompileShowsSizeUnreviewed(t *testing.T) {
	in := project(
		iss("U", sizes("executor", "fits_executor", "over_line", "")),
		iss("B", sizes("executor", "fits_executor", "over_line", "one concept")),
		iss("S", sizes("split", "fits_executor", "over_line", "")),
		iss("BS", sizes("bundle_split", "own_goal", "over_line", "")),
		iss("ACC", sizes("split", "regions_gt3", "over_line_accepted", "kept whole")),
		iss("OK", sizes("executor", "fits_executor", "ok", "")),
		iss("NONE"),
	)
	p := compileOne(t, in, nil) // not refused
	if p.Status != StatusChanges {
		t.Fatalf("status %s", p.Status)
	}
	if fmt.Sprint(p.Preview.SizeUnreviewed) != "[U]" {
		t.Errorf("size_unreviewed %v", p.Preview.SizeUnreviewed)
	}
	by := map[string]IssuePreview{}
	for _, i := range p.Preview.Issues {
		by[i.Key] = i
	}
	if !by["U"].SizeUnreviewed || by["B"].SizeUnreviewed || by["S"].SizeUnreviewed || by["BS"].SizeUnreviewed || by["ACC"].SizeUnreviewed || by["OK"].SizeUnreviewed || by["NONE"].SizeUnreviewed {
		t.Errorf("flags: %+v", by)
	}
	if a := by["ACC"]; a.SizeFit != "split" || a.SizeReason != "regions_gt3" || a.IssueSize != "over_line_accepted" || a.BundleReason != "kept whole" {
		t.Errorf("the size fields were not carried: %+v", a)
	}
	// an issue with no size fields is an executor that fits
	if n := by["NONE"]; n.SizeFit != "executor" || n.SizeReason != "fits_executor" || n.IssueSize != "" {
		t.Errorf("defaults: %+v", n)
	}
	// the closed sets are checked, so a made-up grade is not carried
	for name, bad := range map[string]func(*Issue){
		"size_fit":    func(i *Issue) { i.SizeFit = "L" },
		"size_reason": func(i *Issue) { i.SizeReason = "too_big" },
		"issue_size":  func(i *Issue) { i.IssueSize = "grade_M" },
	} {
		r := mustRefuse(t, project(iss("X", bad)), nil)
		if !r.Has("size_value") || !strings.Contains(r.Error(), name) {
			t.Errorf("%s: %v", name, r)
		}
	}
	// `child` left the closed set: a plan input that still sizes an issue `child` is refused, and the refusal names the set
	if r := mustRefuse(t, project(iss("X", sizes("child", "regions_gt3", "", ""))), nil); !r.Has("size_value") ||
		!strings.Contains(r.Error(), `size_fit "child" is not one of executor, split, bundle_split`) {
		t.Errorf("size_fit child: %v", r)
	}
}

// TestGroupBecomesOrderEdges: the members of a group, in the order declared, are chained by order edges; the group
// is no node, the members stay nodes of their own, and the edges are plain add_edge changes.
func TestGroupBecomesOrderEdges(t *testing.T) {
	in := project(
		iss("M3", group("g-x"), regions("internal/shared/")),
		iss("M1", group("g-x"), regions("internal/shared/")),
		iss("OTHER", regions("internal/other/")),
		iss("M2", group("g-x"), regions("internal/shared/"), after("M1")),
	)
	p := compileOne(t, in, nil)
	edges := edgesOf(p)
	if len(edges) != 2 {
		t.Fatalf("edges %v", edges)
	}
	for _, pair := range [][2]string{{"M3", "M1"}, {"M1", "M2"}} {
		e, ok := edges[pair]
		if !ok {
			t.Fatalf("no order edge %v in %v", pair, edges)
		}
		if e.Kind != dag.EdgeIntegrated || e.TargetRepository != "acme/shop" || e.TargetBaseRef != "dev" {
			t.Errorf("edge %+v", e)
		}
	}
	if _, ok := edges[[2]string{"M3", "M2"}]; ok {
		t.Error("an edge between members that are not neighbours")
	}
	// the declared M1 -> M2 prerequisite and the group's order are one edge, not two
	ops := changeOps(p)
	if len(ops[dag.OpAddNode]) != 4 || len(ops[dag.OpAddEdge]) != 2 {
		t.Errorf("changes %v", ops)
	}
	for _, id := range ops[dag.OpAddNode] {
		if id == "g-x" {
			t.Error("the group became a node")
		}
	}
	// each member is its own node; the preview shows the order the group runs in
	if g := p.Preview.Groups; len(g) != 1 || g[0].Group != "g-x" || fmt.Sprint(g[0].Order) != "[M3 M1 M2]" {
		t.Errorf("groups %+v", g)
	}
	if p.Preview.CriticalPathLen != 3 || fmt.Sprint(p.Preview.CriticalPath) != "[M3 M1 M2]" || p.Preview.ParallelWidth != 2 {
		t.Errorf("shape: %+v", p.Preview)
	}
	fold(t, dag.State{PlanID: "plan-px"}, p)

	// a non_pr member before an implementation member is a verified artifact, not an integration
	mixed := project(iss("D", group("g-y"), nonPR()), iss("I", group("g-y")))
	e := edgesOf(compileOne(t, mixed, nil))[[2]string{"D", "I"}]
	if e.Kind != dag.EdgeArtifactVerified || e.TargetRepository != "" || e.PinsCodeHead {
		t.Errorf("edge from a non_pr member: %+v", e)
	}
}

func TestInputIsStrict(t *testing.T) {
	good := `{"schema":"crw-flow-compile-input/1","projects":[{"key":"P","target":{"repository":"r","base_ref":"b"},"issues":[{"key":"A","title":"a","criteria":["c"]}]}]}`
	if _, err := Decode([]byte(good)); err != nil {
		t.Fatal(err)
	}
	for name, bad := range map[string]string{
		"unknown field":       strings.Replace(good, `"title":"a"`, `"title":"a","level":"L"`, 1),
		"trailing value":      good + `{}`,
		"region object field": strings.Replace(good, `"criteria":["c"]`, `"criteria":["c"],"regions":[{"path":"x","grade":1}]`, 1),
		"region of a number":  strings.Replace(good, `"criteria":["c"]`, `"criteria":["c"],"regions":[3]`, 1),
	} {
		if _, err := Decode([]byte(bad)); err == nil {
			t.Errorf("%s was read", name)
		}
	}
	for name, mutate := range map[string]func(*Input){
		"wrong schema":          func(in *Input) { in.Schema = "x" },
		"no projects":           func(in *Input) { in.Projects = nil },
		"duplicate key":         func(in *Input) { in.Projects[0].Issues = append(in.Projects[0].Issues, in.Projects[0].Issues[0]) },
		"unknown prerequisite":  func(in *Input) { in.Projects[0].Issues[0].After = []string{"NOPE"} },
		"self prerequisite":     func(in *Input) { in.Projects[0].Issues[0].After = []string{in.Projects[0].Issues[0].Key} },
		"bad kind":              func(in *Input) { in.Projects[0].Issues[0].Kind = "research" },
		"no criteria":           func(in *Input) { in.Projects[0].Issues[0].Criteria = nil },
		"region leaves repo":    func(in *Input) { in.Projects[0].Issues[0].Regions = []Region{{Path: "../x"}} },
		"two projects, one key": func(in *Input) { in.Projects = append(in.Projects, in.Projects[0]) },
	} {
		in := project(iss("A"))
		in.Projects[0].Key = "P"
		mutate(&in)
		var refused *Refused
		if _, err := Compile(in, Options{}); !errors.As(err, &refused) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestBaseFromLog(t *testing.T) {
	v1 := project(iss("A"), iss("B", after("A")))
	p1 := compileOne(t, v1, nil)
	s1 := fold(t, dag.State{PlanID: "plan-px"}, p1)
	p2 := compileOne(t, project(iss("A")), map[string]dag.State{"plan-px": s1})
	s2 := fold(t, s1, p2)

	log := func(revs ...*dag.Revision) []byte {
		var events []any
		for i, r := range revs {
			var changes any
			_ = json.Unmarshal([]byte(dag.ChangesJSON(r.Changes)), &changes)
			events = append(events, map[string]any{"revision_no": i + 1, "parent_revision_no": i, "request_id": r.RequestID, "changes": changes})
		}
		raw, _ := json.Marshal(map[string]any{"ok": true, "schema": "dag-plan-log/1", "plan_id": "plan-px", "after": 0, "head_revision_no": len(revs), "cursor": len(revs), "events": events})
		return raw
	}
	base, err := BaseFromLog(log(p1.Change, p2.Change), "PX")
	if err != nil {
		t.Fatal(err)
	}
	if base.Revision != 2 || base.At(2).StateDigest != s2.At(2).StateDigest {
		t.Errorf("revision %d, digest %s != %s", base.Revision, base.At(2).StateDigest, s2.At(2).StateDigest)
	}
	// the history is kept, so B's id is known to be used
	back := compileOne(t, v1, map[string]dag.State{"plan-px": base})
	for _, c := range back.Change.Changes {
		if c.Op == dag.OpAddNode && c.Node.NodeID == "B" {
			t.Error("the id of the retired B was introduced again")
		}
	}
	// a log that does not start at revision 1, or stops short of its head, is not a base
	var doc map[string]any
	_ = json.Unmarshal(log(p1.Change, p2.Change), &doc)
	doc["head_revision_no"] = 3
	short, _ := json.Marshal(doc)
	if _, err := BaseFromLog(short, "PX"); err == nil {
		t.Error("a log short of its head was read")
	}
	doc["head_revision_no"] = 2
	events := doc["events"].([]any)
	events[0].(map[string]any)["revision_no"] = 2
	late, _ := json.Marshal(doc)
	if _, err := BaseFromLog(late, "PX"); err == nil {
		t.Error("a log that does not start at revision 1 was read")
	}
}

// Verification fix round 1 (CRW-1203): the checks that hold across the projects of one handoff, a plan whose base holds two edges
// between one pair of nodes, and the root of the repository as a region.

func twoProjects(a, b []Issue) Input {
	return Input{Schema: InputSchema, Projects: []Project{
		{Key: "P1", Target: target, Issues: a},
		{Key: "P2", Target: target, Issues: b},
	}}
}

func TestCycleAcrossProjectsRefused(t *testing.T) {
	t.Run("two issues of two projects wait for each other", func(t *testing.T) {
		r := mustRefuse(t, twoProjects([]Issue{iss("A", after("B"))}, []Issue{iss("B", after("A"))}), nil)
		if !r.Has("cycle") || !strings.Contains(r.Error(), "A -> B -> A") {
			t.Fatalf("refusal %v", r)
		}
	})
	t.Run("a cycle through three projects and a group", func(t *testing.T) {
		in := Input{Schema: InputSchema, Projects: []Project{
			{Key: "P1", Target: target, Issues: []Issue{iss("A", after("C")), iss("A2")}},
			{Key: "P2", Target: target, Issues: []Issue{iss("B", after("A"))}},
			{Key: "P3", Target: target, Issues: []Issue{iss("C", after("B"))}},
		}}
		if r := mustRefuse(t, in, nil); !r.Has("cycle") {
			t.Fatalf("refusal %v", r)
		}
	})
	t.Run("a handoff across projects without a cycle compiles", func(t *testing.T) {
		res, err := Compile(twoProjects([]Issue{iss("A")}, []Issue{iss("B", after("A"))}), Options{})
		if err != nil || len(res.Plans) != 2 {
			t.Fatalf("%v", err)
		}
	})
	t.Run("a cycle inside one project is refused once", func(t *testing.T) {
		r := mustRefuse(t, twoProjects([]Issue{iss("A", after("B")), iss("B", after("A"))}, []Issue{iss("C")}), nil)
		n := 0
		for _, f := range r.Refusals {
			if f.Rule == "cycle" {
				n++
			}
		}
		if n != 1 {
			t.Fatalf("%d cycle refusals: %v", n, r)
		}
	})
}

func TestIssueKeyInTwoProjectsRefused(t *testing.T) {
	r := mustRefuse(t, twoProjects([]Issue{iss("X")}, []Issue{iss("X"), iss("Y")}), nil)
	if !r.Has("input") || !strings.Contains(r.Error(), "X") || !strings.Contains(r.Error(), "P1") {
		t.Fatalf("refusal %v", r)
	}
	// distinct plan ids do not make the key two issues
	in := twoProjects([]Issue{iss("X")}, []Issue{iss("X")})
	in.Projects[0].PlanID, in.Projects[1].PlanID = "plan-1", "plan-2"
	if r := mustRefuse(t, in, nil); !r.Has("input") {
		t.Fatalf("refusal %v", r)
	}
}

func TestBaseWithTwoEdgesOfOnePair(t *testing.T) {
	node := func(id string) *dag.Node {
		return &dag.Node{NodeID: id, IssueKey: id, Kind: dag.NodeImplementation, Title: "issue " + id, CriteriaSetDigest: criteriaDigest([]string{"criterion of " + id})}
	}
	edge := func(id string) *dag.Edge {
		// the edge as compile writes it: the target's branch is its short name (CRW-1317)
		return &dag.Edge{EdgeID: id, FromNodeID: "A", ToNodeID: "B", Kind: dag.EdgeIntegrated, TargetRepository: target.Repository, TargetBaseRef: target.Branch()}
	}
	seed := dag.Revision{PlanID: "plan-px", ProjectKey: "PX", RequestID: "seed", AuthorTaskID: "t", Changes: []dag.Change{
		{Op: dag.OpAddNode, Node: node("A")}, {Op: dag.OpAddNode, Node: node("B")},
		{Op: dag.OpAddEdge, Edge: edge("e:A:B")}, {Op: dag.OpAddEdge, Edge: edge("second-edge")},
	}}
	base, _, vs := dag.Apply(dag.State{PlanID: "plan-px"}, seed)
	if len(vs) > 0 {
		t.Skipf("the DAG does not take two edges of one pair (%v): nothing to prove", vs)
	}
	bases := map[string]dag.State{"plan-px": base}

	t.Run("an issue that left retires every edge it had", func(t *testing.T) {
		p := compileOne(t, project(iss("A")), bases)
		ops := changeOps(p)
		if fmt.Sprint(ops["retire_edge"]) != "[e:A:B second-edge]" || fmt.Sprint(ops["retire_node"]) != "[B]" {
			t.Fatalf("changes %v", ops)
		}
		fold(t, base, p)
	})
	t.Run("the edge the handoff still wants is kept once, the other retired", func(t *testing.T) {
		p := compileOne(t, project(iss("A"), iss("B", after("A"))), bases)
		ops := changeOps(p)
		if fmt.Sprint(ops["retire_edge"]) != "[second-edge]" || len(ops["add_edge"]) != 0 {
			t.Fatalf("changes %v", ops)
		}
		fold(t, base, p)
	})
}

func TestRegionOfTheRepositoryRootRefused(t *testing.T) {
	for _, r := range []string{"./", ".", "a/..", "x/../"} {
		in := project(iss("A", regions(r)))
		if res := mustRefuse(t, in, nil); !res.Has("input") || !strings.Contains(res.Error(), "region") {
			t.Errorf("%q: refusal %v", r, res)
		}
	}
	// the structured form too
	a := iss("A")
	a.Regions = []Region{{Path: ".", Kind: "tree"}}
	if res := mustRefuse(t, project(a), nil); !res.Has("input") {
		t.Errorf("refusal %v", res)
	}
}

// --- post-evaluation round (CRW-1203, pre-merge evaluation of ceb7a599)

// TestCriteriaDigestIsTheRelaysSetDigest: the digest a node carries for criteria:[string] is the one dag-release recomputes from the
// canonical criteria (delivery.SetDigest of {id, title, required}); ids are c1.. in the declared order.
func TestCriteriaDigestIsTheRelaysSetDigest(t *testing.T) {
	p := compileOne(t, project(iss("A", criteria("produce the report", "  review   it ", "ship it"))), nil)
	var got string
	for _, c := range p.Change.Changes {
		if c.Op == dag.OpAddNode {
			got = c.Node.CriteriaSetDigest
		}
	}
	want := delivery.SetDigest([]delivery.Criterion{
		{ID: "c1", Title: "produce the report", Required: true},
		{ID: "c2", Title: "review   it", Required: true},
		{ID: "c3", Title: "ship it", Required: true},
	})
	if got != want {
		t.Fatalf("criteria_set_digest %s, the relay recomputes %s", got, want)
	}
	// the order the criteria are listed in is the order of their ids
	if criteriaDigest([]string{"b", "a"}) == criteriaDigest([]string{"a", "b"}) {
		t.Errorf("ids follow the declared order, so a reordered list is another set")
	}
	// a blank criterion cannot be registered by the relay
	a := iss("A")
	a.Criteria = []string{"fine", "   "}
	if r := mustRefuse(t, project(a), nil); !r.Has("input") {
		t.Errorf("refusal %v", r)
	}
}

// TestCoordinatorEpochOfTheWritingSession: a project names the epoch its session holds, and the revision document carries it, so a claimed plan's fence admits the put.
func TestCoordinatorEpochOfTheWritingSession(t *testing.T) {
	in := project(iss("A"))
	in.AuthorTaskID = "parent-task"
	in.Projects[0].CoordinatorEpoch = 3
	p := compileOne(t, in, nil)
	if p.Change.CoordinatorEpoch != 3 {
		t.Fatalf("revision epoch %d", p.Change.CoordinatorEpoch)
	}
	var doc map[string]any
	if err := json.Unmarshal(p.Revision, &doc); err != nil {
		t.Fatal(err)
	}
	if doc["coordinator_epoch"] != float64(3) {
		t.Fatalf("document %s", p.Revision)
	}
	back, err := dag.DecodeRevision(p.Revision)
	if err != nil || back.CoordinatorEpoch != 3 || dag.RequestDigest(back) != dag.RequestDigest(*p.Change) {
		t.Fatalf("decoded %+v, %v", back, err)
	}
	// the request id follows the epoch: the same issues under another epoch are another request
	in.Projects[0].CoordinatorEpoch = 4
	if q := compileOne(t, in, nil); q.Change.RequestID == p.Change.RequestID {
		t.Errorf("request id %s repeats across epochs", q.Change.RequestID)
	}
	// an unclaimed plan names none
	plain := compileOne(t, project(iss("A")), nil)
	if strings.Contains(string(plain.Revision), "coordinator_epoch") {
		t.Errorf("document %s names an epoch nobody holds", plain.Revision)
	}
	// a negative epoch is not an epoch
	in.Projects[0].CoordinatorEpoch = -1
	if r := mustRefuse(t, in, nil); !r.Has("input") {
		t.Errorf("refusal %v", r)
	}
	// and the input reads it from the handoff
	raw := `{"schema":"crw-flow-compile-input/1","projects":[{"key":"P","coordinator_epoch":2,"target":{"repository":"r","base_ref":"b"},"issues":[{"key":"A","title":"a","criteria":["c"]}]}]}`
	if d, err := Decode([]byte(raw)); err != nil || d.Projects[0].CoordinatorEpoch != 2 {
		t.Fatalf("decode: %+v, %v", d, err)
	}
}

// TestRecompileKeepsDecisionEdges: an edge that carries an approval (kind decision) is not something the handoff can state, so a recompile never retires it
// while both of its nodes stay; a node that leaves still takes its edges with it.
func TestRecompileKeepsDecisionEdges(t *testing.T) {
	node := func(id, kind string) *dag.Node {
		return &dag.Node{NodeID: id, IssueKey: id, Kind: kind, Title: "issue " + id, CriteriaSetDigest: criteriaDigest([]string{"criterion of " + id})}
	}
	seed := dag.Revision{PlanID: "plan-px", ProjectKey: "PX", RequestID: "seed", AuthorTaskID: "t", Changes: []dag.Change{
		{Op: dag.OpAddNode, Node: node("A", dag.NodeNonPR)}, {Op: dag.OpAddNode, Node: node("B", dag.NodeImplementation)},
		{Op: dag.OpAddEdge, Edge: &dag.Edge{EdgeID: "e:A:B", FromNodeID: "A", ToNodeID: "B", Kind: dag.EdgeArtifactVerified}},
		{Op: dag.OpAddEdge, Edge: &dag.Edge{EdgeID: "gate", FromNodeID: "A", ToNodeID: "B", Kind: dag.EdgeDecision,
			DecisionSubject: "release", DecisionDigest: "d1", RequiredAuthority: []string{"owner"}}},
	}}
	base, _, vs := dag.Apply(dag.State{PlanID: "plan-px"}, seed)
	if len(vs) > 0 {
		t.Fatalf("seed refused: %v", vs)
	}
	bases := map[string]dag.State{"plan-px": base}
	liveGate := func(s dag.State) bool {
		for _, e := range s.Edges {
			if e.EdgeID == "gate" && e.RetiredRev == 0 {
				return true
			}
		}
		return false
	}
	a := func() Issue { return iss("A", nonPR()) }

	t.Run("the order edge is stated, the gate is not touched", func(t *testing.T) {
		p := compileOne(t, project(a(), iss("B", after("A"))), bases)
		if p.Status != StatusUnchanged {
			t.Fatalf("status %s, changes %v", p.Status, changeOps(p))
		}
	})
	t.Run("the prerequisite is dropped: only the order edge goes", func(t *testing.T) {
		p := compileOne(t, project(a(), iss("B")), bases)
		if ops := changeOps(p); fmt.Sprint(ops["retire_edge"]) != "[e:A:B]" {
			t.Fatalf("changes %v", ops)
		}
		if !liveGate(fold(t, base, p)) {
			t.Errorf("the decision edge was retired")
		}
	})
	t.Run("another change of the node keeps the gate", func(t *testing.T) {
		p := compileOne(t, project(a(), iss("B", after("A"), title("issue B, retitled"))), bases)
		if ops := changeOps(p); len(ops["retire_edge"]) != 0 {
			t.Fatalf("changes %v", ops)
		}
		if !liveGate(fold(t, base, p)) {
			t.Errorf("the decision edge was retired")
		}
	})
	t.Run("a node that leaves takes its gate with it", func(t *testing.T) {
		p := compileOne(t, project(a()), bases)
		if ops := changeOps(p); fmt.Sprint(ops["retire_edge"]) != "[e:A:B gate]" || fmt.Sprint(ops["retire_node"]) != "[B]" {
			t.Fatalf("changes %v", ops)
		}
		if liveGate(fold(t, base, p)) {
			t.Errorf("the decision edge stays on a retired node")
		}
	})
	t.Run("the preview counts the gates it kept", func(t *testing.T) {
		p := compileOne(t, project(a(), iss("B")), bases)
		if p.Preview.Changes.KeepDecisionEdge != 1 {
			t.Fatalf("preview %+v", p.Preview.Changes)
		}
	})
}

// TestPreviewShowsTheKeptDecisionEdges: the preview is the plan the revision leaves, so a decision edge that stays orders its two issues
// in the width, the critical path, the waves and the edge count just as it does once the revision is applied.
func TestPreviewShowsTheKeptDecisionEdges(t *testing.T) {
	node := func(id, kind string) *dag.Node {
		return &dag.Node{NodeID: id, IssueKey: id, Kind: kind, Title: "issue " + id, CriteriaSetDigest: criteriaDigest([]string{"criterion of " + id})}
	}
	seed := dag.Revision{PlanID: "plan-px", ProjectKey: "PX", RequestID: "seed", AuthorTaskID: "t", Changes: []dag.Change{
		{Op: dag.OpAddNode, Node: node("A", dag.NodeNonPR)}, {Op: dag.OpAddNode, Node: node("B", dag.NodeNonPR)},
		{Op: dag.OpAddEdge, Edge: &dag.Edge{EdgeID: "gate", FromNodeID: "A", ToNodeID: "B", Kind: dag.EdgeDecision,
			DecisionSubject: "release", DecisionDigest: "d1", RequiredAuthority: []string{"owner"}}},
	}}
	base, _, vs := dag.Apply(dag.State{PlanID: "plan-px"}, seed)
	if len(vs) > 0 {
		t.Fatalf("seed refused: %v", vs)
	}
	bases := map[string]dag.State{"plan-px": base}
	check := func(t *testing.T, p Plan) {
		t.Helper()
		pv := p.Preview
		if pv.Edges != 1 || pv.ParallelWidth != 1 || fmt.Sprint(pv.CriticalPath) != "[A B]" || pv.CriticalPathLen != 2 || fmt.Sprint(pv.Waves) != "[[A] [B]]" {
			t.Errorf("preview edges %d, width %d, critical path %v (%d), waves %v", pv.Edges, pv.ParallelWidth, pv.CriticalPath, pv.CriticalPathLen, pv.Waves)
		}
	}
	t.Run("unchanged", func(t *testing.T) {
		p := compileOne(t, project(iss("A", nonPR()), iss("B", nonPR())), bases)
		if p.Status != StatusUnchanged {
			t.Fatalf("status %s, changes %v", p.Status, changeOps(p))
		}
		check(t, p)
	})
	t.Run("a retitle", func(t *testing.T) {
		p := compileOne(t, project(iss("A", nonPR()), iss("B", nonPR(), title("issue B, retitled"))), bases)
		if ops := changeOps(p); len(ops["retire_edge"]) != 0 || len(ops["update_node"]) != 1 {
			t.Fatalf("changes %v", ops)
		}
		check(t, p)
		if next := fold(t, base, p); len(next.Edges) != 1 {
			t.Errorf("the plan has %d edges", len(next.Edges))
		}
	})
	t.Run("an order edge on the same pair is one ordering", func(t *testing.T) {
		p := compileOne(t, project(iss("A", nonPR()), iss("B", nonPR(), after("A"))), bases)
		if pv := p.Preview; pv.Edges != 2 || pv.ParallelWidth != 1 || fmt.Sprint(pv.Waves) != "[[A] [B]]" || fmt.Sprint(pv.CriticalPath) != "[A B]" {
			t.Errorf("preview %+v", pv)
		}
	})
	t.Run("a third issue stays beside the gate", func(t *testing.T) {
		p := compileOne(t, project(iss("A", nonPR()), iss("B", nonPR()), iss("C", nonPR())), bases)
		if pv := p.Preview; pv.Edges != 1 || pv.ParallelWidth != 2 || fmt.Sprint(pv.Waves) != "[[A C] [B]]" {
			t.Errorf("preview %+v", pv)
		}
	})
	t.Run("an order edge against the gate is a cycle", func(t *testing.T) {
		r := mustRefuse(t, project(iss("A", nonPR(), after("B")), iss("B", nonPR())), bases)
		if !r.Has("cycle") {
			t.Errorf("refusal %v", r)
		}
	})
}

// bundleHandoff is a handoff JSON whose target is written the way crw-plan writes it for a bundle: the branch the run integrates on
// (base_ref) and, when given, the branch the bundle's pull request is opened against (pr_base).
func bundleHandoff(baseRef, prBase string) string {
	pr := ""
	if prBase != "" {
		pr = fmt.Sprintf(`,"pr_base":%q`, prBase)
	}
	return fmt.Sprintf(`{"schema":%q,"projects":[{"key":"PX","plan_id":"plan-px","target":{"repository":"/srv/acme/shop","base_ref":%q%s},`+
		`"issues":[{"key":"A","title":"issue A","criteria":["criterion of A"]},{"key":"B","title":"issue B","criteria":["criterion of B"],"after":["A"]}]}]}`,
		InputSchema, baseRef, pr)
}

// TestBundleTargetEqualToThePRBaseIsRefused (CRW-1440): a bundle's run integrates on its own branch and the pull request is opened
// against pr_base; a base_ref that is the pr_base (written short or as refs/heads/BRANCH) would integrate the nodes straight on the
// branch the pull request targets, so the handoff is refused at target.base_ref. A distinct pr_base is no edge and no digest change,
// and the handoff that carries pr_base decodes strictly.
func TestBundleTargetEqualToThePRBaseIsRefused(t *testing.T) {
	for _, baseRef := range []string{"dev", "refs/heads/dev"} {
		in, err := Decode([]byte(bundleHandoff(baseRef, "dev")))
		if err != nil {
			t.Fatalf("base_ref %q with pr_base: the handoff does not decode: %v", baseRef, err)
		}
		r := mustRefuse(t, in, nil)
		if !r.Has("input") || len(r.Refusals) != 1 || r.Refusals[0].Path != "target.base_ref" || r.Refusals[0].Project != "PX" ||
			!strings.Contains(r.Refusals[0].Detail, "pr_base") {
			t.Errorf("base_ref %q equal to pr_base: refusals %+v", baseRef, r.Refusals)
		}
	}
	// a pr_base written refs/heads/BRANCH is the same branch
	in, err := Decode([]byte(bundleHandoff("dev", "refs/heads/dev")))
	if err != nil {
		t.Fatal(err)
	}
	if r := mustRefuse(t, in, nil); !r.Has("input") || len(r.Refusals) != 1 || r.Refusals[0].Path != "target.base_ref" {
		t.Errorf("pr_base refs/heads/dev: refusals %+v", r.Refusals)
	}

	// a bundle branch distinct from pr_base compiles, and pr_base changes no edge and no digest
	with, err := Decode([]byte(bundleHandoff("bundle/m1", "dev")))
	if err != nil {
		t.Fatal(err)
	}
	if with.Projects[0].Target.PRBase != "dev" {
		t.Fatalf("pr_base not read: %+v", with.Projects[0].Target)
	}
	without, err := Decode([]byte(bundleHandoff("bundle/m1", "")))
	if err != nil {
		t.Fatal(err)
	}
	a, b := compileOne(t, with, nil), compileOne(t, without, nil)
	if len(a.Warnings) != 0 || len(b.Warnings) != 0 {
		t.Errorf("warnings: %+v / %+v", a.Warnings, b.Warnings)
	}
	if da, db := dag.RequestDigest(*a.Change), dag.RequestDigest(*b.Change); da != db || a.Change.RequestID != b.Change.RequestID {
		t.Errorf("pr_base changed the plan digest: %s != %s", da, db)
	}
	ea, eb := edgesOf(a), edgesOf(b)
	if len(ea) == 0 || len(ea) != len(eb) {
		t.Fatalf("edges %v / %v", ea, eb)
	}
	for k, e := range ea {
		if !reflect.DeepEqual(eb[k], e) || e.TargetBaseRef != "bundle/m1" {
			t.Errorf("edge %v: %+v != %+v", k, e, eb[k])
		}
	}
}
