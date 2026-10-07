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
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

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
	gitInit(t, cwd)
	const id = "rec-lock"
	const slug = "bound-objective"
	loopSession(t, cwd, id)

	lockPath := state.StatePath(cwd, id) + ".lock"
	if err := os.WriteFile(lockPath, []byte(strconv.Itoa(os.Getpid())), 0o666); err != nil {
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
	}{
		{"a failure before the rename", errors.New("disk full"), false},
		{"a published but unsynced write", &state.PublishedError{Err: errors.New("directory sync failed")}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cwd := loopReadWorkspace(t)
			gitInit(t, cwd)
			const id = "rec-bind"
			const slug = "bound-objective"
			loopSession(t, cwd, id)
			loopInitWriteStateHook = func(string, state.State) error { return tc.err }
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
				for _, want := range []string{"are published", slug, id, "disk full"} {
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
		})
	}
}
