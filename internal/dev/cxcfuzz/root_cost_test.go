//go:build dev

package cxcfuzz

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// A case root made twice over, or made over a tree that already holds some of its homes and files, is the same root
// afterwards, and what was in it stays (CRW-1066: PrepareRoot judges the root once and makes the homes as plain
// directories, so what it leaves behind must not change).
func TestPrepareRootAcceptsAnExistingTreeAndKeepsItsFiles(t *testing.T) {
	root := t.TempDir()
	env := RootEnv(root)
	if err := os.MkdirAll(env.CrwHome, 0o755); err != nil {
		t.Fatal(err)
	}
	keep := filepath.Join(env.CrwHome, "keep.txt")
	if err := os.WriteFile(keep, []byte("kept"), 0o644); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := PrepareRoot(root); err != nil {
			t.Fatalf("PrepareRoot #%d: %v", i, err)
		}
	}
	if raw, err := os.ReadFile(keep); err != nil || string(raw) != "kept" {
		t.Fatalf("keep.txt: %q, %v", raw, err)
	}
	for _, dir := range []string{env.Home, env.CodexHome, env.CrwHome, env.TmpDir, filepath.Join(root, "codexclaw-home")} {
		if info, err := os.Lstat(dir); err != nil || !info.IsDir() {
			t.Errorf("%s: %v %v", dir, info, err)
		}
	}
}

// A home that is a plain file in the way is an error and no directory takes its place, as when MkdirAll made it.
func TestPrepareRootRefusesAHomeThatIsAFile(t *testing.T) {
	root := t.TempDir()
	env := RootEnv(root)
	if err := os.WriteFile(env.Home, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := PrepareRoot(root); err == nil {
		t.Fatal("PrepareRoot made a home over a file")
	}
	if info, err := os.Lstat(env.Home); err != nil || info.IsDir() {
		t.Fatalf("the file was replaced: %v %v", info, err)
	}
}

// A case root that holds a directory nothing can list, with a link inside it that leaves the root, is removed whole
// and the link's target is not touched (CRW-1066: the removal tries the plain way first and only then makes the
// directories listable).
func TestRemoveCaseRootRemovesAnUnlistableDirectoryAndLeavesALinksTarget(t *testing.T) {
	outside := t.TempDir()
	target := filepath.Join(outside, "target.txt")
	if err := os.WriteFile(target, []byte("outside"), 0o644); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(t.TempDir(), "case")
	if err := PrepareRoot(root); err != nil {
		t.Fatal(err)
	}
	sealed := filepath.Join(root, "home", "sealed")
	if err := os.MkdirAll(filepath.Join(sealed, "inner"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(sealed, "inner", "escape")); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(sealed, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(sealed, 0o755) })
	if err := RemoveCaseRoot(root); err != nil {
		t.Fatalf("RemoveCaseRoot: %v", err)
	}
	if _, err := os.Lstat(root); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the root survived: %v", err)
	}
	if raw, err := os.ReadFile(target); err != nil || string(raw) != "outside" {
		t.Fatalf("the link's target: %q, %v", raw, err)
	}
}
