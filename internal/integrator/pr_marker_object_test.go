package integrator

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/tracking/trackingfake"
)

// CRW-1507: a pushing marker whose head names a commit object the checkout has but cannot read (an empty loose object file) is a
// repository error, not a commit that was never pushed. The probe that tells the two apart asks whether the object exists
// ("git cat-file -e <head>", exit codes only), so the object's existence decides, not whether it can be peeled and read.

// emptiedDanglingCommit makes a commit in the checkout that no ref reaches (a child of the remote branch tip with the tip's tree) and
// truncates its loose object file to zero bytes. The entry head, the tip and the ancestry between them stay readable, so the pass
// reaches the marker. restore puts the original bytes back.
func emptiedDanglingCommit(t *testing.T, w *prWorld) (sha string, restore func()) {
	t.Helper()
	tip := w.git(w.checkout, "rev-parse", "--verify", "refs/remotes/origin/dev")
	sha = w.git(w.checkout, "commit-tree", tip+"^{tree}", "-p", tip, "-m", "dangling")
	path := filepath.Join(w.checkout, ".git", "objects", sha[:2], sha[2:])
	orig, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("the dangling commit %s is not a loose object: %v", sha, err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(path, 0); err != nil {
		t.Fatal(err)
	}
	return sha, func() {
		t.Helper()
		if err := os.WriteFile(path, orig, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// markerOnlyWorld is the TestAMarkerWhosePushNeverLandedIsIgnored shape: the run's own files are gone, one pushing marker names head,
// the pull request is merged. It answers the world, the pushed dev commit and the marker.
func markerOnlyWorld(t *testing.T, head func(w *prWorld) string) (w *prWorld, dev string, made Summary) {
	t.Helper()
	w = newPRWorld(t, true)
	pr := w.milestone("10")
	w.enqueue("o/r#1")
	base := w.remoteDev()
	first := w.pass()
	requireExit(t, first, ExitPushed, "pushed")
	dev = w.remoteDev()
	w.crashAfterPush(first, pr.HeadSHA)
	for _, ext := range []string{".summary.json", ".out", ".json", ".review.json"} {
		_ = os.Remove(filepath.Join(w.cfg.RunsDir, first.Summary.Tag+ext))
	}
	made = Summary{Tag: "20260101T000000Z-0", Base: base, Head: head(w), Merged: first.Summary.Merged, Record: filepath.Join(w.cfg.RunsDir, "20260101T000000Z-0.json"), Result: "pushing"}
	data, err := json.Marshal(made)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(w.cfg.RunsDir, made.Tag+".summary.json"), append(data, '\n'), 0o664); err != nil {
		t.Fatal(err)
	}
	w.fake.SetPull(1, func(p *trackingfake.Pull) { p.State, p.Merged = "closed", true })
	return w, dev, made
}

// TestAnEmptyCommitObjectOfAMarkerHoldsTheEntry: the marker's head is a commit object whose file is empty. The ancestor check fails
// with 128 and the object exists, so the repository cannot answer: the entry is held, no landing is written, the marker stays; after
// the object is back, the marker is read as not in the branch and the landing is recorded without a run.
func TestAnEmptyCommitObjectOfAMarkerHoldsTheEntry(t *testing.T) {
	var restore func()
	w, dev, made := markerOnlyWorld(t, func(w *prWorld) string {
		var sha string
		sha, restore = emptiedDanglingCommit(t, w)
		return sha
	})

	rep := w.pass()
	if ls := w.landings(); len(ls) != 0 {
		t.Errorf("a landing was recorded for an unreadable marker head: %+v", ls)
	}
	want := "whether the pushing marker of run " + made.Tag + " (head " + made.Head + ") is in " + w.cfg.Branch + " could not be read"
	if len(rep.Held) != 1 || !strings.HasPrefix(rep.Held[0], "pr-1: ") || !strings.Contains(rep.Held[0], want) || rep.Landing != nil {
		t.Errorf("held %q, landing %+v, want the text %q", rep.Held, rep.Landing, want)
	}
	if w.pendingHead("pr-1") == "" || w.doneFor("pr-1", w.pendingHead("pr-1")) {
		t.Errorf("the entry left pending (pending %q)", w.pendingHead("pr-1"))
	}
	if got, err := os.ReadFile(filepath.Join(w.cfg.RunsDir, made.Tag+".summary.json")); err != nil || !strings.Contains(string(got), `"pushing"`) {
		t.Errorf("the marker did not stay (%v): %s", err, got)
	}

	restore()
	w.pass()
	ls := w.landings()
	if len(ls) != 1 || ls[0].Result != "landed_before_record" || ls[0].DevAfter != dev || !strings.Contains(ls[0].Verification.Note, "not recorded: no run file") {
		t.Fatalf("landing records after the retry %+v", ls)
	}
}

// TestAMarkerHeadAbsentFromTheCheckoutIsStillAbsentUnderGitTrace: a head the checkout does not have was never pushed, also when
// git writes its trace to standard error (the answer comes from exit codes only).
func TestAMarkerHeadAbsentFromTheCheckoutIsStillAbsentUnderGitTrace(t *testing.T) {
	w, dev, _ := markerOnlyWorld(t, func(*prWorld) string { return strings.Repeat("ab", 20) })
	w.r.Env = append(append([]string(nil), w.env...), "GIT_TRACE=1")

	rep := w.pass()
	if len(rep.Held) != 0 {
		t.Errorf("held %q", rep.Held)
	}
	ls := w.landings()
	if len(ls) != 1 || ls[0].Result != "landed_before_record" || ls[0].DevAfter != dev || !strings.Contains(ls[0].Verification.Note, "not recorded: no run file") {
		t.Fatalf("landing records %+v", ls)
	}
}

// TestTheCommitProbeOfAMarkerIsACatFileExistenceCheck: the probe asks "git cat-file -e <head>" with the bare sha, never
// "<head>^{commit}", which would peel and read the object.
func TestTheCommitProbeOfAMarkerIsACatFileExistenceCheck(t *testing.T) {
	var restore func()
	w, _, made := markerOnlyWorld(t, func(w *prWorld) string {
		var sha string
		sha, restore = emptiedDanglingCommit(t, w)
		return sha
	})
	defer restore()
	var probes, peeled int
	delegate := w.r.Exec
	w.r.Exec = func(ctx context.Context, c Cmd) (int, error) {
		for i, a := range c.Argv {
			if a == made.Head+"^{commit}" {
				peeled++
			}
			if a == "cat-file" && i+2 < len(c.Argv) && c.Argv[i+1] == "-e" && c.Argv[i+2] == made.Head && i+3 == len(c.Argv) {
				probes++
			}
		}
		return delegate(ctx, c)
	}

	w.pass()
	if probes != 1 || peeled != 0 {
		t.Errorf("cat-file -e probes %d, peeling calls %d; want 1 and 0", probes, peeled)
	}
}
