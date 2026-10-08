package reset

import (
	"os"
	"path/filepath"
	"testing"
)

// resetLinkDotWorkspace makes a directory holding a "keep" directory (with one file) and a symlink
// named "link" pointing at target, and pins it for the link judgement. The pin is closed when the
// test ends.
func resetLinkDotWorkspace(t *testing.T, target string, keepMode os.FileMode) (*resetLinkWalkPin, string) {
	t.Helper()
	dir := t.TempDir()
	keep := filepath.Join(dir, "keep")
	if err := os.Mkdir(keep, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(keep, "inner.txt"), []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	if keepMode != 0 {
		if err := os.Chmod(keep, keepMode); err != nil {
			t.Fatal(err)
		}
		// Restore search-only directories before t.TempDir's own cleanup walks the tree.
		t.Cleanup(func() { _ = os.Chmod(keep, 0o755) })
	}
	if err := os.Symlink(target, filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	pinned, err := resetLinkWalkPinOf(root)
	if err != nil {
		root.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pinned.Close() })
	return pinned, dir
}

// TestResetLinkDotTargetSkipsTheRootStat: a link whose Readlink target ends in a "." or ".."
// component is judged by the walk, which reads the target's components through the pinned
// descriptor with Lstat and Readlink only, so the target directory is never opened the way
// os.Root.Stat would open it with O_DIRECTORY (CRW-554); every other target keeps the descriptor
// path. Verdicts match the descriptor path's.
func TestResetLinkDotTargetSkipsTheRootStat(t *testing.T) {
	for _, tc := range []struct {
		name      string
		target    string
		keepMode  os.FileMode
		want      bool
		wantCalls int
	}{
		{"keep_slash_dot", "keep/.", 0, true, 0},
		{"keep_slash_dotdot", "keep/..", 0, true, 0},
		{"missing_slash_dot", "missing/.", 0, false, 0},
		{"search_only_keep_slash_dot", "keep/.", 0o311, true, 0},
		// A target that stays inside the root is decided by the walk alone, dot-ending or not, so the
		// descriptor path is never asked for it. This row is the one expectation CRW-927 changes.
		{"plain_keep_is_decided_by_the_walk", "keep", 0, true, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, _ := resetLinkDotWorkspace(t, tc.target, tc.keepMode)
			calls := 0
			stat := func(name string) (os.FileInfo, error) {
				calls++
				return root.Stat(name)
			}
			got, err := resetLinkTargetExistsWith(root, "link", stat)
			if err != nil {
				t.Fatalf("resetLinkTargetExistsWith: %v", err)
			}
			if got != tc.want {
				t.Errorf("exists = %v, want %v", got, tc.want)
			}
			if calls != tc.wantCalls {
				t.Errorf("root stat calls = %d, want %d", calls, tc.wantCalls)
			}
		})
	}
}

// TestResetLinkDotTargetRemovesTheLinkNotTheTarget: through resetRmIfExists, a link whose target is
// "keep/." is present and removed (the link itself), leaving the target directory; one whose target
// is "missing/." is absent and stays.
func TestResetLinkDotTargetRemovesTheLinkNotTheTarget(t *testing.T) {
	t.Run("present", func(t *testing.T) {
		root, dir := resetLinkDotWorkspace(t, "keep/.", 0)
		result := ResetResult{Removed: []string{}, Absent: []string{}}
		if err := resetRmIfExists(root, "link", "link", &result); err != nil {
			t.Fatalf("resetRmIfExists: %v", err)
		}
		if len(result.Removed) != 1 || result.Removed[0] != "link" {
			t.Fatalf("removed = %v, want [link]", result.Removed)
		}
		if _, err := os.Lstat(filepath.Join(dir, "link")); !os.IsNotExist(err) {
			t.Errorf("link remains: %v", err)
		}
		if _, err := os.Stat(filepath.Join(dir, "keep", "inner.txt")); err != nil {
			t.Errorf("target directory lost: %v", err)
		}
	})
	t.Run("absent", func(t *testing.T) {
		root, dir := resetLinkDotWorkspace(t, "missing/.", 0)
		result := ResetResult{Removed: []string{}, Absent: []string{}}
		if err := resetRmIfExists(root, "link", "link", &result); err != nil {
			t.Fatalf("resetRmIfExists: %v", err)
		}
		if len(result.Absent) != 1 || result.Absent[0] != "link" {
			t.Fatalf("absent = %v, want [link]", result.Absent)
		}
		if _, err := os.Lstat(filepath.Join(dir, "link")); err != nil {
			t.Errorf("dangling link was removed: %v", err)
		}
	})
}

// TestResetLinkDotTargetFailsClosedWhenThePinnedPathMoved: CRW-745 judged a dot-ending target on the
// root's own path, so it needed that path to still name the pinned directory and refused once another
// process renamed it. The walk through the pinned descriptor never uses the root's path name, so the
// verdict is the one dev had before CRW-745: the link is removed and its target survives. The name
// keeps the CRW-745 case label; its expectation is the one row this issue changes.
func TestResetLinkDotTargetFailsClosedWhenThePinnedPathMoved(t *testing.T) {
	base := t.TempDir()
	sessions := filepath.Join(base, "sessions")
	if err := os.MkdirAll(filepath.Join(sessions, "keep"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sessions, "keep", "inner.txt"), []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("keep/.", filepath.Join(sessions, "a.json")); err != nil {
		t.Fatal(err)
	}
	parent, err := os.OpenRoot(base)
	if err != nil {
		t.Fatal(err)
	}
	defer parent.Close()
	observed, err := parent.Lstat("sessions")
	if err != nil {
		t.Fatal(err)
	}
	pinned, err := resetPin(parent, "sessions", observed)
	if err != nil {
		t.Fatal(err)
	}
	defer pinned.Close()
	moved := filepath.Join(base, "moved")
	if err := os.Rename(sessions, moved); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(sessions, 0o755); err != nil {
		t.Fatal(err)
	}
	result := ResetResult{Removed: []string{}, Absent: []string{}}
	if err := resetRmIfExists(pinned, "a.json", "a.json", &result); err != nil {
		t.Fatalf("resetRmIfExists: %v", err)
	}
	if len(result.Removed) != 1 || result.Removed[0] != "a.json" {
		t.Errorf("removed = %v, want [a.json]", result.Removed)
	}
	if _, err := os.Lstat(filepath.Join(moved, "a.json")); !os.IsNotExist(err) {
		t.Errorf("the pinned link must be removed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(moved, "keep", "inner.txt")); err != nil {
		t.Errorf("target directory lost: %v", err)
	}
}

// TestResetLinkDotTargetFromACwdOutsideTheProcessDirectory: RunReset takes its workspace as an argument,
// which need not be the process working directory. The judgement walks the target through the pinned
// descriptor and never uses the root's path name, so it concerns the supplied cwd alone: the walk is
// anchored at the descriptor os.OpenRoot opened for that cwd, and Root.Name keeps the name it was given
// only for the out-of-root fallback. The dot-ending link is removed and its target survives.
func TestResetLinkDotTargetFromACwdOutsideTheProcessDirectory(t *testing.T) {
	process, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	if root == process {
		t.Fatal("the test workspace must differ from the process directory")
	}
	crw := filepath.Join(root, ".crw")
	if err := os.MkdirAll(filepath.Join(crw, "sessions", "keep"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(crw, "sessions", "keep", "inner.txt"), []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("keep/.", filepath.Join(crw, "sessions", "a.json")); err != nil {
		t.Fatal(err)
	}
	got, err := RunReset(root, State)
	if err != nil {
		t.Fatalf("RunReset: %v", err)
	}
	if len(got.Removed) != 1 || got.Removed[0] != filepath.Join(root, ".crw", "sessions", "a.json") {
		t.Fatalf("removed = %v, want the dot-ending link", got.Removed)
	}
	if _, err := os.Lstat(filepath.Join(crw, "sessions", "a.json")); !os.IsNotExist(err) {
		t.Errorf("link remains: %v", err)
	}
	if _, err := os.Stat(filepath.Join(crw, "sessions", "keep", "inner.txt")); err != nil {
		t.Errorf("target directory lost: %v", err)
	}
}
