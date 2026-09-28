package testsupport_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
)

func TestRemoveTempTreeReadOnlyCacheWithoutFollowingSymlinks(t *testing.T) {
	root := filepath.Join(t.TempDir(), "owned")
	module := filepath.Join(root, "go", "pkg", "mod", "module@v1")
	if err := os.MkdirAll(module, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(module, "go.mod"), []byte("module example.invalid/test\n"), 0400); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "untouched")
	if err := os.WriteFile(outside, []byte("keep"), 0400); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(module, "outside")); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(module, 0500); err != nil {
		t.Fatal(err)
	}
	if err := testsupport.RemoveTempTree(root); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatalf("owned tree remains: %v", err)
	}
	info, err := os.Stat(outside)
	if err != nil || info.Mode().Perm() != 0400 {
		t.Fatalf("external symlink target changed: %v %v", info, err)
	}
	if err := testsupport.RemoveTempTree(root); err != nil {
		t.Fatalf("already removed tree: %v", err)
	}
}
