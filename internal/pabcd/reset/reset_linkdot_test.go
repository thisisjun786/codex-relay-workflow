package reset

import (
	"os"
	"path/filepath"
	"testing"
)

// resetLinkDotWorkspace makes a directory holding a "keep" directory (with one file) and a symlink
// named "link" pointing at target, and opens it as an os.Root. The root is closed when the test ends.
func resetLinkDotWorkspace(t *testing.T, target string, keepMode os.FileMode) (*os.Root, string) {
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
	t.Cleanup(func() { _ = root.Close() })
	return root, dir
}

// TestResetLinkDotTargetSkipsTheRootStat: a link whose Readlink target ends in a "." or ".."
// component is judged on the root's own path without opening the target through the pinned
// descriptor, which os.Root.Stat would do with O_DIRECTORY (CRW-554's requirement that the target is
// not opened); every other target keeps the descriptor path. Verdicts match the descriptor path's.
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
		{"plain_keep_keeps_the_descriptor_path", "keep", 0, true, 1},
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
