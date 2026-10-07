// The three write-order and session-argument defects of loop init, found by the post-merge
// evaluation of PR #660 and fixed here (docs/port-cxc/known-defects/CRW-646.md, all port: fixed):
//
//	c1 (P0, data loss) two concurrent inits on one slug let the later one replace the plan the
//	   earlier one published, because the absence check and the publication were not one critical
//	   section and the publication's rename replaces its destination.
//	c2 (P1) with --session the plan and its created row were written before the session lock was
//	   taken, so a lock that could not be taken left an unbound plan that refused the retry.
//	c3 (P1) a --session of blanks skipped the canonical check (which tested the trimmed value) and
//	   then resolved through the RAW value, whose sanitized key is the 'missing' session.
//
// Every case sets HOME, CODEX_HOME and CRW_HOME into a temporary root through loopReadWorkspace, so
// nothing here can reach the operator's real state.
package cli

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/goalplan"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
)

// loopHoldAfterAbsenceCheck installs the init seam: the FIRST init to reach it (claimed by a compare
// and swap, so a later caller never waits on the first) signals held and waits for release; every
// later init passes straight through. It returns the release function and restores the production nil
// seam when the test ends.
func loopHoldAfterAbsenceCheck(t *testing.T) (held <-chan struct{}, release func()) {
	t.Helper()
	heldCh, releaseCh := make(chan struct{}), make(chan struct{})
	var claimed atomic.Bool
	loopInitAfterAbsenceCheck = func() {
		if !claimed.CompareAndSwap(false, true) {
			return
		}
		close(heldCh)
		<-releaseCh
	}
	t.Cleanup(func() { loopInitAfterAbsenceCheck = nil })
	return heldCh, func() { close(releaseCh) }
}

// loopInitOutcome is one init's answer from a goroutine that may not call t.Fatal.
type loopInitOutcome struct {
	result LoopCliResult
	err    error
}

// loopInitAsync runs one init in a goroutine and hands its answer back over a channel.
func loopInitAsync(cwd string, argv ...string) <-chan loopInitOutcome {
	out := make(chan loopInitOutcome, 1)
	go func() {
		args, err := ParseLoopCliArgs(argv, cwd)
		if err != nil {
			out <- loopInitOutcome{err: err}
			return
		}
		result, err := RunLoopCli(args)
		out <- loopInitOutcome{result: result, err: err}
	}()
	return out
}

// loopPlanFile is slug's goalplan.json under cwd.
func loopPlanFile(cwd, slug string) string {
	return filepath.Join(cwd, ".crw", "goalplans", slug, "goalplan.json")
}

// loopCreatedLedgerRows reads slug's ledger and returns the detail of every created row, so a case
// can prove exactly one init created the plan.
func loopCreatedLedgerRows(t *testing.T, cwd, slug string) []string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(cwd, ".crw", "goalplans", slug, "ledger.jsonl"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	rows := []string{}
	for _, line := range strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n") {
		if line != "" && strings.Contains(line, "\"event\":\"created\"") {
			rows = append(rows, line)
		}
	}
	return rows
}

// TestLoopInitCreatesThePlanExclusivelyUnderConcurrency is c1 (P0, data loss). Init A is held after
// its outer absence check while init B on the same slug runs to completion; A then resumes. Exactly
// one plan must exist, it must be B's, and A must be refused with the "already exists" text and the
// created ledger must hold exactly one row. On the pre-fix code A's publication replaces B's plan
// (the replacing rename), which is the data loss this case pins.
func TestLoopInitCreatesThePlanExclusivelyUnderConcurrency(t *testing.T) {
	cwd := loopReadWorkspace(t)
	held, release := loopHoldAfterAbsenceCheck(t)
	const slug = "ship-the-export-feature"
	const objective = "Ship the export feature"

	first := loopInitAsync(cwd, "init", "--objective", objective, "--criterion", "criterion A")
	<-held // A passed its outer absence check and is paused before creating anything

	second := loopRun(t, cwd, "init", "--objective", objective, "--criterion", "criterion B")
	if second.Code != 0 {
		t.Fatalf("the second init was refused: %d %q", second.Code, second.Output)
	}
	release()

	got := <-first
	if got.err != nil {
		t.Fatalf("the held init failed: %v", got.err)
	}
	want := "loop init: a plan already exists at slug '" + slug + "' (use show/validate)"
	if got.result.Code != 1 || got.result.Output != want {
		t.Fatalf("the held init:\n got %d %q\nwant 1 %q", got.result.Code, got.result.Output, want)
	}

	plan := goalplan.ReadGoalplan(cwd, slug)
	if plan == nil {
		t.Fatalf("no plan at slug %q", slug)
	}
	if len(plan.Criteria) != 1 || plan.Criteria[0].Scenario != "criterion B" {
		t.Fatalf("the held init replaced the published plan: %+v", plan.Criteria)
	}
	if rows := loopCreatedLedgerRows(t, cwd, slug); len(rows) != 1 {
		t.Fatalf("created ledger rows = %d, want 1: %v", len(rows), rows)
	}
}

// TestLoopInitWritesNothingWhenTheSessionLockIsHeld is c2 (P1). With the session's lock file held by
// another holder, init --session must write no plan, no plan directory and no created row, and it
// must leave the session unbound; once the lock is released the same command succeeds and binds the
// slug. On the pre-fix code the plan and the created row are written before the lock is taken, so
// the failed init leaves an unbound plan that refuses the retry.
func TestLoopInitWritesNothingWhenTheSessionLockIsHeld(t *testing.T) {
	cwd := loopReadWorkspace(t)
	loopInitFastWaits(t) // the competing init never publishes, so do not spend the full wait budget
	gitInit(t, cwd)
	const id = "rec-lock"
	const slug = "bound-objective"
	loopSession(t, cwd, id)

	lockPath := state.StatePath(cwd, id) + ".lock"
	if err := os.WriteFile(lockPath, []byte(strconv.Itoa(loopDeadPID(t))), 0o666); err != nil {
		t.Fatal(err)
	}

	args, err := ParseLoopCliArgs([]string{"init", "--objective", "Bound objective", "--session", id}, cwd)
	if err != nil {
		t.Fatal(err)
	}
	result, err := RunLoopCli(args)
	if err == nil {
		t.Fatalf("init succeeded while the session lock was held: %d %q", result.Code, result.Output)
	}
	if _, statErr := os.Stat(loopPlanFile(cwd, slug)); !os.IsNotExist(statErr) {
		t.Fatalf("a failed init wrote a plan: %v", statErr)
	}
	if _, statErr := os.Stat(filepath.Join(cwd, ".crw", "goalplans", slug)); !os.IsNotExist(statErr) {
		t.Fatalf("a failed init wrote a plan directory: %v", statErr)
	}
	if rows := loopCreatedLedgerRows(t, cwd, slug); len(rows) != 0 {
		t.Fatalf("a failed init wrote a created row: %v", rows)
	}
	if bound := state.ReadState(cwd, id).Slug; bound != "" {
		t.Fatalf("a failed init bound a slug: %q", bound)
	}

	if err := os.Remove(lockPath); err != nil {
		t.Fatal(err)
	}
	again := loopRun(t, cwd, "init", "--objective", "Bound objective", "--session", id)
	if again.Code != 0 {
		t.Fatalf("the retry after the lock was released: %d %q", again.Code, again.Output)
	}
	if bound := state.ReadState(cwd, id).Slug; bound != slug {
		t.Fatalf("the retry bound %q, want %q", bound, slug)
	}
}

// TestLoopBlankSessionIsRefusedOnEveryVerb is c3 (P1). A --session value that is empty after
// trimming is not "absent": the trimmed value fails the canonical check, so every verb refuses it
// instead of resolving through the raw value (whose sanitized key is the 'missing' session) and
// printing a plan the caller never named. On the pre-fix code show/ready/validate print the plan
// bound to the 'missing' session and init treats the flag as absent and writes a plan.
func TestLoopBlankSessionIsRefusedOnEveryVerb(t *testing.T) {
	cwd := loopReadWorkspace(t)
	gitInit(t, cwd)
	loopSession(t, cwd, "missing")
	if bound := loopRun(t, cwd, "init", "--objective", "Private", "--session", "missing"); bound.Code != 0 {
		t.Fatalf("the setup init: %d %q", bound.Code, bound.Output)
	}
	if slug := state.ReadState(cwd, "missing").Slug; slug != "private" {
		t.Fatalf("the setup bound %q", slug)
	}

	for _, verb := range []string{"show", "ready", "validate"} {
		t.Run(verb, func(t *testing.T) {
			argv := []string{verb, "--session", "   "}
			if verb == "ready" {
				argv = append(argv, "--json")
			}
			result := loopRun(t, cwd, argv...)
			want := "loop " + verb + ": session id is not canonical"
			if result.Code != 1 || result.Output != want {
				t.Fatalf("got %d %q, want 1 %q", result.Code, result.Output, want)
			}
			if strings.Contains(result.Output, "private") {
				t.Fatalf("the refusal printed the plan bound to another session: %q", result.Output)
			}
		})
	}

	blank := loopRun(t, cwd, "init", "--objective", "Blank probe", "--session", "   ")
	want := "loop init: session id is not canonical"
	if blank.Code != 1 || blank.Output != want {
		t.Fatalf("init: got %d %q, want 1 %q", blank.Code, blank.Output, want)
	}
	if _, err := os.Stat(filepath.Join(cwd, ".crw", "goalplans", "blank-probe")); !os.IsNotExist(err) {
		t.Fatalf("the refused init wrote a plan: %v", err)
	}
}

// TestLoopInitRefusesADamagedPlanInsideTheCreationLock is c1's companion. A plan file that appears
// after the outer absence check is caught by the re-check inside the slug's goalplan write lock, and
// the bytes stay for repair instead of being replaced by the publication's rename.
func TestLoopInitRefusesADamagedPlanInsideTheCreationLock(t *testing.T) {
	cwd := loopReadWorkspace(t)
	const slug = "ship-the-export-feature"
	damaged := `{"objective": "Ship the export feature"`
	loopInitAfterAbsenceCheck = func() {
		dir := filepath.Join(cwd, ".crw", "goalplans", slug)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "goalplan.json"), []byte(damaged), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { loopInitAfterAbsenceCheck = nil })

	result := loopRun(t, cwd, "init", "--objective", "Ship the export feature")
	wantPrefix := "loop init: a plan file for slug '" + slug + "' already exists but could not be read (invalid-json)"
	if result.Code != 1 || !strings.HasPrefix(result.Output, wantPrefix) || !strings.HasSuffix(result.Output, "Nothing was written.") {
		t.Fatalf("got %d %q", result.Code, result.Output)
	}
	got, err := os.ReadFile(loopPlanFile(cwd, slug))
	if err != nil || string(got) != damaged {
		t.Fatalf("the damaged plan was replaced: %q %v", got, err)
	}
	if rows := loopCreatedLedgerRows(t, cwd, slug); len(rows) != 0 {
		t.Fatalf("the refused init wrote a created row: %v", rows)
	}
}

// TestLoopInitNamesThePublishedPlanWhenTheBindingFails is the commit-order case (failure class 2):
// the plan and its created row are published, then the binding write fails. The failure must say the
// plan is there instead of denying that anything was written, because a retry is refused with "a
// plan already exists" and would otherwise look like the defect this issue fixes. A write that
// published the state and then failed its directory sync is a visible binding, so it is a warning
// on a successful answer instead.
func TestLoopInitNamesThePublishedPlanWhenTheBindingFails(t *testing.T) {
	for _, tc := range []struct {
		name    string
		err     error
		publish bool
		want    []string
	}{
		{"a failure before the rename", errors.New("disk full"), false, []string{"are published", "disk full"}},
		{"a published but unsynced write", &state.PublishedError{Err: errors.New("directory sync failed")}, true, nil},
		// A link refusal from the binding write is a failure AFTER the plan and its created row were
		// published, so the answer must name them. On the pre-fix code the lock-acquisition branch caught
		// this error too and answered "Nothing was written." beside the published plan (CRW-646).
		{"a link refusal from the binding write", state.ErrStateRootSymlink, false, []string{"are published", "symlink"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cwd := loopReadWorkspace(t)
			gitInit(t, cwd)
			const id = "rec-bind"
			const slug = "bound-objective"
			loopSession(t, cwd, id)
			loopInitWriteStateHook = func(cwd string, next state.State) error {
				if tc.publish {
					// The real write lands the binding, then the injected failure stands in for the
					// directory sync that runs after the rename.
					if err := state.WriteState(cwd, next); err != nil {
						return err
					}
				}
				return tc.err
			}
			t.Cleanup(func() { loopInitWriteStateHook = nil })

			args, err := ParseLoopCliArgs([]string{"init", "--objective", "Bound objective", "--session", id}, cwd)
			if err != nil {
				t.Fatal(err)
			}
			result, runErr := RunLoopCli(args)
			if !tc.publish {
				if runErr == nil {
					t.Fatalf("got %d %q, want an error naming the published plan", result.Code, result.Output)
				}
				for _, want := range append(tc.want, slug, id) {
					if !strings.Contains(runErr.Error(), want) {
						t.Fatalf("the failure does not name %q: %v", want, runErr)
					}
				}
				if plan := goalplan.ReadGoalplan(cwd, slug); plan == nil {
					t.Fatalf("the failure denied a plan that is published")
				}
				if bound := state.ReadState(cwd, id).Slug; bound != "" {
					t.Fatalf("the failed binding bound %q", bound)
				}
				return
			}
			if runErr != nil {
				t.Fatalf("a published write must answer, not fail: %v", runErr)
			}
			if result.Code != 0 || !strings.HasPrefix(result.Output, "[crw loop: "+slug+"]") {
				t.Fatalf("got %d %q", result.Code, result.Output)
			}
			if !strings.Contains(result.Output, "session state was published but its directory could not be synced") {
				t.Fatalf("the answer carries no durability warning: %q", result.Output)
			}
			// The write really landed, so the binding and the created row must both be there: a
			// warning on a success that bound nothing would approve a half-created plan.
			if bound := state.ReadState(cwd, id).Slug; bound != slug {
				t.Fatalf("the warned success bound %q, want %q", bound, slug)
			}
			if rows := loopCreatedLedgerRows(t, cwd, slug); len(rows) != 1 {
				t.Fatalf("created ledger rows = %d, want 1: %v", len(rows), rows)
			}
		})
	}
}

// TestLoopInitRefusesALinkedPlanFile is the c1 companion for a plan path that is a symbolic link: the
// read path refuses the link (O_NOFOLLOW), so before the fix the absence predicate called the path
// absent and the publication's rename replaced the link. The link and its target's bytes must both
// survive.
func TestLoopInitRefusesALinkedPlanFile(t *testing.T) {
	cwd := loopReadWorkspace(t)
	const slug = "ship-the-export-feature"
	dir := filepath.Join(cwd, ".crw", "goalplans", slug)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(cwd, "elsewhere.json")
	stored := "{\"objective\": \"Ship the export feature\", \"slug\": \"" + slug + "\"}"
	if err := os.WriteFile(target, []byte(stored), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "goalplan.json")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}

	result := loopRun(t, cwd, "init", "--objective", "Ship the export feature")
	if result.Code != 1 || !strings.Contains(result.Output, "refusing to overwrite it") {
		t.Fatalf("got %d %q, want the refusal that keeps the link", result.Code, result.Output)
	}
	if info, err := os.Lstat(link); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("the plan link was replaced: %v %v", info, err)
	}
	if got, err := os.ReadFile(target); err != nil || string(got) != stored {
		t.Fatalf("the link target was rewritten: %q %v", got, err)
	}
}

// TestLoopInitAnswersAlreadyExistsWhenTheLockTimesOut is the other half of c1: the plan appears only
// AFTER the creation lock's own wait budget has run out, while the lock directory is still there. The
// loser must keep waiting for the winner's publication and then answer the criterion's "already
// exists" refusal, not the lock's busy message.
//
// The publication is synchronized at the wait's own entry (loopInitPlanWaitEntered), not scheduled on
// a timer: the seam publishes exactly when the loser has exhausted its acquisition budget and is about
// to wait, so the case cannot pass by a scheduler delay landing the plan earlier (CRW-646 d4). The
// entry flag is asserted, so removing the wait makes this case fail instead of silently passing.
func TestLoopInitAnswersAlreadyExistsWhenTheLockTimesOut(t *testing.T) {
	cwd := loopReadWorkspace(t)
	const slug = "ship-the-export-feature"
	loopInitFastWaits(t)
	// The competing init is a LIVE holder — this test process, whose pid the lock's owner.json names —
	// that publishes the plan only after the loser's acquisition budget has run out. The loser must
	// keep waiting for a live holder and then answer the criterion's refusal, not the busy message.
	holder := newLoopPlanHolder(t, cwd, slug, "Ship the export feature")
	loopInitAfterAbsenceCheck = func() { holder.plant() }
	loopInitPlanWaitEntered = holder.publishOnce()
	t.Cleanup(func() { loopInitAfterAbsenceCheck, loopInitPlanWaitEntered = nil, nil })

	result := loopRun(t, cwd, "init", "--objective", "Ship the export feature")
	want := "loop init: a plan already exists at slug '" + slug + "' (use show/validate)"
	if result.Code != 1 || result.Output != want {
		t.Fatalf("got %d %q\nwant 1 %q", result.Code, result.Output, want)
	}
	if !holder.entered.Load() {
		t.Fatal("the loser answered without entering the post-budget wait")
	}
	if plan := goalplan.ReadGoalplan(cwd, slug); plan == nil {
		t.Fatal("the competing init's plan is missing")
	}
}

// loopPlanHolder is a competing init that holds slug's goalplan creation lock and publishes the plan
// when the loser enters its post-budget wait, in this test process, so the lock's owner.json names a
// live pid and the loser's liveness check sees a live holder.
type loopPlanHolder struct {
	plant   func()
	publish func() error
	entered atomic.Bool
	err     error
}

// newLoopPlanHolder returns a holder whose plant creates the lock directory (owner.json naming this
// process) and whose publishOnce writes the plan exactly once, when the wait's entry seam runs.
func newLoopPlanHolder(t *testing.T, cwd, slug, objective string) *loopPlanHolder {
	t.Helper()
	dir := filepath.Join(cwd, ".crw", "goalplans", slug)
	h := &loopPlanHolder{}
	h.plant = func() {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Error(err)
			return
		}
		lock := filepath.Join(dir, ".goalplan.lock")
		if err := os.Mkdir(lock, 0o755); err != nil {
			t.Error(err)
			return
		}
		owner := "{\"pid\":" + strconv.Itoa(os.Getpid()) + ",\"acquiredAt\":\"2026-01-01T00:00:00.000Z\"}\n"
		if err := os.WriteFile(filepath.Join(lock, "owner.json"), []byte(owner), 0o600); err != nil {
			t.Error(err)
		}
	}
	h.publish = func() error {
		plan := goalplan.BuildGoalplan(goalplan.NewGoalplanInput{Objective: objective})
		return goalplan.WriteGoalplan(cwd, plan)
	}
	return h
}

// publishOnce returns the wait-entry seam: it records that the wait was entered and publishes the plan
// on that first entry, so the plan lands at the exact moment the loser gives up on acquisition.
func (h *loopPlanHolder) publishOnce() func() {
	return func() {
		if !h.entered.CompareAndSwap(false, true) {
			return
		}
		if err := h.publish(); err != nil {
			h.err = err
		}
	}
}

// TestLoopInitNamesThePublishedPlanWhenTheLedgerRowFails is the other commit-order case inside the
// creation critical section: the plan is published and the created ledger row then fails. The failure
// must name the published plan, because a retry answers "a plan already exists" and would otherwise
// look like the defect this issue fixes.
func TestLoopInitNamesThePublishedPlanWhenTheLedgerRowFails(t *testing.T) {
	cwd := loopReadWorkspace(t)
	const slug = "ship-the-export-feature"
	loopInitAppendLedgerHook = func(string, string, goalplan.GoalplanLedgerEntry) error {
		return errors.New("ledger write failed")
	}
	t.Cleanup(func() { loopInitAppendLedgerHook = nil })

	args, err := ParseLoopCliArgs([]string{"init", "--objective", "Ship the export feature"}, cwd)
	if err != nil {
		t.Fatal(err)
	}
	result, runErr := RunLoopCli(args)
	if runErr == nil {
		t.Fatalf("got %d %q, want an error naming the published plan", result.Code, result.Output)
	}
	for _, want := range []string{"is published", slug, "ledger write failed"} {
		if !strings.Contains(runErr.Error(), want) {
			t.Fatalf("the failure does not name %q: %v", want, runErr)
		}
	}
	if plan := goalplan.ReadGoalplan(cwd, slug); plan == nil {
		t.Fatalf("the failure denied a plan that is published")
	}
	if rows := loopCreatedLedgerRows(t, cwd, slug); len(rows) != 0 {
		t.Fatalf("a failed row append left a created row: %v", rows)
	}
}

// TestLoopInitWritesNothingWhenTheBoundCycleIsRefused is the Devin finding on this pull request:
// taking the session lock creates the state directory, so a bound cycle the workspace cannot close
// (no resolvable git source identity) would leave a fresh .crw behind while answering "Nothing was
// written". The source gate therefore runs before the lock too, and the workspace must be untouched.
func TestLoopInitWritesNothingWhenTheBoundCycleIsRefused(t *testing.T) {
	cwd := loopReadWorkspace(t)
	const id = "019a0000-0000-7000-8000-000000000177"

	result := loopRun(t, cwd, "init", "--objective", "bound probe in a non-git tree", "--session", id)
	if result.Code != 1 || !strings.Contains(result.Output, "no resolvable git source identity") {
		t.Fatalf("got %d %q", result.Code, result.Output)
	}
	if !strings.Contains(result.Output, "Nothing was written") {
		t.Fatalf("the refusal does not claim nothing was written: %q", result.Output)
	}
	// The artifacts init owns are absent: no plan directory, no plan file, no created row and no
	// session state. The state root itself is the session lock's own home — taking the lock creates
	// it — and is deliberately not removed: a pre-lock observation cannot prove this call created it,
	// so acting on that guess could delete another writer's state root (CRW-646 c2).
	if _, err := os.Stat(filepath.Join(cwd, ".crw", "goalplans")); !os.IsNotExist(err) {
		t.Fatalf("the refused bound init wrote a plan directory: %v", err)
	}
	if _, err := os.Stat(state.StatePath(cwd, id)); !os.IsNotExist(err) {
		t.Fatalf("the refused bound init wrote session state: %v", err)
	}
}

// TestLoopInitCarriesOnWhenThePlanWriteOnlyFailedItsDirectorySync is the commit-order case of d2: the
// plan write published at the final path and then failed a step after the rename. The plan is
// visible, so init must still append the created row and write the binding, and carry the durability
// failure as a warning; aborting would leave a visible plan with no row and no binding, and the retry
// would be refused with "a plan already exists".
func TestLoopInitCarriesOnWhenThePlanWriteOnlyFailedItsDirectorySync(t *testing.T) {
	cwd := loopReadWorkspace(t)
	gitInit(t, cwd)
	const id = "rec-sync"
	const slug = "bound-objective"
	loopSession(t, cwd, id)
	loopInitWriteGoalplanHook = func(cwd string, plan *goalplan.Goalplan) error {
		if err := goalplan.WriteGoalplan(cwd, plan); err != nil {
			return err
		}
		return &state.PublishedError{Err: errors.New("directory sync failed")}
	}
	t.Cleanup(func() { loopInitWriteGoalplanHook = nil })

	result := loopRun(t, cwd, "init", "--objective", "Bound objective", "--session", id)
	if result.Code != 0 {
		t.Fatalf("the published plan must not abort init: %d %q", result.Code, result.Output)
	}
	if !strings.Contains(result.Output, "was published but its directory could not be synced") {
		t.Fatalf("the answer carries no goalplan durability warning: %q", result.Output)
	}
	if rows := loopCreatedLedgerRows(t, cwd, slug); len(rows) != 1 {
		t.Fatalf("created ledger rows = %d, want 1: %v", len(rows), rows)
	}
	if bound := state.ReadState(cwd, id).Slug; bound != slug {
		t.Fatalf("the binding was skipped: %q", bound)
	}
}

// TestLoopInitAnswersAlreadyExistsWhenTheSessionLockTimesOut is d1's session-lock half: when the
// competing init of the same session published the plan while this init waited for the session lock,
// the loser owes the criterion's "already exists" refusal rather than the lock file's own error.
func TestLoopInitAnswersAlreadyExistsWhenTheSessionLockTimesOut(t *testing.T) {
	cwd := loopReadWorkspace(t)
	gitInit(t, cwd)
	const id = "rec-slock"
	const slug = "bound-objective"
	loopSession(t, cwd, id)
	loopInitFastWaits(t)
	// The competing init is a LIVE holder — this test process, whose pid the lock file names — that
	// publishes the plan only after the loser's session-lock budget has run out. The loser must keep
	// waiting for a live holder and then answer the criterion's refusal, not the raw lock error.
	if err := os.WriteFile(state.StatePath(cwd, id)+".lock", []byte(strconv.Itoa(os.Getpid())), 0o666); err != nil {
		t.Fatal(err)
	}
	// The publication is synchronized at the post-budget entry (loopInitPlanWaitEntered), so it lands
	// exactly when the loser gives up on acquisition and cannot be pulled earlier by a scheduler delay
	// (CRW-646 d4).
	publish := make(chan error, 1)
	loopInitPlanWaitEntered = func() {
		plan := goalplan.BuildGoalplan(goalplan.NewGoalplanInput{Objective: "Bound objective"})
		publish <- goalplan.WriteGoalplan(cwd, plan)
	}
	t.Cleanup(func() { loopInitPlanWaitEntered = nil })

	result := loopRun(t, cwd, "init", "--objective", "Bound objective", "--session", id)
	want := "loop init: a plan already exists at slug '" + slug + "' (use show/validate)"
	if result.Code != 1 || result.Output != want {
		t.Fatalf("got %d %q\nwant 1 %q", result.Code, result.Output, want)
	}
	if err := <-publish; err != nil {
		t.Fatalf("the competing init could not publish: %v", err)
	}
}

// TestLoopInitDoesNotHangOnASpecialFileAtTheSessionLock is the hardening finding on this pull
// request: the liveness probe reads the lock file, and a FIFO with no writer at that path would block
// the read forever. The probe opens with O_NOFOLLOW|O_NONBLOCK and refuses a non-regular file, so the
// command answers instead of hanging.
func TestLoopInitDoesNotHangOnASpecialFileAtTheSessionLock(t *testing.T) {
	cwd := loopReadWorkspace(t)
	gitInit(t, cwd)
	const id = "rec-fifo"
	loopSession(t, cwd, id)
	loopInitFastWaits(t)
	if err := syscall.Mkfifo(state.StatePath(cwd, id)+".lock", 0o666); err != nil {
		t.Skipf("mkfifo is unavailable here: %v", err)
	}

	done := make(chan LoopCliResult, 1)
	go func() {
		args, err := ParseLoopCliArgs([]string{"init", "--objective", "Bound objective", "--session", id}, cwd)
		if err != nil {
			done <- LoopCliResult{Code: -1, Output: err.Error()}
			return
		}
		result, runErr := RunLoopCli(args)
		if runErr != nil {
			done <- LoopCliResult{Code: -2, Output: runErr.Error()}
			return
		}
		done <- result
	}()
	select {
	case result := <-done:
		if result.Code == 0 {
			t.Fatalf("init succeeded against a FIFO lock: %q", result.Output)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("init hung on a FIFO at the session lock path")
	}
}

// TestLoopInitRefusesALinkedStateRootBeforeWriting is the review finding on this pull request: taking
// the session lock creates .crw/sessions and the lock file, and those creates follow a symbolic link,
// so a linked state root would send them outside the workspace before any session check could refuse
// it. The root's shape is therefore settled before the lock, and nothing is written through the link.
func TestLoopInitRefusesALinkedStateRootBeforeWriting(t *testing.T) {
	cwd := loopReadWorkspace(t)
	target := filepath.Join(cwd, "elsewhere")
	if err := os.Mkdir(target, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(cwd, ".crw")); err != nil {
		t.Fatal(err)
	}
	const id = "rec-link"

	result := loopRun(t, cwd, "init", "--objective", "Probe", "--session", id)
	if result.Code != 1 || !strings.Contains(result.Output, "must not be a symlink") {
		t.Fatalf("got %d %q", result.Code, result.Output)
	}
	// Nothing reached the link's target: no sessions directory, no lock file, no plan.
	for _, leaked := range []string{"sessions", "goalplans"} {
		if _, err := os.Stat(filepath.Join(target, leaked)); !os.IsNotExist(err) {
			t.Fatalf("the refused init wrote %q through the linked root: %v", leaked, err)
		}
	}
}

// loopInitFastWaits shortens the pause between init's post-lock wait rounds so a case that drives
// that wait does not spend the production budget in it, while leaving the wait long enough (limit x
// pause) for a competing publication scheduled just after the lock's own budget to land inside it.
// The wait itself follows the competing holder's liveness, which the cases control. It restores the
// seam when the test ends.
func loopInitFastWaits(t *testing.T) {
	t.Helper()
	pause := loopInitPlanWaitPause
	deadline := loopInitPlanWaitDeadline
	loopInitPlanWaitPause = func() { time.Sleep(time.Millisecond) }
	loopInitPlanWaitDeadline = 5 * time.Second
	t.Cleanup(func() { loopInitPlanWaitPause, loopInitPlanWaitDeadline = pause, deadline })
}

// loopDeadPID returns the pid of a process that has already exited, so a lock file naming it is an
// abandoned lock (its owner is gone) rather than a live holder. The wait follows a live holder's
// lifetime without a fixed round limit (CRW-646 c1), so a case that must fail on a held lock models an
// abandoned one, which is exactly what the oracle's stale-lock note describes.
func loopDeadPID(t *testing.T) int {
	t.Helper()
	for attempt := 0; attempt < 20; attempt++ {
		cmd := exec.Command("sh", "-c", "exit 0")
		if err := cmd.Run(); err != nil {
			t.Fatalf("start and reap a short-lived process: %v", err)
		}
		if pid := cmd.Process.Pid; !loopInitProcessAlive(pid) {
			return pid
		}
	}
	// Every reaped pid was reused by a live process (vanishingly unlikely); answer one above pid_max,
	// which no process can hold and the kernel therefore reports as gone.
	return 1 << 22
}

// TestLoopInitDoesNotWaitForAReleasedSessionLock is d2 (P1): the holder of the session lock released
// it without publishing a plan. An absent lock file is the holder's ordinary release, not a live
// holder, so the waiter must take the lock itself and complete instead of waiting out its whole budget
// for a plan no one will publish. On the pre-fix code every read error, ENOENT included, read as
// "alive", so the loser spent the full wait and then answered the lock's error.
func TestLoopInitDoesNotWaitForAReleasedSessionLock(t *testing.T) {
	cwd := loopReadWorkspace(t)
	gitInit(t, cwd)
	const id = "rec-released"
	const slug = "bound-objective"
	loopSession(t, cwd, id)
	loopInitFastWaits(t)
	lockPath := state.StatePath(cwd, id) + ".lock"
	if err := os.WriteFile(lockPath, []byte(strconv.Itoa(os.Getpid())), 0o666); err != nil {
		t.Fatal(err)
	}
	// The holder finishes without publishing, after the loser's own acquisition budget has run out, so
	// the loser meets the released lock inside its post-budget wait.
	released := make(chan struct{})
	go func() {
		time.Sleep(400 * time.Millisecond)
		_ = os.Remove(lockPath)
		close(released)
	}()

	result := loopRun(t, cwd, "init", "--objective", "Bound objective", "--session", id)
	<-released
	if result.Code != 0 {
		t.Fatalf("the init after the lock was released: %d %q", result.Code, result.Output)
	}
	if bound := state.ReadState(cwd, id).Slug; bound != slug {
		t.Fatalf("the init after the release bound %q, want %q", bound, slug)
	}
}

// TestLoopInitAnswersTheLockRecoveryWhenALockHasNoReadableOwner is d3's bound: a lock whose owner
// metadata is absent or unreadable (a foreign or corrupt lock, not a just-started holder) is followed
// only for loopInitPlanWaitGrace rounds and then answers the shared lock's own recovery text — never a
// claim that nothing is there, and never a raw error a caller cannot act on. The grace is the test seam.
func TestLoopInitAnswersTheLockRecoveryWhenALockHasNoReadableOwner(t *testing.T) {
	cwd := loopReadWorkspace(t)
	const slug = "ship-the-export-feature"
	dir := filepath.Join(cwd, ".crw", "goalplans", slug)
	loopInitFastWaits(t)
	// A lock directory with NO owner.json: the state a foreign or corrupt lock has, and the state a
	// holder has for the instant between mkdir and the owner write. The waiter must not hang on it.
	loopInitAfterAbsenceCheck = func() {
		if err := os.MkdirAll(filepath.Join(dir, ".goalplan.lock"), 0o755); err != nil {
			t.Error(err)
		}
	}
	t.Cleanup(func() { loopInitAfterAbsenceCheck = nil })

	result := loopRun(t, cwd, "init", "--objective", "Ship the export feature")
	if result.Code != 1 {
		t.Fatalf("got %d %q, want the lock's recovery answer", result.Code, result.Output)
	}
	for _, want := range []string{"is busy", "lock directory", "remove"} {
		if !strings.Contains(result.Output, want) {
			t.Fatalf("the recovery answer does not name %q: %q", want, result.Output)
		}
	}
	if _, err := os.Stat(loopPlanFile(cwd, slug)); !os.IsNotExist(err) {
		t.Fatalf("the refused init wrote a plan: %v", err)
	}
}

// TestLoopInitWaitsForALiveWinnerWithoutARoundLimit is c1's unconditional answer: a live winner that
// publishes only after many rounds still gets its plan seen by the loser, because the wait follows the
// holder's lifetime and not a fixed round count (CRW-646 c1, d4). The holder here stays live and
// publishes well past any small round budget a fixed limit would have set.
func TestLoopInitWaitsForALiveWinnerWithoutARoundLimit(t *testing.T) {
	cwd := loopReadWorkspace(t)
	const slug = "ship-the-export-feature"
	loopInitFastWaits(t)
	holder := newLoopPlanHolder(t, cwd, slug, "Ship the export feature")
	loopInitAfterAbsenceCheck = func() { holder.plant() }
	// Publish only after a long stretch of the loser's wait, well past a small fixed limit, and stop the
	// holder being "live" only after it has published: the owner is this test process throughout.
	loopInitPlanWaitEntered = func() {
		go func() {
			time.Sleep(300 * time.Millisecond) // ~300 fast-wait rounds, far past a small backstop
			_ = holder.publish()
		}()
	}
	t.Cleanup(func() { loopInitAfterAbsenceCheck, loopInitPlanWaitEntered = nil, nil })

	result := loopRun(t, cwd, "init", "--objective", "Ship the export feature")
	want := "loop init: a plan already exists at slug '" + slug + "' (use show/validate)"
	if result.Code != 1 || result.Output != want {
		t.Fatalf("got %d %q\nwant 1 %q", result.Code, result.Output, want)
	}
}

// TestLoopInitRefusesALinkedSessionsDirectory is d1's companion on the session side: the lock file is
// created relative to a descriptor opened with O_NOFOLLOW, so a link standing at .crw/sessions is
// refused instead of followed, and neither the lock file nor a session file lands in the link's target.
func TestLoopInitRefusesALinkedSessionsDirectory(t *testing.T) {
	cwd := loopReadWorkspace(t)
	gitInit(t, cwd) // so the refusal is the link, not the source-identity gate that follows it
	target := filepath.Join(cwd, "elsewhere")
	if err := os.MkdirAll(filepath.Join(target, "sessions"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(cwd, ".crw"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(target, "sessions"), filepath.Join(cwd, ".crw", "sessions")); err != nil {
		t.Fatal(err)
	}
	const id = "rec-linked-sessions"

	result := loopRun(t, cwd, "init", "--objective", "Probe", "--session", id)
	if result.Code != 1 || !strings.Contains(result.Output, "must not be a symlink") {
		t.Fatalf("got %d %q", result.Code, result.Output)
	}
	entries, err := os.ReadDir(filepath.Join(target, "sessions"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("the refused init wrote through the linked sessions directory: %v", entries)
	}
}
