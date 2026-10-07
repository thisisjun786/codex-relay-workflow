package manage

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/acceptance"
)

// The readings this issue fixes, each with the red test the issue body names. The fixture is
// branchFixture (capacity_branches_test.go): a plan is described as revisions and written through the
// DAG repository, so the branch reading's own canon (dag.SnapshotAt) accepts it, and the store's
// marks are written directly.

// capacityReview779Seam installs the reading's test seam for one test. It runs inside the reading,
// right after the plan has been read, and writes the revisions the test described but did not write,
// so the plan changes while the reading is in flight.
func capacityReview779Seam(t *testing.T, f *branchFixture) {
	t.Helper()
	previous := branchReadSeam
	branchReadSeam = func() { f.writePlan() }
	t.Cleanup(func() { branchReadSeam = previous })
}

// capacityReview779Plan is a four-node plan in two bundles: A and B joined by an edge, C and D
// joined by an edge, every node declaring a place of its own. Neither bundle is the whole plan, so
// both are candidates while nothing else changes.
func capacityReview779Plan(f *branchFixture) *branchFixture {
	f.node("A", "CRW-1").node("B", "CRW-2").node("C", "CRW-3").node("D", "CRW-4")
	f.edge("e1", "A", "B").edge("e2", "C", "D")
	for _, node := range []string{"A", "B", "C", "D"} {
		f.region(node, "pkg/"+node+".go", "file", "", "edit", false)
	}
	return f
}

// capacityReview779Chain is a plan whose subject node C joins D, which joins E. Judging C landed
// takes it out of the graph and leaves D and E as a bundle of their own, so the judgement is
// visible in the candidates: a landed C reads [A+B, D+E], and a C that did not land reads [A+B]
// alone, because the relay ran C (its execution row is what the integration judgement walks) and a
// bundle that holds a released node cannot be taken out.
func capacityReview779Chain(f *branchFixture, extraTargets ...string) *branchFixture {
	f.node("A", "CRW-1").node("B", "CRW-2").node("C", "CRW-3").node("D", "CRW-4").node("E", "CRW-5")
	f.edge("e1", "A", "B").edge("e2", "C", "D").edge("e3", "D", "E")
	// A second target C's head also has to land in: its base ref is one the plan names, and no
	// observation of it is recorded, so the landing is incomplete.
	for i, baseRef := range extraTargets {
		f.integratedEdge(fmt.Sprintf("x%d", i), "C", "D", baseRef)
	}
	for _, node := range []string{"A", "B", "C", "D", "E"} {
		f.region(node, "pkg/"+node+".go", "file", "", "edit", false)
	}
	return f
}

// capacityReview779ChainIntegrated is the chain's candidates when its subject node landed.
func capacityReview779ChainIntegrated() []string {
	return []string{"A+B pkg/A.go,pkg/B.go ready=2 edges=1", "D+E pkg/D.go,pkg/E.go ready=0 edges=1"}
}

// capacityReview779ChainLive is the chain's candidates when its subject node did not land.
func capacityReview779ChainLive() []string {
	return []string{"A+B pkg/A.go,pkg/B.go ready=2 edges=1"}
}

// capacityReview779Relationship is the relationship the fixture binds one node to.
func capacityReview779Relationship(node string) string { return "rel-" + node }

// capacityReview779MergedMark records the parent's merged mark on one generation of a node's event,
// which is what the relay's integration judgement asks for beside a contained observation.
func capacityReview779MergedMark(f *branchFixture, node string, generation int) {
	f.exec("INSERT OR IGNORE INTO assignment_marks (relationship_id, mark, event_id, execution_generation, revision_hash, evidence, actor, marked_at) VALUES (?,'merged','event',?,'revision','merged','parent',?)",
		capacityReview779Relationship(node), generation, branchTestStamp(0))
}

// capacityReview779Observe records one observation of a named head for one node's acceptance, so a
// test can pin that an observation of another head does not resolve the node.
func capacityReview779Observe(f *branchFixture, node, head string, seq int, ancestor bool) {
	capacityReview779ObserveIn(f, node, branchTestRepo, "dev", head, seq, ancestor)
}

// capacityReview779ObserveIn records one observation of a named head in a named target.
func capacityReview779ObserveIn(f *branchFixture, node, repository, baseRef, head string, seq int, ancestor bool) {
	f.exec("INSERT INTO dag_integration_observations (observation_id, acceptance_id, repository, base_ref, subject_sha, tip_sha, is_ancestor, method, observed_seq, reverted_by, observed_at) VALUES (?,?,?,?,?,?,?,?,?,NULL,?)",
		"observation-"+node+"-"+repository+"-"+baseRef+"-"+branchItoa(seq), "acceptance-"+node, repository, baseRef, head, "tip",
		dagReviewFlag(ancestor), "ancestry", seq, branchTestStamp(0))
}

// capacityReview779Refresh records a valid base refresh for one node's acceptance: the acceptance was
// accepted in generation 1, and the refresh moves it to the head of generation 2.
func capacityReview779Refresh(t *testing.T, f *branchFixture, node, head string) {
	t.Helper()
	proof, resolved := "{}", "[]"
	refreshID := acceptance.RefreshDigest("acceptance-"+node, capacityReview779Relationship(node), 2, "event", "revision", head, branchTestRepo, "dev", "tip", proof, resolved)
	f.exec("INSERT INTO dag_base_refreshes (refresh_id, acceptance_id, refresh_seq, relationship_id, execution_generation, event_id, revision_hash, head_sha, base_repository, base_ref, base_tip_sha, proof_json, resolved_paths_json, recorded_by_task_id, coordinator_epoch, recorded_at) VALUES (?,?,1,?,2,'event','revision',?,?,?,?,?,?,'parent',0,?)",
		refreshID, "acceptance-"+node, capacityReview779Relationship(node), head, branchTestRepo, "dev", "tip", proof, resolved, branchTestStamp(0))
}

// capacityReview779BareStore writes a store that holds only the relay's delivery tables: it predates
// the DAG zone, so nothing about a plan's branches can be measured from it.
func capacityReview779BareStore(t *testing.T, path string) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, statement := range []string{
		"CREATE TABLE deliveries (event_id TEXT PRIMARY KEY, recipient_task_id TEXT NOT NULL, created_at TEXT NOT NULL)",
		"CREATE TABLE acks (event_id TEXT PRIMARY KEY, ack_at TEXT NOT NULL)",
		"CREATE TABLE events (event_id TEXT PRIMARY KEY, outcome TEXT NOT NULL)",
	} {
		if _, err := db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
}

// capacityReview779BareFixture is a branch fixture whose store carries no DAG zone: the capacity
// reading still answers, and the plan reports its branches as unmeasured.
func capacityReview779BareFixture(t *testing.T, ready ...string) *branchFixture {
	t.Helper()
	coreTempHome(t)
	dir := t.TempDir()
	f := &branchFixture{t: t, dir: dir, stateDir: filepath.Join(dir, "state"), relayDir: filepath.Join(dir, "relay"),
		ready: ready, nodes: map[string]string{}}
	for _, path := range []string{f.stateDir, f.relayDir} {
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	capacityReview779BareStore(t, filepath.Join(f.relayDir, "relay.sqlite3"))
	f.env = &Env{Stdin: strings.NewReader(""), Stdout: io.Discard, Stderr: io.Discard, Getenv: os.Getenv,
		Now: func() time.Time { return branchTestNow }, Executable: filepath.Join(dir, "crw")}
	f.section = map[string]any{"plans": []map[string]any{{"plan": branchTestPlan, "project": branchTestProject, "parent": branchTestParent}}}
	f.load()
	return f
}

// capacityReview779Text runs the command's text form and returns what it printed.
func capacityReview779Text(t *testing.T, f *branchFixture) string {
	t.Helper()
	config := capacityConfig
	t.Cleanup(func() { capacityConfig = config })
	capacityConfig = func(*Env) *Config { return f.cfg }
	var out strings.Builder
	f.env.Stdout, f.env.Stderr = &out, &out
	if code := capacityCommand.Run(context.Background(), f.env, []string{"--text"}); code != 0 {
		t.Fatalf("--text: exit %d: %s", code, out.String())
	}
	return out.String()
}

// C1: the whole reading is one snapshot. The plan's next revision retires the edge between C and D
// while the reading is in flight, and the answer still comes from the revision the reading began on,
// so both bundles survive. A reading that mixed two revisions would lose the second one.
func TestCapacityReview779OneSnapshot(t *testing.T) {
	f := branchNewFixture(t, "CRW-1", "CRW-2")
	capacityReview779Plan(f)
	// The revision that retires the edge is described here and written by the seam, so it lands
	// after the reading has taken its snapshot and before the readings beside the plan.
	f.revision(2)
	f.retireEdge("e2")
	f.publishOpen()
	capacityReview779Seam(t, f)
	plan := f.run()
	if plan.Verdict != capacityExpand {
		t.Fatalf("the plan is %s, want %s", plan.Verdict, capacityExpand)
	}
	branchWant(t, branchSummaries(branchList(t, plan)),
		"A+B pkg/A.go,pkg/B.go ready=2 edges=1", "C+D pkg/C.go,pkg/D.go ready=0 edges=1")
}

// C3: readiness is judged by node id. Two nodes implement the same issue, and the pass lists only
// one of them as ready; a reading that judged readiness by issue key would count both.
func TestCapacityReview779ReadyByNodeID(t *testing.T) {
	f := branchNewFixture(t, "CRW-808")
	f.node("A", "CRW-808").node("B", "CRW-808")
	f.node("C", "CRW-3").node("D", "CRW-4")
	f.edge("e1", "A", "B").edge("e2", "C", "D")
	for _, node := range []string{"A", "B", "C", "D"} {
		f.region(node, "pkg/"+node+".go", "file", "", "edit", false)
	}
	// One waiting node keeps the judgement persistent at a floor of one, so the plan is an expansion
	// candidate and its branches are read.
	f.section["min_waiting"] = 1
	f.load()
	f.publish()
	candidates := branchList(t, f.run())
	if len(candidates) != 2 {
		t.Fatalf("branches = %v, want two", branchSummaries(candidates))
	}
	var bundle *BranchCandidate
	for i, candidate := range candidates {
		if candidate.Nodes[0].NodeID == "A" {
			bundle = &candidates[i]
		}
	}
	if bundle == nil {
		t.Fatalf("the plan carries no bundle of A: %v", branchSummaries(candidates))
	}
	if bundle.ReadyCount != 1 {
		t.Fatalf("ready_count = %d, want 1: only the node the pass listed as ready", bundle.ReadyCount)
	}
	for _, node := range bundle.Nodes {
		if want := node.NodeID == "A"; node.Ready != want {
			t.Errorf("node %s ready = %t, want %t", node.NodeID, node.Ready, want)
		}
	}
}

// C4: an acceptance a recorded base refresh moved to a later generation is judged at the stand it
// holds now, so the refreshed head is the head the integration judgement asks for.
func TestCapacityReview779BaseRefreshedAcceptanceIsIntegrated(t *testing.T) {
	t.Run("the refreshed head is the head the judgement asks for", func(t *testing.T) {
		f := branchNewFixture(t, "CRW-1", "CRW-2")
		capacityReview779Chain(f)
		// C stands on generation 2's head through the refresh, and that head landed everywhere it
		// had to; nothing observed the head it was accepted on.
		f.accepted("C")
		f.executed("C")
		capacityReview779Refresh(t, f, "C", "head-C2")
		capacityReview779MergedMark(f, "C", 2)
		capacityReview779Observe(f, "C", "head-C2", 1, true)
		f.publish()
		branchWant(t, branchSummaries(branchList(t, f.run())), capacityReview779ChainIntegrated()...)
	})

	t.Run("an observation of the head it was accepted on does not integrate it", func(t *testing.T) {
		f := branchNewFixture(t, "CRW-1", "CRW-2")
		capacityReview779Chain(f)
		// The refresh moved C to generation 2, and the only observation is of generation 1's head:
		// the stand's own head was never observed, so C stays live.
		f.accepted("C")
		f.executed("C")
		f.mergedMark(capacityReview779Relationship("C"))
		f.observation("C", 1, true)
		capacityReview779Refresh(t, f, "C", "head-C2")
		f.publish()
		branchWant(t, branchSummaries(branchList(t, f.run())), capacityReview779ChainLive()...)
	})
}

// C4: a node that has to land in two targets is integrated only when both contain it.
func TestCapacityReview779IntegrationNeedsEveryTarget(t *testing.T) {
	t.Run("only the first target observed", func(t *testing.T) {
		f := branchNewFixture(t, "CRW-1", "CRW-2")
		// C hands its result to a second branch as well, and only the first one observed the landing:
		// the canon asks for every target, so C is not landed and stays live.
		capacityReview779Chain(f, "release")
		f.accepted("C")
		f.executed("C")
		f.mergedMark(capacityReview779Relationship("C"))
		f.observation("C", 1, true)
		f.publish()
		branchWant(t, branchSummaries(branchList(t, f.run())), capacityReview779ChainLive()...)
	})

	t.Run("both targets observed", func(t *testing.T) {
		f := branchNewFixture(t, "CRW-1", "CRW-2")
		capacityReview779Chain(f, "release")
		f.accepted("C")
		f.executed("C")
		f.mergedMark(capacityReview779Relationship("C"))
		capacityReview779ObserveIn(f, "C", branchTestRepo, "dev", "head-C", 1, true)
		capacityReview779ObserveIn(f, "C", branchTestRepo, "release", "head-C", 1, true)
		f.publish()
		branchWant(t, branchSummaries(branchList(t, f.run())), capacityReview779ChainIntegrated()...)
	})
}

// C2: a cancelled node is not a live node, so the edge it carried joins nothing and the node it
// joined stands alone, below the floor. A paused node is still live.
func TestCapacityReview779CancelledNodeIsNotLive(t *testing.T) {
	t.Run("a cancelled node leaves the plan", func(t *testing.T) {
		f := branchNewFixture(t, "CRW-1", "CRW-2")
		capacityReview779Plan(f)
		f.revision(2)
		f.cancelNode("C")
		f.publish()
		branchWant(t, branchSummaries(branchList(t, f.run())), "A+B pkg/A.go,pkg/B.go ready=2 edges=1")
	})

	t.Run("a paused node is still live", func(t *testing.T) {
		f := branchNewFixture(t, "CRW-1", "CRW-2")
		capacityReview779Plan(f)
		f.revision(2)
		f.pauseNode("C")
		f.publish()
		branchWant(t, branchSummaries(branchList(t, f.run())),
			"A+B pkg/A.go,pkg/B.go ready=2 edges=1", "C+D pkg/C.go,pkg/D.go ready=0 edges=1")
	})
}

// C5: the text form carries one line per candidate, in the document's order.
func TestCapacityReview779TextLinesCarryCandidates(t *testing.T) {
	f := branchNewFixture(t, "CRW-1", "CRW-2")
	f.node("A", "CRW-1").node("B", "CRW-2").node("C", "CRW-3")
	f.edge("e1", "A", "B")
	for _, node := range []string{"A", "B", "C"} {
		f.region(node, "pkg/"+node+".go", "file", "", "edit", false)
	}
	f.publish()
	text := capacityReview779Text(t, f)
	want := "capacity: " + branchTestPlan + " branch 1: nodes A,B ready 2/2 regions pkg/A.go,pkg/B.go"
	if !strings.Contains(text, want) {
		t.Fatalf("--text carries no candidate line %q:\n%s", want, text)
	}
	if got := branchSummaries(branchList(t, f.run())); len(got) != 1 {
		t.Fatalf("branches = %v, want the one bundle", got)
	}
}

// C5: a store without the DAG zone reports its branches as unmeasured, which is a different answer
// from the empty list of a plan nothing can be detached from.
func TestCapacityReview779StoreWithoutDagIsUnmeasured(t *testing.T) {
	f := capacityReview779BareFixture(t, "CRW-1", "CRW-2")
	f.publish()
	plan := f.run()
	if plan.Verdict != capacityExpand {
		t.Fatalf("the plan is %s, want %s", plan.Verdict, capacityExpand)
	}
	if plan.BranchesUnmeasured == "" {
		t.Fatal("a store without the DAG zone measured its branches")
	}
	if plan.Branches == nil {
		t.Fatal("the plan carries no branches key at all, want a null list beside the reason")
	}
	if *plan.Branches != nil {
		t.Fatalf("branches = %v, want a null list", *plan.Branches)
	}
	data, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	if doc["branches"] != nil {
		t.Fatalf("branches = %v, want null", doc["branches"])
	}
	if doc["branches_unmeasured"] != plan.BranchesUnmeasured {
		t.Fatalf("branches_unmeasured = %v, want %q", doc["branches_unmeasured"], plan.BranchesUnmeasured)
	}
	if text := capacityReview779Text(t, f); !strings.Contains(text, "branches unmeasured: ") {
		t.Fatalf("--text does not name the unmeasured reading:\n%s", text)
	}
}

// C5: a plan nothing can be detached from still carries the empty list, which is not the same answer
// as an unmeasured reading.
func TestCapacityReview779NoCandidatesIsAnEmptyList(t *testing.T) {
	f := branchNewFixture(t, "CRW-1", "CRW-2")
	f.node("A", "CRW-1").node("B", "CRW-2")
	f.edge("e1", "A", "B")
	for _, node := range []string{"A", "B"} {
		f.region(node, "pkg/"+node+".go", "file", "", "edit", false)
	}
	f.publish()
	plan := f.run()
	if plan.BranchesUnmeasured != "" {
		t.Fatalf("branches_unmeasured = %q, want none", plan.BranchesUnmeasured)
	}
	if plan.Branches == nil || len(*plan.Branches) != 0 {
		t.Fatalf("branches = %v, want an empty list", plan.Branches)
	}
	data, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	empty, ok := doc["branches"].([]any)
	if !ok || len(empty) != 0 {
		t.Fatalf("branches = %v, want an empty list", doc["branches"])
	}
}
