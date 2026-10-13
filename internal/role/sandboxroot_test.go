package role

import (
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/sandboxroot"
	"github.com/thisisjun786/codex-relay-workflow/internal/sandboxroot/sandboxroottest"
)

// CRW-1265. A managed spawn's child works in the parent's cwd. CRW-1265 refused a parent session opened
// in the account home, above it or above a project root at the selection step; since CRW-1308 the
// selection and the issuance go ahead, and the rule's verdict is a recorded warning.
func TestManagedSpawnRefusesHomeParentCwd(t *testing.T) {
	w := sandboxroottest.Fake(t)
	project := w.Mkdir(t, w.Home, "code", "aas")
	w.Project(t, project)
	for name, c := range map[string]struct{ cwd, rule string }{
		"the account home": {w.Home, sandboxroot.RuleAccountHome},
		"above the home":   {w.Root + "/home", sandboxroot.RuleHomeAncestor},
		"above a project":  {project[:len(project)-len("/aas")], sandboxroot.RuleProjectAncestor},
	} {
		t.Run(name, func(t *testing.T) {
			w.ClearWarnings(t)
			env, _ := home(t)
			start := dispatchTestCall(t, c.cwd, env, map[string]any{"action": "start", "role": "executor"})
			claim := dispatchTestCall(t, c.cwd, env, map[string]any{"action": "claim", "attemptId": start.AttemptID})
			got, err := ManagedSpawn(c.cwd, "session-test", claim.Marker)
			if err != nil || got == nil {
				t.Fatalf("selection %+v, error %v", got, err)
			}
			w.Warned(t, "role/managed_spawn", c.rule, c.cwd)
			if _, err := IssueManagedSpawn(c.cwd, "session-test", claim.Marker, nil); err != nil {
				t.Fatalf("the issuance did not go ahead: %v", err)
			}
		})
	}
	// A message without the marker is no spawn of ours and is judged by nobody.
	if got, err := ManagedSpawn(w.Home, "session-test", "TASK"); got != nil || err != nil {
		t.Fatalf("an unmarked message: %+v %v", got, err)
	}
	// A parent in a dedicated directory, or in a project root, selects as before, with no warning.
	w.ClearWarnings(t)
	for _, cwd := range []string{w.Mkdir(t, w.Sandbox, "run"), project} {
		env, _ := home(t)
		start := dispatchTestCall(t, cwd, env, map[string]any{"action": "start", "role": "executor"})
		claim := dispatchTestCall(t, cwd, env, map[string]any{"action": "claim", "attemptId": start.AttemptID})
		if got, err := ManagedSpawn(cwd, "session-test", claim.Marker); err != nil || got == nil {
			t.Fatalf("%s: selection %+v, error %v", cwd, got, err)
		}
	}
	if records := w.Warnings(); len(records) != 0 {
		t.Fatalf("an allowed parent was warned about: %+v", records)
	}
}
