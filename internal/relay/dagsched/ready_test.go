package dagsched

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dag"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

func emitted(t *testing.T, r Reading) string {
	t.Helper()
	var b strings.Builder
	if err := contract.Emit(&b, r.Object()); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

// The store has one connection: a reading that opened a second to read a relationship's state would wait for itself. Read and Ready inside a Compose are
// the two ways a caller reaches the reader, and both must return on a read-only handle (the handle dag-ready opens) and on a writable one.
func TestReadReturnsOnTheReadOnlyHandle(t *testing.T) {
	f := newFixture(t)
	forkJoinPlan(f, "p1")
	f.projectParent()
	f.startNode("p1", "research")
	f.acceptNode("p1", "design", acceptOpts{})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	readOnly, err := store.Open(store.WithReadOnlyCommand(ctx), f.path, "")
	if err != nil {
		t.Fatal(err)
	}
	defer readOnly.Close()
	reading, err := (&Scheduler{Store: readOnly}).Read(ctx, "p1", ReadyOptions{})
	if err != nil {
		t.Fatalf("a reading on the read-only handle: %v", err)
	}
	if len(reading.Nodes) != 7 {
		t.Fatalf("the reading holds %d nodes, want the plan's 7", len(reading.Nodes))
	}
	err = f.s.Compose(ctx, func(txCtx context.Context, _ *sql.Conn) error {
		_, err := f.sched.Ready(txCtx, f.s.Q(txCtx), "p1", ReadyOptions{})
		return err
	})
	if err != nil {
		t.Fatalf("Ready inside a Compose: %v", err)
	}
}

// Two readings of one store state are byte-identical, and a write that changes what the reading depends on changes them (criterion c1).
func TestReadyDeterministic(t *testing.T) {
	f := newFixture(t)
	forkJoinPlan(f, "p1")
	f.projectParent()
	f.acceptNode("p1", "research", acceptOpts{})
	f.acceptNode("p1", "design", acceptOpts{})
	f.declare("p1", "impl-a", "a.go")
	f.declare("p1", "impl-b", "b.go")
	f.startNode("p1", "impl-a")
	first, second := emitted(t, f.read("p1")), emitted(t, f.read("p1"))
	if first != second {
		t.Fatalf("two readings of one store state differ:\n%s\n%s", first, second)
	}
	if len(f.read("p1").Ready) == 0 {
		t.Fatalf("the fixture has no ready node, so the comparison proves little: %s", f.read("p1").brief())
	}
	f.startNode("p1", "impl-b")
	if third := emitted(t, f.read("p1")); third == first {
		t.Fatal("a reading did not change after a node started")
	}
	reading := f.read("p1")
	again, err := f.sched.Read(context.Background(), "p1", ReadyOptions{SkipArtifactBytes: true})
	if err != nil {
		t.Fatal(err)
	}
	if again.InputDigest != reading.InputDigest {
		t.Fatal("the digest of the store half differs from the full reading's although no file changed")
	}
}

// Criterion c2: a long unrelated sibling never holds back a short dependent chain. Y waits for X's integration and for nothing else; L, a node
// that is running the whole time, is never in Y's reason.
func TestFrontierNotBarrier(t *testing.T) {
	f := newFixture(t)
	f.projectParent()
	f.putPlan("fb", 0, "fb-r1", addNode("L", dag.NodeImplementation), addNode("X", dag.NodeImplementation), addNode("Y", dag.NodeImplementation),
		addEdge("xy", "X", "Y", dag.EdgeIntegrated, nil))
	f.declare("fb", "L", "l.go")
	f.declare("fb", "X", "x.go")
	f.declare("fb", "Y", "y.go")

	reading := f.read("fb")
	if got := reading.readyIDs(); len(got) != 2 || got[0] != "X" || got[1] != "L" {
		t.Fatalf("ready = %v, want the chain head X before the independent L", got)
	}
	if y := reading.node("Y"); y.Reason != WaitEdge("xy") {
		t.Fatalf("Y = %+v, want it waiting for the edge from X", y)
	}

	f.startNode("fb", "L") // L runs for the rest of the test
	x := f.acceptNode("fb", "X", pinnedOpts)
	if y := f.read("fb").node("Y"); y.Reason != WaitEdge("xy") {
		t.Fatalf("Y after X was accepted but not landed = %+v, want it still waiting for X's integration", y)
	}
	f.integrate(x, "owner/repo", "dev", false, false)
	if y := f.read("fb").node("Y"); y.Reason != WaitEdge("xy") {
		t.Fatalf("Y with a negative observation = %+v, want it still waiting", y)
	}
	f.integrate(x, "owner/repo", "dev", true, true)
	reading = f.read("fb")
	y, l := reading.node("Y"), reading.node("L")
	if y.Disposition != DispReady {
		t.Fatalf("Y after X landed = %+v, want it ready while the long node still runs", y)
	}
	if l.State != StateRunning || l.Disposition != DispSkip {
		t.Fatalf("L = %+v, want it running and untouched", l)
	}
	if got := reading.node("X").State; got != StateIntegrated {
		t.Fatalf("X is %q, want integrated", got)
	}
}

// Criterion c2 (ordering): critical path first, then descendants, then age, then node id; and when the candidates outnumber the free slots the pass says so.
func TestOrderCriticalPathDescendantsAge(t *testing.T) {
	setup := func(t *testing.T) *fixture {
		f := newFixture(t)
		f.projectParent()
		f.putPlan("o", 0, "o-r1",
			addNode("a", dag.NodeNonPR), addNode("a2", dag.NodeNonPR), addNode("a3", dag.NodeNonPR),
			addNode("b", dag.NodeNonPR), addNode("b2", dag.NodeNonPR), addNode("b3", dag.NodeNonPR),
			addNode("c", dag.NodeNonPR), addNode("c2", dag.NodeNonPR), addNode("e", dag.NodeNonPR),
			addEdge("a-a2", "a", "a2", dag.EdgeArtifactVerified, nil), addEdge("a2-a3", "a2", "a3", dag.EdgeArtifactVerified, nil),
			addEdge("b-b2", "b", "b2", dag.EdgeArtifactVerified, nil), addEdge("b-b3", "b", "b3", dag.EdgeArtifactVerified, nil),
			addEdge("c-c2", "c", "c2", dag.EdgeArtifactVerified, nil))
		// d, f and g arrive one revision after e, so they are younger than e; between themselves only the id decides.
		f.putPlan("o", 1, "o-r2", addNode("g", dag.NodeNonPR), addNode("d", dag.NodeNonPR), addNode("f", dag.NodeNonPR))
		return f
	}
	t.Run("every key decides one pair", func(t *testing.T) {
		f := setup(t)
		reading := f.read("o")
		want := []string{"a", "b", "c", "e", "d", "f"}
		if got := reading.readyIDs(); strings.Join(got, ",") != strings.Join(want, ",") {
			t.Fatalf("ready = %v, want %v (the seventh candidate, g, is cut by the standing cap of 6)", got, want)
		}
		if g := reading.node("g"); g.Reason != DeferNoCapacity || g.Rank == nil {
			t.Fatalf("g = %+v, want defer:no_capacity with its rank", g)
		}
		a, b, c := reading.node("a").Rank, reading.node("b").Rank, reading.node("c").Rank
		if a.CriticalPath != 3 || a.Descendants != 2 || b.CriticalPath != 2 || b.Descendants != 2 || c.CriticalPath != 2 || c.Descendants != 1 {
			t.Fatalf("ranks a=%+v b=%+v c=%+v", a, b, c)
		}
		if reading.Pass.DecidingLimit != LimitNoCapacity || reading.Pass.ReadyCount != 6 || reading.Pass.FreeSlots != 6 {
			t.Fatalf("pass = %+v", reading.Pass)
		}
	})
	t.Run("two free slots keep the two highest and cut the rest in order", func(t *testing.T) {
		f := setup(t)
		f.declareLimit("project", "P-TEST", "runs", 2)
		reading := f.read("o")
		if got := reading.readyIDs(); strings.Join(got, ",") != "a,b" {
			t.Fatalf("ready = %v, want a,b", got)
		}
		for _, id := range []string{"c", "e", "d", "f", "g"} {
			if n := reading.node(id); n.Reason != DeferNoCapacity {
				t.Fatalf("%s = %+v, want defer:no_capacity", id, n)
			}
		}
		if reading.Pass.DecidingLimit != LimitNoCapacity || reading.Pass.Ceiling != 2 {
			t.Fatalf("pass = %+v", reading.Pass)
		}
	})
}
