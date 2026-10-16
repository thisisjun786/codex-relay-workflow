// Package publication is where text bound for a public forge is made safe (CRW-1549): the shared
// scrub of publishpolicy applied with this host's context and the repository's configured policy,
// the audit event written when a field was changed or stopped, and the repository the local
// checkout publishes to.
package publication

import (
	"context"
	"encoding/json"
	"io"
	"net/url"
	"os"
	"os/exec"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/crwconfig"
	"github.com/thisisjun786/codex-relay-workflow/internal/gitprobe"
	"github.com/thisisjun786/codex-relay-workflow/internal/publishpolicy"
	"github.com/thisisjun786/codex-relay-workflow/internal/tracking"
)

// Recorder returns the event sink the publisher options take: each event one JSON object in one
// write on w. The event carries only the repository, surface, field, kinds, counts and whether the
// field was blocked — never the text or a match.
func Recorder(w io.Writer) func(publishpolicy.Event) {
	return func(ev publishpolicy.Event) {
		if data, err := json.Marshal(ev); err == nil {
			_, _ = w.Write(append(data, '\n'))
		}
	}
}

// Prepare scrubs text for surface/field of repo: a field that changed or was blocked leaves its
// audit event on w; a blocked field is ErrSecret and nothing else.
func Prepare(repo, surface, field, text string, c publishpolicy.Context, p publishpolicy.Policy, w io.Writer) (string, error) {
	r := publishpolicy.Scrub(text, c, p)
	if r.Blocked || len(r.Changes) > 0 {
		Recorder(w)(publishpolicy.Event{Repo: repo, Surface: surface, Field: field, Changes: r.Changes, Blocked: r.Blocked})
	}
	if r.Blocked {
		return "", publishpolicy.ErrSecret
	}
	return r.Text, nil
}

// Policy is the publication policy of repo: the defaults for an empty repo, else the repository's
// entry of the configuration file.
func Policy(repo string, getenv func(string) string) (publishpolicy.Policy, error) {
	if repo == "" {
		return publishpolicy.Policy{}, nil
	}
	return crwconfig.LoadPublicationPolicy(getenv, repo)
}

// NewGitHub is the tracking adapter for repo with the publication scrub wired: the configured
// policy, this host's context and the stderr event sink.
func NewGitHub(repo string) (*tracking.GitHub, error) {
	p, err := Policy(repo, os.Getenv)
	if err != nil {
		return nil, err
	}
	return tracking.NewGitHub(repo, tracking.DefaultTokenSource(),
		tracking.WithPublication(p, publishpolicy.HostContext(), Recorder(os.Stderr)))
}

// Repository is the OWNER/NAME a github.com remote points at, "" for anything else: an https or ssh
// URL on exactly github.com, or the scp spelling git@github.com:o/r, with an optional .git suffix
// and nothing after it (no query, no fragment). The result is judged with crwconfig.ValidateRepo;
// the remote is never echoed because the answer is just "".
func Repository(remote string) string {
	s := strings.TrimSpace(remote)
	if rest, ok := strings.CutPrefix(s, "git@github.com:"); ok {
		s = "https://github.com/" + rest
	}
	u, err := url.Parse(s)
	if err != nil || u.Host != "github.com" || (u.Scheme != "https" && u.Scheme != "ssh") || u.RawQuery != "" || u.Fragment != "" {
		return ""
	}
	repo := strings.TrimSuffix(strings.Trim(u.Path, "/"), ".git")
	if crwconfig.ValidateRepo(repo) != nil {
		return ""
	}
	return repo
}

// ForCheckout is the repository and policy of the checkout at dir: GH_REPO (OWNER/NAME, a
// github.com/OWNER/NAME URL or the bare github.com/OWNER/NAME) wins, else the origin remote of the
// checkout, read with the routing variables removed so an inherited one cannot point the lookup at
// another repository. A checkout whose repository cannot be resolved publishes with the default
// policy; a named repository whose policy cannot be read is an error.
func ForCheckout(ctx context.Context, dir string) (string, publishpolicy.Policy, error) {
	repo := ""
	if gh := strings.TrimSpace(os.Getenv("GH_REPO")); gh != "" {
		if repo = Repository(gh); repo == "" {
			if repo = Repository("https://" + gh); repo == "" {
				if r := strings.TrimPrefix(gh, "github.com/"); crwconfig.ValidateRepo(r) == nil {
					repo = r
				}
			}
		}
	}
	if repo == "" {
		cmd := exec.CommandContext(ctx, "git", gitprobe.Args(dir, "remote", "get-url", "origin")...)
		cmd.Env = gitprobe.ProbeEnv(nil)
		if out, err := cmd.Output(); err == nil {
			repo = Repository(string(out))
		}
	}
	if repo == "" {
		return "", publishpolicy.Policy{}, nil
	}
	p, err := Policy(repo, os.Getenv)
	if err != nil {
		return "", publishpolicy.Policy{}, err
	}
	return repo, p, nil
}
