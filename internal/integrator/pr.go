package integrator

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/tracking"
)

// PR-unit landing (CRW-1355). A project session lands its milestone's nodes on the milestone integration branch, opens one pull
// request from it toward dev (crw flow pr open) and enqueues it (EnqueuePR). A pass lands it alone: it reads the pull request,
// merges its head onto dev in its own worktree, verifies the merged tree, runs the configured review command, and moves dev
// fast-forward only when the verification passed and the review did not ask for changes. The pull request is the place of the
// record and of the review; the gate is the verification-record (CRW-1306): with no review command the queue warns, records
// "review not configured" and lands, and a review that gives no verdict is recorded the same way. Only needs_changes refuses.

// PullRequestSource is what the queue reads of a pull request and where it writes the record of its landing: the tracking
// adapter (tracking.GitHub) in production.
type PullRequestSource interface {
	PullRequest(ctx context.Context, ref tracking.Ref) (tracking.PullRequest, error)
	Comment(ctx context.Context, ref tracking.Ref, header, text string) error
}

var _ PullRequestSource = (*tracking.GitHub)(nil)

// LandingSchema names the record of a pull request's landing, <tag>.pr.json.
const LandingSchema = "crw-pr-landing/1"

// The verdicts a review command writes.
const (
	verdictAccept       = "accept"
	verdictNeedsChanges = "needs_changes"
)

// maxVerdict bounds the verdict file the queue reads.
const maxVerdict = 16 << 20

// Landing is the record of one integration run of a pull request, against the pull request: its number and head, dev before and
// after, the verification-record and the review verdict.
type Landing struct {
	Schema      string   `json:"schema"`
	Tag         string   `json:"tag"`
	PullRequest string   `json:"pull_request"`
	Number      int      `json:"number"`
	URL         string   `json:"url"`
	Head        string   `json:"head"`
	HeadRef     string   `json:"head_ref"`
	Issues      []string `json:"issues"`
	// DevBefore is the branch's commit the head was merged onto; DevAfter the commit it was moved to, empty when it did not move.
	DevBefore string `json:"dev_before"`
	DevAfter  string `json:"dev_after"`
	// Merged is the merged tree's commit that was verified (and reviewed).
	Merged       string              `json:"merged"`
	Result       string              `json:"result"`
	Verification LandingVerification `json:"verification"`
	Review       LandingReview       `json:"review"`
	Warnings     []string            `json:"warnings,omitempty"`
	// Comment is "written" once the record is on the pull request as a comment, else why it is not.
	Comment string `json:"comment"`
}

// LandingVerification is the verification of the merged tree: the record the command wrote, its digest, and the command's
// status (-1 when it did not run).
type LandingVerification struct {
	Record string `json:"record"`
	SHA256 string `json:"sha256,omitempty"`
	Status int    `json:"status"`
}

// LandingReview is what the review of a pull request's merged tree gave.
type LandingReview struct {
	Configured bool `json:"configured"`
	// Verdict is accept or needs_changes, empty when the review gave none.
	Verdict     string `json:"verdict,omitempty"`
	VerdictFile string `json:"verdict_file,omitempty"`
	// Status is the review command's status (-1 when it could not start or did not run).
	Status int    `json:"status"`
	Log    string `json:"log,omitempty"`
	// Note says why there is no verdict: "review not configured", or why the review gave none.
	Note string `json:"note,omitempty"`
}

// prRef is the pull request a pr-<N> entry names.
func (r *Runner) prRef(e Entry) tracking.Ref {
	return tracking.Ref{Repo: r.Cfg.PullRequestRepo, Number: e.PR}
}

// EnqueuePR puts pull request ref ("owner/name#N" of the configured repository) into the queue: it reads the pull request and
// writes its head to pending/pr-<N>, replacing an earlier head of the same pull request. A pull request that is not open, not
// toward the branch, or names no head commit is refused and nothing is written.
func (r *Runner) EnqueuePR(ctx context.Context, ref string) (Entry, error) {
	c := r.Cfg
	if c.PullRequestRepo == "" || r.PullRequests == nil {
		return Entry{}, errors.New("the queue lands no pull request: the integrate section names no pull_request_repo")
	}
	pr, err := tracking.ParseRef(ref)
	if err != nil {
		return Entry{}, err
	}
	if !strings.EqualFold(pr.Repo, c.PullRequestRepo) {
		return Entry{}, fmt.Errorf("%s is not a pull request of %s, the queue's pull_request_repo", ref, c.PullRequestRepo)
	}
	if pr.Number > maxPRNumber {
		return Entry{}, fmt.Errorf("%s: the number is out of range", ref)
	}
	pr.Repo = c.PullRequestRepo
	got, err := r.PullRequests.PullRequest(ctx, pr)
	if err != nil {
		return Entry{}, err
	}
	switch {
	case got.Merged || got.State != tracking.StateOpen:
		return Entry{}, fmt.Errorf("%s is closed: the queue lands an open pull request", pr)
	case got.BaseRef != c.Branch:
		return Entry{}, fmt.Errorf("%s goes to %s, not %s", pr, got.BaseRef, c.Branch)
	case !strings.EqualFold(got.HeadRepo, c.PullRequestRepo):
		return Entry{}, fmt.Errorf("%s comes from %s, not %s: the queue lands a pull request whose branch is in the repository", pr, headRepoName(got.HeadRepo), c.PullRequestRepo)
	case !headRE.MatchString(got.HeadSHA):
		return Entry{}, fmt.Errorf("%s names no head commit (%q)", pr, got.HeadSHA)
	}
	if err := r.ensureQueueDirs(); err != nil {
		return Entry{}, err
	}
	name := prPrefix + strconv.Itoa(pr.Number)
	// the head is written beside the queue and renamed in, so a pass never reads half a file
	tmp, err := os.CreateTemp(c.QueueDir, ".enqueue-"+name+"-")
	if err != nil {
		return Entry{}, err
	}
	_, werr := io.WriteString(tmp, got.HeadSHA+"\n")
	cerr := tmp.Close()
	if werr == nil {
		werr = cerr
	}
	if werr == nil {
		werr = os.Rename(tmp.Name(), r.qpath(dirPending, name))
	}
	if werr != nil {
		_ = os.Remove(tmp.Name())
		return Entry{}, werr
	}
	r.logf("%s %s enqueued (%s -> %s)", r.keyOf(name), got.HeadSHA, got.HeadRef, got.BaseRef)
	return Entry{Name: name, Key: r.keyOf(name), Node: name, Head: got.HeadSHA, HeadOK: true, PR: pr.Number, num: uint64(pr.Number)}, nil
}

// headRepoName names the repository of a pull request's head branch; GitHub names none for a fork that was deleted.
func headRepoName(repo string) string {
	if repo == "" {
		return "a repository GitHub no longer names"
	}
	return repo
}

// checkPR reads the pull request of a pending entry before anything is merged. It answers the pull request to land, or nil when
// the entry was moved (a closed, merged, retargeted or moved-on pull request, or a head that is not a commit), or why the entry is
// held in pending this pass (the pull request could not be read: no verdict, so the entry waits and the issue heads go on).
func (r *Runner) checkPR(ctx context.Context, e Entry, rep *Report) (*tracking.PullRequest, string) {
	if !e.HeadOK {
		r.logf("%s %q is not a full head SHA: blocked", e.Key, e.Head)
		rep.moved(r, e, dirBlocked, "head is not a 40-hex commit")
		return nil, ""
	}
	if r.PullRequests == nil {
		return nil, "the pending pull request cannot be read: the queue has no tracking adapter"
	}
	pr, err := r.PullRequests.PullRequest(ctx, r.prRef(e))
	if ctx.Err() != nil {
		return nil, ""
	}
	if err != nil {
		held := fmt.Sprintf("the pull request could not be read, it stays pending: %v", err)
		r.logf("%s %s: %s", e.Key, e.Head, held)
		return nil, held
	}
	reason, dir := "", ""
	switch {
	case pr.Merged:
		reason, dir = "the pull request is merged", dirDone
	case pr.State != tracking.StateOpen:
		reason, dir = "the pull request is closed", dirConflict
	case pr.BaseRef != r.Cfg.Branch:
		reason, dir = fmt.Sprintf("the pull request goes to %s, not %s", pr.BaseRef, r.Cfg.Branch), dirBlocked
	case !strings.EqualFold(pr.HeadRepo, r.Cfg.PullRequestRepo):
		reason, dir = fmt.Sprintf("the pull request comes from %s, not %s", headRepoName(pr.HeadRepo), r.Cfg.PullRequestRepo), dirBlocked
	case pr.HeadSHA != e.Head:
		reason, dir = fmt.Sprintf("the pull request's head is now %s; enqueue it again", pr.HeadSHA), dirConflict
	}
	if reason != "" {
		r.logf("%s %s: %s", e.Key, e.Head, reason)
		rep.moved(r, e, dir, reason)
		return nil, ""
	}
	return &pr, ""
}

// review runs the review command on the verified merged tree and reads its verdict. Only a verdict file that says needs_changes
// refuses; a review that is not configured, cannot start, or writes no readable verdict is recorded and warned, and the
// verification-record stays the gate.
func (r *Runner) review(ctx context.Context, tag, wt, merged, base, record string, pr tracking.PullRequest, head string, log io.Writer) LandingReview {
	c := r.Cfg
	if len(c.ReviewCommand) == 0 {
		fmt.Fprintf(log, "%s: review not configured; dev moves on the verification-record alone\n", pr.Ref)
		return LandingReview{Status: -1, Note: "review not configured"}
	}
	res := LandingReview{Configured: true, Status: -1, VerdictFile: filepath.Join(c.RunsDir, tag+".review.json"), Log: filepath.Join(c.RunsDir, tag+".review.log")}
	rlog, err := os.OpenFile(res.Log, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o664)
	if err != nil {
		res.Note = "review gave no verdict: " + err.Error()
		return res
	}
	defer rlog.Close()
	pairs := []string{"{pr}", pr.Ref.String(), "{pr_number}", strconv.Itoa(pr.Ref.Number), "{head}", head, "{merged}", merged, "{base}", base,
		"{verdict}", res.VerdictFile, "{worktree}", wt, "{record}", record}
	env := append(r.gitEnv(), "CRW_REVIEW_PR="+pr.Ref.String(), "CRW_REVIEW_HEAD="+head, "CRW_REVIEW_MERGED="+merged, "CRW_REVIEW_BASE="+base,
		"CRW_REVIEW_VERDICT="+res.VerdictFile, "CRW_VERIFY_RECORD="+record)
	rc, err := r.Exec(ctx, Cmd{Argv: expandAll(c.ReviewCommand, pairs...), Dir: wt, Env: env, Stdout: rlog, Stderr: rlog})
	res.Status = rc
	if err != nil {
		res.Status = -1
		fmt.Fprintf(rlog, "review command: %v\n", err)
	}
	verdict, verr := readVerdict(res.VerdictFile)
	switch {
	case verr == nil:
		res.Verdict = verdict
	case err != nil:
		res.Note = "review gave no verdict: the review command could not run: " + err.Error()
	default:
		res.Note = fmt.Sprintf("review gave no verdict (status %d): %v", rc, verr)
	}
	return res
}

// readVerdict reads the verdict of a review's verdict file: "verdict" at the top or under "structured_output".
func readVerdict(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxVerdict+1))
	if err != nil {
		return "", err
	}
	if len(data) > maxVerdict {
		return "", fmt.Errorf("%s is over %d bytes", path, maxVerdict)
	}
	var v struct {
		Verdict    string `json:"verdict"`
		Structured *struct {
			Verdict string `json:"verdict"`
		} `json:"structured_output"`
	}
	if err := json.Unmarshal(data, &v); err != nil {
		return "", fmt.Errorf("%s: %w", path, err)
	}
	verdict := v.Verdict
	if verdict == "" && v.Structured != nil {
		verdict = v.Structured.Verdict
	}
	if verdict != verdictAccept && verdict != verdictNeedsChanges {
		return "", fmt.Errorf("%s says verdict %q, not accept or needs_changes", path, verdict)
	}
	return verdict, nil
}

// recordLanding writes <tag>.pr.json and puts the same record on the pull request as a comment.
func (r *Runner) recordLanding(ctx context.Context, pr tracking.PullRequest, out Outcome) *Landing {
	sum := out.Summary
	l := &Landing{Schema: LandingSchema, Tag: sum.Tag, PullRequest: pr.Ref.String(), Number: pr.Ref.Number, URL: pr.URL, Head: pr.HeadSHA,
		HeadRef: pr.HeadRef, Issues: []string{}, DevBefore: sum.Base, Merged: sum.Head, Result: sum.Result,
		Verification: LandingVerification{Record: sum.Record, Status: out.verifyRC}}
	for _, is := range pr.Issues {
		l.Issues = append(l.Issues, is.String())
	}
	if out.Exit == ExitPushed && len(out.merged) > 0 {
		l.DevAfter = sum.Head
	}
	if data, err := os.ReadFile(sum.Record); err == nil {
		d := sha256.Sum256(data)
		l.Verification.SHA256 = hex.EncodeToString(d[:])
	}
	switch {
	case out.review != nil:
		l.Review = *out.review
	case out.verifyRC > 0 || out.verifyRC == 127:
		l.Review = LandingReview{Configured: len(r.Cfg.ReviewCommand) > 0, Status: -1, Note: "not reviewed: the verification failed"}
	default:
		l.Review = LandingReview{Configured: len(r.Cfg.ReviewCommand) > 0, Status: -1, Note: "not reviewed: nothing was verified"}
	}
	if l.Review.Verdict == "" && l.Review.Note != "" && !strings.HasPrefix(l.Review.Note, "not reviewed") {
		l.Warnings = append(l.Warnings, l.Review.Note+": dev moves on the verification-record alone (CRW-1306)")
		r.logf("%s %s: %s", pr.Ref, pr.HeadSHA, l.Review.Note)
	}
	l.Comment = "not written yet"
	r.writeLanding(l)
	if err := r.PullRequests.Comment(context.WithoutCancel(ctx), pr.Ref, "[dev queue · "+sum.Tag+"]", landingText(l)); err != nil {
		l.Comment = "not written: " + err.Error()
		r.logf("%s: the landing record could not be written on the pull request: %v", pr.Ref, err)
	} else {
		l.Comment = "written"
	}
	r.writeLanding(l)
	return l
}

func (r *Runner) writeLanding(l *Landing) {
	data, err := json.MarshalIndent(l, "", "  ")
	if err != nil {
		return
	}
	path := filepath.Join(r.Cfg.RunsDir, l.Tag+".pr.json")
	if err := os.WriteFile(path+".new", append(data, '\n'), 0o664); err == nil {
		_ = os.Rename(path+".new", path)
	}
}

// landingText is the comment body of a landing record.
func landingText(l *Landing) string {
	var b bytes.Buffer
	fmt.Fprintf(&b, "result: %s\n", l.Result)
	fmt.Fprintf(&b, "pull request: %s\n", l.PullRequest)
	fmt.Fprintf(&b, "head: %s (%s)\n", l.Head, l.HeadRef)
	fmt.Fprintf(&b, "issues: %s\n", strings.Join(l.Issues, ", "))
	if l.DevAfter != "" {
		fmt.Fprintf(&b, "dev: %s -> %s\n", l.DevBefore, l.DevAfter)
	} else {
		fmt.Fprintf(&b, "dev: %s (not moved)\n", l.DevBefore)
	}
	fmt.Fprintf(&b, "merged tree: %s\n", l.Merged)
	fmt.Fprintf(&b, "verification-record: %s sha256:%s (status %d)\n", l.Verification.Record, l.Verification.SHA256, l.Verification.Status)
	switch {
	case l.Review.Verdict != "":
		fmt.Fprintf(&b, "review: %s (%s)\n", l.Review.Verdict, l.Review.VerdictFile)
	case !l.Review.Configured && l.Review.Note == "review not configured":
		fmt.Fprintf(&b, "review: not configured\n")
	default:
		fmt.Fprintf(&b, "review: %s\n", l.Review.Note)
	}
	for _, w := range l.Warnings {
		fmt.Fprintf(&b, "warning: %s\n", w)
	}
	return strings.TrimRight(b.String(), "\n")
}
