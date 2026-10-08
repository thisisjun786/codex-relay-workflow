//go:build dev

package ci

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// CRW-964: the pre-push hook, in a temporary repository. The hook is installed only here and in
// the operator's own checkout, never by a test into a shared one.

// localHookFixture is a repository with one commit on main and a branch to push.
type localHookFixture struct {
	*fixtureRepo
	base string
}

// localHookScanner is a stand-in Gitleaks: it fails when the range carries the marker, so the
// test proves the hook's decision rather than Gitleaks' rules.
const localHookScanner = `#!/bin/sh
if [ "$1" = version ]; then
  echo "${CRW964_SCANNER_VERSION:-8.30.1}"
  exit 0
fi
if [ "$1" != git ]; then
  echo "stand-in scanner: unexpected command $1" >&2
  exit 2
fi
for arg in "$@"; do
  case $arg in --log-opts=*) opts=${arg#--log-opts=} ;; esac
done
[ -n "$opts" ] || { echo 'stand-in scanner: no --log-opts' >&2; exit 2; }
if git -C "$2" log -p -U0 $opts | grep -q "$CRW964_MARKER"; then
  echo "stand-in scanner: a leak"
  exit 1
fi
exit 0`

func newLocalHookFixture(t *testing.T) *localHookFixture {
	t.Helper()
	repo := &localHookFixture{fixtureRepo: newRepo(t)}
	repo.write("file.txt", "one\n")
	repo.commit()
	base := strings.TrimSpace(repo.git("rev-parse", "HEAD"))
	return &localHookFixture{fixtureRepo: repo.fixtureRepo, base: base}
}

// localHookScannerPath is the stand-in scanner a test uses.
func localHookScannerPath(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "gitleaks")
	if err := os.WriteFile(path, []byte(localHookScanner), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// comment-c7: a push whose range carries a secret pattern is blocked, and the block names the
// range. Red first: with no hook installed the push is allowed.
func TestLocalHook_blocks_a_push_carrying_a_secret(t *testing.T) {
	repo := newLocalHookFixture(t)
	// The commit under judgment carries the marker the scanner fails on.
	repo.write("secret.txt", "token = CRW964-LEAKED-VALUE\n")
	repo.commit()
	head := strings.TrimSpace(repo.git("rev-parse", "HEAD"))
	scanner := localHookScannerPath(t)
	env := []string{"CRW_CI_GITLEAKS=" + scanner, "CRW964_MARKER=CRW964-LEAKED-VALUE"}

	// No hook yet: nothing blocks the push.
	if state := localHookState(mustHookPath(t, repo.root)); state != "absent" {
		t.Fatalf("the hook starts %q, want absent", state)
	}
	if code, out := localHookRun(repo.root, "refs/heads/main", head, "refs/heads/main", repo.base, env); code == 0 {
		t.Fatalf("a push carrying the marker was allowed before the hook was installed:\n%s", out)
	}

	// Install the hook, then push the same range: it is refused.
	path := mustHookPath(t, repo.root)
	if _, err := localHookInstall(path); err != nil {
		t.Fatal(err)
	}
	if state := localHookState(path); state != "installed" {
		t.Fatalf("the hook is %q after install, want installed", state)
	}
	code, out := localHookRun(repo.root, "refs/heads/main", head, "refs/heads/main", repo.base, env)
	if code == 0 {
		t.Fatalf("the hook allowed a push carrying the marker:\n%s", out)
	}
	if !strings.Contains(out, "Gitleaks found a secret") {
		t.Errorf("the refusal does not name the finding:\n%s", out)
	}
	if !strings.Contains(out, repo.base+".."+head) {
		t.Errorf("the refusal does not name the range:\n%s", out)
	}
}

// A range without the marker passes, so the hook blocks the finding and not every push.
func TestLocalHook_allows_a_clean_push(t *testing.T) {
	repo := newLocalHookFixture(t)
	repo.write("clean.txt", "nothing to see\n")
	repo.commit()
	head := strings.TrimSpace(repo.git("rev-parse", "HEAD"))
	if _, err := localHookInstall(mustHookPath(t, repo.root)); err != nil {
		t.Fatal(err)
	}
	code, out := localHookRun(repo.root, "refs/heads/main", head, "refs/heads/main", repo.base,
		[]string{"CRW_CI_GITLEAKS=" + localHookScannerPath(t), "CRW964_MARKER=CRW964-LEAKED-VALUE"})
	if code != 0 {
		t.Fatalf("a clean push was blocked:\n%s", out)
	}
}

// A missing scanner blocks the push rather than scanning nothing.
func TestLocalHook_refuses_when_the_scanner_is_missing(t *testing.T) {
	repo := newLocalHookFixture(t)
	repo.write("clean.txt", "nothing to see\n")
	repo.commit()
	head := strings.TrimSpace(repo.git("rev-parse", "HEAD"))
	if _, err := localHookInstall(mustHookPath(t, repo.root)); err != nil {
		t.Fatal(err)
	}
	code, out := localHookRun(repo.root, "refs/heads/main", head, "refs/heads/main", repo.base,
		[]string{"CRW_CI_GITLEAKS=" + filepath.Join(t.TempDir(), "no-such-scanner")})
	if code == 0 {
		t.Fatalf("a push went through with no scanner:\n%s", out)
	}
	if !strings.Contains(out, "not on PATH") {
		t.Errorf("the refusal does not name the missing scanner:\n%s", out)
	}
}

// A blob over the limit in the pushed range blocks the push.
func TestLocalHook_blocks_a_push_carrying_a_large_blob(t *testing.T) {
	repo := newLocalHookFixture(t)
	repo.write("big.bin", strings.Repeat("x", (2<<20)+1))
	repo.commit()
	head := strings.TrimSpace(repo.git("rev-parse", "HEAD"))
	if _, err := localHookInstall(mustHookPath(t, repo.root)); err != nil {
		t.Fatal(err)
	}
	code, out := localHookRun(repo.root, "refs/heads/main", head, "refs/heads/main", repo.base,
		[]string{"CRW_CI_GITLEAKS=" + localHookScannerPath(t)})
	if code == 0 {
		t.Fatalf("a push carrying a large blob went through:\n%s", out)
	}
	if !strings.Contains(out, "big.bin") {
		t.Errorf("the refusal does not name the file:\n%s", out)
	}
}

// supplement 2: an existing hook this tool did not write is left untouched and the install
// refuses.
func TestLocalHook_refuses_a_foreign_hook(t *testing.T) {
	repo := newLocalHookFixture(t)
	path := mustHookPath(t, repo.root)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	foreign := "#!/bin/sh\necho someone else's hook\n"
	if err := os.WriteFile(path, []byte(foreign), 0o755); err != nil {
		t.Fatal(err)
	}
	if state := localHookState(path); state != "foreign" {
		t.Fatalf("the existing hook reads %q, want foreign", state)
	}
	if _, err := localHookInstall(path); err == nil {
		t.Error("installing over a foreign hook succeeded")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != foreign {
		t.Errorf("the foreign hook was changed:\n%s", data)
	}
}

// Installing twice is idempotent: this tool's own hook is rewritten.
func TestLocalHook_install_is_idempotent(t *testing.T) {
	repo := newLocalHookFixture(t)
	path := mustHookPath(t, repo.root)
	for range 2 {
		if _, err := localHookInstall(path); err != nil {
			t.Fatal(err)
		}
	}
	if state := localHookState(path); state != "installed" {
		t.Errorf("the hook is %q, want installed", state)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o111 == 0 {
		t.Errorf("the hook is not executable: %v", info.Mode())
	}
}

// The hook never writes git config.
func TestLocalHook_writes_no_git_config(t *testing.T) {
	repo := newLocalHookFixture(t)
	before := repo.git("config", "--local", "--list")
	if _, err := localHookInstall(mustHookPath(t, repo.root)); err != nil {
		t.Fatal(err)
	}
	if after := repo.git("config", "--local", "--list"); after != before {
		t.Errorf("the install changed the repository's git config:\nbefore:\n%s\nafter:\n%s", before, after)
	}
}

// mustHookPath is localHookPath, failing the test when it cannot resolve.
func mustHookPath(t *testing.T, root string) string {
	t.Helper()
	path, err := localHookPath(root)
	if err != nil {
		t.Fatal(err)
	}
	return path
}

// The hook requires the pinned Gitleaks release: a scanner of another version is refused rather
// than approving a range with different rules.
func TestLocalHook_refuses_a_scanner_of_another_version(t *testing.T) {
	repo := newLocalHookFixture(t)
	repo.write("clean.txt", "nothing to see\n")
	repo.commit()
	head := strings.TrimSpace(repo.git("rev-parse", "HEAD"))
	if _, err := localHookInstall(mustHookPath(t, repo.root)); err != nil {
		t.Fatal(err)
	}
	code, out := localHookRun(repo.root, "refs/heads/main", head, "refs/heads/main", repo.base,
		[]string{"CRW_CI_GITLEAKS=" + localHookScannerPath(t), "CRW964_SCANNER_VERSION=8.18.0"})
	if code == 0 {
		t.Fatalf("a push went through with an unpinned scanner:\n%s", out)
	}
	if !strings.Contains(out, "8.18.0") || !strings.Contains(out, localGitleaksPin) {
		t.Errorf("the refusal does not name both versions:\n%s", out)
	}
}

// An older hook this tool wrote is recognised so an upgrade replaces it, while a hook it did not
// write stays foreign.
func TestLocalHook_recognises_its_own_older_hook(t *testing.T) {
	repo := newLocalHookFixture(t)
	path := mustHookPath(t, repo.root)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	older := "#!/usr/bin/env bash\n" + localHookMarker + "\n# an older body this tool wrote\nexit 0\n"
	if err := os.WriteFile(path, []byte(older), 0o755); err != nil {
		t.Fatal(err)
	}
	if state := localHookState(path); state != "installed" {
		t.Fatalf("this tool's own older hook reads %q, want installed", state)
	}
	if _, err := localHookInstall(path); err != nil {
		t.Fatalf("an upgrade of this tool's own older hook was refused: %v", err)
	}
	// A file that merely mentions the marker is still foreign.
	impostor := "#!/usr/bin/env bash\necho " + localHookMarker + "\nexit 0\n"
	if err := os.WriteFile(path, []byte(impostor), 0o755); err != nil {
		t.Fatal(err)
	}
	if state := localHookState(path); state != "foreign" {
		t.Errorf("a hook with the marker in the wrong place reads %q, want foreign", state)
	}
	if _, err := localHookInstall(path); err == nil {
		t.Error("a foreign hook was replaced")
	}
}

// A SHA-256 all-zero object id (a 64-character sentinel) is recognised as a new ref, so the whole
// local sha is judged rather than an invalid range.
func TestLocalHook_recognises_a_sha256_zero_remote(t *testing.T) {
	repo := newLocalHookFixture(t)
	repo.write("secret.txt", "token = CRW964-LEAKED-VALUE\n")
	repo.commit()
	head := strings.TrimSpace(repo.git("rev-parse", "HEAD"))
	if _, err := localHookInstall(mustHookPath(t, repo.root)); err != nil {
		t.Fatal(err)
	}
	zeros := strings.Repeat("0", 64)
	code, out := localHookRun(repo.root, "refs/heads/main", head, "refs/heads/main", zeros,
		[]string{"CRW_CI_GITLEAKS=" + localHookScannerPath(t), "CRW964_MARKER=CRW964-LEAKED-VALUE"})
	if code == 0 {
		t.Fatalf("a new-ref push carrying the marker went through:\n%s", out)
	}
	if !strings.Contains(out, "Gitleaks found a secret") {
		t.Errorf("the refusal does not name the finding:\n%s", out)
	}
}
