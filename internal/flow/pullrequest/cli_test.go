package pullrequest

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/tracking"
	"github.com/thisisjun786/codex-relay-workflow/internal/tracking/trackingfake"
)

// gitWorld is a checkout with a milestone integration branch and a local bare remote, the place `crw flow pr open --push` pushes to.
type gitWorld struct {
	t              *testing.T
	checkout, bare string
	env            []string
}

func newGitWorld(t *testing.T) *gitWorld {
	t.Helper()
	root := t.TempDir()
	home := filepath.Join(root, "home")
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}
	g := &gitWorld{t: t, checkout: filepath.Join(root, "checkout"), bare: filepath.Join(root, "remote.git"),
		env: append(os.Environ(), "HOME="+home, "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0",
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.invalid", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.invalid")}
	g.git(root, "init", "-q", "--bare", "-b", "dev", g.bare)
	g.git(root, "init", "-q", "-b", "dev", g.checkout)
	if err := os.WriteFile(filepath.Join(g.checkout, "README.md"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	g.git(g.checkout, "add", "-A")
	g.git(g.checkout, "commit", "-q", "-m", "base")
	g.git(g.checkout, "remote", "add", "origin", g.bare)
	g.git(g.checkout, "push", "-q", "origin", "dev")
	g.git(g.checkout, "checkout", "-q", "-b", "crw/m1")
	if err := os.WriteFile(filepath.Join(g.checkout, "a.txt"), []byte("a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	g.git(g.checkout, "add", "-A")
	g.git(g.checkout, "commit", "-q", "-m", "landed")
	g.git(g.checkout, "checkout", "-q", "dev")
	return g
}

func (g *gitWorld) git(dir string, args ...string) string {
	g.t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = g.env
	out, err := cmd.CombinedOutput()
	if err != nil {
		g.t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// remoteSHA is the commit a branch of the bare remote is at, "" when it has none.
func (g *gitWorld) remoteSHA(branch string) string {
	cmd := exec.Command("git", "-C", g.bare, "rev-parse", "--verify", "-q", "refs/heads/"+branch)
	cmd.Env = g.env
	out, _ := cmd.Output()
	return strings.TrimSpace(string(out))
}

func deps(t *testing.T, fake *trackingfake.Server, g *gitWorld) *Deps {
	t.Helper()
	d := &Deps{NewPullRequests: func(repo string) (tracking.PullRequests, error) {
		return tracking.NewGitHub(repo, tracking.StaticToken(trackingfake.Token), tracking.WithBaseURL(fake.URL))
	}}
	if g != nil {
		d.Env = g.env
	}
	return d
}

func run(t *testing.T, d *Deps, args ...string) (int, string, string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code := RunWith(context.Background(), d, args, &out, &errOut)
	return code, out.String(), errOut.String()
}

// TestOpenPushesTheMilestoneBranchAndOpensOnePullRequestCarryingItsIssues (CRW-1355 c2): the project session opens the dev-ward pull
// request of its milestone branch with one command: the branch is pushed to the remote fast-forward only, the pull request names the
// issues it carries, and the answer is the pull request with the head it carries.
func TestOpenPushesTheMilestoneBranchAndOpensOnePullRequestCarryingItsIssues(t *testing.T) {
	g := newGitWorld(t)
	fake := trackingfake.New(t, "o/r")
	fake.HeadSHA = g.remoteSHA
	tip := g.git(g.checkout, "rev-parse", "crw/m1")
	code, out, errOut := run(t, deps(t, fake, g), "open", "--repo", "o/r", "--head", "crw/m1", "--base", "dev", "--title", "milestone M1",
		"--body", "Two issues of M1.", "--issue", "o/r#10", "--issue", "o/r#11,o/r#10", "--push", g.checkout)
	if code != 0 {
		t.Fatalf("open: exit %d\n%s%s", code, out, errOut)
	}
	if got := g.remoteSHA("crw/m1"); got != tip {
		t.Fatalf("the remote's crw/m1 is %q, want the local tip %s", got, tip)
	}
	var ans Answer
	if err := json.Unmarshal([]byte(out), &ans); err != nil {
		t.Fatalf("answer %q: %v", out, err)
	}
	pr := ans.PullRequest
	if pr.Ref != "o/r#1" || pr.Head != tip || pr.HeadRef != "crw/m1" || pr.Base != "dev" || pr.State != "open" || ans.Pushed != tip ||
		strings.Join(pr.Issues, ",") != "o/r#10,o/r#11" {
		t.Errorf("answer %+v", ans)
	}
	pulls := fake.Pulls()
	if len(pulls) != 1 || pulls[1].Body != "Two issues of M1.\n\nCarries: o/r#10, o/r#11" || pulls[1].Head != "crw/m1" || pulls[1].Base != "dev" {
		t.Errorf("pull requests %+v", pulls)
	}
}

// TestOpenRefusesWhatCannotBeOpened: without --push nothing is pushed; a push the remote refuses (not a fast-forward) opens
// no pull request; an input the adapter cannot send is a usage error (2).
func TestOpenRefusesWhatCannotBeOpened(t *testing.T) {
	g := newGitWorld(t)
	fake := trackingfake.New(t, "o/r")
	fake.HeadSHA = g.remoteSHA
	if code, out, _ := run(t, deps(t, fake, g), "open", "--repo", "o/r", "--head", "crw/m1", "--title", "t", "--issue", "o/r#1"); code != 0 {
		t.Fatalf("open without --push: exit %d %s", code, out)
	}
	if got := g.remoteSHA("crw/m1"); got != "" {
		t.Errorf("open without --push pushed %s", got)
	}
	// the remote's branch moved on another way: the push is not a fast-forward and is refused, so no second pull request is opened
	g.git(g.checkout, "push", "-q", "origin", "dev:refs/heads/crw/m2")
	g.git(g.checkout, "branch", "crw/m2", "crw/m1")
	other := filepath.Join(t.TempDir(), "other")
	g.git(filepath.Dir(other), "clone", "-q", "-b", "crw/m2", g.bare, other)
	if err := os.WriteFile(filepath.Join(other, "b.txt"), []byte("b\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	g.git(other, "add", "-A")
	g.git(other, "commit", "-q", "-m", "elsewhere")
	g.git(other, "push", "-q", "origin", "crw/m2")
	code, _, errOut := run(t, deps(t, fake, g), "open", "--repo", "o/r", "--head", "crw/m2", "--title", "t", "--issue", "o/r#2", "--push", g.checkout)
	if code != 1 || !strings.Contains(errOut, "push") {
		t.Errorf("open over a refused push: exit %d %q", code, errOut)
	}
	if n := len(fake.Pulls()); n != 1 {
		t.Errorf("%d pull requests after a refused push, want the first one only", n)
	}
	for name, args := range map[string][]string{
		"no repo":             {"open", "--head", "crw/m1", "--title", "t", "--issue", "o/r#1"},
		"no issue":            {"open", "--repo", "o/r", "--head", "crw/m1", "--title", "t"},
		"bad issue":           {"open", "--repo", "o/r", "--head", "crw/m1", "--title", "t", "--issue", "#1"},
		"head is base":        {"open", "--repo", "o/r", "--head", "dev", "--title", "t", "--issue", "o/r#1"},
		"body and file":       {"open", "--repo", "o/r", "--head", "crw/m1", "--title", "t", "--issue", "o/r#1", "--body", "x", "--body-file", "y"},
		"no command":          {},
		"branch no milestone": {"branch", "--checkout", g.checkout},
		"branch no checkout":  {"branch", "--milestone", "m1"},
		"other command":       {"merge"},
		"show no ref":         {"show"},
		"show bad ref":        {"show", "--pr", "o/r"},
		// --remote is a remote name: an option-shaped or path-shaped value is refused before anything is pushed
		"remote starts with a dash": {"open", "--repo", "o/r", "--head", "crw/m1", "--title", "t", "--issue", "o/r#1", "--push", g.checkout, "--remote", "--receive-pack=x"},
		"remote is a path":          {"open", "--repo", "o/r", "--head", "crw/m1", "--title", "t", "--issue", "o/r#1", "--push", g.checkout, "--remote", "/tmp/x"},
	} {
		if code, _, errOut := run(t, deps(t, fake, g), args...); code != 2 || !strings.Contains(errOut, "usage:") {
			t.Errorf("%s: exit %d %q, want 2 with the usage", name, code, errOut)
		}
	}
	if n := len(fake.Pulls()); n != 1 {
		t.Errorf("%d pull requests after the refused inputs, want the first one only", n)
	}
}

// TestPushBranchPutsDoubleDashBeforeTheRemote: the push's argv ends the options before the remote, so a remote that reads as an
// option is never one. A recording git first on PATH keeps the argv; git's own refusal of the pathname is not the oracle.
func TestPushBranchPutsDoubleDashBeforeTheRemote(t *testing.T) {
	g := newGitWorld(t)
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
	// exec resolves "git" in the process PATH, so the wrapper goes there (and pushEnv keeps PATH for git's own children)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	tip, _ := pushBranch(context.Background(), g.env, g.checkout, "--receive-pack=x", "crw/m1")
	data, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	var push string
	for _, line := range strings.Split(string(data), "\n") {
		if strings.Contains(line, " push ") {
			push = line
		}
	}
	tipSHA := strings.TrimSpace(g.git(g.checkout, "rev-parse", "refs/heads/crw/m1"))
	want := "-C " + g.checkout + " push --porcelain -- --receive-pack=x " + tipSHA + ":refs/heads/crw/m1"
	if push != want {
		t.Errorf("push argv\n got %q\nwant %q (returned tip %q)", push, want, tip)
	}
}

// TestShowReadsTheStateAndChecksOfAPullRequest (CRW-1355 c2): `crw flow pr show` reads a pull request's state, head, base and carried
// issues, and with --checks the judgment of its head's checks.
func TestShowReadsTheStateAndChecksOfAPullRequest(t *testing.T) {
	g := newGitWorld(t)
	fake := trackingfake.New(t, "o/r")
	fake.HeadSHA = g.remoteSHA
	if code, out, errOut := run(t, deps(t, fake, g), "open", "--repo", "o/r", "--head", "crw/m1", "--title", "t", "--issue", "o/r#7", "--push", g.checkout); code != 0 {
		t.Fatalf("open: %d %s %s", code, out, errOut)
	}
	fake.SetPull(1, func(p *trackingfake.Pull) { p.State, p.Merged = "closed", true })
	code, out, errOut := run(t, deps(t, fake, g), "show", "--pr", "o/r#1", "--checks")
	if code != 0 {
		t.Fatalf("show: %d %s", code, errOut)
	}
	var ans Answer
	if err := json.Unmarshal([]byte(out), &ans); err != nil {
		t.Fatal(err)
	}
	if ans.PullRequest.State != "closed" || !ans.PullRequest.Merged || ans.PullRequest.Head != g.remoteSHA("crw/m1") || ans.Checks == nil || ans.Checks.State != "none" {
		t.Errorf("show answer %s", out)
	}
	if code, _, errOut := run(t, deps(t, fake, g), "show", "--pr", "o/r#9"); code != 1 || !strings.Contains(strings.ToLower(errOut), "not found") {
		t.Errorf("show of a pull request that does not exist: %d %q", code, errOut)
	}
	for _, r := range fake.Requests() {
		if strings.HasPrefix(r, "POST") && !strings.HasSuffix(r, "/pulls") {
			t.Errorf("show wrote %s", r)
		}
	}
}

// otherRepository is a second repository with its own crw/m1 and its own remote, the one a GIT_DIR inherited from the process
// environment would name; it answers its .git directory and its remote.
func (g *gitWorld) otherRepository() (gitDir, bare string) {
	g.t.Helper()
	root := g.t.TempDir()
	repo, bare := filepath.Join(root, "repo"), filepath.Join(root, "other.git")
	g.git(root, "init", "-q", "--bare", "-b", "dev", bare)
	g.git(root, "init", "-q", "-b", "crw/m1", repo)
	if err := os.WriteFile(filepath.Join(repo, "other.txt"), []byte("other\n"), 0o644); err != nil {
		g.t.Fatal(err)
	}
	g.git(repo, "add", "-A")
	g.git(repo, "commit", "-q", "-m", "other")
	g.git(repo, "remote", "add", "origin", bare)
	return filepath.Join(repo, ".git"), bare
}

func bareSHA(t *testing.T, env []string, bare, branch string) string {
	t.Helper()
	cmd := exec.Command("git", "-C", bare, "rev-parse", "--verify", "-q", "refs/heads/"+branch)
	cmd.Env = env
	out, _ := cmd.Output()
	return strings.TrimSpace(string(out))
}

// TestOpenPushesTheCheckoutItNamesWhateverGitDirTheEnvironmentHolds (CRW-1355 fix 1): `crw flow pr open --push DIR` pushes the
// branch of DIR: a GIT_DIR, GIT_WORK_TREE, GIT_OBJECT_DIRECTORY or GIT_NAMESPACE of the environment it runs in does not route the
// rev-parse or the push to another repository, with the dependencies' environment and with the process's (Default, nil Env).
func TestOpenPushesTheCheckoutItNamesWhateverGitDirTheEnvironmentHolds(t *testing.T) {
	for _, viaProcess := range []bool{false, true} {
		name := "deps env"
		if viaProcess {
			name = "process env"
		}
		t.Run(name, func(t *testing.T) {
			g := newGitWorld(t)
			otherDir, otherBare := g.otherRepository()
			fake := trackingfake.New(t, "o/r")
			fake.HeadSHA = g.remoteSHA
			tip := g.git(g.checkout, "rev-parse", "crw/m1")
			routing := []string{"GIT_DIR=" + otherDir, "GIT_WORK_TREE=" + filepath.Dir(otherDir), "GIT_OBJECT_DIRECTORY=" + filepath.Join(otherDir, "objects"), "GIT_NAMESPACE=elsewhere"}
			d := deps(t, fake, g)
			if viaProcess {
				for _, kv := range g.env {
					if k, v, ok := strings.Cut(kv, "="); ok && strings.HasPrefix(k, "GIT_") || k == "HOME" {
						t.Setenv(k, v)
					}
				}
				for _, kv := range routing {
					k, v, _ := strings.Cut(kv, "=")
					t.Setenv(k, v)
				}
				d.Env = nil
			} else {
				d.Env = append(append([]string(nil), g.env...), routing...)
			}
			code, out, errOut := run(t, d, "open", "--repo", "o/r", "--head", "crw/m1", "--title", "t", "--issue", "o/r#1", "--push", g.checkout)
			if code != 0 {
				t.Fatalf("open: exit %d\n%s%s", code, out, errOut)
			}
			if got := g.remoteSHA("crw/m1"); got != tip {
				t.Errorf("the checkout's remote has crw/m1 at %q, want the checkout's tip %s", got, tip)
			}
			if got := bareSHA(t, g.env, otherBare, "crw/m1"); got != "" {
				t.Errorf("the push went to the other repository's remote (crw/m1 at %s)", got)
			}
			var ans Answer
			if err := json.Unmarshal([]byte(out), &ans); err != nil || ans.Pushed != tip {
				t.Errorf("answer %s (%v), want pushed %s", out, err, tip)
			}
		})
	}
}

// TestUsageNamesTheBranchCommand (CRW-1440): the usage, the help and the two command-list messages name `branch` next to open and show.
func TestUsageNamesTheBranchCommand(t *testing.T) {
	want := "crw flow pr branch --checkout DIR --milestone M [--base BRANCH] [--name BRANCH]"
	if !strings.Contains(Usage, want) {
		t.Errorf("Usage does not name %q:\n%s", want, Usage)
	}
	code, out, _ := run(t, deps(t, nil, nil), "--help")
	if code != 0 || !strings.Contains(out, "  branch ") {
		t.Errorf("help: exit %d, no branch paragraph:\n%s", code, out)
	}
	for name, args := range map[string][]string{"no command": {}, "other command": {"merge"}} {
		if code, _, errOut := run(t, deps(t, nil, nil), args...); code != 2 || !strings.Contains(errOut, "open, show or branch") {
			t.Errorf("%s: exit %d %q, want 2 naming open, show or branch", name, code, errOut)
		}
	}
}
