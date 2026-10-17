package tracking

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"unicode"

	"github.com/thisisjun786/codex-relay-workflow/internal/publishpolicy"
)

type CommitStatusUpdate struct {
	State       string `json:"state"`
	Context     string `json:"context"`
	TargetURL   string `json:"target_url,omitempty"`
	Description string `json:"description,omitempty"`
}

var commitStatusHead = regexp.MustCompile("^[0-9a-f]{40}([0-9a-f]{24})?$")

var commitStatusStates = map[string]bool{"pending": true, "success": true, "failure": true, "error": true}

func (g *GitHub) SetCommitStatus(ctx context.Context, head string, status CommitStatusUpdate) error {
	if !commitStatusHead.MatchString(head) {
		return fmt.Errorf("commit status: the head is not a full lowercase hex SHA")
	}
	if !commitStatusStates[status.State] {
		return fmt.Errorf("commit status: the state is not pending, success, failure or error")
	}
	if err := checkCommitStatusText("context", status.Context, true); err != nil {
		return err
	}
	if status.TargetURL != "" {
		u, err := url.Parse(status.TargetURL)
		if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil {
			return fmt.Errorf("commit status: target_url is not an absolute http(s) URL")
		}
		if err := checkCommitStatusText("target_url", status.TargetURL, false); err != nil {
			return err
		}
	}
	for field, text := range map[string]string{"context": status.Context, "target_url": status.TargetURL} {
		if text == "" {
			continue
		}
		r := publishpolicy.Scrub(text, g.pubctx, g.policy)
		if (r.Blocked || len(r.Changes) > 0) && g.record != nil {
			g.record(publishpolicy.Event{Repo: g.repo, Surface: "github", Field: field, Changes: r.Changes, Blocked: r.Blocked})
		}
		if r.Blocked {
			return publishpolicy.ErrSecret
		}
		if r.Text != text {
			return fmt.Errorf("commit status: the %s cannot be published unchanged", field)
		}
	}
	var out CommitStatusUpdate
	if _, err := g.request(ctx, "tracking: set commit status", http.MethodPost, g.repoURL("/statuses/"+head, nil), status, &out); err != nil {
		return err
	}
	if out.State != status.State || out.Context != status.Context || out.TargetURL != status.TargetURL {
		return fmt.Errorf("tracking: set commit status: the answer does not match the request")
	}
	return nil
}

func checkCommitStatusText(field, text string, required bool) error {
	if text == "" {
		if required {
			return fmt.Errorf("commit status: %s is required", field)
		}
		return nil
	}
	if strings.TrimSpace(text) != text {
		return fmt.Errorf("commit status: %s must not have surrounding whitespace", field)
	}
	for _, r := range text {
		if unicode.IsControl(r) {
			return fmt.Errorf("commit status: %s must not carry a control character", field)
		}
	}
	return nil
}
