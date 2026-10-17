package worker

import (
	"github.com/thisisjun786/codex-relay-workflow/internal/executor"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestToolchainPinnedOwnedVersionsAndReadback(t *testing.T) {
	root := filepath.Join(t.TempDir(), "worker")
	source := filepath.Join(t.TempDir(), "tool")
	if err := os.WriteFile(source, []byte("synthetic tool"), 0600); err != nil {
		t.Fatal(err)
	}
	m := Toolchain{OS: runtime.GOOS, Arch: runtime.GOARCH, Files: []ToolFile{{Path: "bin/tool", Version: "1", SHA256: executor.Digest([]byte("synthetic tool")), Source: source, Executable: true}}}
	receipt, err := InstallToolchain(root, m)
	if err != nil {
		t.Fatal(err)
	}
	if receipt.Digest != m.Digest() {
		t.Fatal("wrong readback")
	}
	if _, err := InstallToolchain(root, m); err != nil {
		t.Fatal("idempotent install:", err)
	}
	path := filepath.Join(root, "versions", m.Digest(), "bin", "tool")
	if err := os.WriteFile(path, []byte("drift"), 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := InstallToolchain(root, m); err == nil {
		t.Fatal("overwrote a changed installed version")
	}
}
func TestToolchainRejectsSymlinkAndTraversal(t *testing.T) {
	root := filepath.Join(t.TempDir(), "worker")
	m := Toolchain{OS: runtime.GOOS, Arch: runtime.GOARCH, Files: []ToolFile{{Path: "../escape", Version: "1", SHA256: executor.Digest(nil), Source: "/missing"}}}
	if _, err := InstallToolchain(root, m); err == nil {
		t.Fatal("traversal")
	}
	target := t.TempDir()
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	m.Files[0].Path = "bin/tool"
	if _, err := InstallToolchain(link, m); err == nil {
		t.Fatal("foreign symlink root")
	}
}
