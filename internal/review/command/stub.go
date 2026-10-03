package command

// Stub for the red run: the shapes the tests need, none of the behavior. Replaced by the implementation.

import (
	"context"
	"errors"
	"io"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/review"
	"github.com/thisisjun786/codex-relay-workflow/internal/review/pipeline"
)

const (
	OutcomeReviewed        = "reviewed"
	OutcomeAlreadyReviewed = "already_reviewed"
	OutcomeDailyCap        = "daily_cap_reached"
)

type Summary struct {
	Outcome, Issue, Base, Head, PatchID, Artifact, SHA256, Status, Reason string
	DailyCap, RunsToday                                                   int
	ReviewedHead                                                          string
	ArtifactPresent                                                       *bool
	*Counts
}

type Counts struct {
	Reviewers                review.ReviewerCounts
	Findings, Dropped, Calls int
}

type env struct {
	runner pipeline.Runner
	now    func() time.Time
}

type Config struct {
	Out, StateDir, Model, Binary, LockPath string
	DailyCap                               int
	LockWait                               time.Duration
}

func Run(context.Context, []string, io.Writer, io.Writer) int { return 99 }

func run(context.Context, []string, io.Writer, io.Writer, env) int { return 99 }

func parseConfig([]string, io.Writer, io.Writer) (Config, int) { return Config{}, -1 }

type record struct {
	Time, Event, PatchID, Base, Head, Issue, Reason, Artifact, SHA256, Status string
}

type ledger struct {
	dir string
	now func() time.Time
}

func (l *ledger) path() string                                        { return l.dir + "/ledger.jsonl" }
func (l *ledger) read() ([]record, error)                             { return nil, nil }
func (l *ledger) append(record) error                                 { return nil }
func (l *ledger) lock(context.Context, time.Duration) (func(), error) { return func() {}, nil }
func runsOn([]record, string) int                                     { return 0 }

type gitHead struct {
	ctx        context.Context
	repo, head string
}

func (g *gitHead) Lines(string) (int, error)                    { return 0, errors.New("stub") }
func (g *gitHead) ReadLines(string, int, int) ([]string, error) { return nil, errors.New("stub") }
