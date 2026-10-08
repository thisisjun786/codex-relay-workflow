package delivery

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/execution"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/registry"
)

// A task's record need not cite its own role: the sender's gate resolves the role the task is
// bound to and authorizes the send against that role's pair. A rule that reads the role from the
// record alone finds none and would silently send no auto-compaction limit on a delivery the gate
// accepted, which is exactly the case the limit exists to cover. The gate therefore reports the
// role it authorized against, so the transport reads the pair's limit from that role.
// sequential: t.Setenv(execution.EnvPolicy) is process-wide.
func TestTheDefaultRoleGateReportsTheBoundRoleItAuthorizedAgainst(t *testing.T) {
	f := newFixture(t, "")
	const task = "01parent-bound"
	settings := taskSettings(f.root)
	// The policy declares exactly the pair the fixture records, read from the fixture rather than
	// restated, so this test cannot drift away from what the gate is asked to authorize.
	record := loadsObj(settings)
	model, _ := record.Lookup("model")
	effort, _ := record.Lookup("reasoningEffort")
	declared := fmt.Sprintf(`{"roles": {"parent": {"model": %q, "reasoningEffort": %q, "autoCompactTokenLimit": 550000}}}`, model, effort)
	if _, err := execSQL(f.ctx, f.store, "INSERT INTO authorized_settings (task_id, settings, source, recorded_at) VALUES (?,?,?,?)", task, settings, "creation_result", "t"); err != nil {
		t.Fatal(err)
	}
	var last *TaskSettings
	gate := func() error {
		var err error
		last, err = AuthorizedSettings(f.ctx, f.store, task, nil)
		return err
	}
	usePolicy := func(text string) {
		t.Helper()
		if text == "" {
			t.Setenv(execution.EnvPolicy, "")
		} else {
			path := filepath.Join(t.TempDir(), "execution-policy.json")
			if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
				t.Fatal(err)
			}
			t.Setenv(execution.EnvPolicy, path)
		}
		registry.ResetRolePolicySnapshot()
	}
	t.Cleanup(registry.ResetRolePolicySnapshot)

	usePolicy("")
	if err := gate(); err != nil {
		t.Fatalf("a task bound to no role is gated: %v", err)
	}
	if last.BoundRole != "" {
		t.Fatalf("a task bound to no role reported bound role %q", last.BoundRole)
	}
	if _, err := execSQL(f.ctx, f.store, "INSERT INTO scope_bindings (binding_id, role, scope_kind, scope_key, task_id, host_id, status, revision, created_at, updated_at) VALUES ('bnd-1','parent','project','PROJ-1',?,'host-a','active',1,'t','t')", task); err != nil {
		t.Fatal(err)
	}
	usePolicy(declared)
	if err := gate(); err != nil {
		t.Fatalf("a bound parent whose recorded pair is the policy's is withheld: %v", err)
	}
	if _, cited := last.Data.Lookup("citedRole"); cited {
		t.Fatal("the fixture cites its own role, so it does not exercise the bound-role path")
	}
	if last.BoundRole != "parent" {
		t.Fatalf("BoundRole = %q, want the role the gate authorized against (parent)", last.BoundRole)
	}
}
