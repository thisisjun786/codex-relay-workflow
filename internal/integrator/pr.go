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
	// HeadMoved is the head GitHub named right before dev was to move, when it was not Head: dev did not move for Head, and the
	// entry stays pending with HeadMoved for the next pass to verify and review.
	HeadMoved string `json:"head_moved,omitempty"`
	// Comment is "written" once the record is on the pull request as a comment, else why it is not.
	Comment string `json:"comment"`
}

// LandingVerification is the verification of the merged tree: the record the command wrote, its digest, and the command's
// status (-1 when it did not run).
type LandingVerification struct {
	Record string `json:"record"`
	SHA256 string `json:"sha256,omitempty"`
	Status int    `json:"status"`
	// Note says why the verification is not in the record (a landing recovered without the files of its run).
	Note string `json:"note,omitempty"`
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
// toward the branch, or names no head commit is refused and nothing is written, and so is a head of it that already landed (a
// done/ entry or a landing record for that number and head): after new commits on its branch the pull request is enqueued
// again with the new head.
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
	e := Entry{Name: name, Key: r.keyOf(name), Node: name, Head: got.HeadSHA, HeadOK: true, PR: pr.Number, num: uint64(pr.Number)}
	if why := r.landedBefore(e); why != "" {
		return Entry{}, fmt.Errorf("%s at %s %s; enqueue it again after new commits on %s", pr, got.HeadSHA, why, got.HeadRef)
	}
	if err := r.writePending(name, got.HeadSHA); err != nil {
		return Entry{}, err
	}
	r.logf("%s %s enqueued (%s -> %s)", r.keyOf(name), got.HeadSHA, got.HeadRef, got.BaseRef)
	return e, nil
}

// resultLandedBeforeRecord is the result of a landing record written after the fact: the head was found in the branch with no
// record of its landing (a pass that ended after its push, or a pull request GitHub shows merged).
const resultLandedBeforeRecord = "landed_before_record"

// noteReviewNotConfigured is the note of a landing whose queue has no review command, in a pass's record and in a recovered one.
const noteReviewNotConfigured = "review not configured"

// landedBefore says why a pull request entry already landed, "" when it did not: done/ holds the entry for its number and head,
// or runs_dir holds a landing record of that number and head that moved the branch. A landing is recorded once per (number,
// head).
func (r *Runner) landedBefore(e Entry) string {
	if e.PR == 0 || !e.HeadOK {
		return ""
	}
	if fileExists(r.qpath(dirDone, doneName(e))) {
		return "already landed (done/" + doneName(e) + ")"
	}
	if path := r.findLanding(e.PR, e.Head); path != "" {
		return "already landed (" + path + ")"
	}
	return ""
}

// findLanding is the landing record in runs_dir of pull request number at head that moved the branch, "" when there is none.
func (r *Runner) findLanding(number int, head string) string {
	paths, _ := filepath.Glob(filepath.Join(r.Cfg.RunsDir, "*.pr.json"))
	for _, p := range paths {
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var l Landing
		if json.Unmarshal(data, &l) == nil && l.Number == number && l.Head == head && l.DevAfter != "" {
			return p
		}
	}
	return ""
}

// headInBranch fetches the remote and says whether head is in the remote branch (a commit the checkout does not have is not).
func (r *Runner) headInBranch(ctx context.Context, head string) (bool, error) {
	c := r.Cfg
	if rc, err := r.git(ctx, c.Checkout, nil, "fetch", c.Remote, "--quiet"); err != nil || rc != 0 {
		return false, fmt.Errorf("fetch failed (status %d, %v)", rc, err)
	}
	rc, err := r.git(ctx, c.Checkout, nil, "merge-base", "--is-ancestor", head, c.remoteRef())
	if err != nil {
		return false, err
	}
	return rc == 0, nil
}

// firstContaining is the commit of the branch's first-parent line that brought head into it: the merge of head, or head itself
// when the branch moved to it.
func (r *Runner) firstContaining(ctx context.Context, head string) string {
	c := r.Cfg
	out, err := r.gitOut(ctx, c.Checkout, "rev-list", "--first-parent", "--ancestry-path", head+".."+c.remoteRef())
	if err != nil {
		return ""
	}
	lines := strings.Fields(out)
	if len(lines) == 0 {
		return head
	}
	oldest := lines[len(lines)-1]
	// a first parent that holds head is head itself (it is not on the ancestry path from head)
	if rc, err := r.git(ctx, c.Checkout, nil, "merge-base", "--is-ancestor", head, oldest+"^1"); err == nil && rc == 0 {
		return head
	}
	return oldest
}

// pushedRun is the summary of this queue's run that pushed the entry's head (its tag and summary), nil when no run file says so.
// A run whose summary is still the pushing marker (the signal ended it during the push) counts when its head is in the branch. A
// git that cannot answer whether a marker's head is in the branch is an error, never "no pushed run": the caller holds the entry
// and the marker stays for the next pass.
func (r *Runner) pushedRun(ctx context.Context, e Entry) (*Summary, error) {
	paths, _ := filepath.Glob(filepath.Join(r.Cfg.RunsDir, "*.summary.json"))
	var found *Summary
	for _, p := range paths {
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var s Summary
		if json.Unmarshal(data, &s) == nil && s.Merged == e.Spec() && headRE.MatchString(s.Head) && (found == nil || s.Tag > found.Tag) {
			switch s.Result {
			case "pushed":
				found = &s
			case resultPushing:
				in, err := r.markerInBranch(ctx, s.Head)
				if err != nil {
					return nil, fmt.Errorf("whether the pushing marker of run %s (head %s) is in %s could not be read: %w", s.Tag, s.Head, r.Cfg.Branch, err)
				}
				if in {
					found = &s
				}
			}
		}
	}
	return found, nil
}

// markerInBranch says whether head, the head a pushing marker names, is in the remote branch. A commit the checkout does not have
// was never pushed (false); a git that fails otherwise (it cannot start, the context ended, or it exits with a status other than 0
// and 1 for a commit it has) is an error.
func (r *Runner) markerInBranch(ctx context.Context, head string) (bool, error) {
	c := r.Cfg
	rc, err := r.git(ctx, c.Checkout, nil, "merge-base", "--is-ancestor", head, c.remoteRef())
	if err != nil {
		return false, err
	}
	switch rc {
	case 0:
		return true, nil
	case 1:
		return false, nil
	}
	have, err := r.git(ctx, c.Checkout, nil, "cat-file", "-e", head+"^{commit}")
	if err != nil {
		return false, err
	}
	if have != 0 {
		return false, nil
	}
	return false, fmt.Errorf("git merge-base --is-ancestor exited %d", rc)
}

// recoverLanding writes the one landing record of a pull request entry whose head is in the branch with no record (result
// landed_before_record): dev_after is the branch commit that brought the head in, and the verification and the review come from
// the files of the run that pushed it, or are stated as not recorded. tag is the run's tag ("" reserves one). The record is
// written before its comment, so a pass that ends between the two posts no second comment.
func (r *Runner) recoverLanding(ctx context.Context, tag string, e Entry, pr tracking.PullRequest) (*Landing, error) {
	run, err := r.pushedRun(ctx, e)
	if err != nil {
		return nil, err
	}
	if tag == "" {
		if tag, err = r.reserveTag(); err != nil {
			return nil, err
		}
	}
	l := &Landing{Schema: LandingSchema, Tag: tag, PullRequest: pr.Ref.String(), Number: pr.Ref.Number, URL: pr.URL, Head: e.Head,
		HeadRef: pr.HeadRef, Issues: []string{}, Result: resultLandedBeforeRecord}
	for _, is := range pr.Issues {
		l.Issues = append(l.Issues, is.String())
	}
	configured := len(r.Cfg.ReviewCommand) > 0
	if run != nil {
		l.DevBefore, l.DevAfter, l.Merged = run.Base, run.Head, run.Head
		l.Verification = LandingVerification{Record: run.Record, Status: 0}
		if data, err := os.ReadFile(run.Record); err == nil {
			d := sha256.Sum256(data)
			l.Verification.SHA256 = hex.EncodeToString(d[:])
		} else {
			l.Verification.Note = "the verification-record of run " + run.Tag + " is not readable: " + err.Error()
		}
		verdictFile := filepath.Join(r.Cfg.RunsDir, run.Tag+".review.json")
		if v, err := readVerdict(verdictFile); err == nil {
			l.Review = LandingReview{Configured: true, Verdict: v, VerdictFile: verdictFile, Status: -1, Log: filepath.Join(r.Cfg.RunsDir, run.Tag+".review.log"),
				Note: "the review command's status is not recorded"}
		} else if !configured {
			l.Review = LandingReview{Status: -1, Note: noteReviewNotConfigured}
		} else {
			l.Review = LandingReview{Configured: true, Status: -1, Note: "not recorded: run " + run.Tag + " left no readable verdict"}
		}
	} else {
		after := r.firstContaining(ctx, e.Head)
		l.DevAfter, l.Merged = after, after
		if before, err := r.gitOut(ctx, r.Cfg.Checkout, "rev-parse", "--verify", "-q", after+"^1"); err == nil {
			l.DevBefore = before
		}
		const none = "not recorded: no run file of this queue names the head"
		l.Verification = LandingVerification{Status: -1, Note: none}
		l.Review = LandingReview{Configured: configured, Status: -1, Note: none}
		if !configured {
			l.Review = LandingReview{Status: -1, Note: noteReviewNotConfigured}
		}
	}
	l.Warnings = append(l.Warnings, "the head was in "+r.Cfg.Branch+" before its landing was recorded; this record is written after the fact")
	r.logf("%s %s: in %s with no landing record: recovered record %s", pr.Ref, e.Head, r.Cfg.Branch, tag)
	if err := r.publishLanding(ctx, l); err != nil {
		return nil, err
	}
	return l, nil
}

// recheckPR reads the pull request again right before the branch moves. It answers "" when the pull request is still the one
// the run merged (open, toward the branch, from the repository, at head), else the run's result: head-moved:<sha> with the new
// head, or pull-request-changed with why. Neither moves the branch; the entry stays pending.
func (r *Runner) recheckPR(ctx context.Context, pr tracking.PullRequest, head string) (result, moved, why string) {
	now, err := r.PullRequests.PullRequest(ctx, pr.Ref)
	switch {
	case ctx.Err() != nil:
		return "", "", ""
	case err != nil:
		return "pull-request-changed", "", "the pull request could not be read again before " + r.Cfg.Branch + " moves: " + err.Error()
	case now.Merged || now.State != tracking.StateOpen:
		return "pull-request-changed", "", "the pull request was closed or merged during the run"
	case now.BaseRef != r.Cfg.Branch:
		return "pull-request-changed", "", "the pull request goes to " + now.BaseRef + " now"
	case !strings.EqualFold(now.HeadRepo, r.Cfg.PullRequestRepo):
		return "pull-request-changed", "", "the pull request comes from " + headRepoName(now.HeadRepo) + " now"
	case now.HeadSHA != head && headRE.MatchString(now.HeadSHA):
		return "head-moved:" + now.HeadSHA, now.HeadSHA, "the pull request's head moved to " + now.HeadSHA + " during the run"
	case now.HeadSHA != head:
		return "pull-request-changed", "", fmt.Sprintf("the pull request names no head commit now (%q)", now.HeadSHA)
	}
	return "", "", ""
}

// headRepoName names the repository of a pull request's head branch; GitHub names none for a fork that was deleted.
func headRepoName(repo string) string {
	if repo == "" {
		return "a repository GitHub no longer names"
	}
	return repo
}

// checkPR reads the pull request of a pending entry before anything is merged. It answers the pull request to land, or nil when
// the entry was moved (a closed, merged, retargeted or moved-on pull request, a head that is not a commit, or a head already in
// the branch, whose landing it records once), or why the entry is held in pending this pass (the pull request or the branch
// could not be read: no verdict, so the entry waits and the issue heads go on).
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
	// a head already in the branch with no landing record (landedBefore found none): a pass ended after its push, or GitHub
	// shows the pull request merged; the landing is recorded once, after the fact, and the entry is done
	in, err := r.headInBranch(ctx, e.Head)
	if ctx.Err() != nil {
		return nil, ""
	}
	if err != nil {
		held := fmt.Sprintf("whether %s holds the head could not be read, it stays pending: %v", r.Cfg.Branch, err)
		r.logf("%s %s: %s", e.Key, e.Head, held)
		return nil, held
	}
	if in {
		l, err := r.recoverLanding(ctx, "", e, pr)
		if err != nil {
			return nil, "the landing could not be recorded, it stays pending: " + err.Error()
		}
		rep.Landing = l
		rep.moved(r, e, dirDone, "landed before its record: "+filepath.Join(r.Cfg.RunsDir, l.Tag+".pr.json"))
		return nil, ""
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
		return LandingReview{Status: -1, Note: noteReviewNotConfigured}
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
	l.HeadMoved = out.headMoved
	if out.prNote != "" {
		l.Warnings = append(l.Warnings, out.prNote+": "+r.Cfg.Branch+" did not move; the entry stays pending")
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
	if err := r.publishLanding(ctx, l); err != nil {
		// dev already moved: the entry goes to done/ (its landing stands) and the comment still goes up when no record holds its state
		r.logf("%s %s: %v", pr.Ref, pr.HeadSHA, err)
		if l.Comment == commentNotWrittenYet {
			r.postLandingComment(ctx, l)
		}
	}
	return l
}

// The states of a landing record's comment field: the record is written first, so a pass that ends before the comment call leaves
// commentNotWrittenYet, which the next pass that finds the head landed posts (repostLandingComment). A refusal is commentRefused
// plus the reason and is final: the landing stands and the file says why the comment is missing.
const (
	commentNotWrittenYet = "not written yet"
	commentWritten       = "written"
	commentRefused       = "not written: "
)

// publishLanding writes <tag>.pr.json, then puts the same record on the pull request as a comment and writes the file again
// with what the comment became. A record that cannot be written is an error: when the first write fails no comment is posted
// (nothing would remember it), and l.Comment stays commentNotWrittenYet; when the second fails the comment is posted and the file
// still says "not written yet".
func (r *Runner) publishLanding(ctx context.Context, l *Landing) error {
	l.Comment = commentNotWrittenYet
	if err := r.writeLanding(l); err != nil {
		return err
	}
	r.postLandingComment(ctx, l)
	return r.writeLanding(l)
}

// postLandingComment puts the landing record on the pull request as a comment headed [dev queue · <tag>] and sets l.Comment to what
// it became. A signal that ends the pass does not cancel the call (the push already happened).
func (r *Runner) postLandingComment(ctx context.Context, l *Landing) {
	ref := tracking.Ref{Repo: r.Cfg.PullRequestRepo, Number: l.Number}
	if err := r.PullRequests.Comment(context.WithoutCancel(ctx), ref, "[dev queue · "+l.Tag+"]", landingText(l)); err != nil {
		l.Comment = commentRefused + err.Error()
		r.logf("%s: the landing record could not be written on the pull request: %v", ref, err)
	} else {
		l.Comment = commentWritten
	}
}

// repostLandingComment posts the comment of the landing record of e that an earlier pass left "not written yet" (it ended between
// the record and the comment call) and writes the record again. A record whose comment is written or was refused, and an entry with
// no record (its done/ file alone), are left as they are. It answers why the entry is held, "" when it is not: a queue with no
// tracking adapter cannot post the comment, so the entry waits in pending with the record as it is; a record that cannot be written
// after the comment went up holds the entry too, since done/ must not stand over a record that still says "not written yet". posted is
// true when the comment went up and the record says so: that is work the pass did. It is
// called only by a pass, never by --enqueue-pr or --dry-run. One window stays: a pass that ends between the comment call and the
// second write, or whose second write fails, leaves "not written yet" for a comment that exists, and the next pass posts it a second
// time.
func (r *Runner) repostLandingComment(ctx context.Context, e Entry) (held string, posted bool) {
	path := r.findLanding(e.PR, e.Head)
	if path == "" {
		return "", false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	var l Landing
	if json.Unmarshal(data, &l) != nil || l.Comment != commentNotWrittenYet {
		return "", false
	}
	if r.PullRequests == nil {
		return "the landing comment of " + l.Tag + " cannot be posted, it stays pending: the queue has no tracking adapter", false
	}
	r.postLandingComment(ctx, &l)
	if err := r.writeLanding(&l); err != nil {
		held = "the landing comment of " + l.Tag + " was posted (" + l.Comment + ") but its record could not be written, it stays pending: " + err.Error()
		r.logf("%s %s: %s", e.Key, e.Head, held)
		return held, false
	}
	r.logf("%s %s: the landing comment of %s was posted (%s)", e.Key, e.Head, l.Tag, l.Comment)
	return "", true
}

// writeLanding writes <tag>.pr.json through <tag>.pr.json.new and answers why it could not.
func (r *Runner) writeLanding(l *Landing) error {
	data, err := json.MarshalIndent(l, "", "  ")
	if err != nil {
		return fmt.Errorf("landing record %s: %w", l.Tag, err)
	}
	path := filepath.Join(r.Cfg.RunsDir, l.Tag+".pr.json")
	if err := os.WriteFile(path+".new", append(data, '\n'), 0o664); err != nil {
		return fmt.Errorf("landing record %s could not be written: %w", path, err)
	}
	if err := os.Rename(path+".new", path); err != nil {
		_ = os.Remove(path + ".new")
		return fmt.Errorf("landing record %s could not be written: %w", path, err)
	}
	return nil
}

// landingText is the comment body of a landing record.
func landingText(l *Landing) string {
	var b bytes.Buffer
	fmt.Fprintf(&b, "result: %s\n", l.Result)
	fmt.Fprintf(&b, "pull request: %s\n", l.PullRequest)
	fmt.Fprintf(&b, "head: %s (%s)\n", l.Head, l.HeadRef)
	if l.HeadMoved != "" {
		fmt.Fprintf(&b, "head moved: %s (dev did not move for %s; the next pass verifies and reviews the new head)\n", l.HeadMoved, shortHead(l.Head))
	}
	fmt.Fprintf(&b, "issues: %s\n", strings.Join(l.Issues, ", "))
	if l.DevAfter != "" {
		fmt.Fprintf(&b, "dev: %s -> %s\n", l.DevBefore, l.DevAfter)
	} else {
		fmt.Fprintf(&b, "dev: %s (not moved)\n", l.DevBefore)
	}
	fmt.Fprintf(&b, "merged tree: %s\n", l.Merged)
	if l.Verification.Record == "" && l.Verification.Note != "" {
		fmt.Fprintf(&b, "verification-record: %s\n", l.Verification.Note)
	} else {
		fmt.Fprintf(&b, "verification-record: %s sha256:%s (status %d)\n", l.Verification.Record, l.Verification.SHA256, l.Verification.Status)
	}
	switch {
	case l.Review.Verdict != "":
		fmt.Fprintf(&b, "review: %s (%s)\n", l.Review.Verdict, l.Review.VerdictFile)
	case !l.Review.Configured && l.Review.Note == noteReviewNotConfigured:
		fmt.Fprintf(&b, "review: not configured\n")
	default:
		fmt.Fprintf(&b, "review: %s\n", l.Review.Note)
	}
	for _, w := range l.Warnings {
		fmt.Fprintf(&b, "warning: %s\n", w)
	}
	return strings.TrimRight(b.String(), "\n")
}
