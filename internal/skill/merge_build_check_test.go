package skill

import (
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

// The merge-build-check tests run the real command on synthetic repositories that hold a tiny Go
// module. A sibling is a branch from dev that adds files, as a parallel pull request does; each
// sibling is green on its own base and git merges two of them without a conflict, so only the
// build of the merge can tell they clash.

// mbcRepo is the module under git. Every run of the command has TMPDIR pointing at a directory of
// its own, which has to be empty again afterwards.
type mbcRepo struct {
	*refreshRepo
	scratch string
}

func newMbcRepo(t *testing.T) *mbcRepo {
	t.Helper()
	if _, err := exec.LookPath("go"); err != nil {
		t.Fatalf("these tests build real modules and need the go tool on PATH: %v", err)
	}
	m := &mbcRepo{refreshRepo: newRefreshRepoFormat(t, "sha1"), scratch: filepath.Join(t.TempDir(), "scratch")}
	if err := os.Mkdir(m.scratch, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", m.scratch)
	m.commitFiles("module", map[string]string{
		"go.mod": "module example.test/m\n\ngo 1.21\n",
		"p/a.go": "package p\n\nfunc A() int { return 1 }\n",
	})
	return m
}

// commitFiles writes the files, removes the named ones and commits everything on the current branch.
func (m *mbcRepo) commitFiles(message string, files map[string]string, remove ...string) string {
	m.t.Helper()
	for name, content := range files {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(m.path, name)), 0o755); err != nil {
			m.t.Fatal(err)
		}
		m.write(name, content)
	}
	for _, name := range remove {
		m.git("rm", "-q", name)
	}
	m.git("add", "-A")
	m.git("commit", "-q", "-m", message)
	return m.git("rev-parse", "HEAD")
}

// sibling branches from dev, commits the files there and returns the head; dev stays checked out.
func (m *mbcRepo) sibling(name string, files map[string]string, remove ...string) string {
	m.branchFrom(name, "dev")
	head := m.commitFiles(name, files, remove...)
	m.git("checkout", "-q", "dev")
	return head
}

// land merges a sibling into dev, as the merge lane does.
func (m *mbcRepo) land(branch string) { m.git("merge", "-q", "--no-ff", "-m", "land "+branch, branch) }

// check runs the command on this repository and requires the scratch directory to be empty again.
func (m *mbcRepo) check(t *testing.T, args ...string) skillProcessResult {
	t.Helper()
	got := runSkillInProcess(append([]string{"merge-build-check", "--repo", m.path}, args...)...)
	if left, _ := os.ReadDir(m.scratch); len(left) != 0 {
		t.Errorf("the check left %d entries in its scratch directory", len(left))
	}
	return got
}

func mbcExpect(t *testing.T, got skillProcessResult, exit int, wants ...string) {
	t.Helper()
	if got.exit != exit {
		t.Errorf("exit %d, want %d:\n%s%s", got.exit, exit, got.stdout, got.stderr)
	}
	for _, want := range wants {
		if !strings.Contains(got.stdout+got.stderr, want) {
			t.Errorf("the answer lacks %q:\n%s%s", want, got.stdout, got.stderr)
		}
	}
}

// mbcWithoutGo leaves only git on PATH, so a check that reaches for the go tool cannot run.
func mbcWithoutGo(t *testing.T) {
	t.Helper()
	git, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.Symlink(git, filepath.Join(dir, "git")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
}

const mbcTokenize = "package p\n\nfunc tokenize() {}\n"

// Two siblings that each add a file declaring one identifier to one package are each fine on their
// own base. Merged, the package does not compile: that is the answer the check exists to give,
// and the sibling that landed first is not blamed.
func TestMergeBuildCheckFindsAnIdentifierBothSiblingsDeclare(t *testing.T) {
	m := newMbcRepo(t)
	fork := m.git("rev-parse", "dev")
	m.sibling("s1", map[string]string{"p/b.go": mbcTokenize})
	head := m.sibling("s2", map[string]string{"p/c.go": mbcTokenize})
	m.land("s1")
	if out := m.git("merge-tree", "--write-tree", "dev", "s2"); len(out) < 40 {
		t.Fatalf("git does not merge the siblings cleanly: %q", out)
	}
	mbcExpect(t, m.check(t, "--head", head, "--base", fork), 0, "ok: ", "packages=1")
	got := m.check(t, "--head", head, "--base", "dev")
	mbcExpect(t, got, 1, "refused: build_failed:", "tokenize redeclared in this block", "example.test/m/p", "merge_tree=", "step=build")
	if strings.Contains(got.stdout, "ok: ") {
		t.Errorf("a refusal also says ok: %s", got.stdout)
	}
}

func TestMergeBuildCheckPassesAHeadThatDoesNotClash(t *testing.T) {
	m := newMbcRepo(t)
	m.sibling("s1", map[string]string{"p/b.go": mbcTokenize})
	head := m.sibling("s3", map[string]string{"p/d.go": "package p\n\nfunc other() {}\n", "p/d_test.go": "package p\n\nimport \"testing\"\n\nfunc TestOther(t *testing.T) { other() }\n"})
	m.land("s1")
	mbcExpect(t, m.check(t, "--head", head, "--base", "dev"), 0,
		"ok: ", "packages=1", "steps=build,vet,test_compile,vet_darwin", "merge_tree=", "rule=merged_tree_build",
		"checked: example.test/m/p steps=build,vet,test_compile,vet_darwin")
}

// A real conflict is git's to report, and the check stops there without building: the go tool is not
// even on PATH.
func TestMergeBuildCheckRefusesARealConflictBeforeAnyBuild(t *testing.T) {
	m := newMbcRepo(t)
	head := m.sibling("s4", map[string]string{"p/a.go": "package p\n\nfunc A() int { return 2 }\n"})
	m.commitFiles("dev edits the same line", map[string]string{"p/a.go": "package p\n\nfunc A() int { return 3 }\n"})
	mbcWithoutGo(t)
	mbcExpect(t, m.check(t, "--head", head, "--base", "dev"), 1,
		"refused: merge_conflict:", "p/a.go", "not a build finding", "merge_tree=none")
}

func TestMergeBuildCheckRefusesAHeadThatIsAlreadyOnTheBase(t *testing.T) {
	m := newMbcRepo(t)
	head := m.sibling("s1", map[string]string{"p/b.go": mbcTokenize})
	m.land("s1")
	mbcWithoutGo(t)
	mbcExpect(t, m.check(t, "--head", head, "--base", "dev"), 1, "refused: head_on_base:")
}

// Both a check that finds something and one that passes leave the caller's repository exactly as it
// was: its files, its refs, its worktrees and its working tree.
func TestMergeBuildCheckLeavesTheCallersRepositoryAlone(t *testing.T) {
	m := newMbcRepo(t)
	m.sibling("s1", map[string]string{"p/b.go": mbcTokenize})
	bad := m.sibling("s2", map[string]string{"p/c.go": mbcTokenize})
	good := m.sibling("s3", map[string]string{"p/d.go": "package p\n\nfunc other() {}\n"})
	m.land("s1")
	m.git("status", "--porcelain")
	snapshot := func() []any {
		files := map[string]int64{}
		root := filepath.Join(m.path, ".git")
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			info, err := d.Info()
			rel, _ := filepath.Rel(root, path)
			files[rel] = info.Size()
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		return []any{files, m.git("for-each-ref"), m.git("worktree", "list"), m.git("rev-parse", "HEAD"), m.git("status", "--porcelain")}
	}
	before := snapshot()
	mbcExpect(t, m.check(t, "--head", bad, "--base", "dev"), 1, "refused: build_failed:")
	mbcExpect(t, m.check(t, "--head", good, "--base", "dev"), 0, "ok: ")
	if after := snapshot(); !reflect.DeepEqual(before, after) {
		t.Errorf("the caller's repository changed:\nbefore %v\nafter  %v", before, after)
	}
}

// Each step names itself in the refusal, so a build that passes and a vet that does not is not
// reported as a build failure, and a package that does not even list is its own finding.
func TestMergeBuildCheckNamesTheStepThatFailed(t *testing.T) {
	m := newMbcRepo(t)
	vet := m.sibling("v1", map[string]string{"p/v.go": "package p\n\nimport \"fmt\"\n\nfunc V() { fmt.Printf(\"%d\\n\", \"s\") }\n"})
	list := m.sibling("v2", map[string]string{"p/w.go": "package q\n"})
	got := m.check(t, "--head", vet, "--base", "dev")
	mbcExpect(t, got, 1, "refused: vet_failed:", "step=vet", "example.test/m/p", "Printf")
	if strings.Contains(got.stdout, "build_failed") {
		t.Errorf("the build passed, but the answer blames it: %s", got.stdout)
	}
	mbcExpect(t, m.check(t, "--head", list, "--base", "dev"), 1, "refused: list_failed:", "found packages p (a.go) and q (w.go)")
}

// Directories that are not packages of the merge are skipped for a stated reason, and a package that
// is built only under a tag, or only on linux, is still checked where it builds.
func TestMergeBuildCheckSkipsWhatItCannotBuildAndSaysWhy(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the linux-only package is excluded from the default build elsewhere")
	}
	m := newMbcRepo(t)
	m.commitFiles("a package the head removes", map[string]string{"gone/g.go": "package gone\n"})
	head := m.sibling("t1", map[string]string{
		"devtool/d.go":    "//go:build dev\n\npackage devtool\n\nfunc D() {}\n",
		"plat/l_linux.go": "package plat\n\nfunc L() {}\n",
		"testdata/x/x.go": "package x\n\nthis is not go\n",
		"docs/notes.md":   "notes\n",
	}, "gone/g.go")
	mbcExpect(t, m.check(t, "--head", head, "--base", "dev"), 0,
		"packages=2",
		"checked: example.test/m/devtool tags=dev steps=build,vet,vet_darwin",
		"checked: example.test/m/plat steps=build,vet\n",
		"skipped: example.test/m/plat step=vet_darwin reason=not_built_on_darwin",
		"skipped: gone reason=removed",
		"skipped: testdata/x reason=ignored_dir")
}

// A head that changes no Go package has nothing to build, so the check answers without the go tool.
func TestMergeBuildCheckHasNothingToBuildForAHeadWithoutGoPackages(t *testing.T) {
	m := newMbcRepo(t)
	docs := m.sibling("d1", map[string]string{"README.md": "docs\n"})
	mod := m.sibling("d2", map[string]string{"go.mod": "module example.test/m\n\ngo 1.21.0\n"})
	mbcWithoutGo(t)
	mbcExpect(t, m.check(t, "--head", docs, "--base", "dev"), 0, "ok: ", "packages=0")
	mbcExpect(t, m.check(t, "--head", mod, "--base", "dev"), 0, "packages=0", "note: go.mod changed")
}

// An input git or the command line cannot read is exit 2, and a missing go tool is exit 3.
func TestMergeBuildCheckReadsItsInputsOrSaysItCannot(t *testing.T) {
	m := newMbcRepo(t)
	head := m.sibling("s1", map[string]string{"p/b.go": mbcTokenize})
	mbcExpect(t, m.check(t, "--head", "nosuchrevision", "--base", "dev"), 2, "cannot read nosuchrevision")
	mbcExpect(t, m.check(t, "--head", head), 2, "origin/dev")
	mbcExpect(t, m.check(t, "--base", "dev"), 2, "--head is required")
	mbcExpect(t, m.check(t, "--head", head, "--base", "dev", "--parallel", "0"), 2, "--parallel")
	mbcExpect(t, runSkillInProcess("merge-build-check", "--repo", t.TempDir(), "--head", "HEAD", "--base", "dev"), 2, "cannot read")
	mbcExpect(t, runSkillInProcess("merge-build-check", "-h"), 0, "usage: crw skill merge-build-check [flags]", "-head", "-base", "-tags")
	mbcWithoutGo(t)
	mbcExpect(t, m.check(t, "--head", head, "--base", "dev"), 3, "go tool")
}
