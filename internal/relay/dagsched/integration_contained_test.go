package dagsched

import (
	"context"
	"errors"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dag"
)

// addChildNode adds an implementation node whose commit is a child of parent, reports it and records its verified head,
// as newBatchKit does for its own nodes.
func (k *batchKit) addChildNode(name, parent, file string) {
	k.t.Helper()
	k.repo.git("checkout", "-q", "-B", "feature-"+name, parent)
	k.repo.write(file, name+"\n")
	k.repo.git("add", file)
	k.repo.git("commit", "-q", "-m", "node "+name)
	k.heads[name] = k.repo.git("rev-parse", "HEAD")
	k.repo.git("checkout", "-q", "dev")
	snap := k.snapshot("g")
	k.putPlan("g", int(snap.Revision), "g-"+name, addRelNode(name, dag.NodeImplementation))
	r := k.reportNode("g", name, acceptOpts{})
	k.exec("INSERT INTO dag_verified_heads (event_id, relationship_id, execution_generation, verdict_turn_id, head_sha, recorded_by_task_id, recorded_at) VALUES (?,?,?,?,?,?,?)",
		r.Event, r.Acceptance.RelationshipID, 1, "verdict-turn", k.heads[name], "parent", k.clock())
}

// CRW-965 (parent decision, case 1): a batch that moved the branch and died before it recorded the move leaves the
// candidate contained in a head the relay verified. The next batch marks it from its frozen row, with no merge and no
// verifier call.
func TestContainedCandidateIsMarkedAfterABatchDiedAfterMovingTheBranch(t *testing.T) {
	k := newBatchKit(t, batchNode{name: "a", files: map[string]string{"a.txt": "a\n"}})
	k.acceptByCommit("a")
	ctx := context.Background()
	dying := IntegrationBatchDeps{Verify: stubVerifier(writeStubVerifier(t)), Update: func(ctx context.Context, checkout, ref, newCommit, oldCommit string) error {
		if err := updateIntegrationRef(ctx, checkout, ref, newCommit, oldCommit); err != nil {
			return err
		}
		return errors.New("simulated death after the branch moved")
	}}
	if _, err := k.sched.IntegrateBatch(ctx, k.batchIn(), dying); err == nil {
		t.Fatal("the dying batch should fail after it moved the branch")
	}
	moved, found := k.branchTip("dev-int")
	if !found {
		t.Fatal("the dying batch should have moved the branch")
	}
	runs := 0
	base := stubVerifier(writeStubVerifier(t))
	counting := IntegrationBatchDeps{Verify: func(ctx context.Context, dir string, env []string) error {
		runs++
		return base(ctx, dir, env)
	}, Update: updateIntegrationRef}
	res, err := k.sched.IntegrateBatch(ctx, k.batchIn(), counting)
	if err != nil {
		t.Fatalf("next batch: %v", err)
	}
	if runs != 0 || len(res.Merged) != 0 {
		t.Fatalf("verifier ran %d times and merged %d; a contained candidate needs neither", runs, len(res.Merged))
	}
	if tip, _ := k.branchTip("dev-int"); tip != moved {
		t.Fatalf("the branch moved from %s to %s; no new merge commit is expected", moved, tip)
	}
	if len(res.AlreadyContained) != 1 || res.AlreadyContained[0].NodeID != "a" || res.AlreadyContained[0].MarkedEvent == "" {
		t.Fatalf("want a marked already_contained candidate a: %+v", res.AlreadyContained)
	}
}

// CRW-965 (parent decision, cases 2 and 4): a named batch merges X, whose history contains Y's head. The next run marks Y
// as already contained, and the stage rows agree with the events the run reports.
func TestContainedCandidateOfANamedBatchIsMarkedOnTheNextRun(t *testing.T) {
	k := newBatchKit(t, batchNode{name: "y", files: map[string]string{"y.txt": "y\n"}})
	k.addChildNode("x", k.heads["y"], "x.txt")
	k.acceptByCommit("y")
	k.acceptByCommit("x")
	ctx := context.Background()
	runs := 0
	base := stubVerifier(writeStubVerifier(t))
	counting := IntegrationBatchDeps{Verify: func(ctx context.Context, dir string, env []string) error {
		runs++
		return base(ctx, dir, env)
	}, Update: updateIntegrationRef}
	first := k.batchIn()
	first.Nodes = []string{"x"}
	if _, err := k.sched.IntegrateBatch(ctx, first, counting); err != nil {
		t.Fatalf("named batch: %v", err)
	}
	res, err := k.sched.IntegrateBatch(ctx, k.batchIn(), counting)
	if err != nil {
		t.Fatalf("next batch: %v", err)
	}
	if runs != 1 {
		t.Fatalf("verifier ran %d times; only the named batch may verify", runs)
	}
	var y *IntegrationBatchContained
	for i := range res.AlreadyContained {
		if res.AlreadyContained[i].NodeID == "y" {
			y = &res.AlreadyContained[i]
		}
	}
	if y == nil || y.MarkedEvent == "" {
		t.Fatalf("Y should be marked as already contained: %+v", res.AlreadyContained)
	}
	marked := 0
	for _, r := range k.stageRows() {
		if r.BatchID == res.BatchID && r.Stage == "marked" {
			marked++
		}
	}
	reported := 0
	for _, c := range res.AlreadyContained {
		if c.MarkedEvent != "" {
			reported++
		}
	}
	if marked != reported {
		t.Fatalf("%d marked stage rows for the batch, %d marks reported", marked, reported)
	}
}

// CRW-965 (parent decision, case 3): a branch head no recorded batch verified does not launder a candidate. Y is reported
// as contained_unverified, is not marked, and stays a ready candidate.
func TestContainedCandidateWaitsWhenTheBranchHeadWasNeverVerified(t *testing.T) {
	k := newBatchKit(t, batchNode{name: "y", files: map[string]string{"y.txt": "y\n"}})
	k.addChildNode("x", k.heads["y"], "x.txt")
	k.acceptByCommit("y")
	k.acceptByCommit("x")
	ctx := context.Background()
	k.repo.git("update-ref", "refs/heads/dev-int", k.heads["x"])
	runs := 0
	counting := IntegrationBatchDeps{Verify: func(ctx context.Context, dir string, env []string) error {
		runs++
		return stubVerifier(writeStubVerifier(t))(ctx, dir, env)
	}, Update: updateIntegrationRef}
	res, err := k.sched.IntegrateBatch(ctx, k.batchIn(), counting)
	if err != nil {
		t.Fatalf("batch: %v", err)
	}
	if runs != 0 || len(res.AlreadyContained) != 0 {
		t.Fatalf("an unverified branch head must not be marked or verified: runs %d, already contained %+v", runs, res.AlreadyContained)
	}
	found := false
	for _, c := range res.ContainedUnverified {
		if c.NodeID == "y" {
			found = true
		}
	}
	if !found {
		t.Fatalf("Y should be reported contained_unverified: %+v", res.ContainedUnverified)
	}
	cands, err := k.sched.AcceptedCandidates(ctx, "g")
	if err != nil {
		t.Fatal(err)
	}
	ready := false
	for _, c := range cands {
		if c.NodeID == "y" {
			ready = true
		}
	}
	if !ready {
		t.Fatal("Y should stay a ready candidate")
	}
}
