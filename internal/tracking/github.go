package tracking

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/publishpolicy"
)

const (
	defaultBaseURL = "https://api.github.com"
	apiVersion     = "2022-11-28"
	pageSize       = "100"
	// maxPages bounds one list read, so a server that never ends its Link chain ends the read.
	maxPages = 200
	// maxBody bounds what one answer may weigh.
	maxBody = 16 << 20
	// maxMessage bounds the GitHub message an APIError keeps.
	maxMessage = 500
)

// GitHub is a Tracker over the GitHub REST API for one repository. It sends the token its
// TokenSource gives with every request and to no other host than its API root, follows no redirect
// (a moved repository is an *APIError of status 301) and retries nothing: a rate limit comes back as
// a *RateLimitError and the caller decides when to ask again.
type GitHub struct {
	repo    string
	baseRaw string
	base    *url.URL
	client  *http.Client
	tokens  TokenSource
	// policy, pubctx and record are the publication scrub (CRW-1549): what is replaced in text
	// bound for the forge, what is local to this host, and where a changed or blocked field is
	// recorded. pubInit marks the option given so an explicit nil recorder is honoured.
	policy  publishpolicy.Policy
	pubctx  publishpolicy.Context
	record  func(publishpolicy.Event)
	pubInit bool
}

var _ Tracker = (*GitHub)(nil)

// Option changes a GitHub made by NewGitHub.
type Option func(*GitHub)

// WithBaseURL sets the API root, "https://api.github.com" by default; a test points it at a fake.
func WithBaseURL(u string) Option { return func(g *GitHub) { g.baseRaw = u } }

// WithHTTPClient sets the client the requests go through. A client without a CheckRedirect gets one
// that follows no redirect.
func WithHTTPClient(c *http.Client) Option { return func(g *GitHub) { g.client = c } }

// WithPublication sets the publication scrub (CRW-1549): the repository's policy and the local
// context the public text is scrubbed with, and where the event of a changed or blocked field is
// written. A nil record asks for no event; without the option the event is one JSON line on stderr.
func WithPublication(policy publishpolicy.Policy, context publishpolicy.Context, record func(publishpolicy.Event)) Option {
	return func(g *GitHub) {
		g.policy, g.pubctx, g.record, g.pubInit = policy, context, record, true
	}
}

// recordPublication is the default event sink: one JSON object, one write, on stderr. The event
// carries the repository, surface and field and the kinds and counts of the replacements — never
// the text or a match.
func recordPublication(ev publishpolicy.Event) {
	if data, err := json.Marshal(ev); err == nil {
		_, _ = os.Stderr.Write(append(data, '\n'))
	}
}

// NewGitHub makes a Tracker for the repository "owner/name". Every Ref it is given must name that
// repository, compared without regard to case.
func NewGitHub(repo string, tokens TokenSource, opts ...Option) (*GitHub, error) {
	if !validRepo(repo) {
		return nil, fmt.Errorf("tracking: repository %q is not owner/name", repo)
	}
	if tokens == nil {
		return nil, errors.New("tracking: no token source")
	}
	g := &GitHub{repo: repo, baseRaw: defaultBaseURL, tokens: tokens}
	for _, o := range opts {
		o(g)
	}
	if !g.pubInit {
		g.policy, g.pubctx, g.record = publishpolicy.Policy{}, publishpolicy.HostContext(), recordPublication
	}
	if err := g.policy.Validate(); err != nil {
		return nil, fmt.Errorf("tracking: publication policy: %w", err)
	}
	base, err := url.Parse(strings.TrimRight(g.baseRaw, "/"))
	if err != nil || (base.Scheme != "https" && base.Scheme != "http") || base.Host == "" {
		return nil, fmt.Errorf("tracking: API root %q is not an http(s) URL", g.baseRaw)
	}
	g.base = base
	switch {
	case g.client == nil:
		g.client = &http.Client{Timeout: 30 * time.Second}
		fallthrough
	case g.client.CheckRedirect == nil:
		c := *g.client
		c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
		g.client = &c
	}
	return g, nil
}

// apiIssue is the part of a GitHub issue the adapter reads.
type apiIssue struct {
	Number        int       `json:"number"`
	Title         string    `json:"title"`
	Body          string    `json:"body"`
	State         string    `json:"state"`
	StateReason   string    `json:"state_reason"`
	HTMLURL       string    `json:"html_url"`
	RepositoryURL string    `json:"repository_url"`
	PullRequest   *struct{} `json:"pull_request"`
	Labels        []struct {
		Name string `json:"name"`
	} `json:"labels"`
	Milestone *struct {
		Number int `json:"number"`
	} `json:"milestone"`
}

func (g *GitHub) issueFrom(a apiIssue) Issue {
	is := Issue{
		Ref: Ref{Repo: g.repo, Number: a.Number}, URL: a.HTMLURL, Title: a.Title, Body: a.Body,
		State: State(a.State), Labels: make([]string, 0, len(a.Labels)),
		BlockedBy: ParseBlockedBy(g.repo, a.Body),
	}
	if is.State == StateClosed && (a.StateReason == string(Completed) || a.StateReason == string(NotPlanned)) {
		is.CloseReason = CloseReason(a.StateReason)
	}
	for _, l := range a.Labels {
		is.Labels = append(is.Labels, l.Name)
	}
	if a.Milestone != nil {
		is.Milestone = a.Milestone.Number
	}
	return is
}

// Milestone reads the milestone and its issues, open and closed, page by page.
func (g *GitHub) Milestone(ctx context.Context, ref Ref) (Milestone, []Issue, error) {
	if err := g.checkRef(ref); err != nil {
		return Milestone{}, nil, err
	}
	var m struct {
		Number       int    `json:"number"`
		Title        string `json:"title"`
		Description  string `json:"description"`
		State        string `json:"state"`
		OpenIssues   int    `json:"open_issues"`
		ClosedIssues int    `json:"closed_issues"`
	}
	op := "tracking: read milestone " + ref.String()
	if _, err := g.request(ctx, op, http.MethodGet, g.repoURL("/milestones/"+strconv.Itoa(ref.Number), nil), nil, &m); err != nil {
		return Milestone{}, nil, err
	}
	query := url.Values{"milestone": {strconv.Itoa(ref.Number)}, "state": {"all"}, "per_page": {pageSize}}
	var raw []apiIssue
	if err := getAll(ctx, g, "tracking: list issues of milestone "+ref.String(), g.repoURL("/issues", query), &raw); err != nil {
		return Milestone{}, nil, err
	}
	var issues []Issue
	for _, a := range raw {
		if a.PullRequest == nil {
			issues = append(issues, g.issueFrom(a))
		}
	}
	return Milestone{
		Ref: Ref{Repo: g.repo, Number: ref.Number}, Title: m.Title, Description: m.Description, State: State(m.State),
		OpenIssues: m.OpenIssues, ClosedIssues: m.ClosedIssues,
	}, issues, nil
}

// Issue reads the issue and its sub-issues. A pull request is ErrNotIssue.
func (g *GitHub) Issue(ctx context.Context, ref Ref) (Issue, error) {
	if err := g.checkRef(ref); err != nil {
		return Issue{}, err
	}
	n := strconv.Itoa(ref.Number)
	var a apiIssue
	if _, err := g.request(ctx, "tracking: read issue "+ref.String(), http.MethodGet, g.repoURL("/issues/"+n, nil), nil, &a); err != nil {
		return Issue{}, err
	}
	if a.PullRequest != nil {
		return Issue{}, fmt.Errorf("%w: %s is a pull request", ErrNotIssue, ref)
	}
	var subs []apiIssue
	query := url.Values{"per_page": {pageSize}}
	if err := getAll(ctx, g, "tracking: list sub-issues of "+ref.String(), g.repoURL("/issues/"+n+"/sub_issues", query), &subs); err != nil {
		return Issue{}, err
	}
	is := g.issueFrom(a)
	for _, s := range subs {
		is.SubIssues = append(is.SubIssues, SubIssue{Ref: g.subRef(s), Title: s.Title, State: State(s.State)})
	}
	return is, nil
}

// subRef names a sub-issue by the repository its repository_url says; a sub-issue can live in
// another repository than its parent.
func (g *GitHub) subRef(s apiIssue) Ref {
	repo := g.repo
	if u, err := url.Parse(s.RepositoryURL); err == nil {
		parts := strings.Split(strings.Trim(u.Path, "/"), "/")
		if n := len(parts); n >= 3 && parts[n-3] == "repos" && validRepo(parts[n-2]+"/"+parts[n-1]) {
			if r := parts[n-2] + "/" + parts[n-1]; !strings.EqualFold(r, g.repo) {
				repo = r
			}
		}
	}
	return Ref{Repo: repo, Number: s.Number}
}

// Comment writes a comment of header, a blank line and text; with no text the comment is the header.
func (g *GitHub) Comment(ctx context.Context, ref Ref, header, text string) error {
	if err := g.checkRef(ref); err != nil {
		return err
	}
	// a secret in the header is stopped before the shape check, whose error would name it (CRW-1549).
	if r := publishpolicy.Scrub(header, g.pubctx, g.policy); r.Blocked {
		return publishpolicy.ErrSecret
	}
	if err := checkHeader(header); err != nil {
		return err
	}
	body := header
	if text != "" {
		body += "\n\n" + text
	}
	_, err := g.request(ctx, "tracking: comment on "+ref.String(), http.MethodPost,
		g.repoURL("/issues/"+strconv.Itoa(ref.Number)+"/comments", nil), map[string]string{"body": body}, nil)
	return err
}

// HasComment is whether some comment of the issue or pull request has header as its first line: the comment is the header
// alone, or the header and a line break (LF or CRLF) and more. A header inside the text, or a first line that is longer than
// the header, does not match. It reads every page of the comments (an answer that cannot be read is an error, never false), so
// a caller that looks before it posts again sees the comments that a crashed run wrote.
func (g *GitHub) HasComment(ctx context.Context, ref Ref, header string) (bool, error) {
	if err := g.checkRef(ref); err != nil {
		return false, err
	}
	// a secret in the header is stopped before the shape check, whose error would name it (CRW-1549);
	// the stored header was scrubbed before it was posted, so the comparison is against the sanitized
	// text — a stable id pseudonym still matches itself.
	r := publishpolicy.Scrub(header, g.pubctx, g.policy)
	if r.Blocked {
		return false, publishpolicy.ErrSecret
	}
	if err := checkHeader(header); err != nil {
		return false, err
	}
	header = r.Text
	var comments []struct {
		Body string `json:"body"`
	}
	query := url.Values{"per_page": {pageSize}}
	if err := getAll(ctx, g, "tracking: list comments of "+ref.String(),
		g.repoURL("/issues/"+strconv.Itoa(ref.Number)+"/comments", query), &comments); err != nil {
		return false, err
	}
	for _, c := range comments {
		first, _, _ := strings.Cut(c.Body, "\n")
		if strings.TrimSuffix(first, "\r") == header {
			return true, nil
		}
	}
	return false, nil
}

// Close closes the issue as completed or not planned.
func (g *GitHub) Close(ctx context.Context, ref Ref, reason CloseReason) error {
	if err := g.checkRef(ref); err != nil {
		return err
	}
	if reason != Completed && reason != NotPlanned {
		return fmt.Errorf("%w: %q", ErrInvalidReason, reason)
	}
	_, err := g.request(ctx, "tracking: close "+ref.String(), http.MethodPatch,
		g.repoURL("/issues/"+strconv.Itoa(ref.Number), nil),
		map[string]string{"state": "closed", "state_reason": string(reason)}, nil)
	return err
}

// AddLabel adds one label to the issue; its other labels stay.
func (g *GitHub) AddLabel(ctx context.Context, ref Ref, label string) error {
	if err := g.checkRef(ref); err != nil {
		return err
	}
	if strings.TrimSpace(label) == "" || strings.ContainsAny(label, "\r\n") {
		return fmt.Errorf("%w: %q", ErrInvalidLabel, label)
	}
	_, err := g.request(ctx, "tracking: label "+ref.String(), http.MethodPost,
		g.repoURL("/issues/"+strconv.Itoa(ref.Number)+"/labels", nil), map[string][]string{"labels": {label}}, nil)
	return err
}

func (g *GitHub) checkRef(ref Ref) error {
	if ref.Number <= 0 {
		return fmt.Errorf("%w: %s", ErrInvalidRef, ref)
	}
	if !strings.EqualFold(ref.Repo, g.repo) {
		return fmt.Errorf("%w: %s, tracker is for %s", ErrOtherRepository, ref, g.repo)
	}
	return nil
}

// repoURL is the URL of path under /repos/<owner>/<name>.
func (g *GitHub) repoURL(path string, query url.Values) string {
	u := g.base.String() + "/repos/" + g.repo + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	return u
}

// token asks the source and refuses a token that cannot be a header value.
func (g *GitHub) token(ctx context.Context) (string, error) {
	t, err := g.tokens.Token(ctx)
	if err != nil {
		return "", fmt.Errorf("tracking: token: %w", err)
	}
	t = strings.TrimSpace(t)
	if t == "" {
		return "", fmt.Errorf("tracking: token: %w", ErrNoToken)
	}
	for i := 0; i < len(t); i++ {
		if t[i] < 0x20 || t[i] == 0x7f {
			return "", errors.New("tracking: token: the token source gave a token with a control character")
		}
	}
	return t, nil
}

// request sends one request and decodes a success answer into out (when not nil). It returns the
// answer's headers.
func (g *GitHub) request(ctx context.Context, op, method, rawURL string, body, out any) (http.Header, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("%s: %w", op, err)
	}
	var rd io.Reader
	if body != nil {
		// the public text fields of the whole outbound body are scrubbed before a token is asked
		// for and before anything is written: a blocked field is ErrSecret and no request is sent
		// (CRW-1549).
		b, err := g.publicJSON(body)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", op, err)
		}
		rd = bytes.NewReader(b)
	}
	token, err := g.token(ctx)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", op, err)
	}
	req, err := http.NewRequestWithContext(ctx, method, rawURL, rd)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", op, err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", apiVersion)
	req.Header.Set("User-Agent", "crw-tracking")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := g.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", op, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if err != nil {
		return nil, fmt.Errorf("%s: read answer: %w", op, err)
	}
	if len(data) > maxBody {
		return nil, fmt.Errorf("%s: answer over %d bytes", op, maxBody)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, apiError(op, resp, data, token)
	}
	if out != nil {
		if err := json.Unmarshal(data, out); err != nil {
			return nil, fmt.Errorf("%s: decode answer: %w", op, err)
		}
	}
	return resp.Header, nil
}

// publicFields are the root-level JSON fields of a write request that carry prose to the forge:
// issue and pull request titles and bodies, milestone descriptions, comment bodies.
var publicFields = []string{"title", "body", "description", "message"}

// publicJSON is body marshalled with every public text field scrubbed (CRW-1549): each named string
// field passes the publication policy and host context in one pass, a changed or blocked field is
// recorded (the event never carries the text), a blocked field answers ErrSecret so no request is
// sent, and a non-string one (a null is kept, for a PATCH that clears) is refused. Every other field
// is preserved as it was marshalled.
func (g *GitHub) publicJSON(body any) ([]byte, error) {
	b, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(b, &doc); err != nil || doc == nil {
		return b, err
	}
	for _, key := range publicFields {
		raw, ok := doc[key]
		if !ok {
			continue
		}
		// a JSON null is a PATCH that clears the field and is kept as it is, never turned into text
		if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			continue
		}
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return nil, fmt.Errorf("the public field %q is not a string", key)
		}
		r := publishpolicy.Scrub(s, g.pubctx, g.policy)
		if (r.Blocked || len(r.Changes) > 0) && g.record != nil {
			g.record(publishpolicy.Event{Repo: g.repo, Surface: "github", Field: key, Changes: r.Changes, Blocked: r.Blocked})
		}
		if r.Blocked {
			return nil, publishpolicy.ErrSecret
		}
		if r.Text != s {
			data, err := json.Marshal(r.Text)
			if err != nil {
				return nil, err
			}
			doc[key] = data
		}
	}
	return json.Marshal(doc)
}

// getAll reads every page of a list into out, following the Link header's rel="next". The next link
// must stay on the API root's host, so the token goes nowhere else.
func getAll[T any](ctx context.Context, g *GitHub, op, first string, out *[]T) error {
	next := first
	for page := 0; next != ""; page++ {
		if page == maxPages {
			return fmt.Errorf("%s: more than %d pages", op, maxPages)
		}
		var items []T
		header, err := g.request(ctx, op, http.MethodGet, next, nil, &items)
		if err != nil {
			return err
		}
		*out = append(*out, items...)
		if next, err = g.nextLink(header); err != nil {
			return fmt.Errorf("%s: %w", op, err)
		}
	}
	return nil
}

// nextLink is the rel="next" target of a Link header, "" at the last page.
func (g *GitHub) nextLink(h http.Header) (string, error) {
	for _, field := range h.Values("Link") {
		for _, part := range strings.Split(field, ",") {
			target, params, _ := strings.Cut(part, ";")
			if !strings.Contains(strings.ReplaceAll(params, " ", ""), `rel="next"`) {
				continue
			}
			target = strings.Trim(strings.TrimSpace(target), "<>")
			u, err := url.Parse(target)
			if err != nil || u.Scheme != g.base.Scheme || u.Host != g.base.Host {
				return "", errors.New("next page link leaves the API root")
			}
			return u.String(), nil
		}
	}
	return "", nil
}

// apiError makes the typed error of a non-success answer.
func apiError(op string, resp *http.Response, data []byte, token string) error {
	var msg struct {
		Message string `json:"message"`
		DocURL  string `json:"documentation_url"`
	}
	_ = json.Unmarshal(data, &msg)
	message := msg.Message
	if token != "" {
		message = strings.ReplaceAll(message, token, "[redacted]")
	}
	if len(message) > maxMessage {
		message = message[:maxMessage]
	}
	e := APIError{Op: op, Status: resp.StatusCode, Message: message, DocURL: msg.DocURL}
	h := resp.Header
	limited := resp.StatusCode == http.StatusTooManyRequests ||
		(resp.StatusCode == http.StatusForbidden && (h.Get("X-RateLimit-Remaining") == "0" || h.Get("Retry-After") != "" ||
			strings.Contains(strings.ToLower(message), "rate limit")))
	if !limited {
		return &e
	}
	rl := &RateLimitError{APIError: e}
	if secs, err := strconv.ParseInt(h.Get("X-RateLimit-Reset"), 10, 64); err == nil && secs > 0 {
		rl.Reset = time.Unix(secs, 0)
	}
	if secs, err := strconv.Atoi(h.Get("Retry-After")); err == nil && secs > 0 {
		rl.RetryAfter = time.Duration(secs) * time.Second
	}
	return rl
}
