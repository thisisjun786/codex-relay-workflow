package mergeturn

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// gitDirOfCheckout is the git directory of a checkout: the .git directory of a main checkout, or the directory a
// linked worktree's .git file names (CRW-965 review: a linked worktree's .git is a file, not a directory).
func gitDirOfCheckout(ctx context.Context, repository string) string {
	dotGit := filepath.Join(repository, ".git")
	info, err := os.Stat(dotGit)
	if err != nil || info.IsDir() {
		return dotGit
	}
	out, err := exec.CommandContext(ctx, "git", "-C", repository, "rev-parse", "--absolute-git-dir").Output()
	if err != nil {
		return dotGit
	}
	if dir := strings.TrimSpace(string(out)); dir != "" {
		return dir
	}
	return dotGit
}
