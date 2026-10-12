package cli

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dag"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
)

// TestRunIdentityPinsThePlanRevision: a new production run records the revision its plan has when it starts, so what the run shows is
// the plan basis an answer is judged against; a plan the store does not hold is refused, and a run that names no plan pins none
// (CRW-1205 d5).
func TestRunIdentityPinsThePlanRevision(t *testing.T) {
	w := newWorld(t)
	repo := &dag.Repo{Store: w.st}
	for i, node := range []string{"a", "b", "c"} {
		raw, err := json.Marshal(map[string]any{"schema": dag.SchemaRevision, "plan_id": "plan-x", "project_key": "P-TEST", "request_id": "r" + node,
			"expected_parent_revision": i, "author_task_id": "task-test", "changes": []any{map[string]any{"op": dag.OpAddNode, "node": testsupport.NodeDoc(node, dag.NodeNonPR)}}})
		if err != nil {
			t.Fatal(err)
		}
		rev, err := dag.DecodeRevision(raw)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := repo.Put(context.Background(), rev); err != nil {
			t.Fatal(err)
		}
	}
	id, err := RunIdentity(ctx, w.st, Spec{Thread: "thread-owner", Args: json.RawMessage(`{"plan":"plan-x"}`)})
	if err != nil || id.PlanID != "plan-x" || id.PlanRevision != 3 || id.ActorTaskID != "thread-owner" {
		t.Fatalf("the identity: %+v %v", id, err)
	}
	if id, err := RunIdentity(ctx, w.st, Spec{Thread: "thread-owner", Args: json.RawMessage(`{}`)}); err != nil || id.PlanID != "" || id.PlanRevision != 0 {
		t.Errorf("a run with no plan: %+v %v", id, err)
	}
	if _, err := RunIdentity(ctx, w.st, Spec{Thread: "thread-owner", Args: json.RawMessage(`{"plan":"plan-ghost"}`)}); err == nil || !strings.Contains(err.Error(), "plan-ghost") {
		t.Errorf("a plan the store does not hold: %v", err)
	}
}
