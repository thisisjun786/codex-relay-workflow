package integrator

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/tracking/trackingfake"
)

// CRW-1507: pushedRun reads the run files of an entry newest first. A pushing marker that git cannot answer for (an unreadable commit
// object) holds the entry only when no NEWER valid evidence exists: an older unreadable marker behind a newer pushed summary or a
// newer marker in the branch is never asked.

const (
	olderMarkerTag = "20260101T000000Z-0"
	newerMarkerTag = "20990101T000000Z-0"
)

// writeMarker writes the summary of a run for the entry the first pass merged: a pushing marker naming head under tag.
func writeMarker(t *testing.T, w *prWorld, tag, head string, first Report) Summary {
	t.Helper()
	made := Summary{Tag: tag, Head: head, Merged: first.Summary.Merged, Record: filepath.Join(w.cfg.RunsDir, tag+".json"), Result: "pushing"}
	data, err := json.Marshal(made)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(w.cfg.RunsDir, tag+".summary.json"), append(data, '\n'), 0o664); err != nil {
		t.Fatal(err)
	}
	return made
}

// newerThan is a run tag that sorts after the tag of the run the first pass made (a clock tag that does not start with a year).
func newerThan(first Report) string { return first.Summary.Tag + "-1" }

// pushedEntryWorld is a world whose first pass pushed the pull request and then "crashed" before the record: the entry is pending, the
// pull request is merged, the run's own pushed summary is on disk. It answers the world, the first report and the dev commit.
func pushedEntryWorld(t *testing.T) (w *prWorld, first Report, dev string) {
	t.Helper()
	w = newPRWorld(t, true)
	pr := w.milestone("10")
	w.enqueue("o/r#1")
	first = w.pass()
	requireExit(t, first, ExitPushed, "pushed")
	dev = w.remoteDev()
	w.crashAfterPush(first, pr.HeadSHA)
	w.fake.SetPull(1, func(p *trackingfake.Pull) { p.State, p.Merged = "closed", true })
	return w, first, dev
}

// removeRunFiles deletes the files of the run the first pass made.
func removeRunFiles(w *prWorld, first Report) {
	for _, ext := range []string{".summary.json", ".out", ".json", ".review.json"} {
		_ = os.Remove(filepath.Join(w.cfg.RunsDir, first.Summary.Tag+ext))
	}
}

// TestAnUnreadableOlderMarkerDoesNotHideALaterPushedSummary: an older pushing marker names an unreadable commit object, the run's own
// pushed summary is newer. The landing is recorded from the newer run; the older marker is never asked.
func TestAnUnreadableOlderMarkerDoesNotHideALaterPushedSummary(t *testing.T) {
	w, first, dev := pushedEntryWorld(t)
	bad, restore := emptiedDanglingCommit(t, w)
	defer restore()
	writeMarker(t, w, olderMarkerTag, bad, first)
	head := w.pendingHead("pr-1")

	rep := w.pass()
	if len(rep.Held) != 0 {
		t.Fatalf("held %q", rep.Held)
	}
	ls := w.landings()
	if len(ls) != 1 {
		t.Fatalf("landing records %+v", ls)
	}
	l := ls[0]
	if l.Result != "landed_before_record" || l.DevAfter != dev || l.Verification.Record != filepath.Join(w.cfg.RunsDir, first.Summary.Tag+".json") ||
		strings.Contains(l.Verification.Note, "not recorded") || l.Review.Verdict != "accept" {
		t.Errorf("the recovered record %+v", l)
	}
	if w.pendingHead("pr-1") != "" || !w.doneFor("pr-1", head) {
		t.Errorf("the entry is not done (pending %q)", w.pendingHead("pr-1"))
	}
}

// TestAnUnreadableOlderMarkerDoesNotHideALaterMarkerInTheBranch: the run's files are gone; an older pushing marker names an unreadable
// commit object, a newer one names the pushed head, which is in the branch. The landing is recorded with the newer marker's run.
func TestAnUnreadableOlderMarkerDoesNotHideALaterMarkerInTheBranch(t *testing.T) {
	w, first, dev := pushedEntryWorld(t)
	removeRunFiles(w, first)
	bad, restore := emptiedDanglingCommit(t, w)
	defer restore()
	writeMarker(t, w, olderMarkerTag, bad, first)
	newer := writeMarker(t, w, newerMarkerTag, dev, first)

	rep := w.pass()
	if len(rep.Held) != 0 {
		t.Fatalf("held %q", rep.Held)
	}
	ls := w.landings()
	if len(ls) != 1 {
		t.Fatalf("landing records %+v", ls)
	}
	l := ls[0]
	if l.Result != "landed_before_record" || l.DevAfter != dev || l.Verification.Record != newer.Record ||
		strings.Contains(l.Verification.Note, "not recorded: no run file") {
		t.Errorf("the recovered record %+v, want the evidence of run %s", l, newer.Tag)
	}
	if w.pendingHead("pr-1") != "" {
		t.Errorf("the entry is still pending (%q)", w.pendingHead("pr-1"))
	}
}

// TestAnUnreadableNewerMarkerHoldsOverAnOlderPushedSummary: the unreadable marker is the NEWER evidence, so it may be the real
// highest-tag run: the entry is held, no landing, the marker stays. (Unchanged by newest-first.)
func TestAnUnreadableNewerMarkerHoldsOverAnOlderPushedSummary(t *testing.T) {
	w, first, _ := pushedEntryWorld(t)
	bad, restore := emptiedDanglingCommit(t, w)
	defer restore()
	made := writeMarker(t, w, newerThan(first), bad, first)

	rep := w.pass()
	want := "whether the pushing marker of run " + made.Tag + " (head " + made.Head + ") is in " + w.cfg.Branch + " could not be read"
	if len(rep.Held) != 1 || !strings.Contains(rep.Held[0], want) || rep.Landing != nil {
		t.Errorf("held %q, landing %+v, want the text %q", rep.Held, rep.Landing, want)
	}
	if ls := w.landings(); len(ls) != 0 {
		t.Errorf("landing records %+v", ls)
	}
	if w.pendingHead("pr-1") == "" {
		t.Errorf("the entry left pending")
	}
	if _, err := os.Stat(filepath.Join(w.cfg.RunsDir, made.Tag+".summary.json")); err != nil {
		t.Errorf("the marker did not stay: %v", err)
	}
}

// TestAMarkerNotInTheBranchIsSkippedForOlderEvidence: a newer marker whose head the checkout lacks names no pushed run, so the older
// pushed summary is the evidence. (Unchanged by newest-first.)
func TestAMarkerNotInTheBranchIsSkippedForOlderEvidence(t *testing.T) {
	w, first, dev := pushedEntryWorld(t)
	writeMarker(t, w, newerThan(first), strings.Repeat("ab", 20), first)

	rep := w.pass()
	if len(rep.Held) != 0 {
		t.Fatalf("held %q", rep.Held)
	}
	ls := w.landings()
	if len(ls) != 1 || ls[0].DevAfter != dev || ls[0].Verification.Record != filepath.Join(w.cfg.RunsDir, first.Summary.Tag+".json") ||
		strings.Contains(ls[0].Verification.Note, "not recorded") {
		t.Fatalf("landing records %+v, want the evidence of run %s", ls, first.Summary.Tag)
	}
}
