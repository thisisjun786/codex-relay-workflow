package command

import (
	"slices"
	"strings"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/review"
	"github.com/thisisjun786/codex-relay-workflow/internal/review/agy"
)

// retryReasons are the reasons of a review call that could not run for the account or the configuration (out of quota, not logged in, an unknown model, agy that cannot be started) or because the runner
// itself failed (a crash, a runner error): the same patch-id may be tried once more on a later UTC day. Every other outcome (a content filter, a denied action, the time limit) is a result and closes the
// patch-id on its first run. A lock wait is the exception below: agy was never started, so it spends nothing at all.
var retryReasons = []agy.Reason{agy.ReasonQuota, agy.ReasonAuthentication, agy.ReasonUnknownModel, agy.ReasonNotStarted, agy.ReasonCrash, agy.Reason(reasonRunnerError)}

// retryableUnavailable reports whether a is an unavailable review whose every review call failed as unavailable with one of retryReasons (one call of another kind decides against); reasons lists them,
// sorted and joined by commas. Calls of the auxiliary stages do not count.
func retryableUnavailable(a *review.Artifact) (reasons string, ok bool) {
	if a.Status != review.StatusUnavailable {
		return "", false
	}
	var seen []string
	for _, c := range a.Calls {
		if c.Stage != "review" {
			continue
		}
		if c.Class != string(agy.ClassUnavailable) || !slices.Contains(retryReasons, agy.Reason(c.Reason)) {
			return "", false
		}
		if !slices.Contains(seen, c.Reason) {
			seen = append(seen, c.Reason)
		}
	}
	slices.Sort(seen)
	return strings.Join(seen, ","), len(seen) > 0
}

// reasonRunnerError is the pipeline's reason for a call that failed at the runner rather than at agy (internal/review/pipeline): the same kind of runner-side failure as a crash.
const reasonRunnerError = "runner_error"

// agyNeverStarted are the reasons of a review call where agy was not started at all: the host-wide lock was not free within the wait, or agy could not be started. agyReported are the reasons of a call
// where agy ran and reported the failure itself. A crash or a runner error is in neither: whether agy was called cannot be told from the record.
var (
	agyNeverStarted = []agy.Reason{agy.ReasonLockWaitExpired, agy.ReasonNotStarted}
	agyReported     = []agy.Reason{agy.ReasonQuota, agy.ReasonAuthentication, agy.ReasonUnknownModel}
)

// lockWaitExpired reports whether a is an unavailable review where agy was never started because the host-wide lock was not free within the wait: every review call failed that way. Such a run is not a
// review of the patch: nothing was reviewed, so it closes nothing, counts toward nothing and may be tried again at the next chance, the same day included.
func lockWaitExpired(a *review.Artifact) bool {
	if a.Status != review.StatusUnavailable {
		return false
	}
	seen := false
	for _, c := range a.Calls {
		if c.Stage != "review" {
			continue
		}
		if c.Class != string(agy.ClassUnavailable) || agy.Reason(c.Reason) != agy.ReasonLockWaitExpired {
			return false
		}
		seen = true
	}
	return seen
}

// agyCalledOf reports whether agy was actually called for this review, when that can be told: false when every review call is one where agy never started (a lock wait, agy that could not be started),
// true when agy ran and reported the failure itself, and nil when it cannot be told (a crash, a runner error, or a review that did not end as unavailable).
func agyCalledOf(a *review.Artifact) *bool {
	if a.Status != review.StatusUnavailable {
		return nil
	}
	var called *bool
	for _, c := range a.Calls {
		if c.Stage != "review" {
			continue
		}
		switch r := agy.Reason(c.Reason); {
		case slices.Contains(agyNeverStarted, r):
			if called == nil {
				called = new(bool)
			}
		case slices.Contains(agyReported, r):
			ran := true
			called = &ran
		default:
			return nil
		}
	}
	return called
}

// nextDay is the UTC day after day (2006-01-02).
func nextDay(day string) string {
	t, err := time.Parse(time.DateOnly, day)
	if err != nil {
		return day
	}
	return t.AddDate(0, 0, 1).Format(time.DateOnly)
}
