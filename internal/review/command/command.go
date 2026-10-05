package command

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/crwdir"
	"github.com/thisisjun786/codex-relay-workflow/internal/review"
	"github.com/thisisjun786/codex-relay-workflow/internal/review/agy"
	"github.com/thisisjun786/codex-relay-workflow/internal/review/bundle"
	"github.com/thisisjun786/codex-relay-workflow/internal/review/pipeline"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/buildinfo"
)

// The outcomes of a command line that parsed.
const (
	OutcomeReviewed        = "reviewed"
	OutcomeAlreadyReviewed = "already_reviewed"
	OutcomeDailyCap        = "daily_cap_reached"
	OutcomeRetryDeferred   = "retry_deferred"
	OutcomeRecorded        = "recorded" // --post-only: the recorded result of the patch, nothing ran
)

// Exit statuses besides usageExit.
const (
	exitError       = 1
	exitRefused     = 3 // the run rules stopped the review: the daily cap, or another review holding the run lock
	exitInterrupted = 130
)

// Summary is the one JSON object the command prints on stdout when a review ran and when the run rules stopped it.
type Summary struct {
	Outcome   string `json:"outcome"`
	Issue     string `json:"issue"`
	Base      string `json:"base"`
	Head      string `json:"head"`
	PatchID   string `json:"patchId"`
	Artifact  string `json:"artifact,omitempty"` // absolute; for already_reviewed, where the earlier review recorded it
	SHA256    string `json:"sha256,omitempty"`
	Status    string `json:"status,omitempty"` // of the artifact
	Reason    string `json:"reason,omitempty"` // the artifact's, or why the daily cap stopped the review
	DailyCap  int    `json:"dailyCap,omitempty"`
	RunsToday int    `json:"runsToday,omitempty"`
	// already_reviewed only: the head of the earlier review, and whether its artifact file is there now.
	ReviewedHead    string   `json:"reviewedHead,omitempty"`
	ArtifactPresent *bool    `json:"artifactPresent,omitempty"`
	Restored        []string `json:"restored,omitempty"`       // the files of the recorded artifact that were missing and were written from the kept copy of the result
	RetryNotBefore  string   `json:"retryNotBefore,omitempty"` // the first UTC day (2006-01-02) on which the one more attempt of a review that could not run is allowed
	Comment         *Posted  `json:"summaryComment,omitempty"`
	*Counts                  // a review that ran
}

// Posted says what --post-summary did to the pull request: created, updated or unchanged the one summary comment.
type Posted struct {
	Action string `json:"action"`
	URL    string `json:"url,omitempty"`
	Reason string `json:"reason,omitempty"` // set when the comment shows a newer result of the patch than the one this run holds
}

type Counts struct {
	Reviewers review.ReviewerCounts `json:"reviewers"`
	Findings  int                   `json:"findings"`
	Dropped   int                   `json:"dropped"`
	Calls     int                   `json:"calls"`
}

type env struct {
	runner pipeline.Runner
	now    func() time.Time
	forge  func(Config) forge // nil: the gh CLI of the checkout
}

// Run is crw review.
func Run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	// agy runs in a process group of its own and holds no lock descriptor, so a SIGTERM or SIGHUP that ended this process without cancelling the run would leave
	// agy running with both locks released. Cancelling the context makes the runner kill agy's group first.
	ctx, stop := signal.NotifyContext(ctx, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()
	return run(ctx, args, stdout, stderr, env{runner: agy.Run, now: time.Now})
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer, e env) int {
	cfg, code := parseConfig(args, stdout, stderr)
	if code >= 0 {
		return code
	}
	sum, err := execute(ctx, cfg, e)
	switch {
	case err != nil && ctx.Err() != nil:
		fmt.Fprintln(stderr, "crw review: interrupted")
		return exitInterrupted
	case errors.Is(err, errBusy):
		fmt.Fprintf(stderr, "crw review: busy: another review holds the run lock in %s; try again later\n", cfg.StateDir)
		return exitRefused
	case err != nil:
		fmt.Fprintf(stderr, "crw review: error: %v\n", err)
		return exitError
	}
	var postErr error
	if cfg.PostSummary && sum.Artifact != "" { // an artifact is named by a review that ran, an already reviewed answer and a deferred retry; the daily cap names none
		postErr = postSummary(ctx, cfg, e.forgeFor(cfg), sum, stderr)
	}
	out, _ := json.Marshal(sum)
	fmt.Fprintf(stdout, "%s\n", out)
	switch {
	case postErr != nil && ctx.Err() != nil:
		fmt.Fprintln(stderr, "crw review: interrupted")
		return exitInterrupted
	case errors.Is(postErr, errBusy):
		fmt.Fprintf(stderr, "crw review: busy: another post holds the post lock in %s; the summary comment was not posted, try again later\n", cfg.StateDir)
		return exitRefused
	case postErr != nil:
		fmt.Fprintf(stderr, "crw review: error: the summary comment was not posted: %v\n", postErr)
		return exitError
	case sum.Outcome == OutcomeDailyCap || sum.Outcome == OutcomeRetryDeferred:
		return exitRefused
	}
	return 0
}

// execute is one review under the run rules; the package documentation gives the order of its steps.
func execute(ctx context.Context, cfg Config, e env) (*Summary, error) {
	b, err := bundle.Build(ctx, cfg.Repo, cfg.Base, cfg.Head, bundle.Options{})
	if err != nil {
		return nil, err
	}
	m := b.Metadata
	if m.FileCount == 0 || m.PatchID == "" {
		return nil, errors.New("base...head has no changes; there is nothing to review")
	}
	sum := &Summary{Outcome: OutcomeReviewed, Issue: cfg.Issue, Base: m.Base, Head: m.Head, PatchID: m.PatchID}
	l := &ledger{dir: cfg.StateDir, now: e.now}
	entry := func(event string) record {
		return record{Event: event, PatchID: m.PatchID, Base: m.Base, Head: m.Head, Issue: cfg.Issue}
	}
	// recorded puts the artifact an earlier attempt recorded into sum, after writing the files of it that are missing from the copy kept with the record (ledger.restore; locked says that the caller holds the run lock).
	recorded := func(r record, earlier *record, locked bool) error {
		restored, err := l.restore(ctx, r, earlier, cfg.Out, locked)
		if err != nil {
			return err
		}
		info, statErr := os.Stat(r.Artifact)
		present := statErr == nil && info.Mode().IsRegular()
		sum.Artifact, sum.SHA256, sum.Status, sum.ReviewedHead, sum.ArtifactPresent, sum.Restored = r.Artifact, r.SHA256, r.Status, r.Head, &present, restored
		return nil
	}
	// already reads the ledger and, if this patch is closed, turns sum into the answer that says so. Without the run lock only a finished record closes it (an attempt that runs now may still finish).
	already := func(locked bool) (recs []record, done bool, err error) {
		if recs, err = l.read(); err != nil {
			return nil, false, err
		}
		st := standingOf(recs, m.PatchID)
		if r, closed := st.closer(); closed && (locked || st.finished != nil) {
			sum.Outcome = OutcomeAlreadyReviewed
			return recs, true, recorded(r, st.unavailable, locked)
		}
		return recs, false, nil
	}
	if cfg.PostOnly { // the recorded result of the patch, open or closed: no run lock, no record of any kind, so the one more attempt and the daily cap are untouched whatever the day
		recs, err := l.read()
		if err != nil {
			return nil, err
		}
		newest := newestResult(recs, m.PatchID)
		if newest == nil {
			return nil, errors.New("this patch has no recorded result to post; --post-only never runs a review")
		}
		sum.Outcome = OutcomeRecorded
		return sum, recorded(*newest, standingOf(recs, m.PatchID).unavailable, false)
	}
	if _, done, err := already(false); done || err != nil { // without the lock: a finished record never goes away, and waiting behind another review would only delay this answer
		return sum, err
	}
	unlock, err := l.lock(ctx, cfg.LockWait)
	if err != nil {
		return nil, err
	}
	defer unlock()
	recs, done, err := already(true)
	if done || err != nil {
		return sum, err
	}
	st := standingOf(recs, m.PatchID)
	day := e.now().UTC().Format(time.DateOnly)
	if st.open() { // the one more attempt of an unavailable review: on a later UTC day than the last attempt's, never in the same window
		if last := dayOf(st.unavailable.Time); day <= last {
			sum.Outcome, sum.RetryNotBefore = OutcomeRetryDeferred, nextDay(last)
			sum.Reason = fmt.Sprintf("the review of this patch could not run on %s (UTC): %s; one more attempt is allowed from %s (UTC)", last, st.unavailable.Reason, sum.RetryNotBefore)
			refused := entry("refused")
			refused.Reason = sum.Reason
			return sum, errors.Join(recorded(*st.unavailable, nil, true), l.append(refused))
		}
	}
	if n := runsOn(recs, day); n >= cfg.DailyCap {
		sum.Outcome, sum.DailyCap, sum.RunsToday = OutcomeDailyCap, cfg.DailyCap, n
		sum.Reason = fmt.Sprintf("daily cap reached: %d of %d reviews started on %s (UTC)", n, cfg.DailyCap, day)
		refused := entry("refused")
		refused.Reason = sum.Reason
		return sum, l.append(refused)
	}
	artifact := filepath.Join(cfg.Out, m.Head+".json")
	if _, err = os.Lstat(artifact); err == nil && !replaces(st.unavailable, artifact) { // the file name is the head's, the ledger key the patch's: the same head reviewed against another base lands here
		return nil, fmt.Errorf("%s already exists but is no review of this patch (the same head, reviewed against another base?); use another --out", artifact)
	}
	if err = os.MkdirAll(cfg.Out, 0o755); err != nil {
		return nil, err
	}
	if err = l.append(entry("started")); err != nil {
		return nil, err
	}
	fail := func(err error) (*Summary, error) {
		failed := entry("failed")
		failed.Reason = err.Error()
		if ctx.Err() != nil {
			failed.Reason = "interrupted"
		}
		return nil, errors.Join(err, l.append(failed))
	}
	a, err := pipeline.Run(ctx, b, e.runner, pipeline.Config{
		Agy:  agy.Config{Binary: cfg.Binary, Model: cfg.Model, LockPath: cfg.LockPath, LockWait: cfg.LockWait, TimeLimitFloor: cfg.TimeLimitFloor, TimeLimitCeiling: cfg.TimeLimitCeiling},
		Head: &gitHead{ctx: ctx, repo: cfg.Repo, head: m.Head}, ToolVersion: buildinfo.Version})
	if err != nil {
		return fail(err)
	}
	data, err := a.Marshal(m.Head)
	if err != nil {
		return fail(err)
	}
	digest := sha256.Sum256(data)
	result := entry("finished")
	result.Time = e.now().UTC().Format(time.RFC3339) // the day the attempt ended, maybe after the one it began on: the ledger line and retryNotBefore must agree on it
	result.Artifact, result.SHA256, result.Status = artifact, hex.EncodeToString(digest[:]), string(a.Status)
	if reasons, ok := accountUnavailable(a); ok {
		result.Reason = reasons
		if st.unavailable == nil { // the first attempt that could not run: the patch stays open for one more, on a later day
			result.Event, sum.RetryNotBefore = "unavailable", nextDay(dayOf(result.Time))
		}
	}
	// The result is kept before the review is recorded, so that a record always has its copy; if it cannot be kept, the review is recorded and published all the same (a second model call is what this order exists to avoid) and the failure is reported at the end.
	keepErr := l.keep(result.SHA256, data)
	// The review is recorded before its files are written, so that nothing after this point can let the patch be reviewed again.
	if err = l.append(result); err != nil {
		return nil, err
	}
	for _, f := range []struct {
		path string
		data []byte
	}{{artifact, data}, {artifact + ".sha256", []byte(result.SHA256 + "  " + m.Head + ".json\n")}} {
		if err = crwdir.Publish(f.path, f.data); err != nil {
			return nil, fmt.Errorf("the review is recorded as finished but %s could not be written: %w", f.path, err)
		}
	}
	sum.Artifact, sum.SHA256, sum.Status, sum.Reason = artifact, result.SHA256, result.Status, a.Reason
	sum.Counts = &Counts{Reviewers: a.Reviewers, Findings: len(a.Findings), Dropped: len(a.Dropped), Calls: len(a.Calls)}
	if keepErr != nil {
		return nil, fmt.Errorf("the review is recorded and its files are written, but its result could not be kept in the state directory, so a failed write of the files could not have been repaired without a model call: %w", keepErr)
	}
	return sum, nil
}

// replaces reports whether the file at path is the artifact the unavailable attempt r recorded (the same path with the bytes it recorded), which the one more attempt may replace.
func replaces(r *record, path string) bool {
	if r == nil || r.Artifact != path {
		return false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:]) == r.SHA256
}
