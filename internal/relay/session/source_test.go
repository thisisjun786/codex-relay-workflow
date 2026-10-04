package session

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func gitIn(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func linkedSource(t *testing.T, f fixture) string {
	t.Helper()
	for _, entry := range os.Environ() {
		if name, _, _ := strings.Cut(entry, "="); strings.HasPrefix(name, "GIT_") {
			t.Setenv(name, "")
			must(t, os.Unsetenv(name))
		}
	}
	for name, value := range map[string]string{"GIT_CONFIG_GLOBAL": os.DevNull, "GIT_CONFIG_NOSYSTEM": "1", "GIT_CEILING_DIRECTORIES": f.root,
		"GIT_AUTHOR_NAME": "fixture", "GIT_AUTHOR_EMAIL": "fixture@example.invalid", "GIT_COMMITTER_NAME": "fixture", "GIT_COMMITTER_EMAIL": "fixture@example.invalid"} {
		t.Setenv(name, value)
	}
	gitIn(t, f.cwd, "init", "-q", "-b", "main")
	f.write(t, filepath.Join(f.cwd, "tracked.txt"), "baseline\n")
	gitIn(t, f.cwd, "add", "tracked.txt")
	gitIn(t, f.cwd, "commit", "-qm", "fixture")
	target := filepath.Join(f.root, "source")
	gitIn(t, f.cwd, "worktree", "add", "-q", "-b", "source", target, "HEAD")
	return target
}

func TestSourceNeedsStateAndRefusesLateBinding(t *testing.T) {
	f := newFixture(t, nil)
	target := linkedSource(t, f)
	opts := Options{Command: "source", SourceRoot: target, JSON: true}
	if r := Run(opts, f.cwd, f.lookup); r.Code != 1 || field(t, r, "error") != "Bind session state before binding a source worktree." {
		t.Fatalf("%+v", r)
	}
	f.write(t, f.path, `{"sessionId":"`+child+`","phase":"B"}`)
	if r := Run(opts, f.cwd, f.lookup); r.Code != 1 || field(t, r, "error") != "SOURCE-ROOT: Bind the source before B. Preserve the old baseline and re-plan before binding." {
		t.Fatalf("%+v", r)
	}
}

func TestSourceIsImmutableReportsDirtyIdentityAndFailsClosedWhenMissing(t *testing.T) {
	f := newFixture(t, nil)
	target := linkedSource(t, f)
	if r := f.run("bind"); r.Code != 0 {
		t.Fatalf("bind: %+v", r)
	}
	f.write(t, filepath.Join(target, "tracked.txt"), "changed\n")
	opts := Options{Command: "source", SourceRoot: target, JSON: true}
	r := Run(opts, f.cwd, f.lookup)
	if r.Code != 0 || field(t, r, "sourceCwd") != target {
		t.Fatalf("source: %+v", r)
	}
	id := Result{Out: field(t, r, "sourceIdentity")}
	if field(t, id, "kind") != "resolved" || field(t, id, "dirty") != true || field(t, id, "treeHash") == nil || field(t, id, "sourceRoot") != target {
		t.Fatalf("dirty source identity: %+v", id)
	}
	binding := filepath.Join(f.cwd, ".crw", "sources", child+".json")
	before, err := os.ReadFile(binding)
	must(t, err)
	if r := Run(opts, f.cwd, f.lookup); r.Code != 0 {
		t.Fatalf("repeat: %+v", r)
	}
	after, err := os.ReadFile(binding)
	must(t, err)
	if string(before) != string(after) {
		t.Fatal("repeat source binding replaced bytes")
	}
	plain := Run(Options{Command: "current"}, f.cwd, f.lookup)
	if plain.Code != 0 || !strings.Contains(plain.Out.(string), "sourceIdentity: [object Object]") {
		t.Fatalf("oracle text-mode defect changed: %+v", plain)
	}
	other := filepath.Join(f.root, "other-source")
	gitIn(t, f.cwd, "worktree", "add", "-q", "-b", "other", other, "HEAD")
	opts.SourceRoot = other
	if r := Run(opts, f.cwd, f.lookup); r.Code != 1 || field(t, r, "error") != "SOURCE-ROOT: Source binding is immutable; use a new session for a different worktree." {
		t.Fatalf("rebind: %+v", r)
	}
	f.write(t, f.path, `{"sessionId":"`+child+`","phase":"B","boundSourceRoot":"`+target+`"}`)
	must(t, os.Remove(binding))
	if r := f.run("current"); r.Code != 1 || !strings.Contains(field(t, r, "error").(string), "Source binding is missing for the pinned worktree") {
		t.Fatalf("missing binding: %+v", r)
	}
}
