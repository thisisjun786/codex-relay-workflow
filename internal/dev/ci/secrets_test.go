//go:build dev

package ci

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"testing"
)

// scripts/ci/secrets.sh decides which commits the pinned Gitleaks scans. These tests run the
// script as CI does, in a temporary repository, with the download replaced by stand-ins ahead of
// it on PATH: a curl that hands out an archive built here, a sha256sum that accepts it, and a
// scanner that does what Gitleaks does with --log-opts (git log -p -U0 <opts>, split on spaces) and
// fails when a synthetic marker is in a selected patch. They hold which commits the script selects
// and that it refuses to guess; what Gitleaks flags is Gitleaks' own, and the pinned binary was run
// through the real script by hand for the pull request that introduced the scope.

// flaggedMarker is what the stand-in scanner fails on. No Gitleaks rule matches it, so the
// repository's own secrets job passes over this file.
const flaggedMarker = "FLAGGED-BY-STAND-IN-SCANNER"

const (
	standInCurl = `#!/bin/sh
while [ $# -gt 0 ]; do
  case $1 in --output) out=$2; shift ;; esac
  shift
done
[ -n "$out" ] || { echo "stand-in curl: no --output" >&2; exit 2; }
cp "$SECRETS_TEST_DIR/gitleaks.tar.gz" "$out"
echo download >> "$SECRETS_TEST_DIR/downloads"
`
	standInSha256sum = "#!/bin/sh\ncat > /dev/null\n"
	standInScanner   = `#!/bin/sh
printf '%s\n' "$@" > "$SECRETS_TEST_DIR/args"
[ "$1" = git ] || { echo "stand-in scanner: unexpected command $1" >&2; exit 2; }
for arg in "$@"; do
  case $arg in --log-opts=*) opts=${arg#--log-opts=} ;; esac
done
[ -n "$opts" ] || { echo 'stand-in scanner: no --log-opts' >&2; exit 2; }
git -C "$2" log --format=%s $opts > "$SECRETS_TEST_DIR/commits" || exit 2
if git -C "$2" log -p -U0 $opts | grep -q "$SECRETS_TEST_MARKER"; then
  echo 'stand-in scanner: leaks found' >&2
  exit 1
fi
`
)

// secretsRepo is the shape of a pull request run: a dev branch, a branch outside the pull request,
// the pull request's branch, and their merge candidate checked out, as actions/checkout leaves it:
// a two-parent commit whose first parent is dev's tip (base) and whose second is the pull request.
type secretsRepo struct {
	*fixtureRepo
	base, head string // dev's tip, and the merge candidate
	standIns   string
}

// newSecretsRepo carries the marker on the outside branch (outside) and in the pull request's own
// commits (inPR). The pull request adds it in one commit and removes it in the next.
func newSecretsRepo(t *testing.T, outside, inPR bool) *secretsRepo {
	t.Helper()
	r := &secretsRepo{fixtureRepo: newRepo(t), standIns: t.TempDir()}
	commit := func(subject string) {
		r.git("add", ".")
		r.git("commit", "-qm", subject)
	}
	r.git("symbolic-ref", "HEAD", "refs/heads/dev")
	r.write("README", "base\n")
	commit("base")
	r.git("checkout", "-q", "-b", "other")
	r.write("outside.txt", "clean\n")
	if outside {
		r.write("outside.txt", flaggedMarker+"\n")
	}
	commit("outside the pull request")
	r.git("checkout", "-q", "dev")
	r.git("checkout", "-q", "-b", "pr")
	if inPR {
		r.write("leak.txt", flaggedMarker+"\n")
		commit("pr adds the marker")
		r.git("rm", "-q", "leak.txt")
		commit("pr removes it again")
	}
	r.write("change.txt", "change\n")
	commit("pr change")
	r.git("checkout", "-q", "dev")
	r.base = strings.TrimSpace(r.git("rev-parse", "HEAD"))
	r.git("checkout", "-q", "--detach")
	r.git("merge", "-q", "--no-ff", "pr", "-m", "merge candidate")
	r.head = strings.TrimSpace(r.git("rev-parse", "HEAD"))

	var archive bytes.Buffer
	gz := gzip.NewWriter(&archive)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: "gitleaks", Mode: 0o755, Size: int64(len(standInScanner))}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte(standInScanner)); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	files := map[string]struct {
		body string
		mode os.FileMode
	}{
		"gitleaks.tar.gz": {archive.String(), 0o644},
		"bin/curl":        {standInCurl, 0o755},
		"bin/sha256sum":   {standInSha256sum, 0o755},
	}
	for name, f := range files {
		path := filepath.Join(r.standIns, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		// The stand-ins (curl, sha256sum, gitleaks) go on PATH and are run, so each descriptor is
		// open only under syscall.ForkLock: a fork in that window would inherit it and leave the
		// path unexecutable (ETXTBSY, golang/go#22315).
		syscall.ForkLock.RLock()
		writeErr := os.WriteFile(path, []byte(f.body), f.mode)
		syscall.ForkLock.RUnlock()
		if writeErr != nil {
			t.Fatal(writeErr)
		}
	}
	return r
}

// secretsRun is one run of the script: its exit code, stderr, and what the stand-ins saw.
type secretsRun struct {
	code      int
	stderr    string
	args      []string // the scanner's argv, nil when it never ran
	scanned   []string // subjects of the commits its --log-opts select
	downloads int
}

// run starts scripts/ci/secrets.sh in the repository with the Actions variables the workflow
// sets for the event (event = "" leaves GITHUB_EVENT_NAME unset, as on a developer's machine) and
// the given PR_BASE_SHA.
func (r *secretsRepo) run(t *testing.T, event, base string) secretsRun {
	t.Helper()
	var env []string
	for _, kv := range os.Environ() {
		// The tests may themselves run under Actions, which sets these.
		if !strings.HasPrefix(kv, "GITHUB_EVENT_NAME=") && !strings.HasPrefix(kv, "PR_BASE_SHA=") && !strings.HasPrefix(kv, "GITLEAKS_CONFIG") {
			env = append(env, kv)
		}
	}
	env = append(env, "PATH="+filepath.Join(r.standIns, "bin")+string(os.PathListSeparator)+os.Getenv("PATH"),
		// Should the stand-ins ever not be found first, a real curl cannot reach the network.
		"https_proxy=http://127.0.0.1:9", "HTTPS_PROXY=http://127.0.0.1:9",
		"SECRETS_TEST_DIR="+r.standIns, "SECRETS_TEST_MARKER="+flaggedMarker, "PR_BASE_SHA="+base)
	if event != "" {
		env = append(env, "GITHUB_EVENT_NAME="+event)
	}
	script := filepath.Join(repoRoot(), "scripts", "ci", "secrets.sh")
	res := runEnv(t, r.root, env, "bash", "--noprofile", "--norc", "-eo", "pipefail", script)
	lines := func(name string) []string {
		data, err := os.ReadFile(filepath.Join(r.standIns, name))
		if err != nil {
			return nil
		}
		return strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	}
	out := secretsRun{code: res.code, stderr: res.stderr, args: lines("args"), scanned: lines("commits"), downloads: len(lines("downloads"))}
	for _, name := range []string{"args", "commits", "downloads"} {
		if err := os.RemoveAll(filepath.Join(r.standIns, name)); err != nil {
			t.Fatal(err)
		}
	}
	return out
}

// logOpts is the --log-opts value the script gave the scanner.
func (s secretsRun) logOpts() string {
	for _, arg := range s.args {
		if value, ok := strings.CutPrefix(arg, "--log-opts="); ok {
			return value
		}
	}
	return ""
}

// A pull request run scans the commits the pull request adds to its base (the merge candidate and
// the pull request's own), whatever another branch carries. -m still diffs the candidate against
// its second parent, so a flagged value on the base side would fail it too; dev's own push scan
// has already passed those commits.
func TestSecrets_a_pull_request_scans_only_the_commits_it_adds(t *testing.T) {
	repo := newSecretsRepo(t, true, false)
	if parents := strings.Fields(repo.git("rev-list", "--parents", "-n", "1", repo.head)); len(parents) != 3 || parents[1] != repo.base {
		t.Fatalf("the merge candidate is not a merge of the base tip: %q", parents)
	}
	got := repo.run(t, "pull_request", repo.base)
	if got.code != 0 {
		t.Fatalf("a marker on another branch fails the pull request (%d)\n%s", got.code, got.stderr)
	}
	expectEqual(t, "--log-opts", got.logOpts(), "-m "+repo.base+".."+repo.head)
	expectEqual(t, "scanned commits", sortedCopy(got.scanned), []string{"merge candidate", "pr change"})
	expectEqual(t, "downloads", got.downloads, 1)
}

// The marker the pull request's own commits carry fails it, also when a later commit removes it.
func TestSecrets_a_pull_request_still_fails_on_its_own_commits(t *testing.T) {
	repo := newSecretsRepo(t, false, true)
	got := repo.run(t, "pull_request", repo.base)
	if got.code == 0 {
		t.Fatalf("a marker in the pull request's own commit passes\n%s", got.stderr)
	}
	if !strings.Contains(strings.Join(got.scanned, "\n"), "pr adds the marker") {
		t.Errorf("the pull request's own commits were not scanned: %q", got.scanned)
	}
}

// Every other event, and a run outside Actions, scans every ref exactly as before the scope
// existed, whatever base is in the environment.
func TestSecrets_every_other_event_scans_every_ref(t *testing.T) {
	for _, event := range []string{"push", "workflow_dispatch", "merge_group", ""} {
		repo := newSecretsRepo(t, true, false)
		got := repo.run(t, event, repo.base)
		if got.code == 0 {
			t.Errorf("event %q: a marker on another branch passes the full scan", event)
		}
		expectEqual(t, "event "+event+" --log-opts", got.logOpts(), "--all -m")
		if !strings.Contains(strings.Join(got.scanned, "\n"), "outside the pull request") {
			t.Errorf("event %q: the other branch was not scanned: %q", event, got.scanned)
		}
	}
}

// A pull request run without a usable base is refused before anything is downloaded, so a wiring
// fault cannot narrow the scan, or widen it again, without a trace.
func TestSecrets_a_pull_request_without_a_usable_base_is_refused(t *testing.T) {
	repo := newSecretsRepo(t, false, false)
	for _, base := range []string{"", "--all", "not-a-sha", repo.base[:12], strings.Repeat("0", 39) + "1"} {
		got := repo.run(t, "pull_request", base)
		if got.code != 1 {
			t.Errorf("base %q: exit %d, want 1\n%s", base, got.code, got.stderr)
		}
		if !strings.Contains(got.stderr, "PR_BASE_SHA") {
			t.Errorf("base %q: the refusal does not name PR_BASE_SHA: %q", base, got.stderr)
		}
		if got.args != nil || got.downloads != 0 {
			t.Errorf("base %q: the scan went on (args %q, %d downloads)", base, got.args, got.downloads)
		}
	}
}

// The secrets job hands the script the pull request's base tip, and still fetches every branch's
// history, which the script needs both for the range and for the full scan. Without the variable a
// pull request run is refused (above), so a regression here fails loudly, but only after the edit.
func TestWorkflow_the_secrets_job_passes_the_pull_request_base(t *testing.T) {
	jobs, _ := workflowJobs(t)
	body := jobs["secrets"]
	script := regexp.MustCompile(`(?m)^      - run: bash scripts/ci/secrets\.sh\n        env:\n          PR_BASE_SHA: \$\{\{ github\.event\.pull_request\.base\.sha \}\}$`)
	if !script.MatchString(body) {
		t.Error("the secrets job does not run scripts/ci/secrets.sh with PR_BASE_SHA from the pull request event's base sha")
	}
	if !regexp.MustCompile(`(?m)^          fetch-depth: 0$`).MatchString(body) {
		t.Error("the secrets job does not fetch full history")
	}
}
