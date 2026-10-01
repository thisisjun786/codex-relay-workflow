package delivery

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/execution"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/registry"
)

// The default role gate is the registry's role check against the execution policy this process
// was started with. Until 2026-10-01 it refused every bound task as if no policy were readable,
// so a completion event addressed to a parent bound with linkage-bind was withheld forever as
// role_policy_unconfigured even by a relay service started with its policy.
func TestTheDefaultRoleGateAuthorizesABoundTaskUnderThisProcessesPolicy(t *testing.T) {
	f := newFixture(t, "")
	const task = "01parent-bound"
	settings := taskSettings(f.root)
	if _, err := execSQL(f.ctx, f.store, "INSERT INTO authorized_settings (task_id, settings, source, recorded_at) VALUES (?,?,?,?)", task, settings, "creation_result", "t"); err != nil {
		t.Fatal(err)
	}
	gate := func() error {
		_, err := AuthorizedSettings(f.ctx, f.store, task, nil)
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

	if _, err := execSQL(f.ctx, f.store, "INSERT INTO scope_bindings (binding_id, role, scope_kind, scope_key, task_id, host_id, status, revision, created_at, updated_at) VALUES ('bnd-1','parent','project','PROJ-1',?,'host-a','active',1,'t','t')", task); err != nil {
		t.Fatal(err)
	}
	if err := gate(); Reason(err) != RolePolicyUnconfigured {
		t.Fatalf("a bound task without a policy: %v, want %s", err, RolePolicyUnconfigured)
	}

	usePolicy(`{"roles": {"parent": {"model": "anthropic/claude-opus-5", "reasoningEffort": "xhigh"}}}`)
	if err := gate(); err != nil {
		t.Fatalf("a bound parent whose recorded pair is the policy's parent pair is withheld: %v", err)
	}

	usePolicy(`{"roles": {"parent": {"model": "devin/swe-2", "reasoningEffort": "high"}}}`)
	if err := gate(); Reason(err) == "" || Reason(err) == RolePolicyUnconfigured {
		t.Fatalf("a bound parent whose recorded pair is not the policy's passed or was misread: %v", err)
	}
}
