package mergeturn

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyvalue"
)

// Movement is how the base branch got from one commit to another along its first-parent line.
// It reads, never writes: git cat-file --batch with replacement off and every GIT_ variable removed for an
// absolute path, one read-only forge GET per commit for owner/name. Both follow the first parent from
// to, one commit at a time, and stop at the commit whose first parent is from, after MaxMovementSteps
// commits, or at a root.
func (r TargetReader) Movement(ctx context.Context, repository, base, from, to string) (Movement, error) {
	if err := branch(base); err != nil {
		return Movement{}, err
	}
	if !shaFull.MatchString(from) || !shaFull.MatchString(to) {
		return Movement{}, unreadable("a movement is read between two full object names, not %s and %s", pyvalue.StrRepr(from), pyvalue.StrRepr(to))
	}
	if !filepath.IsAbs(repository) {
		if slug.MatchString(repository) {
			return r.githubMovement(ctx, repository, base, from, to)
		}
		return Movement{}, unreadable("repository %s is neither an absolute local path nor owner/name, so there is no target this can read", pyvalue.StrRepr(repository))
	}
	return r.gitMovement(ctx, repository, base, from, to)
}

// gitMovement walks the first-parent line with git cat-file, one stored commit at a time, and
// parses the object itself. git log is not used: it shows a commit through replace refs and a grafts
// file, which change a commit's parents without changing the commit, and so could make a direct
// commit read as a merge. Replacement is switched off for every call, and nothing here reads the
// commit graph or a grafts file.
func (r TargetReader) gitMovement(ctx context.Context, repository, base, from, to string) (Movement, error) {
	info, err := os.Stat(repository)
	if err != nil || !info.IsDir() {
		return Movement{}, unreadable("repository %s is not a directory here", pyvalue.StrRepr(repository))
	}
	gitdir := filepath.Join(repository, ".git")
	if _, err := os.Stat(gitdir); os.IsNotExist(err) {
		gitdir = repository
	}
	var steps []Step
	for current := to; len(steps) < MaxMovementSteps; {
		step, err := r.gitCommit(ctx, repository, gitdir, current)
		if err != nil {
			return Movement{}, err
		}
		steps = append(steps, step)
		if len(step.Parents) == 0 || step.Parents[0] == from {
			break
		}
		current = step.Parents[0]
	}
	return Movement{From: from, To: to, Steps: steps, Source: "local_git", Reference: "refs/heads/" + base, Repository: repository}, nil
}

// gitCommit is one commit as it is stored: its parents in order and the first line of its message.
// The object comes from cat-file --batch, whose header names its type, so a tag object (which
// cat-file commit would peel to the commit it tags) is refused rather than read as a commit.
func (r TargetReader) gitCommit(ctx context.Context, repository, gitdir, sha string) (Step, error) {
	git := r.Git
	if git == "" {
		git = "git"
	}
	timeoutCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(timeoutCtx, git, "--no-replace-objects", "--git-dir="+gitdir, "cat-file", "--batch")
	cmd.Stdin = strings.NewReader(sha + "\n")
	env := make([]string, 0)
	for _, v := range os.Environ() {
		if !strings.HasPrefix(v, "GIT_") {
			env = append(env, v)
		}
	}
	cmd.Env = append(env, "GIT_TERMINAL_PROMPT=0", "LC_ALL=C")
	output, err := cmd.Output()
	if err != nil {
		if timeoutCtx.Err() == context.DeadlineExceeded {
			return Step{}, unreadable("git did not answer within 30 seconds")
		}
		detail := "exit 1"
		if e, ok := err.(*exec.ExitError); ok {
			detail = excerpt(strings.TrimSpace(string(e.Stderr)))
			if detail == "" {
				detail = fmt.Sprintf("exit %d", e.ExitCode())
			}
		} else {
			return Step{}, unreadable("git could not be started: %v", err)
		}
		return Step{}, unreadable("git could not read commit %s in %s: %s", sha, pyvalue.StrRepr(repository), detail)
	}
	header, body, _ := strings.Cut(string(output), "\n")
	fields := strings.Fields(header)
	if len(fields) != 3 || fields[0] != sha {
		return Step{}, unreadable("git has no object %s in %s (it answered %s)", sha, pyvalue.StrRepr(repository), pyvalue.StrRepr(excerpt(header)))
	}
	if fields[1] != "commit" {
		return Step{}, unreadable("object %s in %s is a %s, not a commit", sha, pyvalue.StrRepr(repository), fields[1])
	}
	size, err := strconv.Atoi(fields[2])
	if err != nil || size < 0 || size > len(body) {
		return Step{}, unreadable("git answered a size of %s for commit %s that its output does not hold", pyvalue.StrRepr(excerpt(fields[2])), sha)
	}
	return parseCommitObject(sha, body[:size])
}

// parseCommitObject reads a stored commit: the tree line, the parent lines that follow it, and the
// first line of the message after the blank line.
func parseCommitObject(sha, raw string) (Step, error) {
	header, message, _ := strings.Cut(raw, "\n\n")
	lines := strings.Split(header, "\n")
	if !strings.HasPrefix(lines[0], "tree ") {
		return Step{}, unreadable("git answered %s for commit %s, which is not a commit object", pyvalue.StrRepr(excerpt(lines[0])), sha)
	}
	parents := make([]string, 0, 2)
	for _, line := range lines[1:] {
		parent, ok := strings.CutPrefix(line, "parent ")
		if !ok {
			break
		}
		if !shaFull.MatchString(parent) {
			return Step{}, unreadable("git named %s as a parent of commit %s, which is not a full object name", pyvalue.StrRepr(excerpt(parent)), sha)
		}
		parents = append(parents, parent)
	}
	subject, _, _ := strings.Cut(message, "\n")
	return Step{SHA: sha, Parents: parents, Subject: subject}, nil
}

func (r TargetReader) githubMovement(ctx context.Context, repository, base, from, to string) (Movement, error) {
	host := os.Getenv("GH_HOST")
	if host == "" {
		host = "github.com"
	}
	var steps []Step
	for current := to; len(steps) < MaxMovementSteps; {
		step, err := r.githubCommit(ctx, repository, current)
		if err != nil {
			return Movement{}, err
		}
		steps = append(steps, step)
		if len(step.Parents) == 0 || step.Parents[0] == from {
			break
		}
		current = step.Parents[0]
	}
	return Movement{From: from, To: to, Steps: steps, Source: "github:" + host, Reference: "refs/heads/" + base, Repository: repository}, nil
}

// githubCommit is one commit as the forge's git data API answers for it.
func (r TargetReader) githubCommit(ctx context.Context, repository, sha string) (Step, error) {
	gh := r.GH
	if gh == "" {
		gh = "gh"
	}
	argv := []string{"api", "--method", "GET", "-H", "Accept: application/vnd.github+json", "repos/" + repository + "/git/commits/" + sha}
	timeoutCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	output, err := exec.CommandContext(timeoutCtx, gh, argv...).Output()
	if err != nil {
		if timeoutCtx.Err() == context.DeadlineExceeded {
			return Step{}, unreadable("reading commit %s exceeded the timeout of 30 seconds, so no answer was observed", sha)
		}
		if e, ok := err.(*exec.ExitError); ok {
			detail := excerpt(strings.TrimSpace(string(e.Stderr)))
			if strings.Contains(detail, "HTTP 404") {
				return Step{}, unreadable("commit %s does not exist in %s", sha, repository)
			}
			return Step{}, unreadable("reading commit %s failed: %s", sha, detail)
		}
		return Step{}, unreadable("the forge could not be read: %v", err)
	}
	var payload struct {
		SHA     string `json:"sha"`
		Message string `json:"message"`
		Parents []struct {
			SHA string `json:"sha"`
		} `json:"parents"`
	}
	if json.Unmarshal(output, &payload) != nil || payload.SHA != sha {
		return Step{}, unreadable("the forge's answer for commit %s does not name that commit", sha)
	}
	parents := make([]string, 0, len(payload.Parents))
	for _, p := range payload.Parents {
		if !githubSHA.MatchString(p.SHA) {
			return Step{}, unreadable("the forge named %s as a parent of commit %s, which is not a full object name", pyvalue.StrRepr(excerpt(p.SHA)), sha)
		}
		parents = append(parents, p.SHA)
	}
	subject, _, _ := strings.Cut(payload.Message, "\n")
	return Step{SHA: sha, Parents: parents, Subject: subject}, nil
}
