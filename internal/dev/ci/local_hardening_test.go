//go:build dev

package ci

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// CRW-964 pre-merge finding d1: a clean worktree whose files a smudge and clean filter changed is not the
// commit, even though git status reports it clean. The record fails rather than naming the commit.
func TestLocal_a_filter_changed_checkout_is_refused(t *testing.T) {
	repo := newLocalFixture(t)
	runGit(repo.root, "config", "filter.up.smudge", "tr a-z A-Z")
	runGit(repo.root, "config", "filter.up.clean", "tr A-Z a-z")
	repo.write(".gitattributes", "file.txt filter=up\n")
	repo.commit()
	record := filepath.Join(t.TempDir(), "record.json")
	made, _, err := localVerify(localRunOptions(repo, localFixturePlan("cat file.txt"), record), "", io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if made.Result != localFail {
		t.Errorf("the record passes a checkout that a filter changed (result %q)", made.Result)
	}
}

// CRW-964 pre-merge finding d2: the version probes receive the same runtime and bus variables the steps do,
// so a probe run under a user systemd gate reads the same tool as the step.
func TestLocalTools_the_probe_env_keeps_the_runtime_and_bus_variables(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", "/run/user/crw-test")
	t.Setenv("DBUS_SESSION_BUS_ADDRESS", "unix:path=/run/user/crw-test/bus")
	env := localProbeEnv(t.TempDir(), "/usr/bin")
	for _, want := range []string{"XDG_RUNTIME_DIR=/run/user/crw-test", "DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/crw-test/bus"} {
		if !localContains(env, want) {
			t.Errorf("the probe environment lacks %s: %s", want, strings.Join(env, " "))
		}
	}
}

// CRW-964 pre-merge finding d4: the pre-push hook path is never replaced when it is a symlink, even a dangling
// one another installer owns; the install refuses and leaves the link as it is.
func TestLocalHook_a_dangling_symlink_is_never_replaced(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "pre-push")
	target := filepath.Join(dir, "elsewhere-missing")
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	if _, err := localHookInstall(path); err == nil {
		t.Error("the install replaced a dangling symlink")
	}
	if info, err := os.Lstat(path); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Errorf("the symlink at the hook path is gone (%v)", err)
	}
}
