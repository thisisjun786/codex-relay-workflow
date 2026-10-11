package integrator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
)

// Summary is what one integration run reports, in the shell script's own shape: the heads
// merged as "node=head" and the heads left out as "node:reason", each list space-separated.
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
}

type leftOut struct {
	Entry  Entry
	Reason string
}

// integrate merges the entries onto the remote branch in a private worktree, verifies the
// merged tree and pushes it fast-forward only. The caller holds the integration lock.
func (r *Runner) integrate(ctx context.Context, tag string, entries []Entry) Outcome {
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

	out := Outcome{}
	rerecord := false
	for _, e := range entries {
		if ctx.Err() != nil {
			return fail("%v", ctx.Err())
		}
		reason, resolvedVersion := r.mergeOne(ctx, wt, base, e, logf)
		if ctx.Err() != nil {
			// A git call that ended with the context says nothing about the head.
			return fail("%v", ctx.Err())
		}
		switch {
		case reason == "":
			out.merged = append(out.merged, e)
			rerecord = rerecord || resolvedVersion
		default:
			out.left = append(out.left, leftOut{Entry: e, Reason: reason})
		}
	}

	pluginChanged := false
	if len(out.merged) > 0 {
		rc, err := r.git(ctx, wt, nil, "diff", "--quiet", base, "HEAD", "--", c.PluginDir)
		pluginChanged = err != nil || rc != 0
	}
	if rerecord || pluginChanged {
		if err := r.recordVersion(ctx, tag, wt, tmp, logf); err != nil {
			return fail("%v", err)
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
		r.writeSummary(sum)
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

	if vrc := r.verify(ctx, tag, wt, head, base, record); vrc != 0 {
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
	// Never forced: no plus, no --force, so a remote that moved refuses the push.
	if rc, err := r.git(ctx, wt, logf, "push", c.Remote, head+":refs/heads/"+c.Branch); err != nil || rc != 0 {
		if ctx.Err() != nil {
			return fail("%v", ctx.Err())
		}
		return finish("push-failed", ExitMoved)
	}
	return finish("pushed", ExitPushed)
}

func joinSpace(list, item string) string {
	if list == "" {
		return item
	}
	return list + " " + item
}

// mergeOne merges one head into the worktree and returns why it was left out ("" when merged)
// and whether a version-line conflict was resolved on the way.
func (r *Runner) mergeOne(ctx context.Context, wt, base string, e Entry, log io.Writer) (string, bool) {
	c := r.Cfg
	if !headRE.MatchString(e.Head) {
		return reasonBadHead, false
	}
	if rc, err := r.git(ctx, wt, nil, "cat-file", "-e", e.Head+"^{commit}"); err != nil || rc != 0 {
		return reasonNotInCheck, false
	}
	if rc, err := r.git(ctx, wt, nil, "merge-base", "--is-ancestor", e.Head, "HEAD"); err == nil && rc == 0 {
		return reasonAlreadyInDev, false
	}
	msg := fmt.Sprintf("Merge %s at %s into %s", e.Node, e.Head[:8], c.Branch)
	rc, err := r.git(ctx, wt, log, "merge", "--no-ff", "-m", msg, e.Head)
	if err == nil && rc == 0 {
		return "", false
	}
	// A merge that stopped for conflicts leaves MERGE_HEAD; anything else is not a conflict.
	inMerge := false
	if rc, err := r.git(ctx, wt, nil, "rev-parse", "-q", "--verify", "MERGE_HEAD"); err == nil && rc == 0 {
		inMerge = true
	}
	if inMerge && r.resolveVersionOnly(ctx, wt, log) {
		return "", true
	}
	_, _ = r.git(ctx, wt, log, "merge", "--abort")
	if !inMerge {
		return reasonMergeError, false
	}
	return reasonConflict, false
}

// resolveVersionOnly resolves the merge in progress when the one conflicted file is the
// plugin manifest and its conflicts are only the version line, and commits the merge. It
// reports whether it did; on false the merge is left for the caller to abort.
func (r *Runner) resolveVersionOnly(ctx context.Context, wt string, log io.Writer) bool {
	c := r.Cfg
	unmerged, err := r.gitOut(ctx, wt, "diff", "--name-only", "--diff-filter=U")
	if err != nil || unmerged != c.PluginManifest {
		return false
	}
	path := filepath.Join(wt, filepath.FromSlash(c.PluginManifest))
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	resolved, err := ResolveVersionConflict(data)
	if err != nil {
		fmt.Fprintf(log, "%s: %v\n", c.PluginManifest, err)
		return false
	}
	if err := os.WriteFile(path, resolved, 0o644); err != nil {
		return false
	}
	fmt.Fprintf(log, "%s: version-line conflict resolved; re-derived after the merges\n", c.PluginManifest)
	if rc, err := r.git(ctx, wt, log, "add", "--", c.PluginManifest); err != nil || rc != 0 {
		return false
	}
	rc, err := r.git(ctx, wt, log, "commit", "--no-edit", "-q")
	return err == nil && rc == 0
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

// recordVersion re-derives the manifest's version line on the merged tree with the configured
// command and commits the change. A failing command is logged and left to the verification; a
// change that cannot be committed is an error, because the verification would see a tree the
// push does not carry.
func (r *Runner) recordVersion(ctx context.Context, tag, wt, tmp string, log io.Writer) error {
	c := r.Cfg
	if err := os.MkdirAll(tmp, 0o775); err != nil {
		fmt.Fprintf(log, "record version: %v\n", err)
		return nil
	}
	pairs := []string{"{tmpdir}", tmp, "{tag}", tag}
	// The command finds its repository from its directory, the private worktree, so no
	// inherited Git routing variable reaches it. record_version_env is the operator's explicit
	// word and is applied after that cleanup.
	env := r.gitEnv()
	for _, k := range sortedKeys(c.RecordVersionEnv) {
		env = append(env, k+"="+expand(c.RecordVersionEnv[k], pairs...))
	}
	rc, err := r.Exec(ctx, Cmd{Argv: expandAll(c.RecordVersionCommand, pairs...), Dir: wt, Env: env, Stdout: log, Stderr: log})
	if err != nil || rc != 0 {
		fmt.Fprintf(log, "record version command: status %d, %v\n", rc, err)
	}
	if rc, err := r.git(ctx, wt, nil, "diff", "--quiet", "--", c.PluginManifest); err == nil && rc == 0 {
		return nil
	}
	if rc, err := r.git(ctx, wt, log, "add", "--", c.PluginManifest); err != nil || rc != 0 {
		return fmt.Errorf("record version: git add of %s failed (status %d, %v); nothing is verified or pushed", c.PluginManifest, rc, err)
	}
	msg := "integration " + tag + ": re-derive the plugin version line (payload digest) after the merges"
	if rc, err := r.git(ctx, wt, log, "commit", "-q", "-m", msg); err != nil || rc != 0 {
		return fmt.Errorf("record version: the re-derived version line could not be committed (status %d, %v); nothing is verified or pushed", rc, err)
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
	// Like the record command, the verification runs against the private worktree only.
	env := append(r.gitEnv(), "CRW_VERIFY_HEAD="+head, "CRW_VERIFY_BASE="+base, "CRW_VERIFY_RECORD="+record)
	rc, err := r.Exec(ctx, Cmd{Argv: c.VerifyCommand, Dir: wt, Env: env, Stdout: vlog, Stderr: vlog})
	if err != nil {
		fmt.Fprintf(vlog, "verify command: %v\n", err)
		return 127
	}
	return rc
}

// writeSummary writes <tag>.summary.json and <tag>.out, the files the shell script left.
func (r *Runner) writeSummary(s *Summary) {
	data, err := json.Marshal(s)
	if err != nil {
		return
	}
	data = append(data, '\n')
	for _, ext := range []string{".summary.json", ".out"} {
		_ = os.WriteFile(filepath.Join(r.Cfg.RunsDir, s.Tag+ext), data, 0o664)
	}
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
