package dagsched

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// CRW-952 c4 (answer 3): the batch's intent row freezes the record digest of each candidate, and a re-run after a
// re-validation with a new record is a different batch, so the old intent is never reused.

// premergeIntentDetail reads the intent row of batch and the frozen fields in it.
func premergeIntentDetail(t *testing.T, k *batchKit, batch string) map[string]string {
	t.Helper()
	rows, err := store.IntegrationStagesOfPlan(context.Background(), k.sched.Store, "g")
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if r.BatchID == batch && r.Stage == "intent" {
			var fields map[string]string
			if err := json.Unmarshal([]byte(r.Detail), &fields); err != nil {
				t.Fatal(err)
			}
			return fields
		}
	}
	t.Fatalf("batch %s has no intent row", batch)
	return nil
}

func TestPremergeIntentFreezesTheDigestAndAReRunDoesNotReuseIt(t *testing.T) {
	k := newBatchKit(t, batchNode{name: "a", files: map[string]string{"a.txt": "a\n"}})
	k.acceptByCommit("a")
	ctx := context.Background()
	failing := func(context.Context, string, []string) error { return errors.New("the merged tree fails") }
	first, err := k.sched.IntegrateBatch(ctx, k.batchIn(), IntegrationBatchDeps{Verify: failing, Update: updateIntegrationRef})
	if err != nil {
		t.Fatalf("the failing batch: %v", err)
	}
	if len(first.Merged) != 0 {
		t.Fatalf("a failing batch merged %+v", first.Merged)
	}
	firstDigest := premergeIntentDetail(t, k, first.BatchID)["premerge.a"]
	if firstDigest == "" {
		t.Fatal("the intent row does not freeze the record digest of a")
	}

	// the record of a is judged again under changed criteria: a new revalidation row with its own record and digest
	acc := k.acceptByCommitForCriteria("a")
	k.invRevise("g", "a", "g-r2", func(n doc) { n["criteria_set_digest"] = dig("changed a") })
	var event string
	if err := k.s.DB.QueryRow("SELECT event_id FROM events WHERE relationship_id = ?", acc.RelationshipID).Scan(&event); err != nil {
		t.Fatal(err)
	}
	k.exec("UPDATE canonical_criteria SET set_digest = ? WHERE relationship_id = ?", dig("changed a"), acc.RelationshipID)
	k.exec("INSERT INTO dag_acceptance_revalidations (revalidation_id, acceptance_id, criteria_set_digest, event_id, verdict_turn_id, reval_seq, revalidated_by, revalidated_at) VALUES ('rv-a-intent', ?, ?, ?, 'vt3', 1, 'parent', 't')",
		acc.AcceptanceID, dig("changed a"), event)
	premergeRevalidationRecord(k.t, k.s.DB, acc.AcceptanceID, "rv-a-intent", dig("changed a"))

	second, err := k.sched.IntegrateBatch(ctx, k.batchIn(), IntegrationBatchDeps{Verify: stubVerifier(writeStubVerifier(t)), Update: updateIntegrationRef})
	if err != nil {
		t.Fatalf("the re-run: %v", err)
	}
	if second.BatchID == first.BatchID {
		t.Fatal("a re-run after a re-validation with a new record reused the old batch identity")
	}
	secondDigest := premergeIntentDetail(t, k, second.BatchID)["premerge.a"]
	if secondDigest == "" || secondDigest == firstDigest {
		t.Fatalf("the re-run froze %q; the first batch froze %q: they must differ", secondDigest, firstDigest)
	}
	if got := premergeIntentDetail(t, k, first.BatchID)["premerge.a"]; got != firstDigest {
		t.Fatalf("the first intent row changed: %q, want %q", got, firstDigest)
	}
}
