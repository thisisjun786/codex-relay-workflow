package tracking

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

// PullRequests is the pull request side of the adapter (CRW-1355): a project session opens a pull request from its milestone
// integration branch toward dev naming the issues it carries, and reads its state and the checks of its head; the dev queue reads
// it before it merges the head. The operations go through the transport of the issue operations: the same token, headers, typed
// errors and repository check. Issue still refuses a pull request (ErrNotIssue).
type PullRequests interface {
	// OpenPullRequest opens a pull request in the tracker's repository from the branch in.Head onto in.Base. The body is in.Body
	// followed by one "Carries:" line naming in.Issues.
	OpenPullRequest(ctx context.Context, in NewPullRequest) (PullRequest, error)
	// PullRequest reads one pull request: its state, whether it is merged, its head and base, and the issues its body carries.
	PullRequest(ctx context.Context, ref Ref) (PullRequest, error)
	// Checks reads the check runs and commit statuses of the pull request's head commit and judges them together.
	Checks(ctx context.Context, ref Ref) (Checks, error)
}

var _ PullRequests = (*GitHub)(nil)

// ErrInvalidPullRequest is the error of a pull request the adapter will not open: a head or base that is not a branch name of the
// repository, a head that is the base, a title that is not one line, or no issue to carry.
var ErrInvalidPullRequest = errors.New("tracking: invalid pull request")

// NewPullRequest is what OpenPullRequest sends.
type NewPullRequest struct {
	// Head is the short name of the branch of the tracker's repository the pull request comes from (a milestone integration
	// branch); Base the short name of the branch it goes to (dev).
	Head, Base string
	Title      string
	// Body is the text above the Carries: line; it may be empty.
	Body string
	// Issues are the issues the pull request carries, named once each, in order, on the Carries: line.
	Issues []Ref
	Draft  bool
}

// PullRequest is a pull request of the tracking repository.
type PullRequest struct {
	Ref   Ref
	URL   string
	Title string
	Body  string
	State State
	// Merged is whether GitHub counts the pull request merged (its head reached the base).
	Merged bool
	Draft  bool
	// HeadRef and HeadSHA are the branch and the commit the pull request carries; HeadRepo is the repository the branch is in
	// ("owner/name"), the tracker's own unless the pull request comes from a fork.
	HeadRef, HeadSHA, HeadRepo string
	BaseRef                    string
	// Issues are the issues named on the "Carries:" lines of Body.
	Issues []Ref
}

// CheckState is the judgment of the checks of a head.
type CheckState string

const (
	// CheckNone is a head with no check run and no commit status.
	CheckNone CheckState = "none"
	// CheckPending is a head with a check that has not finished and none that failed.
	CheckPending CheckState = "pending"
	// CheckSuccess is a head whose every check finished as success, neutral or skipped.
	CheckSuccess CheckState = "success"
	// CheckFailure is a head with a check that finished in any other way.
	CheckFailure CheckState = "failure"
)

// The kinds of a Check.
const (
	CheckRun     = "check_run"
	CommitStatus = "status"
)

// Check is one check run or commit status of a head.
type Check struct {
	Name string
	// Kind is CheckRun or CommitStatus.
	Kind string
	// Status is a check run's status (queued, in_progress, completed) or a commit status's state (pending, success, failure, error).
	Status string
	// Conclusion is a completed check run's conclusion; empty for a commit status.
	Conclusion string
	URL        string
	State      CheckState
}

// Checks is what Checks reads: the checks of the pull request's head and their judgment together.
type Checks struct {
	PullRequest Ref
	Head        string
	State       CheckState
	Checks      []Check
}

// apiPull is the part of a GitHub pull request the adapter reads.
type apiPull struct {
	Number  int    `json:"number"`
	Title   string `json:"title"`
	Body    string `json:"body"`
	State   string `json:"state"`
	Merged  bool   `json:"merged"`
	Draft   bool   `json:"draft"`
	HTMLURL string `json:"html_url"`
	Head    struct {
		Ref  string `json:"ref"`
		SHA  string `json:"sha"`
		Repo *struct {
			FullName string `json:"full_name"`
		} `json:"repo"`
	} `json:"head"`
	Base struct {
		Ref string `json:"ref"`
	} `json:"base"`
}

func (g *GitHub) pullFrom(a apiPull) PullRequest {
	pr := PullRequest{
		Ref: Ref{Repo: g.repo, Number: a.Number}, URL: a.HTMLURL, Title: a.Title, Body: a.Body, State: State(a.State), Merged: a.Merged, Draft: a.Draft,
		HeadRef: a.Head.Ref, HeadSHA: a.Head.SHA, BaseRef: a.Base.Ref, Issues: ParseCarries(g.repo, a.Body),
	}
	if a.Head.Repo != nil {
		pr.HeadRepo = a.Head.Repo.FullName
		if strings.EqualFold(pr.HeadRepo, g.repo) {
			pr.HeadRepo = g.repo
		}
	}
	return pr
}

var branchName = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._/-]*$`)

// checkBranch accepts the short name of a branch of the repository: no fork prefix (owner:branch), no full ref, nothing git refuses
// in a branch name that this check can see.
func checkBranch(what, name string) error {
	if !branchName.MatchString(name) || strings.Contains(name, "..") || strings.Contains(name, "//") || strings.HasSuffix(name, "/") ||
		strings.HasSuffix(name, ".lock") || strings.HasSuffix(name, ".") || strings.HasPrefix(name, "refs/") {
		return fmt.Errorf("%w: the %s %q is not the short name of a branch of the repository", ErrInvalidPullRequest, what, name)
	}
	return nil
}

// Check is the refusal OpenPullRequest gives in before it sends anything (ErrInvalidPullRequest), nil when in can be sent.
func (in NewPullRequest) Check() error {
	_, err := in.body()
	return err
}

// body checks in and makes the body: the text, a blank line and the Carries: line naming each issue once, in order.
func (in NewPullRequest) body() (string, error) {
	if err := checkBranch("head", in.Head); err != nil {
		return "", err
	}
	if err := checkBranch("base", in.Base); err != nil {
		return "", err
	}
	if in.Head == in.Base {
		return "", fmt.Errorf("%w: the head %q is the base", ErrInvalidPullRequest, in.Head)
	}
	if strings.TrimSpace(in.Title) == "" || strings.ContainsAny(in.Title, "\r\n") {
		return "", fmt.Errorf("%w: the title %q is not one line", ErrInvalidPullRequest, in.Title)
	}
	var carried []string
	seen := map[Ref]bool{}
	for _, is := range in.Issues {
		if is.Number <= 0 || !validRepo(is.Repo) {
			return "", fmt.Errorf("%w: the issue %s is not <owner>/<name>#N", ErrInvalidPullRequest, is)
		}
		if !seen[is] {
			seen[is] = true
			carried = append(carried, is.String())
		}
	}
	if len(carried) == 0 {
		return "", fmt.Errorf("%w: a pull request names the issues it carries", ErrInvalidPullRequest)
	}
	body := "Carries: " + strings.Join(carried, ", ")
	if text := strings.TrimRight(in.Body, "\r\n \t"); text != "" {
		body = text + "\n\n" + body
	}
	return body, nil
}

// OpenPullRequest opens the pull request; its body ends with the Carries: line of the issues.
func (g *GitHub) OpenPullRequest(ctx context.Context, in NewPullRequest) (PullRequest, error) {
	body, err := in.body()
	if err != nil {
		return PullRequest{}, err
	}
	var a apiPull
	op := "tracking: open pull request " + in.Head + " -> " + in.Base + " in " + g.repo
	send := map[string]any{"head": in.Head, "base": in.Base, "title": in.Title, "body": body, "draft": in.Draft}
	if _, err := g.request(ctx, op, http.MethodPost, g.repoURL("/pulls", nil), send, &a); err != nil {
		return PullRequest{}, err
	}
	return g.pullFrom(a), nil
}

// PullRequest reads the pull request ref.
func (g *GitHub) PullRequest(ctx context.Context, ref Ref) (PullRequest, error) {
	if err := g.checkRef(ref); err != nil {
		return PullRequest{}, err
	}
	var a apiPull
	if _, err := g.request(ctx, "tracking: read pull request "+ref.String(), http.MethodGet, g.repoURL("/pulls/"+strconv.Itoa(ref.Number), nil), nil, &a); err != nil {
		return PullRequest{}, err
	}
	return g.pullFrom(a), nil
}

// Checks reads the pull request, then the check runs and the commit statuses of its head, every page of each.
func (g *GitHub) Checks(ctx context.Context, ref Ref) (Checks, error) {
	pr, err := g.PullRequest(ctx, ref)
	if err != nil {
		return Checks{}, err
	}
	if !headSHA.MatchString(pr.HeadSHA) {
		return Checks{}, fmt.Errorf("tracking: read checks of %s: the pull request names no head commit (%q)", ref, pr.HeadSHA)
	}
	out := Checks{PullRequest: pr.Ref, Head: pr.HeadSHA, State: CheckNone}
	query := url.Values{"per_page": {pageSize}}
	type runsPage struct {
		CheckRuns []struct {
			Name       string `json:"name"`
			Status     string `json:"status"`
			Conclusion string `json:"conclusion"`
			HTMLURL    string `json:"html_url"`
		} `json:"check_runs"`
	}
	if err := getPages(ctx, g, "tracking: list check runs of "+ref.String(), g.repoURL("/commits/"+pr.HeadSHA+"/check-runs", query), func(p runsPage) {
		for _, r := range p.CheckRuns {
			out.Checks = append(out.Checks, Check{Name: r.Name, Kind: CheckRun, Status: r.Status, Conclusion: r.Conclusion, URL: r.HTMLURL, State: runState(r.Status, r.Conclusion)})
		}
	}); err != nil {
		return Checks{}, err
	}
	type statusPage struct {
		Statuses []struct {
			Context   string `json:"context"`
			State     string `json:"state"`
			TargetURL string `json:"target_url"`
		} `json:"statuses"`
	}
	if err := getPages(ctx, g, "tracking: read commit status of "+ref.String(), g.repoURL("/commits/"+pr.HeadSHA+"/status", query), func(p statusPage) {
		for _, s := range p.Statuses {
			out.Checks = append(out.Checks, Check{Name: s.Context, Kind: CommitStatus, Status: s.State, URL: s.TargetURL, State: statusState(s.State)})
		}
	}); err != nil {
		return Checks{}, err
	}
	for _, c := range out.Checks {
		out.State = worse(out.State, c.State)
	}
	return out, nil
}

var headSHA = regexp.MustCompile(`^[0-9a-f]{40}$`)

func runState(status, conclusion string) CheckState {
	if status != "completed" {
		return CheckPending
	}
	switch conclusion {
	case "success", "neutral", "skipped":
		return CheckSuccess
	}
	return CheckFailure
}

func statusState(state string) CheckState {
	switch state {
	case "success":
		return CheckSuccess
	case "pending":
		return CheckPending
	}
	return CheckFailure
}

// worse is the judgment of two together: a failure over anything, then pending, then success, then none.
func worse(a, b CheckState) CheckState {
	rank := map[CheckState]int{CheckNone: 0, CheckSuccess: 1, CheckPending: 2, CheckFailure: 3}
	if rank[b] > rank[a] {
		return b
	}
	return a
}

// getPages reads every page of an answer that is an object, following the Link header's rel="next" on the API root's host, and
// hands each page to each.
func getPages[T any](ctx context.Context, g *GitHub, op, first string, each func(T)) error {
	next := first
	for page := 0; next != ""; page++ {
		if page == maxPages {
			return fmt.Errorf("%s: more than %d pages", op, maxPages)
		}
		var p T
		header, err := g.request(ctx, op, http.MethodGet, next, nil, &p)
		if err != nil {
			return err
		}
		each(p)
		if next, err = g.nextLink(header); err != nil {
			return fmt.Errorf("%s: %w", op, err)
		}
	}
	return nil
}
