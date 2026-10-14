package capacity

import (
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
)

// CRW-1390 (Argon V5 P3 of 146cbab5, checkDeclarer): a management seat holder states or observes a store-wide figure in a store whose
// store seat was never bound, so the store scope has no declarer at all (StoreScopeSupervisor names nobody). The figure is accepted
// (fail open), and the warning says the store has no store-scope supervisor and names none.
func TestStoreDeclarationOfAManagementHolderWithNoStoreSeatWarnsThatThereIsNoDeclarer(t *testing.T) {
	e := newEnv(t)
	const manager = "01loneManager"
	bindManagementSeat(t, e, manager)
	limit := storeSeatLimit()
	limit.DeclaredBy, limit.Ceiling = manager, 25
	answer, err := e.cap.DeclareLimit(ctx(), limit)
	if err != nil {
		t.Fatalf("the management holder's store declaration was refused: %v", err)
	}
	if got := field(answer, "ceiling"); got != 25.0 {
		t.Fatalf("ceiling %v, want 25 (fail open)", got)
	}
	warning, _ := field(answer, "declarerWarning").(contract.OrderedObject)
	if warning == nil || field(warning, "declaredBy") != manager || field(warning, "reason") != string(contract.RefusalScopeRoleMismatch) {
		t.Fatalf("declarerWarning %v, want the holder named with scope_role_mismatch", field(answer, "declarerWarning"))
	}
	if holders, ok := field(warning, "storeScopeSupervisor").([]any); !ok || len(holders) != 0 {
		t.Fatalf("the warning names store-scope supervisors %v, want none", field(warning, "storeScopeSupervisor"))
	}
	if detail, _ := field(warning, "detail").(string); !strings.Contains(detail, "the store has no store-scope supervisor") {
		t.Fatalf("the warning's detail %q does not say the store has no store-scope supervisor", detail)
	}
	seen, err := e.cap.Observe(ctx(), Observation{ScopeKind: "store", ScopeKey: "store", Dimension: "model_cost", Observed: 2, ObservedBy: manager, Method: "counted"})
	if err != nil {
		t.Fatalf("the holder's store observation was refused: %v", err)
	}
	if _, warned := seen.Lookup("declarerWarning"); !warned {
		t.Fatalf("the observation was not warned: %v", seen)
	}
	recorded := declarerWarnings(t, e)
	if len(recorded) != 2 || recorded[0]["action"] != "declare_limit" || recorded[1]["action"] != "observe" || recorded[0]["declaredBy"] != manager {
		t.Fatalf("store_scope_declarer_warned rows %v, want the declaration and the observation", recorded)
	}
}
