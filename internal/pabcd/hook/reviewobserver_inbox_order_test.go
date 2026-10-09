package hook_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/goalplan"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/hook"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
)

// CRW-1113 correction round 3. The inbox is a bounded, ordered queue: the bound holds when several observers keep a sign-off at once,
// the order of the whole pending set is the order the sign-offs arrived in (never the order of an arbitrary subset of file names),
// and an inbox the drain cannot read is not an empty one.

// reviewObsFiller writes an entry the way an observer of an earlier head or a racing one could have left it: a valid, older sign-off
// for a launch no round has, under a name that sorts ahead of every hashed entry name.
func (e reviewObsEnv) filler(t *testing.T, dir string, n int) {
	t.Helper()
	body, err := json.Marshal(map[string]any{"version": 1, "sessionId": e.session, "slug": e.slug, "planEpoch": reviewObsEpoch, "workPhaseId": "wp0",
		"launchId": fmt.Sprintf("filler-%d", n), "agentId": "filler", "verdict": "PASS", "receivedAt": "2000-01-01T00:00:00.000000000Z"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("%032x.json", n)), append(body, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
}

// d1: an entry the drain cannot read (access refused; it is not corrupt) is kept, and nothing newer is applied ahead of it, because
// what it holds decides how the newer sign-offs are judged.
func TestReviewObserverKeepsAnEntryItCannotReadAndAppliesNothingAheadOfIt(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores file permissions")
	}
	e := reviewObsSeed(t, "rb", nil)
	launch := e.open(t)
	release := e.holdGoalplanLock(t)
	e.stop(t, reviewObsType("explorer"), "reviewer-1", reviewObsSignoff(launch, "FAIL"))
	release()
	files := e.inboxFiles(t)
	if len(files) != 1 {
		t.Fatalf("setup: %v", files)
	}
	if err := os.Chmod(files[0], 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(files[0], 0o644) })
	// A later reviewer's PASS arrives while the earlier FAIL cannot be read.
	e.stop(t, reviewObsType("explorer"), "reviewer-2", reviewObsSignoff(launch, "PASS"))
	_ = os.Chmod(files[0], 0o644)
	if _, err := os.Lstat(files[0]); err != nil {
		t.Fatalf("an entry that could not be read is not deleted: %v", err)
	}
	if r := e.round(t); r.Lane.Verdict != "" {
		t.Fatalf("nothing is recorded ahead of an entry nobody has read: %+v", r)
	}
	e.stop(t, reviewObsType("explorer"), "other-1", "nothing")
	r := e.round(t)
	if r.Lane.Verdict != goalplan.VerdictFail || r.Lane.ReviewerSession == nil || *r.Lane.ReviewerSession != "reviewer-1" {
		t.Fatalf("once readable the earlier FAIL is the verdict: %+v", r)
	}
	if got := e.inboxFiles(t); len(got) != 0 {
		t.Fatalf("both entries reached a decision: %v", got)
	}
}

// d3, admission: the bound of 64 holds when two observers both pass the capacity check before either has linked its entry.
func TestReviewObserverInboxBoundHoldsForConcurrentAdmission(t *testing.T) {
	e := reviewObsSeed(t, "rb", nil)
	launch := e.open(t)
	release := e.holdGoalplanLock(t)
	defer release()
	e.stop(t, reviewObsType("explorer"), "reviewer-0", reviewObsSignoff(launch, "FAIL"))
	dir := filepath.Dir(e.inboxFiles(t)[0])
	for n := 1; n < 63; n++ {
		e.filler(t, dir, n)
	}
	if got := len(e.inboxFiles(t)); got != 63 {
		t.Fatalf("setup: 63 kept entries: %d", got)
	}
	var arrived sync.WaitGroup
	arrived.Add(2)
	restore := hook.SetReviewObserverInboxCounted(func() {
		// Hold the first two arrivals until both are here (or give up, which is what a serialised admission does to the first).
		done := make(chan struct{})
		go func() { arrived.Done(); arrived.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(400 * time.Millisecond):
		}
	})
	defer restore()
	var wg sync.WaitGroup
	for _, agent := range []string{"reviewer-1", "reviewer-2"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			payload, _ := json.Marshal(map[string]any{"hook_event_name": "SubagentStop", "cwd": e.cwd, "session_id": e.session, "agent_type": "explorer",
				"agent_id": agent, "last_assistant_message": reviewObsSignoff(launch, "PASS")})
			hook.HandleReviewObserver(string(payload))
		}()
	}
	wg.Wait()
	if got := len(e.inboxFiles(t)); got > 64 {
		t.Fatalf("the inbox holds %d entries, more than its bound of 64", got)
	}
}

// d3, order: the pending set is applied in the order the sign-offs arrived in, however many entries there are and whatever their
// file names sort like. An older kept FAIL whose file name falls outside the first 64 names is still judged before a newer PASS
// for the same launch.
func TestReviewObserverInboxAppliesTheWholePendingSetInArrivalOrder(t *testing.T) {
	e := reviewObsSeed(t, "rb", nil)
	launch := e.open(t)
	release := e.holdGoalplanLock(t)
	e.stop(t, reviewObsType("explorer"), "reviewer-1", reviewObsSignoff(launch, "FAIL"))
	failFile := e.inboxFiles(t)[0]
	dir := filepath.Dir(failFile)
	// A later PASS whose entry name sorts before the FAIL's.
	var passFile string
	for n := 2; passFile == "" && n < 200; n++ {
		before := map[string]bool{}
		for _, f := range e.inboxFiles(t) {
			before[f] = true
		}
		e.stop(t, reviewObsType("explorer"), fmt.Sprintf("reviewer-%d", n), reviewObsSignoff(launch, "PASS"))
		for _, f := range e.inboxFiles(t) {
			switch {
			case before[f]:
			case f < failFile:
				passFile = f
			default:
				_ = os.Remove(f)
			}
		}
	}
	if passFile == "" {
		t.Fatal("setup: no PASS name sorts before the FAIL name")
	}
	for n := 1; n <= 63; n++ {
		e.filler(t, dir, n)
	}
	if got := len(e.inboxFiles(t)); got != 65 {
		t.Fatalf("setup: 65 kept entries as a race could leave them: %d", got)
	}
	release()
	e.stop(t, reviewObsType("explorer"), "other-1", "nothing")
	r := e.round(t)
	if r.Lane.Verdict != goalplan.VerdictFail || r.Lane.ReviewerSession == nil || *r.Lane.ReviewerSession != "reviewer-1" {
		t.Fatalf("the older FAIL is the verdict, not the newer PASS that sorts ahead of it: %+v", r)
	}
}

// inLock runs the A>B publication's drain the way the transition does: under the session lock and the goalplan write lock.
func (e reviewObsEnv) inLock(t *testing.T, beforeWrite func() error) (hook.ReviewObserverDrain, error) {
	t.Helper()
	var drained hook.ReviewObserverDrain
	var drainErr error
	if err := state.WithSessionLock(e.cwd, e.session, func() error {
		_, err := goalplan.WithGoalplanWriteLock(e.cwd, e.slug, func(plan *goalplan.Goalplan) (string, error) {
			drained, drainErr = hook.DrainReviewObserverInboxInLock(e.cwd, e.session, plan, beforeWrite)
			return "", nil
		}, nil)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return drained, drainErr
}

// d2: the in-lock drain runs the caller's check before its first durable change; an error from it ends the drain with nothing
// written, nothing removed and nothing counted as kept.
func TestReviewObserverInLockDrainStopsBeforeItsFirstWrite(t *testing.T) {
	e := reviewObsSeed(t, "rb", nil)
	launch := e.open(t)
	release := e.holdGoalplanLock(t)
	e.stop(t, reviewObsType("explorer"), "reviewer-1", reviewObsSignoff(launch, "PASS"))
	release()
	cancelled := errors.New("cancelled")
	drained, err := e.inLock(t, func() error { return cancelled })
	if !errors.Is(err, cancelled) || drained.Wrote {
		t.Fatalf("the caller's check ends the drain before any write: %v %+v", err, drained)
	}
	if r := e.round(t); r.Lane.Verdict != "" || len(e.inboxFiles(t)) != 1 || e.ledger(t) != "" {
		t.Fatalf("nothing was written: %+v %v %q", r, e.inboxFiles(t), e.ledger(t))
	}
	drained, err = e.inLock(t, func() error { return nil })
	if err != nil || !drained.Wrote || drained.Kept != 0 || e.round(t).Status != goalplan.ReviewApproved {
		t.Fatalf("a check that passes lets the drain write: %v %+v", err, drained)
	}
}

// d1: the in-lock drain reports what it could not read as kept, writes no verdict and removes nothing but entries that were read and
// are not entries.
func TestReviewObserverInLockDrainReportsAnInboxItCannotRead(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores file permissions")
	}
	for _, what := range []string{"entry file", "session inbox directory"} {
		t.Run(what, func(t *testing.T) {
			e := reviewObsSeed(t, "rb", nil)
			launch := e.open(t)
			release := e.holdGoalplanLock(t)
			e.stop(t, reviewObsType("explorer"), "reviewer-1", reviewObsSignoff(launch, "FAIL"))
			release()
			entry := e.inboxFiles(t)[0]
			blocked, mode := entry, os.FileMode(0o644)
			if what != "entry file" {
				blocked, mode = filepath.Dir(entry), 0o755
			}
			if err := os.Chmod(blocked, 0); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Chmod(blocked, mode) })
			drained, err := e.inLock(t, nil)
			_ = os.Chmod(blocked, mode)
			if err != nil || drained.Kept == 0 {
				t.Fatalf("an inbox that cannot be read is kept, not empty: %v %+v", err, drained)
			}
			if _, err := os.Lstat(entry); err != nil {
				t.Fatalf("the FAIL is not deleted: %v", err)
			}
			if r := e.round(t); r.Lane.Verdict != "" {
				t.Fatalf("no verdict is recorded: %+v", r)
			}
		})
	}
}
