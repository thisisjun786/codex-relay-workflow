//go:build dev

package ci

import (
	"os/exec"
	"strings"
	"testing"
)

// gitConfigMaybe reads key from the fixture repository's config and reports whether it is set. A
// missing key is not a failure here, so one run reports every key the fixture must set instead of
// stopping at the first.
func gitConfigMaybe(t *testing.T, r *fixtureRepo, key string) (string, bool) {
	t.Helper()
	cmd := exec.Command("git", "config", "--get", key)
	cmd.Dir = r.root
	out, err := cmd.Output()
	if err != nil {
		return "", false
	}
	return strings.TrimSpace(string(out)), true
}

// newRepo must turn git's automatic maintenance off. After a commit git otherwise starts a detached
// `git maintenance run --auto` (or `gc --auto`), which creates and removes files under .git while
// t.TempDir removes the tree and fails as ".git: directory not empty". The large-blob tests commit
// blobs over 2 MiB several times and hit it most; the same cause was fixed in internal/dev/cxccorpus.
func TestNewRepo_disables_git_automatic_maintenance(t *testing.T) {
	r := newRepo(t)
	for _, tc := range []struct{ key, want string }{
		{"maintenance.auto", "false"},
		{"gc.auto", "0"},
	} {
		got, ok := gitConfigMaybe(t, r, tc.key)
		if !ok {
			t.Errorf("%s is unset in the fixture repo, want %q", tc.key, tc.want)
			continue
		}
		if got != tc.want {
			t.Errorf("%s = %q, want %q", tc.key, got, tc.want)
		}
	}
}
