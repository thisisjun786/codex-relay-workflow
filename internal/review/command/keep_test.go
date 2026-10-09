package command

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/crwdir"
	"github.com/thisisjun786/codex-relay-workflow/internal/review"
)

// finishedRecords counts the finished records of the ledger.
func (f *fixture) finishedRecords() (n int) {
	for _, r := range f.ledger() {
		if r.Event == "finished" {
			n++
		}
	}
	return n
}

// A result that cannot be kept in the state directory is not recorded as finished and nothing is published: the command fails naming the cause, and the same patch is reviewed again once
// the fault is gone (a second model call is accepted over a finished record that points to a copy that does not exist).
func TestResultThatCannotBeKeptIsNotRecordedAsFinishedAndTheNextCallReviewsAgain(t *testing.T) {
	f := newFixture(t)
	h := f.repo.change(f.base, 2)
	blocker := filepath.Join(f.state, "results")
	if err := errors.Join(os.MkdirAll(f.state, 0o700), os.WriteFile(blocker, nil, 0o600)); err != nil { // a file where the directory of kept results belongs
		t.Fatal(err)
	}
	code, _, errOut := f.run(h)
	recs := f.ledger()
	if code != 1 || !strings.Contains(errOut, "could not be kept") || !strings.Contains(errOut, "results") || f.s.count() != 2 || f.finishedRecords() != 0 || len(recs) == 0 || recs[len(recs)-1].Event != eventKeepFailed {
		t.Fatalf("kept copy failure: %d %s (calls %d, ledger %+v)", code, errOut, f.s.count(), recs)
	}
	for _, name := range []string{h + ".json", h + ".json.sha256"} {
		if _, err := os.Stat(filepath.Join(f.out, name)); !os.IsNotExist(err) {
			t.Errorf("%s was published although the result was not kept: %v", name, err)
		}
	}
	if err := os.Remove(blocker); err != nil { // the fault is gone
		t.Fatal(err)
	}
	code, again, errOut := f.run(h)
	if code != 0 || again.Outcome != OutcomeReviewed || f.s.count() != 4 || f.finishedRecords() != 1 {
		t.Fatalf("run again: %d %+v %s (calls %d, ledger %+v)", code, again, errOut, f.s.count(), f.ledger())
	}
	for _, name := range []string{h + ".json", h + ".json.sha256"} {
		if _, err := os.Stat(filepath.Join(f.out, name)); err != nil {
			t.Errorf("%s was not published after the retry: %v", name, err)
		}
	}
	if _, third, _ := f.run(h); third.Outcome != OutcomeAlreadyReviewed || f.s.count() != 4 {
		t.Fatalf("a kept and recorded result is reviewed again: %+v (calls %d)", third, f.s.count())
	}
}

// The failure that kept a copy from being written may also be one of the kept copy's last step (the directory entry that makes it durable): the result is then not recorded either, and the cause is named.
func TestResultWhoseKeptCopyCannotBeMadeDurableIsNotRecordedAsFinished(t *testing.T) {
	f := newFixture(t)
	h := f.repo.change(f.base, 2)
	injected := errors.New("directory fsync injected")
	f.keep = func(path string, data []byte) error {
		if err := crwdir.PublishDurable(path, data); err != nil {
			return err
		}
		return injected // the copy is in place, but the directory entry is not known to be durable
	}
	code, _, errOut := f.run(h)
	if code != 1 || !strings.Contains(errOut, injected.Error()) || f.finishedRecords() != 0 {
		t.Fatalf("durability failure: %d %s (ledger %+v)", code, errOut, f.ledger())
	}
	if _, err := os.Stat(filepath.Join(f.out, h+".json")); !os.IsNotExist(err) {
		t.Fatalf("the artifact was published: %v", err)
	}
}

// The kept copy is written by the durable publish (the directory entry fsynced after the rename), and that happens before the finished record is appended, never after.
func TestKeptCopyIsWrittenDurablyBeforeTheFinishedRecord(t *testing.T) {
	f := newFixture(t)
	h := f.repo.change(f.base, 2)
	var seen []string
	f.keep = func(path string, data []byte) error {
		seen = append(seen, path)
		if n := f.finishedRecords(); n != 0 {
			t.Errorf("%d finished records exist while the kept copy is being written", n)
		}
		return crwdir.PublishDurable(path, data)
	}
	if code, sum, errOut := f.run(h); code != 0 || len(seen) != 1 || seen[0] != filepath.Join(f.state, "results", sum.SHA256+".json") || f.finishedRecords() != 1 {
		t.Fatalf("run: %d %+v %s (kept %v)", code, sum, errOut, seen)
	}
}

// A path two patches of one head share (the same head against two bases, the file name being the head's) belongs, in the ledger, to the result that was recorded for it last. A missing path is restored
// from that result whichever of the two patches asks, and a path that holds the bytes of the other, superseded result gets the newest result back, so Q is restored after P was, and P never takes the path from Q.
func TestRestoreOfASharedPathFollowsTheResultTheLedgerAssignedLast(t *testing.T) {
	f := newFixture(t)
	h2 := f.repo.change(f.base, 2)
	f.repo.git("checkout", "-q", "--detach", h2)
	h3 := f.repo.commit(map[string]string{"a.go": "package a\n\nfunc F() int { return 3 }\n"})
	baseP, baseQ := f.base, h2
	f.base = baseP
	_, p, errOut := f.run(h3)
	if p.Outcome != OutcomeReviewed {
		t.Fatalf("P: %+v %s", p, errOut)
	}
	pBytes, _ := os.ReadFile(p.Artifact)
	lose := func() {
		t.Helper()
		if err := errors.Join(os.Remove(p.Artifact), os.Remove(p.Artifact+".sha256")); err != nil {
			t.Fatal(err)
		}
	}
	lose() // the path is free again, as when the first output was cleaned up, and Q's review lands on it
	f.base = baseQ
	_, q, errOut := f.run(h3)
	if q.Outcome != OutcomeReviewed || q.Artifact != p.Artifact || q.SHA256 == p.SHA256 {
		t.Fatalf("Q: %+v %s", q, errOut)
	}
	lose()
	check := func(step, want string) {
		t.Helper()
		data, err := os.ReadFile(p.Artifact)
		sum, _ := os.ReadFile(p.Artifact + ".sha256")
		digest := fmt.Sprintf("%x", sha256.Sum256(data))
		if err != nil || digest != want || string(sum) != want+"  "+h3+".json\n" {
			t.Fatalf("%s: the path holds %s (%v), checksum file %q, want %s", step, digest, err, sum, want)
		}
	}
	// P asks about the missing path: the result that owns it in the ledger, Q's, is written.
	f.base = baseP
	if _, again, errOut := f.run(h3); again.Outcome != OutcomeAlreadyReviewed || len(again.Restored) != 2 {
		t.Fatalf("P asks: %+v %s", again, errOut)
	}
	check("P asked", q.SHA256)
	// Q asks: its own file is there, nothing is written.
	f.base = baseQ
	if _, again, errOut := f.run(h3); again.Outcome != OutcomeAlreadyReviewed || len(again.Restored) != 0 {
		t.Fatalf("Q asks: %+v %s", again, errOut)
	}
	check("Q asked", q.SHA256)
	// P's bytes are on the path again (as an earlier restore of P left them): the ledger's owner of the path is Q, so a request of either patch puts Q's result back, and never P's over Q's.
	plantP := func() {
		t.Helper()
		if err := errors.Join(os.WriteFile(p.Artifact, pBytes, 0o644), os.WriteFile(p.Artifact+".sha256", []byte(p.SHA256+"  "+h3+".json\n"), 0o644)); err != nil {
			t.Fatal(err)
		}
	}
	for _, asker := range []struct{ name, base string }{{"P", baseP}, {"Q", baseQ}} {
		plantP()
		f.base = asker.base
		if _, again, errOut := f.run(h3); again.Outcome != OutcomeAlreadyReviewed || len(again.Restored) != 2 || again.ArtifactPresent == nil || !*again.ArtifactPresent {
			t.Fatalf("%s asks with P's bytes on the path: %+v %s", asker.name, again, errOut)
		}
		check(asker.name+" asked with P's bytes on the path", q.SHA256)
	}
	// A foreign file on the path is nobody's to replace.
	if err := os.WriteFile(p.Artifact, []byte("someone else's file\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.base = baseQ
	if _, again, _ := f.run(h3); len(again.Restored) != 0 {
		t.Fatalf("a foreign file was replaced: %+v", again)
	}
	if f.s.count() != 4 {
		t.Fatalf("a restore called the model: %d calls", f.s.count())
	}
}

// A result recorded before results were kept (an older ledger) has no copy to restore from: nothing is written and the answer is what it always was.
func TestResultWithoutAKeptCopyIsNotRestored(t *testing.T) {
	f := newFixture(t)
	h := f.repo.change(f.base, 2)
	_, first, _ := f.run(h)
	kept := filepath.Join(f.state, "results", first.SHA256+".json")
	if _, err := os.Stat(kept); err != nil {
		t.Fatalf("the result was not kept at %s: %v", kept, err)
	}
	if err := errors.Join(os.RemoveAll(filepath.Join(f.state, "results")), os.Remove(first.Artifact)); err != nil {
		t.Fatal(err)
	}
	code, again, _ := f.run(h)
	if _, err := os.Stat(first.Artifact); code != 0 || again.Outcome != OutcomeAlreadyReviewed || again.ArtifactPresent == nil || *again.ArtifactPresent || len(again.Restored) != 0 || !os.IsNotExist(err) {
		t.Fatalf("an older ledger: %d %+v %v", code, again, err)
	}
}

// Restoring writes into the directory a concurrent review of the same head may be about to publish into, so it takes the run lock; a busy lock leaves the repair to the next call and the answer at once.
func TestRestoreIsLeftToTheNextCallWhileAnotherReviewHoldsTheRunLock(t *testing.T) {
	f := newFixture(t)
	h := f.repo.change(f.base, 2)
	_, first, _ := f.run(h)
	if err := os.Remove(first.Artifact); err != nil {
		t.Fatal(err)
	}
	unlock, err := (&ledger{dir: f.state}).lock(context.Background(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	code, busy, errOut := f.run(h)
	unlock()
	if code != 0 || busy.Outcome != OutcomeAlreadyReviewed || busy.ArtifactPresent == nil || *busy.ArtifactPresent || len(busy.Restored) != 0 {
		t.Fatalf("while the run lock is held: %d %+v %s", code, busy, errOut)
	}
	if _, again, _ := f.run(h); !slices.Equal(again.Restored, []string{first.Artifact}) { // the checksum file was never lost
		t.Fatalf("after the release: %+v", again)
	}
}

// A retry whose files were not written leaves the unavailable attempt's bytes at the path; they are the one thing a restore replaces, and no other file at the path ever is.
func TestRestoreReplacesOnlyTheUnavailableBytesOfARetryWhoseFilesWereNotWritten(t *testing.T) {
	f := newFixture(t)
	h := f.repo.change(f.base, 2)
	f.on("2026-10-04", quotaResult)
	_, first, _ := f.run(h)
	oldData, _ := os.ReadFile(first.Artifact)
	oldSum, _ := os.ReadFile(first.Artifact + ".sha256")
	f.on("2026-10-05", okResult)
	if code, retry, errOut := f.run(h); code != 0 || retry.Outcome != OutcomeReviewed || retry.SHA256 == first.SHA256 {
		t.Fatalf("retry: %d %+v %s", code, retry, errOut)
	}
	newData, _ := os.ReadFile(first.Artifact)
	if err := errors.Join(os.WriteFile(first.Artifact, oldData, 0o644), os.WriteFile(first.Artifact+".sha256", oldSum, 0o644)); err != nil {
		t.Fatal(err)
	}
	calls := f.s.count()
	code, again, errOut := f.run(h)
	if got, _ := os.ReadFile(first.Artifact); code != 0 || again.Outcome != OutcomeAlreadyReviewed || len(again.Restored) != 2 || !bytes.Equal(got, newData) || f.s.count() != calls {
		t.Fatalf("restore after a retry whose files were not written: %d %+v %s", code, again, errOut)
	}
	if err := os.WriteFile(first.Artifact, []byte("someone else's file\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(first.Artifact + ".sha256"); err != nil { // not even its checksum file is written beside a file that is not the recorded artifact
		t.Fatal(err)
	}
	if _, foreign, _ := f.run(h); len(foreign.Restored) != 0 {
		t.Fatalf("a foreign file was restored over: %+v", foreign)
	} else if got, _ := os.ReadFile(first.Artifact); string(got) != "someone else's file\n" {
		t.Fatalf("a foreign file was replaced: %q", got)
	} else if _, err := os.Stat(first.Artifact + ".sha256"); !os.IsNotExist(err) {
		t.Fatalf("a checksum file was written beside a foreign file: %v", err)
	}
}

// A restore of an older result that reaches the lock after a newer result replaced it at the same path must leave the newer result's files alone: its checksum is not the older one's.
func TestRestoreOfAnOlderResultLeavesTheNewerResultsChecksumAlone(t *testing.T) {
	f := newFixture(t)
	h := f.repo.change(f.base, 2)
	f.on("2026-10-04", quotaResult)
	f.run(h)
	f.on("2026-10-05", okResult)
	_, retry, _ := f.run(h)
	var older record
	for _, r := range f.ledger() {
		if r.Event == "unavailable" {
			older = r
		}
	}
	before, _ := os.ReadFile(retry.Artifact + ".sha256")
	written, err := (&ledger{dir: f.state}).restore(context.Background(), older, f.ledger(), f.out, true)
	if after, _ := os.ReadFile(retry.Artifact + ".sha256"); err != nil || len(written) != 0 || !bytes.Equal(before, after) || !strings.HasPrefix(string(after), retry.SHA256) {
		t.Fatalf("restoring the older result: wrote %v (%v); checksum file %q, was %q", written, err, after, before)
	}
}

// The state directory is fsynced by every call that keeps a result, not only by the one that created results/: a first call whose sync failed (or that was killed right after the directory was made) leaves
// results/ in place, and the next call must not take its existence for proof that its entry is durable.
func TestEveryKeptCopySyncsTheStateDirectoryAndNotOnlyTheCallThatCreatedResults(t *testing.T) {
	dir := t.TempDir()
	var synced []string
	failFirst := true
	l := &ledger{dir: dir, syncDir: func(d string) error {
		synced = append(synced, d)
		if failFirst {
			failFirst = false
			return errors.New("fsync injected")
		}
		return crwdir.SyncDir(d)
	}}
	if err := l.keep("aa", []byte("one")); err == nil || !strings.Contains(err.Error(), "fsync injected") {
		t.Fatalf("the failed sync of the state directory must fail the keep: %v", err)
	}
	if err := l.keep("bb", []byte("two")); err != nil {
		t.Fatal(err)
	}
	if err := l.keep("cc", []byte("three")); err != nil {
		t.Fatal(err)
	}
	if want := []string{dir, dir, dir}; !slices.Equal(synced, want) {
		t.Fatalf("the state directory was synced %v, want it for each of the three keeps", synced)
	}
}

// The one more attempt of an unavailable review is spent when it starts, except when what ended it was a result that could not be kept: that attempt left no result and nothing was recorded, so the
// patch stays open and the same command can be run again once the fault is gone.
func TestRetryWhoseResultCannotBeKeptDoesNotSpendTheOneMoreAttempt(t *testing.T) {
	f := newFixture(t)
	h := f.repo.change(f.base, 2)
	f.on("2026-10-04", quotaResult)
	if code, first, errOut := f.run(h); code != 0 || first.RetryNotBefore != "2026-10-05" {
		t.Fatalf("quota: %d %+v %s", code, first, errOut)
	}
	f.on("2026-10-05", okResult)
	blocker := filepath.Join(f.state, "results")
	if err := errors.Join(os.RemoveAll(blocker), os.WriteFile(blocker, nil, 0o600)); err != nil { // the state directory takes no copy
		t.Fatal(err)
	}
	calls := f.s.count()
	if code, _, errOut := f.run(h); code != 1 || !strings.Contains(errOut, "could not be kept") || f.s.count() == calls {
		t.Fatalf("the retry whose result cannot be kept: %d %s (calls %d)", code, errOut, f.s.count())
	}
	if err := os.Remove(blocker); err != nil { // the fault is gone
		t.Fatal(err)
	}
	calls = f.s.count()
	code, again, errOut := f.run(h)
	if code != 0 || again.Outcome != OutcomeReviewed || again.Status != "complete" || f.s.count() == calls {
		t.Fatalf("the run after the fault: %d %+v %s (calls %d, ledger %+v)", code, again, errOut, f.s.count(), f.ledger())
	}
	// This attempt did end in a result, so the patch is closed on it from now on.
	calls = f.s.count()
	if _, third, _ := f.run(h); third.Outcome != OutcomeAlreadyReviewed || third.Status != "complete" || f.s.count() != calls {
		t.Fatalf("a recorded result is reviewed again: %+v", third)
	}
	// A retry that ends in some other way still spends the attempt: an interrupted or crashed process leaves a bare started line.
	g := newFixture(t)
	gh := g.repo.change(g.base, 2)
	g.on("2026-10-04", quotaResult)
	g.run(gh)
	killed := &ledger{dir: g.state, now: func() time.Time { return time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC) }}
	recs, err := killed.read() // the length of the whole lines, so that the append does not cut them off
	if err != nil {
		t.Fatal(err)
	}
	if err := killed.append(record{Event: "started", PatchID: recs[0].PatchID, Base: g.base, Head: gh}); err != nil {
		t.Fatal(err)
	}
	g.on("2026-10-05", okResult)
	if _, sum, _ := g.run(gh); sum.Outcome != OutcomeAlreadyReviewed || sum.Status != string(review.StatusUnavailable) {
		t.Fatalf("a retry that was killed must still spend the attempt: %+v", sum)
	}
}

// Two patches of one head share the output path. Whichever of them asks, the path is restored from the result the ledger assigned to it last, and the summary comment of the asking patch is made from that patch's
// own result (its kept copy), because the file at the path is a result of the ledger too, only another patch's.
func TestPostOfASharedPathUsesTheAskingPatchsOwnResult(t *testing.T) {
	f := newFixture(t)
	f.forge = &scriptedForge{}
	h2 := f.repo.change(f.base, 2)
	f.repo.git("checkout", "-q", "--detach", h2)
	h3 := f.repo.commit(map[string]string{"a.go": "package a\n\nfunc F() int { return 3 }\n"})
	baseP, baseQ := f.base, h2
	f.base = baseP
	_, p, errOut := f.run(h3)
	if p.Outcome != OutcomeReviewed {
		t.Fatalf("P: %+v %s", p, errOut)
	}
	remove := func() {
		t.Helper()
		if err := errors.Join(os.Remove(p.Artifact), os.Remove(p.Artifact+".sha256")); err != nil {
			t.Fatal(err)
		}
	}
	remove()
	f.base = baseQ
	_, q, errOut := f.run(h3)
	if q.Outcome != OutcomeReviewed || q.SHA256 == p.SHA256 || q.Artifact != p.Artifact {
		t.Fatalf("Q: %+v %s", q, errOut)
	}
	remove()
	for _, c := range []struct{ name, base, sha string }{{"P", baseP, p.SHA256}, {"Q", baseQ, q.SHA256}, {"P again", baseP, p.SHA256}} {
		f.base = c.base
		for _, flag := range []string{"--post-only", "--post-summary"} {
			remove2 := func() { _ = errors.Join(os.Remove(p.Artifact), os.Remove(p.Artifact+".sha256")) }
			remove2() // the path is missing, so the restore puts the ledger's owner of it back
			code, sum, errOut := f.run(h3, flag, "--pr", "7")
			if code != 0 || sum.Comment == nil || sum.SHA256 != c.sha {
				t.Fatalf("%s %s: %d %+v %s", c.name, flag, code, sum, errOut)
			}
			if body := f.forge.(*scriptedForge).comments; len(body) != 1 || !strings.Contains(body[0].Body, short(c.sha)) {
				t.Fatalf("%s %s: the comment is not made from this patch's result %s: %+v", c.name, flag, short(c.sha), body)
			}
			if data, _ := os.ReadFile(p.Artifact); fmt.Sprintf("%x", sha256.Sum256(data)) != q.SHA256 {
				t.Fatalf("%s %s: the path does not hold the result the ledger assigned to it last", c.name, flag)
			}
		}
	}
	// A file at the path that no result of the ledger explains is still no artifact to post.
	if err := os.WriteFile(p.Artifact, []byte("someone else's file\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.base = baseP
	if code, _, errOut := f.run(h3, "--post-only", "--pr", "7"); code != 1 || !strings.Contains(errOut, "not the recorded") {
		t.Fatalf("a foreign file was posted from: %d %s", code, errOut)
	}
}

// The retry whose result cannot be kept and whose keep_failed line cannot be appended either (the state directory takes neither) has called the model, so its started line stays in the ledger and
// counts toward the daily cap: the ledger is never rewritten to forget a review that ran. The attempt is then spent as the ledger stands (a bare started line spends the one more attempt, as a killed
// process does), so the same patch is not reviewed again and another patch of the same day is refused by the cap.
func TestRetryWhoseResultAndKeepFailedLineCannotBeWrittenStillCountsTowardTheDailyCap(t *testing.T) {
	f := newFixture(t)
	h := f.repo.change(f.base, 2)
	other := f.repo.change(f.base, 3)
	f.on("2026-10-04", quotaResult)
	if code, first, errOut := f.run(h, "--daily-cap", "1"); code != 0 || first.RetryNotBefore != "2026-10-05" {
		t.Fatalf("quota: %d %+v %s", code, first, errOut)
	}
	before := f.ledger()
	f.on("2026-10-05", okResult)
	f.keep = func(string, []byte) error { return errors.New("keep injected") }
	f.fault = func(r record) error {
		if r.Event == eventKeepFailed {
			return errors.New("append injected")
		}
		return nil
	}
	calls := f.s.count()
	code, _, errOut := f.run(h, "--daily-cap", "1")
	if code != 1 || !strings.Contains(errOut, "keep injected") || !strings.Contains(errOut, "append injected") || f.s.count() == calls {
		t.Fatalf("the retry whose result and keep_failed line cannot be written: %d %s (calls %d)", code, errOut, f.s.count())
	}
	after := f.ledger()
	if len(after) != len(before)+1 || after[len(after)-1].Event != "started" || runsOn(after, "2026-10-05") != 1 {
		t.Fatalf("the attempt that called the model left no started line that counts toward the cap: %+v", after[len(before):])
	}
	f.keep, f.fault = nil, nil // the fault is gone
	calls = f.s.count()
	if code, sum, _ := f.run(other, "--daily-cap", "1"); code != 3 || sum.Outcome != OutcomeDailyCap || f.s.count() != calls {
		t.Fatalf("a second call the same day must be refused by the cap: %d %+v (calls %d)", code, sum, f.s.count())
	}
	if code, sum, _ := f.run(h, "--daily-cap", "1"); code != 0 || sum.Outcome != OutcomeAlreadyReviewed || sum.Status != string(review.StatusUnavailable) || f.s.count() != calls {
		t.Fatalf("the patch whose attempt is spent: %d %+v (calls %d)", code, sum, f.s.count())
	}
}

// When the keep_failed line cannot be appended, the attempt is spent as the ledger stands, and the error says so and gives the line that leaves the attempt to come instead of only telling the
// operator to run the same command again; appending that line after the attempt's started line, as the error says, makes the next run the one more attempt.
func TestRetryWhoseLedgerTakesNoChangeAtAllNamesTheLineThatLeavesTheOneMoreAttempt(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes a read-only file")
	}
	f := newFixture(t)
	h := f.repo.change(f.base, 2)
	f.on("2026-10-04", quotaResult)
	f.run(h)
	f.on("2026-10-05", okResult)
	path := filepath.Join(f.state, "ledger.jsonl")
	f.keep = func(string, []byte) error {
		if err := os.Chmod(path, 0o400); err != nil { // from here on the ledger takes no append and no truncation
			t.Error(err)
		}
		return errors.New("keep injected")
	}
	code, _, errOut := f.run(h)
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	f.keep = nil
	if code != 1 || !strings.Contains(errOut, "keep injected") || !strings.Contains(errOut, "spent") || strings.Contains(errOut, "so run the same command again") {
		t.Fatalf("the retry whose ledger takes no change: %d %s", code, errOut)
	}
	// As the ledger stands, the attempt is spent.
	calls := f.s.count()
	if _, sum, _ := f.run(h); sum.Outcome != OutcomeAlreadyReviewed || sum.Status != string(review.StatusUnavailable) || f.s.count() != calls {
		t.Fatalf("the bare started line must spend the attempt: %+v", sum)
	}
	// The recovery the error gives: the keep_failed line, appended right after the attempt's started line.
	var line string
	for _, l := range strings.Split(errOut, "\n") {
		if l = strings.TrimSpace(l); strings.HasPrefix(l, `{"time"`) && strings.Contains(l, `"event":"`+eventKeepFailed+`"`) {
			line = l
		}
	}
	if line == "" {
		t.Fatalf("the error gives no keep_failed line to append: %s", errOut)
	}
	recs := f.ledger()
	if last := recs[len(recs)-1]; last.Event != "started" || !strings.Contains(errOut, last.Time) {
		t.Fatalf("the last line is not the attempt's started line the error names: %+v", last)
	}
	ledgerFile, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ledgerFile.WriteString(line + "\n"); err != nil {
		t.Fatal(err)
	}
	if err := ledgerFile.Close(); err != nil {
		t.Fatal(err)
	}
	calls = f.s.count()
	if code, again, errOut := f.run(h); code != 0 || again.Outcome != OutcomeReviewed || again.Status != "complete" || f.s.count() == calls {
		t.Fatalf("the run after the recovery: %d %+v %s (ledger %+v)", code, again, errOut, f.ledger())
	}
}

// The output path's identity is the physical path: a symlinked output directory and the directory it points to are one history. The ledger records the path with its directory resolved, and ownership and
// the known results of a path are matched on resolved directories, so P and Q alternating on one head through /alias and /real still restore the newest result, and each patch's post accepts the other's bytes
// on the path as a result the ledger assigned to it.
func TestSharedPathHistoryIsOneAcrossSymlinkAliasesOfTheOutputDirectory(t *testing.T) {
	f := newFixture(t)
	f.forge = &scriptedForge{}
	h2 := f.repo.change(f.base, 2)
	f.repo.git("checkout", "-q", "--detach", h2)
	h3 := f.repo.commit(map[string]string{"a.go": "package a\n\nfunc F() int { return 3 }\n"})
	baseP, baseQ := f.base, h2
	parent := filepath.Dir(f.out)
	realDir, alias := filepath.Join(parent, "real"), filepath.Join(parent, "alias")
	if err := errors.Join(os.MkdirAll(realDir, 0o755), os.Symlink(realDir, alias)); err != nil {
		t.Fatal(err)
	}
	f.base = baseP
	_, p, errOut := f.run(h3, "--out", realDir)
	if p.Outcome != OutcomeReviewed {
		t.Fatalf("P: %+v %s", p, errOut)
	}
	path := filepath.Join(realDir, h3+".json")
	pBytes, _ := os.ReadFile(path)
	remove := func() {
		t.Helper()
		if err := errors.Join(os.Remove(path), os.Remove(path+".sha256")); err != nil {
			t.Fatal(err)
		}
	}
	remove()
	f.base = baseQ
	_, q, errOut := f.run(h3, "--out", alias)
	if q.Outcome != OutcomeReviewed || q.SHA256 == p.SHA256 || q.Artifact != p.Artifact || q.Artifact != path {
		t.Fatalf("Q through the alias: %+v (P %q) %s", q, p.Artifact, errOut)
	}
	remove()
	holds := func(step, want string) {
		t.Helper()
		data, err := os.ReadFile(path)
		if got := fmt.Sprintf("%x", sha256.Sum256(data)); err != nil || got != want {
			t.Fatalf("%s: the path holds %s (%v), want %s", step, got, err, want)
		}
	}
	// P asks about the missing path through /real: the ledger's last assignment of that physical path is Q's.
	f.base = baseP
	if _, again, errOut := f.run(h3, "--out", realDir); again.Outcome != OutcomeAlreadyReviewed || len(again.Restored) != 2 {
		t.Fatalf("P asks: %+v %s", again, errOut)
	}
	holds("P asked", q.SHA256)
	// P's bytes on the path (an earlier restore left them): Q's post, through the alias, still recognises them as a result of the ledger for this path and posts Q's own kept result; P's post through /real
	// posts P's.
	plantP := func() {
		t.Helper()
		if err := errors.Join(os.WriteFile(path, pBytes, 0o644), os.WriteFile(path+".sha256", []byte(p.SHA256+"  "+h3+".json\n"), 0o644)); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range []struct{ name, base, out, sha string }{{"Q via alias", baseQ, alias, q.SHA256}, {"P via real", baseP, realDir, p.SHA256}} {
		plantP()
		f.base = c.base
		code, sum, errOut := f.run(h3, "--post-only", "--pr", "7", "--out", c.out)
		if code != 0 || sum.Comment == nil || sum.SHA256 != c.sha {
			t.Fatalf("%s post-only: %d %+v %s", c.name, code, sum, errOut)
		}
		holds(c.name, q.SHA256) // the restore put the ledger's owner of the path back
	}
	if f.s.count() != 4 {
		t.Fatalf("a restore or a post called the model: %d calls", f.s.count())
	}
}

// A ledger written before the output directory was resolved holds the path as the operator spelled it; ownership and known results are matched on the resolved directory, so those lines still count.
func TestOwnerOfMatchesRecordsOfAnAliasSpelling(t *testing.T) {
	parent, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	realDir, alias := filepath.Join(parent, "real"), filepath.Join(parent, "alias")
	if err := errors.Join(os.MkdirAll(realDir, 0o755), os.Symlink(realDir, alias)); err != nil {
		t.Fatal(err)
	}
	recs := []record{
		{Event: "finished", PatchID: "p", Artifact: filepath.Join(realDir, "h.json"), SHA256: "aa"},
		{Event: "finished", PatchID: "q", Artifact: filepath.Join(alias, "h.json"), SHA256: "bb"},
	}
	if o := ownerOf(recs, filepath.Join(realDir, "h.json")); o == nil || o.SHA256 != "bb" {
		t.Fatalf("owner through /real: %+v", o)
	}
	if o := ownerOf(recs, filepath.Join(alias, "h.json")); o == nil || o.SHA256 != "bb" {
		t.Fatalf("owner through the alias: %+v", o)
	}
	if !assignedTo(recs, filepath.Join(realDir, "h.json"), "bb") || !assignedTo(recs, filepath.Join(alias, "h.json"), "aa") {
		t.Fatal("assignedTo ignores the other spelling")
	}
	missing := filepath.Join(alias, "later", "h.json") // a directory that does not exist yet resolves through its existing ancestor
	if !samePath(missing, filepath.Join(realDir, "later", "h.json")) {
		t.Fatal("a path below a missing directory is not resolved through its existing ancestor")
	}
}
