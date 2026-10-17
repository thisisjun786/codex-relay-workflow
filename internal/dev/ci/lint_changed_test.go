//go:build dev

package ci

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const lintChangedModule = "module example.invalid/lintfixture\n\ngo 1.27\n"

const lintChangedGoWrapper = `#!/usr/bin/env bash
if [ -n "${LINT_FAKE_GO_LOG:-}" ]; then
  printf '%s\n' "$*" >> "$LINT_FAKE_GO_LOG"
fi
if [ "${1:-}" = run ] && [ "${2:-}" = honnef.co/go/tools/cmd/staticcheck ]; then
  echo "fake staticcheck: $*" >&2
  exit "${LINT_FAKE_SC_STATUS:-0}"
fi
exec "$LINT_REAL_GO" "$@"
`

func lintChangedCleanSource(name string) string {
	return "package " + name + "\n\nfunc F() {}\n"
}

func lintChangedVetBadSource(name string) string {
	return "package " + name + "\n\nimport \"fmt\"\n\nfunc F() { fmt.Printf(\"%d\", \"wrong\") }\n"
}

func lintChangedRepo(t *testing.T) *fixtureRepo {
	t.Helper()
	repo := newRepo(t)
	repo.write("go.mod", lintChangedModule)
	return repo
}

func lintChangedPublish(t *testing.T, repo *fixtureRepo) {
	t.Helper()
	repo.git("update-ref", "refs/remotes/origin/dev", strings.TrimSpace(repo.git("rev-parse", "HEAD")))
}

func lintChangedTools(t *testing.T, staticcheckStatus string) (logPath string, env []string) {
	t.Helper()
	dir := t.TempDir()
	wrapper := filepath.Join(dir, "go")
	if err := os.WriteFile(wrapper, []byte(lintChangedGoWrapper), 0o755); err != nil {
		t.Fatal(err)
	}
	real, err := exec.LookPath("go")
	if err != nil {
		t.Fatal(err)
	}
	logPath = filepath.Join(dir, "go.log")
	env = []string{
		"GO=" + wrapper,
		"GOWORK=off",
		"GOPROXY=off",
		"LINT_FAKE_GO_LOG=" + logPath,
		"LINT_REAL_GO=" + real,
	}
	if staticcheckStatus != "" {
		env = append(env, "LINT_FAKE_SC_STATUS="+staticcheckStatus)
	}
	return logPath, env
}

func runLintChanged(t *testing.T, root string, env []string, base ...string) result {
	t.Helper()
	args := append([]string{filepath.Join(repoRoot(), "scripts", "ci", "lint-changed.sh")}, base...)
	return runCommand(t, root, env, "bash", args...)
}

func lintChangedCalls(t *testing.T, logPath string) [][]string {
	t.Helper()
	data, err := os.ReadFile(logPath)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var calls [][]string
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line != "" {
			calls = append(calls, strings.Fields(line))
		}
	}
	return calls
}

func lintChangedCommandCalls(calls [][]string, cmd ...string) [][]string {
	var out [][]string
	for _, c := range calls {
		if len(c) < len(cmd) {
			continue
		}
		match := true
		for i := range cmd {
			if c[i] != cmd[i] {
				match = false
				break
			}
		}
		if match {
			out = append(out, c)
		}
	}
	return out
}

func lintChangedVetArgs(calls [][]string, tag string) []string {
	var out []string
	for _, c := range lintChangedCommandCalls(calls, "vet") {
		args := c[1:]
		got := ""
		if len(args) >= 2 && args[0] == "-tags" {
			got = args[1]
			args = args[2:]
		}
		if got == tag {
			out = append(out, args...)
		}
	}
	return out
}

func lintChangedContains(args []string, pkg string) bool {
	for _, a := range args {
		if a == pkg {
			return true
		}
	}
	return false
}

func TestLintChangedNoChanges(t *testing.T) {
	repo := lintChangedRepo(t)
	repo.write("pkga/a.go", lintChangedCleanSource("pkga"))
	repo.commit()
	lintChangedPublish(t, repo)
	repo.write("README.md", "notes\n")
	logPath, env := lintChangedTools(t, "")
	res := runLintChanged(t, repo.root, env)
	if res.code != 0 {
		t.Fatalf("lint-changed failed with no Go changes: %d\n%s%s", res.code, res.stdout, res.stderr)
	}
	if !strings.Contains(res.stdout, "no Go files changed") {
		t.Errorf("the output does not report the empty scope:\n%s%s", res.stdout, res.stderr)
	}
	if calls := lintChangedCalls(t, logPath); len(calls) != 0 {
		t.Errorf("the go tool ran with no Go changes: %v", calls)
	}
}

func TestLintChangedRejectsChangedViolations(t *testing.T) {
	for _, tc := range []struct {
		name   string
		file   string
		source string
		want   []string
	}{
		{"vet", "pkga/a.go", lintChangedVetBadSource("pkga"), []string{"fmt.Printf format %d"}},
		{"gofmt", "pkgb/b.go", "package pkgb\n\nfunc  F( ) {}\n", []string{"gofmt needed", "pkgb/b.go"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := lintChangedRepo(t)
			repo.write("pkgc/c.go", lintChangedCleanSource("pkgc"))
			repo.commit()
			lintChangedPublish(t, repo)
			repo.write(tc.file, tc.source)
			_, env := lintChangedTools(t, "")
			res := runLintChanged(t, repo.root, env)
			if res.code == 0 {
				t.Fatalf("a changed %s violation passed:\n%s%s", tc.name, res.stdout, res.stderr)
			}
			for _, want := range tc.want {
				if !strings.Contains(res.stdout+res.stderr, want) {
					t.Errorf("the output does not show %q:\n%s%s", want, res.stdout, res.stderr)
				}
			}
		})
	}
}

func TestLintChangedIgnoresUnchangedViolations(t *testing.T) {
	repo := lintChangedRepo(t)
	repo.write("unrelated/u.go", "package unrelated\n\nimport \"fmt\"\n\nfunc F() {  fmt.Printf(\"%d\", \"wrong\")  }\n")
	repo.write("clean/c.go", lintChangedCleanSource("clean"))
	repo.commit()
	lintChangedPublish(t, repo)
	repo.write("clean/c.go", "package clean\n\nfunc F() {}\n\nfunc G() {}\n")
	logPath, env := lintChangedTools(t, "")
	res := runLintChanged(t, repo.root, env)
	if res.code != 0 {
		t.Fatalf("lint-changed failed on a clean change: %d\n%s%s", res.code, res.stdout, res.stderr)
	}
	for _, c := range lintChangedCalls(t, logPath) {
		for _, a := range c {
			if strings.Contains(a, "unrelated") {
				t.Errorf("the unchanged broken package was linted: %v", c)
			}
		}
	}
}

func TestLintChangedWorktreeAndCommits(t *testing.T) {
	repo := lintChangedRepo(t)
	for _, d := range []string{"pkga", "pkgb", "pkgc", "pkgd", "pkge"} {
		repo.write(d+"/f.go", lintChangedCleanSource(d))
	}
	repo.commit()
	lintChangedPublish(t, repo)
	repo.write("pkga/f.go", "package pkga\n\nfunc F() {}\n\nfunc G() {}\n")
	repo.commit()
	repo.write("pkgb/f.go", "package pkgb\n\nfunc F() {}\n\nfunc G() {}\n")
	repo.git("add", "pkgb/f.go")
	repo.write("pkgc/f.go", "package pkgc\n\nfunc F() {}\n\nfunc G() {}\n")
	repo.write("pkgd/new.go", "package pkgd\n\nfunc New() {}\n")
	repo.write("pkge/f.go", "package pkge\n\nfunc F() {}\n\nfunc G() {}\n")
	repo.git("add", "pkge/f.go")
	repo.write("pkge/f.go", lintChangedCleanSource("pkge"))
	logPath, env := lintChangedTools(t, "")
	res := runLintChanged(t, repo.root, env)
	if res.code != 0 {
		t.Fatalf("lint-changed failed on clean worktree changes: %d\n%s%s", res.code, res.stdout, res.stderr)
	}
	calls := lintChangedCalls(t, logPath)
	vet := lintChangedCommandCalls(calls, "vet")
	if len(vet) != 1 {
		t.Fatalf("vet ran %d times for the default scope, want 1: %v", len(vet), vet)
	}
	count := map[string]int{}
	for _, a := range vet[0][1:] {
		count[a]++
	}
	for _, d := range []string{"pkga", "pkgb", "pkgc", "pkgd", "pkge"} {
		pkg := "example.invalid/lintfixture/" + d
		if count[pkg] != 1 {
			t.Errorf("%s was vetted %d times, want 1: %v", pkg, count[pkg], vet[0])
		}
	}
	if len(count) != 5 {
		t.Errorf("unexpected packages vetted: %v", vet[0])
	}
}

func TestLintChangedMergeBase(t *testing.T) {
	repo := lintChangedRepo(t)
	repo.write("pkga/a.go", lintChangedCleanSource("pkga"))
	repo.write("unrelated/u.go", lintChangedCleanSource("unrelated"))
	repo.commit()
	base := strings.TrimSpace(repo.git("rev-parse", "HEAD"))
	repo.write("pkga/a.go", "package pkga\n\nfunc F() {}\n\nfunc G() {}\n")
	repo.commit()
	feature := strings.TrimSpace(repo.git("rev-parse", "HEAD"))
	repo.git("reset", "--hard", "-q", base)
	repo.write("unrelated/u.go", "package unrelated\n\nfunc F() {}\n\nfunc U() {}\n")
	repo.commit()
	lintChangedPublish(t, repo)
	repo.git("reset", "--hard", "-q", feature)
	logPath, env := lintChangedTools(t, "")
	res := runLintChanged(t, repo.root, env, "origin/dev")
	if res.code != 0 {
		t.Fatalf("lint-changed failed: %d\n%s%s", res.code, res.stdout, res.stderr)
	}
	vet := lintChangedVetArgs(lintChangedCalls(t, logPath), "")
	want := []string{"example.invalid/lintfixture/pkga"}
	if strings.Join(vet, " ") != strings.Join(want, " ") {
		t.Errorf("vetted packages = %v, want %v (the unrelated change lives on the base side)", vet, want)
	}
	res = runLintChanged(t, repo.root, env, "no-such-ref")
	if res.code == 0 {
		t.Fatalf("lint-changed passed with a missing base:\n%s%s", res.stdout, res.stderr)
	}
	if !strings.Contains(res.stderr, "no merge-base") {
		t.Errorf("the missing-base failure does not name it:\n%s", res.stderr)
	}
}

func TestLintChangedTags(t *testing.T) {
	repo := lintChangedRepo(t)
	repo.write("pkga/a.go", lintChangedCleanSource("pkga"))
	repo.commit()
	lintChangedPublish(t, repo)
	repo.write("pkgv/v.go", lintChangedVetBadSource("pkgv"))
	repo.write("internal/dev/devpkg/d.go", "//go:build dev\n\n"+lintChangedVetBadSource("devpkg"))
	repo.write("internal/runtime/integration/itest/i.go", "//go:build integration\n\n"+lintChangedVetBadSource("itest"))
	repo.commit()
	logPath, env := lintChangedTools(t, "")
	res := runLintChanged(t, repo.root, env)
	if res.code == 0 {
		t.Fatalf("changed vet violations under every tag passed:\n%s%s", res.stdout, res.stderr)
	}
	calls := lintChangedCalls(t, logPath)
	defaultArgs := lintChangedVetArgs(calls, "")
	if strings.Join(defaultArgs, " ") != "example.invalid/lintfixture/pkgv" {
		t.Errorf("default-scope vet args = %v, want only pkgv", defaultArgs)
	}
	if devArgs := lintChangedVetArgs(calls, "dev"); !lintChangedContains(devArgs, "example.invalid/lintfixture/internal/dev/devpkg") {
		t.Errorf("the dev-tagged package was not vetted: %v", devArgs)
	}
	if integArgs := lintChangedVetArgs(calls, "integration"); !lintChangedContains(integArgs, "example.invalid/lintfixture/internal/runtime/integration/itest") {
		t.Errorf("the integration-tagged package was not vetted: %v", integArgs)
	}
	sc := lintChangedCommandCalls(calls, "run", "honnef.co/go/tools/cmd/staticcheck")
	sawDefault, sawDev := false, false
	for _, c := range sc {
		args := c[2:]
		if lintChangedContains(args, "example.invalid/lintfixture/pkgv") {
			sawDefault = true
		}
		if lintChangedContains(args, "-tags") && lintChangedContains(args, "dev") && lintChangedContains(args, "example.invalid/lintfixture/internal/dev/devpkg") {
			sawDev = true
		}
		if lintChangedContains(args, "integration") {
			t.Errorf("staticcheck ran for the integration scope: %v", c)
		}
	}
	if !sawDefault {
		t.Errorf("staticcheck did not run for the default scope: %v", sc)
	}
	if !sawDev {
		t.Errorf("staticcheck did not run for the dev scope: %v", sc)
	}
}

func TestLintChangedDeletedAndRenamed(t *testing.T) {
	repo := lintChangedRepo(t)
	repo.write("pkga/a.go", lintChangedCleanSource("pkga"))
	repo.write("pkga/b.go", "package pkga\n\nfunc G() {}\n")
	repo.write("pkgold/keep.go", lintChangedCleanSource("pkgold"))
	repo.write("pkgold/out.go", "package pkgold\n\nfunc Out() {}\n")
	repo.write("gonepkg/g.go", lintChangedCleanSource("gonepkg"))
	repo.commit()
	lintChangedPublish(t, repo)
	repo.git("rm", "-q", "pkga/a.go", "gonepkg/g.go")
	if err := os.MkdirAll(filepath.Join(repo.root, "pkgnew"), 0o755); err != nil {
		t.Fatal(err)
	}
	repo.git("mv", "pkgold/out.go", "pkgnew/moved.go")
	repo.commit()
	logPath, env := lintChangedTools(t, "")
	res := runLintChanged(t, repo.root, env)
	if res.code != 0 {
		t.Fatalf("lint-changed failed over deletions and a rename: %d\n%s%s", res.code, res.stdout, res.stderr)
	}
	vet := lintChangedVetArgs(lintChangedCalls(t, logPath), "")
	for _, d := range []string{"pkga", "pkgold", "pkgnew"} {
		if !lintChangedContains(vet, "example.invalid/lintfixture/"+d) {
			t.Errorf("the surviving package %s was not vetted: %v", d, vet)
		}
	}
	if lintChangedContains(vet, "example.invalid/lintfixture/gonepkg") {
		t.Errorf("the deleted package was vetted: %v", vet)
	}
	if strings.Contains(res.stderr, "no such file") {
		t.Errorf("gofmt was asked about a deleted file:\n%s", res.stderr)
	}
}

func TestLintChangedTempdir(t *testing.T) {
	for _, tmpdir := range []string{"scratch", "."} {
		t.Run("relative "+tmpdir, func(t *testing.T) {
			repo := lintChangedRepo(t)
			repo.write("pkga/a.go", lintChangedCleanSource("pkga"))
			repo.commit()
			lintChangedPublish(t, repo)
			entries := func() []string {
				list, err := os.ReadDir(repo.root)
				if err != nil {
					t.Fatal(err)
				}
				names := make([]string, 0, len(list))
				for _, e := range list {
					names = append(names, e.Name())
				}
				return names
			}
			before := entries()
			_, env := lintChangedTools(t, "")
			res := runLintChanged(t, repo.root, append(env, "TMPDIR="+tmpdir))
			if res.code != 2 {
				t.Fatalf("a relative TMPDIR gave exit %d, want 2:\n%s%s", res.code, res.stdout, res.stderr)
			}
			want := "tmpdir_not_absolute: TMPDIR=" + tmpdir + "; set TMPDIR to an absolute directory"
			if !strings.Contains(res.stderr, want) {
				t.Errorf("the refusal does not carry %q:\n%s", want, res.stderr)
			}
			expectEqual(t, "repository entries after the refusal", entries(), before)
		})
	}
	for _, tc := range []struct {
		name string
		set  bool
	}{
		{"empty", true},
		{"unset", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := lintChangedRepo(t)
			repo.write("pkga/a.go", lintChangedCleanSource("pkga"))
			repo.commit()
			lintChangedPublish(t, repo)
			_, tools := lintChangedTools(t, "")
			script := filepath.Join(repoRoot(), "scripts", "ci", "lint-changed.sh")
			var res result
			if tc.set {
				res = runLintChanged(t, repo.root, append(tools, "TMPDIR="))
			} else {
				var environ []string
				for _, e := range os.Environ() {
					if !strings.HasPrefix(e, "TMPDIR=") {
						environ = append(environ, e)
					}
				}
				res = runEnv(t, repo.root, append(environ, tools...), "bash", script)
			}
			if res.code != 0 {
				t.Fatalf("lint-changed with %s TMPDIR failed: %d\n%s%s", tc.name, res.code, res.stdout, res.stderr)
			}
		})
	}
}

func TestLintChangedStaticcheckFailure(t *testing.T) {
	repo := lintChangedRepo(t)
	repo.write("pkga/a.go", lintChangedCleanSource("pkga"))
	repo.commit()
	lintChangedPublish(t, repo)
	repo.write("pkga/a.go", "package pkga\n\nfunc F() {}\n\nfunc G() {}\n")
	logPath, env := lintChangedTools(t, "1")
	res := runLintChanged(t, repo.root, env)
	if res.code == 0 {
		t.Fatalf("a staticcheck failure passed:\n%s%s", res.stdout, res.stderr)
	}
	sc := lintChangedCommandCalls(lintChangedCalls(t, logPath), "run", "honnef.co/go/tools/cmd/staticcheck")
	found := false
	for _, c := range sc {
		if lintChangedContains(c[2:], "example.invalid/lintfixture/pkga") {
			found = true
		}
	}
	if !found {
		t.Errorf("no staticcheck call covered the changed package: %v", sc)
	}
	if !strings.Contains(res.stderr, "fake staticcheck") {
		t.Errorf("the staticcheck failure is invisible:\n%s", res.stderr)
	}
}
