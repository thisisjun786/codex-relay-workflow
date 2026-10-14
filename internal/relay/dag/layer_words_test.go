package dag

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/layername"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// CRW-1449: a write whose claim was made under a binding that is no longer a live 자식 binding of the plan's project is refused
// stale_coordinator_epoch, and the refusal names the layer (자식), not the relay role id parent.
func TestStaleEpochNamesLayers(t *testing.T) {
	r, s, _ := newRepo(t)
	ctx := context.Background()
	parentBinding(t, s, "b1", "t1", "P-TEST", "active")
	if _, err := s.DB.Exec("UPDATE scope_bindings SET role = 'child' WHERE binding_id = 'b1'"); err != nil {
		t.Fatal(err)
	}
	claimRow(t, s, "plan", 1, "b1", "t1", "n1")
	_, err := r.Put(ctx, decode(t, epochDoc("plan", "r1", 0, "t1", 1, "P-TEST", addNode("a", NodeNonPR))))
	var refused *store.RefusedError
	if !errors.As(err, &refused) || refused.Reason != "stale_coordinator_epoch" {
		t.Fatalf("want stale_coordinator_epoch, got %v", err)
	}
	if want := "which is not a live 자식 binding of project P-TEST any more"; !strings.Contains(refused.Detail, want) {
		t.Errorf("detail = %q, want it to say %q", refused.Detail, want)
	}
	if hits := layername.Find(refused.Detail); len(hits) != 0 {
		t.Errorf("%q names a bare role id: %v", refused.Detail, hits)
	}
}
