package pullrequest

// `crw flow pr decide` (CRW-1444): the parent's merge decision for a pull request of the dev queue. It looks at the pull
// request's head, the base tip, the checks, the queue's order and the release impact — never at the diff's content — and
// enqueues the pull request only when all of them pass. When the base moved past the head a no-ff merge of the base tip into
// the head is pushed (never forced, compare-and-swap on the advertised remote head through a private pre-push hook) and the
// decision is needs_ci. Every semantic decision is persisted as a crw-pr-decision/1 record and, for the all-green path,
// announced on the pull request once.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"

	"github.com/thisisjun786/codex-relay-workflow/internal/gitprobe"
	"github.com/thisisjun786/codex-relay-workflow/internal/integrator"
	"github.com/thisisjun786/codex-relay-workflow/internal/tracking"
)

// DecisionSchema names the record `crw flow pr decide` writes.
const DecisionSchema = "crw-pr-decision/1"

// The states a Decision answers. refused carries a Reason; needs_ci the pushed merge as UpdatedHead; hold_order the Earlier
// pull requests it overlaps; enqueued the queue Entry.
const (
	DecisionRefused   = "refused"
	DecisionNeedsBase = "needs_base"
	DecisionNeedsCI   = "needs_ci"
	DecisionHoldOrder = "hold_order"
	DecisionEnqueued  = "enqueued"
	// decisionReady is the persisted state between the last check and the enqueue call.
	decisionReady = "ready"
)

// The reasons a refusal carries.
const (
	reasonHeadMoved       = "head_moved"
	reasonPRNotOpen       = "pr_not_open"
	reasonWrongBase       = "wrong_base"
	reasonWrongRepository = "wrong_repository"
	reasonChecksFailed    = "checks_failed"
	reasonChecksPending   = "checks_pending"
	reasonChecksNone      = "checks_none"
)

// ErrDecideInput marks an input Decide cannot use: a bad flag value, a head that is not a full commit, a pull request of
// another repository than the configured pull_request_repo, or a missing release impact. The command line answers it with 2.
var ErrDecideInput = errors.New("the input cannot be used")

var (
	decideSHA    = regexp.MustCompile(`^[0-9a-f]{40}$`)
	decideBranch = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._/-]*$`)
	pendingPR    = regexp.MustCompile(`^pr-([0-9]{1,18})$`)
)

// DecideInput is what `crw flow pr decide` and `crw manage integrate --enqueue-pr` are given.
type DecideInput struct {
	// PR is the pull request as OWNER/NAME#N of the queue's pull_request_repo.
	PR string `json:"pr"`
	// Head is the pull request's expected head: the full commit id the decision pins.
	Head string `json:"head"`
	// ReleaseImpact is the supplied impact line, "none" included; it is recorded, never inferred.
	ReleaseImpact string `json:"release_impact"`
	// Record names the decision file (default <runs_dir>/decisions/pr-<N>-<head>.json).
	Record string `json:"record,omitempty"`
	// UpdateBase allows merging the moved base tip into the head (no-ff, never forced) and answering needs_ci.
	UpdateBase bool `json:"update_base"`
}

// Decision is the persisted and printed verdict of a parent merge decision.
type Decision struct {
	Schema        string            `json:"schema"`
	PR            string            `json:"pr"`
	Head          string            `json:"head"`
	Base          string            `json:"base"`
	BaseHead      string            `json:"base_head"`
	State         string            `json:"state"`
	Reason        string            `json:"reason,omitempty"`
	ReleaseImpact string            `json:"release_impact"`
	Earlier       []string          `json:"earlier,omitempty"`
	UpdatedHead   string            `json:"updated_head,omitempty"`
	Enqueued      *integrator.Entry `json:"enqueued,omitempty"`
	Record        string            `json:"record"`
}

// checksSource is the checks half of the tracking adapter, asserted from the runner's pull request source.
type checksSource interface {
	Checks(context.Context, tracking.Ref) (tracking.Checks, error)
}

// decider carries one decision: the resolved configuration, the git and pull request seams, and the pinned expectations.
type decider struct {
	cfg    integrator.Config
	exec   integrator.Exec
	env    []string
	src    integrator.PullRequestSource
	checks checksSource
	in     DecideInput
	ref    tracking.Ref
}

func (d *decider) gitErr(ctx context.Context, dir string, args ...string) (stdout, stderr string, rc int, err error) {
	var out, errb bytes.Buffer
	rc, err = d.exec(ctx, integrator.Cmd{Argv: append([]string{d.cfg.Git, "-C", dir}, args...), Env: d.env, Stdout: &out, Stderr: &errb})
	return out.String(), errb.String(), rc, err
}

func (d *decider) gitOK(ctx context.Context, dir string, args ...string) (string, error) {
	out, errb, rc, err := d.gitErr(ctx, dir, args...)
	if err != nil {
		return "", err
	}
	if rc != 0 {
		return "", fmt.Errorf("git %s: status %d: %.4096s", strings.Join(args, " "), rc, strings.TrimSpace(errb))
	}
	return strings.TrimSpace(out), nil
}

// advertise is the remote's advertised sha40 of each wanted head ref: ls-remote after fetch (objects), or alone to re-check.
func (d *decider) advertise(ctx context.Context, refs ...string) (map[string]string, error) {
	args := append([]string{"ls-remote", "--heads", "--", d.cfg.Remote}, refs...)
	out, err := d.gitOK(ctx, d.cfg.Checkout, args...)
	if err != nil {
		return nil, err
	}
	got := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		sha, ref, _ := strings.Cut(line, "\t")
		got[ref] = sha
	}
	for _, want := range refs {
		full := "refs/heads/" + want
		if !decideSHA.MatchString(got[full]) {
			return nil, fmt.Errorf("the remote %s advertises no %s (%q)", d.cfg.Remote, full, got[full])
		}
	}
	return got, nil
}

// fetchRefs pulls the objects of the wanted branches into the checkout's object store without moving any ref but FETCH_HEAD;
// the refspecs are fully qualified so no tag of the same short name can be mistaken for a branch.
func (d *decider) fetchRefs(ctx context.Context, branches ...string) error {
	args := []string{"fetch", "--no-tags", "--", d.cfg.Remote}
	for _, b := range branches {
		args = append(args, "refs/heads/"+b)
	}
	_, err := d.gitOK(ctx, d.cfg.Checkout, args...)
	return err
}

// Decide is the parent's merge decision for the pull request: head match, base tip, checks, queue order and the release
// impact, then the enqueue. Semantic answers come back as a Decision with nil error; IO and configuration failures are
// errors. The queue's integration lock (the same file integrator uses, held over the whole decision) serializes it against
// the passes.
func Decide(ctx context.Context, runner *integrator.Runner, in DecideInput) (Decision, error) {
	cfg, err := runner.Cfg.Resolved()
	if err != nil {
		return Decision{}, err
	}
	ref, err := tracking.ParseRef(in.PR)
	if err != nil {
		return Decision{}, fmt.Errorf("%w: --pr %q: %v", ErrDecideInput, in.PR, err)
	}
	if cfg.PullRequestRepo == "" {
		return Decision{}, errors.New("the integrate section names no pull_request_repo: the queue lands no pull request")
	}
	if !strings.EqualFold(ref.Repo, cfg.PullRequestRepo) {
		return Decision{}, fmt.Errorf("%w: %s is not a pull request of %s, the queue's pull_request_repo", ErrDecideInput, ref, cfg.PullRequestRepo)
	}
	ref.Repo = cfg.PullRequestRepo
	if !decideSHA.MatchString(in.Head) {
		return Decision{}, fmt.Errorf("%w: --head %q is not a full commit id", ErrDecideInput, in.Head)
	}
	if strings.TrimSpace(in.ReleaseImpact) == "" || strings.ContainsAny(in.ReleaseImpact, "\r\n") {
		return Decision{}, fmt.Errorf("%w: --release-impact is required, one line", ErrDecideInput)
	}
	if runner.PullRequests == nil {
		return Decision{}, errors.New("the queue's pull request source is not wired (pull_request_repo has no tracking adapter)")
	}
	src, ok := runner.PullRequests.(interface {
		integrator.PullRequestSource
		checksSource
	})
	if !ok {
		return Decision{}, errors.New("the queue's pull request source cannot read comments or checks")
	}
	exec := runner.Exec
	if exec == nil {
		exec = integrator.OSExec
	}
	d := &decider{
		cfg: cfg, exec: exec, src: src, checks: src, in: in, ref: ref,
		env: gitprobe.Sanitize(runner.Env, "GIT_TERMINAL_PROMPT=0", "GIT_EDITOR=true", "GIT_MERGE_AUTOEDIT=no"),
	}
	unlock, err := decideLock(cfg.LockFile)
	if err != nil {
		return Decision{}, err
	}
	defer unlock()
	// the remote must be a configured remote of the checkout before anything else
	remotes, err := d.gitOK(ctx, cfg.Checkout, "remote")
	if err != nil {
		return Decision{}, fmt.Errorf("the remotes of %s could not be listed: %w", cfg.Checkout, err)
	}
	configured := false
	for _, name := range strings.Split(remotes, "\n") {
		if name == cfg.Remote {
			configured = true
		}
	}
	if !configured {
		return Decision{}, fmt.Errorf("%s is not a remote configured in %s", cfg.Remote, cfg.Checkout)
	}
	if err := checkDecideBranch("base", cfg.Branch); err != nil {
		return Decision{}, err
	}

	record := in.Record
	if record == "" {
		record = filepath.Join(cfg.RunsDir, "decisions", fmt.Sprintf("pr-%d-%s.json", ref.Number, in.Head))
	}
	dec := Decision{Schema: DecisionSchema, PR: ref.String(), Head: in.Head, Base: cfg.Branch, ReleaseImpact: in.ReleaseImpact, Record: record}
	finish := func(state, reason string) (Decision, error) {
		dec.State, dec.Reason = state, reason
		if werr := writeJSONRecordFile(record, dec); werr != nil {
			return Decision{}, werr
		}
		return dec, nil
	}
	// the pull request must carry exactly the head the decision pins
	pr, err := src.PullRequest(ctx, ref)
	if err != nil {
		return Decision{}, err
	}
	if pr.HeadSHA != in.Head {
		return finish(DecisionRefused, reasonHeadMoved)
	}
	if pr.Merged || pr.State != tracking.StateOpen || pr.Draft {
		return finish(DecisionRefused, reasonPRNotOpen)
	}
	if pr.BaseRef != cfg.Branch {
		return finish(DecisionRefused, reasonWrongBase)
	}
	if !strings.EqualFold(pr.HeadRepo, cfg.PullRequestRepo) {
		return finish(DecisionRefused, reasonWrongRepository)
	}
	if err := checkDecideBranch("head", pr.HeadRef); err != nil {
		return Decision{}, err
	}
	// the remote's advertised tips, fetched into the object store without touching the checkout's branch
	if err := d.fetchRefs(ctx, cfg.Branch, pr.HeadRef); err != nil {
		return Decision{}, err
	}
	adv, err := d.advertise(ctx, cfg.Branch, pr.HeadRef)
	if err != nil {
		return Decision{}, err
	}
	baseSHA, remoteHead := adv["refs/heads/"+cfg.Branch], adv["refs/heads/"+pr.HeadRef]
	if remoteHead != in.Head {
		return finish(DecisionRefused, reasonHeadMoved)
	}
	for _, sha := range []string{baseSHA, in.Head} {
		if _, err := d.gitOK(ctx, cfg.Checkout, "cat-file", "-e", sha+"^{commit}"); err != nil {
			return Decision{}, fmt.Errorf("the fetched object %s is not in the checkout: %w", sha, err)
		}
	}
	dec.BaseHead = baseSHA
	// is the base tip already on the head's first line?
	_, errb, rc, err := d.gitErr(ctx, cfg.Checkout, "merge-base", "--is-ancestor", baseSHA, in.Head)
	if err != nil {
		return Decision{}, err
	}
	switch rc {
	case 0:
	case 1:
		if !in.UpdateBase {
			return finish(DecisionNeedsBase, "")
		}
		merged, moved, err := d.updateBase(ctx, pr, baseSHA)
		if err != nil {
			return Decision{}, err
		}
		if moved {
			return finish(DecisionRefused, reasonHeadMoved)
		}
		dec.UpdatedHead = merged
		return finish(DecisionNeedsCI, "")
	default:
		return Decision{}, fmt.Errorf("git merge-base --is-ancestor %s %s: status %d: %.4096s", baseSHA, in.Head, rc, strings.TrimSpace(errb))
	}
	// the checks of the pinned head must all be green
	chk, err := d.checks.Checks(ctx, ref)
	if err != nil {
		return Decision{}, err
	}
	if chk.Head != in.Head {
		return finish(DecisionRefused, reasonHeadMoved)
	}
	switch chk.State {
	case tracking.CheckSuccess:
	case tracking.CheckFailure:
		return finish(DecisionRefused, reasonChecksFailed)
	case tracking.CheckPending:
		return finish(DecisionRefused, reasonChecksPending)
	default:
		return finish(DecisionRefused, reasonChecksNone)
	}
	// the queue's order: an earlier pending pull request whose files overlap holds this one
	earlier, err := d.earlierOverlaps(ctx, ref.Number, baseSHA)
	if err != nil {
		return Decision{}, err
	}
	if len(earlier) > 0 {
		dec.Earlier = earlier
		return finish(DecisionHoldOrder, "")
	}
	// right before the write: the pull request, the remote and the checks again
	if reason, err := d.recheck(ctx, pr, baseSHA); err != nil {
		return Decision{}, err
	} else if reason != "" {
		return finish(DecisionRefused, reason)
	}
	dec.State = decisionReady
	if err := writeJSONRecordFile(record, dec); err != nil {
		return Decision{}, err
	}
	// the decision is announced on the pull request once, under a header that names this head and base tip
	header := fmt.Sprintf("[crw-pr-decide · %d · %s · %s]", ref.Number, in.Head, baseSHA)
	has, err := src.HasComment(ctx, ref, header)
	if err != nil {
		return Decision{}, err
	}
	if !has {
		text := fmt.Sprintf("state: %s\nhead: %s\nbase: %s\nbase_head: %s\nrelease_impact: %s\n\nThe parent's checks passed for this head and base; the queue entry is still being written.", decisionReady, in.Head, cfg.Branch, baseSHA, in.ReleaseImpact)
		if err := src.Comment(ctx, ref, header, text); err != nil {
			return Decision{}, err
		}
	}
	// enqueue through the source pinned to the expectations: it re-reads and re-checks inside EnqueuePR
	rr := *runner
	rr.Cfg = cfg
	rr.PullRequests = d.pinned(pr, baseSHA)
	entry, err := rr.EnqueuePR(ctx, ref.String())
	if err != nil {
		return Decision{}, err
	}
	if entry.Head != in.Head {
		return Decision{}, fmt.Errorf("the queue enqueued %s, not the pinned head %s", entry.Head, in.Head)
	}
	dec.State, dec.Enqueued = DecisionEnqueued, &entry
	if err := writeJSONRecordFile(record, dec); err != nil {
		return Decision{}, err
	}
	return dec, nil
}

// checkDecideBranch accepts the short name of a branch of the repository (as the adapter's checkBranch).
func checkDecideBranch(what, name string) error {
	if !decideBranch.MatchString(name) || strings.Contains(name, "..") || strings.Contains(name, "//") || strings.HasSuffix(name, "/") ||
		strings.HasSuffix(name, ".lock") || strings.HasSuffix(name, ".") || strings.HasPrefix(name, "refs/") {
		return fmt.Errorf("the %s %q is not the short name of a branch of the repository", what, name)
	}
	return nil
}

// decideLock takes the integration lock the passes hold, the same file and protocol as internal/integrator's acquireLock:
// O_NOFOLLOW, LOCK_EX|LOCK_NB, the file never unlinked.
func decideLock(path string) (func(), error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0o664)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, fmt.Errorf("%w: %s", integrator.ErrLockHeld, path)
		}
		return nil, err
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, nil
}

// writeJSONRecordFile writes v to path atomically (a temporary file in the directory, mode 0600, then rename), the
// directory made 0755; an existing symbolic link at the path is refused.
func writeJSONRecordFile(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	if fi, err := os.Lstat(path); err == nil && fi.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%s is a symbolic link", path)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".record-*")
	if err != nil {
		return err
	}
	if _, werr := tmp.Write(append(data, '\n')); werr != nil {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
		return werr
	}
	if cerr := tmp.Close(); cerr != nil {
		_ = os.Remove(tmp.Name())
		return cerr
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		_ = os.Remove(tmp.Name())
		return err
	}
	return nil
}

// recheckSnapshot re-reads the pull request and the remote's advertised tips and requires them to be still exactly the ones
// the decision was made over: the refusal reason of a change, "" when nothing moved. The pull request's head ref must be the
// same ref want named too.
func (d *decider) recheckSnapshot(ctx context.Context, want tracking.PullRequest, baseSHA string) (string, error) {
	pr, err := d.src.PullRequest(ctx, d.ref)
	if err != nil {
		return "", err
	}
	switch {
	case pr.HeadSHA != want.HeadSHA || pr.HeadRef != want.HeadRef:
		return reasonHeadMoved, nil
	case pr.Merged || pr.State != tracking.StateOpen || pr.Draft:
		return reasonPRNotOpen, nil
	case pr.BaseRef != want.BaseRef:
		return reasonWrongBase, nil
	case !strings.EqualFold(pr.HeadRepo, d.cfg.PullRequestRepo):
		return reasonWrongRepository, nil
	}
	adv, err := d.advertise(ctx, d.cfg.Branch, want.HeadRef)
	if err != nil {
		return "", err
	}
	if adv["refs/heads/"+want.HeadRef] != want.HeadSHA || adv["refs/heads/"+d.cfg.Branch] != baseSHA {
		return reasonHeadMoved, nil
	}
	return "", nil
}

// recheck is the snapshot revalidation plus the head's checks, right before the enqueue.
func (d *decider) recheck(ctx context.Context, want tracking.PullRequest, baseSHA string) (string, error) {
	reason, err := d.recheckSnapshot(ctx, want, baseSHA)
	if err != nil || reason != "" {
		return reason, err
	}
	chk, err := d.checks.Checks(ctx, d.ref)
	if err != nil {
		return "", err
	}
	if chk.Head != d.in.Head {
		return reasonHeadMoved, nil
	}
	switch chk.State {
	case tracking.CheckSuccess:
	case tracking.CheckFailure:
		return reasonChecksFailed, nil
	case tracking.CheckPending:
		return reasonChecksPending, nil
	default:
		return reasonChecksNone, nil
	}
	return "", nil
}

// updateBase merges the remote base tip into the head in a private detached worktree (no-ff) and pushes the merge to the
// pull request's head branch through the compare-and-swap pre-push hook: the remote must still advertise the pinned head
// when the push runs. It answers the pushed commit; moved is true when the pull request or the remote changed meanwhile
// (the caller refuses it) and an error is a real failure — a conflict, a failed push or a refused CAS.
func (d *decider) updateBase(ctx context.Context, pr tracking.PullRequest, baseSHA string) (merged string, moved bool, err error) {
	c := d.cfg
	if err := os.MkdirAll(c.WorkRoot, 0o775); err != nil {
		return "", false, err
	}
	wt, err := os.MkdirTemp(c.WorkRoot, "decide-wt-")
	if err != nil {
		return "", false, err
	}
	hooks, err := os.MkdirTemp(c.WorkRoot, "decide-hooks-")
	if err != nil {
		_ = os.RemoveAll(wt)
		return "", false, err
	}
	defer func() {
		cleanup := context.WithoutCancel(ctx)
		_, _, _, _ = d.gitErr(cleanup, c.Checkout, "worktree", "remove", "--force", wt)
		_ = os.RemoveAll(wt)
		_ = os.RemoveAll(hooks)
	}()
	if _, err := d.gitOK(ctx, c.Checkout, "worktree", "add", "--detach", wt, d.in.Head); err != nil {
		return "", false, err
	}
	mergeEnv := gitprobe.Sanitize(nil, "GIT_TERMINAL_PROMPT=0", "GIT_EDITOR=true", "GIT_MERGE_AUTOEDIT=no",
		"GIT_AUTHOR_NAME=crw", "GIT_AUTHOR_EMAIL=crw@localhost", "GIT_COMMITTER_NAME=crw", "GIT_COMMITTER_EMAIL=crw@localhost")
	if d.env != nil {
		mergeEnv = append(append([]string(nil), d.env...), "GIT_AUTHOR_NAME=crw", "GIT_AUTHOR_EMAIL=crw@localhost",
			"GIT_COMMITTER_NAME=crw", "GIT_COMMITTER_EMAIL=crw@localhost")
	}
	var mout bytes.Buffer
	rc, err := d.exec(ctx, integrator.Cmd{Argv: []string{c.Git, "-C", wt, "merge", "--no-ff", "--no-edit", baseSHA}, Env: mergeEnv, Stdout: &mout, Stderr: &mout})
	if err != nil {
		return "", false, err
	}
	if rc != 0 {
		return "", false, fmt.Errorf("the base merge of %s into %s stopped (status %d)", baseSHA, d.in.Head, rc)
	}
	merged, err = d.gitOK(ctx, wt, "rev-parse", "HEAD")
	if err != nil {
		return "", false, err
	}
	parents, err := d.gitOK(ctx, wt, "rev-list", "--parents", "-n", "1", "HEAD")
	if err != nil {
		return "", false, err
	}
	if fields := strings.Fields(parents); len(fields) != 3 || fields[0] != merged || fields[1] != d.in.Head || fields[2] != baseSHA {
		return "", false, fmt.Errorf("the merge %s does not have head %s and base %s as its parents (%q)", merged, d.in.Head, baseSHA, parents)
	}
	// the pull request and the remote must still be exactly what the decision was made over (the checks of the old head may
	// be pending or red: that is what the base update is for, so they are not re-read here)
	if reason, err := d.recheckSnapshot(ctx, pr, baseSHA); err != nil {
		return "", false, err
	} else if reason != "" {
		return "", true, nil
	}
	// the push is a compare-and-swap and keeps the repository's own policy: a private pre-push hook asserts the remote still
	// advertises the pinned head on the one expected update, then executes the repository's configured pre-push hook with the
	// same arguments, working directory and update lines — the repository's scans (POLICY's push-time secret and large-file
	// checks) run exactly as an ordinary push would run them. The original hook is resolved before the command-scoped
	// core.hooksPath override, so the default hooks dir and relative or absolute core.hooksPath values all resolve.
	originalHook, err := d.gitOK(ctx, c.Checkout, "rev-parse", "--path-format=absolute", "--git-path", "hooks/pre-push")
	if err != nil {
		return "", false, err
	}
	if originalHook == "" || !filepath.IsAbs(originalHook) {
		return "", false, fmt.Errorf("the pre-push hook path %q is not absolute", originalHook)
	}
	quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'" }
	input := filepath.Join(hooks, "pre-push-input")
	hook := filepath.Join(hooks, "pre-push")
	script := "#!/bin/sh\n" +
		"input=" + quote(input) + "\n" +
		"original=" + quote(originalHook) + "\n" +
		"cat > \"$input\" || exit 1\n" +
		"n=0\n" +
		"while read -r lref lsha rref rsha; do\n" +
		"  n=$((n + 1))\n" +
		"  [ \"$n\" = 1 ] || exit 1\n" +
		"  [ \"$lsha\" = '" + merged + "' ] || exit 1\n" +
		"  [ \"$rref\" = 'refs/heads/" + pr.HeadRef + "' ] || exit 1\n" +
		"  [ \"$rsha\" = '" + d.in.Head + "' ] || exit 1\n" +
		"done < \"$input\"\n" +
		"[ \"$n\" = 1 ] || exit 1\n" +
		"if [ -x \"$original\" ]; then\n" +
		"  exec \"$original\" \"$@\" < \"$input\"\n" +
		"fi\n" +
		"exit 0\n"
	if err := os.WriteFile(hook, []byte(script), 0o700); err != nil {
		return "", false, err
	}
	if _, err := d.gitOK(ctx, c.Checkout, "-c", "core.hooksPath="+hooks, "push", "--porcelain", "--", c.Remote, merged+":refs/heads/"+pr.HeadRef); err != nil {
		return "", false, fmt.Errorf("the base-update push was refused: %w", err)
	}
	return merged, false, nil
}

// earlierOverlaps lists the pending pull request entries with a smaller number whose changed file names overlap this one's:
// the file sets come only from `git diff --name-only --no-renames -z base...head`, never from content.
func (d *decider) earlierOverlaps(ctx context.Context, number int, baseSHA string) ([]string, error) {
	mine, err := d.diffNames(ctx, baseSHA, d.in.Head)
	if err != nil {
		return nil, err
	}
	des, err := os.ReadDir(filepath.Join(d.cfg.QueueDir, "pending"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var earlier []string
	for _, de := range des {
		m := pendingPR.FindStringSubmatch(de.Name())
		if m == nil {
			continue
		}
		if !de.Type().IsRegular() {
			return nil, fmt.Errorf("pending/%s is not a regular file", de.Name())
		}
		n, _ := strconv.Atoi(m[1])
		if n >= number {
			continue
		}
		data, err := os.ReadFile(filepath.Join(d.cfg.QueueDir, "pending", de.Name()))
		if err != nil {
			return nil, err
		}
		queued := strings.TrimSpace(string(data))
		if !decideSHA.MatchString(queued) {
			return nil, fmt.Errorf("pending/%s does not hold a full head SHA", de.Name())
		}
		other, err := d.src.PullRequest(ctx, tracking.Ref{Repo: d.cfg.PullRequestRepo, Number: n})
		if err != nil {
			return nil, err
		}
		if other.Merged || other.State != tracking.StateOpen {
			continue
		}
		if other.HeadSHA != queued || other.BaseRef != d.cfg.Branch {
			return nil, fmt.Errorf("pending/%s queues %s but the pull request is at %s toward %s", de.Name(), queued, other.HeadSHA, other.BaseRef)
		}
		if err := checkDecideBranch("head", other.HeadRef); err != nil {
			return nil, err
		}
		if err := d.fetchRefs(ctx, other.HeadRef); err != nil {
			return nil, err
		}
		theirs, err := d.diffNames(ctx, baseSHA, queued)
		if err != nil {
			return nil, err
		}
		for name := range theirs {
			if mine[name] {
				earlier = append(earlier, other.Ref.String())
				break
			}
		}
	}
	return earlier, nil
}

// diffNames is the set of file names that changed on head relative to base: `git diff --name-only --no-renames -z base...head`,
// the raw output split on NUL (rename paths count on both sides).
func (d *decider) diffNames(ctx context.Context, base, head string) (map[string]bool, error) {
	out, errb, rc, err := d.gitErr(ctx, d.cfg.Checkout, "diff", "--name-only", "--no-renames", "-z", base+"..."+head)
	if err != nil {
		return nil, err
	}
	if rc != 0 {
		return nil, fmt.Errorf("git diff --name-only %s...%s: status %d: %.4096s", base, head, rc, strings.TrimSpace(errb))
	}
	set := map[string]bool{}
	for _, name := range strings.Split(out, "\x00") {
		if name != "" {
			set[name] = true
		}
	}
	return set, nil
}

// pinnedSource wraps the pull request source the enqueue call uses: its PullRequest re-reads the live pull request and
// requires the pinned head, base, repository and open state, and the remote's advertised tips unchanged.
type pinnedSource struct {
	integrator.PullRequestSource
	d       *decider
	head    string
	base    string
	baseSHA string
	headRef string
}

func (p pinnedSource) PullRequest(ctx context.Context, ref tracking.Ref) (tracking.PullRequest, error) {
	pr, err := p.PullRequestSource.PullRequest(ctx, ref)
	if err != nil {
		return pr, err
	}
	if pr.HeadSHA != p.head || pr.HeadRef != p.headRef || pr.BaseRef != p.base || !strings.EqualFold(pr.HeadRepo, p.d.cfg.PullRequestRepo) ||
		pr.State != tracking.StateOpen || pr.Merged || pr.Draft {
		return pr, fmt.Errorf("%s changed during the decision (head %s, base %s, repo %s, state %s)", ref, pr.HeadSHA, pr.BaseRef, pr.HeadRepo, pr.State)
	}
	adv, err := p.d.advertise(ctx, p.base, p.headRef)
	if err != nil {
		return pr, err
	}
	if adv["refs/heads/"+p.headRef] != p.head || adv["refs/heads/"+p.base] != p.baseSHA {
		return pr, fmt.Errorf("the remote moved during the decision of %s", ref)
	}
	return pr, nil
}

// pinned is the pull request source EnqueuePR sees: it refuses any pull request that is not still the decided one.
func (d *decider) pinned(pr tracking.PullRequest, baseSHA string) integrator.PullRequestSource {
	return pinnedSource{PullRequestSource: d.src, d: d, head: d.in.Head, base: d.cfg.Branch, baseSHA: baseSHA, headRef: pr.HeadRef}
}
