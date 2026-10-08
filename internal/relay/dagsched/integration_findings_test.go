package dagsched

import (
	"context"
	"errors"
	"testing"
)

// revalidateCriteria re-rules a node's acceptance under a new criteria set, the way the scheduler leaves a revalidation: the
// canonical criteria of its relationship and one revalidation row that names the new digest (CRW-965, parent decision d2).
func (k *batchKit) revalidateCriteria(t *testing.T, node, digest string) {
	t.Helper()
	ctx := context.Background()
	acc, found, err := loadActiveAcceptance(ctx, k.sched.Store.Q(ctx), "g", node)
	if err != nil || !found {
		t.Fatalf("the active acceptance of %s: %v, %v", node, found, err)
	}
	rel, found, err := currentRelationshipOf(ctx, k.sched.Store.Q(ctx), "g", node)
	if err != nil || !found {
		t.Fatalf("the relationship of %s: %v, %v", node, found, err)
	}
	var event string
	if err := k.s.DB.QueryRow("SELECT event_id FROM events WHERE relationship_id = ?", rel.ID).Scan(&event); err != nil {
		t.Fatal(err)
	}
	var seq int
	if err := k.s.DB.QueryRow("SELECT COUNT(*) FROM dag_acceptance_revalidations WHERE acceptance_id = ?", acc.AcceptanceID).Scan(&seq); err != nil {
		t.Fatal(err)
	}
	k.exec("UPDATE canonical_criteria SET set_digest = ? WHERE relationship_id = ?", digest, rel.ID)
	k.exec("INSERT INTO dag_acceptance_revalidations (revalidation_id, acceptance_id, criteria_set_digest, event_id, verdict_turn_id, reval_seq, revalidated_by, revalidated_at) VALUES (?, ?, ?, ?, 'vt', ?, 'parent', 't')",
		"rv-"+acc.AcceptanceID+"-"+digest, acc.AcceptanceID, digest, event, seq+1)
	premergeRevalidationRecord(t, k.s.DB, acc.AcceptanceID, "rv-"+acc.AcceptanceID+"-"+digest, digest)
}

// CRW-965 (criteria c2 as read by the parent): a contained candidate whose criteria set changed is verified again with no
// merge, and only an unchanged criteria set is covered by the stored verification.
func TestContainedCandidateIsVerifiedAgainWhenItsCriteriaChanged(t *testing.T) {
	k := newBatchKit(t, batchNode{name: "a", files: map[string]string{"a.txt": "a\n"}})
	k.acceptByCommit("a")
	ctx := context.Background()
	runs := 0
	base := stubVerifier(writeStubVerifier(t))
	deps := IntegrationBatchDeps{Verify: func(ctx context.Context, dir string, env []string) error {
		runs++
		return base(ctx, dir, env)
	}, Update: updateIntegrationRef}
	in := k.batchIn()
	in.Nodes = []string{"a"}
	if _, err := k.sched.IntegrateBatch(ctx, in, deps); err != nil {
		t.Fatalf("batch a: %v", err)
	}
	if runs != 1 {
		t.Fatalf("the first batch verifies once; it ran %d times", runs)
	}
	k.invRevise("g", "a", "g-r2", func(n doc) { n["criteria_set_digest"] = dig("changed a") })
	k.revalidateCriteria(t, "a", dig("changed a"))
	if _, err := k.sched.IntegrateBatch(ctx, in, deps); err != nil {
		t.Fatalf("batch a after its criteria change: %v", err)
	}
	if runs != 2 {
		t.Fatalf("a contained candidate whose criteria changed must be verified again: the verifier ran %d times", runs)
	}
	if _, err := k.sched.IntegrateBatch(ctx, in, deps); err != nil {
		t.Fatalf("batch a again: %v", err)
	}
	if runs != 2 {
		t.Fatalf("a contained candidate whose criteria set is verified needs no verification: the verifier ran %d times", runs)
	}
}

// CRW-965 (parent decision D3, the criteria part): a candidate revalidated while its merged tree is being verified is not
// published on the old verification. The swap is refused, and the branch does not move.
func TestIntegrationBatchRefusesACandidateRevalidatedDuringVerification(t *testing.T) {
	k := newBatchKit(t, batchNode{name: "a", files: map[string]string{"a.txt": "a\n"}})
	k.acceptByCommit("a")
	base := stubVerifier(writeStubVerifier(t))
	changed := false
	deps := IntegrationBatchDeps{Verify: func(ctx context.Context, dir string, env []string) error {
		if err := base(ctx, dir, env); err != nil {
			return err
		}
		if !changed {
			changed = true
			k.invRevise("g", "a", "g-r2", func(n doc) { n["criteria_set_digest"] = dig("changed a") })
			k.revalidateCriteria(t, "a", dig("changed a"))
		}
		return nil
	}, Update: updateIntegrationRef}
	_, err := k.sched.IntegrateBatch(context.Background(), k.batchIn(), deps)
	if refusalReasonOf(err) != "merge_candidate_moved" {
		t.Fatalf("a candidate revalidated during the verification must refuse the swap: %v", err)
	}
	if _, found := k.branchTip("dev-int"); found {
		t.Fatal("the branch moved on the old verification of a revalidated candidate")
	}
}

// CRW-965 (parent decision D6, the recovery): a move the reconciliation recorded after a crash is pushable, because its
// planned intent carries the verified head.
func TestPushAfterAReconciledMoveIsAllowed(t *testing.T) {
	k := newBatchKit(t, batchNode{name: "a", files: map[string]string{"a.txt": "a\n"}})
	remote := bareRemote(t)
	k.pushRepo(t, remote)
	k.acceptByCommit("a")
	ctx := context.Background()
	crashing := IntegrationBatchDeps{Verify: stubVerifier(writeStubVerifier(t)), Update: func(ctx context.Context, checkout, ref, newCommit, oldCommit string) error {
		if err := updateIntegrationRef(ctx, checkout, ref, newCommit, oldCommit); err != nil {
			return err
		}
		return errors.New("simulated crash between the swap and the commit")
	}}
	if _, err := k.sched.IntegrateBatch(ctx, k.batchIn(), crashing); err == nil {
		t.Fatal("the crashing batch should fail after the swap")
	}
	moved, found := k.branchTip("dev-int")
	if !found {
		t.Fatal("the swap should have moved the branch")
	}
	res, err := k.sched.IntegrateBatch(ctx, k.batchIn(), IntegrationBatchDeps{Verify: stubVerifier(writeStubVerifier(t)), Update: updateIntegrationRef})
	if err != nil || len(res.Reconciled) != 1 {
		t.Fatalf("the next run reconciles the move: %+v, %v", res, err)
	}
	push, err := PushIntegration(ctx, k.repo.path, "origin", "dev", "dev-int", k.sched.VerifiedMoveOnto)
	if err != nil || push.Outcome != PushPushed {
		t.Fatalf("the reconciled head is a verified move and must push: %+v, %v", push, err)
	}
	if got := remoteBranch(t, remote, "dev"); got != moved {
		t.Fatalf("remote dev is %s; want the reconciled head %s", got, moved)
	}
}

// CRW-965 (parent decision D4, the attempt that moved nothing): an attempt that failed before its move on one ref adds no
// completion target, and a later candidate that lands on another ref completes only there.
func TestIntegrationTargetsIgnoreAnAttemptThatMovedNothing(t *testing.T) {
	k := newBatchKit(t,
		batchNode{name: "a", files: map[string]string{"a.txt": "a\n"}},
		batchNode{name: "b", files: map[string]string{"b.txt": "b\n"}})
	k.acceptByCommit("a")
	k.acceptByCommit("b")
	ctx := context.Background()
	refused := errors.New("simulated refusal before the branch moved")
	failing := k.batchIn()
	failing.IntegrationRef = "dev-r"
	failing.Nodes = []string{"a"}
	if _, err := k.sched.IntegrateBatch(ctx, failing, IntegrationBatchDeps{Verify: stubVerifier(writeStubVerifier(t)), Update: func(context.Context, string, string, string, string) error {
		return refused
	}}); err == nil {
		t.Fatal("the failing attempt should not complete")
	}
	other := k.batchIn()
	other.IntegrationRef = "dev-r"
	other.Nodes = []string{"b"}
	if _, err := k.sched.IntegrateBatch(ctx, other, IntegrationBatchDeps{Verify: stubVerifier(writeStubVerifier(t)), Update: updateIntegrationRef}); err != nil {
		t.Fatalf("b on dev-r: %v", err)
	}
	landing := k.batchIn()
	landing.IntegrationRef = "dev-s"
	landing.Nodes = []string{"a"}
	if _, err := k.sched.IntegrateBatch(ctx, landing, IntegrationBatchDeps{Verify: stubVerifier(writeStubVerifier(t)), Update: updateIntegrationRef}); err != nil {
		t.Fatalf("a on dev-s: %v", err)
	}
	acc, found, err := loadActiveAcceptance(ctx, k.sched.Store.Q(ctx), "g", "a")
	if err != nil || !found {
		t.Fatalf("the active acceptance of a: %v, %v", found, err)
	}
	refs, err := integrationRefsOf(ctx, k.sched.Store.Q(ctx), acc.AcceptanceID)
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 1 || refs[0] != "dev-s" {
		t.Fatalf("a completes on dev-s only; the attempt on dev-r moved nothing for it: %v", refs)
	}
}
