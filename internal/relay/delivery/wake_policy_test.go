package delivery

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/execution"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/registry"
)

// A wake identity is measured under the role policy its caller names rather than under this
// process's environment: the flow CLI names the policy its service declares, so the identity's
// RolePolicy digest and the gate that authorizes the recorded settings come from one policy.
// sequential: t.Setenv(execution.EnvPolicy) is process-wide.
func TestWakeConfigurationIsMeasuredUnderTheGivenRolePolicy(t *testing.T) {
	f := newFixture(t, "")
	const task = "01parent-wake"
	if _, err := execSQL(f.ctx, f.store, "INSERT INTO authorized_settings (task_id, settings, source, recorded_at) VALUES (?,?,?,?)", task, taskSettings(f.root), "creation_result", "t"); err != nil {
		t.Fatal(err)
	}
	if _, err := execSQL(f.ctx, f.store, "INSERT INTO scope_bindings (binding_id, role, scope_kind, scope_key, task_id, host_id, status, revision, created_at, updated_at) VALUES ('bnd-wake','parent','project','PROJ-WAKE',?,'host-a','active',1,'t','t')", task); err != nil {
		t.Fatal(err)
	}
	t.Setenv(execution.EnvPolicy, "")
	registry.ResetRolePolicySnapshot()
	t.Cleanup(registry.ResetRolePolicySnapshot)
	path := filepath.Join(t.TempDir(), "execution-policy.json")
	if err := os.WriteFile(path, []byte(`{"roles": {"parent": {"model": "anthropic/claude-opus-5", "reasoningEffort": "xhigh"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	policy := registry.ResolveRolePolicy(map[string]string{execution.EnvPolicy: path})
	observe := func() (WakeIdentity, error) {
		return WakeIdentity{OperatingScope: "host-user", AppServerInstance: "server-instance", Socket: "socket-a",
			ServiceInstance: "service-instance", TransportRevision: "transport-revision"}, nil
	}
	want, err := observeWakeConfiguration(f.ctx, f.store, policy, task, "session_step_waiting", nil, observe)
	if err != nil {
		t.Fatalf("a parent whose recorded pair is the given policy's parent pair is withheld: %v", err)
	}
	if want.Identity.RolePolicy != policy.Digest() || want.Identity.Role != "parent" {
		t.Fatalf("identity policy %q role %q, want %q and parent", want.Identity.RolePolicy, want.Identity.Role, policy.Digest())
	}
	unresolved := registry.RolePolicy{Detail: "probe-detail"}
	if _, err := observeWakeConfiguration(f.ctx, f.store, unresolved, task, "session_step_waiting", nil, observe); err == nil || Reason(err) != RolePolicyUnconfigured || !strings.Contains(err.Error(), "probe-detail") {
		t.Fatalf("an unresolved policy must refuse as %s with its detail: %v", RolePolicyUnconfigured, err)
	}
}
