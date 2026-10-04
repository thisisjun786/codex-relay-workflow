package command

import (
	"slices"
	"strings"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/review"
	"github.com/thisisjun786/codex-relay-workflow/internal/review/agy"
)

// accountReasons are the reasons of a call that could not run at all because of the account or the configuration (out of quota, not logged in, an unknown model, agy that cannot be started): the same patch-id may be
// tried once more on a later UTC day. Every other outcome (a content filter, a denied action, the time limit, a crash, a wait for agy's lock) is a result and closes the patch-id on its first run.
var accountReasons = []agy.Reason{agy.ReasonQuota, agy.ReasonAuthentication, agy.ReasonUnknownModel, agy.ReasonNotStarted}

// accountUnavailable reports whether a is an unavailable review whose every review call failed as unavailable with one of accountReasons (one call of another kind decides against); reasons lists them,
// sorted and joined by commas. Calls of the auxiliary stages do not count.
func accountUnavailable(a *review.Artifact) (reasons string, ok bool) {
	if a.Status != review.StatusUnavailable {
		return "", false
	}
	var seen []string
	for _, c := range a.Calls {
		if c.Stage != "review" {
			continue
		}
		if c.Class != string(agy.ClassUnavailable) || !slices.Contains(accountReasons, agy.Reason(c.Reason)) {
			return "", false
		}
		if !slices.Contains(seen, c.Reason) {
			seen = append(seen, c.Reason)
		}
	}
	slices.Sort(seen)
	return strings.Join(seen, ","), len(seen) > 0
}

// nextDay is the UTC day after day (2006-01-02).
func nextDay(day string) string {
	t, err := time.Parse(time.DateOnly, day)
	if err != nil {
		return day
	}
	return t.AddDate(0, 0, 1).Format(time.DateOnly)
}
