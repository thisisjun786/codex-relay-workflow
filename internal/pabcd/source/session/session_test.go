package session

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
)

func TestMain(m *testing.M) { testsupport.Main(m) }

// hermetic isolates git from the machine: no inherited git variables, no user or system configuration, a fixed identity
// and dates, and repository discovery that stops at the returned directory (canonical, one test's repositories live
// below it). Setenv forbids t.Parallel.
func hermetic(t *testing.T) string {
	t.Helper()
	for _, entry := range os.Environ() {
		if name, _, _ := strings.Cut(entry, "="); strings.HasPrefix(name, "GIT_") {
			t.Setenv(name, "")
			_ = os.Unsetenv(name)
		}
	}
	base, err := filepath.EvalSymlinks(t.TempDir())
	must(t, err)
	for name, value := range map[string]string{
		"GIT_CONFIG_GLOBAL": os.DevNull, "GIT_CONFIG_NOSYSTEM": "1", "GIT_CEILING_DIRECTORIES": base,
		"GIT_AUTHOR_NAME": "fixture", "GIT_AUTHOR_EMAIL": "fixture@example.invalid", "GIT_AUTHOR_DATE": "2026-01-01T00:00:00Z",
		"GIT_COMMITTER_NAME": "fixture", "GIT_COMMITTER_EMAIL": "fixture@example.invalid", "GIT_COMMITTER_DATE": "2026-01-01T00:00:00Z",
	} {
		t.Setenv(name, value)
	}
	return base
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func gitIn(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

// newRepo is a repository named name below base with one commit holding tracked.txt.
func newRepo(t *testing.T, base, name string) string {
	t.Helper()
	root := filepath.Join(base, name)
	must(t, os.Mkdir(root, 0o755))
	gitIn(t, root, "init", "-q", "-b", "main", ".")
	must(t, os.WriteFile(filepath.Join(root, "tracked.txt"), []byte("x\n"), 0o644))
	gitIn(t, root, "add", "tracked.txt")
	gitIn(t, root, "commit", "-qm", "init")
	return root
}

func TestPublishNeverReplacesAndLeavesNoTemp(t *testing.T) {
	dir := t.TempDir()
	fresh, kept := filepath.Join(dir, "fresh.json"), filepath.Join(dir, "kept.json")
	must(t, publish(fresh, []byte("whole\n")))
	if got, _ := os.ReadFile(fresh); string(got) != "whole\n" {
		t.Fatalf("fresh content %q", got)
	}
	if info, err := os.Stat(fresh); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("fresh mode %v, %v", info, err)
	}
	// The loser of a publication race: the path exists, its error is nil, and the winner's bytes stay.
	must(t, os.WriteFile(kept, []byte("keep"), 0o644))
	must(t, publish(kept, []byte("other")))
	if got, _ := os.ReadFile(kept); string(got) != "keep" {
		t.Fatalf("an existing file was replaced: %q", got)
	}
	entries, err := os.ReadDir(dir)
	must(t, err)
	if len(entries) != 2 {
		t.Fatalf("a temp file stayed: %v", entries)
	}
}

// realpath applies ".." after the symlinks before it, rejects a missing component and an empty path.
func TestCanonicalIsRealpath(t *testing.T) {
	base := hermetic(t)
	must(t, os.MkdirAll(filepath.Join(base, "main", "sub"), 0o755))
	must(t, os.Symlink("main/sub", filepath.Join(base, "link")))
	if got, err := canonical(base + "/link/.."); err != nil || got != base+"/main" {
		t.Fatalf("symlink then ..: %q, %v", got, err)
	}
	for _, in := range []string{base + "/nosuch/..", ""} {
		if _, err := canonical(in); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("%q: %v", in, err)
		}
	}
	t.Chdir(base)
	if got, err := canonical("link/.."); err != nil || got != base+"/main" {
		t.Fatalf("relative: %q, %v", got, err)
	}
}

// JSON.stringify(value, null, 2) writes < > & and U+2028 / U+2029 as they are, and escapes a backslash before "u2029".
func TestEncodeIsJSONStringify(t *testing.T) {
	got, err := encode(Binding{Version: 1, OwnerSessionID: "s", NativeCwd: "/a<b>&c", SourceRoot: "/l\u2028s\u2029\\u2029", CommonDir: "/c", GitDir: "/g"})
	must(t, err)
	want := "{\n  \"version\": 1,\n  \"ownerSessionId\": \"s\",\n  \"nativeCwd\": \"/a<b>&c\",\n  \"sourceRoot\": \"/l\u2028s\u2029\\\\u2029\",\n  \"commonDir\": \"/c\",\n  \"gitDir\": \"/g\"\n}\n"
	if string(got) != want {
		t.Fatalf("got  %q\nwant %q", got, want)
	}
}

// The oracle's execFileSync fails a probe whose output (stdout and stderr together) passes its 1 MiB maxBuffer, and every
// probe failure is the one identity message; a fake git on PATH prints to stderr and names an existing directory.
func TestGitIdentityRefusesProbeOutputOverOneMiB(t *testing.T) {
	hermetic(t)
	bin, root := t.TempDir(), t.TempDir()
	root, err := filepath.EvalSymlinks(root)
	must(t, err)
	must(t, os.WriteFile(filepath.Join(bin, "git"), []byte("#!/bin/sh\nhead -c \"$FAKE_STDERR\" /dev/zero | tr '\\0' x >&2\necho \"$FAKE_ROOT\"\n"), 0o755))
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("FAKE_ROOT", root)
	for stderr, ok := range map[int]bool{1<<20 - 1000: true, 1 << 20: false} {
		t.Setenv("FAKE_STDERR", fmt.Sprint(stderr))
		w, err := gitIdentity(t.TempDir())
		if ok != (err == nil) || (ok && w != worktree{root, root, root}) || (!ok && err.Error() != "Cannot resolve source Git worktree identity.") {
			t.Fatalf("%d bytes of stderr: %+v, %v", stderr, w, err)
		}
	}
}

// Two binders of different worktrees race: one binding stays, a winner owns it, every successful call names it, a loser
// fails with one of the oracle's messages, and no temp file stays.
func TestConcurrentBindKeepsOneBinding(t *testing.T) {
	base := hermetic(t)
	main := newRepo(t, base, "main")
	targets := []string{filepath.Join(base, "wt1"), filepath.Join(base, "wt2")}
	for i, target := range targets {
		gitIn(t, main, "worktree", "add", "-q", "-b", fmt.Sprintf("w%d", i), target)
	}
	type outcome struct {
		root string
		err  error
	}
	outcomes := make([]outcome, 8)
	var wg sync.WaitGroup
	for i := range outcomes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			outcomes[i].root, outcomes[i].err = Bind(main, "s1", targets[i%2])
		}()
	}
	wg.Wait()
	final, err := Resolve(main, "s1")
	must(t, err)
	if !slices.Contains(targets, final) {
		t.Fatalf("binding names %q", final)
	}
	won := 0
	for _, o := range outcomes {
		switch {
		case o.err == nil && o.root == final:
			won++
		case o.err != nil && slices.Contains([]string{"Source binding is immutable; use a new session for a different worktree.", "Another source binding won publication; existing binding preserved."}, o.err.Error()):
		default:
			t.Errorf("outcome %+v with the binding at %s", o, final)
		}
	}
	if won == 0 {
		t.Error("nobody bound")
	}
	if entries, err := os.ReadDir(filepath.Join(main, ".crw", "sources")); err != nil || len(entries) != 1 {
		t.Fatalf("sources: %v, %v", entries, err)
	}
}
