//go:build dev

package cxcfuzz

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// CRW-958 fix round 3: a scratch_root spelled through a symbolic link and ".." means the directory the
// filesystem resolves it to. The default output directory keeps that spelling at its last join, and the
// files a campaign writes below it land on that disk.
func TestDefaultOutKeepsTheRawSpellingOfTheScratchRoot(t *testing.T) {
	base := t.TempDir()
	for _, dir := range []string{"a", "b/deep"} {
		if err := os.MkdirAll(filepath.Join(base, dir), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(filepath.Join(base, "b", "deep"), filepath.Join(base, "a", "link")); err != nil {
		t.Fatal(err)
	}
	spelled, real, cleaned := base+"/a/link/../root", filepath.Join(base, "b", "root"), filepath.Join(base, "a", "root")
	config := filepath.Join(base, "config.json")
	if err := os.WriteFile(config, []byte(`{"paths":{"scratch_root":"`+spelled+`"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	env := func(key string) string {
		switch key {
		case "CRW_CONFIG":
			return config
		case "HOME":
			return base
		}
		return ""
	}
	out, err := DefaultOut(env, "state", time.Date(2026, 10, 11, 1, 2, 3, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if want := spelled + "/fuzz/state/20261011T010203Z"; out != want {
		t.Fatalf("DefaultOut = %q, want %q", out, want)
	}
	if err := os.MkdirAll(real+"/fuzz/state/20261011T010203Z", 0o700); err != nil {
		t.Fatal(err)
	}
	if err := writeSummary(out, Summary{}); err != nil {
		t.Fatal(err)
	}
	if err := writeDivergence(out, Divergence{Kind: "reply", Input: "x"}, map[string]bool{}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := writeFailure(out, Failure{Input: "x"}, map[string]bool{}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(real + "/fuzz/state/20261011T010203Z/summary.json"); err != nil {
		t.Errorf("summary.json is not on the real disk: %v", err)
	}
	if _, err := os.Stat(cleaned); err == nil {
		t.Errorf("the lexically cleaned directory %s was made", cleaned)
	}
}
