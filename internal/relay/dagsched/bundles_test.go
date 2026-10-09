package dagsched

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"slices"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dag"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dispatch"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// bundleRead reads the bundle candidates of one plan, with the given exclusions, and fails the test on an error.
func bundleRead(t *testing.T, f *fixture, plan string, exclude ...string) BundleReading {
	t.Helper()
	out, err := f.sched.ReadBundles(context.Background(), plan, exclude)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// bundleOf is the bundle that holds a node, or false when the node is in none.
func (r BundleReading) bundleOf(node string) (Bundle, bool) {
	for _, b := range r.Bundles {
		for _, id := range b.Nodes {
			if id == node {
				return b, true
			}
		}
	}
	return Bundle{}, false
}

// reasonOf is the closed reason a node is left out for, or "" when it is a candidate.
func (r BundleReading) reasonOf(node string) string {
	for _, e := range r.Excluded {
		if e.Node == node {
			return e.Reason
		}
	}
	return ""
}

// bundleEdge is an artifact_verified edge that pins its head, the shape an implementation node's edge takes.
func bundleEdge(id, from, to string) doc {
	return addEdge(id, from, to, dag.EdgeArtifactVerified, doc{"pins_code_head": true, "target_repository": "owner/repo", "target_base_ref": "dev"})
}

// bundleRelease records a release of a node the way dag-release leaves it: a dag_releases row and a dag_release_requests row.
func (f *fixture) bundleRelease(plan, node string) {
	f.t.Helper()
	f.exec("INSERT INTO dag_releases (plan_id, node_id, manifest_digest, managed_request_id, coordinator_epoch, decided_at) VALUES (?, ?, ?, ?, 0, 't')",
		plan, node, dig("manifest "+node), "req-"+node)
	f.exec("INSERT INTO dag_release_requests (plan_id, node_id, manifest_digest, request_sha256, request_json, marker_root, socket, state_selector, recorded_at) VALUES (?, ?, ?, ?, '{}', '', '', '', 't')",
		plan, node, dig("manifest "+node), dig("request "+node))
}

// The place a change edits is judged by its repository and path: a file in the same parent directory, or the same file, is one bundle reason.
func TestBundleSameRegion(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		a, b    []string
		bundled bool
	}{
		{name: "the same file", a: []string{"pkg/x.go"}, b: []string{"pkg/x.go"}, bundled: true},
		{name: "two files in one directory", a: []string{"pkg/a.go"}, b: []string{"pkg/b.go"}, bundled: true},
		{name: "different package directories", a: []string{"pkg/a/a.go"}, b: []string{"pkg/b/b.go"}},
		{name: "two files at the repository root", a: []string{"a.go"}, b: []string{"b.go"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t)
			f.projectParent()
			f.putPlan("s", 0, "s-r1", addNode("a", dag.NodeImplementation), addNode("b", dag.NodeImplementation))
			f.declare("s", "a", c.a...)
			f.declare("s", "b", c.b...)
			got := bundleRead(t, f, "s")
			bundle, ok := got.bundleOf("a")
			if ok != c.bundled {
				t.Fatalf("bundled = %v, want %v: %+v", ok, c.bundled, got)
			}
			if c.bundled && (!reflect.DeepEqual(bundle.Nodes, []string{"a", "b"}) || !reflect.DeepEqual(bundle.Reasons, []string{"same_region"})) {
				t.Fatalf("bundle = %+v", bundle)
			}
		})
	}
}

// A mechanical region is settled by its rule, so it never joins two nodes on its own.
func TestBundleSameRegionLeavesMechanicalRegionsOut(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.projectParent()
	f.putPlan("m", 0, "m-r1", addNode("a", dag.NodeImplementation), addNode("b", dag.NodeImplementation), addNode("c", dag.NodeImplementation))
	mechanical := gr("pkg/gen.go", "mechanical", "union")
	for _, node := range []string{"a", "b"} {
		if _, err := f.sched.DeclareRegions(context.Background(), "m", node, "parent", []Region{mechanical}); err != nil {
			t.Fatal(err)
		}
	}
	f.declare("m", "c", "pkg/gen.go")
	if got := bundleRead(t, f, "m"); len(got.Bundles) != 0 {
		t.Fatalf("two mechanical regions and an independent one at one path made bundles: %+v", got.Bundles)
	}
}

// A chain of two candidates joins them when the successor has no other predecessor and the predecessor no other successor.
func TestBundleChainSlice(t *testing.T) {
	t.Parallel()
	impl := dag.NodeImplementation
	t.Run("one edge between two candidates", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t)
		f.putPlan("c", 0, "c-r1", addNode("a", impl), addNode("b", impl), bundleEdge("ab", "a", "b"))
		got := bundleRead(t, f, "c")
		bundle, ok := got.bundleOf("a")
		if !ok || !reflect.DeepEqual(bundle.Nodes, []string{"a", "b"}) || !reflect.DeepEqual(bundle.Reasons, []string{"chain_slice"}) {
			t.Fatalf("bundle = %+v (ok %v)", bundle, ok)
		}
		if !reflect.DeepEqual(bundle.InternalEdges, []string{"ab"}) {
			t.Fatalf("internal edges = %v, want [ab]", bundle.InternalEdges)
		}
	})
	t.Run("the successor has two predecessors", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t)
		f.putPlan("c", 0, "c-r1", addNode("a", impl), addNode("b", impl), addNode("x", impl), bundleEdge("ab", "a", "b"), bundleEdge("xb", "x", "b"))
		if got := bundleRead(t, f, "c"); len(got.Bundles) != 0 {
			t.Fatalf("a successor with two predecessors made a chain: %+v", got.Bundles)
		}
	})
	t.Run("the predecessor has two successors", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t)
		f.putPlan("c", 0, "c-r1", addNode("a", impl), addNode("b", impl), addNode("x", impl), bundleEdge("ab", "a", "b"), bundleEdge("ax", "a", "x"))
		if got := bundleRead(t, f, "c"); len(got.Bundles) != 0 {
			t.Fatalf("a predecessor with two successors made a chain: %+v", got.Bundles)
		}
	})
	t.Run("one end is released", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t)
		f.putPlan("c", 0, "c-r1", addNode("a", impl), addNode("b", impl), bundleEdge("ab", "a", "b"))
		f.bundleRelease("c", "a")
		got := bundleRead(t, f, "c")
		if len(got.Bundles) != 0 || got.reasonOf("a") != "released" {
			t.Fatalf("a released predecessor: bundles %+v, a %q", got.Bundles, got.reasonOf("a"))
		}
	})
}

// A pair that meets both rules carries both reasons.
func TestBundlePairCarriesEveryReason(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.projectParent()
	f.putPlan("b", 0, "b-r1", addNode("a", dag.NodeImplementation), addNode("b", dag.NodeImplementation), bundleEdge("ab", "a", "b"))
	f.declare("b", "a", "pkg/x.go")
	f.declare("b", "b", "pkg/y.go")
	got := bundleRead(t, f, "b")
	bundle, ok := got.bundleOf("a")
	if !ok || !reflect.DeepEqual(bundle.Reasons, []string{"chain_slice", "same_region"}) {
		t.Fatalf("bundle = %+v (ok %v)", bundle, ok)
	}
	if len(bundle.Pairs) != 1 || !reflect.DeepEqual(bundle.Pairs[0].Reasons, []string{"chain_slice", "same_region"}) {
		t.Fatalf("pairs = %+v", bundle.Pairs)
	}
}

// Three nodes in a chain form one bundle whose regions are the union of theirs and whose internal edges are both edges.
func TestBundleChainOfThree(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.projectParent()
	f.putPlan("t", 0, "t-r1", addNode("a", dag.NodeImplementation), addNode("b", dag.NodeImplementation), addNode("c", dag.NodeImplementation),
		bundleEdge("ab", "a", "b"), bundleEdge("bc", "b", "c"))
	f.declare("t", "a", "one/a.go")
	f.declare("t", "b", "two/b.go")
	f.declare("t", "c", "three/c.go")
	got := bundleRead(t, f, "t")
	bundle, ok := got.bundleOf("b")
	if !ok || !reflect.DeepEqual(bundle.Nodes, []string{"a", "b", "c"}) {
		t.Fatalf("bundle = %+v (ok %v)", bundle, ok)
	}
	if len(got.Bundles) != 1 {
		t.Fatalf("bundles = %d, want 1", len(got.Bundles))
	}
	if !reflect.DeepEqual(bundle.InternalEdges, []string{"ab", "bc"}) {
		t.Fatalf("internal edges = %v", bundle.InternalEdges)
	}
	var paths []string
	for _, r := range bundle.Regions {
		paths = append(paths, r.Path)
	}
	if !reflect.DeepEqual(paths, []string{"one/a.go", "three/c.go", "two/b.go"}) {
		t.Fatalf("regions = %v", paths)
	}
	if len(bundle.Pairs) != 2 {
		t.Fatalf("pairs = %+v, want the two chain pairs", bundle.Pairs)
	}
	if again := bundleRead(t, f, "t"); !reflect.DeepEqual(got, again) {
		t.Fatalf("two readings of one store differ:\n%+v\n%+v", got, again)
	}
}

// Non-PR, released, parent-excluded and other-plan-exclusive nodes stay out, each with its reason.
func TestBundleExcludesNodes(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.projectParent()
	f.putPlan("x", 0, "x-r1", addNode("doc", dag.NodeNonPR), addNode("rel", dag.NodeImplementation), addNode("pe", dag.NodeImplementation),
		addNode("p1", dag.NodeImplementation), addNode("p2", dag.NodeImplementation))
	f.bundleRelease("x", "rel")
	f.declare("x", "p1", "pkg/a.go")
	f.declare("x", "p2", "pkg/b.go")
	got := bundleRead(t, f, "x", "pe")
	for node, want := range map[string]string{"doc": "non_pr", "rel": "released", "pe": "parent_excluded"} {
		if reason := got.reasonOf(node); reason != want {
			t.Errorf("%s: reason %q, want %q", node, reason, want)
		}
	}
	if bundle, ok := got.bundleOf("p1"); !ok || !reflect.DeepEqual(bundle.Nodes, []string{"p1", "p2"}) {
		t.Fatalf("the two remaining candidates should bundle: %+v", got)
	}
	if _, err := f.sched.ReadBundles(context.Background(), "x", []string{"nope"}); err == nil {
		t.Fatal("an unknown --exclude node was accepted")
	} else {
		var usage *dispatch.UsageError
		if !errors.As(err, &usage) || usage.Code != 4 {
			t.Fatalf("an unknown --exclude node: %v, want a usage error", err)
		}
	}
}

// A live implementation node of another plan that holds a place exclusively keeps a candidate out, until that node integrates.
func TestBundleOtherPlanExclusive(t *testing.T) {
	t.Parallel()
	exclusive := Region{Repository: "owner/repo", Path: "pkg/x.go", Kind: "file", Change: "edit", Exclusive: true}
	setup := func(t *testing.T, holder []Region) *fixture {
		f := newFixture(t)
		f.projectParent()
		forkJoinPlan(f, "q")
		if _, err := f.sched.DeclareRegions(context.Background(), "q", "impl-a", "parent", holder); err != nil {
			t.Fatal(err)
		}
		f.putPlan("p", 0, "p-r1", addNode("a", dag.NodeImplementation), addNode("b", dag.NodeImplementation))
		f.declare("p", "a", "pkg/x.go")
		f.declare("p", "b", "elsewhere/y.go")
		return f
	}
	t.Run("an unintegrated holder by the stated hold", func(t *testing.T) {
		t.Parallel()
		got := bundleRead(t, setup(t, []Region{exclusive}), "p")
		if reason := got.reasonOf("a"); reason != "other_plan_exclusive" {
			t.Fatalf("a: reason %q, want other_plan_exclusive", reason)
		}
		if reason := got.reasonOf("b"); reason != "" {
			t.Fatalf("b holds another path and is a candidate, got %q", reason)
		}
	})
	t.Run("an unintegrated holder by the exclusive grade", func(t *testing.T) {
		t.Parallel()
		got := bundleRead(t, setup(t, []Region{gr("pkg/x.go", "exclusive", "")}), "p")
		if reason := got.reasonOf("a"); reason != "other_plan_exclusive" {
			t.Fatalf("a: reason %q, want other_plan_exclusive", reason)
		}
	})
	t.Run("a holder that a later revision made a non_pr node still holds", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t)
		f.projectParent()
		f.putPlan("q", 0, "q-r1", addNode("h", dag.NodeImplementation))
		if _, err := f.sched.DeclareRegions(context.Background(), "q", "h", "parent", []Region{exclusive}); err != nil {
			t.Fatal(err)
		}
		f.putPlan("q", 1, "q-r2", doc{"op": dag.OpUpdateNode, "node": nodeDoc("h", dag.NodeNonPR)})
		f.putPlan("p", 0, "p-r1", addNode("a", dag.NodeImplementation), addNode("b", dag.NodeImplementation))
		f.declare("p", "a", "pkg/x.go")
		f.declare("p", "b", "elsewhere/y.go")
		got := bundleRead(t, f, "p")
		if reason := got.reasonOf("a"); reason != "other_plan_exclusive" {
			t.Fatalf("a: reason %q, want other_plan_exclusive: the holder is live and unintegrated whatever its kind", reason)
		}
	})
	t.Run("an integrated holder no longer holds", func(t *testing.T) {
		t.Parallel()
		f := setup(t, []Region{exclusive})
		a := f.acceptNode("q", "impl-a", pinnedOpts)
		f.integrate(a, "owner/repo", "dev", true, true)
		if got := bundleRead(t, f, "p"); got.reasonOf("a") != "" {
			t.Fatalf("a integrated holder still keeps a out: %q", got.reasonOf("a"))
		}
	})
}

// dag-ready carries the same candidates as the command, and reading the store changes no byte of it.
func TestBundleCommandAndReadyAgree(t *testing.T) {
	t.Parallel()
	state := closedState(t, func(f *fixture) {
		f.projectParent()
		f.putPlan("k", 0, "k-r1", addNode("a", dag.NodeImplementation), addNode("b", dag.NodeImplementation), addNode("doc", dag.NodeNonPR), bundleEdge("ab", "a", "b"))
		f.declare("k", "a", "pkg/a.go")
		f.declare("k", "b", "pkg/b.go")
	})
	path := state + "/relay.sqlite3"
	before := bundleFileSum(t, path)
	command, code := crw(t, state, "dag-bundle-candidates", "--plan", "k")
	if code != 0 {
		t.Fatalf("dag-bundle-candidates exit %d: %s", code, command)
	}
	again, _ := crw(t, state, "dag-bundle-candidates", "--plan", "k")
	ready, code := crw(t, state, "dag-ready", "--plan", "k")
	if code != 0 {
		t.Fatalf("dag-ready exit %d: %s", code, ready)
	}
	if after := bundleFileSum(t, path); after != before {
		t.Fatal("reading the candidates changed the store")
	}
	if command != again {
		t.Fatalf("two candidate readings differ:\n%s\n%s", command, again)
	}
	answer := parseOut(t, command)
	ridden := parseOut(t, ready)["bundleCandidates"].(map[string]any)
	if !reflect.DeepEqual(ridden["bundles"], answer["bundles"]) || !reflect.DeepEqual(ridden["excluded"], answer["excluded"]) {
		t.Fatalf("dag-ready bundleCandidates = %v, command = %v", ridden, answer)
	}
	if bundles := answer["bundles"].([]any); len(bundles) != 1 {
		t.Fatalf("bundles = %v, want one chain", bundles)
	}
}

func bundleFileSum(t *testing.T, path string) [32]byte {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return sha256.Sum256(raw)
}

// bundleMerged is the single region a two-member bundle holds for one identity, after passing it through the normaliser dag-region-declare applies.
func bundleMerged(t *testing.T, a, b Region) Region {
	t.Helper()
	f := newFixture(t)
	f.projectParent()
	f.putPlan("m", 0, "m-r1", addNode("a", dag.NodeImplementation), addNode("b", dag.NodeImplementation), bundleEdge("ab", "a", "b"))
	for node, r := range map[string]Region{"a": a, "b": b} {
		if _, err := f.sched.DeclareRegions(context.Background(), "m", node, "parent", []Region{r}); err != nil {
			t.Fatal(err)
		}
	}
	bundle, ok := bundleRead(t, f, "m").bundleOf("a")
	if !ok {
		t.Fatal("the two nodes did not bundle")
	}
	if len(bundle.Regions) != 1 {
		t.Fatalf("one identity, %d regions: %+v", len(bundle.Regions), bundle.Regions)
	}
	normal, err := normalizeRegions(bundle.Regions)
	if err != nil {
		t.Fatalf("dag-region-declare refuses the bundle's regions: %v", err)
	}
	if !reflect.DeepEqual(normal, bundle.Regions) {
		t.Fatalf("the normaliser changed the bundle's regions:\n%+v\n%+v", bundle.Regions, normal)
	}
	return bundle.Regions[0]
}

// Members that declare one place (repository, path, kind, key) differently give one region, merged so that it never holds the place less strictly than any member did.
func TestBundleRegionsMergeOneIdentity(t *testing.T) {
	t.Parallel()
	region := func(change, grade, rule string, exclusive bool) Region {
		return Region{Repository: "owner/repo", Path: "pkg/x.go", Kind: "file", Change: change, Grade: grade, Rule: rule, Exclusive: exclusive}
	}
	cases := []struct {
		name string
		a, b Region
		want Region
	}{
		{"local and independent", region("edit", "local", "", false), region("edit", "independent", "", false), region("edit", "independent", "", false)},
		{"independent and exclusive grade", region("edit", "independent", "", false), region("edit", "exclusive", "", false), region("edit", "exclusive", "", false)},
		{"mechanical and local", region("edit", "mechanical", "union", false), region("edit", "local", "", false), region("edit", "local", "", false)},
		{"mechanical under two rules", region("edit", "mechanical", "union", false), region("edit", "mechanical", "renumber", false), region("edit", "local", "", false)},
		{"mechanical under one rule", region("edit", "mechanical", "union", false), region("edit", "mechanical", "union", false), region("edit", "mechanical", "union", false)},
		{"the stated whole-repository hold", region("edit", "local", "", true), region("edit", "local", "", false), region("edit", "exclusive", "", true)},
		{"edit and rename", region("edit", "local", "", false), region("rename", "local", "", false), region("rename", "exclusive", "", false)},
		{"edit and delete", region("edit", "independent", "", false), region("delete", "local", "", false), region("delete", "exclusive", "", false)},
		{"rename and delete", region("rename", "local", "", false), region("delete", "local", "", false), region("delete", "exclusive", "", false)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if got := bundleMerged(t, c.a, c.b); got != c.want {
				t.Fatalf("merged = %+v, want %+v", got, c.want)
			}
			// the merge does not depend on which member declared which
			if got := bundleMerged(t, c.b, c.a); got != c.want {
				t.Fatalf("merged (members swapped) = %+v, want %+v", got, c.want)
			}
		})
	}
}

// A bundle of several members holds each place once, and what it holds is accepted by dag-region-declare as it stands.
func TestBundleRegionsUnionIsDeclarable(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.projectParent()
	f.putPlan("u", 0, "u-r1", addNode("a", dag.NodeImplementation), addNode("b", dag.NodeImplementation), addNode("c", dag.NodeImplementation), addNode("m", dag.NodeImplementation),
		bundleEdge("ab", "a", "b"), bundleEdge("bc", "b", "c"))
	file := func(p, change, grade string) Region {
		return Region{Repository: "owner/repo", Path: p, Kind: "file", Change: change, Grade: grade}
	}
	symbol := func(key string) Region {
		return Region{Repository: "owner/repo", Path: "pkg/s.go", Kind: "symbol", Key: key, Change: "edit"}
	}
	declare := func(node string, regions ...Region) {
		t.Helper()
		if _, err := f.sched.DeclareRegions(context.Background(), "u", node, "parent", regions); err != nil {
			t.Fatal(err)
		}
	}
	declare("a", file("pkg/x.go", "edit", "local"), file("pkg/only-a.go", "edit", ""), symbol("One"))
	declare("b", file("pkg/x.go", "edit", "independent"), symbol("Two"))
	declare("c", file("pkg/x.go", "delete", "local"), file("pkg/only-c.go", "edit", "local"), symbol("One"))
	bundle, ok := bundleRead(t, f, "u").bundleOf("a")
	if !ok || !reflect.DeepEqual(bundle.Nodes, []string{"a", "b", "c"}) {
		t.Fatalf("bundle = %+v (ok %v)", bundle, ok)
	}
	var identities []string
	for _, r := range bundle.Regions {
		identities = append(identities, r.Path+"|"+r.Kind+"|"+r.Key)
	}
	want := []string{"pkg/only-a.go|file|", "pkg/only-c.go|file|", "pkg/s.go|symbol|One", "pkg/s.go|symbol|Two", "pkg/x.go|file|"}
	if !reflect.DeepEqual(identities, want) {
		t.Fatalf("identities = %v, want %v", identities, want)
	}
	normal, err := normalizeRegions(bundle.Regions)
	if err != nil {
		t.Fatalf("dag-region-declare refuses the bundle's regions: %v", err)
	}
	if !reflect.DeepEqual(normal, bundle.Regions) {
		t.Fatalf("the normaliser changed the bundle's regions:\n%+v\n%+v", bundle.Regions, normal)
	}
	if _, err := f.sched.DeclareRegions(context.Background(), "u", "m", "parent", bundle.Regions); err != nil {
		t.Fatalf("declaring the bundle's regions on the merged node: %v", err)
	}
	for _, r := range bundle.Regions {
		if r.Path == "pkg/x.go" && (r.Change != "delete" || r.Grade != "exclusive") {
			t.Fatalf("the merged pkg/x.go = %+v, want the delete held exclusive", r)
		}
	}
}

// Either row alone is enough to call a node released: a managed start leaves a dag_release_requests row before a dag_releases row exists.
func TestBundleReleasedByEitherRow(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.putPlan("r", 0, "r-r1", addNode("req", dag.NodeImplementation), addNode("rel", dag.NodeImplementation), addNode("free", dag.NodeImplementation))
	f.exec("INSERT INTO dag_release_requests (plan_id, node_id, manifest_digest, request_sha256, request_json, marker_root, socket, state_selector, recorded_at) VALUES (?, ?, ?, ?, '{}', '', '', '', 't')",
		"r", "req", dig("manifest req"), dig("request req"))
	f.exec("INSERT INTO dag_releases (plan_id, node_id, manifest_digest, managed_request_id, coordinator_epoch, decided_at) VALUES (?, ?, ?, ?, 0, 't')",
		"r", "rel", dig("manifest rel"), "req-rel")
	got := bundleRead(t, f, "r")
	for node, want := range map[string]string{"req": "released", "rel": "released", "free": ""} {
		if reason := got.reasonOf(node); reason != want {
			t.Errorf("%s: reason %q, want %q", node, reason, want)
		}
	}
}

// dag-ready --record keeps its pass and carries the same candidates as the plain reading.
func TestBundleReadyRecordCarriesTheCandidates(t *testing.T) {
	t.Parallel()
	state := closedState(t, func(f *fixture) {
		f.projectParent()
		f.putPlan("k", 0, "k-r1", addNode("a", dag.NodeImplementation), addNode("b", dag.NodeImplementation), bundleEdge("ab", "a", "b"))
	})
	plain, code := crw(t, state, "dag-ready", "--plan", "k")
	if code != 0 {
		t.Fatalf("dag-ready exit %d: %s", code, plain)
	}
	recorded, code := crw(t, state, "dag-ready", "--plan", "k", "--record", "--actor", "parent")
	if code != 0 {
		t.Fatalf("dag-ready --record exit %d: %s", code, recorded)
	}
	out := parseOut(t, recorded)
	if out["pass_seq"] != float64(1) {
		t.Fatalf("pass_seq = %v", out["pass_seq"])
	}
	if !reflect.DeepEqual(out["bundleCandidates"], parseOut(t, plain)["bundleCandidates"]) {
		t.Fatalf("recorded bundleCandidates = %v, plain = %v", out["bundleCandidates"], parseOut(t, plain)["bundleCandidates"])
	}
}

// dag-ready --record answers the recorded pass and the candidates from one state of the store: a plan revision that another process commits while the command runs
// cannot land between the two readings. The writer is its own Store over the same file (another connection, as another process has) and it writes from inside the
// seam between the two readings, synchronously, so there is no start to wait for and no schedule to hope for: a command that holds no transaction between the
// readings lets the revision commit at once, and one that keeps the pass's transaction open leaves the write lock taken, so the writer is refused (its lock wait
// ends) having surely reached the write. The refusal is then shown to be the lock's: the same request commits after the command.
func TestBundleRecordedPassAndCandidatesShareASnapshot(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.putPlan("s", 0, "s-r1", addNode("a", dag.NodeImplementation), addNode("b", dag.NodeImplementation), bundleEdge("ab", "a", "b"))
	other, err := store.Open(context.Background(), f.path, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = other.Close() })
	// the writer's one connection gives up on a taken write lock after a second, so the refusal below comes quickly (the store's own wait is thirty)
	if _, err := other.DB.ExecContext(context.Background(), "PRAGMA busy_timeout=1000"); err != nil {
		t.Fatal(err)
	}
	writer := &dag.Repo{Store: other, Now: f.clock}
	raw, err := json.Marshal(doc{"schema": dag.SchemaRevision, "plan_id": "s", "project_key": "P-TEST", "request_id": "s-r2",
		"expected_parent_revision": 1, "author_task_id": "task-test", "changes": []any{addNode("c", dag.NodeImplementation)}})
	if err != nil {
		t.Fatal(err)
	}
	rev, err := dag.DecodeRevision(raw)
	if err != nil {
		t.Fatal(err)
	}
	var inSeam error
	reached := false
	f.sched.testBetweenPassBundles = func() {
		reached = true
		_, inSeam = writer.Put(context.Background(), rev)
	}
	reading, seq, bundles, err := f.sched.RecordPassWithBundles(context.Background(), "s", "parent")
	if err != nil {
		t.Fatal(err)
	}
	if !reached {
		t.Fatal("the seam between the pass and the candidates never ran")
	}
	if seq != 1 || reading.PlanRevision != 1 {
		t.Fatalf("pass %d at revision %d, want pass 1 at revision 1", seq, reading.PlanRevision)
	}
	if inSeam == nil {
		t.Fatalf("a plan revision committed between the recorded pass and the candidates (the candidates read revision %d)", bundles.PlanRevision)
	}
	if bundles.PlanRevision != reading.PlanRevision {
		t.Fatalf("the pass was recorded at revision %d and the candidates read at revision %d", reading.PlanRevision, bundles.PlanRevision)
	}
	// the writer was held off by the lock, not by its own request: after the command it commits
	if _, err := writer.Put(context.Background(), rev); err != nil {
		t.Fatalf("the concurrent revision, refused while the command ran (%v), does not commit afterwards: %v", inSeam, err)
	}
}

// bundleFiles declares count files pkg/<prefix>00.go ... on a node.
func (f *fixture) bundleFiles(plan, node, prefix string, count int) {
	f.t.Helper()
	regions := make([]Region, count)
	for i := range regions {
		regions[i] = Region{Repository: "owner/repo", Path: fmt.Sprintf("pkg/%s%02d.go", prefix, i), Kind: "file", Change: "edit"}
	}
	if _, err := f.sched.DeclareRegions(context.Background(), plan, node, "parent", regions); err != nil {
		f.t.Fatal(err)
	}
}

// A bundle is offered only when dag-region-declare accepts its regions as they stand: 64 places are declarable, 65 are not, so a group whose union would hold 65
// is not offered as one bundle (the members keep their own declarations; the parent declares nothing it cannot).
func TestBundleUnionStaysDeclarable(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name         string
		a, b, c      int // the places each node declares; a and b share a directory, c joins them
		wantBundle   []string
		wantRegions  int
		wantNoBundle bool
	}{
		{name: "64 places", a: 32, b: 32, wantBundle: []string{"a", "b"}, wantRegions: 64},
		{name: "65 places", a: 33, b: 32, wantNoBundle: true},
		{name: "a third node that would make 65 stays out", a: 33, b: 31, c: 2, wantBundle: []string{"a", "b"}, wantRegions: 64},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t)
			f.putPlan("w", 0, "w-r1", addNode("a", dag.NodeImplementation), addNode("b", dag.NodeImplementation), addNode("c", dag.NodeImplementation))
			f.bundleFiles("w", "a", "a", c.a)
			f.bundleFiles("w", "b", "b", c.b)
			if c.c > 0 {
				f.bundleFiles("w", "c", "c", c.c)
			}
			got := bundleRead(t, f, "w")
			if c.wantNoBundle {
				if len(got.Bundles) != 0 {
					t.Fatalf("%d bundles, want none (first: %v)", len(got.Bundles), got.Bundles[0].Nodes)
				}
				return
			}
			if len(got.Bundles) != 1 || !reflect.DeepEqual(got.Bundles[0].Nodes, c.wantBundle) {
				t.Fatalf("%d bundles, want one of %v", len(got.Bundles), c.wantBundle)
			}
			b := got.Bundles[0]
			if len(b.Regions) != c.wantRegions {
				t.Fatalf("the bundle holds %d places, want %d", len(b.Regions), c.wantRegions)
			}
			for _, pair := range b.Pairs {
				for _, n := range pair.Nodes {
					if !slices.Contains(b.Nodes, n) {
						t.Fatalf("pair %v names %s, which is outside the bundle %v", pair.Nodes, n, b.Nodes)
					}
				}
			}
			normal, err := normalizeRegions(b.Regions)
			if err != nil || !reflect.DeepEqual(normal, b.Regions) {
				t.Fatalf("dag-region-declare does not accept the bundle's regions as they stand: %v", err)
			}
		})
	}
}

// bundleRegionsOffered checks the contract of a bundle's regions: empty (nobody declared) or accepted by dag-region-declare's normaliser without a change.
func bundleRegionsOffered(t *testing.T, b Bundle) {
	t.Helper()
	if len(b.Regions) == 0 {
		return
	}
	normal, err := normalizeRegions(b.Regions)
	if err != nil || !reflect.DeepEqual(normal, b.Regions) {
		t.Fatalf("dag-region-declare does not accept the bundle's regions %+v as they stand: %v", b.Regions, err)
	}
}

// A chain of nodes that declared nothing is still a bundle (chain_slice needs no declaration) and its regions are empty: dag-region-declare refuses an empty
// declaration, so an empty list is the one answer that is not passed on. A chain in which only some members declared holds exactly their places, and that list
// is accepted as it stands.
func TestBundleRegionsOfUndeclaredMembers(t *testing.T) {
	t.Parallel()
	impl := dag.NodeImplementation
	t.Run("no member declared", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t)
		f.putPlan("n", 0, "n-r1", addNode("a", impl), addNode("b", impl), bundleEdge("ab", "a", "b"))
		got := bundleRead(t, f, "n")
		bundle, ok := got.bundleOf("a")
		if !ok || !reflect.DeepEqual(bundle.Nodes, []string{"a", "b"}) || !reflect.DeepEqual(bundle.Reasons, []string{"chain_slice"}) {
			t.Fatalf("bundle = %+v (ok %v)", bundle, ok)
		}
		if len(bundle.Regions) != 0 {
			t.Fatalf("regions = %+v, want none", bundle.Regions)
		}
		if _, err := normalizeRegions(bundle.Regions); err == nil {
			t.Fatal("dag-region-declare takes an empty declaration; the empty list would then be declarable and the documented policy is wrong")
		}
		var list any
		for _, field := range bundle.object() {
			if field.Key == "regions" {
				list = field.Value
			}
		}
		if empty, isList := list.([]any); !isList || len(empty) != 0 {
			t.Fatalf("regions = %#v, want an empty list (not null)", list)
		}
	})
	t.Run("one member of a chain declared", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t)
		f.projectParent()
		f.putPlan("n", 0, "n-r1", addNode("a", impl), addNode("b", impl), addNode("c", impl), bundleEdge("ab", "a", "b"), bundleEdge("bc", "b", "c"))
		f.declare("n", "b", "pkg/b.go", "pkg/more.go")
		bundle, ok := bundleRead(t, f, "n").bundleOf("a")
		if !ok || !reflect.DeepEqual(bundle.Nodes, []string{"a", "b", "c"}) {
			t.Fatalf("bundle = %+v (ok %v)", bundle, ok)
		}
		var paths []string
		for _, r := range bundle.Regions {
			paths = append(paths, r.Path)
		}
		if !reflect.DeepEqual(paths, []string{"pkg/b.go", "pkg/more.go"}) {
			t.Fatalf("regions = %v, want the declaring member's two places", paths)
		}
		bundleRegionsOffered(t, bundle)
	})
	t.Run("every bundle of a mixed plan offers a list that is empty or accepted", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t)
		f.projectParent()
		f.putPlan("n", 0, "n-r1", addNode("a", impl), addNode("b", impl), addNode("c", impl), addNode("d", impl), addNode("e", impl), addNode("g", impl),
			bundleEdge("ab", "a", "b"), bundleEdge("cd", "c", "d"))
		f.declare("n", "c", "pkg/c.go")
		f.declare("n", "e", "lib/e.go")
		f.declare("n", "g", "lib/g.go")
		got := bundleRead(t, f, "n")
		if len(got.Bundles) != 3 {
			t.Fatalf("bundles = %+v, want a-b, c-d and e-g", got.Bundles)
		}
		for _, b := range got.Bundles {
			bundleRegionsOffered(t, b)
		}
	})
}
