//go:build dev

package ci

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The large-blob guard of `crw-dev ci validate` (CRW-536). Every test but the last two drives the
// real crw-dev binary on a temporary repository and pins the two variables the check reads
// (GITHUB_EVENT_NAME, BLOB_RANGE_BASE), because hosted CI sets them for the test process itself.

const (
	largeBlobTestLimit = 2 << 20
	largeBlobTestList  = ".large-blob-allowlist.json"
	// largeBlobTestFull is what a passing run says when it judged every commit reachable from HEAD.
	largeBlobTestFull = "No blob over 2 MiB comes into the history (every commit reachable from HEAD).\n"
)

// largeBlobTestSince is what a passing run says when it judged the commits added since base.
func largeBlobTestSince(base string) string {
	return "No blob over 2 MiB comes into the history (commits added since " + base + ").\n"
}

// largeBlobTestRepo is a validate fixture with one commit, the base of what a test adds.
func largeBlobTestRepo(t *testing.T) (*fixtureRepo, string) {
	t.Helper()
	r := validateRepo(t)
	r.commit()
	return r, largeBlobTestRev(r, "HEAD")
}

func largeBlobTestRev(r *fixtureRepo, rev string) string {
	return strings.TrimSpace(r.git("rev-parse", rev))
}

// largeBlobTestPayload is size bytes of one repeated byte; seed keeps two payloads apart.
func largeBlobTestPayload(size int, seed byte) []byte {
	return bytes.Repeat([]byte{seed}, size)
}

func largeBlobTestWrite(t *testing.T, r *fixtureRepo, name string, data []byte) {
	t.Helper()
	target := filepath.Join(r.root, name)
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

// largeBlobTestCommit commits everything with a message of its own, so a refusal can be told
// which commit it names.
func largeBlobTestCommit(r *fixtureRepo, message string) string {
	r.git("add", "-A")
	r.git("commit", "-qm", message)
	return largeBlobTestRev(r, "HEAD")
}

func largeBlobTestShort(r *fixtureRepo, rev string) string {
	return strings.TrimSpace(r.git("rev-parse", "--short", rev))
}

// largeBlobTestValidate runs `crw-dev ci validate` with the two variables set as given.
func largeBlobTestValidate(t *testing.T, dir, event, base string) result {
	t.Helper()
	return goCheck(t, dir, []string{"GITHUB_EVENT_NAME=" + event, "BLOB_RANGE_BASE=" + base}, "validate")
}

func largeBlobTestContains(t *testing.T, label, text string, wants ...string) {
	t.Helper()
	for _, want := range wants {
		if !strings.Contains(text, want) {
			t.Errorf("%s: %q is missing from\n%s", label, want, text)
		}
	}
}

// A new file over 2 MiB in the pull request's range is refused with its path, its exact size, the
// commit that brought it, how to shrink it and how to rebuild the branch from its base.
func TestLargeBlob_a_new_file_over_the_limit_is_refused(t *testing.T) {
	r, base := largeBlobTestRepo(t)
	largeBlobTestWrite(t, r, "testdata/oracle.json", largeBlobTestPayload(largeBlobTestLimit+1, 'a'))
	largeBlobTestCommit(r, "add oracle")
	got := largeBlobTestValidate(t, r.root, "pull_request", base)
	expectEqual(t, "exit", got.code, 1)
	expectEqual(t, "stdout", got.stdout, "")
	largeBlobTestContains(t, "refusal", got.stderr,
		"testdata/oracle.json: 2097153 bytes, over the 2 MiB (2097152 bytes) limit",
		"brought in by commit "+largeBlobTestShort(r, "HEAD")+` "add oracle"`,
		"regenerate a generated input deterministically in the test that uses it",
		"hash and count",
		largeBlobTestList,
		"git switch -c <new-branch> "+base,
		"git merge --squash <old-branch>",
		"replacement pull request",
	)
	if strings.Contains(strings.ToLower(got.stderr), "force") {
		t.Errorf("the recovery advice names a force push:\n%s", got.stderr)
	}
}

// The limit is strict: a blob of exactly 2 MiB comes in, and the pass says what was judged.
func TestLargeBlob_a_file_of_exactly_the_limit_passes(t *testing.T) {
	r, base := largeBlobTestRepo(t)
	largeBlobTestWrite(t, r, "testdata/exact.bin", largeBlobTestPayload(largeBlobTestLimit, 'a'))
	largeBlobTestCommit(r, "add exact")
	expectEqual(t, "pull request", largeBlobTestValidate(t, r.root, "pull_request", base),
		result{0, validated + largeBlobTestSince(base), ""})
}

// The allow list admits a named larger file up to its ceiling and the pass counts it; a ceiling
// below the size, or an entry for another path, does not admit it.
func TestLargeBlob_the_allow_list_admits_a_named_file_up_to_its_ceiling(t *testing.T) {
	r, base := largeBlobTestRepo(t)
	size := largeBlobTestLimit + 10
	largeBlobTestWrite(t, r, "testdata/oracle.json", largeBlobTestPayload(size, 'a'))
	largeBlobTestCommit(r, "add oracle")
	list := func(path string, ceiling int) {
		t.Helper()
		largeBlobTestWrite(t, r, largeBlobTestList, []byte(fmt.Sprintf(
			`{"entries": [{"path": %q, "max_bytes": %d, "reason": "recorded corpus the replay test needs"}]}`+"\n", path, ceiling)))
		largeBlobTestCommit(r, "allow list")
	}
	list("testdata/oracle.json", size+5)
	got := largeBlobTestValidate(t, r.root, "pull_request", base)
	expectEqual(t, "ceiling above the size", got, result{0, validated + "No blob over 2 MiB comes into the history (commits added since " + base +
		") beyond 1 allow-listed in " + largeBlobTestList + ".\n", ""})
	list("testdata/oracle.json", size-5)
	got = largeBlobTestValidate(t, r.root, "pull_request", base)
	expectEqual(t, "ceiling below the size: exit", got.code, 1)
	largeBlobTestContains(t, "ceiling below the size", got.stderr,
		"testdata/oracle.json: 2097162 bytes, allow-listed in "+largeBlobTestList+" only up to 2097157 bytes (recorded corpus the replay test needs)")
	list("testdata/other.json", size+5)
	got = largeBlobTestValidate(t, r.root, "pull_request", base)
	expectEqual(t, "entry for another path: exit", got.code, 1)
	largeBlobTestContains(t, "entry for another path", got.stderr, "testdata/oracle.json: 2097162 bytes, over the 2 MiB")
}

// A blob that sits only in an intermediate commit of the range is refused even though the final
// tree no longer has it, and the commit named is the one that brought it. A blob that only a merge
// commit introduces (a conflict resolution) is the merge commit's; one merged in from another
// branch is its own commit's.
func TestLargeBlob_a_blob_only_in_an_intermediate_commit_is_refused(t *testing.T) {
	r, base := largeBlobTestRepo(t)
	largeBlobTestWrite(t, r, "testdata/big.bin", largeBlobTestPayload(largeBlobTestLimit+1, 'b'))
	added := largeBlobTestCommit(r, "add big")
	r.git("rm", "-q", "testdata/big.bin")
	largeBlobTestCommit(r, "drop big")
	got := largeBlobTestValidate(t, r.root, "pull_request", base)
	expectEqual(t, "intermediate: exit", got.code, 1)
	largeBlobTestContains(t, "intermediate", got.stderr, "testdata/big.bin: 2097153 bytes",
		"brought in by commit "+largeBlobTestShort(r, added)+` "add big"`)
	if strings.Contains(got.stderr, `"drop big"`) {
		t.Errorf("the deleting commit is named:\n%s", got.stderr)
	}

	// Only the merge commit holds the blob: its conflict resolution is where it came in.
	m, mbase := largeBlobTestRepo(t)
	trunk := strings.TrimSpace(m.git("branch", "--show-current"))
	m.git("switch", "-q", "-c", "side")
	largeBlobTestWrite(t, m, "side.txt", []byte("side\n"))
	largeBlobTestCommit(m, "side work")
	m.git("switch", "-q", trunk)
	largeBlobTestWrite(t, m, "trunk.txt", []byte("trunk\n"))
	largeBlobTestCommit(m, "trunk work")
	m.git("merge", "-q", "--no-commit", "--no-ff", "side")
	largeBlobTestWrite(t, m, "resolved.bin", largeBlobTestPayload(largeBlobTestLimit+1, 'c'))
	merge := largeBlobTestCommit(m, "merge with a big resolution")
	got = largeBlobTestValidate(t, m.root, "push", mbase)
	expectEqual(t, "merge only: exit", got.code, 1)
	largeBlobTestContains(t, "merge only", got.stderr, "resolved.bin: 2097153 bytes",
		"brought in by commit "+largeBlobTestShort(m, merge)+` "merge with a big resolution"`)

	// Merged in from another branch: the branch's own commit brought it.
	n, nbase := largeBlobTestRepo(t)
	trunk = strings.TrimSpace(n.git("branch", "--show-current"))
	n.git("switch", "-q", "-c", "side")
	largeBlobTestWrite(t, n, "branch.bin", largeBlobTestPayload(largeBlobTestLimit+1, 'd'))
	own := largeBlobTestCommit(n, "branch adds big")
	n.git("switch", "-q", trunk)
	largeBlobTestWrite(t, n, "trunk.txt", []byte("trunk\n"))
	largeBlobTestCommit(n, "trunk work")
	n.git("merge", "-q", "--no-ff", "-m", "merge the branch", "side")
	got = largeBlobTestValidate(t, n.root, "push", nbase)
	expectEqual(t, "merged in: exit", got.code, 1)
	largeBlobTestContains(t, "merged in", got.stderr, "branch.bin: 2097153 bytes",
		"brought in by commit "+largeBlobTestShort(n, own)+` "branch adds big"`)
}

// A blob the base already holds is not new: one left by an earlier commit passes when the base is
// given and fails when the whole history is judged, and so does one the base's own history had
// deleted before the range added it again.
func TestLargeBlob_a_blob_already_in_the_base_history_is_not_new(t *testing.T) {
	r, _ := largeBlobTestRepo(t)
	largeBlobTestWrite(t, r, "testdata/old.bin", largeBlobTestPayload(largeBlobTestLimit+1, 'e'))
	first := largeBlobTestCommit(r, "old big")
	r.git("rm", "-q", "testdata/old.bin")
	base := largeBlobTestCommit(r, "drop old big")
	largeBlobTestWrite(t, r, "testdata/old.bin", largeBlobTestPayload(largeBlobTestLimit+1, 'e'))
	largeBlobTestCommit(r, "add the same bytes again")
	expectEqual(t, "re-added after the base", largeBlobTestValidate(t, r.root, "pull_request", base),
		result{0, validated + largeBlobTestSince(base), ""})
	got := largeBlobTestValidate(t, r.root, "", "")
	expectEqual(t, "full history: exit", got.code, 1)
	largeBlobTestContains(t, "full history", got.stderr, "testdata/old.bin: 2097153 bytes",
		"brought in by commit "+largeBlobTestShort(r, first)+` "old big"`)

	// A later, small commit after a base that holds the blob: the range is clean.
	s, _ := largeBlobTestRepo(t)
	largeBlobTestWrite(t, s, "testdata/old.bin", largeBlobTestPayload(largeBlobTestLimit+1, 'f'))
	sbase := largeBlobTestCommit(s, "old big")
	largeBlobTestWrite(t, s, "note.txt", []byte("note\n"))
	largeBlobTestCommit(s, "small change")
	expectEqual(t, "after a base that holds it", largeBlobTestValidate(t, s.root, "pull_request", sbase),
		result{0, validated + largeBlobTestSince(sbase), ""})
}

// How the range is learned: a pull request needs its base, any other run falls back to the whole
// history (which is stricter, never weaker), and a repository with no commit has nothing to judge.
func TestLargeBlob_the_range_follows_the_event(t *testing.T) {
	r, base := largeBlobTestRepo(t)
	largeBlobTestWrite(t, r, "testdata/small.bin", largeBlobTestPayload(1024, 'g'))
	largeBlobTestCommit(r, "small")
	for _, row := range []struct{ name, base string }{
		{"no base", ""},
		{"not a sha", "not-a-sha"},
		{"a short sha", base[:12]},
		{"an unknown sha", strings.Repeat("0", 39) + "1"},
		{"a revision name", "HEAD~1"},
	} {
		got := largeBlobTestValidate(t, r.root, "pull_request", row.base)
		expectEqual(t, row.name+": exit", got.code, 1)
		expectEqual(t, row.name+": stdout", got.stdout, "")
		largeBlobTestContains(t, row.name, got.stderr, "BLOB_RANGE_BASE")
	}
	for _, row := range []struct{ name, event, base string }{
		{"a push with no previous commit", "push", strings.Repeat("0", 40)},
		{"a push whose previous commit is unknown", "push", strings.Repeat("0", 39) + "1"},
		{"a manual run", "workflow_dispatch", ""},
		{"a local run", "", ""},
	} {
		expectEqual(t, row.name, largeBlobTestValidate(t, r.root, row.event, row.base), result{0, validated + largeBlobTestFull, ""})
	}
	expectEqual(t, "a push with its previous commit", largeBlobTestValidate(t, r.root, "push", base),
		result{0, validated + largeBlobTestSince(base), ""})

	// No commit at all: nothing is judged and the output is the one a repository without history gets.
	bare := validateRepo(t)
	expectEqual(t, "no commit", largeBlobTestValidate(t, bare.root, "pull_request", ""), result{0, validated, ""})
}

// A shallow checkout cannot say which blobs are new, so the check refuses it instead of judging
// fewer commits than it claims.
func TestLargeBlob_a_shallow_checkout_is_refused(t *testing.T) {
	r, base := largeBlobTestRepo(t)
	largeBlobTestWrite(t, r, "note.txt", []byte("note\n"))
	largeBlobTestCommit(r, "second")
	clone := filepath.Join(t.TempDir(), "shallow")
	if out, err := exec.Command("git", "clone", "-q", "--depth", "1", "file://"+r.root, clone).CombinedOutput(); err != nil {
		t.Fatalf("git clone: %v\n%s", err, out)
	}
	for _, event := range []string{"", "pull_request"} {
		got := largeBlobTestValidate(t, clone, event, base)
		expectEqual(t, "event "+event+": exit", got.code, 1)
		expectEqual(t, "event "+event+": stdout", got.stdout, "")
		largeBlobTestContains(t, "event "+event, got.stderr, "shallow", "git fetch --unshallow")
	}
}

// The allow list is read strictly, so a typo cannot quietly leave a file unlisted or an entry
// without a ceiling or a reason, and a bad list is reported whether or not a blob needs it.
func TestLargeBlob_the_allow_list_is_read_strictly(t *testing.T) {
	const entry = `{"path": "a.bin", "max_bytes": 3145728, "reason": "needed"}`
	for _, row := range []struct{ name, list, want string }{
		{"not JSON", `{`, "unexpected end of JSON input"},
		{"not an object", `[]`, "must be a JSON object"},
		{"unknown key", `{"entries": [], "extra": 1}`, `unknown key "extra"`},
		{"no entries", `{}`, `"entries" must be a list`},
		{"entries not a list", `{"entries": {}}`, `"entries" must be a list`},
		{"entry not an object", `{"entries": [1]}`, "entries[0]: must be an object"},
		{"unknown entry key", `{"entries": [{"path": "a.bin", "max_bytes": 3145728, "reason": "needed", "note": 1}]}`, `entries[0]: unknown key "note"`},
		{"no path", `{"entries": [{"max_bytes": 3145728, "reason": "needed"}]}`, "entries[0]: path must be a clean relative path"},
		{"absolute path", `{"entries": [{"path": "/a.bin", "max_bytes": 3145728, "reason": "needed"}]}`, "entries[0]: path must be a clean relative path"},
		{"unclean path", `{"entries": [{"path": "x/../a.bin", "max_bytes": 3145728, "reason": "needed"}]}`, "entries[0]: path must be a clean relative path"},
		{"directory path", `{"entries": [{"path": "x/", "max_bytes": 3145728, "reason": "needed"}]}`, "entries[0]: path must be a clean relative path"},
		{"no reason", `{"entries": [{"path": "a.bin", "max_bytes": 3145728}]}`, "entries[0]: reason must not be empty"},
		{"blank reason", `{"entries": [{"path": "a.bin", "max_bytes": 3145728, "reason": "  "}]}`, "entries[0]: reason must not be empty"},
		{"ceiling at the limit", `{"entries": [{"path": "a.bin", "max_bytes": 2097152, "reason": "needed"}]}`, "entries[0]: max_bytes must be an integer above 2097152"},
		{"ceiling not an integer", `{"entries": [{"path": "a.bin", "max_bytes": 3145728.5, "reason": "needed"}]}`, "entries[0]: max_bytes must be an integer above 2097152"},
		{"ceiling a string", `{"entries": [{"path": "a.bin", "max_bytes": "3145728", "reason": "needed"}]}`, "entries[0]: max_bytes must be an integer above 2097152"},
		{"no ceiling", `{"entries": [{"path": "a.bin", "reason": "needed"}]}`, "entries[0]: max_bytes must be an integer above 2097152"},
		{"duplicate path", `{"entries": [` + entry + `, ` + entry + `]}`, `entries[1]: duplicate path "a.bin"`},
	} {
		r, _ := largeBlobTestRepo(t)
		largeBlobTestWrite(t, r, largeBlobTestList, []byte(row.list+"\n"))
		largeBlobTestCommit(r, "allow list")
		got := largeBlobTestValidate(t, r.root, "", "")
		expectEqual(t, row.name+": exit", got.code, 1)
		expectEqual(t, row.name+": stdout", got.stdout, "")
		largeBlobTestContains(t, row.name, got.stderr, largeBlobTestList+": ", row.want)
	}
	// A list with one good entry, and a repository with no list at all, are both accepted.
	r, _ := largeBlobTestRepo(t)
	expectEqual(t, "no list", largeBlobTestValidate(t, r.root, "", ""), result{0, validated + largeBlobTestFull, ""})
	largeBlobTestWrite(t, r, largeBlobTestList, []byte(`{"entries": [`+entry+"]}\n"))
	largeBlobTestCommit(r, "allow list")
	expectEqual(t, "one entry", largeBlobTestValidate(t, r.root, "", ""), result{0, validated + largeBlobTestFull, ""})
}

// A path is the bytes git records, not what a line-oriented listing makes of it: a name with a
// space and a newline, and one that starts with a newline, are refused under their exact names and
// admitted only by an entry with the exact name, never by one for a prefix of it.
func TestLargeBlob_paths_are_kept_exactly(t *testing.T) {
	r, base := largeBlobTestRepo(t)
	odd := "dir with space/we ird\nname.bin"
	lead := "\nlead.bin"
	largeBlobTestWrite(t, r, odd, largeBlobTestPayload(largeBlobTestLimit+1, 'h'))
	largeBlobTestWrite(t, r, lead, largeBlobTestPayload(largeBlobTestLimit+1, 'i'))
	largeBlobTestCommit(r, "odd names")
	got := largeBlobTestValidate(t, r.root, "pull_request", base)
	expectEqual(t, "odd names: exit", got.code, 1)
	largeBlobTestContains(t, "odd names", got.stderr, `"dir with space/we ird\nname.bin": 2097153 bytes`, `"\nlead.bin": 2097153 bytes`)

	entries := func(paths ...string) {
		t.Helper()
		var items []string
		for _, path := range paths {
			items = append(items, fmt.Sprintf(`{"path": %s, "max_bytes": 3145728, "reason": "needed"}`, jsonQuote(path)))
		}
		largeBlobTestWrite(t, r, largeBlobTestList, []byte(`{"entries": [`+strings.Join(items, ", ")+"]}\n"))
		largeBlobTestCommit(r, "allow list")
	}
	entries("dir with space/we ird", "lead.bin")
	if got := largeBlobTestValidate(t, r.root, "pull_request", base); got.code != 1 {
		t.Errorf("entries for prefixes admitted the files: %+v", got)
	}
	entries(odd, lead)
	expectEqual(t, "exact entries", largeBlobTestValidate(t, r.root, "pull_request", base), result{0, validated + "No blob over 2 MiB comes into the history (commits added since " +
		base + ") beyond 2 allow-listed in " + largeBlobTestList + ".\n", ""})
}

// jsonQuote is text as a JSON string.
func jsonQuote(text string) string {
	return "\"" + strings.NewReplacer("\\", "\\\\", "\"", "\\\"", "\n", "\\n").Replace(text) + "\""
}

// Every blob this repository's own history holds is within the limit, the guard's own proof that
// it judges the repository it ships in without a refusal. A shallow checkout (the hosted test legs)
// sees too little history to say so; the validate job fetches all of it and covers that case.
func TestLargeBlob_this_repository_is_within_the_limit(t *testing.T) {
	root := repoRoot()
	if out, err := runGit(root, "rev-parse", "--is-shallow-repository"); err != nil || strings.TrimSpace(string(out)) != "false" {
		t.Skip("shallow checkout: the validate job judges the full history")
	}
	errs, summary := largeBlobCheck(root, func(string) string { return "" })
	if len(errs) != 0 {
		t.Fatalf("the repository's history holds a blob over the limit:\n%s", strings.Join(errs, "\n"))
	}
	expectEqual(t, "summary", summary+"\n", largeBlobTestFull)
}

// The validate job fetches every commit and hands the check the pull request's base (or the push's
// previous commit), as the secrets job does; without the base the check still runs, over the whole
// history, and a checkout that is shallow is refused, so a regression here fails loudly.
func TestWorkflow_the_validate_job_passes_the_range_base(t *testing.T) {
	jobs, _ := workflowJobs(t)
	body := jobs["validate"]
	step := regexp.MustCompile(`(?m)^      - run: '"\$RUNNER_TEMP/crw-dev" ci validate'\n        env:\n          BLOB_RANGE_BASE: \$\{\{ github\.event\.pull_request\.base\.sha \|\| github\.event\.before \}\}$`)
	if !step.MatchString(body) {
		t.Error("the validate job does not run ci validate with BLOB_RANGE_BASE from the pull request's base sha or the push's previous sha")
	}
	if !regexp.MustCompile(`(?m)^          fetch-depth: 0$`).MatchString(body) {
		t.Error("the validate job does not fetch full history")
	}
}
