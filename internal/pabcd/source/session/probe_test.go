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
			if root, err := Bind(main, "s1", other); err == nil {
				t.Fatalf("bound %q from a native directory whose repository is unknown", root)
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
			if got, err := Resolve(main, "s2"); err == nil {
				t.Fatalf("resolved %q with the native repository unknown", got)
			}
			if _, err := Bind(main, "s2", wt); err == nil {
				t.Fatal("rebinding passed with the native repository unknown")
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
