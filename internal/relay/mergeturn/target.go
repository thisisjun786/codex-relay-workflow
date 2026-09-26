package mergeturn

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/settings"
)

// Tip is the branch's current full object name, not a caller's restatement.
type Tip struct {
	SHA        string `json:"sha"`
	Source     string `json:"source"`
	Reference  string `json:"reference"`
	Repository string `json:"repository"`
}
type TargetUnreadable struct{ Detail string }

func (e *TargetUnreadable) Error() string { return e.Detail }
func unreadable(format string, args ...any) error {
	return &TargetUnreadable{fmt.Sprintf(format, args...)}
}

var shaFull = regexp.MustCompile(`^(?:[0-9a-f]{40}|[0-9a-f]{64})$`)
var githubSHA = regexp.MustCompile(`^[0-9a-f]{40}$`)
var slug = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]{0,38}/[A-Za-z0-9._-]{1,100}$`)
var invalidRef = regexp.MustCompile(`[\x00-\x20\x7f~^:?*\[\\]`)

func branch(base string) error {
	if base == "" || base != strings.TrimSpace(base) {
		return unreadable("a base ref is a branch name without surrounding whitespace, not %s", pyRepr(base))
	}
	bad := invalidRef.MatchString(base) || strings.Contains(base, "..") || strings.Contains(base, "@{") || base == "@" || strings.HasPrefix(base, "-") || strings.HasSuffix(base, ".")
	for _, part := range strings.Split(base, "/") {
		if part == "" || strings.HasPrefix(part, ".") || strings.HasSuffix(part, ".lock") {
			bad = true
		}
	}
	if bad {
		return unreadable("a base ref is a plain branch name; %s carries revision syntax or a form git refuses, so it names no branch this can read exactly", pyRepr(base))
	}
	return nil
}
func SameCommit(a, b string) bool {
	a = strings.ToLower(strings.TrimSpace(a))
	b = strings.ToLower(strings.TrimSpace(b))
	return a != "" && a == b
}

// Reader is mergeturn.py's target_reader: where a target's base branch points now.
type Reader interface {
	Tip(ctx context.Context, repository, base string) (Tip, error)
}

// readTarget is MergeTurn._read_target: a reading, or why there is none. A nil reader is a
// relay with no target reader configured.
func readTarget(ctx context.Context, reader Reader, repository, base string) (Tip, string) {
	if reader == nil {
		return Tip{}, "this relay has no target reader configured, so it cannot read where " + pyRepr(base) + " points"
	}
	tip, err := reader.Tip(ctx, repository, base)
	if err != nil {
		return Tip{}, err.Error()
	}
	return tip, ""
}

// pyRepr is repr() of a str, as every refusal detail here spells an identifier.
func pyRepr(s string) string { return settings.Repr(s) }

type TargetReader struct {
	Git string
	GH  string
}

func (r TargetReader) Tip(ctx context.Context, repository, base string) (Tip, error) {
	if err := branch(base); err != nil {
		return Tip{}, err
	}
	if !filepath.IsAbs(repository) {
		if slug.MatchString(repository) {
			return r.github(ctx, repository, base)
		}
		return Tip{}, unreadable("repository %s is neither an absolute local path nor owner/name, so there is no target this can read", pyRepr(repository))
	}
	info, err := os.Stat(repository)
	if err != nil || !info.IsDir() {
		return Tip{}, unreadable("repository %s is not a directory here", pyRepr(repository))
	}
	gitdir := filepath.Join(repository, ".git")
	if _, err := os.Stat(gitdir); os.IsNotExist(err) {
		gitdir = repository
	}
	ref := "refs/heads/" + base
	git := r.Git
	if git == "" {
		git = "git"
	}
	cmd := exec.CommandContext(ctx, git, "--git-dir="+gitdir, "show-ref", "--verify", "--hash", ref)
	env := make([]string, 0)
	for _, v := range os.Environ() {
		if !strings.HasPrefix(v, "GIT_") {
			env = append(env, v)
		}
	}
	cmd.Env = append(env, "GIT_TERMINAL_PROMPT=0", "LC_ALL=C")
	output, err := cmd.Output()
	if err != nil {
		detail := "exit 1"
		if e, ok := err.(*exec.ExitError); ok {
			detail = excerpt(strings.TrimSpace(string(e.Stderr)))
			if detail == "" {
				detail = fmt.Sprintf("exit %d", e.ExitCode())
			}
		} else {
			return Tip{}, unreadable("git could not be started: %v", err)
		}
		detail = excerpt(detail)
		return Tip{}, unreadable("git could not read %s in %s: %s", ref, pyRepr(repository), detail)
	}
	sha := strings.TrimSpace(string(output))
	if !shaFull.MatchString(sha) {
		return Tip{}, unreadable("git answered %s for %s, which is not a full object name", pyRepr(excerpt(sha)), ref)
	}
	return Tip{SHA: sha, Source: "local_git", Reference: ref, Repository: repository}, nil
}

// github is the branch-ref and Forge.rest subset ported for todo 26; todo 24 owns forge
// and extends its observer. Only one read-only GET is performed, without a shell.
func (r TargetReader) github(ctx context.Context, repository, base string) (Tip, error) {
	if strings.ContainsAny(base, "?#%:`^\\") || strings.Contains(base, "..") || strings.HasPrefix(base, "/") {
		return Tip{}, unreadable("a branch name is a path segment without traversal or query characters, not %s", pyRepr(base))
	}
	reference := "refs/heads/" + base
	gh := r.GH
	if gh == "" {
		gh = "gh"
	}
	argv := []string{"api", "--method", "GET", "-H", "Accept: application/vnd.github+json", "repos/" + repository + "/git/ref/heads/" + base}
	timeoutCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(timeoutCtx, gh, argv...)
	output, err := cmd.Output()
	if err != nil {
		if timeoutCtx.Err() == context.DeadlineExceeded {
			return Tip{}, unreadable("reading the base branch exceeded the timeout of 30 seconds, so no answer was observed")
		}
		if e, ok := err.(*exec.ExitError); ok {
			detail := excerpt(strings.TrimSpace(string(e.Stderr)))
			if strings.Contains(detail, "HTTP 404") {
				return Tip{}, unreadable("branch %s does not exist in %s", pyRepr(base), repository)
			}
			if strings.Contains(detail, "HTTP 409") {
				return Tip{}, unreadable("%s is an empty repository", repository)
			}
			return Tip{}, unreadable("reading the base branch failed: %s", detail)
		}
		return Tip{}, unreadable("the forge could not be read: %v", err)
	}
	var payload any
	if json.Unmarshal(output, &payload) != nil {
		return Tip{}, unreadable("reading the base branch returned something that is not JSON")
	}
	data, ok := payload.(map[string]any)
	if !ok {
		return Tip{}, unreadable("the forge answered %s with something that is not one ref", reference)
	}
	object, _ := data["object"].(map[string]any)
	sha, _ := object["sha"].(string)
	if data["ref"] != reference || object["type"] != "commit" || !githubSHA.MatchString(sha) {
		return Tip{}, unreadable("the forge's answer for %s does not name that branch's commit", reference)
	}
	host := os.Getenv("GH_HOST")
	if host == "" {
		host = "github.com"
	}
	return Tip{SHA: sha, Source: "github:" + host, Reference: reference, Repository: repository}, nil
}

// excerpt is text[:EXCERPT], counted in characters as Python slices a str.
func excerpt(text string) string {
	runes := []rune(text)
	if len(runes) > 400 {
		return string(runes[:400])
	}
	return text
}
