package integrator

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// CRW-1413 pingpong round 1, items 1 and 2.
//
// Item 1: the head reaches dev after checkPR fetched and before the run fetched. The run reports already-in-dev and the pass
// recovers the landing; that recovery is the pass result (landed_before_record, status 0), and one that fails holds the entry and
// is reported as held, never as nothing-merged.
//
// Item 2: a landing record that cannot be written is not a posted comment: the entry stays pending.

// headArrivesAfterCheck wraps the runner's Exec: once checkPR's headInBranch finished (its merge-base of head against the remote branch), dev moves to head in
// the remote, as a push by someone else between the pass's check and its run. after (may be nil) runs on that move.
func (w *prWorld) headArrivesAfterCheck(head string, after func()) *bool {
	fired := new(bool)
	delegate := w.r.Exec
	w.r.Exec = func(ctx context.Context, c Cmd) (int, error) {
		rc, err := delegate(ctx, c)
		if !*fired {
			for i, a := range c.Argv {
				if a == "merge-base" && i+3 < len(c.Argv) && c.Argv[i+1] == "--is-ancestor" && c.Argv[i+2] == head && c.Argv[len(c.Argv)-1] != "HEAD" {
					*fired = true
					w.git(w.remote, "update-ref", "refs/heads/dev", head)
					if after != nil {
						after()
					}
					break
				}
			}
		}
		return rc, err
	}
	return fired
}

// TestARunPathRecoveryIsThePassResult: the head arrives in dev between the check and the run; the pass records the landing after
// the fact and reports landed_before_record with status 0, not nothing-merged.
func TestARunPathRecoveryIsThePassResult(t *testing.T) {
	w := newPRWorld(t, true)
	pr := w.milestone("10")
	w.enqueue("o/r#1")
	w.git(w.remote, "merge-base", "--is-ancestor", w.remoteDev(), pr.HeadSHA)
	fired := w.headArrivesAfterCheck(pr.HeadSHA, nil)

	rep := w.pass()
	if !*fired {
		t.Fatal("the head never arrived between the check and the run")
	}
	requireExit(t, rep, ExitPushed, resultReportLandedBeforeRecord)
	if rep.Landing == nil || rep.Landing.Result != resultLandedBeforeRecord || rep.Landing.Number != 1 || rep.Landing.Head != pr.HeadSHA {
		t.Errorf("the pass reports %+v", rep.Landing)
	}
	if rep.Error != "" || len(rep.Held) != 0 {
		t.Errorf("error %q, held %q", rep.Error, rep.Held)
	}
	if !w.doneFor("pr-1", pr.HeadSHA) || w.pendingHead("pr-1") != "" {
		t.Errorf("pr-1 is not in done/ (moved %q)", rep.Moved)
	}
	if ls := w.landings(); len(ls) != 1 || ls[0].Result != resultLandedBeforeRecord {
		t.Errorf("landing records %+v", ls)
	}
}

// TestAFailedRunPathRecoveryIsHeldNotNothingMerged: the same race, and the recovery cannot tell whether the earlier run's pushing
// marker is in dev (git fails). The entry stays pending, no record is written, and the pass reports it as held with the reason.
func TestAFailedRunPathRecoveryIsHeldNotNothingMerged(t *testing.T) {
	w := newPRWorld(t, true)
	pr := w.milestone("10")
	w.enqueue("o/r#1")
	base := w.remoteDev()
	first := w.pass()
	requireExit(t, first, ExitPushed, "pushed")
	dev := w.remoteDev()
	w.crashAfterPush(first, pr.HeadSHA)
	marker := readSummary(t, w, first.Summary.Tag)
	marker.Result = "pushing"
	summaryPath := filepath.Join(w.cfg.RunsDir, marker.Tag+".summary.json")
	data, err := json.Marshal(marker)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(summaryPath, append(data, '\n'), 0o664); err != nil {
		t.Fatal(err)
	}
	// dev is back where it was before the push, so the pass's check does not find the head; it arrives after the check
	w.git(w.remote, "update-ref", "refs/heads/dev", base)
	armed := new(bool)
	delegate := w.r.Exec
	w.r.Exec = func(ctx context.Context, c Cmd) (int, error) {
		// markerInBranch asks about the marker's head against the remote branch; the run's own check asks against HEAD
		for i, a := range c.Argv {
			if *armed && a == "merge-base" && i+3 < len(c.Argv) && c.Argv[i+1] == "--is-ancestor" && c.Argv[i+2] == marker.Head && c.Argv[len(c.Argv)-1] != "HEAD" {
				return -1, errors.New("git: boom")
			}
		}
		return delegate(ctx, c)
	}
	fired := w.headArrivesAfterCheck(pr.HeadSHA, func() {
		w.git(w.remote, "update-ref", "refs/heads/dev", dev)
		*armed = true
	})

	rep := w.pass()
	if !*fired || !*armed {
		t.Fatal("the head never arrived between the check and the run")
	}
	if rep.Result == "nothing-merged" || rep.Exit == ExitNothingMerged || rep.Exit == ExitPushed {
		t.Fatalf("the failed recovery is reported as exit %d result %q", rep.Exit, rep.Result)
	}
	if rep.Exit != ExitUsage || rep.Result != "error" || !strings.Contains(rep.Error, "pushing marker of run "+marker.Tag) {
		t.Errorf("pass = exit %d result %q error %q", rep.Exit, rep.Result, rep.Error)
	}
	if len(rep.Held) != 1 || !strings.HasPrefix(rep.Held[0], "pr-1: ") || !strings.Contains(rep.Held[0], "pushing marker of run "+marker.Tag) {
		t.Errorf("held %q", rep.Held)
	}
	if ls := w.landings(); len(ls) != 0 {
		t.Errorf("a landing was recorded: %+v", ls)
	}
	if w.pendingHead("pr-1") != pr.HeadSHA || w.doneFor("pr-1", pr.HeadSHA) {
		t.Errorf("the entry left pending (pending %q, done %v)", w.pendingHead("pr-1"), w.doneFor("pr-1", pr.HeadSHA))
	}
	if got, err := os.ReadFile(summaryPath); err != nil || !strings.Contains(string(got), `"pushing"`) {
		t.Errorf("the marker did not stay (%v): %s", err, got)
	}
}

// TestAStatus128RunPathRecoveryIsHeldAndTheNextPassRecordsIt: the same race as above, but git answers the recovery's ancestor
// question with status 128 (no start error). That is a repository error, not "no run pushed it": the pass is held with the
// reason, no landing record and no comment is written, the entry stays pending and the marker stays. With git working again, the
// next pass records the landing and moves the entry to done/.
func TestAStatus128RunPathRecoveryIsHeldAndTheNextPassRecordsIt(t *testing.T) {
	w := newPRWorld(t, true)
	pr := w.milestone("10")
	w.enqueue("o/r#1")
	base := w.remoteDev()
	first := w.pass()
	requireExit(t, first, ExitPushed, "pushed")
	dev := w.remoteDev()
	w.crashAfterPush(first, pr.HeadSHA)
	marker := readSummary(t, w, first.Summary.Tag)
	marker.Result = "pushing"
	summaryPath := filepath.Join(w.cfg.RunsDir, marker.Tag+".summary.json")
	data, err := json.Marshal(marker)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(summaryPath, append(data, '\n'), 0o664); err != nil {
		t.Fatal(err)
	}
	// dev is back where it was before the push, so the pass's check does not find the head; it arrives after the check
	w.git(w.remote, "update-ref", "refs/heads/dev", base)
	armed := new(bool)
	failed := 0
	delegate := w.r.Exec
	w.r.Exec = func(ctx context.Context, c Cmd) (int, error) {
		// markerInBranch asks about the marker's head against the remote branch; the run's own check asks against HEAD. Only the
		// ancestor question fails: the commit probe goes to the real git and finds the commit, so the status is an error.
		for i, a := range c.Argv {
			if *armed && a == "merge-base" && i+3 < len(c.Argv) && c.Argv[i+1] == "--is-ancestor" && c.Argv[i+2] == marker.Head && c.Argv[len(c.Argv)-1] != "HEAD" {
				failed++
				return 128, nil
			}
		}
		return delegate(ctx, c)
	}
	fired := w.headArrivesAfterCheck(pr.HeadSHA, func() {
		w.git(w.remote, "update-ref", "refs/heads/dev", dev)
		*armed = true
	})
	before := len(w.fake.Comments(1))

	rep := w.pass()
	if !*fired || !*armed {
		t.Fatal("the head never arrived between the check and the run")
	}
	if failed == 0 {
		t.Fatal("the recovery never asked git about the marker")
	}
	if rep.Exit != ExitUsage || rep.Result != "error" || !strings.Contains(rep.Error, "pushing marker of run "+marker.Tag) {
		t.Errorf("pass = exit %d result %q error %q", rep.Exit, rep.Result, rep.Error)
	}
	if len(rep.Held) != 1 || !strings.HasPrefix(rep.Held[0], "pr-1: ") || !strings.Contains(rep.Held[0], "landing could not be recorded") || !strings.Contains(rep.Held[0], "pushing marker of run "+marker.Tag) {
		t.Errorf("held %q", rep.Held)
	}
	if ls := w.landings(); len(ls) != 0 {
		t.Errorf("a landing was recorded on a git failure: %+v", ls)
	}
	if w.pendingHead("pr-1") != pr.HeadSHA || w.doneFor("pr-1", pr.HeadSHA) {
		t.Errorf("the entry left pending (pending %q, done %v)", w.pendingHead("pr-1"), w.doneFor("pr-1", pr.HeadSHA))
	}
	if got, err := os.ReadFile(summaryPath); err != nil || !strings.Contains(string(got), `"pushing"`) {
		t.Errorf("the marker did not stay (%v): %s", err, got)
	}
	if n := len(w.fake.Comments(1)); n != before {
		t.Errorf("%d comments, want %d: a comment was posted for a landing that was not recorded", n, before)
	}

	*armed = false
	w.pass()
	ls := w.landings()
	if len(ls) != 1 {
		t.Fatalf("landing records %+v", ls)
	}
	if ls[0].Result != resultLandedBeforeRecord || ls[0].Head != pr.HeadSHA || ls[0].Verification.Record != marker.Record {
		t.Errorf("the retried record %+v", ls[0])
	}
	if !w.doneFor("pr-1", pr.HeadSHA) || w.pendingHead("pr-1") != "" {
		t.Errorf("the entry is not done after the retry (pending %q)", w.pendingHead("pr-1"))
	}
}

// refuseRecordWrite makes writing the landing record of tag fail: a directory sits where its temporary file goes.
func (w *prWorld) refuseRecordWrite(tag string) {
	w.t.Helper()
	if err := os.Mkdir(filepath.Join(w.cfg.RunsDir, tag+".pr.json.new"), 0o775); err != nil {
		w.t.Fatal(err)
	}
}

// TestARepostWhoseRecordCannotBeWrittenHoldsTheEntry: the comment of a landing record "not written yet" is posted, and the record
// cannot be written. The entry is held in pending with the reason (it does not move to done/ over a record that still says "not
// written yet"); with the write possible again, a later pass finishes it.
func TestARepostWhoseRecordCannotBeWrittenHoldsTheEntry(t *testing.T) {
	w := newPRWorld(t, true)
	pr := w.milestone("10")
	w.enqueue("o/r#1")
	first := w.pass()
	requireExit(t, first, ExitPushed, "pushed")
	path := w.interruptComment(first, pr.HeadSHA, "not written yet")
	w.refuseRecordWrite(first.Summary.Tag)

	rep := w.pass()
	if len(rep.Held) != 1 || !strings.HasPrefix(rep.Held[0], "pr-1: ") || !strings.Contains(rep.Held[0], "record") {
		t.Errorf("held %q", rep.Held)
	}
	if w.pendingHead("pr-1") != pr.HeadSHA || w.doneFor("pr-1", pr.HeadSHA) {
		t.Errorf("the entry left pending (pending %q, done %v)", w.pendingHead("pr-1"), w.doneFor("pr-1", pr.HeadSHA))
	}
	if l := readLanding(t, path); l.Comment != "not written yet" {
		t.Errorf("the record changed: %+v", l)
	}

	if err := os.Remove(filepath.Join(w.cfg.RunsDir, first.Summary.Tag+".pr.json.new")); err != nil {
		t.Fatal(err)
	}
	w.pass()
	if l := readLanding(t, path); l.Comment != "written" {
		t.Errorf("the record after the write works again %+v", l)
	}
	if !w.doneFor("pr-1", pr.HeadSHA) {
		t.Error("the entry is not done once the record could be written")
	}
}

// TestARecoveredRecordThatCannotBeWrittenHoldsTheEntry: the recovery of a landing whose record cannot be written posts no comment
// and holds the entry (checkPR path).
func TestARecoveredRecordThatCannotBeWrittenHoldsTheEntry(t *testing.T) {
	w := newPRWorld(t, true)
	pr := w.milestone("10")
	w.enqueue("o/r#1")
	first := w.pass()
	requireExit(t, first, ExitPushed, "pushed")
	w.crashAfterPush(first, pr.HeadSHA)
	// the pass reserves the tag its clock gives: the record of that tag cannot be written
	w.refuseRecordWrite(w.cfg.TagPrefix + w.now.Add(time.Minute).Format("0102150405"))
	before := len(w.fake.Comments(1))

	rep := w.pass()
	if len(rep.Held) != 1 || !strings.HasPrefix(rep.Held[0], "pr-1: ") || !strings.Contains(rep.Held[0], "landing could not be recorded") {
		t.Errorf("held %q", rep.Held)
	}
	if w.pendingHead("pr-1") != pr.HeadSHA || w.doneFor("pr-1", pr.HeadSHA) {
		t.Errorf("the entry left pending (pending %q, done %v)", w.pendingHead("pr-1"), w.doneFor("pr-1", pr.HeadSHA))
	}
	if n := len(w.fake.Comments(1)); n != before {
		t.Errorf("%d comments, want %d: a comment was posted for a record that does not exist", n, before)
	}
}
