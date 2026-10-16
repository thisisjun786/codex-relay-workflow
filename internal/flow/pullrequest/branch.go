package pullrequest

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
)

// BranchAnswer is what `crw flow pr branch` prints: the integration branch of the bundle, the base it was cut from and that base's tip,
// the branch's tip, and whether this call created it.
type BranchAnswer struct {
	Branch  string `json:"branch"`
	Base    string `json:"base"`
	BaseTip string `json:"base_tip"`
	Tip     string `json:"tip"`
	Created bool   `json:"created"`
}

// inputError is an input the command cannot use (exit 2): a retry does not change it. A git that fails is any other error (exit 1).
type inputError string

func (e inputError) Error() string { return string(e) }

// gitExitCode is the exit code of the git run behind err (gitIn keeps the *exec.ExitError with %w). It answers false for an error that
// is not a git exit, such as git not found or a cancelled context.
func gitExitCode(err error) (int, bool) {
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return exit.ExitCode(), true
	}
	return 0, false
}

// Branch makes sure the local branch name exists in the checkout and is the base's tip or a descendant of it, and answers where it is.
// Local refs only: nothing is fetched, and the checkout's HEAD and working tree are not touched. An absent branch is created at the
// base's tip (update-ref with an empty old value, which creates and never moves a ref); an existing branch that contains the base's tip
// is reused as it is; one that does not is never moved, it is an inputError. A name git does not accept, a base that is not a local
// branch, and a branch that does not contain the base are inputErrors; every other git failure is returned as it is.
func Branch(ctx context.Context, env []string, checkout, base, name string) (BranchAnswer, error) {
	git := func(args ...string) (string, error) { return gitIn(ctx, env, checkout, args...) }
	// a directory that is no repository (or no directory) is a git failure, so a 128 below is only ever a name git does not accept
	if _, err := git("rev-parse", "--git-dir"); err != nil {
		return BranchAnswer{}, err
	}
	for _, n := range []string{base, name} {
		out, err := git("check-ref-format", "--branch", n)
		if code, ok := gitExitCode(err); ok && code == 128 || err == nil && out != n {
			return BranchAnswer{}, inputError(fmt.Sprintf("%q is not a valid branch name", n))
		}
		if err != nil {
			return BranchAnswer{}, err
		}
	}
	// tipOf is the commit of a local branch, "" when there is none: rev-parse --verify --quiet answers 1 for a missing ref.
	tipOf := func(branch string) (string, error) {
		tip, err := git("rev-parse", "--verify", "--quiet", "refs/heads/"+branch+"^{commit}")
		if code, ok := gitExitCode(err); ok && code == 1 {
			return "", nil
		}
		return tip, err
	}
	baseTip, err := tipOf(base)
	if err != nil {
		return BranchAnswer{}, err
	}
	if baseTip == "" {
		return BranchAnswer{}, inputError(fmt.Sprintf("the base branch %s does not exist in %s", base, checkout))
	}
	tip, err := tipOf(name)
	if err != nil {
		return BranchAnswer{}, err
	}
	ans := BranchAnswer{Branch: name, Base: base, BaseTip: baseTip, Tip: tip}
	if tip == "" {
		if _, err := git("update-ref", "-m", "crw flow pr branch: create from "+base, "refs/heads/"+name, baseTip, ""); err == nil {
			ans.Tip, ans.Created = baseTip, true
			return ans, nil
		} else if tip, rerr := tipOf(name); rerr != nil || tip == "" {
			// not a lost race: the ref is still absent
			return BranchAnswer{}, err
		} else {
			// another run created it first: judged as an existing branch
			ans.Tip = tip
		}
	}
	if _, err := git("merge-base", "--is-ancestor", baseTip, ans.Tip); err != nil {
		if code, ok := gitExitCode(err); ok && code == 1 {
			return BranchAnswer{}, inputError(fmt.Sprintf("the branch %s does not contain %s at %s: it is never moved", name, base, baseTip[:min(12, len(baseTip))]))
		}
		return BranchAnswer{}, err
	}
	return ans, nil
}
