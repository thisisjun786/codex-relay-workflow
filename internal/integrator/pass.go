package integrator

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/tracking"
)

// resultReportLandedBeforeRecord is the pass result of a recovered landing (the hyphenated form, like the other pass results; the
// record itself says landed_before_record).
const resultReportLandedBeforeRecord = "landed-before-record"

// Report is what one pass reports.
type Report struct {
	// Result is idle, stopped, locked, nothing-to-integrate, error, or the run's own result
	// (pushed, nothing-merged, verification-failed(rc=N), dev-moved:<sha>, push-failed; for a pull request also
	// review-needs-changes, head-moved:<sha> and pull-request-changed).
	// landed-before-record (status 0) is a pull request whose head was already in the branch with no landing record: the pass wrote the
	// recovered record (Landing), ran nothing (no Summary) and moved the entry to done/.
	// pushing is the marker a run leaves in its <tag>.summary.json while its push is under way; it is never a pass result.
	Result  string   `json:"result"`
	Exit    int      `json:"exit"`
	Error   string   `json:"error,omitempty"`
	Summary *Summary `json:"summary,omitempty"`
	// Moved is where each pending file went, as "<name> -> <dir>: <reason>".
	Moved   []string `json:"moved,omitempty"`
	Ignored []string `json:"ignored,omitempty"`
	// Retry describes the retry step the pass took after a failed verification.
	Retry string `json:"retry,omitempty"`
	// Held are the pending pull requests this pass could not read, as "<name>: <why>"; they stay pending.
	Held []string `json:"held,omitempty"`
	// Landing is the record of a pull request's run (CRW-1355), nil for a run of issue heads.
	Landing *Landing `json:"pull_request_landing,omitempty"`
}

// Pass runs one pass of the queue: the shell runner's loop body. A stop file ends it before it
// reads the queue; an empty queue is idle; a lock another run holds is refused (ExitUsage)
// without reading the queue.
func (r *Runner) Pass(ctx context.Context) Report {
	if fileExists(r.qpath(fileStop)) {
		r.logf("stop file, exiting")
		return Report{Result: "stopped", Exit: ExitPushed}
	}
	if err := r.ensureQueueDirs(); err != nil {
		return Report{Result: "error", Exit: ExitUsage, Error: err.Error()}
	}
	entries, ignored, err := r.listPending()
	if err != nil {
		return Report{Result: "error", Exit: ExitUsage, Error: err.Error()}
	}
	if len(entries) == 0 && len(r.listClaims()) == 0 {
		return Report{Result: "idle", Exit: ExitPushed, Ignored: ignored}
	}
	unlock, err := acquireLock(r.Cfg.LockFile)
	if err != nil {
		if errors.Is(err, ErrLockHeld) {
			return Report{Result: "locked", Exit: ExitUsage, Error: err.Error(), Ignored: ignored}
		}
		return Report{Result: "error", Exit: ExitUsage, Error: err.Error(), Ignored: ignored}
	}
	defer unlock()

	rep := Report{Ignored: ignored}
	// A file an earlier pass could neither move on nor put back is pending again first.
	r.recoverClaims()
	// The queue is read again under the lock: another run may have emptied it meanwhile.
	entries, _, err = r.listPending()
	if err != nil || len(entries) == 0 {
		rep.Result, rep.Exit = "idle", ExitPushed
		return rep
	}
	// A pending pull request is landed alone, before the issue heads (CRW-1355): one pull request per pass, the lowest number first.
	var prs, heads []Entry
	for _, e := range entries {
		if e.PR > 0 {
			prs = append(prs, e)
		} else {
			heads = append(heads, e)
		}
	}
	for _, e := range prs {
		// a head of the pull request that already landed (done/ or its landing record) is done again, with no second record
		if why := r.landedBefore(e); why != "" {
			r.logf("%s %s: %s", e.Key, e.Head, why)
			// a record whose comment an earlier pass did not get to is posted now, once
			r.repostLandingComment(ctx, e)
			rep.moved(r, e, dirDone, why)
			continue
		}
		pr, held := r.checkPR(ctx, e, &rep)
		if ctx.Err() != nil {
			return r.cancelled(ctx, rep)
		}
		if held != "" {
			// not a verdict on the pull request: it waits in pending, and the queue goes on without it
			rep.Held = append(rep.Held, e.Name+": "+held)
			continue
		}
		if rep.Landing != nil {
			// checkPR found the head in the branch and recorded its landing after the fact: that is what the pass did. It ends the pass
			// (one pull request per pass); the pull requests and issue heads behind it wait for the next one, and Held of earlier
			// pull requests stays in the report without turning the result into an error.
			rep.Result, rep.Exit = resultReportLandedBeforeRecord, ExitPushed
			return rep
		}
		if pr != nil {
			return r.run(ctx, rep, []Entry{e}, pr)
		}
	}
	if len(prs) > 0 && len(heads) == 0 {
		if len(rep.Held) > 0 {
			rep.Result, rep.Exit, rep.Error = "error", ExitUsage, strings.Join(rep.Held, "; ")
		} else {
			rep.Result, rep.Exit = "nothing-to-integrate", ExitNothingMerged
		}
		return rep
	}
	entries = heads
	solo := len(r.soloPrune(entries)) > 0
	var batch []Entry
	for _, e := range entries {
		if ctx.Err() != nil {
			return r.cancelled(ctx, rep)
		}
		if !e.HeadOK {
			r.logf("%s %q is not a full head SHA: blocked", e.Key, e.Head)
			rep.moved(r, e, dirBlocked, "head is not a 40-hex commit")
			continue
		}
		if err := r.prepare(); err != nil {
			r.logf("%s %s prepare: %v", e.Key, e.Head, err)
			rep.Result, rep.Exit, rep.Error = "error", ExitUsage, err.Error()
			return rep
		}
		note, verdict := r.gate(ctx, e)
		if ctx.Err() != nil {
			// A gate the signal ended says nothing about the head.
			return r.cancelled(ctx, rep)
		}
		switch verdict {
		case gateAborted:
			// The gate could not run: not a verdict, so the head and those behind it wait.
			r.logf("%s %s gate could not run: %s", e.Key, e.Head, note)
			rep.Result, rep.Exit, rep.Error = "error", ExitUsage, note
			return rep
		case gateFail:
			r.logf("%s %s gate not PASS: %s", e.Key, e.Head, note)
			rep.moved(r, e, dirBlocked, "gate not PASS: "+note)
			continue
		}
		batch = append(batch, e)
		if solo {
			break
		}
	}
	if len(batch) == 0 {
		rep.Result, rep.Exit = "nothing-to-integrate", ExitNothingMerged
		return rep
	}
	return r.run(ctx, rep, batch, nil)
}

// run is one integration run of the batch (or of one pull request's head, with pr) and what the pass does with its result.
func (r *Runner) run(ctx context.Context, rep Report, batch []Entry, pr *tracking.PullRequest) Report {
	tag, err := r.reserveTag()
	if err != nil {
		r.logf("run tag: %v", err)
		rep.Result, rep.Exit, rep.Error = "error", ExitUsage, err.Error()
		return rep
	}
	specs := make([]string, len(batch))
	for i, e := range batch {
		specs[i] = e.Spec()
	}
	r.logf("%s start %s", tag, strings.Join(specs, " "))
	out := r.integrate(ctx, tag, batch, pr)
	if out.Err != nil {
		if ctx.Err() != nil {
			return r.cancelled(ctx, rep)
		}
		r.logf("%s rc=%d %v", tag, out.Exit, out.Err)
		rep.Result, rep.Exit, rep.Error = "error", out.Exit, out.Err.Error()
		return rep
	}
	rep.Summary, rep.Exit, rep.Result = out.Summary, out.Exit, out.Summary.Result
	data, _ := jsonLine(out.Summary)
	r.logf("%s rc=%d %s", tag, out.Exit, data)
	if pr != nil {
		if out.Exit == ExitNothingMerged && len(out.left) == 1 && out.left[0].Reason == reasonAlreadyInDev {
			// the head reached the branch between the pass's check and its run: recorded once, after the fact
			l, err := r.recoverLanding(ctx, tag, batch[0], *pr)
			if err != nil {
				r.logf("%s: %v", pr.Ref, err)
			}
			rep.Landing = l
		} else {
			rep.Landing = r.recordLanding(ctx, *pr, out)
		}
		if out.headMoved != "" {
			r.retarget(batch[0], out.headMoved)
		}
	}
	if out.Exit == ExitReviewRefused {
		reason := "review needs_changes"
		if out.review != nil {
			reason += ": " + out.review.VerdictFile
		}
		for _, e := range out.merged {
			rep.moved(r, e, dirBlocked, reason)
		}
	}

	if out.Exit == ExitPushed || out.Exit == ExitNothingMerged {
		if out.Exit == ExitPushed {
			for _, e := range out.merged {
				rep.moved(r, e, dirDone, "merged")
			}
		}
		for _, l := range out.left {
			// A head that is already in the branch is done; any other left-out head goes back to
			// its owner to refresh.
			if l.Reason == reasonAlreadyInDev {
				rep.moved(r, l.Entry, dirDone, l.Reason)
			} else {
				rep.moved(r, l.Entry, dirConflict, l.Reason)
			}
		}
	}
	r.afterRun(batch, out, &rep)
	return rep
}

// retarget puts the pull request's new head in place of the head the run merged, in pending/<name>, so the next pass verifies and
// reviews the new head. A pending file that no longer holds the run's head (the project session enqueued again meanwhile) is
// left as it is.
func (r *Runner) retarget(e Entry, head string) {
	if now, err := readHead(r.qpath(dirPending, e.Name)); err != nil || now != e.Head {
		r.logf("%s %s changed or went away during the run: left as it is", e.Key, e.Head)
		return
	}
	if err := r.writePending(e.Name, head); err != nil {
		r.logf("%s: the new head %s could not be written to pending: %v", e.Key, head, err)
		return
	}
	r.logf("%s %s: the pull request's head moved during the run; pending now holds %s", e.Key, e.Head, head)
}

// runFileExts are the files a run leaves in runs_dir under its tag.
var runFileExts = []string{".log", ".out", ".summary.json", ".verify.log", ".json", ".review.log", ".review.json", ".pr.json"}

// reserveTag picks the tag of the next run, tag_prefix plus the month, day and time, and
// reserves it by creating its log exclusively. A tag an earlier run already used (two passes in
// the same second) gets the suffix -2, -3, ..., so no run overwrites another's evidence.
func (r *Runner) reserveTag() (string, error) {
	if err := os.MkdirAll(r.Cfg.RunsDir, 0o775); err != nil {
		return "", err
	}
	stem := r.Cfg.TagPrefix + r.Now().Format("0102150405")
	for i := 1; i <= 1000; i++ {
		tag := stem
		if i > 1 {
			tag = fmt.Sprintf("%s-%d", stem, i)
		}
		used := false
		for _, ext := range runFileExts[1:] {
			if _, err := os.Lstat(filepath.Join(r.Cfg.RunsDir, tag+ext)); err == nil {
				used = true
				break
			}
		}
		if used {
			continue
		}
		f, err := os.OpenFile(filepath.Join(r.Cfg.RunsDir, tag+runFileExts[0]), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o664)
		if errors.Is(err, os.ErrExist) {
			continue
		}
		if err != nil {
			return "", err
		}
		return tag, f.Close()
	}
	return "", fmt.Errorf("no free run tag for %s in %s", stem, r.Cfg.RunsDir)
}

// cancelled ends a pass whose context ended: what was decided before stays, the rest is left
// in pending for the next pass.
func (r *Runner) cancelled(ctx context.Context, rep Report) Report {
	r.logf("pass cancelled: %v", ctx.Err())
	rep.Result, rep.Exit, rep.Error = "cancelled", ExitUsage, ctx.Err().Error()
	return rep
}

func (rep *Report) moved(r *Runner, e Entry, dir, reason string) {
	if r.moveEntry(e, dir) {
		rep.Moved = append(rep.Moved, fmt.Sprintf("%s -> %s: %s", e.Name, dir, reason))
	}
}

// afterRun is the retry policy after a run: a failed batch is retried once as the same batch
// (load flakes), then every one of its heads runs alone (not a bisection), and a head that
// fails alone is retried once before it is blocked.
func (r *Runner) afterRun(batch []Entry, out Outcome, rep *Report) {
	if out.Exit == ExitVerifyFailed {
		switch {
		case len(batch) > 1 && !fileExists(r.batchMarker(batch)):
			if err := r.touch(r.batchMarker(batch)); err != nil {
				r.logf("retry marker: %v", err)
			}
			rep.Retry = "same batch once more"
			r.logf("batch verification failed: one retry of the same batch (%s)", batchKey(batch))
		case len(batch) > 1:
			specs := make([]string, len(batch))
			for i, e := range batch {
				specs[i] = e.Spec()
			}
			r.setSoloOwed(specs)
			rep.Retry = fmt.Sprintf("one at a time: %d heads", len(batch))
			r.logf("batch verification failed: one at a time for each of its %d heads", len(batch))
		default:
			e := batch[0]
			marker := r.qpath(dirRetried, e.Name)
			if prev, _ := readHead(marker); prev != e.Head {
				if err := os.MkdirAll(r.qpath(dirRetried), 0o775); err == nil {
					err = os.WriteFile(marker, []byte(e.Head+"\n"), 0o664)
					if err != nil {
						r.logf("retry marker: %v", err)
					}
				}
				rep.Retry = "alone, one retry of the same head"
				r.logf("%s alone failed verification: one retry of %s", e.Key, e.Head)
			} else {
				rep.moved(r, e, dirBlocked, "alone failed verification twice")
				rep.Retry = "alone, blocked"
				r.logf("%s alone failed verification: blocked", e.Key)
			}
		}
	}
	// Solo mode owes each member of the failed batch a run of its own and lasts while one of them
	// is still pending with that head: a member's retry or a run that did not conclude (the
	// branch moved, the push was refused) keeps it owed, a pushed, blocked or moved one does not.
	if entries, _, err := r.listPending(); err == nil {
		r.soloPrune(entries)
	}
}

type gateVerdict int

const (
	gatePass    gateVerdict = iota
	gateFail                // the gate ran and did not say PASS
	gateAborted             // the gate could not run or did not finish: no verdict
)

// gate re-checks the pre-merge gate of one head: the configured command's last output line
// starts with PASS and its status is 0. The string is that line (or the reason the gate could
// not run). A gate that ran and said anything else is a negative verdict; one that could not
// start or was killed is no verdict at all.
func (r *Runner) gate(ctx context.Context, e Entry) (string, gateVerdict) {
	var out bytes.Buffer
	argv := expandAll(r.Cfg.GateCommand, "{issue}", e.Key, "{head}", e.Head)
	rc, err := r.Exec(ctx, Cmd{Argv: argv, Dir: r.Cfg.Checkout, Env: r.gitEnv(), Stdout: &out, Stderr: &out})
	last := lastLine(out.String())
	if err != nil {
		return "gate command: " + err.Error(), gateAborted
	}
	if rc != 0 || !strings.HasPrefix(last, "PASS") {
		if last == "" {
			last = fmt.Sprintf("status %d, no output", rc)
		}
		return last, gateFail
	}
	return last, gatePass
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimRight(s, "\r\n \t"), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}

// Watch runs a pass, waits the configured interval, and repeats until the stop file appears or
// ctx ends. report receives every pass that did something.
func (r *Runner) Watch(ctx context.Context, report func(Report)) error {
	for {
		rep := r.Pass(ctx)
		if rep.Result == "stopped" {
			report(rep)
			return nil
		}
		if rep.Result != "idle" {
			report(rep)
		}
		if err := r.Sleep(ctx, r.Cfg.Interval()); err != nil {
			return err
		}
	}
}
