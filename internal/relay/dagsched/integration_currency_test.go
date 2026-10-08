package dagsched

import (
	"context"
	"testing"
)

// CRW-965 (parent decision D3): a candidate that stops being current between its verification and the swap refuses the
// batch under merge_candidate_moved. The branch does not move and nothing is marked; the next batch leaves that candidate
// out and verifies the rest again.
func TestIntegrationBatchRefusesACandidateThatChangedAfterVerification(t *testing.T) {
	changes := map[string]func(k *batchKit){
		"cancelled": func(k *batchKit) {
			k.exec("UPDATE relationships SET status = 'cancelled' WHERE relationship_id = (SELECT relationship_id FROM dag_node_executions WHERE plan_id = 'g' AND node_id = 'a')")
		},
		"superseded": func(k *batchKit) {
			k.exec("UPDATE dag_acceptances SET state = 'superseded' WHERE plan_id = 'g' AND node_id = 'a' AND state = 'active'")
		},
		"closed": func(k *batchKit) {
			k.exec("UPDATE relationships SET status = 'closed' WHERE relationship_id = (SELECT relationship_id FROM dag_node_executions WHERE plan_id = 'g' AND node_id = 'a')")
		},
	}
	for name, change := range changes {
		t.Run(name, func(t *testing.T) {
			k := newBatchKit(t,
				batchNode{name: "a", files: map[string]string{"a.txt": "a\n"}},
				batchNode{name: "b", files: map[string]string{"b.txt": "b\n"}})
			k.acceptByCommit("a")
			k.acceptByCommit("b")
			ctx := context.Background()
			base := stubVerifier(writeStubVerifier(t))
			changed := false
			deps := IntegrationBatchDeps{Verify: func(ctx context.Context, dir string, env []string) error {
				if err := base(ctx, dir, env); err != nil {
					return err
				}
				if !changed {
					changed = true
					change(k)
				}
				return nil
			}, Update: updateIntegrationRef}
			_, err := k.sched.IntegrateBatch(ctx, k.batchIn(), deps)
			if refusalReasonOf(err) != "merge_candidate_moved" {
				t.Fatalf("a candidate that changed after the verification must refuse the batch as merge_candidate_moved: %v", err)
			}
			if _, found := k.branchTip("dev-int"); found {
				t.Fatal("the branch moved although a candidate was no longer current")
			}
			if countStage(k.stageRows(), "marked") != 0 {
				t.Fatal("a refused batch must not mark any candidate")
			}
			verifies := 0
			counting := IntegrationBatchDeps{Verify: func(ctx context.Context, dir string, env []string) error {
				verifies++
				return base(ctx, dir, env)
			}, Update: updateIntegrationRef}
			res, err := k.sched.IntegrateBatch(ctx, k.batchIn(), counting)
			if err != nil {
				t.Fatalf("the next batch: %v", err)
			}
			if len(res.Merged) != 1 || res.Merged[0].NodeID != "b" {
				t.Fatalf("the next batch should integrate only b: %+v", res.Merged)
			}
			if verifies != 1 {
				t.Fatalf("the next batch verifies the surviving tree once; it ran %d times", verifies)
			}
		})
	}
}
