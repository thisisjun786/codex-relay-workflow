package dagsched

import (
	"context"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dag"
)

// The printed vocabulary of the invalidation reading is pinned here as literals: a constant renamed in the code must not silently rename what dag-ready prints.
const (
	invStale           = "stale"
	invSliceChanged    = "stale:slice_changed"
	invCriteriaChanged = "stale:criteria_changed"
)

// invSettle gives a node to a child through the real release path (so its manifest is the one BuildManifest builds from the store), reports the child's verified result and accepts it.
func (k *releaseKit) invSettle(plan, node string) AcceptResult {
	k.t.Helper()
	res := k.mustRelease(plan, node)
	k.seedReport(res.RelationshipID, node, plan)
	out, err := k.accept(plan, node, AcceptInput{})
	if err != nil {
		k.t.Fatalf("accept %s: %v", node, err)
	}
	return out
}

// invSharedRoot is the contract's shared-root case (E-24, Airflow #73710): R -> A, R -> B, A -> C, every node released, reported and accepted in order.
func invSharedRoot(k *releaseKit) map[string]AcceptResult {
	k.t.Helper()
	k.putPlan("sr", 0, "sr-r1", addRelNode("R", dag.NodeNonPR), addRelNode("A", dag.NodeNonPR), addRelNode("B", dag.NodeNonPR), addRelNode("C", dag.NodeNonPR),
		addEdge("ra", "R", "A", dag.EdgeArtifactVerified, nil), addEdge("rb", "R", "B", dag.EdgeArtifactVerified, nil), addEdge("ac", "A", "C", dag.EdgeArtifactVerified, nil))
	accepted := map[string]AcceptResult{}
	for _, node := range []string{"R", "A", "B", "C"} {
		accepted[node] = k.invSettle("sr", node)
	}
	return accepted
}

// invRevise is a plan revision that replaces one node's spec (update_node keeps the id) with a changed title: the slice digest of that node moves and nothing else does.
func (k *releaseKit) invRevise(plan, node, request string, mutate func(n doc)) {
	k.t.Helper()
	snap := k.snapshot(plan)
	n, ok := nodeOf(snap, node)
	if !ok {
		k.t.Fatalf("no node %s", node)
	}
	spec := relNode(node, n.Kind)
	mutate(spec)
	k.putPlan(plan, int(snap.Revision), request, doc{"op": dag.OpUpdateNode, "node": spec})
}

func invTitle(title string) func(n doc) { return func(n doc) { n["title"] = title } }

// invStaleIDs is the sorted ids of the nodes a reading marks stale.
func invStaleIDs(r Reading) []string {
	out := []string{}
	for _, n := range r.Nodes {
		if n.Disposition == invStale {
			out = append(out, n.NodeID)
		}
	}
	sort.Strings(out)
	return out
}

func invField(o contract.OrderedObject, key string) any {
	for _, f := range o {
		if f.Key == key {
			return f.Value
		}
	}
	return nil
}

// invStaleObject is the structured stale reading of one node as dag-ready prints it, or nil when the node carries none.
func invStaleObject(t *testing.T, r Reading, node string) contract.OrderedObject {
	t.Helper()
	nodes, _ := invField(r.Object(), "nodes").([]any)
	for _, item := range nodes {
		o, _ := item.(contract.OrderedObject)
		if invField(o, "node_id") == node {
			stale, _ := invField(o, "stale").(contract.OrderedObject)
			return stale
		}
	}
	t.Fatalf("no node %s in the printed reading", node)
	return nil
}

// invAssertReasons is criterion c3 as a predicate over one reading: every stale node has a closed stale reason, the structured object, and the edge and the predecessor version it
// rests on (a node whose own slice changed names the slice versions instead), and no node that is not stale carries the object.
func invAssertReasons(t *testing.T, r Reading) {
	t.Helper()
	for _, n := range r.Nodes {
		obj := invStaleObject(t, r, n.NodeID)
		if n.Disposition != invStale {
			if obj != nil || strings.HasPrefix(n.Reason, "stale:") {
				t.Errorf("%s is %s and carries a stale reading: %q %v", n.NodeID, n.Disposition, n.Reason, obj)
			}
			continue
		}
		if n.State != invStale || n.Reason == "" || !ReasonsClosed(n.Reason) || !strings.HasPrefix(n.Reason, "stale:") {
			t.Errorf("%s is stale without a closed stale reason: state %q reason %q", n.NodeID, n.State, n.Reason)
		}
		if obj == nil {
			t.Errorf("%s is stale and has no stale object", n.NodeID)
			continue
		}
		cause, _ := invField(obj, "cause").(string)
		if edge, named := strings.CutPrefix(n.Reason, "stale:edge:"); named {
			if invField(obj, "edge_id") != edge || invField(obj, "predecessor_node_id") == nil || !strings.Contains(n.Detail, edge) {
				t.Errorf("%s: reason %s does not name its edge and predecessor: %v / %s", n.NodeID, n.Reason, obj, n.Detail)
			}
			if cause != "edge_added" && invField(obj, "consumed_acceptance_id") == nil && invField(obj, "consumed_decision") == nil {
				t.Errorf("%s: cause %s names no predecessor version: %v", n.NodeID, cause, obj)
			}
		} else if (n.Reason != invSliceChanged || cause != "slice_changed") && (n.Reason != invCriteriaChanged || cause != "criteria_changed") ||
			invField(obj, "consumed_slice_digest") == nil || invField(obj, "current_slice_digest") == nil {
			t.Errorf("%s: reason %s cause %s does not name the slice versions: %v", n.NodeID, n.Reason, cause, obj)
		}
	}
}

// Criterion c1 (contract E-18, E-24): the seeds are what the revision changed and only their descendants can go stale. Over the shared-root DAG every case revises one node; the nodes it
// cannot reach keep reading done:accepted, so a revision never invalidates a sibling or an ancestor.
func TestSharedRootInvalidationMarksOnlyDescendants(t *testing.T) {
	cases := []struct {
		name    string
		revised string
		want    map[string]string // stale node -> reason; every other node stays done:accepted
	}{
		{name: "a middle node changes: it and the node built on it", revised: "A", want: map[string]string{"A": invSliceChanged, "C": "stale:edge:ac"}},
		{name: "the shared root changes: everything below it", revised: "R", want: map[string]string{"R": invSliceChanged, "A": "stale:edge:ra", "B": "stale:edge:rb", "C": "stale:edge:ac"}},
		{name: "a leaf changes: only the leaf", revised: "C", want: map[string]string{"C": invSliceChanged}},
		{name: "a sibling changes: only the sibling", revised: "B", want: map[string]string{"B": invSliceChanged}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			k := newReleaseKit(t)
			accepted := invSharedRoot(k)
			before := k.read("sr")
			if got := invStaleIDs(before); len(got) != 0 {
				t.Fatalf("before the revision %v are stale: %s", got, before.brief())
			}
			k.invRevise("sr", c.revised, "sr-r2", invTitle("a changed title"))
			after := k.read("sr")
			wantIDs := []string{}
			for id := range c.want {
				wantIDs = append(wantIDs, id)
			}
			sort.Strings(wantIDs)
			if got := invStaleIDs(after); !reflect.DeepEqual(got, wantIDs) {
				t.Fatalf("stale after revising %s = %v, want %v: %s", c.revised, got, wantIDs, after.brief())
			}
			for _, n := range after.Nodes {
				if reason, stale := c.want[n.NodeID]; stale {
					if n.Reason != reason {
						t.Errorf("%s reason %s, want %s (%s)", n.NodeID, n.Reason, reason, n.Detail)
					}
				} else if n.Reason != DoneAccepted || n.State != StateAccepted {
					t.Errorf("%s was not reached by the revision and reads %s %s: over-invalidation", n.NodeID, n.State, n.Reason)
				}
			}
			invAssertReasons(t, after)
			if c.revised == "A" {
				obj := invStaleObject(t, after, "C")
				if invField(obj, "cause") != "predecessor_stale" || invField(obj, "seed_node_id") != "A" || invField(obj, "predecessor_node_id") != "A" ||
					invField(obj, "consumed_acceptance_id") != accepted["A"].AcceptanceID {
					t.Errorf("C does not name the predecessor version it rests on: %v", obj)
				}
			}
		})
	}
}

// Criterion c1: an edge added or retired changes the slice digest of the node it points to, so that node is the seed. Its siblings and its ancestors are untouched, and what it reaches follows it.
func TestEdgeChangesSeedTheNodeTheyPointTo(t *testing.T) {
	t.Run("an edge added into C", func(t *testing.T) {
		k := newReleaseKit(t)
		invSharedRoot(k)
		k.putPlan("sr", int(k.snapshot("sr").Revision), "sr-r2", addEdge("bc", "B", "C", dag.EdgeArtifactVerified, nil))
		after := k.read("sr")
		if got := invStaleIDs(after); !reflect.DeepEqual(got, []string{"C"}) {
			t.Fatalf("stale = %v, want [C]: %s", got, after.brief())
		}
		obj := invStaleObject(t, after, "C")
		if after.node("C").Reason != "stale:edge:bc" || invField(obj, "cause") != "edge_added" || invField(obj, "predecessor_node_id") != "B" {
			t.Fatalf("C = %+v %v", after.node("C"), obj)
		}
		invAssertReasons(t, after)
	})
	t.Run("an edge retired from C", func(t *testing.T) {
		k := newReleaseKit(t)
		accepted := invSharedRoot(k)
		k.putPlan("sr", int(k.snapshot("sr").Revision), "sr-r2", doc{"op": dag.OpRetireEdge, "edge_id": "ac"})
		after := k.read("sr")
		if got := invStaleIDs(after); !reflect.DeepEqual(got, []string{"C"}) {
			t.Fatalf("stale = %v, want [C]: %s", got, after.brief())
		}
		obj := invStaleObject(t, after, "C")
		if after.node("C").Reason != "stale:edge:ac" || invField(obj, "cause") != "edge_retired" || invField(obj, "predecessor_node_id") != "A" ||
			invField(obj, "consumed_acceptance_id") != accepted["A"].AcceptanceID {
			t.Fatalf("C = %+v %v", after.node("C"), obj)
		}
		invAssertReasons(t, after)
	})
	t.Run("an edge added into the middle node reaches the node below it", func(t *testing.T) {
		k := newReleaseKit(t)
		invSharedRoot(k)
		k.putPlan("sr", int(k.snapshot("sr").Revision), "sr-r2", addNode("Q", dag.NodeNonPR), addEdge("qa", "Q", "A", dag.EdgeArtifactVerified, nil))
		after := k.read("sr")
		if got := invStaleIDs(after); !reflect.DeepEqual(got, []string{"A", "C"}) {
			t.Fatalf("stale = %v, want [A C]: %s", got, after.brief())
		}
		if after.node("A").Reason != "stale:edge:qa" || after.node("C").Reason != "stale:edge:ac" {
			t.Fatalf("reasons: %s", after.brief())
		}
		invAssertReasons(t, after)
	})
}

// A title is part of a node's slice (dag-plans: the slice digest covers the node's spec), so a title-only revision is a seed. So are the criteria (contract 3.2, E-11): until the same output is
// re-verified against them the node reads stale, and the node built on it with it; the re-verification (dag_acceptance_revalidations) resolves it with no new generation (E-21), and the
// stale mark goes away because it is derived, not stored.
func TestCriteriaChangeIsStaleUntilTheOutputIsReverified(t *testing.T) {
	k := newReleaseKit(t)
	accepted := invSharedRoot(k)
	other := dig("other criteria")
	k.invRevise("sr", "A", "sr-r2", func(n doc) { n["criteria_set_digest"] = other })
	after := k.read("sr")
	if got := invStaleIDs(after); !reflect.DeepEqual(got, []string{"A", "C"}) {
		t.Fatalf("after a change of A's criteria stale = %v, want [A C]: %s", got, after.brief())
	}
	if after.node("A").Reason != invCriteriaChanged || after.node("C").Reason != "stale:edge:ac" {
		t.Fatalf("reasons: %s", after.brief())
	}
	invAssertReasons(t, after)

	// the same output is ruled again under the new criteria (the criteria registered for the relationship and the revalidation row, as dag-accept leaves them)
	var event string
	if err := k.s.DB.QueryRow("SELECT event_id FROM events WHERE relationship_id = ?", accepted["A"].RelationshipID).Scan(&event); err != nil {
		t.Fatal(err)
	}
	k.exec("UPDATE canonical_criteria SET set_digest = ? WHERE relationship_id = ?", other, accepted["A"].RelationshipID)
	k.exec("INSERT INTO dag_acceptance_revalidations (revalidation_id, acceptance_id, criteria_set_digest, event_id, verdict_turn_id, reval_seq, revalidated_by, revalidated_at) VALUES ('rv1', ?, ?, ?, 'vt2', 1, 'parent', 't')",
		accepted["A"].AcceptanceID, other, event)
	resolved := k.read("sr")
	if got := invStaleIDs(resolved); len(got) != 0 {
		t.Fatalf("after the re-verification %v are still stale: %s", got, resolved.brief())
	}
	invAssertReasons(t, resolved)
}

// A change that is both a slice change and a criteria change: the node below it is stale although its edge reads blocked:stale_criteria (the criteria predicate comes first on the edge), and
// what is built on that node is held back too (the judgement of the predecessor is asked directly, not read from the reason the edge shows).
func TestSliceAndCriteriaChangedTogetherStillHoldsBackTheNodesBelow(t *testing.T) {
	k := newReleaseKit(t)
	invSharedRoot(k)
	k.putPlan("sr", int(k.snapshot("sr").Revision), "sr-r2", addRelNode("D", dag.NodeNonPR), addEdge("cd", "C", "D", dag.EdgeArtifactVerified, nil))
	k.invRevise("sr", "A", "sr-r3", func(n doc) {
		n["title"] = "a changed title"
		n["criteria_set_digest"] = dig("other criteria")
	})
	after := k.read("sr")
	if got := invStaleIDs(after); !reflect.DeepEqual(got, []string{"A", "C"}) {
		t.Fatalf("stale = %v, want [A C]: %s", got, after.brief())
	}
	if after.node("A").Reason != invSliceChanged || after.node("C").Reason != "stale:edge:ac" {
		t.Fatalf("reasons: %s", after.brief())
	}
	if d := after.node("D"); d.Disposition == DispReady || d.Reason != BlockedStalePredecessor {
		t.Fatalf("D = %+v, want it held back by the stale C", d)
	}
	invAssertReasons(t, after)
}

// Contract E-25: once the seed is repaired (A is accepted again at its new slice) the nodes built on its first acceptance stay stale until they are accepted again themselves. A mark that
// disappeared with the seed would show C as current while it still rests on a version of A that is no longer accepted. The second acceptance consumed the inputs the product builds for the
// revised A (invRealInputs), and the test checks that it is whole before reading.
func TestStaleSurvivesTheRepairOfItsSeed(t *testing.T) {
	k := newReleaseKit(t)
	accepted := invSharedRoot(k)
	k.invRevise("sr", "A", "sr-r2", invTitle("a changed title"))
	if got := invStaleIDs(k.read("sr")); !reflect.DeepEqual(got, []string{"A", "C"}) {
		t.Fatalf("stale = %v, want [A C]", got)
	}
	inputs := invRealInputs(k, "sr", "A")
	k.exec("UPDATE dag_acceptances SET state = 'superseded' WHERE acceptance_id = ?", accepted["A"].AcceptanceID)
	again := k.acceptNode("sr", "A", acceptOpts{Suffix: "-2", Inputs: inputs})
	invAssertWhole(k, "sr", "A")
	after := k.read("sr")
	if got := invStaleIDs(after); !reflect.DeepEqual(got, []string{"C"}) {
		t.Fatalf("after A was accepted again stale = %v, want [C]: %s", got, after.brief())
	}
	obj := invStaleObject(t, after, "C")
	if after.node("C").Reason != "stale:edge:ac" || invField(obj, "cause") != "input_changed" || invField(obj, "consumed_acceptance_id") != accepted["A"].AcceptanceID ||
		invField(obj, "current_acceptance_id") != again.Acceptance.AcceptanceID {
		t.Fatalf("C = %+v %v", after.node("C"), obj)
	}
	if n := after.node("A"); n.Reason != DoneAccepted {
		t.Fatalf("A = %+v", n)
	}
	invAssertReasons(t, after)
}

// Contract 8.2 and E-25: a stale result never opens an edge. D is built on C; once A changes, C is stale, so D does not become ready and a release of it is refused and creates no child.
func TestStalePredecessorOpensNoEdge(t *testing.T) {
	k := newReleaseKit(t)
	invSharedRoot(k)
	k.putPlan("sr", int(k.snapshot("sr").Revision), "sr-r2", addRelNode("D", dag.NodeNonPR), addEdge("cd", "C", "D", dag.EdgeArtifactVerified, nil))
	if n := k.read("sr").node("D"); n.Disposition != DispReady {
		t.Fatalf("D before the revision = %+v, want ready", n)
	}
	k.invRevise("sr", "A", "sr-r3", invTitle("a changed title"))
	after := k.read("sr")
	if d := after.node("D"); d.Disposition == DispReady || d.Reason != BlockedStalePredecessor || !strings.Contains(d.Detail, "cd") {
		t.Fatalf("D = %+v, want %s naming the edge cd", d, BlockedStalePredecessor)
	}
	created, _ := k.host.counts()
	if _, err := k.release("sr", "D"); refusalReason(err) != "disposition_conflict" {
		t.Fatalf("release of D on a stale predecessor = %v", err)
	}
	if again, _ := k.host.counts(); again != created {
		t.Fatalf("a refused release created %d children", again-created)
	}
	invAssertReasons(t, after)
}

// Two readings of one store state are equal byte for byte (no clock, no map order), stale nodes included.
func TestStaleReadingIsDeterministic(t *testing.T) {
	k := newReleaseKit(t)
	invSharedRoot(k)
	k.invRevise("sr", "R", "sr-r2", invTitle("a changed title"))
	first, second := k.read("sr"), k.read("sr")
	if !reflect.DeepEqual(first.Object(), second.Object()) || first.InputDigest != second.InputDigest {
		t.Fatalf("two readings of one state differ:\n%v\n%v", first.Object(), second.Object())
	}
	if got := invStaleIDs(first); len(got) != 4 {
		t.Fatalf("stale = %v", got)
	}
}

// A consumed manifest that cannot be read is not judged (the edges out of the node already report it as blocked:manifest_tampered): the node reads as before, never stale without a reason.
func TestUnreadableConsumedManifestIsNotJudged(t *testing.T) {
	k := newReleaseKit(t)
	accepted := invSharedRoot(k)
	var manifest string
	if err := k.s.DB.QueryRow("SELECT manifest_digest FROM dag_acceptances WHERE acceptance_id = ?", accepted["A"].AcceptanceID).Scan(&manifest); err != nil {
		t.Fatal(err)
	}
	k.exec("UPDATE dag_input_manifests SET body_json = '{}' WHERE manifest_digest = ?", manifest)
	k.invRevise("sr", "A", "sr-r2", invTitle("a changed title"))
	after := k.read("sr")
	if n := after.node("A"); n.Disposition == invStale {
		t.Fatalf("A = %+v: a manifest nobody can read was judged", n)
	}
	invAssertReasons(t, after)
}

// invAsConsumed is what a rebuild of a node's manifest takes from the manifest it consumed: the author, the time, the base, the volatile snapshots and the rule version are dispatch facts, not plan
// facts.
func invAsConsumed(body map[string]any) ManifestInput {
	rule, _ := body["rule_version"].(map[string]any)
	in := ManifestInput{CreatedByTaskID: textOf(body["created_by_task_id"]), CreatedAt: textOf(body["created_at"]),
		RuleVersion: RuleVersion{SkillsDigest: textOf(rule["skills_digest"]), Model: textOf(rule["model"]), Effort: textOf(rule["effort"]), PromptTemplate: textOf(rule["prompt_template"]), RelayBuild: textOf(rule["relay_build"])}}
	if base, ok := body["base"].(map[string]any); ok {
		in.Base = &BaseRef{Repository: textOf(base["repository"]), Ref: textOf(base["ref"]), SHA: textOf(base["sha"])}
	}
	volatile, _ := body["volatile"].([]any)
	for _, item := range volatile {
		if v, ok := item.(map[string]any); ok {
			in.Volatile = append(in.Volatile, Volatile{Source: textOf(v["source"]), SnapshotURI: textOf(v["snapshot_uri"]), SHA256: textOf(v["sha256"]), CapturedAt: textOf(v["captured_at"])})
		}
	}
	return in
}

// invBuild builds the manifest of a node from the store now with BuildManifest (no file read) and fails the test when an incoming edge yields no input, so what a test compares or stores is a
// whole manifest.
func invBuild(k *releaseKit, plan, node string, in ManifestInput) map[string]any {
	k.t.Helper()
	ctx := context.Background()
	snap := k.snapshot(plan)
	n, _ := nodeOf(snap, node)
	built, findings, err := k.sched.BuildManifest(ctx, k.s.Q(ctx), plan, snap, n, in, VerifyOptions{SkipFileBytes: true})
	if err != nil {
		k.t.Fatal(err)
	}
	if inputs, _ := built["inputs"].([]any); len(inputs) != len(incomingEdges(snap, node)) {
		k.t.Fatalf("the manifest of %s cannot be built: %d inputs for %d edges (%+v)", node, len(inputs), len(incomingEdges(snap, node)), findings)
	}
	return built
}

// invRealInputs are the inputs BuildManifest gives a node now: a fixture that accepts a node with them consumed what the product would have built, not a hand-written shape.
func invRealInputs(k *releaseKit, plan, node string) []any {
	k.t.Helper()
	inputs, _ := invBuild(k, plan, node, ManifestInput{CreatedByTaskID: "parent", CreatedAt: "2026-10-02T00:00:00Z"})["inputs"].([]any)
	return inputs
}

// invRebuilt is the manifest digest a node's acceptance consumed and the digest of the manifest BuildManifest builds from the store now, built the way the consumed one was.
func invRebuilt(k *releaseKit, plan, node string) (consumed, rebuilt string) {
	k.t.Helper()
	ctx := context.Background()
	q := k.s.Q(ctx)
	acc, found, err := loadActiveAcceptance(ctx, q, plan, node)
	if err != nil || !found {
		k.t.Fatalf("no acceptance of %s: %v %v", node, found, err)
	}
	body, found, err := dag.ReadManifestOn(ctx, q, acc.ManifestDigest)
	if err != nil || !found {
		k.t.Fatalf("no consumed manifest of %s: %v %v", node, found, err)
	}
	return acc.ManifestDigest, dag.ManifestDigest(invBuild(k, plan, node, invAsConsumed(body)))
}

// invAssertWhole fails when the manifest a node consumed is not whole: its digest is the one the product builds now, and, with the dispatch facts the fixture helper leaves partial (the rule
// version, and the base of an implementation node) completed in memory, VerifyManifest finds nothing wrong with its inputs against the store (the paths of contract 4.4).
func invAssertWhole(k *releaseKit, plan, node string) {
	k.t.Helper()
	if consumed, rebuilt := invRebuilt(k, plan, node); consumed != rebuilt {
		k.t.Fatalf("the fixture is not whole: %s consumed %s and the product builds %s", node, consumed, rebuilt)
	}
	ctx := context.Background()
	q := k.s.Q(ctx)
	acc, _, err := loadActiveAcceptance(ctx, q, plan, node)
	if err != nil {
		k.t.Fatal(err)
	}
	body, _, err := dag.ReadManifestOn(ctx, q, acc.ManifestDigest)
	if err != nil {
		k.t.Fatal(err)
	}
	rule, _ := body["rule_version"].(map[string]any)
	if rule == nil {
		rule = map[string]any{}
	}
	for _, key := range []string{"skills_digest", "model", "effort", "prompt_template", "relay_build"} {
		if textOf(rule[key]) == "" {
			rule[key] = "fixture"
		}
	}
	body["rule_version"] = rule
	snap := k.snapshot(plan)
	n, _ := nodeOf(snap, node)
	if n.Kind == dag.NodeImplementation && body["base"] == nil {
		body["base"] = map[string]any{"repository": "owner/repo", "ref": "dev", "sha": head1}
	}
	body["manifest_digest"] = dag.ManifestDigest(body)
	findings, err := k.sched.VerifyManifest(ctx, q, plan, snap, n, body, VerifyOptions{SkipFileBytes: true, ArtifactRoots: []string{k.root}})
	if err != nil {
		k.t.Fatal(err)
	}
	if len(findings) != 0 {
		k.t.Fatalf("the manifest %s consumed does not verify against the store: %+v", node, findings)
	}
}

// Criterion c2 (contract E-27): an unrelated merge moves dev. I landed outside the merge lane, so the only landed commit its observation can name is the tip it saw; K consumed that landing.
// Observing the integration again after dev moved must change nothing K consumed: the same edge evidence, the same manifest digest, no stale node. Q is then revised to show the judgement
// still names the real seed (the edge qk) and not the integrated edge.
func TestUnrelatedDevMoveChangesNothing(t *testing.T) {
	k := newIntegrationKit(t)
	repo := k.repo
	k.putPlan("g", int(k.snapshot("g").Revision), "g-r2", addRelNode("Q", dag.NodeNonPR), addEdge("qk", "Q", "K", dag.EdgeArtifactVerified, nil))
	repo.git("checkout", "-q", "-b", "feature")
	feature := repo.commit("feature.txt", "feature")
	repo.git("checkout", "-q", "dev")
	k.declare("g", "I", "feature.txt")
	a := k.acceptNode("g", "I", acceptOpts{HeadSHA: feature, PR: 5, Forge: "owner/repo", Repository: repo.path})
	k.holdSlotsFor("g", "I")
	repo.git("merge", "-q", "--no-ff", "-m", "merge feature", "feature")
	k.mark(a)
	if res, err := k.observe(); err != nil || !res.Integrated {
		t.Fatalf("I did not integrate: %v %+v", err, res)
	}
	k.invSettle("g", "Q")
	k.invSettle("g", "K")
	consumed, rebuilt := invRebuilt(k.releaseKit, "g", "K")
	if consumed != rebuilt {
		t.Fatalf("before dev moves the rebuilt manifest %s is not the consumed one %s", rebuilt, consumed)
	}
	edgeBefore := k.status("g", "ik")
	if !edgeBefore.Satisfied || edgeBefore.ObservationID == "" {
		t.Fatalf("the integrated edge = %+v", edgeBefore)
	}
	devBefore := repo.git("rev-parse", "dev")

	// an unrelated landing moves dev, and the integration is observed again
	repo.commit("unrelated.txt", "an unrelated change")
	if repo.git("rev-parse", "dev") == devBefore {
		t.Fatal("dev did not move")
	}
	res, err := k.observe()
	if err != nil || len(res.Observations) != 1 || res.Observations[0].Replayed || !res.Observations[0].IsAncestor || res.Observations[0].TipSHA == devBefore || !res.Integrated {
		t.Fatalf("the second observation = %v %+v: want a new observation of the moved tip that still contains the head", err, res)
	}
	moved := false
	if edgeAfter := k.status("g", "ik"); edgeAfter != edgeBefore {
		t.Errorf("the integrated edge moved with dev:\nbefore %+v\nafter  %+v", edgeBefore, edgeAfter)
		moved = true
	}
	if consumed, rebuilt := invRebuilt(k.releaseKit, "g", "K"); consumed != rebuilt {
		t.Errorf("after dev moved the rebuilt manifest %s is not the consumed one %s: a needless rerun", rebuilt, consumed)
		moved = true
	}
	if moved {
		t.FailNow()
	}
	reading := k.read("g")
	if got := invStaleIDs(reading); len(got) != 0 {
		t.Fatalf("an unrelated merge marked %v stale: %s", got, reading.brief())
	}
	if n := reading.node("K"); n.Reason != DoneAccepted {
		t.Fatalf("K = %+v", n)
	}

	// the real seed elsewhere: Q changes, K is stale because of qk, not because of the landing
	k.invRevise("g", "Q", "g-r3", invTitle("a changed title"))
	reading = k.read("g")
	if got := invStaleIDs(reading); !reflect.DeepEqual(got, []string{"K", "Q"}) {
		t.Fatalf("stale = %v, want [K Q]: %s", got, reading.brief())
	}
	if n := reading.node("K"); n.Reason != "stale:edge:qk" {
		t.Fatalf("K = %+v, want the reason to name qk", n)
	}
	if n := reading.node("I"); n.State != StateIntegrated {
		t.Fatalf("I = %+v", n)
	}
	invAssertReasons(t, reading)
}

// Contract E-20: a node that landed is never invalidated: its result is in dev, and what rests on the landing keeps resting on it however the plan above it moves. U is the plan's node above I;
// revising it makes U stale and reaches I (a descendant), which has landed, and K (built on the landing), whose consumed manifest is the one rebuilt now.
func TestIntegratedNodeIsNeverStale(t *testing.T) {
	k := newIntegrationKit(t)
	repo := k.repo
	k.putPlan("g", int(k.snapshot("g").Revision), "g-r2", addRelNode("U", dag.NodeNonPR), addEdge("ui", "U", "I", dag.EdgeArtifactVerified, nil))
	k.invSettle("g", "U")
	repo.git("checkout", "-q", "-b", "feature")
	feature := repo.commit("feature.txt", "feature")
	repo.git("checkout", "-q", "dev")
	k.declare("g", "I", "feature.txt")
	a := k.acceptNode("g", "I", acceptOpts{HeadSHA: feature, PR: 5, Forge: "owner/repo", Repository: repo.path, Inputs: invRealInputs(k.releaseKit, "g", "I")})
	k.holdSlotsFor("g", "I")
	repo.git("merge", "-q", "--no-ff", "-m", "merge feature", "feature")
	k.mark(a)
	if res, err := k.observe(); err != nil || !res.Integrated {
		t.Fatalf("I did not integrate: %v %+v", err, res)
	}
	invAssertWhole(k.releaseKit, "g", "I")
	k.invSettle("g", "K")
	invAssertWhole(k.releaseKit, "g", "K")
	k.invRevise("g", "U", "g-r3", invTitle("revised above the landed node"))
	reading := k.read("g")
	if got := invStaleIDs(reading); !reflect.DeepEqual(got, []string{"U"}) {
		t.Fatalf("stale = %v, want [U]: a node that landed, or what consumed its landing, was marked (%s)", got, reading.brief())
	}
	if n := reading.node("I"); n.State != StateIntegrated || n.Reason != DoneIntegrated {
		t.Fatalf("I = %+v", n)
	}
	if n := reading.node("K"); n.Reason != DoneAccepted {
		t.Fatalf("K = %+v", n)
	}
	invAssertReasons(t, reading)
}

// What an integrated input hands over is which head landed, and ancestry is monotone: the tip an observation read is not a value K consumed. A manifest recorded before the earliest observation
// of the run was chosen may name the tip of a later observation of the same run as the landed commit. K consumed the tip of the MIDDLE one of three positive observations, so that it differs from
// the newest observation (what the older choice would rebuild) as well as from the earliest (what the product builds now); below a seed (S, by a decision edge that did not change) it must stay
// current. A landed commit that no observation of the current run names is another landing, and K reads stale because of the edge ik.
func TestALandedTipOfTheSameRunIsNotAChangedInput(t *testing.T) {
	setup := func(t *testing.T, landed func(middle string) string) *integrationKit {
		k := newIntegrationKit(t)
		repo := k.repo
		k.putPlan("g", int(k.snapshot("g").Revision), "g-r2", addRelNode("S", dag.NodeNonPR), addEdge("sk", "S", "K", dag.EdgeDecision, nil))
		repo.git("checkout", "-q", "-b", "feature")
		feature := repo.commit("feature.txt", "feature")
		repo.git("checkout", "-q", "dev")
		k.declare("g", "I", "feature.txt")
		a := k.acceptNode("g", "I", acceptOpts{HeadSHA: feature, PR: 5, Forge: "owner/repo", Repository: repo.path})
		k.holdSlotsFor("g", "I")
		repo.git("merge", "-q", "--no-ff", "-m", "merge feature", "feature")
		k.mark(a)
		var tips []string
		for i := 0; i < 3; i++ {
			if i > 0 {
				repo.commit("unrelated.txt", "an unrelated change "+string(rune('a'+i)))
			}
			res, err := k.observe()
			if err != nil || len(res.Observations) != 1 || res.Observations[0].Replayed || !res.Observations[0].IsAncestor {
				t.Fatalf("observation %d = %v %+v", i+1, err, res)
			}
			tips = append(tips, res.Observations[0].TipSHA)
		}
		k.invSettle("g", "S")
		if _, err := k.sched.RecordDecision(context.Background(), "g", "parent", DecisionInput{Subject: "merge holds", Digest: dig("subject sk"), Disposition: "approved", AuthorityKind: "user", AuthorityRef: "first"}); err != nil {
			t.Fatal(err)
		}
		// K consumed what the product builds now, except for the landed commit
		inputs := invRealInputs(k.releaseKit, "g", "K")
		for _, item := range inputs {
			if in := item.(map[string]any); in["edge_id"] == "ik" {
				if in["landed_sha"] != tips[0] {
					t.Fatalf("the product names %v as the landing, want the earliest tip %s", in["landed_sha"], tips[0])
				}
				in["landed_sha"] = landed(tips[1])
			}
		}
		k.acceptNode("g", "K", acceptOpts{Inputs: inputs})
		if consumed, rebuilt := invRebuilt(k.releaseKit, "g", "K"); consumed == rebuilt {
			t.Fatal("the fixture does not differ from what the product builds: the landed commit is the same")
		}
		return k
	}
	t.Run("the tip of a later observation of the same run", func(t *testing.T) {
		k := setup(t, func(middle string) string { return middle })
		k.invRevise("g", "S", "g-r3", invTitle("a changed title"))
		reading := k.read("g")
		if got := invStaleIDs(reading); !reflect.DeepEqual(got, []string{"S"}) {
			t.Fatalf("stale = %v, want [S]: the tip of another observation of the same run was read as a changed input (%s)", got, reading.brief())
		}
		invAssertReasons(t, reading)
	})
	t.Run("a landed commit no observation of the run names", func(t *testing.T) {
		k := setup(t, func(string) string { return dig("another landing")[:40] })
		k.invRevise("g", "S", "g-r3", invTitle("a changed title"))
		reading := k.read("g")
		if got := invStaleIDs(reading); !reflect.DeepEqual(got, []string{"K", "S"}) || reading.node("K").Reason != "stale:edge:ik" {
			t.Fatalf("stale = %v, K = %+v: another landing was not read as a changed input (%s)", got, reading.node("K"), reading.brief())
		}
		invAssertReasons(t, reading)
	})
}

// The same run means the same containment: positive, then reverted, then positive again is two landings. K consumed the tip of the first one; the head is contained again after a negative
// observation, so that tip belongs to an earlier run and is not named by any observation of the current one.
func TestALandingOfAnEarlierRunIsAChangedInput(t *testing.T) {
	for _, c := range []struct {
		name, landed string
		stale        []string
	}{
		{name: "the tip of the current run", landed: "t3", stale: []string{"S"}},
		{name: "the tip of the run before the negative observation", landed: "t1", stale: []string{"K", "S"}},
		{name: "the tip read by the negative observation", landed: "t2", stale: []string{"K", "S"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t)
			f.projectParent()
			f.putPlan("lr", 0, "lr-r1", addNode("I", dag.NodeImplementation), addNode("S", dag.NodeNonPR), addNode("K", dag.NodeNonPR),
				addEdge("ik", "I", "K", dag.EdgeIntegrated, nil), addEdge("sk", "S", "K", dag.EdgeDecision, nil))
			a := f.acceptNode("lr", "I", pinnedOpts)
			f.integrate(a, "owner/repo", "dev", true, true)
			f.exec("UPDATE dag_integration_observations SET tip_sha = 't1'")
			f.integrate(a, "owner/repo", "dev", false, false)
			f.exec("UPDATE dag_integration_observations SET tip_sha = 't2' WHERE observed_seq = 2")
			f.integrate(a, "owner/repo", "dev", true, false)
			f.exec("UPDATE dag_integration_observations SET tip_sha = 't3' WHERE observed_seq = 3")
			f.acceptNode("lr", "S", acceptOpts{})
			if _, err := f.sched.RecordDecision(context.Background(), "lr", "parent", DecisionInput{Subject: "merge holds", Digest: dig("subject sk"), Disposition: "approved", AuthorityKind: "user", AuthorityRef: "first"}); err != nil {
				t.Fatal(err)
			}
			var decision string
			var revision int64
			if err := f.s.DB.QueryRow("SELECT decision_id, revision FROM dag_decisions WHERE plan_id = 'lr'").Scan(&decision, &revision); err != nil {
				t.Fatal(err)
			}
			f.acceptNode("lr", "K", acceptOpts{Inputs: []any{
				doc{"edge_id": "ik", "kind": dag.EdgeIntegrated, "from_node_id": "I", "acceptance_id": a.Acceptance.AcceptanceID, "head_sha": head1, "landed_sha": c.landed},
				doc{"edge_id": "sk", "kind": dag.EdgeDecision, "from_node_id": "S", "decision_id": decision, "decision_digest": dig("subject sk"), "decision_revision": revision}}})
			n := relNode("S", dag.NodeNonPR)
			n["title"] = "a changed title"
			f.putPlan("lr", int(f.snapshot("lr").Revision), "lr-r2", doc{"op": dag.OpUpdateNode, "node": n})
			reading := f.read("lr")
			if got := invStaleIDs(reading); !reflect.DeepEqual(got, c.stale) {
				t.Fatalf("landed %s: stale = %v, want %v (%s)", c.landed, got, c.stale, reading.brief())
			}
		})
	}
}

// B-14 stays reachable on the public reader. B landed, and a landed node is never stale (contract E-20), so the first acceptance of A, which B consumed, is not a stale result the judgement
// sees: E rests on B's landing and is not marked either. Only the closure over the consumed acceptances finds that G would be built from two acceptances of A: E rests on B, which consumed the
// first, and C consumed the second (A is a non_pr node, so accepting it again is allowed; a landed node would get a follow-up node instead, E-25).
func TestMixedAcceptancesBehindALandedNodeAreStillInconsistentInputs(t *testing.T) {
	f := newFixture(t)
	f.projectParent()
	f.putPlan("fk", 0, "fk-r1", addNode("A", dag.NodeNonPR), addNode("B", dag.NodeImplementation), addNode("E", dag.NodeNonPR), addNode("C", dag.NodeNonPR), addNode("G", dag.NodeNonPR),
		addEdge("ab", "A", "B", dag.EdgeArtifactVerified, nil), addEdge("be", "B", "E", dag.EdgeIntegrated, nil), addEdge("ac", "A", "C", dag.EdgeArtifactVerified, nil),
		addEdge("eg", "E", "G", dag.EdgeArtifactVerified, nil), addEdge("cg", "C", "G", dag.EdgeArtifactVerified, nil))
	a1 := f.acceptNode("fk", "A", acceptOpts{})
	b := f.acceptNode("fk", "B", acceptOpts{Inputs: []any{consumes("ab", "A", a1)}, HeadSHA: head1, PR: 7, Forge: "owner/repo", Repository: "owner/repo"})
	f.integrate(b, "owner/repo", "dev", true, true)
	f.acceptNode("fk", "E", acceptOpts{Inputs: []any{doc{"edge_id": "be", "kind": dag.EdgeIntegrated, "from_node_id": "B", "acceptance_id": b.Acceptance.AcceptanceID, "head_sha": head1, "landed_sha": "tip"}}})
	f.exec("UPDATE dag_acceptances SET state = 'superseded' WHERE acceptance_id = ?", a1.Acceptance.AcceptanceID)
	a2 := f.acceptNode("fk", "A", acceptOpts{Suffix: "-2"})
	f.acceptNode("fk", "C", acceptOpts{Inputs: []any{consumes("ac", "A", a2)}})
	reading := f.read("fk")
	if g := reading.node("G"); g.Reason != BlockedInconsistentInputs {
		t.Fatalf("G = %+v (%s), want %s", g, reading.brief(), BlockedInconsistentInputs)
	}
	if got := invStaleIDs(reading); len(got) != 0 {
		t.Fatalf("stale = %v: a node that landed, and what rests on its landing, are not stale results", got)
	}
}

// The comparison path of the judgement: T hangs below S by a decision edge, so it is a descendant of the seed S, but what T consumed over that edge is the decision with the digest the plan
// fixed, and S changing does not change that. T's rebuilt manifest is the consumed one, so T stays current (no needless rerun through a decision edge). A decision approved again records
// another decision id and revision, which the manifest names as the value T consumed (contract 4.2): once T is below a seed it reads stale because of the edge sd. Without a seed above it nothing
// is judged, so the same re-approval marks nothing.
func TestDecisionEdgeBelowASeed(t *testing.T) {
	setup := func(t *testing.T) (*releaseKit, DecisionInput) {
		k := newReleaseKit(t)
		k.putPlan("dd", 0, "dd-r1", addRelNode("S", dag.NodeNonPR), addRelNode("T", dag.NodeNonPR), addEdge("sd", "S", "T", dag.EdgeDecision, nil))
		k.invSettle("dd", "S")
		in := DecisionInput{Subject: "merge holds", Digest: dig("subject sd"), Disposition: "approved", AuthorityKind: "user", AuthorityRef: "first"}
		if _, err := k.sched.RecordDecision(context.Background(), "dd", "parent", in); err != nil {
			t.Fatal(err)
		}
		k.invSettle("dd", "T")
		return k, in
	}
	approveAgain := func(t *testing.T, k *releaseKit, in DecisionInput) {
		in.AuthorityKind, in.AuthorityRef = "owner", "second"
		if _, err := k.sched.RecordDecision(context.Background(), "dd", "parent", in); err != nil {
			t.Fatal(err)
		}
	}
	t.Run("the decision T consumed is unchanged: T stays current below the seed", func(t *testing.T) {
		k, _ := setup(t)
		k.invRevise("dd", "S", "dd-r2", invTitle("a changed title"))
		reading := k.read("dd")
		if got := invStaleIDs(reading); !reflect.DeepEqual(got, []string{"S"}) {
			t.Fatalf("stale = %v, want [S]: %s", got, reading.brief())
		}
		if consumed, rebuilt := invRebuilt(k, "dd", "T"); consumed != rebuilt {
			t.Fatalf("T's rebuilt manifest %s is not the consumed one %s although nothing T consumed changed", rebuilt, consumed)
		}
		invAssertReasons(t, reading)
	})
	t.Run("the decision was approved again: T below the seed consumed another decision", func(t *testing.T) {
		k, in := setup(t)
		k.invRevise("dd", "S", "dd-r2", invTitle("a changed title"))
		approveAgain(t, k, in)
		if consumed, rebuilt := invRebuilt(k, "dd", "T"); consumed == rebuilt {
			t.Fatal("the re-approval did not change the decision the manifest records")
		}
		reading := k.read("dd")
		if got := invStaleIDs(reading); !reflect.DeepEqual(got, []string{"S", "T"}) {
			t.Fatalf("stale = %v, want [S T]: %s", got, reading.brief())
		}
		obj := invStaleObject(t, reading, "T")
		if reading.node("T").Reason != "stale:edge:sd" || invField(obj, "cause") != "input_changed" || invField(obj, "predecessor_node_id") != "S" ||
			invField(obj, "consumed_decision") == nil || invField(obj, "current_decision") == nil || invField(obj, "consumed_decision") == invField(obj, "current_decision") {
			t.Fatalf("T = %+v %v", reading.node("T"), obj)
		}
		invAssertReasons(t, reading)
	})
	t.Run("no seed above: nothing is judged", func(t *testing.T) {
		k, in := setup(t)
		approveAgain(t, k, in)
		if reading := k.read("dd"); len(invStaleIDs(reading)) != 0 {
			t.Fatalf("a re-approval with no revision marked %v stale: %s", invStaleIDs(reading), reading.brief())
		}
	})
}

// The scheduler page describes the stale reading as built: its section, the parameterised reason, every cause and every field of the structured object the reading prints.
func TestSchedulerPageDescribesTheInvalidationReading(t *testing.T) {
	raw, err := os.ReadFile("../../../docs/relay/dag-scheduler.md")
	if err != nil {
		t.Fatal(err)
	}
	page := string(raw)
	for _, want := range []string{"### Invalidation", "`" + StaleEdgePrefix + "<edge_id>`", "`stale`", "`staleOf`"} {
		if !strings.Contains(page, want) {
			t.Errorf("docs/relay/dag-scheduler.md does not name %s", want)
		}
	}
	for _, cause := range []string{CauseSliceChanged, CauseCriteriaChanged, CauseEdgeAdded, CauseEdgeRetired, CausePredecessorStale, CauseInputChanged} {
		if !strings.Contains(page, "`"+cause+"`") {
			t.Errorf("docs/relay/dag-scheduler.md does not name the cause %s", cause)
		}
	}
	for _, field := range (Stale{}).object() {
		if !strings.Contains(page, "`"+field.Key+"`") {
			t.Errorf("docs/relay/dag-scheduler.md does not name the field %s of the stale object", field.Key)
		}
	}
}
