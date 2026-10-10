package source

import (
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// CRW-1135: the capture runs no fsmonitor program of the repository's (hooks are at the null device too; clean and
// process filters are not disabled and stay out of this test), the probe policy reaching git through the environment
// so its argument list stays the oracle's, and no inherited routing variable reaches it.
func TestCaptureRunsNoFsmonitorAndIgnoresInheritedRouting(t *testing.T) {
	base := hermetic(t)
	root := newRepo(t, base)
	marker := filepath.Join(base, "fsmonitor-ran")
	hook := filepath.Join(base, "fsmonitor.sh")
	must(t, os.WriteFile(hook, []byte("#!/bin/sh\ntouch '"+marker+"'\nexit 1\n"), 0o755))
	gitIn(t, root, "config", "core.fsmonitor", hook)
	plain := exec.Command("git", "status", "--porcelain")
	plain.Dir = root
	_ = plain.Run()
	if _, err := os.Stat(marker); err != nil {
		t.Skipf("this git runs no fsmonitor hook for status: %v", err)
	}
	must(t, os.Remove(marker))
	clean := Capture(root, Options{})
	t.Setenv("GIT_OBJECT_DIRECTORY", filepath.Join(base, "nonexistent-objects"))
	t.Setenv("GIT_ALTERNATE_OBJECT_DIRECTORIES", filepath.Join(base, "nonexistent-alternates"))
	t.Setenv("GIT_NAMESPACE", "elsewhere")
	t.Setenv("GIT_CONFIG_PARAMETERS", "'core.fsmonitor'='"+hook+"'")
	inherited := Capture(root, Options{})
	if clean.Kind != KindResolved || clean.CommitSha == "" || inherited.Kind != clean.Kind || inherited.CommitSha != clean.CommitSha || inherited.TreeHash != clean.TreeHash {
		t.Fatalf("clean %+v, inherited %+v", clean, inherited)
	}
	if _, err := os.Stat(marker); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the capture ran the repository's fsmonitor hook: %v", err)
	}
}
