package hook_test

import (
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/goalplan"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/hook"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
)

// CRW-1113 (verification round 2). The inbox lives under the workspace, so a link planted where the inbox or a session's inbox
// directory belongs must not let the observer create, read or delete anything outside it; and a plan write that failed only after
// the plan was published is a recorded verdict, not a lost one.

// reviewObsDirNames lists dir's names, sorted, for a before/after comparison.
func reviewObsDirNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	names := []string{}
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	sort.Strings(names)
	return names
}

func TestReviewObserverInboxNeverFollowsALinkOutOfTheWorkspace(t *testing.T) {
	for _, linked := range []string{"session directory", "inbox directory"} {
		t.Run(linked, func(t *testing.T) {
			e := reviewObsSeed(t, "rb", nil)
			launch := e.open(t)
			// The foreign directory holds an unrelated JSON file and one shaped like an inbox entry; neither may be read as an entry,
			// removed, or joined by a new file.
			foreign := t.TempDir()
			settings := filepath.Join(foreign, "settings.json")
			shaped := filepath.Join(foreign, strings.Repeat("ab", 16)+".json")
			for _, path := range []string{settings, shaped} {
				if err := os.WriteFile(path, []byte(`{"keep":"not an inbox entry"}`), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			target, inbox := foreign, filepath.Join(e.cwd, ".crw", "review-inbox")
			if linked == "session directory" {
				if err := os.MkdirAll(inbox, 0o700); err != nil {
					t.Fatal(err)
				}
				inbox = filepath.Join(inbox, e.session)
			} else {
				// The session's inbox directory sits inside the foreign directory the inbox links to.
				if err := os.Mkdir(filepath.Join(foreign, e.session), 0o700); err != nil {
					t.Fatal(err)
				}
				for _, path := range []string{settings, shaped} {
					if err := os.WriteFile(filepath.Join(foreign, e.session, filepath.Base(path)), []byte(`{"keep":"not an inbox entry"}`), 0o600); err != nil {
						t.Fatal(err)
					}
				}
			}
			if err := os.Symlink(target, inbox); err != nil {
				t.Fatal(err)
			}
			before := map[string][]string{foreign: reviewObsDirNames(t, foreign)}
			if linked == "inbox directory" {
				before[filepath.Join(foreign, e.session)] = reviewObsDirNames(t, filepath.Join(foreign, e.session))
			}
			e.stop(t, reviewObsType("explorer"), "other-1", "no sign-off")
			// A sign-off still counts once in memory (the inbox could not keep it), and is reported as an inbox failure.
			e.stop(t, reviewObsType("explorer"), "reviewer-1", reviewObsSignoff(launch, "PASS"))
			for dir, names := range before {
				if got := reviewObsDirNames(t, dir); strings.Join(got, ",") != strings.Join(names, ",") {
					t.Fatalf("the observer changed a directory outside the workspace through a link: %s %v, was %v", dir, got, names)
				}
			}
			if r := e.round(t); r.Status != goalplan.ReviewApproved {
				t.Fatalf("the sign-off is still judged in memory: %+v", r)
			}
			if !strings.Contains(e.ledger(t), "review_signoff_inbox_failed") {
				t.Fatalf("the refused inbox is reported: %q", e.ledger(t))
			}
		})
	}
}

// A file in the inbox that does not have the observer's own name shape is not the observer's to remove, even when it is unreadable.
func TestReviewObserverInboxRemovesOnlyItsOwnFileNames(t *testing.T) {
	e := reviewObsSeed(t, "rb", nil)
	e.open(t)
	dir := filepath.Join(e.cwd, ".crw", "review-inbox", e.session)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	foreign := filepath.Join(dir, "settings.json")
	if err := os.WriteFile(foreign, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	e.stop(t, reviewObsType("explorer"), "other-1", "nothing")
	if _, err := os.Stat(foreign); err != nil {
		t.Fatalf("a file the observer did not name was removed: %v", err)
	}
	if strings.Contains(e.ledger(t), "unreadable review inbox entry") {
		t.Fatalf("a file the observer did not name is not reported as one of its entries: %q", e.ledger(t))
	}
}

// failAfterPublish publishes the plan and then reports the failure a directory fsync after the rename reports.
func failAfterPublish(cwd string, plan *goalplan.Goalplan) error {
	if err := goalplan.WriteGoalplan(cwd, plan); err != nil {
		return err
	}
	return &state.PublishedError{Err: errors.New("injected directory fsync failure after the rename")}
}

// failFirstBeforePublish fails the first write before anything is published and lets every later write through, so a sign-off
// judged after the failed one could be written.
func failFirstBeforePublish() func(cwd string, plan *goalplan.Goalplan) error {
	failed := false
	return func(cwd string, plan *goalplan.Goalplan) error {
		if !failed {
			failed = true
			return errors.New("injected plan write failure before the rename")
		}
		return goalplan.WriteGoalplan(cwd, plan)
	}
}

// keepTwoSignoffs keeps reviewer-1's FAIL and then reviewer-2's PASS for the same launch behind a held goalplan lock.
func (e reviewObsEnv) keepTwoSignoffs(t *testing.T, launch string) {
	t.Helper()
	release := e.holdGoalplanLock(t)
	e.stop(t, reviewObsType("explorer"), "reviewer-1", reviewObsSignoff(launch, "FAIL"))
	e.stop(t, reviewObsType("explorer"), "reviewer-2", reviewObsSignoff(launch, "PASS"))
	release()
	if got := len(e.inboxFiles(t)); got != 2 {
		t.Fatalf("setup: two kept sign-offs: %d", got)
	}
}

// A write that published the plan and failed afterwards recorded the verdict: the drain goes on from the published plan, so a later
// child cannot overwrite the terminal verdict, and the entry is not kept to be applied twice.
func TestReviewObserverDrainKeepsAVerdictPublishedBeforeItsWriteFailed(t *testing.T) {
	e := reviewObsSeed(t, "rb", nil)
	launch := e.open(t)
	e.keepTwoSignoffs(t, launch)
	restore := hook.SetReviewObserverWriteGoalplan(failAfterPublish)
	e.stop(t, reviewObsType("explorer"), "other-1", "nothing")
	restore()
	r := e.round(t)
	if r.Lane.Verdict != goalplan.VerdictFail || r.Lane.ReviewerSession == nil || *r.Lane.ReviewerSession != "reviewer-1" {
		t.Fatalf("the published FAIL stands: %+v", r)
	}
	if got := e.inboxFiles(t); len(got) != 0 {
		t.Fatalf("a published verdict and the child refused after it are not kept: %v", got)
	}
	ledger := e.ledger(t)
	if !strings.Contains(ledger, "review_signoff_write_unsynced") || strings.Contains(ledger, "review_signoff_write_failed") {
		t.Fatalf("a published write is a durability warning, not a failed write: %q", ledger)
	}
}

// The A>B publication's in-lock drain answers the plan as published, with nothing left unapplied.
func TestReviewObserverInLockDrainAnswersThePublishedPlan(t *testing.T) {
	e := reviewObsSeed(t, "rb", nil)
	launch := e.open(t)
	release := e.holdGoalplanLock(t)
	e.stop(t, reviewObsType("explorer"), "reviewer-1", reviewObsSignoff(launch, "FAIL"))
	release()
	restore := hook.SetReviewObserverWriteGoalplan(failAfterPublish)
	defer restore()
	if err := state.WithSessionLock(e.cwd, e.session, func() error {
		_, err := goalplan.WithGoalplanWriteLock(e.cwd, e.slug, func(plan *goalplan.Goalplan) (string, error) {
			next, kept := hook.DrainReviewObserverInboxInLock(e.cwd, e.session, plan)
			if kept != 0 || next == nil || next.ReviewRounds[0].Lane.Verdict != goalplan.VerdictFail {
				t.Fatalf("the published FAIL is the plan judged, with nothing kept: kept=%d plan=%+v", kept, next)
			}
			return "", nil
		}, nil)
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

// A write that failed before publication recorded nothing: the entry stays, and no later child's sign-off is judged ahead of it in
// the same drain (its write would succeed), so the order of the sign-offs survives the failure and the next drain records the first.
func TestReviewObserverDrainKeepsAnUnpublishedVerdictAheadOfLaterSignoffs(t *testing.T) {
	e := reviewObsSeed(t, "rb", nil)
	launch := e.open(t)
	e.keepTwoSignoffs(t, launch)
	restore := hook.SetReviewObserverWriteGoalplan(failFirstBeforePublish())
	e.stop(t, reviewObsType("explorer"), "other-1", "nothing")
	restore()
	if r := e.round(t); r.Status != goalplan.ReviewInFlight || r.Lane.Verdict != "" {
		t.Fatalf("nothing was recorded: %+v", r)
	}
	if got := len(e.inboxFiles(t)); got != 2 {
		t.Fatalf("both sign-offs stay, in order: %d", got)
	}
	if !strings.Contains(e.ledger(t), "review_signoff_write_failed") {
		t.Fatalf("the failed write is reported: %q", e.ledger(t))
	}
	e.stop(t, reviewObsType("explorer"), "other-1", "nothing")
	r := e.round(t)
	if r.Lane.Verdict != goalplan.VerdictFail || r.Lane.ReviewerSession == nil || *r.Lane.ReviewerSession != "reviewer-1" {
		t.Fatalf("the first sign-off is recorded once the write succeeds: %+v", r)
	}
	if got := e.inboxFiles(t); len(got) != 0 {
		t.Fatalf("both entries reached a decision: %v", got)
	}
}
