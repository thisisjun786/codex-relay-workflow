package ops

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// CRW-1508 item 3: a gitdir target that cannot be stat'ed (a directory on its path the account may not search) is reported by its
// own error, not as "is not a directory": the error names the permission failure, and errors.Is finds it. The same holds for a
// .git symbolic link into that directory.
func TestCIGitDir_a_target_that_cannot_be_stated_is_reported_by_its_error(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root searches every directory: no permission error to report")
	}
	base := t.TempDir()
	locked := filepath.Join(base, "locked")
	if err := os.MkdirAll(filepath.Join(locked, "g"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(locked, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })
	target := filepath.Join(locked, "g")

	file := filepath.Join(base, "file")
	if err := os.MkdirAll(file, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(file, ".git"), []byte("gitdir: "+target+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "link")
	if err := os.MkdirAll(link, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(link, ".git")); err != nil {
		t.Fatal(err)
	}
	for name, wt := range map[string]string{"a .git file": file, "a .git link": link} {
		_, err := ciGitDir(wt)
		if err == nil {
			t.Errorf("%s: a target that cannot be stat'ed was accepted", name)
			continue
		}
		if strings.Contains(err.Error(), "is not a directory") || !errors.Is(err, fs.ErrPermission) || !strings.Contains(err.Error(), "permission denied") {
			t.Errorf("%s: %v, want the permission error by name", name, err)
		}
		if !strings.Contains(err.Error(), wt+" has no git directory") || name == "a .git file" && !strings.Contains(err.Error(), target) {
			t.Errorf("%s: %v does not name the worktree and the target", name, err)
		}
	}
	// a target that is there and is a file is still "is not a directory"
	plain := filepath.Join(base, "plain")
	if err := os.MkdirAll(plain, 0o755); err != nil {
		t.Fatal(err)
	}
	notDir := filepath.Join(base, "not-a-dir")
	if err := os.WriteFile(notDir, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(plain, ".git"), []byte("gitdir: "+notDir+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ciGitDir(plain); err == nil || !strings.Contains(err.Error(), "is not a directory") {
		t.Errorf("a file target: %v, want is not a directory", err)
	}
}
