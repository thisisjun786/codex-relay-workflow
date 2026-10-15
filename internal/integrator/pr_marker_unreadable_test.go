package integrator

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// CRW-1491 ping-pong round 2: a marker commit that exists but that git cannot read is not an absent commit. "git rev-parse -q
// --verify <sha>^{commit}" exits 1 for it too (an empty loose object), with the object error on stderr; only exit 1 with nothing on
// stderr means the commit is absent.

// markerRepo is a real scratch repository: base is refs/remotes/origin/dev, and head is a commit outside it (the head a pushing
// marker names). Every git call runs with no global or system configuration.
type markerRepo struct {
	t    *testing.T
	dir  string
	base string
	head string
}

func newMarkerRepo(t *testing.T) *markerRepo {
	t.Helper()
	m := &markerRepo{t: t, dir: t.TempDir()}
	m.git("init", "-q", ".")
	m.git("commit", "-q", "--allow-empty", "-m", "base")
	m.base = m.git("rev-parse", "HEAD")
	m.git("update-ref", "refs/remotes/origin/dev", m.base)
	m.git("commit", "-q", "--allow-empty", "-m", "head")
	m.head = m.git("rev-parse", "HEAD")
	return m
}

func (m *markerRepo) env() []string {
	return append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.invalid", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.invalid")
}

func (m *markerRepo) git(args ...string) string {
	m.t.Helper()
	cmd := exec.Command("git", append([]string{"-C", m.dir}, args...)...)
	cmd.Env = m.env()
	out, err := cmd.CombinedOutput()
	if err != nil {
		m.t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// object is the loose object file of sha, made writable.
func (m *markerRepo) object(sha string) string {
	m.t.Helper()
	p := filepath.Join(m.dir, ".git", "objects", sha[:2], sha[2:])
	if err := os.Chmod(p, 0o644); err != nil {
		m.t.Fatalf("the head is not a loose object: %v", err)
	}
	return p
}

func (m *markerRepo) runner() *Runner {
	cfg := Config{Git: "git", Checkout: m.dir, Remote: "origin", Branch: "dev"}
	return &Runner{Cfg: cfg, Exec: OSExec, Env: m.env()}
}

// TestAMarkerCommitGitCannotReadHoldsTheEntry: the marker's commit object is corrupt in the checkout (emptied, or overwritten). git
// cannot say whether it is in the branch, so markerInBranch is an error naming the object error and pushedRun, which recovery asks,
// fails with it: the entry is held, the marker stays, and no "not recorded" landing is written from a missing run.
func TestAMarkerCommitGitCannotReadHoldsTheEntry(t *testing.T) {
	corruptions := map[string]func(t *testing.T, path string){
		"emptied": func(t *testing.T, path string) {
			if err := os.Truncate(path, 0); err != nil {
				t.Fatal(err)
			}
		},
		"overwritten": func(t *testing.T, path string) {
			if err := os.WriteFile(path, []byte("not a zlib stream\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		},
	}
	for name, corrupt := range corruptions {
		t.Run(name, func(t *testing.T) {
			m := newMarkerRepo(t)
			corrupt(t, m.object(m.head))
			r := m.runner()

			in, err := r.markerInBranch(context.Background(), m.head)
			if err == nil || in {
				t.Fatalf("markerInBranch = %v, %v; want an error", in, err)
			}

			r.Cfg.RunsDir = t.TempDir()
			marker := Summary{Tag: "20260101-000000", Base: m.base, Head: m.head, Merged: "crw-1=" + m.head, Result: resultPushing}
			data, jerr := json.Marshal(marker)
			if jerr != nil {
				t.Fatal(jerr)
			}
			if werr := os.WriteFile(filepath.Join(r.Cfg.RunsDir, marker.Tag+".summary.json"), data, 0o664); werr != nil {
				t.Fatal(werr)
			}
			run, perr := r.pushedRun(context.Background(), Entry{Node: "crw-1", Head: m.head})
			if perr == nil || run != nil {
				t.Fatalf("pushedRun = %+v, %v; want an error that holds the entry", run, perr)
			}
			if !strings.Contains(perr.Error(), "pushing marker of run "+marker.Tag) {
				t.Errorf("the hold does not name the marker: %v", perr)
			}
		})
	}
}

// TestAMarkerCommitTheCheckoutDoesNotHaveIsNotInTheBranch: a commit that is truly absent (exit 1, nothing on stderr) was never
// pushed, so markerInBranch is (false, nil); a readable commit outside the branch is (false, nil) too, and one inside it is true.
func TestAMarkerCommitTheCheckoutDoesNotHaveIsNotInTheBranch(t *testing.T) {
	m := newMarkerRepo(t)
	r := m.runner()
	for _, c := range []struct {
		name, sha string
		want      bool
	}{
		{"absent", strings.Repeat("ab", 20), false},
		{"outside the branch", m.head, false},
		{"inside the branch", m.base, true},
	} {
		in, err := r.markerInBranch(context.Background(), c.sha)
		if err != nil || in != c.want {
			t.Errorf("%s: markerInBranch = %v, %v; want %v, nil", c.name, in, err, c.want)
		}
	}
}
