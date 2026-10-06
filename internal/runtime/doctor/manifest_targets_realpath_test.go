package doctor

import (
	"os"
	"path/filepath"
	"testing"
)

// TestManifestTargetsRealpathFallback pins the two failure paths of the walk the containment judgement
// depends on, which are the ones that send BOTH paths to the oracle's lexical branch: a component that
// cannot be read, and a link chain past the depth a realpath follows. Neither may answer a partial path,
// because targetEscapesRoot reads the pair.
func TestManifestTargetsRealpathFallback(t *testing.T) {
	base := t.TempDir()
	if err := os.MkdirAll(filepath.Join(base, "real"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := manifestTargetsRealpath(filepath.Join(base, "real", "absent")); err == nil {
		t.Fatalf("a missing leaf answered a path instead of an error")
	}
	if _, err := manifestTargetsRealpath(filepath.Join(base, "absent", "deeper")); err == nil {
		t.Fatalf("a missing component answered a path instead of an error")
	}
	// A chain longer than the depth a realpath follows is an error, not a resolved path.
	previous := filepath.Join(base, "loop-0")
	if err := os.Symlink("loop-0", previous); err != nil {
		t.Fatal(err)
	}
	if _, err := manifestTargetsRealpath(previous); err == nil {
		t.Fatalf("a self-referential link answered a path instead of an error")
	}
	// The paired fallback then compares the original strings, so a root reached through an unresolvable
	// component is not reported as escaped when the target is inside it lexically.
	root := filepath.Join(base, "real")
	if targetEscapesRoot(root, filepath.Join(root, "leaf.json")) {
		t.Fatalf("a target inside a resolvable root was reported as escaped")
	}
	if !targetEscapesRoot(root, filepath.Join(base, "elsewhere", "leaf.json")) {
		t.Fatalf("a target outside the root was not reported as escaped")
	}
}

// TestManifestTargetsRealpathSymlinkStillEscapes pins that following a link by restarting at its target
// does not lose the containment property: a link out of the root still answers an escape.
func TestManifestTargetsRealpathSymlinkStillEscapes(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "root")
	outside := filepath.Join(base, "outside")
	for _, dir := range []string{root, outside} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(outside, "leaf.js"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	if !targetEscapesRoot(root, filepath.Join(root, "link", "leaf.js")) {
		t.Fatalf("a target reached through a link out of the root was not reported as escaped")
	}
}
