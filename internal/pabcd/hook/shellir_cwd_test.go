package hook

import (
	"os"
	"path/filepath"
	"testing"
)

// TestShellirPayloadCwdKnownOnlyWhenAbsoluteDirectory: the payload cwd is a known directory only when it is an absolute path
// to an existing directory; an empty, relative or missing cwd is unknown.
func TestShellirPayloadCwdKnownOnlyWhenAbsoluteDirectory(t *testing.T) {
	dir := t.TempDir()
	if got := shellirPayloadCwd(dir); got != dir {
		t.Errorf("an existing absolute directory: got %q, want it back", got)
	}
	for _, cwd := range []string{"", "rel", "./rel", filepath.Join(dir, "missing")} {
		if got := shellirPayloadCwd(cwd); got != "" {
			t.Errorf("cwd %q: got %q, want unknown", cwd, got)
		}
	}
}

// TestMemoryGateRelativeCwdLeavesDestinationUnknown: with a relative payload cwd a relative redirection target is an
// unknown destination, never the path under the hook's process directory.
func TestMemoryGateRelativeCwdLeavesDestinationUnknown(t *testing.T) {
	_, _, env := gateScene(t)
	got := memoryGateClassify("Bash", map[string]any{"command": "echo x > out.txt"}, "rel", env)
	if got.Surface != "shell" || got.Target != "(a destination the gate cannot read)" {
		t.Errorf("relative cwd: %+v, want an unknown shell destination", got)
	}
}

// TestWorktreeGuardRelativeCwdLeavesRemovalUnknown: in a managed worktree, a removal whose directory the payload does not
// make known is denied; the relative target is not resolved against the wrong directory.
func TestWorktreeGuardRelativeCwdLeavesRemovalUnknown(t *testing.T) {
	r := newDelRig(t)
	if v := evaluateCommand("rm -rf ./x", "relative", r.id()); !v.Deny {
		t.Errorf("a removal under an unknown directory was allowed")
	}
	if v := evaluateCommand("rm -rf ./x", r.checkout, r.id()); v.Deny {
		t.Errorf("a removal of a child of the known checkout was denied: %s", v.Reason)
	}
}

// TestGitHubPostRelativeBodyNeedsKnownDir: a body file named relative to an unknown directory is not read from the process
// directory.
func TestGitHubPostRelativeBodyNeedsKnownDir(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	if err := os.WriteFile(filepath.Join(dir, "body.md"), []byte("text"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok := githubPostReadFile("body.md", ""); ok {
		t.Errorf("a relative body file was read from the process directory")
	}
	if text, ok := githubPostReadFile("body.md", dir); !ok || text != "text" {
		t.Errorf("the same body file under a known directory: %q, %v", text, ok)
	}
}
