package dagsched

import (
	"context"
	"reflect"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// acceptByCommitForCriteria accepts one node through the production commit path and returns its acceptance.
func (k *batchKit) acceptByCommitForCriteria(node string) AcceptResult {
	k.t.Helper()
	head := k.heads[node]
	record := writeSealedRecord(k.t, k.repo.path, head, k.base, k.treeOf(head), "pass")
	out, err := k.sched.Accept(context.Background(), "g", node, "parent", AcceptInput{
		RuleVersion: VerifierRule{SkillsDigest: dig("skills"), Model: "m", Effort: "none"},
		Commit:      &CommitRef{Head: head, Base: k.base, Checkout: k.repo.path, Record: record},
	})
	if err != nil {
		k.t.Fatalf("accept %s: %v", node, err)
	}
	return out
}

// CRW-965 (parent decision d2): a criteria change is verified again through the scheduler's revalidation, and the next
// batch verifies the node afresh. An integrated node is never merged or verified again, an unrevalidated candidate is
// not ready, and the frozen rows name the criteria set each candidate is verified under.
func TestIntegrationBatchReadsACriteriaChangeAsDecided(t *testing.T) {
	k := newBatchKit(t, batchNode{name: "a", files: map[string]string{"a.txt": "a\n"}}, batchNode{name: "b", files: map[string]string{"b.txt": "b\n"}})
	runs := 0
	base := stubVerifier(writeStubVerifier(t))
	deps := IntegrationBatchDeps{Verify: func(ctx context.Context, dir string, env []string) error {
		runs++
		return base(ctx, dir, env)
	}, Update: updateIntegrationRef}
	ctx := context.Background()

	// A is accepted and integrated.
	k.acceptByCommit("a")
	in := k.batchIn()
	in.Nodes = []string{"a"}
	if _, err := k.sched.IntegrateBatch(ctx, in, deps); err != nil {
		t.Fatalf("batch a: %v", err)
	}
	if runs != 1 {
		t.Fatalf("verifier ran %d times for A; want 1", runs)
	}

	// A's criteria change: A is stale and an explicit request for it names the integration instead of a generic refusal.
	k.invRevise("g", "a", "g-r2", func(n doc) { n["criteria_set_digest"] = dig("changed a") })
	if got := invStaleIDs(k.read("g")); !reflect.DeepEqual(got, []string{"a"}) {
		t.Fatalf("after A's criteria change stale = %v, want [a]", got)
	}
	if _, err := k.sched.IntegrateBatch(ctx, in, deps); refusalReasonOf(err) != "disposition_conflict" {
		t.Fatalf("an integrated node named again: reason %q (err %v)", refusalReasonOf(err), err)
	}
	if runs != 1 {
		t.Fatalf("verifier ran %d times after A's change; an integrated node must not be verified again", runs)
	}

	// B is accepted, its criteria change before any batch: B is stale, is not merged and is not verified.
	bAccept := k.acceptByCommitForCriteria("b")
	k.invRevise("g", "b", "g-r3", func(n doc) { n["criteria_set_digest"] = dig("changed b") })
	if got := invStaleIDs(k.read("g")); !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Fatalf("after B's criteria change stale = %v, want [a b]", got)
	}
	if res, err := k.sched.IntegrateBatch(ctx, k.batchIn(), deps); err == nil && len(res.Merged) != 0 {
		t.Fatalf("a stale candidate was merged: %+v", res)
	}
	if runs != 1 {
		t.Fatalf("verifier ran %d times for a stale candidate; want 1", runs)
	}

	// The re-verification under the new criteria (the revalidation row, as the scheduler leaves it) makes B a candidate
	// again; the next batch verifies it afresh and freezes the new criteria digest on its row.
	var event string
	if err := k.s.DB.QueryRow("SELECT event_id FROM events WHERE relationship_id = ?", bAccept.RelationshipID).Scan(&event); err != nil {
		t.Fatal(err)
	}
	k.exec("UPDATE canonical_criteria SET set_digest = ? WHERE relationship_id = ?", dig("changed b"), bAccept.RelationshipID)
	k.exec("INSERT INTO dag_acceptance_revalidations (revalidation_id, acceptance_id, criteria_set_digest, event_id, verdict_turn_id, reval_seq, revalidated_by, revalidated_at) VALUES ('rv-b', ?, ?, ?, 'vt2', 1, 'parent', 't')",
		bAccept.AcceptanceID, dig("changed b"), event)
	if got := invStaleIDs(k.read("g")); !reflect.DeepEqual(got, []string{"a"}) {
		t.Fatalf("after B's revalidation stale = %v, want [a]", got)
	}
	if _, err := k.sched.IntegrateBatch(ctx, k.batchIn(), deps); err != nil {
		t.Fatalf("batch b: %v", err)
	}
	if runs != 2 {
		t.Fatalf("verifier ran %d times after B's revalidation; want 2", runs)
	}
	rows, err := store.IntegrationStagesOfPlan(ctx, k.sched.Store, "g")
	if err != nil {
		t.Fatal(err)
	}
	var frozen []string
	for _, r := range rows {
		if r.Stage == "intent" && r.NodeID == "b" {
			frozen = append(frozen, r.Detail)
		}
	}
	if !reflect.DeepEqual(frozen, []string{dig("changed b")}) {
		t.Fatalf("B's frozen intent rows name criteria %v; want the new set", frozen)
	}
}
