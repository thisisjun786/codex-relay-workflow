package dagsched

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/evidence"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/mergeturn"
)

// GitAncestry is the production Ancestry: whether a commit is contained in a branch tip. A local checkout is asked git (merge-base --is-ancestor: exit 0 contained, 1 not, anything else
// an error and no answer); a forge repository is asked the compare API (contained when the tip is not behind the commit, behind_by = 0, and the commits are identical or the tip is ahead). A
// squash or rebase landing is never an ancestor: the head the parent accepted is not in the history of the branch, which is the finding the reading names integration_unprovable.
type GitAncestry struct {
	Git, GH string
	// Timeout bounds the forge call; zero is mergeturn.ForgeCallTimeout, the bound of the other forge read.
	Timeout time.Duration
}

// forgeTimeout is the bound of one compare call.
func (g GitAncestry) forgeTimeout() time.Duration {
	if g.Timeout > 0 {
		return g.Timeout
	}
	return mergeturn.ForgeCallTimeout
}

// forgeWaitDelay is how long the call waits for a process that outlived the kill (a child of gh that kept the output pipe open) before it gives up on it, so a stuck process tree cannot hold the
// call past its bound.
const forgeWaitDelay = 5 * time.Second

// Ancestry reports whether subject is an ancestor of tip in repository, and the method that said so.
func (g GitAncestry) Ancestry(ctx context.Context, repository, subject, tip string) (bool, string, error) {
	if filepath.IsAbs(repository) {
		return g.local(ctx, repository, subject, tip)
	}
	owner, name, err := evidence.SplitRepository(repository)
	if err != nil {
		return false, "", err
	}
	return g.forge(ctx, owner, name, subject, tip)
}

func cleanGitEnv() []string {
	env := make([]string, 0)
	for _, v := range os.Environ() {
		if !strings.HasPrefix(v, "GIT_") {
			env = append(env, v)
		}
	}
	return append(env, "GIT_TERMINAL_PROMPT=0", "LC_ALL=C")
}

func (g GitAncestry) local(ctx context.Context, repository, subject, tip string) (bool, string, error) {
	git := g.Git
	if git == "" {
		git = "git"
	}
	// the checkout is whatever git says it is (a working tree, a linked working tree, which has a .git file, or a bare repository)
	cmd := exec.CommandContext(ctx, git, "-C", repository, "merge-base", "--is-ancestor", subject, tip)
	cmd.Env = cleanGitEnv()
	err := cmd.Run()
	method := "git merge-base --is-ancestor"
	if err == nil {
		return true, method, nil
	}
	if exit, ok := err.(*exec.ExitError); ok && exit.ExitCode() == 1 {
		return false, method, nil
	}
	return false, method, fmt.Errorf("%s %s %s in %s: %w", method, subject, tip, repository, err)
}

func (g GitAncestry) forge(ctx context.Context, owner, name, subject, tip string) (bool, string, error) {
	gh := g.GH
	if gh == "" {
		gh = "gh"
	}
	// the call is bounded like the other forge read, the branch tip of the merge lane (mergeturn.ForgeCallTimeout): a forge that does not answer is no answer, and never a call that waits for good
	timeout := g.forgeTimeout()
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(runCtx, gh, "api", "--method", "GET", fmt.Sprintf("repos/%s/%s/compare/%s...%s", owner, name, subject, tip))
	cmd.Env = cleanGitEnv()
	cmd.WaitDelay = forgeWaitDelay
	raw, err := cmd.Output()
	method := "gh api compare behind_by"
	if err != nil {
		if ctx.Err() == nil && runCtx.Err() == context.DeadlineExceeded {
			return false, method, fmt.Errorf("%s %s...%s in %s/%s: the forge call exceeded the timeout of %s, so no answer was observed", method, subject, tip, owner, name, timeout)
		}
		return false, method, fmt.Errorf("%s %s...%s in %s/%s: %w", method, subject, tip, owner, name, err)
	}
	var compare struct {
		Status   string `json:"status"`
		BehindBy *int64 `json:"behind_by"`
	}
	if err := json.Unmarshal(raw, &compare); err != nil || compare.BehindBy == nil {
		return false, method, fmt.Errorf("%s: the answer is not a comparison", method)
	}
	// the compare is between the commit (base) and the tip (head): the commit is an ancestor of the tip when the tip is not behind it
	return *compare.BehindBy == 0 && (compare.Status == "identical" || compare.Status == "ahead"), method, nil
}
