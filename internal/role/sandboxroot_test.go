package role

import (
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/sandboxroot"
	"github.com/thisisjun786/codex-relay-workflow/internal/sandboxroot/sandboxroottest"
)

// CRW-1265. A managed spawn's child works in the parent's cwd, so a parent session opened in the
// account home, above it or above a project root cannot spawn a (sandboxed) child, and the refusal
// says why. The selection step refuses, before the dispatch record is touched.
func TestManagedSpawnRefusesHomeParentCwd(t *testing.T) {
	w := sandboxroottest.Fake(t)
	project := w.Mkdir(t, w.Home, "code", "aas")
	w.Project(t, project)
	for name, cwd := range map[string]string{"the account home": w.Home, "above the home": w.Root + "/home", "above a project": project[:len(project)-len("/aas")]} {
		t.Run(name, func(t *testing.T) {
			env, _ := home(t)
			start := dispatchTestCall(t, cwd, env, map[string]any{"action": "start", "role": "executor"})
			claim := dispatchTestCall(t, cwd, env, map[string]any{"action": "claim", "attemptId": start.AttemptID})
			got, err := ManagedSpawn(cwd, "session-test", claim.Marker)
			if err == nil || got != nil || !strings.Contains(err.Error(), "managed spawn refused") || !strings.Contains(err.Error(), sandboxroot.Code) {
				t.Fatalf("selection %+v, error %v", got, err)
			}
			if _, err := IssueManagedSpawn(cwd, "session-test", claim.Marker, nil); err == nil {
				t.Fatal("the issuance went ahead")
			}
		})
	}
	// A message without the marker is no spawn of ours and is judged by nobody.
	if got, err := ManagedSpawn(w.Home, "session-test", "TASK"); got != nil || err != nil {
		t.Fatalf("an unmarked message: %+v %v", got, err)
	}
	// A parent in a dedicated directory, or in a project root, selects as before.
	for _, cwd := range []string{w.Mkdir(t, w.Sandbox, "run"), project} {
		env, _ := home(t)
		start := dispatchTestCall(t, cwd, env, map[string]any{"action": "start", "role": "executor"})
		claim := dispatchTestCall(t, cwd, env, map[string]any{"action": "claim", "attemptId": start.AttemptID})
		if got, err := ManagedSpawn(cwd, "session-test", claim.Marker); err != nil || got == nil {
			t.Fatalf("%s: selection %+v, error %v", cwd, got, err)
		}
	}
}
