package integrator

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPassMergesBatchInIssueOrderAndPushesFastForward(t *testing.T) {
	w := newWorld(t)
	h30 := w.head("h30", map[string]string{"f30.txt": "30\n"}, true)
	h5 := w.head("h5", map[string]string{"f5.txt": "5\n"}, true)
	h12 := w.head("h12", map[string]string{"f12.txt": "12\n"}, true)
	w.pend("30", h30)
	w.pend("5", h5)
	w.pend("12", h12)
	base := w.remoteDev()

	rep := w.pass()
	requireExit(t, rep, ExitPushed, "pushed")
	if want := "crw-5=" + h5 + " crw-12=" + h12 + " crw-30=" + h30; rep.Summary.Merged != want {
		t.Fatalf("merged %q, want issue-number order %q", rep.Summary.Merged, want)
	}
	if rep.Summary.Base != base || rep.Summary.Head != w.remoteDev() || rep.Summary.LeftOut != "" {
		t.Fatalf("summary %+v, remote dev %s", rep.Summary, w.remoteDev())
	}
	if !strings.Contains(w.git(w.remote, "log", "--first-parent", "--format=%s", "dev"), "Merge crw-30 at "+h30[:8]+" into dev") {
		t.Fatal("heads are not merged with --no-ff merge commits")
	}
	if got := w.git(w.remote, "log", "--first-parent", "--format=%s", base+"..dev"); strings.Index(got, "crw-30") > strings.Index(got, "crw-12") || strings.Index(got, "crw-12") > strings.Index(got, "crw-5") {
		t.Fatalf("merge order (newest first) is not 30, 12, 5:\n%s", got)
	}
	w.git(w.remote, "merge-base", "--is-ancestor", base, "dev") // a fast-forward of the old dev
	for _, n := range []string{"5", "12", "30"} {
		w.mustWhere(n, "done")
	}
	// The run's files are the shell script's: log, out, summary, verify log, record.
	for _, ext := range []string{".log", ".out", ".summary.json", ".verify.log", ".json"} {
		if _, err := os.Stat(filepath.Join(w.cfg.RunsDir, rep.Summary.Tag+ext)); err != nil {
			t.Fatalf("run file %s: %v", ext, err)
		}
	}
	var onDisk Summary
	data, _ := os.ReadFile(filepath.Join(w.cfg.RunsDir, rep.Summary.Tag+".summary.json"))
	if err := json.Unmarshal(data, &onDisk); err != nil || onDisk != *rep.Summary {
		t.Fatalf("summary file %s (%v)", data, err)
	}
	// The worktree and the private temporary directory are gone.
	if got := w.git(w.checkout, "worktree", "list", "--porcelain"); strings.Count(got, "worktree ") != 1 {
		t.Fatalf("worktrees left behind:\n%s", got)
	}
	if entries, _ := os.ReadDir(w.cfg.WorkRoot); len(entries) != 0 {
		t.Fatalf("work root not empty: %v", entries)
	}
	// The verifier saw the merged head, the base and the record path, in the merged worktree.
	calls := strings.Fields(w.ctlRead("verify-calls"))
	if len(calls) != 4 || calls[0] != rep.Summary.Head || calls[1] != base || calls[2] != rep.Summary.Record || !strings.HasSuffix(calls[3], "/run-"+rep.Summary.Tag) {
		t.Fatalf("verify call %v", calls)
	}
	// The lock is released.
	unlock, err := acquireLock(w.cfg.LockFile)
	if err != nil {
		t.Fatalf("lock still held: %v", err)
	}
	unlock()
	// Nothing was forced.
	for _, c := range w.argvCalls() {
		if strings.Contains(c, "push") && (strings.Contains(c, "force") || strings.Contains(c, " +") || strings.Contains(c, "-f ")) {
			t.Fatalf("a push that may force: %s", c)
		}
	}
}

func TestPassIdleStoppedAndIgnored(t *testing.T) {
	w := newWorld(t)
	requireExit(t, w.pass(), ExitPushed, "idle")
	w.write(filepath.Join(w.cfg.QueueDir, "pending", "notes.txt"), "x")
	w.write(filepath.Join(w.cfg.QueueDir, "pending", "7x"), "x")
	if err := os.MkdirAll(filepath.Join(w.cfg.QueueDir, "pending", "9"), 0o755); err != nil {
		t.Fatal(err)
	}
	rep := w.pass()
	requireExit(t, rep, ExitPushed, "idle")
	if strings.Join(rep.Ignored, ",") != "7x,9,notes.txt" {
		t.Fatalf("ignored %v", rep.Ignored)
	}
	w.pend("3", w.head("h3", map[string]string{"a": "a"}, true))
	w.write(filepath.Join(w.cfg.QueueDir, "stop"), "")
	requireExit(t, w.pass(), ExitPushed, "stopped")
	w.mustWhere("3", "pending")
	if got := w.ctlRead("verify-n"); got != "" {
		t.Fatalf("verified under a stop file: %s", got)
	}
}

func TestPassGateThatIsNotPassBlocksTheHead(t *testing.T) {
	w := newWorld(t)
	good := w.head("good", map[string]string{"g": "g\n"}, true)
	bad := w.head("bad", map[string]string{"b": "b\n"}, true)
	w.pend("1", good)
	w.pend("2", bad)
	w.ctlWrite("block-CRW-2", "")
	rep := w.pass()
	requireExit(t, rep, ExitPushed, "pushed")
	w.mustWhere("1", "done")
	w.mustWhere("2", "blocked")
	if !strings.Contains(strings.Join(rep.Moved, "\n"), "gate not PASS: BLOCK: no evaluation") {
		t.Fatalf("moved %v", rep.Moved)
	}
	if got := w.ctlRead("gate-calls"); got != "CRW-1 "+good+"\nCRW-2 "+bad+"\n" {
		t.Fatalf("gate calls %q", got)
	}
	log, _ := os.ReadFile(filepath.Join(w.cfg.QueueDir, "runner.log"))
	if !strings.Contains(string(log), "CRW-2 "+bad+" gate not PASS: BLOCK") {
		t.Fatalf("runner log:\n%s", log)
	}
}

func TestPassAllGatesBlockedIsNothingToIntegrate(t *testing.T) {
	w := newWorld(t)
	w.pend("2", w.head("bad", map[string]string{"b": "b\n"}, true))
	w.ctlWrite("block-CRW-2", "")
	requireExit(t, w.pass(), ExitNothingMerged, "nothing-to-integrate")
	w.mustWhere("2", "blocked")
	if w.ctlRead("verify-n") != "" {
		t.Fatal("verified with nothing to merge")
	}
}

func TestPassBadHeadFileIsBlockedWithoutGate(t *testing.T) {
	w := newWorld(t)
	w.write(filepath.Join(w.cfg.QueueDir, "pending", "4"), "not-a-sha\n")
	rep := w.pass()
	requireExit(t, rep, ExitNothingMerged, "nothing-to-integrate")
	w.mustWhere("4", "blocked")
	if w.ctlRead("gate-calls") != "" {
		t.Fatal("gate ran for a head that is not a sha")
	}
}

func TestPassConflictingHeadLeavesOutAndTheRestGoOn(t *testing.T) {
	w := newWorld(t)
	a := w.head("a", map[string]string{"shared.txt": "line1\nA\nline3\n"}, true)
	b := w.head("b", map[string]string{"shared.txt": "line1\nB\nline3\n"}, true)
	c := w.head("c", map[string]string{"c.txt": "c\n"}, true)
	w.pend("1", a)
	w.pend("2", b)
	w.pend("3", c)
	rep := w.pass()
	requireExit(t, rep, ExitPushed, "pushed")
	if rep.Summary.LeftOut != "crw-2:conflict" || rep.Summary.Merged != "crw-1="+a+" crw-3="+c {
		t.Fatalf("summary %+v", rep.Summary)
	}
	w.mustWhere("1", "done")
	w.mustWhere("2", "conflict")
	w.mustWhere("3", "done")
	if got := w.fileAt("dev", "shared.txt"); got != "line1\nA\nline3" {
		t.Fatalf("shared.txt on dev:\n%s", got)
	}
	if st := w.git(w.checkout, "worktree", "list", "--porcelain"); strings.Count(st, "worktree ") != 1 {
		t.Fatal("worktree left behind")
	}
}

func TestPassOnlyConflictingHeadIsNothingMergedAndGoesToConflict(t *testing.T) {
	w := newWorld(t)
	a := w.head("a", map[string]string{"shared.txt": "line1\nA\nline3\n"}, true)
	w.git(w.remote, "update-ref", "refs/heads/devtmp", a) // keep a reachable
	// dev moves under the head so that merging it conflicts
	w.head("mv", map[string]string{"shared.txt": "line1\nMOVED\nline3\n"}, true)
	w.git(w.other, "push", "-q", "origin", "mv:refs/heads/dev")
	w.pend("1", a)
	before := w.remoteDev()
	rep := w.pass()
	requireExit(t, rep, ExitNothingMerged, "nothing-merged")
	w.mustWhere("1", "conflict")
	if w.remoteDev() != before || w.ctlRead("verify-n") != "" {
		t.Fatal("pushed or verified with nothing merged")
	}
}

func TestPassHeadAlreadyInDevIsDone(t *testing.T) {
	w := newWorld(t)
	h := w.head("h", map[string]string{"h": "h\n"}, true)
	w.git(w.other, "push", "-q", "origin", "h:refs/heads/dev")
	w.pend("8", h)
	rep := w.pass()
	requireExit(t, rep, ExitNothingMerged, "nothing-merged")
	if rep.Summary.LeftOut != "crw-8:already-in-dev" {
		t.Fatalf("summary %+v", rep.Summary)
	}
	w.mustWhere("8", "done")
}

func TestPassHeadNotInCheckoutGoesToConflict(t *testing.T) {
	w := newWorld(t)
	w.pend("8", w.head("unpublished", map[string]string{"u": "u\n"}, false))
	rep := w.pass()
	requireExit(t, rep, ExitNothingMerged, "nothing-merged")
	if rep.Summary.LeftOut != "crw-8:head-not-in-checkout" {
		t.Fatalf("summary %+v", rep.Summary)
	}
	w.mustWhere("8", "conflict")
}

// Two heads that bump the version line differently conflict on that line alone: it is
// resolved, the line is re-derived on the merged tree, and the verifier (which checks the
// derived line) passes.
func TestPassVersionLineConflictIsResolvedAndRederived(t *testing.T) {
	w := newWorld(t)
	a := w.head("a", map[string]string{"plugins/crw/skills/a.txt": "a\n", manifestPath: manifest("v-2-a")}, true)
	b := w.head("b", map[string]string{"plugins/crw/skills/b.txt": "b\n", manifestPath: manifest("v-2-b")}, true)
	w.pend("1", a)
	w.pend("2", b)
	rep := w.pass()
	requireExit(t, rep, ExitPushed, "pushed")
	if rep.Summary.Merged != "crw-1="+a+" crw-2="+b || rep.Summary.LeftOut != "" {
		t.Fatalf("summary %+v", rep.Summary)
	}
	if got := w.fileAt("dev", manifestPath); got != strings.TrimRight(manifest("v-3"), "\n") {
		t.Fatalf("manifest on dev:\n%s", got)
	}
	if !strings.Contains(w.git(w.remote, "log", "--format=%s", "-3", "dev"), "integration "+rep.Summary.Tag+": re-derive the plugin version line") {
		t.Fatal("no re-derive commit")
	}
	if !strings.Contains(w.ctlRead("record-calls"), "tmp-"+rep.Summary.Tag) {
		t.Fatalf("record command did not get the run's private tmpdir: %q", w.ctlRead("record-calls"))
	}
	w.mustWhere("1", "done")
	w.mustWhere("2", "done")
}

// A plugin change that does not conflict still has its version line re-derived.
func TestPassPluginChangeWithoutConflictIsRederived(t *testing.T) {
	w := newWorld(t)
	a := w.head("a", map[string]string{"plugins/crw/skills/a.txt": "a\n"}, true)
	w.pend("1", a)
	rep := w.pass()
	requireExit(t, rep, ExitPushed, "pushed")
	if got := w.fileAt("dev", manifestPath); got != strings.TrimRight(manifest("v-2"), "\n") {
		t.Fatalf("manifest on dev:\n%s", got)
	}
}

// A change outside the plugin package runs no re-derivation.
func TestPassNoPluginChangeRunsNoRecordCommand(t *testing.T) {
	w := newWorld(t)
	w.pend("1", w.head("a", map[string]string{"x.txt": "x\n"}, true))
	requireExit(t, w.pass(), ExitPushed, "pushed")
	if w.ctlRead("record-calls") != "" {
		t.Fatal("record command ran without a plugin change")
	}
}

// A conflict in the manifest beyond the version line is not resolved.
func TestPassManifestConflictBeyondVersionLineIsLeftOut(t *testing.T) {
	w := newWorld(t)
	a := w.head("a", map[string]string{manifestPath: strings.Replace(manifest("v-1"), `"x"`, `"from a"`, 1)}, true)
	b := w.head("b", map[string]string{manifestPath: strings.Replace(manifest("v-1"), `"x"`, `"from b"`, 1)}, true)
	w.pend("1", a)
	w.pend("2", b)
	rep := w.pass()
	requireExit(t, rep, ExitPushed, "pushed")
	if rep.Summary.LeftOut != "crw-2:conflict" {
		t.Fatalf("summary %+v", rep.Summary)
	}
	w.mustWhere("2", "conflict")
}

// A version-line conflict next to another conflicted file is not resolved either.
func TestPassVersionConflictNextToAnotherConflictIsLeftOut(t *testing.T) {
	w := newWorld(t)
	a := w.head("a", map[string]string{manifestPath: manifest("v-9-a"), "shared.txt": "line1\nA\nline3\n"}, true)
	b := w.head("b", map[string]string{manifestPath: manifest("v-9-b"), "shared.txt": "line1\nB\nline3\n"}, true)
	w.pend("1", a)
	w.pend("2", b)
	rep := w.pass()
	// a changed the version line without a matching skill file, so its merged tree drifts and the
	// verifier refuses it: the point here is only that b was not merged.
	if rep.Summary == nil || rep.Summary.LeftOut != "crw-2:conflict" {
		t.Fatalf("report %+v", rep)
	}
}

func TestPassVerificationFailureRetriesBatchThenGoesOneAtATime(t *testing.T) {
	w := newWorld(t)
	a := w.head("a", map[string]string{"a.txt": "a\n"}, true)
	b := w.head("b", map[string]string{"b.txt": "b\n"}, true)
	w.pend("1", a)
	w.pend("2", b)
	// call 1 and 2: the batch fails twice; call 3 and 4: each head alone passes.
	w.ctlWrite("verify-rc", "3\n3\n0\n0\n")
	base := w.remoteDev()

	rep := w.pass()
	requireExit(t, rep, ExitVerifyFailed, "verification-failed(rc=3)")
	if w.remoteDev() != base || w.where("1") != "pending" || w.where("2") != "pending" {
		t.Fatal("a failed verification pushed or moved something")
	}
	if rep.Retry != "same batch once more" || !fileExists(filepath.Join(w.cfg.QueueDir, "retried", "batch-"+batchKey([]Entry{{Node: "crw-1", Head: a}, {Node: "crw-2", Head: b}}))) {
		t.Fatalf("retry %q", rep.Retry)
	}
	if len(w.r.soloOwed()) != 0 {
		t.Fatal("solo mode started before the batch retry")
	}

	rep = w.pass() // the same batch again, fails again
	requireExit(t, rep, ExitVerifyFailed, "verification-failed(rc=3)")
	if len(w.r.soloOwed()) != 2 || !strings.HasPrefix(rep.Retry, "one at a time") {
		t.Fatalf("solo owed %v, retry %q", w.r.soloOwed(), rep.Retry)
	}

	rep = w.pass() // head 1 alone
	requireExit(t, rep, ExitPushed, "pushed")
	if rep.Summary.Merged != "crw-1="+a {
		t.Fatalf("solo run merged %q", rep.Summary.Merged)
	}
	w.mustWhere("1", "done")
	w.mustWhere("2", "pending")
	if owed := w.r.soloOwed(); len(owed) != 1 || owed[0] != "crw-2="+b {
		t.Fatalf("solo owed %v", owed)
	}

	rep = w.pass() // head 2 alone, then batching resumes
	requireExit(t, rep, ExitPushed, "pushed")
	w.mustWhere("2", "done")
	if len(w.r.soloOwed()) != 0 {
		t.Fatalf("solo owed %v", w.r.soloOwed())
	}
}

func TestPassHeadFailingAloneIsRetriedOnceThenBlocked(t *testing.T) {
	w := newWorld(t)
	a := w.head("a", map[string]string{"a.txt": "a\n"}, true)
	w.pend("1", a)
	w.ctlWrite("verify-rc", "3\n3\n3\n")
	rep := w.pass()
	requireExit(t, rep, ExitVerifyFailed, "verification-failed(rc=3)")
	w.mustWhere("1", "pending")
	if marker, _ := os.ReadFile(filepath.Join(w.cfg.QueueDir, "retried", "1")); strings.TrimSpace(string(marker)) != a {
		t.Fatalf("retry marker %q", marker)
	}
	requireExit(t, w.pass(), ExitVerifyFailed, "verification-failed(rc=3)")
	w.mustWhere("1", "blocked")
	// A new head under the same issue is retried afresh.
	b := w.head("b", map[string]string{"b.txt": "b\n"}, true)
	w.pend("1", b)
	w.ctlWrite("verify-rc", "0\n0\n0\n0\n0\n")
	os.Remove(filepath.Join(w.ctl, "verify-n"))
	requireExit(t, w.pass(), ExitPushed, "pushed")
	w.mustWhere("1", "done")
}

func TestPassDevMovedDuringVerificationPushesNothing(t *testing.T) {
	w := newWorld(t)
	w.pend("1", w.head("a", map[string]string{"a.txt": "a\n"}, true))
	w.ctlWrite("during-verify.sh", `cd "$OTHER_DIR" && git checkout -q -B racer origin/dev && echo r > racer.txt && git add racer.txt && git commit -q -m racer && git push -q origin racer:refs/heads/dev
`)
	rep := w.pass()
	if rep.Exit != ExitMoved || !strings.HasPrefix(rep.Result, "dev-moved:") {
		t.Fatalf("report %+v", rep)
	}
	if rep.Result != "dev-moved:"+w.remoteDev() {
		t.Fatalf("result %s, remote %s", rep.Result, w.remoteDev())
	}
	if strings.Contains(w.git(w.remote, "log", "--format=%s", "dev"), "Merge crw-1") {
		t.Fatal("the merge was pushed over a moved dev")
	}
	w.mustWhere("1", "pending")
}

func TestPassRefusedPushIsExit4AndNeverForced(t *testing.T) {
	w := newWorld(t)
	w.pend("1", w.head("a", map[string]string{"a.txt": "a\n"}, true))
	w.write(filepath.Join(w.remote, "hooks", "pre-receive"), "#!/bin/sh\nexit 1\n")
	before := w.remoteDev()
	rep := w.pass()
	requireExit(t, rep, ExitMoved, "push-failed")
	if w.remoteDev() != before {
		t.Fatal("remote moved")
	}
	w.mustWhere("1", "pending")
	pushed := false
	for _, c := range w.argvCalls() {
		if strings.Contains(c, " push ") {
			pushed = true
			if strings.Contains(c, "force") || strings.Contains(c, " +") || strings.Contains(c, " -f") || strings.Contains(c, "--mirror") || strings.Contains(c, "--delete") {
				t.Fatalf("push may rewrite: %s", c)
			}
			if !strings.HasSuffix(c, ":refs/heads/dev") {
				t.Fatalf("push does not name the branch ref: %s", c)
			}
		}
	}
	if !pushed {
		t.Fatal("no push was attempted")
	}
}

func TestPassPushRefusedAsNonFastForwardWithoutForce(t *testing.T) {
	// The fetch before the push sees the old dev, then dev moves: git itself refuses to push a
	// non-fast-forward without force.
	w := newWorld(t)
	w.pend("1", w.head("a", map[string]string{"a.txt": "a\n"}, true))
	exec := w.r.Exec
	moved := false
	w.r.Exec = func(ctx context.Context, c Cmd) (int, error) {
		if !moved && len(c.Argv) > 5 && c.Argv[len(c.Argv)-1] != "" && strings.Contains(strings.Join(c.Argv, " "), " push ") {
			moved = true
			w.git(w.other, "checkout", "-q", "-B", "racer2", "origin/dev")
			w.write(filepath.Join(w.other, "r2.txt"), "r\n")
			w.git(w.other, "add", "-A")
			w.git(w.other, "commit", "-q", "-m", "racer2")
			w.git(w.other, "push", "-q", "origin", "racer2:refs/heads/dev")
		}
		return exec(ctx, c)
	}
	rep := w.pass()
	requireExit(t, rep, ExitMoved, "push-failed")
	if strings.Contains(w.git(w.remote, "log", "--format=%s", "dev"), "Merge crw-1") {
		t.Fatal("a non-fast-forward push went through")
	}
}

func TestPassLockHeldIsRefusedBeforeAnythingRuns(t *testing.T) {
	w := newWorld(t)
	w.pend("1", w.head("a", map[string]string{"a.txt": "a\n"}, true))
	unlock, err := acquireLock(w.cfg.LockFile)
	if err != nil {
		t.Fatal(err)
	}
	rep := w.pass()
	unlock()
	requireExit(t, rep, ExitUsage, "locked")
	w.mustWhere("1", "pending")
	if w.ctlRead("gate-calls") != "" {
		t.Fatal("gate ran without the lock")
	}
	requireExit(t, w.pass(), ExitPushed, "pushed")
}

func TestPassWorktreeAlreadyThereIsExit2(t *testing.T) {
	w := newWorld(t)
	w.pend("1", w.head("a", map[string]string{"a.txt": "a\n"}, true))
	tag := w.cfg.TagPrefix + w.now.Add(time.Minute).Format("0102150405")
	if err := os.MkdirAll(filepath.Join(w.cfg.WorkRoot, "run-"+tag), 0o755); err != nil {
		t.Fatal(err)
	}
	rep := w.pass()
	requireExit(t, rep, ExitUsage, "error")
	if !strings.Contains(rep.Error, "exists") {
		t.Fatal(rep.Error)
	}
	w.mustWhere("1", "pending")
}

func TestPassKeepsAPendingFileTheCoordinatorRefreshedMeanwhile(t *testing.T) {
	w := newWorld(t)
	a := w.head("a", map[string]string{"a.txt": "a\n"}, true)
	newer := w.head("a2", map[string]string{"a2.txt": "a2\n"}, true)
	w.pend("1", a)
	w.ctlWrite("during-verify.sh", `printf '%s\n' `+newer+` > "`+filepath.Join(w.cfg.QueueDir, "pending", "1")+`"
`)
	rep := w.pass()
	requireExit(t, rep, ExitPushed, "pushed")
	w.mustWhere("1", "pending")
	if got, _ := readHead(filepath.Join(w.cfg.QueueDir, "pending", "1")); got != newer {
		t.Fatalf("pending now holds %s", got)
	}
}

func TestPassVerifyCommandThatCannotStartFails(t *testing.T) {
	w := newWorld(t)
	w.r.Cfg.VerifyCommand = []string{filepath.Join(w.root, "scripts", "missing.sh")}
	w.pend("1", w.head("a", map[string]string{"a.txt": "a\n"}, true))
	rep := w.pass()
	requireExit(t, rep, ExitVerifyFailed, "verification-failed(rc=127)")
}

func TestWatchRunsUntilTheStopFile(t *testing.T) {
	w := newWorld(t)
	w.pend("1", w.head("a", map[string]string{"a.txt": "a\n"}, true))
	sleeps := 0
	w.r.Sleep = func(context.Context, time.Duration) error {
		sleeps++
		if sleeps == 2 {
			w.write(filepath.Join(w.cfg.QueueDir, "stop"), "")
		}
		return nil
	}
	var results []string
	if err := w.r.Watch(context.Background(), func(r Report) { results = append(results, r.Result) }); err != nil {
		t.Fatal(err)
	}
	if strings.Join(results, ",") != "pushed,stopped" {
		t.Fatalf("results %v (idle passes are not reported)", results)
	}
	w.mustWhere("1", "done")
}

func TestWatchEndsWithTheContext(t *testing.T) {
	w := newWorld(t)
	ctx, cancel := context.WithCancel(context.Background())
	w.r.Sleep = func(ctx context.Context, _ time.Duration) error { cancel(); return ctx.Err() }
	if err := w.r.Watch(ctx, func(Report) {}); err == nil {
		t.Fatal("watch ignored the cancelled context")
	}
}
