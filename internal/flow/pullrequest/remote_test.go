package pullrequest

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/tracking/trackingfake"
)

// CRW-1413 pingpong item 3: --remote with --push is any remote the checkout configures (git remote), team/review and foo@bar
// included; a value that starts with a dash and anything not configured (a URL, a path, an unknown name) is refused with exit 2
// before anything is pushed. The push keeps its `--` before the remote.

// recordGit puts a git first on PATH that logs its argv and then runs the real git; it answers the log's path.
func recordGit(t *testing.T) string {
	t.Helper()
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Skip("no git")
	}
	bin := t.TempDir()
	log := filepath.Join(bin, "argv.log")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> " + log + "\nexec " + realGit + " \"$@\"\n"
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return log
}

func pushLines(log string) []string {
	data, _ := os.ReadFile(log)
	var out []string
	for _, line := range strings.Split(string(data), "\n") {
		if strings.Contains(line, " push ") {
			out = append(out, line)
		}
	}
	return out
}

func openArgs(g *gitWorld, remote string) []string {
	return []string{"open", "--repo", "o/r", "--head", "crw/m1", "--title", "t", "--issue", "o/r#1", "--push", g.checkout, "--remote", remote}
}

// TestPushTakesAnyConfiguredRemoteName: team/review and foo@bar are configured remotes of the checkout; the branch is pushed to
// each with `-- <remote>` and a pull request is opened.
func TestPushTakesAnyConfiguredRemoteName(t *testing.T) {
	for _, name := range []string{"team/review", "foo@bar"} {
		t.Run(name, func(t *testing.T) {
			g := newGitWorld(t)
			g.git(g.checkout, "remote", "add", name, g.bare)
			fake := trackingfake.New(t, "o/r")
			fake.HeadSHA = g.remoteSHA
			log := recordGit(t)
			code, out, errOut := run(t, deps(t, fake, g), openArgs(g, name)...)
			if code != 0 {
				t.Fatalf("open --remote %s: exit %d %s %s", name, code, out, errOut)
			}
			tip := g.git(g.checkout, "rev-parse", "refs/heads/crw/m1")
			if got := g.remoteSHA("crw/m1"); got != tip {
				t.Errorf("the remote's crw/m1 is %q, want %s", got, tip)
			}
			want := "-C " + g.checkout + " push --porcelain -- " + name + " " + tip + ":refs/heads/crw/m1"
			if p := pushLines(log); len(p) != 1 || p[0] != want {
				t.Errorf("push argv %q, want %q", p, want)
			}
			if n := len(fake.Pulls()); n != 1 {
				t.Errorf("%d pull requests, want 1", n)
			}
		})
	}
}

// TestPushRefusesWhatIsNotAConfiguredRemote: a name the checkout does not list, a URL, a path and a value starting with a dash are
// refused with exit 2 before any push runs and before a pull request is opened.
func TestPushRefusesWhatIsNotAConfiguredRemote(t *testing.T) {
	for name, remote := range map[string]string{
		"unconfigured name": "nope",
		"prefix of a name":  "team",
		"url":               "https://example.invalid/o/r.git",
		"path":              "/tmp/x",
		"leading dash":      "--receive-pack=x",
		"leading dash name": "-origin",
		"empty":             "",
	} {
		t.Run(name, func(t *testing.T) {
			g := newGitWorld(t)
			g.git(g.checkout, "remote", "add", "team/review", g.bare)
			fake := trackingfake.New(t, "o/r")
			fake.HeadSHA = g.remoteSHA
			log := recordGit(t)
			code, _, errOut := run(t, deps(t, fake, g), openArgs(g, remote)...)
			if code != 2 || !strings.Contains(errOut, "usage:") {
				t.Errorf("exit %d %q, want 2 with the usage", code, errOut)
			}
			if p := pushLines(log); len(p) != 0 {
				t.Errorf("a push ran: %q", p)
			}
			if got := g.remoteSHA("crw/m1"); got != "" {
				t.Errorf("the remote has crw/m1 at %s", got)
			}
			if n := len(fake.Pulls()); n != 0 {
				t.Errorf("%d pull requests opened", n)
			}
		})
	}
}
