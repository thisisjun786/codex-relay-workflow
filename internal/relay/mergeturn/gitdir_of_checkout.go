package mergeturn

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// gitDirOfCheckout is the git directory of a checkout: the .git directory of a main checkout, or the directory a
// linked worktree's .git file names (CRW-965 review: a linked worktree's .git is a file, not a directory). Like every
// other git read here, it runs with every GIT_ variable removed, so a poisoned environment cannot name another repository.
func gitDirOfCheckout(ctx context.Context, repository string) string {
	dotGit := filepath.Join(repository, ".git")
	info, err := os.Stat(dotGit)
	if err != nil || info.IsDir() {
		return dotGit
	}
	env := make([]string, 0)
	for _, v := range os.Environ() {
		if !strings.HasPrefix(v, "GIT_") {
			env = append(env, v)
		}
	}
	cmd := exec.CommandContext(ctx, "git", "-C", repository, "rev-parse", "--absolute-git-dir")
	cmd.Env = append(env, "GIT_TERMINAL_PROMPT=0", "LC_ALL=C")
	out, err := cmd.Output()
	if err != nil {
		return dotGit
	}
	if dir := strings.TrimSpace(string(out)); dir != "" {
		return dir
	}
	return dotGit
}
