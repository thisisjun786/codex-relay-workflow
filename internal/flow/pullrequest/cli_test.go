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
		"no repo":       {"open", "--head", "crw/m1", "--title", "t", "--issue", "o/r#1"},
		"no issue":      {"open", "--repo", "o/r", "--head", "crw/m1", "--title", "t"},
		"bad issue":     {"open", "--repo", "o/r", "--head", "crw/m1", "--title", "t", "--issue", "#1"},
		"head is base":  {"open", "--repo", "o/r", "--head", "dev", "--title", "t", "--issue", "o/r#1"},
		"body and file": {"open", "--repo", "o/r", "--head", "crw/m1", "--title", "t", "--issue", "o/r#1", "--body", "x", "--body-file", "y"},
		"no command":    {},
		"other command": {"merge"},
		"show no ref":   {"show"},
		"show bad ref":  {"show", "--pr", "o/r"},
	} {
		if code, _, errOut := run(t, deps(t, fake, g), args...); code != 2 || !strings.Contains(errOut, "usage:") {
			t.Errorf("%s: exit %d %q, want 2 with the usage", name, code, errOut)
		}
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
