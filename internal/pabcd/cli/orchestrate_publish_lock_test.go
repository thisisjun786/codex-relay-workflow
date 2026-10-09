package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/goalplan"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
)

// CRW-975 (with CRW-976 folded in): the A>B publication judges everything it publishes on inside the
// goalplan write lock. These cases are written to fail on the baseline: the review binding was read
// outside the lock, an unreachable plan directory was a Go error where the oracle goes on, and a plan
// the lock withheld from writers (a repeated key, bytes that are not UTF-8) was published over outside
// the lock with no further judgement. Every ordering is a seam, never a sleep.

// orchestrateCommitReviewerFailLock is the ordering seam of the review-binding race: the reviewer's FAIL
// is recorded after the unlocked review binding check passed and before this writer's lock callback runs.
func orchestrateCommitReviewerFailLock(cwd, slug string, fn func(*goalplan.Goalplan) (orchestrateCommitOutcome, error)) (goalplan.GoalplanWriteLockResult[orchestrateCommitOutcome], error) {
	plan := goalplan.ReadGoalplan(cwd, slug)
	if plan == nil || len(plan.ReviewRounds) == 0 {
		return goalplan.GoalplanWriteLockResult[orchestrateCommitOutcome]{Kind: "unreadable", Reason: "goalplan '" + slug + "' does not exist"}, nil
	}
	plan.ReviewRounds[0].Lane.Verdict = goalplan.VerdictFail
	plan.ReviewRounds[0].Status = goalplan.ReviewChangesRequested
	if err := goalplan.WriteGoalplan(cwd, plan); err != nil {
		return goalplan.GoalplanWriteLockResult[orchestrateCommitOutcome]{}, err
	}
	return goalplan.WithGoalplanWriteLock(cwd, slug, fn, nil)
}

// orchestratePublishLockApproved seeds a session at A with a complete, approved plan_audit round.
func orchestratePublishLockApproved(t *testing.T, cwd, id string) {
	t.Helper()
	unit := orchestrateReviewBindingPlanUnit(t, cwd)
	epoch := "e-plan-1"
	files, hash := orchestrateReviewBindingBoundFiles(t, cwd, unit)
	round := orchestrateReviewBindingVerdictRound("r1", id, "wp1", epoch, "pass", files)
	round["planSha256"] = hash
	orchestrateReviewBindingSeed(t, cwd, id, unit, []map[string]any{round}, &epoch, "A")
}

// TestOrchestrateCommitRechecksTheReviewBindingUnderTheLock: a reviewer FAIL recorded under the goalplan
// lock between the review binding check and the publication is honoured, not ignored. The oracle has the
// same race (validateReviewBinding :688-691 and the A>B write both run without the lock); the port fixes
// it, as it does the work-phase gate (CRW-811).
func TestOrchestrateCommitRechecksTheReviewBindingUnderTheLock(t *testing.T) {
	cwd, id := orchestrateTransitionRoot(t), "publish-lock-reviewer-fail"
	orchestratePublishLockApproved(t, cwd, id)
	seams := &orchestrateCommitSeams{lockGoalplan: orchestrateCommitReviewerFailLock}
	got := orchestrateCommitRunOK(t, cwd, seams, "B", "--session", id, "--attest", orchestrateReviewBindingAttest("pass"))
	want := "orchestrate B: current=A session=" + id + "; you attested \"pass\" but the reviewer recorded \"fail\" (LEAN-REVIEW-01)."
	if got.Code != 1 || !strings.HasPrefix(got.Output, want) {
		t.Fatalf("a FAIL recorded under the lock must refuse\n got: %+v\nwant prefix: %s", got, want)
	}
	if after := state.ReadState(cwd, id); after.Phase != state.PhaseA {
		t.Fatalf("the refused transition moved the session: %+v", after)
	}
	if rows := orchestrateTransitionLedger(t, cwd); len(rows) != 0 {
		t.Fatalf("the refused transition wrote a ledger row: %+v", rows)
	}
}

// TestOrchestrateCommitReviewBindingUnderTheLockStillPasses is the contrast: no race, the approved round
// holds under the lock too, and the edge publishes.
func TestOrchestrateCommitReviewBindingUnderTheLockStillPasses(t *testing.T) {
	cwd, id := orchestrateTransitionRoot(t), "publish-lock-approved"
	orchestratePublishLockApproved(t, cwd, id)
	got := orchestrateCommitRunOK(t, cwd, nil, "B", "--session", id, "--attest", orchestrateReviewBindingAttest("pass"))
	if got.Code != 0 {
		t.Fatalf("an approved round must pass under the lock: %+v", got)
	}
	if after := state.ReadState(cwd, id); after.Phase != state.PhaseB {
		t.Fatalf("the edge did not publish: %+v", after)
	}
}

// TestOrchestrateCommitUnreachablePlanFailsOpen: a plan directory the lock cannot reach is a plan that
// cannot be read, and the oracle's gate (orchestrate-cli.ts 606-622) goes on; the baseline returned the
// lock's Go error for it.
func TestOrchestrateCommitUnreachablePlanFailsOpen(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	for name, mode := range map[string]os.FileMode{"no access": 0o000, "no search": 0o400} {
		t.Run(name, func(t *testing.T) {
			cwd, id := orchestrateTransitionRoot(t), "publish-lock-eacces"
			orchestrateCommitPlan(t, cwd, id)
			orchestrateTransitionSession(t, cwd, id, `{"phase":"B","slug":"`+id+`"}`)
			dir := filepath.Join(cwd, ".crw", "goalplans", id)
			if err := os.Chmod(dir, mode); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
			got, err := orchestrateCommitTry(t, cwd, nil, "C", "--session", id, "--attest", `{"from":"B","to":"C","did":"built it"}`)
			if err != nil {
				t.Fatalf("an unreachable plan directory must not be an error: %v", err)
			}
			if got.Code != 0 {
				t.Fatalf("an unreachable plan must not block: %+v", got)
			}
			if after := state.ReadState(cwd, id); after.Phase != state.PhaseC {
				t.Fatalf("the fail-open edge did not publish: %+v", after)
			}
		})
	}
}

// TestOrchestrateCommitWithheldPlanPublishesNothing: a plan the lock reads but withholds from writers (a
// repeated key; bytes that are not UTF-8) refuses the edge with the lock's reason. The baseline released
// the lock and published the state outside it with the work-phase binding it had read before.
func TestOrchestrateCommitWithheldPlanPublishesNothing(t *testing.T) {
	cases := map[string]struct {
		damage func(string) string
		reason string
	}{
		"repeated key": {func(raw string) string { return strings.Replace(raw, "{", `{"objective":"dup",`, 1) }, "repeated key"},
		"invalid UTF-8": {func(raw string) string {
			return regexp.MustCompile(`"objective":\s*"`).ReplaceAllStringFunc(raw, func(m string) string { return m + "\xff" })
		}, "invalid UTF-8"},
		// The reader returns no plan for these two, which is not the same as an absent plan (CRW-975, evaluation of 50f1f3c2).
		"unpaired surrogate": {func(raw string) string {
			return regexp.MustCompile(`"objective":\s*"`).ReplaceAllStringFunc(raw, func(m string) string { return m + `\ud800` })
		}, "unpaired JSON surrogate"},
		"repeated key with a malformed last value": {func(raw string) string {
			return strings.TrimSuffix(strings.TrimSpace(raw), "}") + `,"objective":null}`
		}, "repeated key"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			cwd, id := orchestrateTransitionRoot(t), "publish-lock-withheld"
			orchestrateCommitPlan(t, cwd, id)
			path := filepath.Join(cwd, ".crw", "goalplans", id, "goalplan.json")
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			damaged := c.damage(string(raw))
			if damaged == string(raw) {
				t.Fatal("the damage did not change the plan")
			}
			if err := os.WriteFile(path, []byte(damaged), 0o600); err != nil {
				t.Fatal(err)
			}
			orchestrateTransitionSession(t, cwd, id, `{"phase":"B","slug":"`+id+`"}`)
			got := orchestrateCommitRunOK(t, cwd, nil, "C", "--session", id, "--attest",
				`{"from":"B","to":"C","did":"built it","workPhaseId":"wp1"}`)
			if got.Code != 1 || !strings.Contains(got.Output, c.reason) || !strings.Contains(got.Output, "Nothing was written") {
				t.Fatalf("a withheld plan must refuse with the lock's reason: %+v", got)
			}
			if after := state.ReadState(cwd, id); after.Phase != state.PhaseB {
				t.Fatalf("the refused transition moved the session: %+v", after)
			}
			if rows := orchestrateTransitionLedger(t, cwd); len(rows) != 0 {
				t.Fatalf("the refused transition wrote a ledger row: %+v", rows)
			}
		})
	}
}

// TestOrchestrateCommitLinkedPlanDirectoryAfterTheGateIsNotFailOpen: only an access failure is fail-open. A
// plan directory swapped for a symbolic link between the unlocked gate read and the lock is a path-safety
// refusal, so nothing is published; the baseline refused it as a Go error and must still.
func TestOrchestrateCommitLinkedPlanDirectoryAfterTheGateIsNotFailOpen(t *testing.T) {
	cwd, id := orchestrateTransitionRoot(t), "publish-lock-linked"
	orchestrateCommitPlan(t, cwd, id)
	orchestrateTransitionSession(t, cwd, id, `{"phase":"B","slug":"`+id+`"}`)
	seams := &orchestrateCommitSeams{lockGoalplan: func(cwd, slug string, fn func(*goalplan.Goalplan) (orchestrateCommitOutcome, error)) (goalplan.GoalplanWriteLockResult[orchestrateCommitOutcome], error) {
		path := filepath.Join(cwd, ".crw", "goalplans", slug)
		moved := path + "-moved"
		if err := os.Rename(path, moved); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(moved, path); err != nil {
			t.Fatal(err)
		}
		return goalplan.WithGoalplanWriteLock(cwd, slug, fn, nil)
	}}
	got, err := orchestrateCommitTry(t, cwd, seams, "C", "--session", id, "--attest", `{"from":"B","to":"C","did":"built it","workPhaseId":"wp1"}`)
	if err == nil && got.Code == 0 {
		t.Fatalf("a linked plan directory was treated as fail-open: %+v", got)
	}
	if after := state.ReadState(cwd, id); after.Phase != state.PhaseB {
		t.Fatalf("the refused transition moved the session: %+v", after)
	}
	if rows := orchestrateTransitionLedger(t, cwd); len(rows) != 0 {
		t.Fatalf("the refused transition wrote a ledger row: %+v", rows)
	}
}

// orchestratePublishLockFifo replaces the first recorded plan file of the approved round with a FIFO that
// has no writer, and returns its path. A read that opens it without O_NONBLOCK blocks until a writer
// arrives; the cleanup connects one, so a test that fails by timeout does not leave its goroutine behind.
func orchestratePublishLockFifo(t *testing.T, cwd, slug string) string {
	t.Helper()
	plan := goalplan.ReadGoalplan(cwd, slug)
	file := filepath.Join(cwd, plan.ReviewRounds[0].PlanFiles[0].Path)
	if err := os.Remove(file); err != nil {
		t.Error(err) // not Fatal: the seam calls this from the goroutine under test
		return file
	}
	if err := syscall.Mkfifo(file, 0o600); err != nil {
		t.Error(err)
		return file
	}
	t.Cleanup(func() {
		if f, err := os.OpenFile(file, os.O_RDWR|syscall.O_NONBLOCK, 0); err == nil {
			_ = f.Close()
		}
	})
	return file
}

// TestRecomputedReadsASpecialFileAsMissing: a plan file that is not a regular file (a FIFO swapped in after
// the entry was looked at) must not block the hash. It reads "missing", as every unreadable entry does
// (CRW-975; the oracle's readFileSync would block on it).
func TestRecomputedReadsASpecialFileAsMissing(t *testing.T) {
	cwd, id := orchestrateTransitionRoot(t), "recomputed-fifo"
	orchestratePublishLockApproved(t, cwd, id)
	round := goalplan.ReadGoalplan(cwd, id).ReviewRounds[0]
	orchestratePublishLockFifo(t, cwd, id)
	got := make(chan []goalplan.PlanFileHash, 1)
	go func() { got <- Recomputed(cwd, round.PlanFiles) }()
	select {
	case files := <-got:
		if files[0].Sha256 != "missing" {
			t.Fatalf("a FIFO plan file must read missing: %+v", files[0])
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Recomputed blocked on a FIFO plan file")
	}
}

// TestOrchestrateCommitLockedReviewRehashDoesNotBlockOnAFifo: the re-hash under the goalplan lock reads the
// plan files, so a plan file replaced by a FIFO after the unlocked check must not hold the lock. The edge
// refuses (the plan changed) and releases the lock, so another writer of the plan is not kept waiting.
func TestOrchestrateCommitLockedReviewRehashDoesNotBlockOnAFifo(t *testing.T) {
	cwd, id := orchestrateTransitionRoot(t), "publish-lock-fifo-rehash"
	orchestratePublishLockApproved(t, cwd, id)
	seams := &orchestrateCommitSeams{lockGoalplan: func(cwd, slug string, fn func(*goalplan.Goalplan) (orchestrateCommitOutcome, error)) (goalplan.GoalplanWriteLockResult[orchestrateCommitOutcome], error) {
		orchestratePublishLockFifo(t, cwd, slug)
		return goalplan.WithGoalplanWriteLock(cwd, slug, fn, nil)
	}}
	type answer struct {
		got CliResult
		err error
	}
	parsed := ParseOrchestrateCliArgs([]string{"B", "--session", id, "--attest", orchestrateReviewBindingAttest("pass")}, cwd)
	read, rerr := RunOrchestrateRead(parsed, ReadEnv{})
	if rerr != nil || read.Result != nil {
		t.Fatalf("read: %+v %v", read.Result, rerr)
	}
	done := make(chan answer, 1)
	go func() {
		got, err := orchestrateCommitRun(*parsed.Args, read.SessionID, seams)
		done <- answer{got, err}
	}()
	select {
	case a := <-done:
		if a.err != nil || a.got.Code != 1 || !strings.Contains(a.got.Output, "the plan changed after round") {
			t.Fatalf("a FIFO plan file under the lock must refuse as a changed plan: %+v %v", a.got, a.err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the locked review re-hash blocked on a FIFO plan file while holding the goalplan lock")
	}
	if after := state.ReadState(cwd, id); after.Phase != state.PhaseA {
		t.Fatalf("the refused transition moved the session: %+v", after)
	}
	got, err := goalplan.WithGoalplanWriteLock(cwd, id, func(*goalplan.Goalplan) (int, error) { return 1, nil }, nil)
	if err != nil || got.Kind != "ok" {
		t.Fatalf("the goalplan lock was left held: %+v %v", got, err)
	}
}

// TestOrchestrateCommitCancelDuringLockedReviewRehashWritesNothing: the review binding is judged again under
// the lock and that re-hash reads the plan files, so a cancellation that arrives during it must still write
// nothing (CRW-871: nothing is written once the invocation is cancelled before the first durable effect).
// The order is fixed without a sleep through the interrupt seam, which runs before every cancellation
// check: the first call is the one at the top of the lock callback, the second the one after the re-hash.
func TestOrchestrateCommitCancelDuringLockedReviewRehashWritesNothing(t *testing.T) {
	cwd, id := orchestrateTransitionRoot(t), "publish-lock-cancel-rehash"
	orchestratePublishLockApproved(t, cwd, id)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	calls := 0
	seams := &orchestrateCommitSeams{interrupt: func() {
		calls++
		if calls == 2 {
			cancel()
		}
	}}
	parsed := ParseOrchestrateCliArgs([]string{"B", "--session", id, "--attest", orchestrateReviewBindingAttest("pass")}, cwd)
	read, err := RunOrchestrateRead(parsed, ReadEnv{})
	if err != nil || read.Result != nil {
		t.Fatalf("read: %+v %v", read.Result, err)
	}
	got, err := orchestrateCommitRunContext(ctx, *parsed.Args, read.SessionID, seams)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("a cancellation during the locked re-hash must answer Interrupted: got=%+v err=%v calls=%d", got, err, calls)
	}
	if after := state.ReadState(cwd, id); after.Phase != state.PhaseA {
		t.Fatalf("the cancelled transition moved the session: %+v", after)
	}
	if rows := orchestrateTransitionLedger(t, cwd); len(rows) != 0 {
		t.Fatalf("the cancelled transition wrote a ledger row: %+v", rows)
	}
}
