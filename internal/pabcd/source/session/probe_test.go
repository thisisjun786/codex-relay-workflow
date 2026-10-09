package session

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/source"
)

// inheritedRouting is the environment a shell can hand crw: every variable here points git elsewhere or
// narrows what it sees. The B2-05 reproduction failed source binding with the first one alone.
func inheritedRouting(t *testing.T, base string) {
	t.Helper()
	other := newRepo(t, base, "decoy")
	for name, value := range map[string]string{
		"GIT_OBJECT_DIRECTORY":             filepath.Join(base, "nonexistent-objects"),
		"GIT_ALTERNATE_OBJECT_DIRECTORIES": filepath.Join(base, "nonexistent-alternates"),
		"GIT_DIR":                          filepath.Join(other, ".git"),
		"GIT_CEILING_DIRECTORIES":          base,
		"GIT_NAMESPACE":                    "elsewhere",
	} {
		t.Setenv(name, value)
	}
}

// CRW-1135 criterion 1: with the inherited routing variables, a linked worktree is bound, resolved and
// captured with the same common directory, git directory and root as in a clean environment.
func TestLinkedWorktreeSourceIgnoresInheritedGitRouting(t *testing.T) {
	type answer struct {
		binding []byte
		root    string
		commit  string
		gate    bool
	}
	run := func(inherit bool) answer {
		base := hermetic(t)
		main := newRepo(t, base, "main")
		wt := filepath.Join(base, "wt")
		gitIn(t, main, "worktree", "add", "-q", "-b", "work", wt)
		if inherit {
			inheritedRouting(t, base)
		}
		root, err := Bind(main, "s1", wt)
		if err != nil {
			t.Fatalf("inherit=%v: bind: %v", inherit, err)
		}
		resolved, err := Resolve(main, "s1")
		if err != nil || resolved != root {
			t.Fatalf("inherit=%v: resolve %q, %v", inherit, resolved, err)
		}
		id, err := Capture(main, "s1", CaptureOptions{})
		if err != nil || id.Kind != source.KindResolved {
			t.Fatalf("inherit=%v: capture %+v, %v", inherit, id, err)
		}
		data, err := os.ReadFile(filepath.Join(main, ".crw", "sources", "s1.json"))
		must(t, err)
		data = bytes.ReplaceAll(data, []byte(base), []byte("$R"))
		return answer{data, root[len(base):], id.CommitSha, CheckBound(main, "s1").OK}
	}
	clean, inherited := run(false), run(true)
	if !bytes.Equal(clean.binding, inherited.binding) || clean.root != inherited.root || clean.commit != inherited.commit || clean.gate != inherited.gate || !clean.gate {
		t.Fatalf("clean %+v\ninherited %+v", clean, inherited)
	}
	var b Binding
	must(t, json.Unmarshal(clean.binding, &b))
	if b.CommonDir != "$R/main/.git" || b.GitDir != "$R/main/.git/worktrees/wt" || b.SourceRoot != "$R/wt" {
		t.Fatalf("binding %+v", b)
	}
}

// CRW-1135 criterion 2: a native probe that cannot run (git missing, timed out, a directory it may not
// enter) is unknown, never "not a repository": binding another repository is refused, an existing binding
// is kept byte for byte and resolving it is refused. A native directory git answers is outside any
// repository still binds.
func TestNativeProbeFailureIsUnknownNotOutsideARepository(t *testing.T) {
	for name, failure := range map[string]error{
		"git missing": &exec.Error{Name: "git", Err: exec.ErrNotFound},
		"timeout":     context.DeadlineExceeded,
		"permission":  &fs.PathError{Op: "chdir", Path: "native", Err: fs.ErrPermission},
	} {
		t.Run(name, func(t *testing.T) {
			base := hermetic(t)
			main := newRepo(t, base, "main")
			other := newRepo(t, base, "other")
			wt := filepath.Join(base, "wt")
			gitIn(t, main, "worktree", "add", "-q", "-b", "work", wt)
			fail := func() {
				t.Helper()
				nativeGit = func(string, ...string) (string, error) { return "", failure }
				t.Cleanup(func() { nativeGit = git })
			}
			restore := func() { nativeGit = git }

			fail()
			if root, err := Bind(main, "s1", other); err != unknownNative {
				t.Fatalf("bind from a native directory whose repository is unknown: %q, %v", root, err)
			}
			if _, err := os.Lstat(filepath.Join(main, ".crw", "sources", "s1.json")); !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("a binding was written: %v", err)
			}

			restore()
			if _, err := Bind(main, "s2", wt); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(main, ".crw", "sources", "s2.json")
			before, err := os.ReadFile(path)
			must(t, err)
			fail()
			if got, err := Resolve(main, "s2"); err != unknownNative {
				t.Fatalf("resolve with the native repository unknown: %q, %v", got, err)
			}
			if _, err := Bind(main, "s2", wt); err != unknownNative {
				t.Fatalf("rebind with the native repository unknown: %v", err)
			}
			if res := CheckBound(main, "s2"); res.OK {
				t.Fatalf("the gate passed with the native repository unknown: %+v", res)
			}
			if after, err := os.ReadFile(path); err != nil || !bytes.Equal(before, after) {
				t.Fatalf("binding changed: %v", err)
			}
		})
	}
	base := hermetic(t) // a native directory outside any repository binds a repository root
	plain := filepath.Join(base, "plain")
	must(t, os.Mkdir(plain, 0o755))
	other := newRepo(t, base, "other")
	if root, err := Bind(plain, "s1", other); err != nil || root != other {
		t.Fatalf("non-Git native directory: %q, %v", root, err)
	}
}

// CRW-1135 criterion 2 with the real git error: a native linked worktree whose .git file points at a Git directory
// that cannot be read makes git answer "not a git repository: <gitdir>" (exit 128). That is a repository git could
// not read, not a directory in no repository: binding another repository is refused, an existing binding keeps
// its bytes, and resolving and the gate refuse it.
func TestNativeUnreadableGitdirIsUnknownNotOutsideARepository(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("permissions do not bind root")
	}
	base := hermetic(t)
	main := newRepo(t, base, "main")
	other := newRepo(t, base, "other")
	native := filepath.Join(base, "native")
	gitIn(t, main, "worktree", "add", "-q", "-b", "native", native)
	if _, err := Bind(native, "s2", main); err != nil { // a binding made while the repository is readable
		t.Fatal(err)
	}
	path := filepath.Join(native, ".crw", "sources", "s2.json")
	before, err := os.ReadFile(path)
	must(t, err)

	gitdir := filepath.Join(main, ".git")
	must(t, os.Chmod(gitdir, 0))
	t.Cleanup(func() { _ = os.Chmod(gitdir, 0o755) })

	if root, err := Bind(native, "s1", other); err != unknownNative {
		t.Fatalf("bind from a native worktree whose Git directory is unreadable: %q, %v", root, err)
	}
	if _, err := os.Lstat(filepath.Join(native, ".crw", "sources", "s1.json")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("a binding was written: %v", err)
	}
	if got, err := Resolve(native, "s2"); err != unknownNative {
		t.Fatalf("resolve: %q, %v", got, err)
	}
	if res := CheckBound(native, "s2"); res.OK {
		t.Fatalf("the gate passed: %+v", res)
	}
	must(t, os.Chmod(gitdir, 0o755))
	if after, err := os.ReadFile(path); err != nil || !bytes.Equal(before, after) {
		t.Fatalf("binding changed: %v", err)
	}
}

// CRW-1135 criterion 2 in an ordinary repository: when the native directory's own .git (or what git needs inside
// it: HEAD, objects, refs) cannot be read, git skips it and answers with its discovery message, "not a git
// repository (or any of the parent directories)", exactly as for a directory in no repository. The marker it
// could not read makes the answer unknown: binding another repository is refused and writes nothing, an existing
// binding keeps its bytes, and resolving and the gate refuse it, from the root and from a subdirectory.
func TestNativeUnreadableOrdinaryGitdirIsUnknownNotOutsideARepository(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("permissions do not bind root")
	}
	for _, blocked := range []string{".git", ".git/HEAD", ".git/objects", ".git/refs"} {
		for _, sub := range []string{"", "sub"} {
			t.Run(blocked+"/"+sub, func(t *testing.T) {
				base := hermetic(t)
				main := newRepo(t, base, "main")
				other := newRepo(t, base, "other")
				wt := filepath.Join(base, "wt")
				gitIn(t, main, "worktree", "add", "-q", "-b", "work", wt)
				native := filepath.Join(main, sub)
				must(t, os.MkdirAll(native, 0o755))
				if _, err := Bind(native, "s2", wt); err != nil { // made while the repository is readable
					t.Fatal(err)
				}
				path := filepath.Join(native, ".crw", "sources", "s2.json")
				before, err := os.ReadFile(path)
				must(t, err)

				target := filepath.Join(main, blocked)
				info, err := os.Stat(target)
				must(t, err)
				must(t, os.Chmod(target, 0))
				t.Cleanup(func() { _ = os.Chmod(target, info.Mode().Perm()) })

				if root, err := Bind(native, "s1", other); err != unknownNative {
					t.Fatalf("bind from a repository whose %s is unreadable: %q, %v", blocked, root, err)
				}
				if _, err := os.Lstat(filepath.Join(native, ".crw", "sources", "s1.json")); !errors.Is(err, fs.ErrNotExist) {
					t.Fatalf("a binding was written: %v", err)
				}
				if got, err := Resolve(native, "s2"); err != unknownNative {
					t.Fatalf("resolve: %q, %v", got, err)
				}
				if res := CheckBound(native, "s2"); res.OK {
					t.Fatalf("the gate passed: %+v", res)
				}
				must(t, os.Chmod(target, info.Mode().Perm()))
				if after, err := os.ReadFile(path); err != nil || !bytes.Equal(before, after) {
					t.Fatalf("binding changed: %v", err)
				}
			})
		}
	}
}

// A .git that git can read and rejects (the empty directory a sandbox leaves behind, a stray HEAD-less one) is not
// a repository to git, and not one here: a native directory below it is outside any repository and binds.
func TestNativeBelowReadableNonRepositoryGitMarkerStillBinds(t *testing.T) {
	base := hermetic(t)
	plain := filepath.Join(base, "plain")
	native := filepath.Join(plain, "native")
	must(t, os.MkdirAll(filepath.Join(plain, ".git"), 0o755))
	must(t, os.MkdirAll(filepath.Join(native, ".git", "objects"), 0o755))
	other := newRepo(t, base, "other")
	if root, err := Bind(native, "s1", other); err != nil || root != other {
		t.Fatalf("native directory below non-repository .git markers: %q, %v", root, err)
	}
}
