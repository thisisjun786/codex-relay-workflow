//go:build dev

package cxcfuzz

import (
	"os"
	"path/filepath"
	"testing"
)

// A requirement given as an explicit path is judged as the worker would reach it: the file must exist, be a
// regular file and be executable. A missing path, a directory and a file without the execute bit are refused
// rather than accepted by name (CRW-978 c3a).
func TestLookPathInStatsAnExplicitPath(t *testing.T) {
	env := []string{"PATH=" + t.TempDir()}
	if _, err := lookPathIn(env, filepath.Join(t.TempDir(), "missing-tool")); err == nil {
		t.Fatal("a missing explicit path was accepted")
	}
	if _, err := lookPathIn(env, t.TempDir()); err == nil {
		t.Fatal("a directory was accepted as an explicit requirement")
	}
	plain := filepath.Join(t.TempDir(), "plain")
	if err := os.WriteFile(plain, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := lookPathIn(env, plain); err == nil {
		t.Fatal("a file without the execute bit was accepted")
	}
	tool := filepath.Join(t.TempDir(), "tool")
	if err := os.WriteFile(tool, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got, err := lookPathIn(env, tool); err != nil || got != tool {
		t.Fatalf("an executable explicit path resolved to %q (%v), want %q", got, err, tool)
	}
}
