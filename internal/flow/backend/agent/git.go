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

// Snapshot is the working snapshot of a worktree: the tree object id of every file the worktree holds that git does not
// ignore (tracked, modified and untracked), computed in a throwaway index so the worktree's own index and HEAD are
// untouched. Two worktrees with the same files have the same snapshot, whatever their HEAD.
func (a *Allocator) Snapshot(ctx context.Context, path string) (string, error) {
	dir, err := os.MkdirTemp("", "crw-snapshot-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(dir)
	index := filepath.Join(dir, "index")
	env := []string{"GIT_INDEX_FILE=" + index}
	if err := a.seedIndex(ctx, path, index, env); err != nil {
		return "", err
	}
	if err := a.clearIndexFlags(ctx, path, env); err != nil {
		return "", err
	}
	if _, err := a.git(ctx, path, env, "add", "--all"); err != nil {
		return "", err
	}
	tree, err := a.git(ctx, path, env, "write-tree")
	if err != nil {
		return "", err
	}
	return tree, nil
}

// seedIndex starts the throwaway index from the worktree's own index, so every tracked file stays in the snapshot even
// when an ignore rule matches it (git add --all skips an ignored file only when it is untracked); clearIndexFlags then
// takes off the flags that would hide a file's content. A worktree with no index
// yet starts from its HEAD, and one with neither starts empty.
func (a *Allocator) seedIndex(ctx context.Context, path, index string, env []string) error {
	real, err := a.git(ctx, path, nil, "rev-parse", "--path-format=absolute", "--git-path", "index")
	if err == nil {
		if data, readErr := os.ReadFile(real); readErr == nil {
			return os.WriteFile(index, data, 0o600)
		}
	}
	if _, err := a.git(ctx, path, nil, "rev-parse", "--verify", "--quiet", "HEAD^{commit}"); err != nil {
		return nil
	}
	_, err = a.git(ctx, path, env, "read-tree", "HEAD")
	return err
}

// flagBatch bounds how many paths one update-index command names.
const flagBatch = 500

// clearIndexFlags clears, in the throwaway index, the per-entry flags the copy of the worktree's index carries over and
// that make git add skip a file's content: assume-unchanged on every entry, and skip-worktree on every entry whose file is
// in the worktree. A skip-worktree entry whose file is absent (a sparse checkout) keeps its flag and is held with its
// index content; it is not a deletion. The worktree's own index keeps its flags.
func (a *Allocator) clearIndexFlags(ctx context.Context, path string, env []string) error {
	listing, err := a.git(ctx, path, env, "ls-files", "-v", "-z")
	if err != nil {
		return err
	}
	var assumed, skipped []string
	for _, entry := range strings.Split(listing, "\x00") {
		if len(entry) < 3 || entry[1] != ' ' {
			continue
		}
		tag, name := entry[0], entry[2:]
		if tag >= 'a' && tag <= 'z' {
			assumed = append(assumed, name)
		}
		if tag == 'S' || tag == 's' {
			if _, statErr := os.Lstat(filepath.Join(path, filepath.FromSlash(name))); statErr == nil {
				skipped = append(skipped, name)
			}
		}
	}
	for _, set := range []struct {
		flag  string
		names []string
	}{{"--no-assume-unchanged", assumed}, {"--no-skip-worktree", skipped}} {
		for len(set.names) > 0 {
			n := min(len(set.names), flagBatch)
			if _, err := a.git(ctx, path, env, append([]string{"update-index", set.flag, "--"}, set.names[:n]...)...); err != nil {
				return err
			}
			set.names = set.names[n:]
		}
	}
	return nil
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
