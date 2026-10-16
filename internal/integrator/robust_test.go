package integrator

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// execStub replaces the world's program runner for the commands whose first word ends with
// suffix; every other command runs for real.
func (w *world) execStub(suffix string, stub func(ctx context.Context, c Cmd) (int, error)) {
	delegate := w.r.Exec
	w.r.Exec = func(ctx context.Context, c Cmd) (int, error) {
		if len(c.Argv) > 0 && strings.HasSuffix(c.Argv[0], suffix) {
			return stub(ctx, c)
		}
		return delegate(ctx, c)
	}
}

func (w *world) requireAllPending(nums ...string) {
	w.t.Helper()
	for _, n := range nums {
		w.mustWhere(n, "pending")
	}
}

// A signal (watch turns SIGINT and SIGTERM into a cancelled context) that arrives while a gate
// runs is not a verdict: the head under the gate and every head behind it stay pending.
func TestPassCancelledDuringGateKeepsEveryHeadPending(t *testing.T) {
	w := newWorld(t)
	w.pend("1", w.head("a", map[string]string{"a.txt": "a\n"}, true))
	w.pend("2", w.head("b", map[string]string{"b.txt": "b\n"}, true))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w.execStub("gate.sh", func(ctx context.Context, c Cmd) (int, error) {
		cancel()
		return -1, ctx.Err()
	})
	base := w.remoteDev()
	rep := w.r.Pass(ctx)
	requireExit(t, rep, ExitUsage, "cancelled")
	w.requireAllPending("1", "2")
	if w.remoteDev() != base || len(rep.Moved) != 0 {
		t.Fatalf("a cancelled pass pushed or moved something: %+v", rep)
	}
}

// A verdict that was reached before the signal stays: only the heads not yet judged are kept.
func TestPassCancelledAfterAVerdictKeepsThatVerdictOnly(t *testing.T) {
	w := newWorld(t)
	w.pend("1", w.head("a", map[string]string{"a.txt": "a\n"}, true))
	w.pend("2", w.head("b", map[string]string{"b.txt": "b\n"}, true))
	w.ctlWrite("block-CRW-1", "")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	delegate := w.r.Exec
	w.r.Exec = func(ctx context.Context, c Cmd) (int, error) {
		rc, err := delegate(ctx, c)
		if len(c.Argv) > 1 && strings.HasSuffix(c.Argv[0], "gate.sh") && c.Argv[1] == "CRW-2" {
			cancel()
			return -1, ctx.Err()
		}
		return rc, err
	}
	rep := w.r.Pass(ctx)
	requireExit(t, rep, ExitUsage, "cancelled")
	w.mustWhere("1", "blocked")
	w.mustWhere("2", "pending")
}

// A gate that could not run (not a negative verdict) does not block the head either.
func TestPassGateThatCannotRunKeepsTheHeadPending(t *testing.T) {
	w := newWorld(t)
	w.pend("1", w.head("a", map[string]string{"a.txt": "a\n"}, true))
	w.execStub("gate.sh", func(context.Context, Cmd) (int, error) {
		return -1, errors.New("fork: resource temporarily unavailable")
	})
	rep := w.pass()
	requireExit(t, rep, ExitUsage, "error")
	w.requireAllPending("1")
	if !strings.Contains(rep.Error, "resource temporarily unavailable") {
		t.Fatalf("error %q", rep.Error)
	}
}

// A signal during the verification is not a failed verification: no retry marker, no solo
// state, no block.
func TestPassCancelledDuringVerifyCountsNoFailure(t *testing.T) {
	w := newWorld(t)
	w.pend("1", w.head("a", map[string]string{"a.txt": "a\n"}, true))
	w.pend("2", w.head("b", map[string]string{"b.txt": "b\n"}, true))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w.execStub("verify.sh", func(ctx context.Context, c Cmd) (int, error) {
		cancel()
		return -1, ctx.Err()
	})
	base := w.remoteDev()
	rep := w.r.Pass(ctx)
	requireExit(t, rep, ExitUsage, "cancelled")
	w.requireAllPending("1", "2")
	if w.remoteDev() != base || rep.Retry != "" || fileExists(filepath.Join(w.cfg.QueueDir, "retried")) || len(w.r.soloOwed()) != 0 {
		t.Fatalf("a cancelled verification was counted: %+v", rep)
	}
}

// A signal during the merges leaves the head that was being merged pending, not in conflict.
func TestPassCancelledDuringMergeKeepsTheHeadPending(t *testing.T) {
	w := newWorld(t)
	w.pend("1", w.head("a", map[string]string{"a.txt": "a\n"}, true))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	delegate := w.r.Exec
	w.r.Exec = func(ctx context.Context, c Cmd) (int, error) {
		for _, a := range c.Argv {
			if a == "--is-ancestor" {
				cancel()
				return -1, ctx.Err()
			}
		}
		return delegate(ctx, c)
	}
	rep := w.r.Pass(ctx)
	requireExit(t, rep, ExitUsage, "cancelled")
	w.requireAllPending("1")
}

// The verify command runs in the private worktree whatever Git routing the process
// inherited: GIT_DIR and GIT_WORK_TREE of the shared checkout must not reach it. The retired
// version recorder is never invoked at all (CRW-1582).
func TestPassHooksDoNotInheritGitRouting(t *testing.T) {
	w := newWorld(t)
	w.pend("1", w.head("a", map[string]string{"plugins/crw/skills/a.txt": "a\n"}, true))
	w.r.Env = append(append([]string(nil), w.env...), "GIT_DIR="+filepath.Join(w.checkout, ".git"), "GIT_WORK_TREE="+w.checkout, "GIT_INDEX_FILE="+filepath.Join(w.checkout, ".git", "index"))
	before := w.git(w.checkout, "status", "--porcelain")
	w.write(filepath.Join(w.root, "scripts", "record.sh"), `#!/bin/sh
git rev-parse --show-toplevel > "$CTL_DIR/record-top" || exit 1
`)
	w.write(filepath.Join(w.root, "scripts", "verify.sh"), `#!/bin/sh
git rev-parse --show-toplevel > "$CTL_DIR/verify-top" || exit 1
echo '{"ok":true}' > "$CRW_VERIFY_RECORD"
`)
	rep := w.pass()
	requireExit(t, rep, ExitPushed, "pushed")
	top := strings.TrimSpace(w.ctlRead("verify-top"))
	if !strings.HasPrefix(top, w.cfg.WorkRoot+string(filepath.Separator)+"run-") {
		t.Fatalf("verify-top ran against %q, not the private worktree", top)
	}
	if w.ctlRead("record-top") != "" {
		t.Fatalf("the retired version recorder ran: %q", w.ctlRead("record-top"))
	}
	if after := w.git(w.checkout, "status", "--porcelain"); after != before {
		t.Fatalf("the shared checkout changed:\n%s", after)
	}
	if got := w.fileAt("dev", manifestPath); got != strings.TrimSpace(manifest("0.4.0")) {
		t.Fatalf("the pushed manifest is %q, want the fixed release", got)
	}
}

// A populated legacy record_version_command/record_version_env is accepted for compatibility
// and ignored: the command would write a sentinel, and the sentinel is never written
// (CRW-1582).
func TestRecordVersionConfigIsAcceptedAndNeverInvoked(t *testing.T) {
	w := newWorld(t)
	w.pend("1", w.head("a", map[string]string{"plugins/crw/skills/a.txt": "a\n"}, true))
	w.write(filepath.Join(w.root, "scripts", "record.sh"), "#!/bin/sh\necho ran > \"$CTL_DIR/record-env\"\n")
	w.r.Cfg.RecordVersionEnv = map[string]string{"EXTRA": "{tag}"}
	rep := w.pass()
	requireExit(t, rep, ExitPushed, "pushed")
	if got := w.ctlRead("record-env"); got != "" {
		t.Fatalf("the retired version recorder ran: %q", got)
	}
}

// Solo mode owes every head of the failed batch one run of its own, however often an earlier
// member fails alone: batching resumes only after each member has had its run.
func TestPassSoloAttemptsEveryMemberBeforeBatchingAgain(t *testing.T) {
	w := newWorld(t)
	heads := map[string]string{}
	for i, n := range []string{"1", "2", "3", "4"} {
		name := string(rune('a' + i))
		heads[n] = w.head(name, map[string]string{name + ".txt": name + "\n"}, true)
		w.pend(n, heads[n])
	}
	// The batch fails twice; head 1 fails alone twice (blocked); head 2 fails alone twice
	// (blocked); then everything passes.
	w.ctlWrite("verify-rc", "3\n3\n3\n3\n3\n3\n0\n0\n0\n")
	for i := 0; i < 2; i++ {
		requireExit(t, w.pass(), ExitVerifyFailed, "verification-failed(rc=3)")
	}
	for i := 0; i < 4; i++ {
		requireExit(t, w.pass(), ExitVerifyFailed, "verification-failed(rc=3)")
	}
	w.mustWhere("1", "blocked")
	w.mustWhere("2", "blocked")
	w.requireAllPending("3", "4")

	rep := w.pass()
	requireExit(t, rep, ExitPushed, "pushed")
	if rep.Summary.Merged != "crw-3="+heads["3"] {
		t.Fatalf("head 3 was not run alone: merged %q", rep.Summary.Merged)
	}
	w.mustWhere("4", "pending")
	rep = w.pass()
	requireExit(t, rep, ExitPushed, "pushed")
	if rep.Summary.Merged != "crw-4="+heads["4"] {
		t.Fatalf("head 4 was not run alone: merged %q", rep.Summary.Merged)
	}
	if len(w.r.soloOwed()) != 0 {
		t.Fatalf("solo state left behind: %v", w.r.soloOwed())
	}
}

// A solo-owed head the coordinator refreshed or removed is no longer owed; the solo state
// never outlives the heads it names.
func TestPassSoloStateDropsHeadsThatLeftThePending(t *testing.T) {
	w := newWorld(t)
	a := w.head("a", map[string]string{"a.txt": "a\n"}, true)
	b := w.head("b", map[string]string{"b.txt": "b\n"}, true)
	w.pend("1", a)
	w.pend("2", b)
	w.ctlWrite("verify-rc", "3\n3\n")
	w.pass()
	w.pass()
	if len(w.r.soloOwed()) != 2 {
		t.Fatalf("owed %v", w.r.soloOwed())
	}
	if err := os.Remove(filepath.Join(w.cfg.QueueDir, "pending", "1")); err != nil {
		t.Fatal(err)
	}
	c := w.head("c", map[string]string{"c.txt": "c\n"}, true)
	w.pend("2", c)
	rep := w.pass()
	requireExit(t, rep, ExitPushed, "pushed")
	if len(w.r.soloOwed()) != 0 {
		t.Fatalf("owed %v after both owed heads left", w.r.soloOwed())
	}
}

// No commit but the merge happens: a pre-commit hook that would refuse a commit is irrelevant
// now that nothing re-derives a version line, and the plugin change lands on the fixed release
// (CRW-1582).
func TestPassNoCommitBeyondTheMerge(t *testing.T) {
	w := newWorld(t)
	w.pend("1", w.head("a", map[string]string{"plugins/crw/skills/a.txt": "a\n"}, true))
	w.write(filepath.Join(w.checkout, ".git", "hooks", "pre-commit"), "#!/bin/sh\necho refused >&2\nexit 1\n")
	rep := w.pass()
	requireExit(t, rep, ExitPushed, "pushed")
	w.mustWhere("1", "done")
	if got := w.fileAt("dev", manifestPath); got != strings.TrimSpace(manifest("0.4.0")) {
		t.Fatalf("remote manifest %s", got)
	}
	if calls := w.ctlRead("record-calls"); calls != "" {
		t.Fatalf("the version recorder ran:\n%s", calls)
	}
}

// A change the record command leaves beside the manifest is not part of the pushed commit, so
// the run does not verify that tree: status 2, nothing pushed.
func TestPassDirtyWorktreeBeforeVerifyPushesNothing(t *testing.T) {
	w := newWorld(t)
	w.pend("1", w.head("a", map[string]string{"plugins/crw/skills/a.txt": "a\n"}, true))
	delegate := w.r.Exec
	w.r.Exec = func(ctx context.Context, c Cmd) (int, error) {
		rc, err := delegate(ctx, c)
		if err == nil && rc == 0 && len(c.Argv) > 3 && c.Argv[3] == "merge" {
			if werr := os.WriteFile(filepath.Join(c.Argv[2], "README.md"), []byte("stray\n"), 0o644); werr != nil {
				return 1, werr
			}
		}
		return rc, err
	}
	base := w.remoteDev()
	rep := w.pass()
	requireExit(t, rep, ExitUsage, "error")
	if !strings.Contains(rep.Error, "README.md") {
		t.Fatalf("error %q does not name the stray change", rep.Error)
	}
	w.mustWhere("1", "pending")
	if w.remoteDev() != base || w.ctlRead("verify-calls") != "" {
		t.Fatal("a dirty worktree was verified or pushed")
	}
}

// A signal after a verification that passed, during the second fetch or during the push, is
// still a cancelled pass (status 2), not dev-moved or push-failed (status 4).
func TestPassCancelledAfterVerificationIsCancelled(t *testing.T) {
	for name, at := range map[string]func(c Cmd, fetches *int) bool{
		"after verify passed": func(c Cmd, _ *int) bool { return strings.HasSuffix(c.Argv[0], "verify.sh") },
		"second fetch": func(c Cmd, fetches *int) bool {
			if slices.Contains(c.Argv, "fetch") {
				*fetches++
				return *fetches == 2
			}
			return false
		},
		"push": func(c Cmd, _ *int) bool { return slices.Contains(c.Argv, "push") },
	} {
		t.Run(name, func(t *testing.T) {
			w := newWorld(t)
			w.pend("1", w.head("a", map[string]string{"a.txt": "a\n"}, true))
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			fetches := 0
			delegate := w.r.Exec
			w.r.Exec = func(ctx context.Context, c Cmd) (int, error) {
				if !at(c, &fetches) {
					return delegate(ctx, c)
				}
				if strings.HasSuffix(c.Argv[0], "verify.sh") {
					// The verification passes; the signal arrives as it returns.
					rc, err := delegate(ctx, c)
					cancel()
					return rc, err
				}
				cancel()
				return -1, ctx.Err()
			}
			base := w.remoteDev()
			rep := w.r.Pass(ctx)
			requireExit(t, rep, ExitUsage, "cancelled")
			w.mustWhere("1", "pending")
			if w.remoteDev() != base || rep.Retry != "" {
				t.Fatalf("a cancelled pass pushed or counted something: %+v", rep)
			}
		})
	}
}

// Two passes in the same second keep their own evidence: the second run's tag differs, so the
// first run's summary and verification log are not overwritten.
func TestPassSameSecondRunsKeepTheirOwnEvidence(t *testing.T) {
	w := newWorld(t)
	w.pend("1", w.head("a", map[string]string{"a.txt": "a\n"}, true))
	w.ctlWrite("verify-rc", "3\n0\n")
	first := w.r.Pass(context.Background())
	requireExit(t, first, ExitVerifyFailed, "verification-failed(rc=3)")
	second := w.r.Pass(context.Background())
	requireExit(t, second, ExitPushed, "pushed")
	if first.Summary.Tag == second.Summary.Tag || first.Summary.Record == second.Summary.Record {
		t.Fatalf("both runs used tag %q", first.Summary.Tag)
	}
	data, err := os.ReadFile(filepath.Join(w.cfg.RunsDir, first.Summary.Tag+".summary.json"))
	if err != nil || !strings.Contains(string(data), `"result":"verification-failed(rc=3)"`) {
		t.Fatalf("the first run's summary was lost: %s %v", data, err)
	}
	for _, s := range []*Summary{first.Summary, second.Summary} {
		if _, err := os.Stat(filepath.Join(w.cfg.RunsDir, s.Tag+".verify.log")); err != nil {
			t.Fatal(err)
		}
	}
}

// A temporary directory the run did not create is not the run's to delete: a retained
// work_root/tmp-<tag> survives, and the run ends with status 2 before it touches anything.
func TestPassDoesNotDeleteATemporaryDirectoryItDidNotCreate(t *testing.T) {
	w := newWorld(t)
	w.pend("1", w.head("a", map[string]string{"plugins/crw/skills/a.txt": "a\n"}, true))
	tag := w.cfg.TagPrefix + w.now.Add(time.Minute).Format("0102150405")
	kept := filepath.Join(w.cfg.WorkRoot, "tmp-"+tag)
	if err := os.MkdirAll(kept, 0o755); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(kept, "retained.txt")
	if err := os.WriteFile(sentinel, []byte("keep\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	base := w.remoteDev()
	rep := w.pass()
	requireExit(t, rep, ExitUsage, "error")
	if !strings.Contains(rep.Error, "tmp-") {
		t.Fatalf("error %q does not name the temporary directory", rep.Error)
	}
	if _, err := os.Stat(sentinel); err != nil {
		t.Fatalf("a retained file in a directory the run did not create is gone: %v", err)
	}
	w.mustWhere("1", "pending")
	if w.remoteDev() != base || w.ctlRead("verify-calls") != "" {
		t.Fatal("a run that could not own its temporary directory verified or pushed")
	}
}

// A run's own temporary directory is removed with it, a record run or not.
func TestPassRemovesItsOwnTemporaryDirectory(t *testing.T) {
	w := newWorld(t)
	w.pend("1", w.head("a", map[string]string{"plugins/crw/skills/a.txt": "a\n"}, true))
	requireExit(t, w.pass(), ExitPushed, "pushed")
	left, _ := filepath.Glob(filepath.Join(w.cfg.WorkRoot, "tmp-*"))
	if len(left) != 0 {
		t.Fatalf("temporary directories left: %v", left)
	}
}

// What is verified is what is pushed, also after the verification: a verifier that repairs
// the source, adds a file or commits ends the run with status 2 and nothing is pushed, though
// it exits 0.
func TestPassVerifierThatChangesTheTreeOrHeadPushesNothing(t *testing.T) {
	for name, script := range map[string]string{
		"edits a tracked file": "echo repaired >> README.md\n",
		"adds a file":          "echo new > generated.txt\n",
		"stages a change":      "echo staged >> README.md; git add README.md\n",
		"commits":              "echo c > c2.txt; git add c2.txt; git commit -q -m verifier-commit\n",
		"amends the commit":    "git commit -q --amend -m amended\n",
	} {
		t.Run(name, func(t *testing.T) {
			w := newWorld(t)
			w.pend("1", w.head("a", map[string]string{"plugins/crw/skills/a.txt": "a\n"}, true))
			w.ctlWrite("during-verify.sh", script)
			base := w.remoteDev()
			rep := w.pass()
			requireExit(t, rep, ExitUsage, "error")
			if !strings.Contains(rep.Error, "verification") {
				t.Fatalf("error %q does not say the verification changed the tree", rep.Error)
			}
			w.mustWhere("1", "pending")
			if w.remoteDev() != base {
				t.Fatalf("pushed a commit the verification did not judge: %s", w.git(w.remote, "log", "--format=%s", "-3", "dev"))
			}
		})
	}
}

// The refresh protection holds across the check: a coordinator that replaces the pending file
// after the run read it (a new file by rename, the way a new head is written) keeps its new
// file in pending, and the processed head is the one that moved.
func TestMoveEntryKeepsAFileReplacedBetweenCheckAndMove(t *testing.T) {
	w := newWorld(t)
	a := w.head("a", map[string]string{"a.txt": "a\n"}, true)
	newer := w.head("a2", map[string]string{"a2.txt": "a2\n"}, true)
	w.pend("1", a)
	pending := filepath.Join(w.cfg.QueueDir, "pending", "1")
	fired := false
	w.r.hookMove = func(string) {
		if fired {
			return
		}
		fired = true
		tmp := pending + ".new"
		w.write(tmp, newer+"\n")
		if err := os.Rename(tmp, pending); err != nil {
			t.Fatal(err)
		}
	}
	requireExit(t, w.pass(), ExitPushed, "pushed")
	if !fired {
		t.Fatal("the seam did not run")
	}
	if got, _ := readHead(pending); got != newer {
		t.Fatalf("pending holds %q, want the refreshed head %s", got, newer)
	}
	if _, err := os.Stat(filepath.Join(w.cfg.QueueDir, "done", "1")); err == nil {
		t.Fatal("the refreshed head was carried into done/")
	}
}

// A refresh written into the very file that is being moved is not carried into done/ either.
func TestMoveEntryKeepsAFileRewrittenInPlaceBetweenCheckAndMove(t *testing.T) {
	w := newWorld(t)
	a := w.head("a", map[string]string{"a.txt": "a\n"}, true)
	newer := w.head("a2", map[string]string{"a2.txt": "a2\n"}, true)
	w.pend("1", a)
	pending := filepath.Join(w.cfg.QueueDir, "pending", "1")
	fired := false
	w.r.hookMove = func(src string) {
		if fired {
			return
		}
		fired = true
		// The writer holds the file path it opened before the move: src is pending/1 in the old
		// order and the moved file in the new one; either way the bytes change in place.
		for _, p := range []string{src, filepath.Join(w.cfg.QueueDir, "done", "1")} {
			if _, err := os.Stat(p); err == nil {
				if err := os.WriteFile(p, []byte(newer+"\n"), 0o664); err != nil {
					t.Fatal(err)
				}
				return
			}
		}
		t.Fatal("no file to rewrite")
	}
	requireExit(t, w.pass(), ExitPushed, "pushed")
	if got, _ := readHead(pending); got != newer {
		t.Fatalf("pending holds %q, want the refreshed head %s", got, newer)
	}
	if w.where("1") != "pending" {
		t.Fatalf("file is in %q", w.where("1"))
	}
	if _, err := os.Stat(filepath.Join(w.cfg.QueueDir, "done", "1")); err == nil {
		t.Fatal("the refreshed head was carried into done/")
	}
}
