package skill

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
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
		"go.mod":       "module example.test/m\n\ngo 1.21\n",
		"p/a.go":       "package p\n\nfunc A() int { return 1 }\n",
		"tool/main.go": "package main\n\nfunc main() {}\n",
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
		names := []string{}
		for _, e := range left {
			names = append(names, e.Name())
		}
		t.Errorf("the check left %d entries in its scratch directory: %v", len(left), names)
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
	if os.Getenv("GOCACHE") == "" {
		t.Fatal("the test process pins the Go caches before it moves HOME")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
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
	if left, _ := os.ReadDir(home); len(left) != 0 {
		t.Errorf("the caller's home was written: %v", left)
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
	mbcExpect(t, runSkillInProcess("merge-build-check", "--unknown"), 2, "crw skill merge-build-check: error: flag provided but not defined: -unknown")
	mbcWithoutGo(t)
	mbcExpect(t, m.check(t, "--head", head, "--base", "dev"), 3, "go tool")
	mbcExpect(t, m.check(t, "--head", head, "--base", "dev", "--timeout", "1ns"), 3, "it ran out of its 1ns")
}

// A package with a file that only builds under the dev tag is built under it as well, so a clash
// between a plain file and a dev file shows; --tags with no value leaves that out.
func TestMergeBuildCheckBuildsAPackageUnderTheDevTagToo(t *testing.T) {
	m := newMbcRepo(t)
	m.commitFiles("a plain file", map[string]string{"mix/a.go": "package mix\n\nfunc A() {}\n"})
	head := m.sibling("g1", map[string]string{"mix/b.go": "//go:build dev\n\npackage mix\n\nfunc A() {}\n"})
	mbcExpect(t, m.check(t, "--head", head, "--base", "dev", "--tags", ""), 0, "ok: ")
	got := m.check(t, "--head", head, "--base", "dev")
	mbcExpect(t, got, 1, "refused: build_failed:", "A redeclared in this block", "example.test/m/mix")
}

// A package that builds only for darwin is vetted for darwin although the host skips it, and a main
// package builds without writing a binary next to a directory of its name.
func TestMergeBuildCheckVetsADarwinOnlyPackageAndBuildsAMainPackage(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the darwin-only package is the host's own on darwin")
	}
	m := newMbcRepo(t)
	tool := m.sibling("m1", map[string]string{"tool/main.go": "package main\n\nfunc main() { _ = 1 }\n"})
	mbcExpect(t, m.check(t, "--head", tool, "--base", "dev"), 0, "ok: ", "packages=1", "checked: example.test/m/tool steps=build,vet,vet_darwin")
	head := m.sibling("m2", map[string]string{"dn/x_darwin.go": "package dn\n\nvar X = undefinedName\n"})
	mbcExpect(t, m.check(t, "--head", head, "--base", "dev"), 1,
		"refused: vet_darwin_failed:", "undefinedName", "skipped: example.test/m/dn steps=build,vet,test_compile reason=not_built_on_host")
}

// A -tags the caller carries must not decide the untagged pass: a tag named by GOFLAGS, and again a
// tag named by the caller's go settings file, must leave the two //go:build !dev files of the merge
// in the untagged pass and turn their clash into a build_failed refusal. Without an explicit empty
// tag set both passes run tagged and the check answers ok (CRW-562).
func TestMergeBuildCheckIgnoresATagTheCallerCarries(t *testing.T) {
	for _, c := range []struct {
		name string
		use  func(t *testing.T)
	}{
		{"GOFLAGS", func(t *testing.T) { t.Setenv("GOFLAGS", "-tags=dev") }},
		{"the go settings file", func(t *testing.T) {
			mbcNoGoFlags(t)
			settings := filepath.Join(t.TempDir(), "goenv")
			if err := os.WriteFile(settings, []byte("GOFLAGS=-tags=dev\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			t.Setenv("GOENV", settings)
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			m := newMbcRepo(t)
			clash := "//go:build !dev\n\npackage p\n\nfunc Clash() {}\n"
			m.sibling("s1", map[string]string{"p/b.go": clash})
			head := m.sibling("s2", map[string]string{"p/c.go": clash})
			m.land("s1")
			c.use(t)
			mbcExpect(t, m.check(t, "--head", head, "--base", "dev"), 1,
				"refused: build_failed:", "Clash redeclared in this block", "example.test/m/p", "step=build")
		})
	}
}

// mbcNoGoFlags takes GOFLAGS out of this process's environment, where it would mask the value the
// caller's go settings file names.
func mbcNoGoFlags(t *testing.T) {
	t.Helper()
	if value, ok := os.LookupEnv("GOFLAGS"); ok {
		os.Unsetenv("GOFLAGS")
		t.Cleanup(func() { os.Setenv("GOFLAGS", value) })
	}
}

// Every step of the untagged pass names the empty tag set on its command line, so a -tags the
// caller carries cannot decide it: with GOFLAGS=-tags=dev a shim that logs each go invocation and
// then runs the real go shows list, build, vet and test asked both tagged and untagged (CRW-562).
func TestMergeBuildCheckNamesTheEmptyTagSetOnEveryStep(t *testing.T) {
	realGo, err := exec.LookPath("go")
	if err != nil {
		t.Fatal(err)
	}
	git, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	trueTool, err := exec.LookPath("true")
	if err != nil {
		t.Fatal(err)
	}
	m := newMbcRepo(t)
	head := m.sibling("s1", map[string]string{
		"p/b.go":      mbcTokenize,
		"p/b_test.go": "package p\n\nimport \"testing\"\n\nfunc TestB(t *testing.T) { tokenize() }\n",
	})
	bin := t.TempDir()
	for name, path := range map[string]string{"git": git, "true": trueTool} {
		if err := os.Symlink(path, filepath.Join(bin, name)); err != nil {
			t.Fatal(err)
		}
	}
	log := filepath.Join(t.TempDir(), "go.log")
	shim := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$MBC_SHIM_LOG\"\nexec " + realGo + " \"$@\"\n"
	if err := os.WriteFile(filepath.Join(bin, "go"), []byte(shim), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	t.Setenv("MBC_SHIM_LOG", log)
	t.Setenv("GOFLAGS", "-tags=dev")
	mbcExpect(t, m.check(t, "--head", head, "--base", "dev"), 0, "ok: ")
	raw, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]map[string]bool{}
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		step, _, _ := strings.Cut(line, " ")
		if seen[step] == nil {
			seen[step] = map[string]bool{}
		}
		for _, form := range []string{"-tags=", "-tags dev"} {
			if strings.Contains(line, form+" ") {
				seen[step][form] = true
			}
		}
	}
	for _, step := range []string{"list", "build", "vet", "test"} {
		for _, form := range []string{"-tags=", "-tags dev"} {
			if !seen[step][form] {
				t.Errorf("go %s was never asked with %q: %q", step, form, strings.Split(string(raw), "\n"))
			}
		}
	}
}

// The go tool runs with a home, a temporary directory and a telemetry setting of its own, below the
// directory the check removes, and with the caller's caches. A check that outran its time is cut off:
// the go process is gone, exit 3, and nothing is left, not even the go tool's own temporary files.
func TestMergeBuildCheckKeepsTheGoToolInsideItsOwnScratchAndEndsItOnTimeout(t *testing.T) {
	m := newMbcRepo(t)
	head := m.sibling("s1", map[string]string{"p/b.go": mbcTokenize})
	bin := t.TempDir()
	logFile := filepath.Join(t.TempDir(), "go.log")
	git, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(git, filepath.Join(bin, "git")); err != nil {
		t.Fatal(err)
	}
	shim := "#!/bin/sh\n{ echo \"pid=$$\"; echo \"args=$*\"; echo \"home=$HOME\"; echo \"tmpdir=$TMPDIR\"; echo \"gotmpdir=$GOTMPDIR\"; echo \"gocache=$GOCACHE\"; echo \"gowork=$GOWORK\"; echo \"xdg=$XDG_CONFIG_HOME\"; echo \"goenv=$GOENV\"; } > \"$MBC_SHIM_LOG\"\n/bin/mkdir \"$TMPDIR/go-build123\"\nexec /bin/sleep 60\n"
	if err := os.WriteFile(filepath.Join(bin, "go"), []byte(shim), 0o700); err != nil {
		t.Fatal(err)
	}
	realHome := t.TempDir()
	t.Setenv("HOME", realHome)
	config, err := os.UserConfigDir()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	t.Setenv("MBC_SHIM_LOG", logFile)
	t.Setenv("GOCACHE", "/pinned/gocache")
	mbcExpect(t, m.check(t, "--head", head, "--base", "dev", "--timeout", "3s"), 3, "it ran out of its 3s")
	raw, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatalf("the go tool was never started: %v", err)
	}
	seen := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		key, value, _ := strings.Cut(line, "=")
		seen[key] = value
	}
	// On darwin the check reads the caller's caches first, with the go tool's HOME inside its own
	// scratch directory; elsewhere the first command it runs is the list.
	firstArgs, firstHomeOK := "list -e -p=4 ", seen["home"] == realHome
	if runtime.GOOS == "darwin" {
		firstArgs, firstHomeOK = "env GOCACHE GOMODCACHE GOPATH", strings.HasPrefix(seen["home"], m.scratch)
	}
	if !strings.HasPrefix(seen["args"], firstArgs) {
		t.Errorf("the first go command is %q, want %q", seen["args"], firstArgs)
	}
	for _, key := range []string{"tmpdir", "gotmpdir", "xdg"} {
		if !strings.HasPrefix(seen[key], m.scratch) {
			t.Errorf("%s of the go tool is %q, not below the scratch directory %s", key, seen[key], m.scratch)
		}
	}
	if !firstHomeOK || seen["gocache"] != "/pinned/gocache" || seen["gowork"] != "off" || seen["goenv"] != filepath.Join(config, "go", "env") {
		t.Errorf("home %q, gocache %q, gowork %q, goenv %q", seen["home"], seen["gocache"], seen["gowork"], seen["goenv"])
	}
	pid, _ := strconv.Atoi(seen["pid"])
	for i := 0; i < 20 && syscall.Kill(pid, 0) == nil; i++ {
		time.Sleep(100 * time.Millisecond)
	}
	if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
		t.Errorf("the go process %d outlived the check: %v", pid, err)
	}
	if left, _ := os.ReadDir(realHome); len(left) != 0 {
		t.Errorf("the caller's home was written: %v", left)
	}
}

// What a tag changes in a package the head only calls into counts too: p is built under dev with the
// dev version of q, which lacks what p now uses.
func TestMergeBuildCheckBuildsAChangedPackageAgainstTheDevVersionOfWhatItImports(t *testing.T) {
	m := newMbcRepo(t)
	m.commitFiles("q has a plain and a dev version", map[string]string{
		"q/plain.go": "//go:build !dev\n\npackage q\n\nfunc A() {}\n",
		"q/dev.go":   "//go:build dev\n\npackage q\n",
	})
	head := m.sibling("u1", map[string]string{"p/use.go": "package p\n\nimport \"example.test/m/q\"\n\nfunc Use() { q.A() }\n"})
	mbcExpect(t, m.check(t, "--head", head, "--base", "dev", "--tags", ""), 0, "ok: ")
	mbcExpect(t, m.check(t, "--head", head, "--base", "dev"), 1, "refused: build_failed:", "undefined: q.A", "example.test/m/p")
}

// A head that only deletes a package has no package to check: the check does not fall back to the
// module's root package, which go lists when it is given no pattern.
func TestMergeBuildCheckChecksNothingWhenTheHeadOnlyRemovesAPackage(t *testing.T) {
	m := newMbcRepo(t)
	m.commitFiles("a root package that does not build under dev", map[string]string{
		"root.go":     "package m\n",
		"root_dev.go": "//go:build dev\n\npackage m\n\nvar _ = undefinedName\n",
		"gone/g.go":   "package gone\n",
	})
	head := m.sibling("r1", nil, "gone/g.go")
	mbcExpect(t, m.check(t, "--head", head, "--base", "dev"), 0, "ok: ", "packages=0", "skipped: gone reason=removed")
}

// The go environment keeps the caller's settings file, wherever the caller's directory was.
func TestMergeBuildEnvKeepsTheCallersGoSettingsFile(t *testing.T) {
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	config, err := os.UserConfigDir()
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ set, want string }{
		{"", "GOENV=" + filepath.Join(config, "go", "env")},
		{"settings/go-env", "GOENV=" + filepath.Join(cwd, "settings", "go-env")},
		{"/etc/go-env", "GOENV=/etc/go-env"},
		{"off", ""},
	} {
		t.Setenv("GOENV", c.set)
		env, err := mergeBuildEnv(context.Background(), []string{"A=1", "HOME=" + t.TempDir()}, t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, kv := range env {
			if strings.HasPrefix(kv, "GOENV=") {
				got = append(got, kv)
			}
		}
		switch {
		case c.want == "" && len(got) != 0, c.want != "" && !reflect.DeepEqual(got, []string{c.want}):
			t.Errorf("GOENV=%q: the environment names %v, want %q", c.set, got, c.want)
		}
	}
}

// The go tool on darwin reads the directory it keeps telemetry in from HOME, not from
// XDG_CONFIG_HOME, so the darwin environment moves HOME into the directory the run owns, reads the
// three cache locations the caller's go environment holds without running a go command under the
// caller's home either, and pins the caller's values. Linux keeps the caller's HOME (CRW-562).
func TestMergeBuildEnvMovesOnlyTheDarwinHome(t *testing.T) {
	callerHome := t.TempDir()
	callerConfig := t.TempDir()
	caller := append(os.Environ(), "HOME="+callerHome, "XDG_CONFIG_HOME="+callerConfig)
	owned := t.TempDir()
	darwinEnv, err := mergeBuildEnv(context.Background(), caller, owned, "darwin")
	if err != nil {
		t.Fatal(err)
	}
	got := mbcLastEnv(darwinEnv)
	home := filepath.Join(owned, "home")
	if got["HOME"] != home {
		t.Errorf("darwin: HOME=%q, want the owned %q", got["HOME"], home)
	}
	if info, err := os.Stat(home); err != nil || !info.IsDir() {
		t.Errorf("darwin: the owned home is not a directory: %v", err)
	}
	if left, _ := os.ReadDir(callerHome); len(left) != 0 {
		t.Errorf("darwin: the cache read wrote to the caller's home: %v", left)
	}
	if left, _ := os.ReadDir(callerConfig); len(left) != 0 {
		t.Errorf("darwin: the cache read wrote to the caller's config directory: %v", left)
	}
	caches := mbcCallerGoEnv(t, caller)
	for key, want := range map[string]string{"GOCACHE": caches[0], "GOMODCACHE": caches[1], "GOPATH": caches[2]} {
		if got[key] != want {
			t.Errorf("darwin: %s=%q, want the caller's %q", key, got[key], want)
		}
	}
	linuxEnv, err := mergeBuildEnv(context.Background(), caller, t.TempDir(), "linux")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := mbcLastEnv(linuxEnv)["HOME"], mbcLastEnv(caller)["HOME"]; got != want {
		t.Errorf("linux: HOME=%q, want the caller's %q", got, want)
	}
}

// What go env answers is a value in itself: a path that ends in a space is the path, not another
// one trimmed into shape (CRW-562).
func TestMergeBuildEnvKeepsWhatGoEnvAnswers(t *testing.T) {
	bin := t.TempDir()
	shim := "#!/bin/sh\nprintf '%s\\n' '/cache with space/ ' '/mod' '/gopath'\n"
	if err := os.WriteFile(filepath.Join(bin, "go"), []byte(shim), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	env, err := mergeBuildEnv(context.Background(), []string{"HOME=/caller"}, t.TempDir(), "darwin")
	if err != nil {
		t.Fatal(err)
	}
	got := mbcLastEnv(env)
	for key, want := range map[string]string{"GOCACHE": "/cache with space/ ", "GOMODCACHE": "/mod", "GOPATH": "/gopath"} {
		if got[key] != want {
			t.Errorf("%s=%q, want %q", key, got[key], want)
		}
	}
}

// mbcCallerGoEnv reads the three cache locations a go env names for the caller's environment.
func mbcCallerGoEnv(t *testing.T, base []string) [3]string {
	t.Helper()
	goTool, err := exec.LookPath("go")
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(goTool, "env", "GOCACHE", "GOMODCACHE", "GOPATH")
	cmd.Env = base
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(string(out), "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("go env GOCACHE GOMODCACHE GOPATH answered %q", out)
	}
	return [3]string{strings.TrimSpace(lines[0]), strings.TrimSpace(lines[1]), strings.TrimSpace(lines[2])}
}

// The cache read darwin needs runs under the check's own context: a go env that stalls is cut off
// with the check instead of keeping merge-build-check running past --timeout (CRW-562).
func TestMergeBuildEnvStopsAStalledCacheRead(t *testing.T) {
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "go"), []byte("#!/bin/sh\nexec sleep 60\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	start := time.Now()
	if _, err := mergeBuildEnv(ctx, []string{"HOME=/caller"}, t.TempDir(), "darwin"); err == nil {
		t.Error("a stalled go env was not cut off")
	}
	if elapsed := time.Since(start); elapsed > 30*time.Second {
		t.Errorf("the stalled go env was not cut off promptly: %v", elapsed)
	}
}

// mbcLastEnv folds an environment the way exec uses it: of duplicated keys the last value wins.
func mbcLastEnv(env []string) map[string]string {
	last := map[string]string{}
	for _, kv := range env {
		if key, value, ok := strings.Cut(kv, "="); ok {
			last[key] = value
		}
	}
	return last
}
