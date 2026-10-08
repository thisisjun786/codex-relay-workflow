package dagsched

import (
	"context"
	"crypto/sha256"
	"errors"
	"os"
	"reflect"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dag"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dispatch"
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
