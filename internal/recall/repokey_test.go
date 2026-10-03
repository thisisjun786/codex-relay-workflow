package recall

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// These seven concerns port recall/test/repo-key.test.ts:13-80. Go's string domain maps null to empty.
func TestRepoKeySpellingsAndIdentity(t *testing.T) {
	key := "github.com/lidge-jun/codexclaw"
	for _, raw := range []string{"https://github.com/lidge-jun/codexclaw.git", "https://github.com/lidge-jun/codexclaw", "https://github.com/lidge-jun/codexclaw/", "git@github.com:lidge-jun/codexclaw.git", "ssh://git@github.com/lidge-jun/codexclaw", "ssh://git@github.com:22/lidge-jun/codexclaw.git", "git://github.com/lidge-jun/codexclaw.git", "  https://github.com/lidge-jun/codexclaw.git  ", "https://GitHub.COM/lidge-jun/codexclaw.git"} {
		if got := normalizeRepoKey(raw); got != key {
			t.Errorf("%q: %q, want %q", raw, got, key)
		}
	}
	for _, raw := range []string{"https://github.com/Lidge-Jun/CodexClaw.git", "git@github.com:bitkyc08-arch/codexclaw.git", "https://gitlab.com/lidge-jun/codexclaw.git"} {
		if got := normalizeRepoKey(raw); got == "" || repoKeysEqual(got, key) {
			t.Errorf("distinct remote %q collapsed", raw)
		}
	}
	for _, raw := range []string{"", "  ", "not a url", "/local/repo", "https://github.com/"} {
		if got := normalizeRepoKey(raw); got != "" {
			t.Errorf("%q: unexpected partial key %q", raw, got)
		}
	}
	if repoKeysEqual("", "") || repoKeysEqual(key, "") || !repoKeysEqual(key, key) {
		t.Fatal("key equality contract")
	}
	for _, raw := range []string{"git@github.com:lidge-jun/codexclaw.git", "", "not a url"} {
		got := repoKeyForCwd("/synthetic", func(cwd string) string {
			if cwd != "/synthetic" {
				t.Fatal(cwd)
			}
			return raw
		})
		if got != normalizeRepoKey(raw) {
			t.Errorf("injected origin %q: %q", raw, got)
		}
	}
}

func TestRepoKeyBaseOracle(t *testing.T) {
	b, err := os.ReadFile("testdata/repokey/oracle-repokey-base.json")
	if err != nil {
		t.Fatal(err)
	}
	var rows []struct {
		Raw *string
		Key *string
	}
	if err := json.Unmarshal(b, &rows); err != nil {
		t.Fatal(err)
	}
	if len(rows) != 31 {
		t.Fatalf("got %d base rows, want31", len(rows))
	}
	for _, row := range rows {
		raw, want := "", ""
		if row.Raw != nil {
			raw = *row.Raw
		}
		if row.Key != nil {
			want = *row.Key
		}
		if got := normalizeRepoKey(raw); got != want {
			t.Errorf("%q: got %q, Node %q", raw, got, want)
		}
	}
}

func TestRepoKeyOriginInTemporaryGit(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", root)
	t.Setenv("CODEX_HOME", root)
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CEILING_DIRECTORIES", filepath.Dir(root))
	for _, k := range []string{"GIT_DIR", "GIT_WORK_TREE", "GIT_COMMON_DIR", "GIT_INDEX_FILE", "GIT_OBJECT_DIRECTORY", "GIT_ALTERNATE_OBJECT_DIRECTORIES", "GIT_CONFIG_COUNT"} {
		t.Setenv(k, "")
	}
	// Unset routing variables in the child; the reader deliberately inherits the oracle's environment.
	for _, k := range []string{"GIT_DIR", "GIT_WORK_TREE", "GIT_COMMON_DIR", "GIT_INDEX_FILE", "GIT_OBJECT_DIRECTORY", "GIT_ALTERNATE_OBJECT_DIRECTORIES", "GIT_CONFIG_COUNT"} {
		if err := os.Unsetenv(k); err != nil {
			t.Fatal(err)
		}
	}
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", root}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git%v: %v %s", args, err, out)
		}
	}
	if readOriginUrl(root) != "" || readOriginUrl("") != "" || readOriginUrl(filepath.Join(root, "absent")) != "" {
		t.Fatal("nonrepo/missing/empty must fail soft")
	}
	git("init", "--quiet")
	if readOriginUrl(root) != "" {
		t.Fatal("repository without origin")
	}
	git("config", "remote.origin.url", "  git@HOST:Owner/Repo.GIT  ")
	if got := readOriginUrl(root); got != "git@HOST:Owner/Repo.GIT" {
		t.Fatal(got)
	}
	if got := repoKeyForCwd(root); got != "host/Owner/Repo" {
		t.Fatal(got)
	}
	if got := repoKeyForCwd(root, nil); got != "host/Owner/Repo" {
		t.Fatal(got)
	}
	// Multiple config values are output on separate lines and cannot parse as an scp remote.
	git("config", "--add", "remote.origin.url", "git@host:Other/Repo")
	if got := readOriginUrl(root); !strings.Contains(got, "\n") {
		t.Fatal(got)
	}
}
