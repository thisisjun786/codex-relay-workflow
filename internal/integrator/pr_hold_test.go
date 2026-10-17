package integrator

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// CRW-1413 cross-check fixes: a failure to read the world is a hold, never a verdict. A git that cannot answer whether a pushing
// marker's head is in the branch, and a queue with no tracking adapter that has a landing comment to post, keep the entry pending
// (with the reason in the report) and leave the files as they are for the next pass.

// failAncestorCheck makes the git call "merge-base --is-ancestor <head> ..." fail for head: with status 128 (rc) or with an error
// (err). It answers the number of calls it failed.
func failAncestorCheck(w *prWorld, head string, rc int, err error) *int {
	failed := new(int)
	delegate := w.r.Exec
	w.r.Exec = func(ctx context.Context, c Cmd) (int, error) {
		for i, a := range c.Argv {
			if a == "merge-base" && i+3 < len(c.Argv) && c.Argv[i+1] == "--is-ancestor" && c.Argv[i+2] == head {
				*failed++
				return rc, err
			}
		}
		return delegate(ctx, c)
	}
	return failed
}

// TestAGitThatFailsTheAncestorCheckOfAMarkerHoldsTheEntry: the run that pushed the head left only its pushing marker, and git fails
// when asked whether the marker's head is in the branch. That is not "no run pushed it": the entry stays pending with the reason,
// no landing record is written, and the marker stays; the next pass with a working git records the landing with the run's files.
func TestAGitThatFailsTheAncestorCheckOfAMarkerHoldsTheEntry(t *testing.T) {
	for name, fail := range map[string]struct {
		rc  int
		err error
	}{"status 128": {128, nil}, "error": {-1, errors.New("git: boom")}} {
		t.Run(name, func(t *testing.T) {
			w := newPRWorld(t, true)
			pr := w.milestone("10")
			w.enqueue("o/r#1")
			first := w.pass()
			requireExit(t, first, ExitPushed, "pushed")
			dev := w.remoteDev()
			w.crashAfterPush(first, pr.HeadSHA)
			marker := readSummary(t, w, first.Summary.Tag)
			marker.Result = "pushing"
			data, err := json.Marshal(marker)
			if err != nil {
				t.Fatal(err)
			}
			summaryPath := filepath.Join(w.cfg.RunsDir, marker.Tag+".summary.json")
			if err := os.WriteFile(summaryPath, append(data, '\n'), 0o664); err != nil {
				t.Fatal(err)
			}
			delegate := w.r.Exec
			failed := failAncestorCheck(w, dev, fail.rc, fail.err)

			rep := w.pass()
			if *failed == 0 {
				t.Fatal("the pass never asked git about the marker")
			}
			if ls := w.landings(); len(ls) != 0 {
				t.Errorf("a landing was recorded on a git failure: %+v", ls)
			}
			if w.pendingHead("pr-1") != pr.HeadSHA || w.doneFor("pr-1", pr.HeadSHA) {
				t.Errorf("the entry left pending (pending %q, done %v)", w.pendingHead("pr-1"), w.doneFor("pr-1", pr.HeadSHA))
			}
			if len(rep.Held) != 1 || !strings.HasPrefix(rep.Held[0], "pr-1: ") || !strings.Contains(rep.Held[0], "pushing marker of run "+marker.Tag) || rep.Landing != nil {
				t.Errorf("held %q, landing %+v", rep.Held, rep.Landing)
			}
			if got, err := os.ReadFile(summaryPath); err != nil || !strings.Contains(string(got), `"pushing"`) {
				t.Errorf("the marker did not stay (%v): %s", err, got)
			}
			if n := len(w.fake.Comments(1)); n != 1 {
				t.Errorf("%d comments, want the one of the run", n)
			}

			w.r.Exec = delegate
			w.pass()
			ls := w.landings()
			if len(ls) != 1 {
				t.Fatalf("landing records %+v", ls)
			}
			l := ls[0]
			if l.Result != "landed_before_record" || l.Verification.Record != marker.Record || l.Review.Verdict != "accept" ||
				strings.HasPrefix(l.Verification.Note, "not recorded") || strings.HasPrefix(l.Review.Note, "not recorded") {
				t.Errorf("the retried record %+v", l)
			}
			if !w.doneFor("pr-1", pr.HeadSHA) {
				t.Error("the entry is not done after the retry")
			}
		})
	}
}

// failCommitProbe makes the git call that asks whether the checkout has the commit head ("cat-file -e <head>", the bare sha) fail
// with status rc or with err. It answers the number of calls it failed.
func failCommitProbe(w *prWorld, head string, rc int, err error) *int {
	failed := new(int)
	delegate := w.r.Exec
	w.r.Exec = func(ctx context.Context, c Cmd) (int, error) {
		for i, a := range c.Argv {
			if a == "cat-file" && i+3 == len(c.Argv) && c.Argv[i+1] == "-e" && c.Argv[i+2] == head {
				*failed++
				return rc, err
			}
		}
		return delegate(ctx, c)
	}
	return failed
}

// TestAGitThatFailsTheCommitProbeOfAMarkerHoldsTheEntry: the ancestor check of the marker's head fails with status 128, and the
// probe that tells an absent commit from a repository error fails too (status 128, or an error). A git that cannot say whether the
// checkout has the commit is not "the commit was never pushed": the entry is held, no landing is written, the marker stays, and the
// next pass with a working git records the landing.
func TestAGitThatFailsTheCommitProbeOfAMarkerHoldsTheEntry(t *testing.T) {
	for name, fail := range map[string]struct {
		rc  int
		err error
	}{"status 128": {128, nil}, "error": {-1, errors.New("git: boom")}} {
		t.Run(name, func(t *testing.T) {
			w := newPRWorld(t, true)
			pr := w.milestone("10")
			w.enqueue("o/r#1")
			first := w.pass()
			requireExit(t, first, ExitPushed, "pushed")
			dev := w.remoteDev()
			w.crashAfterPush(first, pr.HeadSHA)
			marker := readSummary(t, w, first.Summary.Tag)
			marker.Result = "pushing"
			data, err := json.Marshal(marker)
			if err != nil {
				t.Fatal(err)
			}
			summaryPath := filepath.Join(w.cfg.RunsDir, marker.Tag+".summary.json")
			if err := os.WriteFile(summaryPath, append(data, '\n'), 0o664); err != nil {
				t.Fatal(err)
			}
			delegate := w.r.Exec
			failedCheck := failAncestorCheck(w, dev, 128, nil)
			failedProbe := failCommitProbe(w, dev, fail.rc, fail.err)

			rep := w.pass()
			if *failedCheck == 0 || *failedProbe == 0 {
				t.Fatalf("the pass did not ask git about the marker (ancestor check %d, commit probe %d)", *failedCheck, *failedProbe)
			}
			if ls := w.landings(); len(ls) != 0 {
				t.Errorf("a landing was recorded on a git failure: %+v", ls)
			}
			if w.pendingHead("pr-1") != pr.HeadSHA || w.doneFor("pr-1", pr.HeadSHA) {
				t.Errorf("the entry left pending (pending %q, done %v)", w.pendingHead("pr-1"), w.doneFor("pr-1", pr.HeadSHA))
			}
			if len(rep.Held) != 1 || !strings.HasPrefix(rep.Held[0], "pr-1: ") || !strings.Contains(rep.Held[0], "pushing marker of run "+marker.Tag) || rep.Landing != nil {
				t.Errorf("held %q, landing %+v", rep.Held, rep.Landing)
			}
			if got, err := os.ReadFile(summaryPath); err != nil || !strings.Contains(string(got), `"pushing"`) {
				t.Errorf("the marker did not stay (%v): %s", err, got)
			}

			w.r.Exec = delegate
			w.pass()
			if ls := w.landings(); len(ls) != 1 || ls[0].Result != "landed_before_record" {
				t.Fatalf("landing records after the retry %+v", ls)
			}
			if !w.doneFor("pr-1", pr.HeadSHA) {
				t.Error("the entry is not done after the retry")
			}
		})
	}
}

// TestAQueueWithoutATrackingAdapterHoldsALandingWhoseCommentIsNotWrittenYet: a record left "not written yet" and a queue whose
// adapter is nil (an empty pull_request_repo) cannot post the comment. The pass does not panic; the entry stays pending with the
// reason and the record is untouched, and the pull request has 0 comments while held (the pass died before the comment call); with
// the adapter back, the next pass posts the comment once and moves the entry to done.
func TestAQueueWithoutATrackingAdapterHoldsALandingWhoseCommentIsNotWrittenYet(t *testing.T) {
	w := newPRWorld(t, true)
	pr := w.milestone("10")
	w.enqueue("o/r#1")
	first := w.pass()
	requireExit(t, first, ExitPushed, "pushed")
	path := w.interruptBeforeComment(first, pr.HeadSHA, "not written yet")
	w.r.PullRequests = nil

	rep := w.pass()
	if len(rep.Held) != 1 || !strings.HasPrefix(rep.Held[0], "pr-1: ") || !strings.Contains(rep.Held[0], "no tracking adapter") {
		t.Errorf("held %q", rep.Held)
	}
	if w.pendingHead("pr-1") != pr.HeadSHA || w.doneFor("pr-1", pr.HeadSHA) {
		t.Errorf("the entry left pending (pending %q, done %v)", w.pendingHead("pr-1"), w.doneFor("pr-1", pr.HeadSHA))
	}
	if l := readLanding(t, path); l.Comment != "not written yet" {
		t.Errorf("the record changed: %+v", l)
	}
	if n := len(w.fake.Comments(1)); n != 0 {
		t.Errorf("%d comments while held, want 0", n)
	}

	w.r.PullRequests = w.gh
	w.pass()
	if c := w.fake.Comments(1); len(c) != 1 || !strings.HasPrefix(c[0], "[dev queue · "+first.Summary.Tag+"]") {
		t.Errorf("comments %q", c)
	}
	if l := readLanding(t, path); l.Comment != "written" {
		t.Errorf("the record after the repost %+v", l)
	}
	if !w.doneFor("pr-1", pr.HeadSHA) {
		t.Error("the entry is not done after the repost")
	}
}

func worktreeAddFailState(t *testing.T, w *prWorld, rep Report, head, base string) {
	t.Helper()
	requireExit(t, rep, ExitUsage, "error")
	if w.remoteDev() != base {
		t.Errorf("remote dev moved: %s != %s", w.remoteDev(), base)
	}
	if w.pendingHead("pr-1") != head || w.doneFor("pr-1", head) {
		t.Errorf("the entry did not stay pending (pending %q, done %v)", w.pendingHead("pr-1"), w.doneFor("pr-1", head))
	}
	if w.ctlRead("verify-n") != "" || w.ctlRead("review-calls") != "" {
		t.Errorf("verification or review ran")
	}
	if n := len(w.fake.Comments(1)); n != 0 {
		t.Errorf("%d comments posted", n)
	}
	if rep.Summary != nil || rep.Landing != nil {
		t.Errorf("summary %+v landing %+v", rep.Summary, rep.Landing)
	}
	tmpDirs, err := filepath.Glob(filepath.Join(w.cfg.WorkRoot, "tmp-*"))
	if err != nil || len(tmpDirs) != 0 {
		t.Errorf("temporary dirs left: %v %v", tmpDirs, err)
	}
}

func soleRunLog(t *testing.T, w *prWorld) []byte {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(w.cfg.RunsDir, "*.log"))
	if err != nil || len(paths) != 1 {
		t.Fatalf("run logs: %v %v", paths, err)
	}
	raw, err := os.ReadFile(paths[0])
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestWorktreeAddFailureIncludesGitStderr(t *testing.T) {
	w := newPRWorld(t, true)
	pr := w.milestone("10")
	w.enqueue("o/r#1")
	base := w.remoteDev()
	delegate := w.r.Exec
	adds := 0
	var wt string
	w.r.Exec = func(ctx context.Context, c Cmd) (int, error) {
		if len(c.Argv) == 8 && c.Argv[3] == "worktree" && c.Argv[4] == "add" {
			adds++
			wt = c.Argv[6]
			if err := os.WriteFile(wt, []byte("fixture-owned collision\n"), 0o600); err != nil {
				t.Fatalf("inject worktree collision: %v", err)
			}
		}
		return delegate(ctx, c)
	}
	rep := w.pass()
	worktreeAddFailState(t, w, rep, pr.HeadSHA, base)
	if adds != 1 {
		t.Fatalf("%d worktree adds", adds)
	}
	if body, err := os.ReadFile(wt); err != nil || string(body) != "fixture-owned collision\n" {
		t.Errorf("the sentinel file did not survive: %q %v", body, err)
	}
	text := strings.TrimSpace(string(soleRunLog(t, w)))
	if text == "" || !strings.Contains(text, wt) {
		t.Fatalf("the run log lost git's rejection: %q", text)
	}
	if !strings.Contains(rep.Error, text) || !strings.Contains(rep.Error, "worktree add failed") || !strings.Contains(rep.Error, "status 128") {
		t.Fatalf("the failure lost git's stderr: %q", rep.Error)
	}
}

func TestWorktreeAddFailureReportsExecutionDetails(t *testing.T) {
	for _, tc := range []struct {
		name, stdout, stderr, wantStatus string
		rc                               int
		err                              error
	}{
		{name: "nonzero exit", rc: 128, stdout: "worktree stdout marker\n", stderr: "fatal: injected worktree stderr\n", wantStatus: "status 128"},
		{name: "execution error", rc: -1, err: errors.New("signal: killed"), stdout: "partial worktree stdout\n", stderr: "worktree process stopped\n", wantStatus: "status -1"},
		{name: "start error", rc: -1, err: errors.New("git executable is unavailable"), wantStatus: "status -1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newPRWorld(t, true)
			pr := w.milestone("10")
			w.enqueue("o/r#1")
			base := w.remoteDev()
			delegate := w.r.Exec
			adds := 0
			w.r.Exec = func(ctx context.Context, c Cmd) (int, error) {
				if len(c.Argv) == 8 && c.Argv[3] == "worktree" && c.Argv[4] == "add" {
					adds++
					if _, err := io.WriteString(c.Stdout, tc.stdout); err != nil {
						return -1, err
					}
					if _, err := io.WriteString(c.Stderr, tc.stderr); err != nil {
						return -1, err
					}
					return tc.rc, tc.err
				}
				return delegate(ctx, c)
			}
			rep := w.pass()
			worktreeAddFailState(t, w, rep, pr.HeadSHA, base)
			if adds != 1 {
				t.Fatalf("%d worktree adds", adds)
			}
			if !strings.Contains(rep.Error, "worktree add failed") || !strings.Contains(rep.Error, tc.wantStatus) {
				t.Fatalf("the failure lost the status: %q", rep.Error)
			}
			if tc.err != nil && !strings.Contains(rep.Error, tc.err.Error()) {
				t.Fatalf("the failure lost the process error: %q", rep.Error)
			}
			for _, s := range []string{tc.stdout, tc.stderr} {
				if s != "" && !strings.Contains(rep.Error, strings.TrimSpace(s)) {
					t.Fatalf("the failure lost %q: %q", s, rep.Error)
				}
			}
			if raw := soleRunLog(t, w); string(raw) != tc.stdout+tc.stderr {
				t.Fatalf("the run log changed: %q", raw)
			}
		})
	}
}
