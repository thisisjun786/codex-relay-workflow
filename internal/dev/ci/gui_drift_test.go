//go:build dev

package ci

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The drift check of `crw-dev ci gui-drift` (CRW-831). The committed tree is the oracle: a
// fresh build of web/ must reproduce internal/gui/assets file for file and byte for byte, or the
// screen in the binary is not the screen the source describes. Each rejection is pinned here, and
// so is the fail-closed direction: an unreadable git or an unbuilt tree must never read as a pass.

// guiDriftRepo is a fixture whose committed assets hold one index.html and one hashed script.
func guiDriftRepo(t *testing.T) *fixtureRepo {
	t.Helper()
	r := newRepo(t)
	r.write("internal/gui/assets/index.html", "<html><body><div id=\"root\"></div><script src=\"/assets/index-AAAA.js\"></script></body></html>")
	r.write("internal/gui/assets/assets/index-AAAA.js", "console.log(1)\n")
	r.commit()
	return r
}

// guiDriftBuild writes the same tree into a fresh directory, which stands for a rebuild.
func guiDriftBuild(t *testing.T, r *fixtureRepo) string {
	t.Helper()
	built := t.TempDir()
	for _, name := range []string{"index.html", "assets/index-AAAA.js"} {
		data, err := os.ReadFile(filepath.Join(r.root, "internal/gui/assets", filepath.FromSlash(name)))
		if err != nil {
			t.Fatal(err)
		}
		target := filepath.Join(built, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(target, data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return built
}

func guiDriftBuildWrite(t *testing.T, built, name, data string) {
	t.Helper()
	target := filepath.Join(built, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
}

// guiDriftRun runs the check and returns its difference lines.
func guiDriftRun(t *testing.T, r *fixtureRepo, built string) ([]string, error) {
	t.Helper()
	return guiDriftProblems(r.root, "HEAD", built)
}

func guiDriftWants(t *testing.T, problems []string, wants ...string) {
	t.Helper()
	joined := strings.Join(problems, "\n")
	for _, want := range wants {
		if !strings.Contains(joined, want) {
			t.Errorf("the report lacks %q:\n%s", want, joined)
		}
	}
}

// An identical tree passes: this is the green half of the check.
func TestGUIDriftAcceptsAnIdenticalTree(t *testing.T) {
	r := guiDriftRepo(t)
	problems, err := guiDriftRun(t, r, guiDriftBuild(t, r))
	if err != nil {
		t.Fatal(err)
	}
	if len(problems) != 0 {
		t.Fatalf("an identical tree was refused:\n%s", strings.Join(problems, "\n"))
	}
}

// A file the build produces and the commit does not hold is refused.
func TestGUIDriftRejectsAnAddedFile(t *testing.T) {
	r := guiDriftRepo(t)
	built := guiDriftBuild(t, r)
	guiDriftBuildWrite(t, built, "assets/index-BBBB.js", "console.log(2)\n")
	problems, err := guiDriftRun(t, r, built)
	if err != nil {
		t.Fatal(err)
	}
	if len(problems) == 0 {
		t.Fatal("a built file that is not committed passed")
	}
	guiDriftWants(t, problems, "index-BBBB.js", "not committed")
}

// A file the commit holds and the build does not produce is refused.
func TestGUIDriftRejectsAMissingFile(t *testing.T) {
	r := guiDriftRepo(t)
	built := guiDriftBuild(t, r)
	if err := os.Remove(filepath.Join(built, "assets", "index-AAAA.js")); err != nil {
		t.Fatal(err)
	}
	problems, err := guiDriftRun(t, r, built)
	if err != nil {
		t.Fatal(err)
	}
	if len(problems) == 0 {
		t.Fatal("a committed file the build dropped passed")
	}
	guiDriftWants(t, problems, "index-AAAA.js", "not built")
}

// A renamed hash is both a missing file and an added one, and both are named.
func TestGUIDriftRejectsARenamedHash(t *testing.T) {
	r := guiDriftRepo(t)
	built := guiDriftBuild(t, r)
	if err := os.Remove(filepath.Join(built, "assets", "index-AAAA.js")); err != nil {
		t.Fatal(err)
	}
	guiDriftBuildWrite(t, built, "assets/index-CCCC.js", "console.log(1)\n")
	problems, err := guiDriftRun(t, r, built)
	if err != nil {
		t.Fatal(err)
	}
	guiDriftWants(t, problems, "index-AAAA.js", "index-CCCC.js")
}

// One byte of difference in one file is refused, and the file is named.
func TestGUIDriftRejectsAOneByteDifference(t *testing.T) {
	r := guiDriftRepo(t)
	built := guiDriftBuild(t, r)
	guiDriftBuildWrite(t, built, "assets/index-AAAA.js", "console.log(3)\n")
	problems, err := guiDriftRun(t, r, built)
	if err != nil {
		t.Fatal(err)
	}
	if len(problems) == 0 {
		t.Fatal("a one-byte difference passed")
	}
	guiDriftWants(t, problems, "index-AAAA.js", "differs")
}

// A difference of the same length is still refused: the size pre-check alone would pass it.
func TestGUIDriftRejectsASameLengthDifference(t *testing.T) {
	r := guiDriftRepo(t)
	built := guiDriftBuild(t, r)
	guiDriftBuildWrite(t, built, "assets/index-AAAA.js", "console.log(2)\n")
	problems, err := guiDriftRun(t, r, built)
	if err != nil {
		t.Fatal(err)
	}
	if len(problems) == 0 {
		t.Fatal("a same-length byte difference passed")
	}
	guiDriftWants(t, problems, "index-AAAA.js")
}

// An unreadable git is an error, never a pass: a comparison that could not read the committed
// tree must not report agreement.
func TestGUIDriftFailsClosedWhenGitIsUnreadable(t *testing.T) {
	r := guiDriftRepo(t)
	built := guiDriftBuild(t, r)
	if _, err := guiDriftProblems(filepath.Join(r.root, "nowhere"), "HEAD", built); err == nil {
		t.Fatal("a repository that cannot be read passed")
	}
	if _, err := guiDriftProblems(r.root, "no-such-revision", built); err == nil {
		t.Fatal("an unresolvable revision passed")
	}
}

// An unbuilt tree fails loud: the directory must exist and hold an index.html, so a run without a
// build cannot compare the committed tree against itself.
func TestGUIDriftRefusesAnUnbuiltTree(t *testing.T) {
	r := guiDriftRepo(t)
	missing := filepath.Join(t.TempDir(), "not-built")
	if _, err := guiDriftProblems(r.root, "HEAD", missing); err == nil {
		t.Fatal("a directory that was never built passed")
	}
	empty := t.TempDir()
	if _, err := guiDriftProblems(r.root, "HEAD", empty); err == nil {
		t.Fatal("a directory without an index.html passed")
	}
	// The committed tree itself is not a build: naming it must be refused, which is what stops a
	// caller from comparing the commit against itself.
	if _, err := guiDriftProblems(r.root, "HEAD", filepath.Join(r.root, "internal/gui/assets")); err == nil {
		t.Fatal("the committed tree was accepted as a fresh build")
	}
}

// The command line refuses an omitted --built as a usage error rather than comparing something.
func TestGUIDriftRequiresTheBuiltFlag(t *testing.T) {
	r := guiDriftRepo(t)
	got := goCheck(t, r.root, nil, "gui-drift")
	if got.code == 0 {
		t.Fatalf("gui-drift with no --built passed: %+v", got)
	}
	if !strings.Contains(got.stderr, "built") {
		t.Errorf("the refusal does not name the flag: %q", got.stderr)
	}
}

// The command line reports drift on stderr and exits non-zero, and a clean tree on stdout.
func TestGUIDriftCommandReportsDriftAndPass(t *testing.T) {
	r := guiDriftRepo(t)
	built := guiDriftBuild(t, r)
	if got := goCheck(t, r.root, nil, "gui-drift", "--built", built); got.code != 0 {
		t.Fatalf("a clean tree was refused (%d): %s%s", got.code, got.stdout, got.stderr)
	}
	guiDriftBuildWrite(t, built, "assets/index-DDDD.js", "console.log(9)\n")
	got := goCheck(t, r.root, nil, "gui-drift", "--built", built)
	if got.code == 0 {
		t.Fatal("drift was reported as a pass")
	}
	if !strings.Contains(got.stderr, "index-DDDD.js") {
		t.Errorf("the refusal does not name the differing file: %q", got.stderr)
	}
}

// The working tree at internal/gui/assets is what //go:embed compiles, so a stray file, a
// modification or a deletion there is drift even when the built and committed trees agree. Without
// this, a local edit to the embedded tree would be approved and then compiled into the binary.
func TestGUIDriftRejectsADirtyWorkingTree(t *testing.T) {
	for _, row := range []struct {
		name   string
		mutate func(t *testing.T, r *fixtureRepo)
		want   string
	}{
		{"an extra untracked file", func(t *testing.T, r *fixtureRepo) {
			r.write("internal/gui/assets/assets/index-STRAY.js", "console.log('stray')\n")
		}, "index-STRAY.js"},
		{"a modified file", func(t *testing.T, r *fixtureRepo) {
			r.write("internal/gui/assets/assets/index-AAAA.js", "console.log(2)\n")
		}, "index-AAAA.js"},
		{"a deleted file", func(t *testing.T, r *fixtureRepo) {
			if err := os.Remove(filepath.Join(r.root, "internal/gui/assets/assets/index-AAAA.js")); err != nil {
				t.Fatal(err)
			}
		}, "index-AAAA.js"},
	} {
		t.Run(row.name, func(t *testing.T) {
			r := guiDriftRepo(t)
			built := guiDriftBuild(t, r)
			row.mutate(t, r)
			problems, err := guiDriftRun(t, r, built)
			if err != nil {
				t.Fatal(err)
			}
			if len(problems) == 0 {
				t.Fatalf("%s in the embedded working tree passed", row.name)
			}
			guiDriftWants(t, problems, row.want, "working tree")
		})
	}
}

// The refusal names the command that regenerates the committed tree, and that command exists.
func TestGUIDriftNamesTheRegenerationCommand(t *testing.T) {
	r := guiDriftRepo(t)
	built := guiDriftBuild(t, r)
	guiDriftBuildWrite(t, built, "assets/index-EEEE.js", "console.log(1)\n")
	got := goCheck(t, r.root, nil, "gui-drift", "--built", built)
	if got.code == 0 {
		t.Fatal("drift passed")
	}
	if !strings.Contains(got.stderr, "make gui-assets") {
		t.Errorf("the refusal does not name the regeneration command: %q", got.stderr)
	}
	data, err := os.ReadFile(filepath.Join(repoRoot(), "Makefile"))
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`(?m)^gui-assets:`).MatchString(string(data)) {
		t.Error("the Makefile has no gui-assets target, so the named command does not exist")
	}
}

// The check reads the committed tree from the revision it is given, so a caller can judge a
// branch tip rather than the working tree.
func TestGUIDriftReadsTheNamedRevision(t *testing.T) {
	r := guiDriftRepo(t)
	built := guiDriftBuild(t, r)
	first := strings.TrimSpace(r.git("rev-parse", "HEAD"))
	// A later commit changes the committed asset; the fresh build still matches the first commit.
	r.write("internal/gui/assets/assets/index-AAAA.js", "console.log(42)\n")
	r.commit()
	if problems, err := guiDriftProblems(r.root, first, built); err != nil || len(problems) != 0 {
		t.Fatalf("the named revision was not used: %v %v", problems, err)
	}
	if problems, err := guiDriftProblems(r.root, "HEAD", built); err != nil || len(problems) == 0 {
		t.Fatalf("HEAD was not judged: %v %v", problems, err)
	}
}
