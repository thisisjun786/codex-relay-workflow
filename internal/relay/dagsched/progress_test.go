package dagsched

import (
	"context"
	"database/sql"
	"errors"
	"go/parser"
	"go/token"
	"os"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dag"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// assertEveryNonMovingNodeHasAReason is criterion c3: a node that is blocked, stale, waiting or deferred carries a closed reason and a detail, and the blocked overlay lists exactly the blocked nodes.
func assertEveryNonMovingNodeHasAReason(t *testing.T, p Progress) {
	t.Helper()
	blocked := 0
	for _, n := range p.Nodes {
		switch n.Disposition {
		case DispBlocked, DispStale, DispWait, DispDefer, DispSkip:
			if !ReasonsClosed(n.Reason) || n.Detail == "" {
				t.Errorf("%s is %s without a closed reason and a detail: %q / %q", n.NodeID, n.Disposition, n.Reason, n.Detail)
			}
		}
		if n.Disposition == DispBlocked {
			blocked++
		}
	}
	if p.Blocked.Nodes != blocked || len(p.Blocked.Entries) != blocked {
		t.Errorf("the blocked overlay lists %d (%d entries), the nodes say %d blocked", p.Blocked.Nodes, len(p.Blocked.Entries), blocked)
	}
	for _, e := range p.Blocked.Entries {
		if !strings.HasPrefix(e.Reason, "blocked:") || !ReasonsClosed(e.Reason) || e.Stage == "" {
			t.Errorf("blocked entry %+v has no closed blocked reason or no stage", e)
		}
	}
}

// assertNoFalseCompletionRate is criterion c3: the document sums nothing that overlaps and offers no completion rate; the cumulative counts are each stated against the denominator, and integrated
// is a subset of accepted.
func assertNoFalseCompletionRate(t *testing.T, p Progress) {
	t.Helper()
	var walk func(path string, v any)
	walk = func(path string, v any) {
		switch x := v.(type) {
		case map[string]any:
			for key, value := range x {
				for _, word := range strings.Split(strings.ToLower(key), "_") {
					for _, banned := range []string{"rate", "percent", "pct", "complete", "completion", "total", "sum", "ratio", "done"} {
						if word == banned {
							t.Errorf("the document has a key %s.%s: no figure is summed or turned into a completion rate", path, key)
						}
					}
				}
				walk(path+"."+key, value)
			}
		case []any:
			for _, e := range x {
				walk(path+"[]", e)
			}
		}
	}
	doc := p.decoded(t)
	walk("", doc)
	c := p.Cumulative
	if c.Accepted.Of != p.Denominator.Nodes || c.Integrated.Of != p.Denominator.Nodes {
		t.Errorf("cumulative %+v is not stated against the denominator %d", c, p.Denominator.Nodes)
	}
	if c.Integrated.Nodes > c.Accepted.Nodes || c.Accepted.Nodes > p.Denominator.Nodes {
		t.Errorf("integrated %d <= accepted %d <= denominator %d does not hold", c.Integrated.Nodes, c.Accepted.Nodes, p.Denominator.Nodes)
	}
	integrated := p.stageNamed(StageIntegrated)
	if c.Integrated.Nodes != integrated.Nodes {
		t.Errorf("cumulative integrated %d is not the integrated stage %d", c.Integrated.Nodes, integrated.Nodes)
	}
}

func pinnedAcceptance(head string, pr int64) acceptOpts {
	return acceptOpts{HeadSHA: head, PR: pr, Forge: "owner/repo", Repository: "owner/repo"}
}

// Criterion c4 (contract E-01 fork and join): the fixed record of the fork/join plan, before anything runs and part way through, gives the expected distribution.
func TestProgressForkJoinDistribution(t *testing.T) {
	f := newFixture(t)
	forkJoinPlan(f, "p1")
	f.projectParent()
	p := f.progress("p1")
	wantStages(t, p, map[string][]string{
		StageReady:              {"research"},
		StageWaitingPredecessor: {"design", "impl-a", "impl-b", "join", "stack2"},
		StageWaitingDecision:    {"ship"},
	})
	if p.Cumulative.Accepted != (Measure{Nodes: 0, Of: 7}) || p.Cumulative.Integrated != (Measure{Nodes: 0, Of: 7}) {
		t.Errorf("cumulative before anything ran = %+v", p.Cumulative)
	}
	if n := p.nodeNamed("design"); n.Reason != WaitEdge("e0") || n.Disposition != DispWait {
		t.Errorf("design = %+v", n)
	}
	if n := p.nodeNamed("ship"); n.Reason != DeferAuthorityPending {
		t.Errorf("ship = %+v", n)
	}
	assertEveryNonMovingNodeHasAReason(t, p)

	f.acceptNode("p1", "research", acceptOpts{})
	f.acceptNode("p1", "design", acceptOpts{})
	a := f.acceptNode("p1", "impl-a", pinnedAcceptance(head1, 7))
	f.integrate(a, "owner/repo", "dev", true, true)
	f.reportNode("p1", "impl-b", acceptOpts{})
	p = f.progress("p1")
	wantStages(t, p, map[string][]string{
		StageAccepted:           {"research", "design"},
		StageIntegrated:         {"impl-a"},
		StageVerifying:          {"impl-b"},
		StageWaitingPredecessor: {"join"},
		StageWaitingDecision:    {"ship"},
		StageWaitingResource:    {"stack2"},
	})
	if p.Cumulative.Accepted != (Measure{Nodes: 3, Of: 7}) || p.Cumulative.Integrated != (Measure{Nodes: 1, Of: 7}) {
		t.Errorf("cumulative part way = %+v, want accepted 3 of 7 and integrated 1 of 7", p.Cumulative)
	}
	if n := p.nodeNamed("stack2"); n.Reason != DeferEditOverlap {
		t.Errorf("stack2 = %+v, want it held back by the undeclared edit regions of the running node", n)
	}
	assertEveryNonMovingNodeHasAReason(t, p)
	assertNoFalseCompletionRate(t, p)
}

// Criterion c4 (contract E-24, E-18: amend) and c3 (overlapping counts): over the shared-root plan a revision that changes one node makes it and the node built on it stale; their siblings and
// their ancestor stay accepted. The nodes that went stale are still counted as accepted, so accepted and stale overlap: neither the document nor the test adds them up.
func TestProgressAmendMakesDescendantsStale(t *testing.T) {
	k := newReleaseKit(t)
	invSharedRoot(k)
	p := k.progress("sr")
	wantStages(t, p, map[string][]string{StageAccepted: {"R", "A", "B", "C"}})
	if p.Cumulative.Accepted != (Measure{Nodes: 4, Of: 4}) {
		t.Errorf("cumulative accepted = %+v", p.Cumulative.Accepted)
	}
	k.invRevise("sr", "A", "sr-r2", invTitle("A, retitled"))
	p = k.progress("sr")
	wantStages(t, p, map[string][]string{StageAccepted: {"R", "B"}, StageStale: {"A", "C"}})
	if n := p.nodeNamed("A"); n.Reason != invSliceChanged || n.Stale == nil || n.Stale.Cause != CauseSliceChanged || n.Stale.Seed != "A" {
		t.Errorf("A = %+v", n)
	}
	if n := p.nodeNamed("C"); n.Reason != "stale:edge:ac" || n.Stale == nil || n.Stale.Seed != "A" || n.Stale.EdgeID != "ac" {
		t.Errorf("C = %+v", n)
	}
	if p.Cumulative.Accepted != (Measure{Nodes: 4, Of: 4}) {
		t.Errorf("a stale node is still an accepted one: cumulative accepted = %+v, want 4 of 4 beside 2 accepted and 2 stale stages", p.Cumulative.Accepted)
	}
	// the revision that did it: the denominator did not move, the node whose spec changed is named
	if len(p.Revisions) != 2 {
		t.Fatalf("revisions = %+v", p.Revisions)
	}
	if r := p.Revisions[1]; r.Nodes != 4 || r.PreviousNodes != 4 || r.Delta != 0 || r.DenominatorChanged || !equalStrings(r.Updated, []string{"A"}) || len(r.Added) != 0 || len(r.Retired) != 0 {
		t.Errorf("revision 2 = %+v", r)
	}
	assertEveryNonMovingNodeHasAReason(t, p)
	assertNoFalseCompletionRate(t, p)
	if err := stalePrinted(t, p); err != nil {
		t.Error(err)
	}
}

// stalePrinted: the stale object the command prints names the cause and the versions and leaves out the digest of a manifest rebuilt from the file system.
func stalePrinted(t *testing.T, p Progress) error {
	t.Helper()
	nodes, _ := p.decoded(t)["nodes"].([]any)
	for _, item := range nodes {
		n, _ := item.(map[string]any)
		stale, has := n["stale"].(map[string]any)
		if n["stage"] != StageStale {
			if has {
				return errors.New(n["node_id"].(string) + " is not stale and prints a stale object")
			}
			continue
		}
		if !has || stale["cause"] == nil || stale["seed_node_id"] == nil {
			return errors.New(n["node_id"].(string) + " is stale and prints no cause or seed")
		}
		if _, leaked := stale["rebuilt_manifest_digest"]; leaked {
			return errors.New("the stale object prints rebuilt_manifest_digest, which depends on the file system")
		}
	}
	return nil
}

// Criterion c4 (contract 3.2, 7.4: pause): a paused node stays where the relay says it is, still holds its slot, and a paused node that was accepted is still an accepted one.
func TestProgressPauseDistribution(t *testing.T) {
	f := newFixture(t)
	f.projectParent()
	f.putPlan("pz", 0, "pz-r1", addNode("x", dag.NodeImplementation), addNode("y", dag.NodeNonPR), addNode("z", dag.NodeNonPR), addEdge("yz", "y", "z", dag.EdgeArtifactVerified, nil))
	f.startNode("pz", "x")
	f.holdSlotsFor("pz", "x")
	f.acceptNode("pz", "y", acceptOpts{Status: "paused"})
	f.holdSlotsFor("pz", "y")
	f.exec("UPDATE relationships SET status = 'paused' WHERE relationship_id = 'rel-pz-x'")
	p := f.progress("pz")
	wantStages(t, p, map[string][]string{StagePaused: {"x", "y"}, StageReady: {"z"}})
	for _, id := range []string{"x", "y"} {
		if n := p.nodeNamed(id); !n.HoldsSlot || n.Links.Relationship == nil || n.Links.Relationship.Status != "paused" {
			t.Errorf("%s = %+v: a paused node keeps its slot and names its paused relationship (contract 7.4)", id, n)
		}
	}
	if n := p.nodeNamed("y"); n.AcceptanceID == "" {
		t.Errorf("y was accepted before the pause and the pause does not revoke it: %+v", n)
	}
	if p.Cumulative.Accepted != (Measure{Nodes: 1, Of: 3}) {
		t.Errorf("cumulative accepted = %+v, want the paused accepted node counted: 1 of 3", p.Cumulative.Accepted)
	}
	if n := p.nodeNamed("z"); n.HoldsSlot {
		t.Errorf("z holds no slot: %+v", n)
	}
	assertEveryNonMovingNodeHasAReason(t, p)
	assertNoFalseCompletionRate(t, p)
}

// Criterion c4 and the first risk of the design: blocked is an overlay, not a stage. A node that is accepted and evicted, one whose child reported blocked, one whose creation is unknown, one with no
// relationship behind its execution and one whose managed start was released all stay in the stage of their derived state and are listed, with their reasons, as blocked.
func TestProgressBlockedOverlay(t *testing.T) {
	f := newFixture(t)
	f.projectParent()
	f.putPlan("bk", 0, "bk-r1", addNode("ev", dag.NodeImplementation), addNode("eff", dag.NodeImplementation), addNode("rep", dag.NodeNonPR), addNode("cre", dag.NodeNonPR), addNode("amb", dag.NodeNonPR), addNode("abn", dag.NodeNonPR))
	evicted := f.acceptNode("bk", "ev", pinnedAcceptance(head1, 7))
	f.evict(evicted)
	f.acceptNode("bk", "eff", pinnedAcceptance(progHead2, 8))
	f.mergeTurn("mtn-eff", "owner/repo", "dev", "unknown", progHead2)
	rid := f.startNode("bk", "rep")
	f.childBlocked(rid)
	_, request := f.releaseRow("bk", "cre")
	f.managedRow(request, "CRW-cre", "create_armed", "")
	f.exec("INSERT INTO dag_node_executions (plan_id, node_id, relationship_id, execution_generation, manifest_digest, kind, managed_request_id) VALUES ('bk', 'amb', 'rel-missing', 1, ?, 'initial', NULL)", dig("amb manifest"))
	_, request = f.releaseRow("bk", "abn")
	f.managedRow(request, "CRW-abn", "released", "")
	p := f.progress("bk")
	wantStages(t, p, map[string][]string{
		StageAccepted: {"ev", "eff"}, StageReported: {"rep"}, StageCreationUnknown: {"cre"}, StageAmbiguous: {"amb"}, StageReleasing: {"abn"},
	})
	want := map[string]string{"ev": BlockedEvicted, "eff": BlockedEffectUnknown, "rep": BlockedInputUnverifiedAtUse, "cre": BlockedCreationUnknown, "amb": BlockedAmbiguousHead, "abn": BlockedReleaseAbandoned}
	for id, reason := range want {
		if n := p.nodeNamed(id); n.Reason != reason || n.Disposition != DispBlocked {
			t.Errorf("%s = %s %s, want %s", id, n.Disposition, n.Reason, reason)
		}
	}
	if p.Blocked.Nodes != 6 {
		t.Errorf("blocked overlay = %+v, want six nodes", p.Blocked)
	}
	for _, e := range p.Blocked.Entries {
		if e.Stage != p.nodeNamed(e.NodeID).Stage {
			t.Errorf("overlay entry %+v names another stage than the node's", e)
		}
	}
	if p.Cumulative.Accepted != (Measure{Nodes: 2, Of: 6}) {
		t.Errorf("accepted work that is blocked is still accepted: %+v", p.Cumulative.Accepted)
	}
	assertEveryNonMovingNodeHasAReason(t, p)
	assertNoFalseCompletionRate(t, p)
}

// Criterion c3: a revision change always shows the change in the denominator. Five revisions: the plan starts with three nodes, a fourth is added, one is retired, one is updated (same id, new
// version) and one is replaced (one id out, one in, the count the same).
func TestProgressRevisionDenominators(t *testing.T) {
	f := newFixture(t)
	f.putPlan("p", 0, "p-r1", addNode("a", dag.NodeNonPR), addNode("b", dag.NodeNonPR), addNode("c", dag.NodeNonPR),
		addEdge("ab", "a", "b", dag.EdgeArtifactVerified, nil), addEdge("bc", "b", "c", dag.EdgeArtifactVerified, nil))
	f.putPlan("p", 1, "p-r2", addNode("d", dag.NodeNonPR))
	f.acceptNode("p", "c", acceptOpts{}) // accepted while it is part of the plan, then retired below
	f.holdSlotsFor("p", "d")             // d will be replaced and still holds its slot
	f.putPlan("p", 2, "p-r3", doc{"op": dag.OpRetireEdge, "edge_id": "bc"}, doc{"op": dag.OpRetireNode, "node_id": "c"})
	retitled := nodeDoc("a", dag.NodeNonPR)
	retitled["title"] = "a, retitled"
	f.putPlan("p", 3, "p-r4", doc{"op": dag.OpUpdateNode, "node": retitled})
	f.putPlan("p", 4, "p-r5", doc{"op": dag.OpReplaceNode, "node": nodeDoc("d2", dag.NodeNonPR), "supersedes_node_id": "d"})
	p := f.progress("p")
	type row struct {
		nodes, previous, delta int
		changed                bool
		added, retired, upd    []string
	}
	want := []row{
		{3, 0, 3, true, []string{"a", "b", "c"}, nil, nil},
		{4, 3, 1, true, []string{"d"}, nil, nil},
		{3, 4, -1, true, nil, []string{"c"}, nil},
		{3, 3, 0, false, nil, nil, []string{"a"}},
		{3, 3, 0, true, []string{"d2"}, []string{"d"}, nil},
	}
	if len(p.Revisions) != len(want) {
		t.Fatalf("revisions = %d, want %d", len(p.Revisions), len(want))
	}
	for i, w := range want {
		r := p.Revisions[i]
		if r.Revision != int64(i+1) || r.Nodes != w.nodes || r.PreviousNodes != w.previous || r.Delta != w.delta || r.DenominatorChanged != w.changed ||
			!equalStrings(r.Added, w.added) || !equalStrings(r.Retired, w.retired) || !equalStrings(r.Updated, w.upd) {
			t.Errorf("revision %d = %+v, want %+v", i+1, r, w)
		}
		var digest, request string
		if err := f.s.DB.QueryRow("SELECT state_digest, request_id FROM dag_plan_revisions WHERE plan_id = 'p' AND revision_no = ?", i+1).Scan(&digest, &request); err != nil {
			t.Fatal(err)
		}
		if r.StateDigest != digest || r.RequestID != request {
			t.Errorf("revision %d prints digest %s request %s, the log recorded %s and %s", i+1, r.StateDigest, r.RequestID, digest, request)
		}
	}
	d := p.Denominator
	if d.Revision != 5 || d.Nodes != 3 || d.PreviousNodes != 3 || d.Delta != 0 || !d.Changed || d.LastChangedRevision != 5 {
		t.Errorf("denominator at the head = %+v: the replacement of a node changes it though the count is the same", d)
	}
	// the printed document says it for every revision, so a reader never has to infer it
	revisions, _ := p.decoded(t)["revisions"].([]any)
	for i, item := range revisions {
		r, _ := item.(map[string]any)
		if _, ok := r["denominator_changed"].(bool); !ok || r["denominator_changed"] != want[i].changed {
			t.Errorf("printed revision %d = %v: denominator_changed is missing or wrong", i+1, r)
		}
	}
	// what left the denominator: an accepted node and a node that still holds its slot are not hidden by it
	o := p.Outside
	if o.Nodes != 2 || o.WithAcceptance != 1 || o.HoldingSlot != 1 || !equalStrings(o.NodeIDs, []string{"c", "d"}) {
		t.Errorf("outside the denominator = %+v, want c (accepted) and d (holding a slot)", o)
	}
	if p.Cumulative.Accepted != (Measure{Nodes: 0, Of: 3}) {
		t.Errorf("a retired node is not in the numerator either: %+v", p.Cumulative.Accepted)
	}
	assertNoFalseCompletionRate(t, p)
}

// Criterion c3: activity is never progress. Polls of the child's turn, observations of its lifecycle, turns admitted and token usage of a dimension nobody limited are written again and again,
// the scheduler's clock is moved far ahead, and the document does not change by a byte.
func TestProgressActivityIsNeverProgress(t *testing.T) {
	f := newFixture(t)
	f.projectParent()
	f.putPlan("a", 0, "a-r1", addNode("run", dag.NodeNonPR), addNode("cand", dag.NodeNonPR))
	rid := f.startNode("a", "run")
	before := f.progress("a")
	wantStages(t, before, map[string][]string{StageRunning: {"run"}, StageReady: {"cand"}})
	if again := f.progress("a"); again.printed() != before.printed() {
		t.Fatal("two queries of one store state print different documents")
	}
	for i := 0; i < 5; i++ {
		turn := "turn-" + strconv.Itoa(i)
		f.exec("INSERT INTO poll_observations (relationship_id, execution_generation, turn_id, last_status, last_polled_at, last_attempt_at) VALUES (?, 1, ?, 'inProgress', ?, ?)", rid, turn, f.clock(), f.clock())
		f.exec("INSERT INTO generation_turns (relationship_id, execution_generation, turn_id, evidence, admitted_at) VALUES (?, 1, ?, 'turn_started', ?)", rid, turn, f.clock())
		f.exec("INSERT OR REPLACE INTO recipient_lifecycle (task_id, runtime_status, archived, goal_status, can_accept_input, deliverable, observed_at) VALUES ('child-run', 'active', 0, 'active', 1, 'yes', ?)", f.clock())
		if err := f.s.ObserveExecutionUsage(context.Background(), store.ExecutionUsageRow{ScopeKind: "project", ScopeKey: "P-TEST", Dimension: "tokens", Observed: float64(1000 * (i + 1)), ObservedBy: "daemon", Method: "poll", ObservedAt: f.clock()}); err != nil {
			t.Fatal(err)
		}
	}
	f.sched.Now = func() string { return "2099-01-01T00:00:00.000000+00:00" }
	after := f.progress("a")
	if after.printed() != before.printed() {
		t.Errorf("activity changed the document:\n--- before\n%s\n--- after\n%s", before.printed(), after.printed())
	}
	if n := after.nodeNamed("run"); n.Stage != StageRunning || n.Disposition == DispBlocked || n.Disposition == DispStale {
		t.Errorf("a node that only heartbeats is running, never overdue, stalled or failed: %+v", n)
	}
}

// Criterion c3, the distinction the activity test leaves open: a limit someone declared and a usage someone recorded against it are a resource fact. It decides whether a node that has not started
// reads ready or waiting on a resource, and it never moves a node that started.
func TestProgressResourceIsNotActivity(t *testing.T) {
	f := newFixture(t)
	f.projectParent()
	f.putPlan("r", 0, "r-r1", addNode("run", dag.NodeNonPR), addNode("cand", dag.NodeNonPR))
	f.startNode("r", "run")
	f.declareLimit("project", "P-TEST", "tokens", 1000)
	observe := func(v float64) {
		if err := f.s.ObserveExecutionUsage(context.Background(), store.ExecutionUsageRow{ScopeKind: "project", ScopeKey: "P-TEST", Dimension: "tokens", Observed: v, ObservedBy: "parent", Method: "usage-observe", ObservedAt: f.clock()}); err != nil {
			t.Fatal(err)
		}
	}
	p := f.progress("r")
	wantStages(t, p, map[string][]string{StageRunning: {"run"}, StageWaitingResource: {"cand"}})
	if n := p.nodeNamed("cand"); n.Reason != DeferCapacityUnmeasured {
		t.Errorf("an enforced ceiling nobody measured = %+v", n)
	}
	observe(400)
	wantStages(t, f.progress("r"), map[string][]string{StageRunning: {"run"}, StageReady: {"cand"}})
	observe(1000)
	p = f.progress("r")
	wantStages(t, p, map[string][]string{StageRunning: {"run"}, StageWaitingResource: {"cand"}})
	if n := p.nodeNamed("cand"); n.Reason != DeferNoCapacity {
		t.Errorf("usage at the ceiling = %+v", n)
	}
}

// Every branch of the stage rule, and the states the rule refuses.
func TestProgressStageOf(t *testing.T) {
	cases := []struct {
		state, disposition, reason string
		want                       string
	}{
		{StateWaiting, DispWait, WaitEdge("e1"), StageWaitingPredecessor},
		{StateWaiting, DispBlocked, BlockedStaleHead, StageWaitingPredecessor},
		{StateWaiting, DispBlocked, BlockedInputMissing, StageWaitingPredecessor},
		{StateWaiting, DispBlocked, BlockedPredecessorCancelled, StageWaitingPredecessor},
		{StateWaiting, DispBlocked, BlockedDecisionMismatch, StageWaitingDecision},
		{StateWaiting, DispDefer, DeferAuthorityPending, StageWaitingDecision},
		{StateReady, DispReady, "", StageReady},
		{StateReady, DispDefer, DeferNoCapacity, StageWaitingResource},
		{StateReady, DispDefer, DeferCapacityUnmeasured, StageWaitingResource},
		{StateReady, DispDefer, DeferEditOverlap, StageWaitingResource},
		{StateReady, DispDefer, DeferMergeWindow, StageWaitingResource},
		{StateReady, DispDefer, DeferOwnershipUnverified, StageWaitingResource},
		{StateReady, DispSkip, SkipAlreadyOwned, StageWaitingResource},
		{StateReleasing, DispSkip, SkipAlreadyOwned, StageReleasing},
		{StateReleasing, DispBlocked, BlockedReleaseAbandoned, StageReleasing},
		{StateCreationUnknown, DispBlocked, BlockedCreationUnknown, StageCreationUnknown},
		{StateRunning, DispSkip, SkipAlreadyOwned, StageRunning},
		{StateReported, DispSkip, SkipAlreadyOwned, StageReported},
		{StateReported, DispBlocked, BlockedInputUnverifiedAtUse, StageReported},
		{StateVerifying, DispSkip, SkipAlreadyOwned, StageVerifying},
		{StateCorrecting, DispSkip, SkipAlreadyOwned, StageCorrecting},
		{StateAccepted, DispDone, DoneAccepted, StageAccepted},
		{StateAccepted, DispBlocked, BlockedEvicted, StageAccepted},
		{StateAccepted, DispBlocked, BlockedEffectUnknown, StageAccepted},
		{StateIntegrated, DispDone, DoneIntegrated, StageIntegrated},
		{StateStale, DispStale, StaleSliceChanged, StageStale},
		{StateStale, DispStale, StaleEdge("e1"), StageStale},
		{StatePausedNode, DispSkip, SkipAlreadyOwned, StagePaused},
		{StateCancelled, DispSkip, SkipAlreadyOwned, StageCancelled},
		{StateClosedNode, DispSkip, SkipAlreadyOwned, StageClosed},
		{StateAmbiguousNode, DispBlocked, BlockedAmbiguousHead, StageAmbiguous},
		{StatePlanned, DispSkip, SkipAlreadyOwned, ""},   // a reading never emits it: refused
		{"frozen", DispSkip, SkipAlreadyOwned, ""},       // not a derived state: refused
		{StateWaiting, "unheard_of", WaitEdge("e1"), ""}, // an unwaiting disposition on a waiting node: refused
		{StateReady, DispDone, DoneAccepted, ""},         // a done node that is not owned: refused
	}
	for _, c := range cases {
		got, err := stageOf(NodeReading{NodeID: "n", State: c.state, Disposition: c.disposition, Reason: c.reason, Detail: "d"})
		if c.want == "" {
			var invariant *InvariantError
			if !errors.As(err, &invariant) {
				t.Errorf("stageOf(%s %s %s) = %q, %v, want an InvariantError", c.state, c.disposition, c.reason, got, err)
			}
			continue
		}
		if err != nil || got != c.want {
			t.Errorf("stageOf(%s %s %s) = %q, %v, want %s", c.state, c.disposition, c.reason, got, err, c.want)
		}
	}
	// every stage the rule can return is in the closed list and each appears once
	seen := map[string]bool{}
	for _, s := range ProgressStages {
		if seen[s] {
			t.Errorf("stage %s is listed twice", s)
		}
		seen[s] = true
	}
	for _, c := range cases {
		if c.want != "" && !seen[c.want] {
			t.Errorf("stage %s is returned by the rule and missing from ProgressStages", c.want)
		}
	}
}

func syntheticInput(nodes ...NodeReading) ProgressInput {
	ids := make([]string, len(nodes))
	for i, n := range nodes {
		ids[i] = n.NodeID
	}
	return ProgressInput{
		Reading:    Reading{PlanID: "p", PlanRevision: 1, StateDigest: dig("p"), Nodes: nodes},
		ProjectKey: "P-TEST",
		Revisions:  []RevisionCount{{Revision: 1, Nodes: len(nodes), PreviousNodes: 0, Delta: len(nodes), DenominatorChanged: true, Added: ids}},
		Facts:      map[string]NodeFacts{},
	}
}

// Criterion c3 (0 blocked nodes without a reason): the projection refuses, as a failure of the host, a reading that would print a blocked or stale node it cannot explain, a state it has no
// stage for, and revisions that do not end at the reading's own denominator.
func TestProgressRefusesWhatItCannotExplain(t *testing.T) {
	good := NodeReading{NodeID: "n", IssueKey: "CRW-n", Kind: dag.NodeNonPR, State: StateRunning, Disposition: DispSkip, Reason: SkipAlreadyOwned, Detail: "relay state requested"}
	if _, err := ProjectProgress(syntheticInput(good)); err != nil {
		t.Fatalf("a sound reading is refused: %v", err)
	}
	cases := map[string]ProgressInput{
		"a blocked node without a reason":    syntheticInput(NodeReading{NodeID: "n", State: StateAccepted, Disposition: DispBlocked, Detail: "x"}),
		"a blocked node with an open reason": syntheticInput(NodeReading{NodeID: "n", State: StateAccepted, Disposition: DispBlocked, Reason: "blocked:made_up", Detail: "x"}),
		"a stale node without a reason":      syntheticInput(NodeReading{NodeID: "n", State: StateStale, Disposition: DispStale, Detail: "x"}),
		"a stale node with a blocked reason": syntheticInput(NodeReading{NodeID: "n", State: StateStale, Disposition: DispStale, Reason: BlockedEvicted, Detail: "x"}),
		"a state with no stage":              syntheticInput(NodeReading{NodeID: "n", State: "frozen", Disposition: DispSkip, Reason: SkipAlreadyOwned, Detail: "x"}),
	}
	mismatched := syntheticInput(good)
	mismatched.Revisions[0].Nodes = 2
	cases["revisions that end at another denominator"] = mismatched
	empty := syntheticInput(good)
	empty.Revisions = nil
	cases["no revisions at all"] = empty
	for name, in := range cases {
		_, err := ProjectProgress(in)
		var invariant *InvariantError
		if !errors.As(err, &invariant) {
			t.Errorf("%s: err = %v, want an InvariantError", name, err)
		}
	}
}

// The links: exact identifiers, null where the store holds nothing, never guessed.
func TestProgressLinks(t *testing.T) {
	f := newFixture(t)
	f.projectParent()
	f.putPlan("l", 0, "l-r1", addNode("run", dag.NodeNonPR), addNode("rel", dag.NodeNonPR), addNode("acc", dag.NodeImplementation), addNode("rep", dag.NodeImplementation),
		addNode("sup", dag.NodeImplementation), addNode("wd", dag.NodeImplementation), addNode("frk", dag.NodeImplementation), addNode("plain", dag.NodeNonPR))
	// a node that runs: relationship, thread, one execution, no pull request
	f.startNode("l", "run")
	// a node being released: the managed request and the child it already has, no relationship yet
	_, request := f.releaseRow("l", "rel")
	f.managedRow(request, "CRW-rel", "create_armed", "accepted")
	f.exec("UPDATE managed_start_requests SET child_task_id = 'child-created' WHERE request_id = ?", request)
	// an accepted implementation node: the forge row and the accepted head
	f.acceptNode("l", "acc", pinnedAcceptance(head1, 7))
	// a reported node whose current head has a work report with a pull request: the link exists before any acceptance
	ridRep, evRep := f.seedReceived("l", "rep")
	f.workReport(ridRep, evRep, 1, dig("revision "+ridRep), 11, progHead2)
	// a node whose report belongs to a head that a newer revision superseded: that pull request is not this head's
	ridSup, evSup := f.seedReceived("l", "sup")
	f.workReport(ridSup, evSup, 1, dig("revision "+ridSup), 12, head1)
	f.supersedeReport(ridSup, "sup", "l", "newer")
	// a node whose newest work report names no pull request: it withdrew the link, and the earlier submission that named one is not consulted
	ridWd, evWd := f.seedReceived("l", "wd")
	f.workReport(ridWd, evWd, 1, dig("revision "+ridWd), 14, head1)
	f.exec("INSERT INTO work_reports (event_id, submission_no, relationship_id, execution_generation, revision_hash, repository, pr_number, pr_url, head_sha, cxc_status, cxc_reason, contract_version, summary, next_action, recorded_at)"+
		" VALUES (?, 2, ?, 1, ?, 'owner/repo', NULL, NULL, NULL, 'DONE', 'proved', 'v1', 'done', 'merge', ?)", evWd, ridWd, dig("revision "+ridWd), f.clock())
	// a node whose head is ambiguous: two revisions that do not name each other
	ridFrk := f.startNode("l", "frk")
	f.finalReport(ridFrk, "evt-frk-1", dig("frk one"))
	f.finalReport(ridFrk, "evt-frk-2", dig("frk two"))
	f.workReport(ridFrk, "evt-frk-1", 1, dig("frk one"), 13, head1)
	p := f.progress("l")

	if l := p.nodeNamed("run").Links; l.Relationship == nil || l.Relationship.ID != "rel-l-run" || l.Relationship.Generation != 1 || l.Relationship.Status != "active" ||
		l.Thread == nil || l.Thread.ChildTaskID != "child-run" || l.Thread.ParentTaskID != "parent" || len(l.Executions) != 1 || l.Executions[0].Kind != "initial" || l.PullRequest != nil || l.Managed != nil {
		t.Errorf("run links = %+v", l)
	}
	if l := p.nodeNamed("rel").Links; l.Relationship != nil || l.Managed == nil || l.Managed.RequestID != request || l.Managed.State != "create_armed" || l.Managed.ReceiptStatus != "accepted" ||
		l.Thread == nil || l.Thread.ChildTaskID != "child-created" || l.Thread.ParentTaskID != "" || len(l.Executions) != 0 {
		t.Errorf("rel links = %+v", l)
	}
	acc := p.nodeNamed("acc")
	if pr := acc.Links.PullRequest; pr == nil || pr.Repository != "owner/repo" || pr.Number != 7 || pr.HeadSHA != head1 || pr.Source != PRSourceAcceptance || acc.AcceptanceID == "" {
		t.Errorf("acc pull request = %+v", acc.Links.PullRequest)
	}
	if pr := p.nodeNamed("rep").Links.PullRequest; pr == nil || pr.Repository != "owner/repo" || pr.Number != 11 || pr.HeadSHA != progHead2 || pr.Source != PRSourceWorkReport || pr.EventID != evRep ||
		pr.URL != "https://forge.example/owner/repo/pull/11" {
		t.Errorf("rep pull request = %+v, want the work report of the head event", pr)
	}
	if pr := p.nodeNamed("sup").Links.PullRequest; pr != nil {
		t.Errorf("sup pull request = %+v: the report belongs to a superseded head and must not become the new head's link", pr)
	}
	if pr := p.nodeNamed("wd").Links.PullRequest; pr != nil {
		t.Errorf("wd pull request = %+v: the newest submission names none, so the older one's link is withdrawn", pr)
	}
	if n := p.nodeNamed("frk"); n.Stage != StageAmbiguous || n.Links.PullRequest != nil {
		t.Errorf("frk = %+v: an ambiguous head names no pull request", n)
	}
	if pr := p.nodeNamed("plain").Links; pr.Relationship != nil || pr.Thread != nil || pr.PullRequest != nil || pr.Managed != nil || len(pr.Executions) != 0 {
		t.Errorf("a node nobody touched has no links: %+v", pr)
	}
	// the printed form: absent links are null, not missing and not empty strings
	nodes, _ := p.decoded(t)["nodes"].([]any)
	for _, item := range nodes {
		n, _ := item.(map[string]any)
		if n["node_id"] != "plain" {
			continue
		}
		links, _ := n["links"].(map[string]any)
		for _, key := range []string{"relationship", "thread", "managed_start", "pull_request"} {
			if value, present := links[key]; !present || value != nil {
				t.Errorf("plain links.%s = %v, present %v: want an explicit null", key, value, present)
			}
		}
		if list, _ := links["executions"].([]any); links["executions"] == nil || len(list) != 0 {
			t.Errorf("plain links.executions = %v: want an empty list", links["executions"])
		}
	}
	// a second execution of one node is listed too: the current relationship is one of them
	f.exec("INSERT INTO dag_node_executions (plan_id, node_id, relationship_id, execution_generation, manifest_digest, kind, managed_request_id) VALUES ('l', 'run', 'rel-older', 1, ?, 'child_replacement', NULL)", dig("older"))
	if l := f.progress("l").nodeNamed("run").Links; len(l.Executions) != 2 {
		t.Errorf("executions = %+v, want both", l.Executions)
	}
}

// A release that was closed (the abandoned managed start ended, its slot returned) no longer owns the node: the node is planned again and shows no managed start. The release that follows a
// close of the same manifest is the node's open intent under its successor request id, and that is the managed start the node shows.
func TestProgressManagedStartFollowsTheOpenIntent(t *testing.T) {
	f := newFixture(t)
	f.projectParent()
	f.putPlan("c", 0, "c-r1", addNode("n", dag.NodeNonPR))
	digest, request := f.releaseRow("c", "n")
	f.managedRow(request, "CRW-n", "released", "")
	p := f.progress("c")
	if n := p.nodeNamed("n"); n.Stage != StageReleasing || n.Reason != BlockedReleaseAbandoned || n.Links.Managed == nil || n.Links.Managed.RequestID != request || n.Links.Managed.State != "released" {
		t.Fatalf("an abandoned release = %+v links %+v", n, n.Links)
	}
	f.exec("INSERT INTO dag_release_recoveries (plan_id, node_id, manifest_digest, abandoned_request_id, action, slot_released, reason, recorded_by, recorded_at) VALUES ('c', 'n', ?, ?, 'closed', 1, 'abandoned', 'parent', ?)", digest, request, f.clock())
	p = f.progress("c")
	if n := p.nodeNamed("n"); n.Stage != StageReady || n.Links.Managed != nil || n.Links.Thread != nil {
		t.Errorf("a closed release still owns the node: %+v links %+v", n, n.Links)
	}
	successor := "dag-" + dig("successor")[:40]
	f.managedRow(successor, "CRW-n", "create_armed", "accepted")
	f.exec("UPDATE managed_start_requests SET child_task_id = 'child-again' WHERE request_id = ?", successor)
	f.exec("INSERT INTO dag_release_recoveries (plan_id, node_id, manifest_digest, abandoned_request_id, action, successor_request_id, request_sha256, request_json, marker_root, socket, state_selector, reason, recorded_by, recorded_at)"+
		" VALUES ('c', 'n', ?, ?, 'rereleased', ?, ?, '{}', 'markers', 'socket', 'state', 'released again', 'parent', ?)", digest, request, successor, dig("request"), f.clock())
	p = f.progress("c")
	if n := p.nodeNamed("n"); n.Stage != StageReleasing || n.Links.Managed == nil || n.Links.Managed.RequestID != successor || n.Links.Thread == nil || n.Links.Thread.ChildTaskID != "child-again" {
		t.Errorf("the successor release = %+v links %+v", n, n.Links)
	}
}

// The store-only path (criterion c2), through the production call and the layers below it: Progress marks its context, and under the mark no artifact is stat'ed and the assignment view
// spells recovery commands with a fixed program name. dag-ready, which does not mark, is unchanged, and the control proves the fixture reaches the stat. The plan is a diamond: d rests on p1 and
// p2; when p1 changes d is rebuilt, and p2, which did not change, hands over an artifact whose receipt declared no size.
func TestProgressStoreOnlyBypasses(t *testing.T) {
	f := newFixture(t)
	f.projectParent()
	f.putPlan("m", 0, "m-r1", addNode("p1", dag.NodeNonPR), addNode("p2", dag.NodeNonPR), addNode("d", dag.NodeNonPR),
		addEdge("ea", "p2", "d", dag.EdgeArtifactVerified, nil), addEdge("eb", "p1", "d", dag.EdgeArtifactVerified, nil))
	acc1 := f.unsizedAccepted("m", "p1", nil)
	acc2 := f.unsizedAccepted("m", "p2", nil)
	f.unsizedAccepted("m", "d", []any{consumes("ea", "p2", acc2), consumes("eb", "p1", acc1)})
	ctx := context.Background()
	calls := countStats(t)

	// the layer below: one input of an unsized predecessor
	e := f.edge(f.snapshot("m"), "ea")
	status := EdgeStatus{Satisfied: true, AcceptanceID: acc2.Acceptance.AcceptanceID}
	if _, finding, err := f.sched.buildInput(ctx, f.s.Q(ctx), e, status, VerifyOptions{SkipFileBytes: true}); err != nil || finding != nil {
		t.Fatalf("buildInput: %v %v", finding, err)
	}
	if *calls != 1 {
		t.Fatalf("an unsized artifact is stat'ed %d times by an unmarked build, want 1: the fixture does not reach the size fallback", *calls)
	}
	*calls = 0
	if _, finding, err := f.sched.buildInput(withMark(ctx), f.s.Q(ctx), e, status, VerifyOptions{SkipFileBytes: true}); err != nil || finding != nil {
		t.Fatalf("buildInput under the mark: %v %v", finding, err)
	}
	if *calls != 0 {
		t.Errorf("an artifact was stat'ed %d times under the store-only mark", *calls)
	}

	// the production call: p1 changes, so judging d rebuilds its manifest from the store
	retitled := nodeDoc("p1", dag.NodeNonPR)
	retitled["title"] = "p1, retitled"
	f.putPlan("m", 1, "m-r2", doc{"op": dag.OpUpdateNode, "node": retitled})
	*calls = 0
	reading := f.read("m")
	if *calls == 0 {
		t.Fatalf("the control did not reach the size fallback: dag-ready read %s", reading.brief())
	}
	*calls = 0
	p := f.progress("m")
	if *calls != 0 {
		t.Errorf("Progress stat'ed an artifact %d times", *calls)
	}
	wantStages(t, p, map[string][]string{StageAccepted: {"p2"}, StageStale: {"p1", "d"}})

	// the assignment view: a fixed program name under the mark, the executable's own resolution otherwise
	if view := f.sched.assignmentView(ctx); view.Program != nil {
		t.Errorf("an unmarked view has a program override")
	}
	view := f.sched.assignmentView(withMark(ctx))
	if view.Program == nil || len(view.Program()) != 1 || view.Program()[0] != progressProgramName {
		t.Errorf("the store-only view has no fixed program %q", progressProgramName)
	}
}

// ReadProgress is a store transaction like Read: inside a composing transaction it joins the caller's, inside a plain one it is refused; a caller already inside a transaction calls Progress with
// its own querier.
func TestProgressInsideCallersTransaction(t *testing.T) {
	f := newFixture(t)
	forkJoinPlan(f, "p1")
	f.projectParent()
	ctx := context.Background()
	want := f.progress("p1").printed()
	err := f.s.Compose(ctx, func(txCtx context.Context, _ *sql.Conn) error {
		p, err := f.sched.Progress(txCtx, f.s.Q(txCtx), "p1")
		if err != nil {
			return err
		}
		if p.printed() != want {
			t.Error("Progress inside a caller's transaction prints another document")
		}
		joined, err := f.sched.ReadProgress(txCtx, "p1")
		if err != nil {
			return err
		}
		if joined.printed() != want {
			t.Error("ReadProgress inside Compose prints another document")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	err = f.s.Transaction(ctx, func(txCtx context.Context, _ *sql.Conn) error {
		_, err := f.sched.ReadProgress(txCtx, "p1")
		return err
	})
	if !errors.Is(err, store.ErrNestedTransaction) {
		t.Errorf("ReadProgress inside a plain transaction = %v, want store.ErrNestedTransaction", err)
	}
	if _, err := f.sched.ReadProgress(ctx, "nope"); refusalReason(err) != "unregistered_scope" {
		t.Errorf("an unknown plan = %v, want unregistered_scope", err)
	}
}

// A guard, not the proof of criterion c2 (that is the behaviour above and the command test): the library file imports nothing that reaches outside the process or the command layer.
func TestProgressSourceImportsGuard(t *testing.T) {
	file, err := parser.ParseFile(token.NewFileSet(), "progress.go", nil, parser.ImportsOnly)
	if err != nil {
		t.Fatal(err)
	}
	banned := map[string]bool{"os": true, "os/exec": true, "net": true, "net/http": true, "io/ioutil": true,
		modulePrefix + "internal/relay/dispatch": true, modulePrefix + "internal/relay/daemon": true, modulePrefix + "internal/relay/service": true}
	for _, imp := range file.Imports {
		path, _ := strconv.Unquote(imp.Path.Value)
		if banned[path] {
			t.Errorf("progress.go imports %s", path)
		}
	}
}

// The page names the command, the schema, every stage and every key of the document, so it cannot drift from what the code prints.
func TestProgressPageNamesEveryStageAndKey(t *testing.T) {
	raw, err := os.ReadFile("../../../docs/relay/dag-progress.md")
	if err != nil {
		t.Fatal(err)
	}
	page := string(raw)
	for _, want := range []string{"dag-progress", SchemaProgress} {
		if !strings.Contains(page, "`"+want) {
			t.Errorf("docs/relay/dag-progress.md does not name %s", want)
		}
	}
	for _, stage := range ProgressStages {
		if !strings.Contains(page, "`"+stage+"`") {
			t.Errorf("docs/relay/dag-progress.md does not name the stage %s", stage)
		}
	}
	f := newFixture(t)
	forkJoinPlan(f, "p1")
	f.projectParent()
	f.acceptNode("p1", "research", pinnedAcceptance(head1, 7))
	doc := f.progress("p1").decoded(t)
	keys := map[string]bool{}
	var walk func(v any)
	walk = func(v any) {
		switch x := v.(type) {
		case map[string]any:
			for key, value := range x {
				keys[key] = true
				walk(value)
			}
		case []any:
			for _, e := range x {
				walk(e)
			}
		}
	}
	walk(doc)
	var missing []string
	for key := range keys {
		if !strings.Contains(page, "`"+key+"`") {
			missing = append(missing, key)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("docs/relay/dag-progress.md does not name the keys %v", missing)
	}
	scheduler, err := os.ReadFile("../../../docs/relay/dag-scheduler.md")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(scheduler), "`dag-progress") {
		t.Error("docs/relay/dag-scheduler.md does not list dag-progress among its commands")
	}
}
