package hook

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/tmploc"
	"github.com/thisisjun786/codex-relay-workflow/internal/tmploc/tmploctest"
)

// rejectedPathToken is the first word of a path that the worktree guard's program reader refuses wherever the word stands in a
// program: a removal, a program start or a run-time name (CRW-1028 accepted that a word inside a string is refused too). A
// temporary directory whose path holds such a word makes every row that puts a fixture path into a python3 -c, node or ruby
// program come out as a deny, so a fixture under it judges the path and not the command (CRW-1183). It returns "" for a clean path.
func rejectedPathToken(path string) string {
	for _, sp := range shellIRTokenSpans(path) {
		tok := path[sp[0]:sp[1]]
		if worktreeDelRemovesToken(tok) || worktreeDelStartsToken(tok) {
			return tok
		}
		for _, lang := range []string{"", "node", "ruby", "python", "perl"} {
			if worktreeDelRunTimeToken(lang, tok) {
				return tok
			}
		}
	}
	return ""
}

// cleanTempBase is whether base is an absolute, writable directory whose name and resolved name
// hold no rejected word or gh spelling. A harmless program can contain a post verb such as print's
// pr prefix, so a fixture path must not introduce the gh half of the inline-program post rule.
func cleanTempBase(base string) bool {
	if !filepath.IsAbs(base) || rejectedPathToken(base) != "" || githubPostInlineNamesPost(base+" pr") {
		return false
	}
	resolved, err := filepath.EvalSymlinks(base)
	if err != nil || rejectedPathToken(resolved) != "" || githubPostInlineNamesPost(resolved+" pr") {
		return false
	}
	probe, err := os.MkdirTemp(base, "tb")
	if err != nil {
		return false
	}
	_ = os.Remove(probe)
	return true
}

// neutralTempBase fixes the test's temporary root to a base the worktree guard's word rule does not refuse (CRW-1183). The
// environment's TMPDIR stays when it is clean; otherwise TMPDIR is set for this test to /tmp or /var/tmp, whichever is a clean
// writable directory. With neither, the test is skipped with the reason, or fails when CRW_TEST_REQUIRE_CLEAN_TMPBASE is set (a
// gate run that must not pass a security row by skipping it). The production rule is not changed.
func neutralTempBase(t *testing.T) {
	t.Helper()
	base, _ := tmploc.Lookup(os.Getenv)
	if cleanTempBase(base) {
		return
	}
	if cand, ok := tmploctest.FallbackDir(cleanTempBase); ok {
		t.Setenv("TMPDIR", cand)
		return
	}
	msg := "no clean temporary base: TMPDIR=" + base + " holds the refused word " + strings.TrimSpace(rejectedPathToken(base)) +
		" (or is not a writable directory), and /tmp and /var/tmp are not clean writable directories"
	if os.Getenv("CRW_TEST_REQUIRE_CLEAN_TMPBASE") != "" {
		t.Fatal(msg)
	}
	t.Skip(msg)
}

// requireCleanFixture fails the test when a fixture directory it is about to put into a command, as written and as the
// filesystem resolves it, holds a word the worktree guard refuses: such a row would pass or fail on the path.
func requireCleanFixture(t *testing.T, dirs ...string) {
	t.Helper()
	for _, d := range dirs {
		if tok := rejectedPathToken(d); tok != "" {
			t.Fatalf("fixture %s holds the refused word %q", d, tok)
		}
		if r, err := filepath.EvalSymlinks(d); err == nil {
			if tok := rejectedPathToken(r); tok != "" {
				t.Fatalf("fixture %s resolves to %s, which holds the refused word %q", d, r, tok)
			}
		}
	}
}

// TestRejectedPathTokenNamesTheWordsTheGuardRefuses pins the helper's word list on paths that held the words in the lanes'
// managed worktrees (CRW-1183): the path of a Codex-managed checkout carries "code" in its parent directory name.
func TestRejectedPathTokenNamesTheWordsTheGuardRefuses(t *testing.T) {
	for path, want := range map[string]string{
		"/home/jun/code-worktrees/x":  "code",
		"/home/u/spawn/x":             "spawn",
		"/srv/exec_dir":               "exec_dir",
		"/srv/remove-me/x":            "remove",
		"/srv/rmtree/x":               "rmtree",
		"/srv/subprocess.d":           "subprocess",
		"/tmp/TestReproductionRows/1": "",
		"/scratch/crw/it-guard":       "",
		"/var/tmp":                    "",
	} {
		if got := rejectedPathToken(path); got != want {
			t.Errorf("rejectedPathToken(%q) = %q, want %q", path, got, want)
		}
	}
}

// TestNeutralTempBaseLeavesACleanBase: a TMPDIR without a refused word is kept, so a lane's short TMPDIR (socket paths) stays.
func TestNeutralTempBaseLeavesACleanBase(t *testing.T) {
	base := t.TempDir()
	if !cleanTempBase(base) {
		t.Skipf("the test's own temporary directory %s is not a clean base", base)
	}
	t.Setenv("TMPDIR", base)
	neutralTempBase(t)
	if got := os.Getenv("TMPDIR"); got != base {
		t.Errorf("TMPDIR = %q, want it kept as %q", got, base)
	}
}

// TestNeutralTempBaseReplacesABaseWithARefusedWord: a TMPDIR with a refused word in it (the checkout parent
// "code-worktrees") is replaced by a clean root, so the fixtures a test makes under it hold no refused word, as written and as
// resolved. The directory with the word is real and writable, so only the word makes it unfit (CRW-1183).
func TestNeutralTempBaseReplacesABaseWithARefusedWord(t *testing.T) {
	bad := badTempBase(t)
	t.Setenv("TMPDIR", bad)
	neutralTempBase(t)
	got := os.Getenv("TMPDIR")
	if got == bad || !cleanTempBase(got) {
		t.Fatalf("TMPDIR = %q after neutralTempBase, want a clean base (it was %q)", got, bad)
	}
	requireCleanFixture(t, t.TempDir())
}

// TestNeutralTempBaseResolvesSymlinks: a clean-looking TMPDIR that is a symlink into a directory with a refused word is not
// clean either, because the fixtures are judged by the path newDelRig resolves.
func TestNeutralTempBaseResolvesSymlinks(t *testing.T) {
	bad := badTempBase(t)
	link := filepath.Join(t.TempDir(), "plain")
	if err := os.Symlink(bad, link); err != nil {
		t.Fatal(err)
	}
	if cleanTempBase(link) {
		t.Errorf("a symlink to %s was taken as a clean base", bad)
	}
	t.Setenv("TMPDIR", link)
	neutralTempBase(t)
	if got := os.Getenv("TMPDIR"); got == link {
		t.Errorf("TMPDIR still names the symlink %s", link)
	}
}

// badTempBase is a real, writable directory whose name holds a refused word, made under a clean root.
func badTempBase(t *testing.T) string {
	t.Helper()
	neutralTempBase(t)
	bad, err := tmploc.MkdirTemp("code-worktrees-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(bad) })
	if rejectedPathToken(bad) == "" {
		t.Fatalf("%s holds no refused word", bad)
	}
	return bad
}

// TestGuardRowsRunWhereTMPDIRHoldsARefusedWord runs the rows that put a fixture path into the program of a command, in a
// child test process whose TMPDIR is a directory with a refused word (CRW-1183): TestReproductionRows (272 rows),
// TestCRW941ShapesThroughTheThreeEntryPoints and the GitHub post guard's body-file cases (a fixture body under the test's
// TMPDIR named through links, and the committed body named by its absolute path). Each must run and pass: the child sets
// CRW_TEST_REQUIRE_CLEAN_TMPBASE so a missing clean base fails and does not skip, and a SKIP line in the child's output fails here.
func TestGuardRowsRunWhereTMPDIRHoldsARefusedWord(t *testing.T) {
	if os.Getenv("CRW_TEMPBASE_CHILD") != "" {
		t.Skip("the child run")
	}
	if testing.Short() {
		t.Skip("runs a child test process")
	}
	bad := badTempBase(t)
	names := []string{"TestReproductionRows", "TestCRW941ShapesThroughTheThreeEntryPoints",
		"TestGitHubPostGuardRefusesAFinalLinkBody_andFollowsADirectoryLink", "TestGitHubPostGuardAcceptsAReadableRegularBodyAnywhere"}
	cmd := exec.Command(os.Args[0], "-test.count=1", "-test.v", "-test.run", "^("+strings.Join(names, "|")+")$")
	cmd.Env = append(os.Environ(), "TMPDIR="+bad, "CRW_TEMPBASE_CHILD=1", "CRW_TEST_REQUIRE_CLEAN_TMPBASE=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("child run with TMPDIR=%s failed: %v\n%s", bad, err, tail(string(out), 4000))
	}
	for _, name := range names {
		if !strings.Contains(string(out), "--- PASS: "+name+" ") {
			t.Errorf("%s did not run to a pass in the child run:\n%s", name, tail(string(out), 4000))
		}
	}
	if strings.Contains(string(out), "--- SKIP") {
		t.Errorf("a row was skipped in the child run:\n%s", tail(string(out), 4000))
	}
}

func tail(s string, n int) string {
	if len(s) > n {
		return s[len(s)-n:]
	}
	return s
}

// cleanTempDir is a fresh directory under a clean temporary base (neutralTempBase), removed with the test. Its name holds no
// word of the test's name: t.TempDir names the directory after the test, and a row id such as "exec-01" or "find-delete" would put
// a refused word into every fixture path the row writes into its command (CRW-1183).
func cleanTempDir(t *testing.T) string {
	t.Helper()
	neutralTempBase(t)
	dir, err := tmploc.MkdirTemp("fx")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	requireCleanFixture(t, dir)
	return dir
}

// A fixture path is data, but the inline-program post guard reads its spelling too.
// Keep a caller's gh-containing directory out of otherwise harmless program fixtures.
func TestNeutralTempBaseAvoidsGitHubPostSpelling(t *testing.T) {
	base := filepath.Join(t.TempDir(), "enough")
	if err := os.MkdirAll(base, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", base)
	cwd, root, _ := gateScene(t)
	command := strings.ReplaceAll(`python3 -c 'import os; print(os.path.exists("{M}/n.md"))'`, "{M}", root)
	if out := HandleGitHubPostGuard(gateBash(t, cwd, command)); out != "" {
		t.Fatalf("fixture path changed a harmless program's verdict: %s", out)
	}
}
