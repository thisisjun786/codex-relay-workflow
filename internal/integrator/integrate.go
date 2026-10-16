package integrator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/thisisjun786/codex-relay-workflow/internal/tracking"
)

// resultPushing is the marker a run writes into <tag>.summary.json right before its push: the push may land with no
// "pushed" summary behind it (the signal ended the call after git exited 0), and the recovery of that landing accepts the
// marker when its head is in the branch. It is never a pass result; the run's own result overwrites it.
const resultPushing = "pushing"

// Summary is what one integration run reports, in the shell script's own shape: the heads
// merged as "node=head" and the heads left out as "node:reason", each list space-separated.
// Result is the run's result, or resultPushing in the summary of a run that ended during its push.
type Summary struct {
	Tag     string `json:"tag"`
	Base    string `json:"base"`
	Head    string `json:"head"`
	Merged  string `json:"merged"`
	LeftOut string `json:"left_out"`
	Record  string `json:"record"`
	Result  string `json:"result"`
}

// The reasons a head is left out of a run.
const (
	reasonBadHead      = "bad-head"
	reasonNotInCheck   = "head-not-in-checkout"
	reasonAlreadyInDev = "already-in-dev"
	reasonConflict     = "conflict"
	reasonMergeError   = "merge-error"
)

// Outcome is one run's result as the pass acts on it.
type Outcome struct {
	Exit    int
	Summary *Summary
	Err     error // set when Exit is ExitUsage: nothing ran
	merged  []Entry
	left    []leftOut
	// verifyRC is the verification command's status, -1 when it did not run; review is the review of a pull request's merged
	// tree, nil when none ran.
	verifyRC int
	review   *LandingReview
	// headMoved is the pull request's head GitHub named right before the branch was to move, when it was no longer the merged
	// head; prNote says why the branch did not move for a pull request that changed during the run.
	headMoved string
	prNote    string
}

type leftOut struct {
	Entry  Entry
	Reason string
}

// integrate merges the entries onto the remote branch in a private worktree, verifies the
// merged tree and pushes it fast-forward only. The caller holds the integration lock. With pr
// (one entry, the pull request's head) the review command judges the verified tree before the
// push, and a review that asks for changes pushes nothing.
func (r *Runner) integrate(ctx context.Context, tag string, entries []Entry, pr *tracking.PullRequest) Outcome {
	c := r.Cfg
	fail := func(format string, args ...any) Outcome {
		return Outcome{Exit: ExitUsage, Err: fmt.Errorf(format, args...)}
	}
	for _, d := range []string{c.RunsDir, c.WorkRoot} {
		if err := os.MkdirAll(d, 0o775); err != nil {
			return fail("%v", err)
		}
	}
	if err := r.prepare(); err != nil {
		return fail("%v", err)
	}
	if rc, err := r.git(ctx, c.Checkout, nil, "fetch", c.Remote, "--quiet"); err != nil || rc != 0 {
		return fail("fetch failed")
	}
	base, err := r.gitOut(ctx, c.Checkout, "rev-parse", c.remoteRef())
	if err != nil || !headRE.MatchString(base) {
		return fail("cannot resolve %s: %v", c.remoteRef(), err)
	}
	wt := filepath.Join(c.WorkRoot, "run-"+tag)
	if _, err := os.Lstat(wt); err == nil {
		return fail("%s exists; remove it with git worktree remove first", wt)
	}
	// The temporary directory is created here, exclusively, and only a directory this run
	// created is removed with it: a name that already exists holds someone else's data.
	tmp := filepath.Join(c.WorkRoot, "tmp-"+tag)
	if err := os.Mkdir(tmp, 0o775); err != nil {
		if errors.Is(err, os.ErrExist) {
			return fail("%s exists; this run did not create it and does not remove it", tmp)
		}
		return fail("%v", err)
	}
	logPath := filepath.Join(c.RunsDir, tag+".log")
	logf, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o664)
	if err != nil {
		_ = os.Remove(tmp)
		return fail("%v", err)
	}
	defer logf.Close()
	added := false
	defer func() {
		// The worktree and the private temporary directory are the run's own; neither
		// outlives it, whatever the result.
		cleanup := context.WithoutCancel(ctx)
		if added {
			_, _ = r.git(cleanup, c.Checkout, nil, "worktree", "remove", "--force", wt)
		}
		_ = os.RemoveAll(tmp)
	}()
	if rc, err := r.git(ctx, c.Checkout, logf, "worktree", "add", "--detach", wt, base); err != nil || rc != 0 {
		// A failed add may have left a half-made worktree; remove it as an added one.
		added = true
		return fail("worktree add failed")
	}
	added = true

	out := Outcome{verifyRC: -1}
	for _, e := range entries {
		if ctx.Err() != nil {
			return fail("%v", ctx.Err())
		}
		msg := fmt.Sprintf("Merge %s at %s into %s", e.Node, shortHead(e.Head), c.Branch)
		if pr != nil {
			msg = fmt.Sprintf("Merge pull request %s (%s) at %s into %s", pr.Ref, pr.HeadRef, shortHead(e.Head), c.Branch)
		}
		reason := r.mergeOne(ctx, wt, e, msg, logf)
		if ctx.Err() != nil {
			// A git call that ended with the context says nothing about the head.
			return fail("%v", ctx.Err())
		}
		switch {
		case reason == "":
			out.merged = append(out.merged, e)
		default:
			out.left = append(out.left, leftOut{Entry: e, Reason: reason})
		}
	}

	if ctx.Err() != nil {
		return fail("%v", ctx.Err())
	}

	head, err := r.gitOut(ctx, wt, "rev-parse", "HEAD")
	if err != nil {
		return fail("%v", err)
	}
	record := filepath.Join(c.RunsDir, tag+".json")
	sum := &Summary{Tag: tag, Base: base, Head: head, Record: record}
	for _, e := range out.merged {
		sum.Merged = joinSpace(sum.Merged, e.Spec())
	}
	for _, l := range out.left {
		sum.LeftOut = joinSpace(sum.LeftOut, l.Entry.Node+":"+l.Reason)
	}
	out.Summary = sum
	finish := func(result string, exit int) Outcome {
		sum.Result = result
		_ = r.writeSummary(sum)
		out.Exit = exit
		return out
	}
	if len(out.merged) == 0 {
		return finish("nothing-merged", ExitNothingMerged)
	}

	// What is verified is what is pushed: the commit HEAD, with nothing beside it in the
	// worktree that the verification would see and the push would not carry.
	if dirty, err := r.gitOut(ctx, wt, "status", "--porcelain"); err != nil || dirty != "" {
		if err == nil {
			err = fmt.Errorf("the merged worktree has changes no commit holds; nothing is verified or pushed:\n%s", dirty)
		}
		return fail("%v", err)
	}

	vrc := r.verify(ctx, tag, wt, head, base, record)
	out.verifyRC = vrc
	if vrc != 0 {
		if ctx.Err() != nil {
			// A verification the signal ended is not a failed verification: no retry is
			// spent and no head is blocked because of it.
			return fail("%v", ctx.Err())
		}
		return finish(fmt.Sprintf("verification-failed(rc=%d)", vrc), ExitVerifyFailed)
	}
	// The verification is a configurable program: the tree it judged must still be the commit
	// that is pushed. A HEAD it moved, or a change it left beside the commit, is not a pass.
	if err := r.treeIsTheCommit(ctx, wt, head); err != nil {
		return fail("%v", err)
	}
	if pr != nil {
		// the review comes after the verification passed and before dev moves (CRW-1355)
		rv := r.review(ctx, tag, wt, head, base, record, *pr, entries[0].Head, logf)
		if ctx.Err() != nil {
			return fail("%v", ctx.Err())
		}
		out.review = &rv
		if rv.Verdict == verdictNeedsChanges {
			return finish("review-needs-changes", ExitReviewRefused)
		}
		// like the verification, the review judged the commit that is pushed and changed nothing beside it
		if err := r.treeIsTheCommit(ctx, wt, head); err != nil {
			return fail("the review: %v", err)
		}
		// the pull request is read again right before the branch moves: a head pushed during the verification or the review
		// is not what was verified, so the branch does not move for the old head and the entry stays pending
		result, moved, why := r.recheckPR(ctx, *pr, entries[0].Head)
		if ctx.Err() != nil {
			return fail("%v", ctx.Err())
		}
		if result != "" {
			fmt.Fprintf(logf, "%s: %s; %s does not move\n", pr.Ref, why, c.Branch)
			out.headMoved, out.prNote = moved, why
			return finish(result, ExitMoved)
		}
	}
	// From here on a git call the signal ended is not a moved branch or a refused push: the
	// pass is cancelled and the heads stay pending (a push that did land makes them
	// already-in-dev on the next pass).
	if ctx.Err() != nil {
		return fail("%v", ctx.Err())
	}
	if rc, err := r.git(ctx, c.Checkout, nil, "fetch", c.Remote, "--quiet"); err != nil || rc != 0 {
		if ctx.Err() != nil {
			return fail("%v", ctx.Err())
		}
		r.logf("%s: the second fetch failed; the push is still fast-forward only", tag)
	}
	now, err := r.gitOut(ctx, c.Checkout, "rev-parse", c.remoteRef())
	if ctx.Err() != nil {
		return fail("%v", ctx.Err())
	}
	if err != nil || now != base {
		return finish("dev-moved:"+now, ExitMoved)
	}
	// The marker first: a push the signal ended after it landed leaves this summary for the recovery to find (finish overwrites it).
	// Without the marker on disk a push the signal ended after it landed leaves nothing to find, so a marker that cannot be written
	// (runs_dir full or read-only) is no push: the pass fails and the entries stay pending.
	sum.Result = resultPushing
	if err := r.writeSummary(sum); err != nil {
		return fail("the pushing marker could not be written, nothing is pushed: %v", err)
	}
	// Never forced: no plus, no --force, so a remote that moved refuses the push.
	if rc, err := r.git(ctx, wt, logf, "push", c.Remote, head+":refs/heads/"+c.Branch); err != nil || rc != 0 {
		if ctx.Err() != nil {
			return fail("%v", ctx.Err())
		}
		return finish("push-failed", ExitMoved)
	}
	return finish("pushed", ExitPushed)
}

// shortHead is the first eight characters of a head (the whole of a shorter one).
func shortHead(h string) string {
	if len(h) > 8 {
		return h[:8]
	}
	return h
}

func joinSpace(list, item string) string {
	if list == "" {
		return item
	}
	return list + " " + item
}

// mergeOne merges one head into the worktree and returns why it was left out ("" when merged).
func (r *Runner) mergeOne(ctx context.Context, wt string, e Entry, msg string, log io.Writer) string {
	if !headRE.MatchString(e.Head) {
		return reasonBadHead
	}
	if rc, err := r.git(ctx, wt, nil, "cat-file", "-e", e.Head+"^{commit}"); err != nil || rc != 0 {
		return reasonNotInCheck
	}
	if rc, err := r.git(ctx, wt, nil, "merge-base", "--is-ancestor", e.Head, "HEAD"); err == nil && rc == 0 {
		return reasonAlreadyInDev
	}
	rc, err := r.git(ctx, wt, log, "merge", "--no-ff", "-m", msg, e.Head)
	if err == nil && rc == 0 {
		return ""
	}
	// A merge that stopped for conflicts leaves MERGE_HEAD; anything else is not a conflict.
	inMerge := false
	if rc, err := r.git(ctx, wt, nil, "rev-parse", "-q", "--verify", "MERGE_HEAD"); err == nil && rc == 0 {
		inMerge = true
	}
	_, _ = r.git(ctx, wt, log, "merge", "--abort")
	if !inMerge {
		return reasonMergeError
	}
	return reasonConflict
}

// treeIsTheCommit reports an error unless the worktree's HEAD is still the commit that was
// verified and the worktree holds nothing beside it (no changed, staged or untracked file).
func (r *Runner) treeIsTheCommit(ctx context.Context, wt, head string) error {
	now, err := r.gitOut(ctx, wt, "rev-parse", "HEAD")
	if err != nil {
		return fmt.Errorf("after the verification: %w", err)
	}
	if now != head {
		return fmt.Errorf("the verification moved HEAD from %s to %s; the tree it judged is not the commit to push, so nothing is pushed", head, now)
	}
	dirty, err := r.gitOut(ctx, wt, "status", "--porcelain")
	if err != nil {
		return fmt.Errorf("after the verification: %w", err)
	}
	if dirty != "" {
		return fmt.Errorf("the verification left changes no commit holds; the tree it judged is not the commit to push, so nothing is pushed:\n%s", dirty)
	}
	return nil
}

// verify runs the verification command on the merged tree and returns its status (127 when it
// could not be started).
func (r *Runner) verify(ctx context.Context, tag, wt, head, base, record string) int {
	c := r.Cfg
	vlog, err := os.OpenFile(filepath.Join(c.RunsDir, tag+".verify.log"), os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o664)
	if err != nil {
		return 127
	}
	defer vlog.Close()
	// The verification runs against the private worktree only.
	env := append(r.gitEnv(), "CRW_VERIFY_HEAD="+head, "CRW_VERIFY_BASE="+base, "CRW_VERIFY_RECORD="+record)
	rc, err := r.Exec(ctx, Cmd{Argv: c.VerifyCommand, Dir: wt, Env: env, Stdout: vlog, Stderr: vlog})
	if err != nil {
		fmt.Fprintf(vlog, "verify command: %v\n", err)
		return 127
	}
	return rc
}

// writeSummary writes <tag>.summary.json and <tag>.out, the files the shell script left.
// It answers the first write that failed; the final summaries of a finished run stay best effort, the pushing marker does not.
func (r *Runner) writeSummary(s *Summary) error {
	data, err := json.Marshal(s)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	var first error
	for _, ext := range []string{".summary.json", ".out"} {
		if err := os.WriteFile(filepath.Join(r.Cfg.RunsDir, s.Tag+ext), data, 0o664); err != nil && first == nil {
			first = err
		}
	}
	return first
}
