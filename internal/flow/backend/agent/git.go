package agent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/ops"
)

// GitFunc runs one git command with `git -C dir args...` and returns its trimmed standard output. The allocator
// runs every git command through it, so a test can see which directories it touched.
type GitFunc func(ctx context.Context, dir string, env []string, args ...string) (string, error)

// ExecGit is the GitFunc that runs the git binary.
func ExecGit(binary string) GitFunc {
	return func(ctx context.Context, dir string, env []string, args ...string) (string, error) {
		cmd := exec.CommandContext(ctx, binary, append([]string{"-C", dir}, args...)...)
		cmd.Env = append(cleanEnvironment(os.Environ()), env...)
		var stdout, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		if err := cmd.Run(); err != nil {
			return "", fmt.Errorf("git -C %s %s: %w: %s", dir, strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
		}
		return strings.TrimSpace(stdout.String()), nil
	}
}

// repositoryVariables are the environment variables that point git at a repository, a work tree, an index or an object
// store (git rev-parse --local-env-vars, and the namespace). `git -C <dir>` does not override them, so a process started by
// a git hook, or from a shell that exports one, would run every command against that other repository.
var repositoryVariables = map[string]bool{
	"GIT_ALTERNATE_OBJECT_DIRECTORIES": true, "GIT_COMMON_DIR": true, "GIT_CONFIG": true, "GIT_CONFIG_COUNT": true,
	"GIT_CONFIG_PARAMETERS": true, "GIT_DIR": true, "GIT_GRAFT_FILE": true, "GIT_IMPLICIT_WORK_TREE": true, "GIT_INDEX_FILE": true,
	"GIT_NAMESPACE": true, "GIT_NO_REPLACE_OBJECTS": true, "GIT_OBJECT_DIRECTORY": true, "GIT_PREFIX": true,
	"GIT_REPLACE_REF_BASE": true, "GIT_SHALLOW_FILE": true, "GIT_WORK_TREE": true,
}

// cleanEnvironment is the environment without the variables that route git to a repository other than the one -C names
// (and without the numbered GIT_CONFIG_KEY_n and GIT_CONFIG_VALUE_n pairs that GIT_CONFIG_COUNT would activate).
func cleanEnvironment(environ []string) []string {
	out := make([]string, 0, len(environ))
	for _, kv := range environ {
		name, _, _ := strings.Cut(kv, "=")
		if repositoryVariables[name] || strings.HasPrefix(name, "GIT_CONFIG_KEY_") || strings.HasPrefix(name, "GIT_CONFIG_VALUE_") {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// identity is the author and committer of the commits the run makes (the retired-worktree refs and Commit); it is passed
// through the environment of one command and never written to a git configuration.
var identity = []string{
	"GIT_AUTHOR_NAME=crw-flow", "GIT_AUTHOR_EMAIL=crw-flow@localhost",
	"GIT_COMMITTER_NAME=crw-flow", "GIT_COMMITTER_EMAIL=crw-flow@localhost",
}

// Snapshot is the working snapshot of a worktree (ops.Snapshot): the tree object id of every file the worktree holds that
// git does not ignore (tracked, modified and untracked), computed in a private index that starts from HEAD, so the
// worktree's own index and HEAD are untouched. Two worktrees with the same files have the same snapshot, whatever their HEAD.
// The pending tree P of a call and the proofs that compare a worktree with it use this one function.
func (a *Allocator) Snapshot(ctx context.Context, path string) (string, error) {
	return ops.Snapshot(ctx, ops.GitFunc(a.git), path)
}

// Head is the commit a worktree's HEAD is at, read from git.
func (a *Allocator) Head(ctx context.Context, path string) (string, error) {
	return a.git(ctx, path, nil, "rev-parse", "--verify", "HEAD^{commit}")
}

// Commit commits everything the worktree holds (the run commits; the model never does) and returns the new HEAD read
// back from git. It is an error when nothing changed unless allowEmpty is set.
func (a *Allocator) Commit(ctx context.Context, path, message string, allowEmpty bool) (string, error) {
	if message == "" {
		return "", errors.New("agent: a commit needs a message")
	}
	if _, err := a.git(ctx, path, nil, "add", "--all"); err != nil {
		return "", err
	}
	args := []string{"-c", "commit.gpgsign=false", "commit", "--no-verify", "--quiet", "-m", message}
	if allowEmpty {
		args = append(args, "--allow-empty")
	}
	if _, err := a.git(ctx, path, identity, args...); err != nil {
		return "", err
	}
	return a.Head(ctx, path)
}

// HeadOf implements engine.HeadKeeper: the worktree's HEAD.
func (a *Allocator) HeadOf(ctx context.Context, path string) (string, error) {
	return a.Head(ctx, path)
}

// KeepRef implements engine.HeadKeeper: it keeps a commit a session executor made in its worktree on a stray ref of this run
// (engine.StrayRef, refs/crw/flow/<run>/stray/...), in the repository the worktrees are made from. Any other ref is refused. The
// ref is created with a compare-and-swap against no ref, so a record is never overwritten: a ref that holds the commit already
// is kept as it is, one that holds an ancestor of the commit (the same call's later HEAD, after a crash) is moved by a
// fast-forward whose compare-and-swap is against the commit it holds (CRW-1392), and one that holds any other commit is refused.
func (a *Allocator) KeepRef(ctx context.Context, ref, commit string) error {
	if !strings.HasPrefix(ref, engine.StrayRefPrefix(a.cfg.RunID)) || strings.Contains(ref, "..") {
		return fmt.Errorf("agent: %s is not a stray ref of run %s", ref, a.cfg.RunID)
	}
	if held, err := a.git(ctx, a.cfg.Repo, nil, "rev-parse", "--verify", "--quiet", ref); err == nil && held != "" {
		want, err := a.git(ctx, a.cfg.Repo, nil, "rev-parse", "--verify", "--quiet", commit+"^{commit}")
		switch {
		case err != nil || want == "":
			return fmt.Errorf("agent: %s already keeps %s, and %s is no commit", ref, held, commit)
		case want == held:
			return nil
		}
		if _, err := a.git(ctx, a.cfg.Repo, nil, "merge-base", "--is-ancestor", held, want); err != nil {
			return fmt.Errorf("agent: %s already keeps %s, and %s does not descend from it: a stray ref is only moved forward", ref, held, want)
		}
		_, err = a.git(ctx, a.cfg.Repo, nil, "update-ref", "-m", "crw stray head", ref, want, held)
		return err
	}
	_, err := a.git(ctx, a.cfg.Repo, nil, "update-ref", "-m", "crw stray head", ref, commit, "")
	return err
}
