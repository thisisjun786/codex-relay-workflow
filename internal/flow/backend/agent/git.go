package agent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// GitFunc runs one git command with `git -C dir args...` and returns its trimmed standard output. The allocator
// runs every git command through it, so a test can see which directories it touched.
type GitFunc func(ctx context.Context, dir string, env []string, args ...string) (string, error)

// ExecGit is the GitFunc that runs the git binary.
func ExecGit(binary string) GitFunc {
	return func(ctx context.Context, dir string, env []string, args ...string) (string, error) {
		cmd := exec.CommandContext(ctx, binary, append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(), env...)
		var stdout, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		if err := cmd.Run(); err != nil {
			return "", fmt.Errorf("git -C %s %s: %w: %s", dir, strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
		}
		return strings.TrimSpace(stdout.String()), nil
	}
}

// identity is the author and committer of the commits the run makes (the retired-worktree refs and Commit); it is passed
// through the environment of one command and never written to a git configuration.
var identity = []string{
	"GIT_AUTHOR_NAME=crw-flow", "GIT_AUTHOR_EMAIL=crw-flow@localhost",
	"GIT_COMMITTER_NAME=crw-flow", "GIT_COMMITTER_EMAIL=crw-flow@localhost",
}

// Snapshot is the working snapshot of a worktree: the tree object id of every file the worktree holds that git does not
// ignore (tracked, modified and untracked), computed in a throwaway index so the worktree's own index and HEAD are
// untouched. Two worktrees with the same files have the same snapshot, whatever their HEAD.
func (a *Allocator) Snapshot(ctx context.Context, path string) (string, error) {
	dir, err := os.MkdirTemp("", "crw-snapshot-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(dir)
	env := []string{"GIT_INDEX_FILE=" + filepath.Join(dir, "index")}
	if _, err := a.git(ctx, path, env, "add", "--all"); err != nil {
		return "", err
	}
	tree, err := a.git(ctx, path, env, "write-tree")
	if err != nil {
		return "", err
	}
	return tree, nil
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
