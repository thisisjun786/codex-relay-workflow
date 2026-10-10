package hook_test

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
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

// Round 4 (regression of the d4 fix): the idle-edit counter creates the state of an armed thread
// that has none, so at a cwd away from an idle anchored root it moves the anchor first, and writes
// nothing when the anchor cannot follow.
func TestIdleEditAwayFromAnIdleAnchorFollowsOrWritesNothing(t *testing.T) {
	for _, writable := range []bool{true, false} {
		t.Run(map[bool]string{true: "anchor follows", false: "anchor cannot follow"}[writable], func(t *testing.T) {
			if !writable && os.Geteuid() == 0 {
				t.Skip("root ignores directory permissions")
			}
			env, dir := idleEnv(t)
			idleGoal(t, dir, "active")
			a, b := t.TempDir(), t.TempDir()
			if err := stateroot.Guard(env, a, a, "s1"); err != nil {
				t.Fatal(err)
			}
			path := stateroot.AnchorPath(env, "s1")
			if !writable {
				if err := os.Chmod(filepath.Dir(path), 0o500); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = os.Chmod(filepath.Dir(path), 0o700) })
			}
			hook.HandleIdleEditAdvisory(idlePayload(b, "s1", "apply_patch"), env)
			_, statErr := os.Stat(state.StatePath(b, "s1"))
			raw, _ := os.ReadFile(path)
			if writable {
				if statErr != nil || !strings.Contains(string(raw), b) {
					t.Fatalf("state %v, anchor %s", statErr, raw)
				}
				return
			}
			if !errors.Is(statErr, fs.ErrNotExist) {
				t.Fatalf("the counter created a state the anchor does not track: %v", statErr)
			}
		})
	}
}
