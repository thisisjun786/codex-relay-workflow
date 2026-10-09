package hook_test

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/hook"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/stateroot"
)

// CRW-1140 round 3 (d1): the idle-edit counter is a state writer too; at the cwd away from the
// anchored root of an in-flight thread it must not create the file.
func TestIdleEditAwayFromTheAnchoredRootCreatesNoState(t *testing.T) {
	env, dir := idleEnv(t)
	idleGoal(t, dir, "active")
	a, b := t.TempDir(), t.TempDir()
	s := state.DefaultState("s1", "work")
	s.Phase, s.OrchestrationActive = state.PhaseP, true
	epoch := "epoch-1"
	s.PlanEpoch = &epoch
	if err := state.WriteState(a, s); err != nil {
		t.Fatal(err)
	}
	if err := stateroot.Guard(env, a, a, "s1"); err != nil {
		t.Fatal(err)
	}
	hook.HandleIdleEditAdvisory(idlePayload(b, "s1", "apply_patch"), env)
	if _, err := os.Stat(state.StatePath(b, "s1")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the counter created a state at the other cwd: %v", err)
	}
	if _, err := os.Stat(filepath.Join(b, ".crw")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the counter created %s: %v", filepath.Join(b, ".crw"), err)
	}
}
