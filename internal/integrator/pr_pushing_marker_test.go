package integrator

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/tracking/trackingfake"
)

// CRW-1413 item 3: a push the signal ended after git exited 0 leaves no "pushed" summary. The run writes a "pushing" marker into
// <tag>.summary.json right before the push; the recovery pass accepts the marker when its head is in the branch, so the run files
// (verification record, review verdict) of that run are still found. The normal run overwrites the marker with "pushed".

// readSummary reads <tag>.summary.json of the runs directory.
func readSummary(t *testing.T, w *prWorld, tag string) Summary {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(w.cfg.RunsDir, tag+".summary.json"))
	if err != nil {
		t.Fatal(err)
	}
	var s Summary
	if err := json.Unmarshal(data, &s); err != nil {
		t.Fatalf("summary %s: %v", data, err)
	}
	return s
}

// onlySummary is the one summary file the runs directory holds.
func onlySummary(t *testing.T, w *prWorld) Summary {
	t.Helper()
	paths, _ := filepath.Glob(filepath.Join(w.cfg.RunsDir, "*.summary.json"))
	if len(paths) != 1 {
		t.Fatalf("summary files %v", paths)
	}
	return readSummary(t, w, strings.TrimSuffix(filepath.Base(paths[0]), ".summary.json"))
}

// TestAPushTheSignalEndedKeepsItsRunFilesForTheRecovery: git pushes the head and exits 0, then the signal ends the call as OSExec
// reports it (-1 and the context error, never the real exit code). The pass is cancelled and the remote holds the head; the next
// pass finds the run's files through the marker.
func TestAPushTheSignalEndedKeepsItsRunFilesForTheRecovery(t *testing.T) {
	w := newPRWorld(t, true)
	pr := w.milestone("10")
	w.enqueue("o/r#1")
	base := w.remoteDev()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	delegate := w.r.Exec
	w.r.Exec = func(ctx context.Context, c Cmd) (int, error) {
		rc, err := delegate(ctx, c)
		isPush, toDev := false, false
		for _, a := range c.Argv {
			isPush = isPush || a == "push"
			toDev = toDev || strings.HasSuffix(a, ":refs/heads/dev")
		}
		if isPush && toDev && rc == 0 && err == nil {
			cancel()
			return -1, ctx.Err()
		}
		return rc, err
	}
	first := w.r.Pass(ctx)
	requireExit(t, first, ExitUsage, "cancelled")
	dev := w.remoteDev()
	if dev == base {
		t.Fatalf("the push did not land")
	}
	w.git(w.remote, "merge-base", "--is-ancestor", pr.HeadSHA, "dev")
	marker := onlySummary(t, w)
	if marker.Result != "pushing" {
		t.Fatalf("the run left %+v, want the pushing marker", marker)
	}
	w.mustWhere("pr-1", "pending")
	w.r.Exec = delegate

	rep := w.pass()
	l := rep.Landing
	if l == nil || l.Result != "landed_before_record" {
		t.Fatalf("the pass reports %+v", rep)
	}
	sum := sha256.Sum256([]byte("{\"ok\":true}\n"))
	if l.Verification.Record != filepath.Join(w.cfg.RunsDir, marker.Tag+".json") || l.Verification.SHA256 != hex.EncodeToString(sum[:]) ||
		l.Verification.Status != 0 || l.Review.Verdict != "accept" || l.DevBefore != base || l.DevAfter != dev {
		t.Errorf("the recovered record %+v", l)
	}
	if strings.HasPrefix(l.Verification.Note, "not recorded") || strings.HasPrefix(l.Review.Note, "not recorded") {
		t.Errorf("the recovered record says not recorded: %+v", l)
	}
	if w.ctlRead("verify-n") != "1\n" || w.remoteDev() != dev {
		t.Errorf("the recovery pass ran again: dev %s, verified %q", w.remoteDev(), w.ctlRead("verify-n"))
	}
}

// TestAMarkerWhosePushNeverLandedIsIgnored: a pushing marker whose head is not in the branch (the push never landed) names no run
// that pushed the entry, so the record says that no run file was found.
func TestAMarkerWhosePushNeverLandedIsIgnored(t *testing.T) {
	w := newPRWorld(t, true)
	pr := w.milestone("10")
	w.enqueue("o/r#1")
	base := w.remoteDev()
	first := w.pass()
	requireExit(t, first, ExitPushed, "pushed")
	dev := w.remoteDev()
	w.crashAfterPush(first, pr.HeadSHA)
	merged := first.Summary.Merged
	for _, ext := range []string{".summary.json", ".out", ".json", ".review.json"} {
		_ = os.Remove(filepath.Join(w.cfg.RunsDir, first.Summary.Tag+ext))
	}
	made := Summary{Tag: "20260101T000000Z-0", Base: base, Head: strings.Repeat("ab", 20), Merged: merged, Record: filepath.Join(w.cfg.RunsDir, "20260101T000000Z-0.json"), Result: "pushing"}
	data, err := json.Marshal(made)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(w.cfg.RunsDir, made.Tag+".summary.json"), append(data, '\n'), 0o664); err != nil {
		t.Fatal(err)
	}
	w.fake.SetPull(1, func(p *trackingfake.Pull) { p.State, p.Merged = "closed", true })

	w.pass()
	ls := w.landings()
	if len(ls) != 1 {
		t.Fatalf("landing records %+v", ls)
	}
	l := ls[0]
	if l.Result != "landed_before_record" || l.DevAfter != dev || l.Verification.Record != "" || l.Verification.Status != -1 ||
		!strings.Contains(l.Verification.Note, "not recorded: no run file") || !strings.Contains(l.Review.Note, "not recorded: no run file") {
		t.Errorf("the recovered record %+v", l)
	}
}

// TestTheNormalRunOverwritesItsPushingMarker: after a pass that pushed, the summary says pushed.
func TestTheNormalRunOverwritesItsPushingMarker(t *testing.T) {
	w := newPRWorld(t, true)
	w.milestone("10")
	w.enqueue("o/r#1")
	first := w.pass()
	requireExit(t, first, ExitPushed, "pushed")
	if s := readSummary(t, w, first.Summary.Tag); s.Result != "pushed" {
		t.Errorf("the summary says %q, want pushed", s.Result)
	}
}
