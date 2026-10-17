package tracking

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
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

// maxBodyChars is the longest body GitHub accepts for a pull request, in characters.
const maxBodyChars = 65536

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
	if n := utf8.RuneCountInString(body); n > maxBodyChars {
		return "", fmt.Errorf("%w: the body is %d characters with its Carries: line, over the %d GitHub accepts", ErrInvalidPullRequest, n, maxBodyChars)
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

// ---- review comment records and collection (CRW-1460) ----

// PRReviewSchema names the record `crw flow pr comments` writes: the known review bots' comments of a pull request,
// deduplicated into items, each decided applied or rejected with its reason, over at most two rounds.
const PRReviewSchema = "crw-pr-review/1"

// The values of a record's Bots and the bot names an item's comment carries.
const (
	ReviewBotsConnected    = "connected"
	ReviewBotsNotConnected = "not_connected"
	ReviewBotDevin         = "devin"
	ReviewBotCodex         = "codex"
)

// The states a record's State carries.
const (
	ReviewStatePending      = "pending"
	ReviewStateComplete     = "complete"
	ReviewStateNotConnected = "not_connected"
	ReviewStateRoundLimit   = "round_limit"
)

// The dispositions an item's Disposition carries.
const (
	DispositionApplied  = "applied"
	DispositionRejected = "rejected"
)

// maxReviewRecord bounds the record file ReadPRReviewRecord accepts.
const maxReviewRecord = 16 << 20

// PRReviewComment is one comment a known review bot left on the pull request.
type PRReviewComment struct {
	// ID is the source kind and the numeric ID of the comment: "issue:<n>", "review:<n>" or "inline:<n>".
	ID   string `json:"id"`
	Bot  string `json:"bot"` // devin or codex
	Body string `json:"body"`
	Path string `json:"path,omitempty"`
	Line int    `json:"line,omitempty"`
	URL  string `json:"url,omitempty"`
}

// PRReviewItem is one deduplicated finding: every bot comment that observed it, and its disposition once decided.
type PRReviewItem struct {
	Key      string            `json:"key"`
	Comments []PRReviewComment `json:"comments"`
	// Disposition is applied or rejected once decided, empty while the item is unresolved.
	Disposition string `json:"disposition,omitempty"`
	Reason      string `json:"reason,omitempty"`
}

// PRReviewRecord is the persisted state of the bot review of one pull request head.
type PRReviewRecord struct {
	Schema string `json:"schema"`
	PR     string `json:"pr"`
	Head   string `json:"head"`
	// Bots is connected or not_connected.
	Bots  string `json:"bots"`
	Round int    `json:"round"`
	// State is pending, complete, not_connected or round_limit.
	State string         `json:"state"`
	Note  string         `json:"note,omitempty"`
	Items []PRReviewItem `json:"items"`
}

// validate is the record check every reader runs: schema, pull request ref, head, round 1..2, bots and state consistent,
// unique nonempty keys, nonempty comments per item, only dispositions applied/rejected or empty with a nonblank reason on
// either, and no items on a not_connected record.
func (r PRReviewRecord) validate() error {
	if r.Schema != PRReviewSchema {
		return fmt.Errorf("tracking: review record schema %q is not %q", r.Schema, PRReviewSchema)
	}
	if _, err := ParseRef(r.PR); err != nil {
		return fmt.Errorf("tracking: review record pr %q: %w", r.PR, err)
	}
	if !headSHA.MatchString(r.Head) {
		return fmt.Errorf("tracking: review record head %q is not a full commit id", r.Head)
	}
	if r.Round < 1 || r.Round > 2 {
		return fmt.Errorf("tracking: review record round %d is not 1 or 2", r.Round)
	}
	if r.Bots != ReviewBotsConnected && r.Bots != ReviewBotsNotConnected {
		return fmt.Errorf("tracking: review record bots %q is not %q or %q", r.Bots, ReviewBotsConnected, ReviewBotsNotConnected)
	}
	switch r.State {
	case ReviewStateNotConnected:
		if r.Bots != ReviewBotsNotConnected {
			return fmt.Errorf("tracking: review record state %q with bots %q", r.State, r.Bots)
		}
	case ReviewStatePending, ReviewStateComplete, ReviewStateRoundLimit:
		if r.Bots != ReviewBotsConnected {
			return fmt.Errorf("tracking: review record state %q with bots %q", r.State, r.Bots)
		}
	default:
		return fmt.Errorf("tracking: review record state %q", r.State)
	}
	if r.Bots == ReviewBotsNotConnected && len(r.Items) != 0 {
		return errors.New("tracking: a not_connected review record carries items")
	}
	seen := map[string]bool{}
	sources := map[string]bool{}
	unresolved := 0
	for _, it := range r.Items {
		if it.Key == "" {
			return errors.New("tracking: review record item with an empty key")
		}
		if seen[it.Key] {
			return fmt.Errorf("tracking: review record item key %q twice", it.Key)
		}
		seen[it.Key] = true
		if len(it.Comments) == 0 {
			return fmt.Errorf("tracking: review record item %s names no comment", it.Key)
		}
		for _, c := range it.Comments {
			if c.ID == "" {
				return fmt.Errorf("tracking: review record item %s names a comment with no id", it.Key)
			}
			if sources[c.ID] {
				return fmt.Errorf("tracking: review record names the comment %s twice", c.ID)
			}
			sources[c.ID] = true
			if c.Bot != ReviewBotDevin && c.Bot != ReviewBotCodex {
				return fmt.Errorf("tracking: review record comment %s bot %q is not devin or codex", c.ID, c.Bot)
			}
			if strings.TrimSpace(c.Body) == "" {
				return fmt.Errorf("tracking: review record comment %s has an empty body", c.ID)
			}
			if c.Line < 0 {
				return fmt.Errorf("tracking: review record comment %s line %d", c.ID, c.Line)
			}
		}
		switch it.Disposition {
		case "":
			unresolved++
			if strings.TrimSpace(it.Reason) != "" {
				return fmt.Errorf("tracking: review record item %s has a reason but no disposition", it.Key)
			}
		case DispositionApplied, DispositionRejected:
			if strings.TrimSpace(it.Reason) == "" {
				return fmt.Errorf("tracking: review record item %s is %s without a reason", it.Key, it.Disposition)
			}
		default:
			return fmt.Errorf("tracking: review record item %s disposition %q is not applied or rejected", it.Key, it.Disposition)
		}
	}
	// the state says the same thing the items do
	switch r.State {
	case ReviewStateComplete:
		if unresolved != 0 {
			return fmt.Errorf("tracking: review record state complete with %d unresolved item(s)", unresolved)
		}
	case ReviewStatePending:
		if unresolved == 0 || r.Round != 1 {
			return errors.New("tracking: review record state pending is round 1 with unresolved items")
		}
	case ReviewStateRoundLimit:
		if unresolved == 0 || r.Round != 2 {
			return errors.New("tracking: review record state round_limit is round 2 with unresolved items")
		}
	case ReviewStateNotConnected:
		if r.Note != "리뷰 봇 미연결" {
			return errors.New("tracking: a not_connected review record lacks the note")
		}
	}
	return nil
}

// Unresolved counts the items with no disposition yet. The whole record is validated first: a record that does not pass is
// an error, never a count.
func (r PRReviewRecord) Unresolved() (int, error) {
	if err := r.validate(); err != nil {
		return 0, err
	}
	n := 0
	for _, it := range r.Items {
		if it.Disposition == "" {
			n++
		}
	}
	return n, nil
}

// ReadPRReviewHistory reads the crw-pr-review/1 record at path for the pull request ref (without regard to case), whatever
// head it is of: the previous record a `crw flow pr comments` round continues. A missing, malformed, oversized or
// trailing-garbage file, a record that fails validation, or one of another pull request is an error.
func ReadPRReviewHistory(path string, ref Ref) (PRReviewRecord, error) {
	f, err := os.Open(path)
	if err != nil {
		return PRReviewRecord{}, fmt.Errorf("tracking: review record %s: %w", path, err)
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxReviewRecord+1))
	if err != nil {
		return PRReviewRecord{}, fmt.Errorf("tracking: review record %s: %w", path, err)
	}
	if len(data) > maxReviewRecord {
		return PRReviewRecord{}, fmt.Errorf("tracking: review record %s is over %d bytes", path, maxReviewRecord)
	}
	var rec PRReviewRecord
	dec := json.NewDecoder(strings.NewReader(string(data)))
	if err := dec.Decode(&rec); err != nil {
		return PRReviewRecord{}, fmt.Errorf("tracking: review record %s: %w", path, err)
	}
	var extra json.RawMessage
	if err := dec.Decode(&extra); err != io.EOF {
		return PRReviewRecord{}, fmt.Errorf("tracking: review record %s has data after the object", path)
	}
	if !strings.EqualFold(rec.PR, ref.String()) {
		return PRReviewRecord{}, fmt.Errorf("tracking: review record %s is of %s, not %s", path, rec.PR, ref)
	}
	if err := rec.validate(); err != nil {
		return PRReviewRecord{}, fmt.Errorf("tracking: review record %s: %w", path, err)
	}
	return rec, nil
}

// ReadPRReviewRecord reads the record at path and binds it to the pull request ref (without regard to case) and exactly to
// head — never a trusted count of another head.
func ReadPRReviewRecord(path string, ref Ref, head string) (PRReviewRecord, error) {
	rec, err := ReadPRReviewHistory(path, ref)
	if err != nil {
		return PRReviewRecord{}, err
	}
	if rec.Head != head {
		return PRReviewRecord{}, fmt.Errorf("tracking: review record %s is of head %s, not %s", path, rec.Head, head)
	}
	return rec, nil
}

// reviewBot is the bot name a login belongs to: devin or codex, "" for a login that is not a known review bot.
func reviewBot(login string) string {
	switch strings.ToLower(login) {
	case "devin-ai-integration[bot]", "devin[bot]":
		return ReviewBotDevin
	case "chatgpt-codex-connector[bot]", "codex[bot]":
		return ReviewBotCodex
	}
	return ""
}

// apiReviewComment is the part of a GitHub issue comment, inline review comment or review the adapter reads.
type apiReviewComment struct {
	ID           int64  `json:"id"`
	Body         string `json:"body"`
	HTMLURL      string `json:"html_url"`
	Path         string `json:"path"`
	Line         int    `json:"line"`
	OriginalLine int    `json:"original_line"`
	User         struct {
		Login string `json:"login"`
	} `json:"user"`
}

// ReviewComments collects the known review bots' comments of the pull request: every page of the issue comments
// (/issues/N/comments), the inline review comments (/pulls/N/comments) and the reviews (/pulls/N/reviews). Only comments of
// devin-ai-integration[bot], devin[bot], chatgpt-codex-connector[bot] and codex[bot] are kept, and only ones with a nonempty
// body; an error of any of the three reads is an error of the collection, never a partial list. No diff hunk or patch is
// ever read.
func (g *GitHub) ReviewComments(ctx context.Context, ref Ref) ([]PRReviewComment, error) {
	if err := g.checkRef(ref); err != nil {
		return nil, err
	}
	n := strconv.Itoa(ref.Number)
	query := url.Values{"per_page": {pageSize}}
	var out []PRReviewComment
	keep := func(kind string, c apiReviewComment) {
		bot := reviewBot(c.User.Login)
		if bot == "" || strings.TrimSpace(c.Body) == "" {
			return
		}
		cm := PRReviewComment{ID: kind + ":" + strconv.FormatInt(c.ID, 10), Bot: bot, Body: c.Body, URL: c.HTMLURL}
		if kind == "inline" {
			cm.Path = c.Path
			if cm.Line = c.Line; cm.Line == 0 {
				cm.Line = c.OriginalLine
			}
		}
		out = append(out, cm)
	}
	var issue []apiReviewComment
	if err := getAll(ctx, g, "tracking: list issue comments of "+ref.String(), g.repoURL("/issues/"+n+"/comments", query), &issue); err != nil {
		return nil, err
	}
	for _, c := range issue {
		keep("issue", c)
	}
	var inline []apiReviewComment
	if err := getAll(ctx, g, "tracking: list review comments of "+ref.String(), g.repoURL("/pulls/"+n+"/comments", query), &inline); err != nil {
		return nil, err
	}
	for _, c := range inline {
		keep("inline", c)
	}
	var reviews []apiReviewComment
	if err := getAll(ctx, g, "tracking: list reviews of "+ref.String(), g.repoURL("/pulls/"+n+"/reviews", query), &reviews); err != nil {
		return nil, err
	}
	for _, c := range reviews {
		keep("review", c)
	}
	return out, nil
}

// FindPullRequest looks up the pull request of head onto base in the repository: the paginated list call
// /pulls?state=all&head=<owner>:<head>&base=<base>, filtered again on the decoded rows (exact head and base refs and a head
// repository of this repository — the API's head filter matches a user, not the repository). More than one matching open
// pull request is an error; one open answers it, else a unique closed or merged one answers that (its existence is a refusal
// to the caller, never a duplicate to create). The list read is never a patch endpoint.
func (g *GitHub) FindPullRequest(ctx context.Context, head, base string) (PullRequest, bool, error) {
	if err := checkBranch("head", head); err != nil {
		return PullRequest{}, false, fmt.Errorf("tracking: head %q: %w", head, err)
	}
	if err := checkBranch("base", base); err != nil {
		return PullRequest{}, false, fmt.Errorf("tracking: base %q: %w", base, err)
	}
	owner, _, _ := strings.Cut(g.repo, "/")
	query := url.Values{"state": {"all"}, "head": {owner + ":" + head}, "base": {base}, "per_page": {pageSize}}
	var pulls []apiPull
	if err := getAll(ctx, g, "tracking: list pull requests of "+g.repo, g.repoURL("/pulls", query), &pulls); err != nil {
		return PullRequest{}, false, err
	}
	var open, other []PullRequest
	for _, a := range pulls {
		p := g.pullFrom(a)
		if p.HeadRef != head || p.BaseRef != base || !strings.EqualFold(p.HeadRepo, g.repo) {
			continue
		}
		if p.State == StateOpen {
			open = append(open, p)
		} else {
			other = append(other, p)
		}
	}
	if len(open) > 1 {
		return PullRequest{}, false, fmt.Errorf("tracking: %d open pull requests of %s onto %s in %s", len(open), head, base, g.repo)
	}
	if len(open) == 1 {
		return open[0], true, nil
	}
	if len(other) > 1 {
		return PullRequest{}, false, fmt.Errorf("tracking: %d historical pull requests of %s onto %s in %s", len(other), head, base, g.repo)
	}
	if len(other) == 1 {
		return other[0], true, nil
	}
	return PullRequest{}, false, nil
}
