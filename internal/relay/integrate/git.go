package integrate

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

// The git helpers of the integration command. Every command runs with a clean environment and an
// explicit argument vector, never a shell string, and every read is bounded by the context. Nothing
// here writes to a caller's checkout except the one ref update the batch is asked for.

// commitPattern is the shape of a full object name this package accepts: 40 hex digits, or 64 for a
// sha256 repository.
var commitPattern = regexp.MustCompile("^[0-9a-f]{40}([0-9a-f]{24})?$")

// gitEnv is the environment every git call runs with: no inherited GIT_* variable can change what a
// checkout answers (a GIT_DIR would point the command at another repository), and no prompt can hang a
// call that has no terminal.
func gitEnv(extra ...string) []string {
	env := make([]string, 0, 8)
	for _, v := range os.Environ() {
		if !strings.HasPrefix(v, "GIT_") {
			env = append(env, v)
		}
	}
	env = append(env, "GIT_TERMINAL_PROMPT=0", "GIT_CONFIG_NOSYSTEM=1", "GIT_ATTR_NOSYSTEM=1", "LC_ALL=C")
	return append(env, extra...)
}

// gitRun runs one git command in dir and returns its exit code and stdout. A non-zero exit is not by
// itself an error: callers that read an answer from the exit code (merge-tree, merge-base) ask for it.
func gitRun(ctx context.Context, dir string, extraEnv []string, args ...string) (int, string, error) {
	argv := args
	if dir != "" {
		argv = append([]string{"-C", dir}, args...)
	}
	cmd := exec.CommandContext(ctx, "git", argv...)
	cmd.Env = gitEnv(extraEnv...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	if err == nil {
		return 0, stdout.String(), nil
	}
	if exit, ok := err.(*exec.ExitError); ok {
		return exit.ExitCode(), stdout.String(), fmt.Errorf("git %s: exit %d: %s", strings.Join(args, " "), exit.ExitCode(), strings.TrimSpace(stderr.String()))
	}
	return -1, "", fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
}

// gitText runs one git command and returns its trimmed stdout.
func gitText(ctx context.Context, dir string, args ...string) (string, error) {
	_, out, err := gitRun(ctx, dir, nil, args...)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// isGitCheckout reports whether dir is a working tree, a linked working tree or a bare repository git
// can read.
func isGitCheckout(ctx context.Context, dir string) bool {
	if dir == "" || !filepath.IsAbs(dir) {
		return false
	}
	_, err := gitText(ctx, dir, "rev-parse", "--git-dir")
	return err == nil
}

// refTip reads a branch's commit in a local checkout. found is false when the ref does not exist,
// which is not an error: an integration branch is created by its first batch.
func refTip(ctx context.Context, dir, ref string) (string, bool, error) {
	code, out, err := gitRun(ctx, dir, nil, "show-ref", "--verify", "--hash", "refs/heads/"+ref)
	if code == 1 {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	sha := strings.TrimSpace(out)
	if !commitPattern.MatchString(sha) {
		return "", false, fmt.Errorf("git answered %q for refs/heads/%s, which is not a full object name", sha, ref)
	}
	return sha, true, nil
}

// hasCommit reports whether the checkout holds a commit object.
func hasCommit(ctx context.Context, dir, commit string) bool {
	code, _, _ := gitRun(ctx, dir, nil, "cat-file", "-e", commit+"^{commit}")
	return code == 0
}

// treeOf reads the tree object of a commit.
func treeOf(ctx context.Context, dir, commit string) (string, error) {
	return gitText(ctx, dir, "rev-parse", commit+"^{tree}")
}

// isAncestor reports whether ancestor is contained in descendant. merge-base --is-ancestor answers with
// its exit status: 0 contained, 1 not, anything else a read failure.
func isAncestor(ctx context.Context, dir, ancestor, descendant string) (bool, error) {
	code, _, err := gitRun(ctx, dir, nil, "merge-base", "--is-ancestor", ancestor, descendant)
	switch code {
	case 0:
		return true, nil
	case 1:
		return false, nil
	}
	return false, err
}

// mergeTrees computes the merge of two commits in a checkout without touching any working tree: it
// answers the merged tree and, when git could not merge them, the paths it could not merge. A conflict
// is an answer (the candidate cannot be merged), not a failure.
func mergeTrees(ctx context.Context, dir, first, second string) (tree string, conflicts []string, err error) {
	code, out, err := gitRun(ctx, dir, nil, "merge-tree", "--write-tree", "--name-only", first, second)
	if code == 0 {
		return strings.TrimSpace(out), nil, nil
	}
	if code != 1 {
		return "", nil, err
	}
	// exit 1 is "merged with conflicts": the first line is the tree git wrote, the rest are the paths
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) == 0 || !commitPattern.MatchString(strings.TrimSpace(lines[0])) {
		return "", nil, fmt.Errorf("git merge-tree answered %q, which does not name a tree", strings.TrimSpace(out))
	}
	for _, line := range lines[1:] {
		if line = strings.TrimSpace(line); line != "" {
			conflicts = append(conflicts, line)
		}
	}
	return strings.TrimSpace(lines[0]), conflicts, nil
}

// mergeCommit writes the merge commit of a tree with the two parents, without a working tree.
func mergeCommit(ctx context.Context, dir, tree, first, second, message string) (string, error) {
	commit, err := gitText(ctx, dir, "commit-tree", tree, "-p", first, "-p", second, "-m", message)
	if err != nil {
		return "", err
	}
	if !commitPattern.MatchString(commit) {
		return "", fmt.Errorf("git commit-tree answered %q, which is not a commit", commit)
	}
	return commit, nil
}

// updateRef moves a branch to newCommit only when it still holds oldCommit, which is git's own
// compare-and-swap: a ref that moved under the caller makes the update fail instead of overwriting the
// work that moved it. An empty oldCommit creates the ref and refuses when it already exists.
func updateRef(ctx context.Context, dir, ref, newCommit, oldCommit string) error {
	if oldCommit == "" {
		_, _, err := gitRun(ctx, dir, nil, "update-ref", "refs/heads/"+ref, newCommit, "")
		return err
	}
	_, _, err := gitRun(ctx, dir, nil, "update-ref", "refs/heads/"+ref, newCommit, oldCommit)
	return err
}

// lsRemote reads where a remote ref points. unreachable reports a failure that means "the remote could
// not be reached", which the push path defers instead of refusing: a GitHub outage must not stop the
// integration that already completed.
func lsRemote(ctx context.Context, dir, remote, ref string) (string, bool, error) {
	code, out, err := gitRun(ctx, dir, nil, "ls-remote", remote, "refs/heads/"+ref)
	if err != nil {
		return "", code < 0 || unreachableError(err.Error()), err
	}
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[1] == "refs/heads/"+ref && commitPattern.MatchString(fields[0]) {
			return fields[0], false, nil
		}
	}
	// the ref is absent: the remote has no such branch, which is an empty answer and not an outage
	return "", false, nil
}

// unreachableError is whether git's message means the remote could not be reached rather than that it
// answered and refused. Only the first case defers the push.
func unreachableError(message string) bool {
	lower := strings.ToLower(message)
	for _, needle := range []string{"could not resolve host", "connection refused", "connection timed out", "network is unreachable",
		"could not read from remote repository", "no route to host", "operation timed out", "temporary failure in name resolution",
		"could not connect to server", "failed to connect to"} {
		if strings.Contains(lower, needle) {
			return true
		}
	}
	return false
}

// pushRefspec pushes a local commit to a remote branch with no force and no leading plus, so the remote
// refuses the update unless it is a fast-forward of what it holds.
func pushRefspec(ctx context.Context, dir, remote, commit, ref string) (string, error) {
	code, out, err := gitRun(ctx, dir, nil, "push", remote, commit+":refs/heads/"+ref)
	if err != nil {
		if code < 0 || unreachableError(err.Error()) {
			return "", &unreachable{detail: err.Error()}
		}
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// unreachable is the error a push returns when the remote could not be reached, so the command can
// answer "deferred" instead of refusing.
type unreachable struct{ detail string }

func (e *unreachable) Error() string { return e.detail }
