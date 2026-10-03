package bundle

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

type repository struct {
	t   *testing.T
	dir string
	env []string
}

func newRepository(t *testing.T) repository {
	t.Helper()
	r := repository{t: t, dir: t.TempDir()}
	for _, v := range os.Environ() {
		if !strings.HasPrefix(v, "GIT_") && !strings.HasPrefix(v, "HOME=") && !strings.HasPrefix(v, "XDG_CONFIG_HOME=") {
			r.env = append(r.env, v)
		}
	}
	r.env = append(r.env, "HOME="+r.dir, "XDG_CONFIG_HOME="+r.dir, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull,
		"GIT_AUTHOR_NAME=Fixture", "GIT_AUTHOR_EMAIL=fixture@example.invalid", "GIT_COMMITTER_NAME=Fixture", "GIT_COMMITTER_EMAIL=fixture@example.invalid",
		"GIT_AUTHOR_DATE=2000-01-01T00:00:00Z", "GIT_COMMITTER_DATE=2000-01-01T00:00:00Z")
	r.git("init", "-q", "--initial-branch=base")
	return r
}

func (r repository) git(args ...string) string {
	r.t.Helper()
	c := exec.Command("git", args...)
	c.Dir, c.Env = r.dir, r.env
	b, err := c.CombinedOutput()
	if err != nil {
		r.t.Fatalf("git %v: %v: %s", args, err, b)
	}
	return string(b)
}

func (r repository) write(path, text string) {
	r.t.Helper()
	name := filepath.Join(r.dir, path)
	if err := os.MkdirAll(filepath.Dir(name), 0700); err != nil {
		r.t.Fatal(err)
	}
	if err := os.WriteFile(name, []byte(text), 0600); err != nil {
		r.t.Fatal(err)
	}
}

func (r repository) commit() string {
	r.t.Helper()
	r.git("add", ".")
	r.git("commit", "-qm", "fixture", "--allow-empty")
	return strings.TrimSpace(r.git("rev-parse", "HEAD"))
}
