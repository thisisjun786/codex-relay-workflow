package dagsched

import (
	"context"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dag"
)

// singleConnectionPlan is the plan these tests share: implementation node A, which carries the correction generations, and
// independent non_pr node B, whose release must not depend on what A's rows say.
func singleConnectionPlan(f *fixture, plan string) {
	f.t.Helper()
	f.putPlan(plan, 0, plan+"-r1", addNode("A", dag.NodeImplementation), addNode("B", dag.NodeNonPR))
}

// assertSingleConnectionRead reads the plan through the store's own pool with a short deadline and fails when the reading
// does not answer, or does not answer the independent node ready.
func assertSingleConnectionRead(t *testing.T, f *fixture, plan, wantReady string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	start := time.Now()
	reading, err := f.sched.Ready(ctx, f.s.DB, plan, ReadyOptions{})
	if err != nil {
		t.Fatalf("Ready on the single-connection store gave up after %s: %v", time.Since(start).Round(time.Millisecond), err)
	}
	ready := false
	for _, n := range reading.Ready {
		if n.NodeID == wantReady {
			ready = true
		}
	}
	if !ready {
		t.Fatalf("node %s is not ready after %s; the reading is %s", wantReady, time.Since(start).Round(time.Millisecond), reading.brief())
	}
}

// The operational store has ONE connection (store.Open sets SetMaxOpenConns(1)) and the release path reads Ready outside a
// transaction, through *sql.DB (release.go and ready.go). A reading that queries again on that same querier while its own
// rows are still open waits for the connection it is holding, until the context deadline. correctionRuns did exactly that:
// it kept the (generation, relationship) rows of its first query open while it called store.LiveGenerationBefore and the
// needs_changes ruling query. So a single node with an unaccepted correction generation stopped every release of its plan.
// These tests read Ready through *sql.DB with a short deadline: before the fix the deadline expires and no node is released,
// after it the independent node answers ready.
func TestReadyOnTheSingleConnectionStoreDoesNotStallOnACorrectionGeneration(t *testing.T) {
	t.Run("a correction generation read for its ruling", func(t *testing.T) {
		f := newFixture(t)
		f.projectParent()
		singleConnectionPlan(f, "sc")
		// generation 2 is the node's first correction, so the ruling that opened it sits on generation 1 and the lookup needs no
		// withdrawn-generation walk: the ruling query alone is issued while the correction rows are open
		f.exec("INSERT INTO dag_node_executions (plan_id, node_id, relationship_id, execution_generation, manifest_digest, kind) VALUES ('sc', 'A', 'rel-sc-A', 2, ?, 'correction')",
			dig("manifest sc A 2"))
		assertSingleConnectionRead(t, f, "sc", "B")
	})

	t.Run("a correction generation over a withdrawn one", func(t *testing.T) {
		f := newFixture(t)
		f.projectParent()
		singleConnectionPlan(f, "sc")
		// generation 2 was opened by hand and withdrawn before it was sent: generation 3 follows the nearest live generation below
		// it, so store.LiveGenerationBefore reads the withdrawal while the correction rows are open
		f.withdrawGeneration("rel-sc-A", "sc", "A", 2, 1)
		f.exec("INSERT INTO dag_node_executions (plan_id, node_id, relationship_id, execution_generation, manifest_digest, kind) VALUES ('sc', 'A', 'rel-sc-A', 3, ?, 'correction')",
			dig("manifest sc A 3"))
		assertSingleConnectionRead(t, f, "sc", "B")
	})

	t.Run("two relationships at one generation are read once, as before", func(t *testing.T) {
		f := newFixture(t)
		f.projectParent()
		singleConnectionPlan(f, "sc")
		// one node whose correction generation is carried by two relationships: the reading walks the rows in
		// (generation, relationship) order and keeps the FIRST relationship of each generation, the de-duplication the
		// collection step must preserve. Both rows are read before either lookup runs, so the answer is the same and the
		// reading still completes on the one connection.
		f.exec("INSERT INTO dag_node_executions (plan_id, node_id, relationship_id, execution_generation, manifest_digest, kind) VALUES ('sc', 'A', 'rel-sc-A', 2, ?, 'correction')",
			dig("manifest sc A 2"))
		f.exec("INSERT INTO dag_node_executions (plan_id, node_id, relationship_id, execution_generation, manifest_digest, kind) VALUES ('sc', 'A', 'rel-sc-A2', 2, ?, 'correction')",
			dig("manifest sc A2 2"))
		assertSingleConnectionRead(t, f, "sc", "B")
	})
}
