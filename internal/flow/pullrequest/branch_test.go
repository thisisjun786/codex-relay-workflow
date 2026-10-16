package pullrequest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// CRW-1440: `crw flow pr branch` creates the bundle's integration branch at the base tip, once, and never moves it. The tests build a
// temp repository with commits on dev (newGitWorld: dev, and crw/m1 one commit ahead of dev) under an isolated git environment.

// branchRun runs `crw flow pr branch` on the world's checkout and decodes the answer when the exit is 0.
func (g *gitWorld) branchRun(t *testing.T, extra ...string) (int, BranchAnswer, string) {
	t.Helper()
	args := append([]string{"branch", "--checkout", g.checkout}, extra...)
	code, out, errOut := run(t, deps(t, nil, g), args...)
	var ans BranchAnswer
	if code == 0 {
		if err := json.Unmarshal([]byte(out), &ans); err != nil {
			t.Fatalf("the answer %q is not a BranchAnswer: %v", out, err)
		}
	}
	return code, ans, errOut
}

func (g *gitWorld) tipOf(branch string) string {
	g.t.Helper()
	return g.git(g.checkout, "rev-parse", "--verify", "refs/heads/"+branch+"^{commit}")
}

func (g *gitWorld) hasBranch(branch string) bool {
	cmd := exec.Command("git", "-C", g.checkout, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch)
	cmd.Env = g.env
	return cmd.Run() == nil
}

// sideBranch makes a branch on a commit with no parent, so it does not contain dev.
func (g *gitWorld) sideBranch(name string) string {
	g.t.Helper()
	sha := g.git(g.checkout, "commit-tree", "dev^{tree}", "-m", "side")
	g.git(g.checkout, "branch", name, sha)
	return sha
}

// advanceDev puts one more commit on dev without touching the working tree's other branches.
func (g *gitWorld) advanceDev() string {
	g.t.Helper()
	sha := g.git(g.checkout, "commit-tree", "dev^{tree}", "-p", "dev", "-m", "dev moves on")
	g.git(g.checkout, "update-ref", "refs/heads/dev", sha)
	return sha
}

// TestGitInKeepsTheExitCode (R2-01, red on dev: gitIn formatted the run error with %v, which dropped the *exec.ExitError): the exit
// code of the git run reaches gitExitCode through gitIn's error, and the text is the one it always was.
func TestGitInKeepsTheExitCode(t *testing.T) {
	g := newGitWorld(t)
	g.sideBranch("side")
	notRepo := t.TempDir()
	env := append(append([]string{}, g.env...), "GIT_CEILING_DIRECTORIES="+filepath.Dir(notRepo))
	unknown := strings.Repeat("ab", 20)
	for _, c := range []struct {
		name     string
		dir      string
		env      []string
		args     []string
		wantCode int
	}{
		{"not an ancestor", g.checkout, g.env, []string{"merge-base", "--is-ancestor", "dev", "side"}, 1},
		{"missing ref", g.checkout, g.env, []string{"rev-parse", "--verify", "--quiet", "refs/heads/nope"}, 1},
		{"not a repository", notRepo, env, []string{"rev-parse", "--git-dir"}, 128},
		{"unknown object", g.checkout, g.env, []string{"merge-base", "--is-ancestor", "dev", unknown}, 128},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := gitIn(context.Background(), c.env, c.dir, c.args...)
			if err == nil {
				t.Fatalf("git %v succeeded", c.args)
			}
			if code, ok := gitExitCode(err); !ok || code != c.wantCode {
				t.Errorf("gitExitCode(%v) = %d, %v; want %d, true", err, code, ok, c.wantCode)
			}
			want := fmt.Sprintf("git %s: exit status %d: ", strings.Join(c.args, " "), c.wantCode)
			if !strings.HasPrefix(err.Error(), want) {
				t.Errorf("the error text is %q, want it to start %q", err, want)
			}
		})
	}
	if code, ok := gitExitCode(errors.New("plain")); ok || code != 0 {
		t.Errorf("gitExitCode of a plain error = %d, %v; want 0, false", code, ok)
	}
	if code, ok := gitExitCode(nil); ok || code != 0 {
		t.Errorf("gitExitCode(nil) = %d, %v; want 0, false", code, ok)
	}
	// git not found is not an exit: no exit code
	t.Setenv("PATH", t.TempDir())
	_, err := gitIn(context.Background(), g.env, g.checkout, "rev-parse", "HEAD")
	if err == nil {
		t.Fatal("gitIn without a git succeeded")
	}
	if code, ok := gitExitCode(err); ok {
		t.Errorf("gitExitCode of a missing git = %d, true; want false", code)
	}
}

// TestBranchCreatesTheBundleBranchAtTheBaseTipOnce: the first call creates bundle/<milestone> at the tip of dev, the second finds it
// there and changes nothing. The checkout's HEAD and working tree stay where they were, and nothing is fetched or pushed.
func TestBranchCreatesTheBundleBranchAtTheBaseTipOnce(t *testing.T) {
	g := newGitWorld(t)
	devTip := g.tipOf("dev")
	head := g.git(g.checkout, "symbolic-ref", "HEAD")
	remoteBefore := g.git(g.checkout, "for-each-ref", "refs/remotes")
	code, ans, errOut := g.branchRun(t, "--milestone", "m1")
	if code != 0 {
		t.Fatalf("branch: exit %d %s", code, errOut)
	}
	want := BranchAnswer{Branch: "bundle/m1", Base: "dev", BaseTip: devTip, Tip: devTip, Created: true}
	if ans != want {
		t.Errorf("first answer %+v, want %+v", ans, want)
	}
	if got := g.tipOf("bundle/m1"); got != devTip {
		t.Errorf("bundle/m1 is at %s, want the dev tip %s", got, devTip)
	}
	code, ans, errOut = g.branchRun(t, "--milestone", "m1")
	if code != 0 {
		t.Fatalf("second branch: exit %d %s", code, errOut)
	}
	want.Created = false
	if ans != want {
		t.Errorf("second answer %+v, want %+v", ans, want)
	}
	if g.git(g.checkout, "symbolic-ref", "HEAD") != head || g.git(g.checkout, "for-each-ref", "refs/remotes") != remoteBefore {
		t.Error("the checkout's HEAD or remote refs changed")
	}
	if g.remoteSHA("bundle/m1") != "" {
		t.Error("the branch was pushed")
	}
	// --name and --base choose the branch and its base
	m1Tip := g.tipOf("crw/m1")
	code, ans, errOut = g.branchRun(t, "--milestone", "m1", "--base", "crw/m1", "--name", "bundle/other")
	if code != 0 || ans != (BranchAnswer{Branch: "bundle/other", Base: "crw/m1", BaseTip: m1Tip, Tip: m1Tip, Created: true}) {
		t.Errorf("--base crw/m1 --name bundle/other: exit %d %+v %s", code, ans, errOut)
	}
}

// TestBranchRefusesAnExistingBranchThatIsNotADescendantOfTheBase: the named branch sits on a commit that does not contain dev; the
// command answers 2 with the never-moved line and the ref is as it was.
func TestBranchRefusesAnExistingBranchThatIsNotADescendantOfTheBase(t *testing.T) {
	g := newGitWorld(t)
	side := g.sideBranch("bundle/m1")
	code, _, errOut := g.branchRun(t, "--milestone", "m1")
	if code != 2 || !strings.Contains(errOut, "bundle/m1 does not contain dev at "+g.tipOf("dev")[:12]) || !strings.Contains(errOut, "it is never moved") {
		t.Errorf("exit %d %q, want 2 naming the branch, the base and that it is never moved", code, errOut)
	}
	if got := g.tipOf("bundle/m1"); got != side {
		t.Errorf("bundle/m1 moved to %s, was %s", got, side)
	}
}

// TestBranchNeverForceMoves: a branch at the old dev tip is not moved when dev advances, and a branch ahead of dev (landed nodes) is
// reused as it is.
func TestBranchNeverForceMoves(t *testing.T) {
	g := newGitWorld(t)
	old := g.tipOf("dev")
	if code, _, errOut := g.branchRun(t, "--milestone", "m1"); code != 0 {
		t.Fatalf("create: exit %d %s", code, errOut)
	}
	g.advanceDev()
	code, _, errOut := g.branchRun(t, "--milestone", "m1")
	if code != 2 || !strings.Contains(errOut, "never moved") {
		t.Errorf("dev advanced: exit %d %q, want 2", code, errOut)
	}
	if got := g.tipOf("bundle/m1"); got != old {
		t.Errorf("bundle/m1 moved to %s, was %s", got, old)
	}
	// ahead of dev: the nodes landed on it
	ahead := g.tipOf("crw/m1")
	g.git(g.checkout, "branch", "bundle/m2", ahead)
	g.git(g.checkout, "branch", "base2", old)
	code, ans, errOut := g.branchRun(t, "--milestone", "m2", "--base", "base2")
	if code != 0 || ans.Created || ans.Tip != ahead || ans.BaseTip != old {
		t.Errorf("ahead of the base: exit %d %+v %s, want reused at %s", code, ans, errOut, ahead)
	}
	if got := g.tipOf("bundle/m2"); got != ahead {
		t.Errorf("bundle/m2 is at %s, want %s", got, ahead)
	}
}

// TestBranchSeparatesAnUnusableBranchFromAGitFailure (R2-01): an input that cannot be used (a branch that is not a descendant, a
// missing base, an invalid name, a missing flag) is exit 2; a git that fails (a directory that is no repository) is exit 1 with git's
// message and no ref written.
func TestBranchSeparatesAnUnusableBranchFromAGitFailure(t *testing.T) {
	g := newGitWorld(t)
	g.sideBranch("bundle/m1")
	for name, args := range map[string][]string{
		"not a descendant": {"--milestone", "m1"},
		"missing base":     {"--milestone", "m2", "--base", "nope"},
		"invalid name":     {"--milestone", "m3", "--name", "a..b"},
		"invalid base":     {"--milestone", "m3", "--base", "-x"},
		"no milestone":     {},
		"argument":         {"--milestone", "m3", "extra"},
	} {
		if code, _, errOut := g.branchRun(t, args...); code != 2 || errOut == "" {
			t.Errorf("%s: exit %d %q, want 2", name, code, errOut)
		}
	}
	if g.hasBranch("bundle/m2") || g.hasBranch("bundle/m3") || g.hasBranch("a..b") {
		t.Error("a refused call wrote a ref")
	}
	notRepo := t.TempDir()
	env := append(append([]string{}, g.env...), "GIT_CEILING_DIRECTORIES="+filepath.Dir(notRepo))
	d := deps(t, nil, g)
	d.Env = env
	code, out, errOut := run(t, d, "branch", "--checkout", notRepo, "--milestone", "m1")
	if code != 1 || !strings.Contains(errOut, "not a git repository") || out != "" {
		t.Errorf("a non-repository: exit %d out %q err %q, want 1 with git's message", code, out, errOut)
	}
	if entries, _ := os.ReadDir(notRepo); len(entries) != 0 {
		t.Errorf("the non-repository holds %v", entries)
	}
	// a checkout path that does not exist at all is a git failure too
	if code, _, _ := run(t, d, "branch", "--checkout", filepath.Join(notRepo, "none"), "--milestone", "m1"); code != 1 {
		t.Errorf("a missing checkout: exit %d, want 1", code)
	}
}
