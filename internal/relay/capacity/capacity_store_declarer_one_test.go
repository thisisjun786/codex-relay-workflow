package capacity

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dispatch"
)

// The store scope's capacity has one declarer (CRW-1361): the holder StoreScopeSupervisor answers,
// that is the live store seat or the management seat that took a laid-down store seat over. Any other
// live management seat holder still states a store-wide bound (a declaration is reversible, so it fails
// open), but the answer warns under declarerWarning and a store_scope_declarer_warned journal row
// records who declared beside the holder.

// relayLine runs one codex-session-relay line over this env's store and fails the test on a non-zero exit.
func relayLine(t *testing.T, e *env, argv ...string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	line := append([]string{"--state", filepath.Dir(e.store.Path)}, argv...)
	if code := dispatch.Execute(ctx(), "codex-session-relay", line, &stdout, &stderr); code != 0 {
		t.Fatalf("%v: exit %d stdout %q stderr %q", argv, code, stdout.String(), stderr.String())
	}
}

// bindManagementSeat registers task's own management seat.
func bindManagementSeat(t *testing.T, e *env, task string) {
	t.Helper()
	relayLine(t, e, "linkage-bind", "--role", "supervisor", "--scope-kind", "management", "--scope", task, "--task", task, "--host", "host-s")
}

// declarerWarnings are the store_scope_declarer_warned journal rows, as decoded details.
func declarerWarnings(t *testing.T, e *env) []map[string]any {
	t.Helper()
	rows, err := e.store.All(ctx(), "SELECT subject, detail FROM journal WHERE kind = 'store_scope_declarer_warned' ORDER BY seq")
	if err != nil {
		t.Fatal(err)
	}
	out := make([]map[string]any, len(rows))
	for i, row := range rows {
		if err := json.Unmarshal([]byte(row.Text("detail")), &out[i]); err != nil {
			t.Fatal(err)
		}
	}
	return out
}

func TestStoreDeclarerIsTheStoreScopeSupervisorHolderAndOthersAreWarned(t *testing.T) {
	e := newEnv(t)
	const other = "01otherManager"
	// The store seat's holder registers its management seat (laying the store seat down, so that seat is
	// the store-scope supervisor now); another management session registers its own seat.
	bindStoreSeat(t, e)
	bindManagementSeat(t, e, storeSeatTask)
	bindManagementSeat(t, e, other)

	// The holder declares: no warning, no record.
	answer, err := e.cap.DeclareLimit(ctx(), storeSeatLimit())
	if err != nil {
		t.Fatal(err)
	}
	if _, warned := answer.Lookup("declarerWarning"); warned || len(declarerWarnings(t, e)) != 0 {
		t.Fatalf("the store-scope supervisor's own declaration was warned: %v", answer)
	}

	// The other management holder declares: it fails open (the ceiling is written) and is warned and recorded.
	limit := storeSeatLimit()
	limit.DeclaredBy, limit.Ceiling = other, 30
	answer, err = e.cap.DeclareLimit(ctx(), limit)
	if err != nil {
		t.Fatalf("another management holder's store declaration was refused: %v", err)
	}
	if got := field(answer, "ceiling"); got != 30.0 {
		t.Fatalf("ceiling %v after the other holder's declaration, want 30 (fail open)", got)
	}
	warning, _ := field(answer, "declarerWarning").(contract.OrderedObject)
	if warning == nil || field(warning, "declaredBy") != other || field(warning, "reason") != string(contract.RefusalScopeRoleMismatch) {
		t.Fatalf("declarerWarning %v, want the other holder named with scope_role_mismatch", field(answer, "declarerWarning"))
	}
	if holders, _ := field(warning, "storeScopeSupervisor").([]any); len(holders) != 1 || holders[0] != storeSeatTask {
		t.Fatalf("the warning names store-scope supervisor %v, want %q", field(warning, "storeScopeSupervisor"), storeSeatTask)
	}
	recorded := declarerWarnings(t, e)
	if len(recorded) != 1 || recorded[0]["declaredBy"] != other || recorded[0]["action"] != "declare_limit" || recorded[0]["scopeKind"] != "store" {
		t.Fatalf("store_scope_declarer_warned rows %v, want one for the other holder's declaration", recorded)
	}

	// The observation path shares the rule.
	seen, err := e.cap.Observe(ctx(), Observation{ScopeKind: "store", ScopeKey: "store", Dimension: "model_cost", Observed: 3, ObservedBy: other, Method: "counted"})
	if err != nil {
		t.Fatalf("another management holder's store observation was refused: %v", err)
	}
	if _, warned := seen.Lookup("declarerWarning"); !warned {
		t.Fatalf("the other holder's observation was not warned: %v", seen)
	}
	if recorded := declarerWarnings(t, e); len(recorded) != 2 || recorded[1]["action"] != "observe" {
		t.Fatalf("store_scope_declarer_warned rows %v, want the observation recorded too", recorded)
	}

	// A task with no store or management seat is still refused.
	limit.DeclaredBy = "01nobody"
	if _, err := e.cap.DeclareLimit(ctx(), limit); reasonOf(err) != string(contract.RefusalScopeRoleMismatch) {
		t.Fatalf("a task with no seat: reason %q (%v), want scope_role_mismatch", reasonOf(err), err)
	}
}
