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
	ReviewedHead    string `json:"reviewedHead,omitempty"`
	ArtifactPresent *bool  `json:"artifactPresent,omitempty"`
	*Counts                // a review that ran
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
}

// Run is crw review.
func Run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	// agy runs in a process group of its own and holds no lock descriptor, so a SIGTERM or SIGHUP that ended this process without cancelling the run would leave
	// agy running with both locks released. Cancelling the context makes the runner kill agy's group first.
	ctx, stop := signal.NotifyContext(ctx, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()
	return run(ctx, args, stdout, stderr, env{agy.Run, time.Now})
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
	out, _ := json.Marshal(sum)
	fmt.Fprintf(stdout, "%s\n", out)
	if sum.Outcome == OutcomeDailyCap {
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
	// already reads the ledger and, if this patch was reviewed, turns sum into the answer that says so.
	already := func() (recs []record, done bool, err error) {
		if recs, err = l.read(); err != nil {
			return nil, false, err
		}
		r, done := reviewed(recs, m.PatchID)
		if done {
			info, statErr := os.Stat(r.Artifact)
			present := statErr == nil && info.Mode().IsRegular()
			sum.Outcome, sum.Artifact, sum.SHA256, sum.Status, sum.ReviewedHead, sum.ArtifactPresent = OutcomeAlreadyReviewed, r.Artifact, r.SHA256, r.Status, r.Head, &present
		}
		return recs, done, nil
	}
	if _, done, err := already(); done || err != nil { // without the lock: a finished record never goes away, and waiting behind another review would only delay this answer
		return sum, err
	}
	unlock, err := l.lock(ctx, cfg.LockWait)
	if err != nil {
		return nil, err
	}
	defer unlock()
	recs, done, err := already()
	if done || err != nil {
		return sum, err
	}
	day := e.now().UTC().Format(time.DateOnly)
	if n := runsOn(recs, day); n >= cfg.DailyCap {
		sum.Outcome, sum.DailyCap, sum.RunsToday = OutcomeDailyCap, cfg.DailyCap, n
		sum.Reason = fmt.Sprintf("daily cap reached: %d of %d reviews started on %s (UTC)", n, cfg.DailyCap, day)
		refused := entry("refused")
		refused.Reason = sum.Reason
		return sum, l.append(refused)
	}
	artifact := filepath.Join(cfg.Out, m.Head+".json")
	if _, err = os.Lstat(artifact); err == nil { // the file name is the head's, the ledger key the patch's: the same head reviewed against another base lands here
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
	finished := entry("finished")
	finished.Artifact, finished.SHA256, finished.Status = artifact, hex.EncodeToString(digest[:]), string(a.Status)
	// The review is recorded before its files are written, so that nothing after this point can let the patch be reviewed again.
	if err = l.append(finished); err != nil {
		return nil, err
	}
	for _, f := range []struct {
		path string
		data []byte
	}{{artifact, data}, {artifact + ".sha256", []byte(finished.SHA256 + "  " + m.Head + ".json\n")}} {
		if err = crwdir.Publish(f.path, f.data); err != nil {
			return nil, fmt.Errorf("the review is recorded as finished but %s could not be written: %w", f.path, err)
		}
	}
	sum.Artifact, sum.SHA256, sum.Status, sum.Reason = artifact, finished.SHA256, finished.Status, a.Reason
	sum.Counts = &Counts{Reviewers: a.Reviewers, Findings: len(a.Findings), Dropped: len(a.Dropped), Calls: len(a.Calls)}
	return sum, nil
}
