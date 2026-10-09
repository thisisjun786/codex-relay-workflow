// Loop CLI command library: goalplan-cli.ts at v0.2.40 (commit 3c1459ac), the read half.
//
// CRW-382 ported the structural parser (loop_args.go) and the renderers (loop_render.go). This file ports
// runGoalplanCli (:751-865) for the verbs that only read or create a plan - init, show, validate and ready -
// plus runReady (:461-525). The mutating verbs (steer, add-criterion, add-work-phase, add-task,
// complete-task, meet-criterion, ask, decide) are CRW-383 (C2) and live in loop_mutate.go.
//
// The caller's shape is the oracle's cli.ts dispatch: parse, then run, and a library error is the oracle's
// uncaught throw (the harness row answers it as "crw cli failed: "). No package-level initializer runs here.
package cli

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/crwdir"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/gate"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/goalplan"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/interview"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/source"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/source/session"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/text"
)

// LoopCliResult is GoalplanCliResult (:272-275): the text and the exit status, without writing process streams.
type LoopCliResult struct {
	Output string `json:"output"`
	Code   int    `json:"code"`
}

// loopNowISO is new Date().toISOString() for the timestamps this file stamps.
func loopNowISO() string { return time.Now().UTC().Format("2006-01-02T15:04:05.000Z") }

// loopOpt is the value of an optional flag, or "" when it is absent.
func loopOpt(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

// loopSessionID is the trimmed --session value, "" when absent.
func loopSessionID(args LoopCliArgs) string { return text.Trim(loopOpt(args.Session)) }

// loopInitAfterAbsenceCheck is a test seam: it runs after init's outer absence check and before the
// plan is created, so a test can hold one init there while a second init on the same slug completes
// and then prove the held init is refused instead of replacing that plan (CRW-646 c1). Production
// leaves it nil, so no call ever carries it.
var loopInitAfterAbsenceCheck func()

// loopInitWriteStateHook, when non-nil, replaces the binding write of init's --session branch so a
// test can drive the commit-order case where the plan and its created ledger row are published and
// the binding then fails (CRW-646, failure class 2). It is nil in production (an uninitialized
// variable, no package-level work at start).
var loopInitWriteStateHook func(string, state.State) error

// loopInitWriteState is init's binding write: the hook when a test set one, state.WriteState otherwise.
func loopInitWriteState(cwd string, next state.State) error {
	if loopInitWriteStateHook != nil {
		return loopInitWriteStateHook(cwd, next)
	}
	return state.WriteState(cwd, next)
}

// loopInitPollStep is the pause between the rounds of init's wait for another writer: the first step of
// the goalplan lock's retry schedule (GoalplanLockRetryDelaysMs).
const loopInitPollStep = 5 * time.Millisecond

// loopInitPlanWaitLimit bounds every wait init makes for another writer, whatever the holder's state: a
// live holder, a holder whose owner metadata is not written yet, and one whose owner cannot be read. It is
// the total of goalplan.GoalplanLockRetryDelaysMs (5+10+20+40 ms), the budget the goalplan lock gives a
// creator before it answers busy (CRW-982 c2). A winner that holds the lock longer than this is answered
// busy with nothing written, and the operator retries. loopInitPlanWaitEntered runs once when the wait
// begins, so a test can synchronize at the post-budget entry instead of guessing with a timer (CRW-646 d4);
// all three are test seams.
var (
	loopInitPlanWaitPause   = func() { time.Sleep(loopInitPollStep) }
	loopInitPlanWaitLimit   = 75 * time.Millisecond
	loopInitPlanWaitEntered func()
)

// loopInitGoalplanProbeSeam is a test seam: it runs before the goalplan lock's holder is probed, so a test can
// publish the plan inside the window between the plan check and the terminal answer (CRW-982 post-evaluation D2).
// Production leaves it nil.
var loopInitGoalplanProbeSeam func()

// loopInitGoalplanHolder maps slug's goalplan lock onto the shared wait vocabulary, so the goalplan
// wait and the session wait act on the same four answers.
func loopInitGoalplanHolder(cwd, slug string) loopInitHolder {
	if loopInitGoalplanProbeSeam != nil {
		loopInitGoalplanProbeSeam()
	}
	switch goalplan.GoalplanLockHolderState(cwd, slug) {
	case goalplan.GoalplanHolderLive:
		return loopInitHolderLive
	case goalplan.GoalplanHolderDead:
		return loopInitHolderDead
	case goalplan.GoalplanHolderGone:
		return loopInitHolderGone
	default:
		return loopInitHolderUnknown
	}
}

// loopInitHolder is what a wait can learn about the process that holds a lock, told apart because the
// three answers call for different actions: a live holder is waited for, an abandoned lock is answered
// with the shared lock's busy message, and a lock that is already gone means the holder released
// without publishing and the waiter should try to take the lock itself.
type loopInitHolder int

const (
	loopInitHolderGone    loopInitHolder = iota // no lock file: released (or never created)
	loopInitHolderLive                          // the lock file names a running process
	loopInitHolderDead                          // the lock file is there, its owner is not
	loopInitHolderUnknown                       // the lock file is there, its owner is unreadable
)

// loopInitSessionProbeSeam is a test seam: it runs after the session owner path is named and before the probe
// opens it, so a test can swap the lock file for a FIFO or a link inside that window (CRW-982 c3). Production
// leaves it nil.
var loopInitSessionProbeSeam func()

// loopInitSessionHolder reads the session lock: gone when the file is absent, dead when its pid is not
// running, live otherwise. An absent lock is the ordinary release path (state's lock removes the file
// when its critical section ends), so it must not be read as a live holder — that mistake waits out the
// whole budget for a plan no one will publish (CRW-646 d2). Metadata that is present but momentarily
// unreadable or empty is the ordinary state of a holder that has just created the file, so it counts as
// live rather than refusing a competing creator that is about to publish.
func loopInitSessionHolder(cwd, sessionID string) loopInitHolder {
	lockPath := state.StatePath(cwd, sessionID) + ".lock"
	if loopInitSessionProbeSeam != nil {
		loopInitSessionProbeSeam()
	}
	// The probe opens the owner with O_NOFOLLOW and O_NONBLOCK and checks the opened descriptor (CRW-982 c3), so
	// a link or a FIFO swapped in after the path was named is refused, never followed or blocked on.
	raw, err := goalplan.ReadLockOwnerFile(lockPath)
	if err != nil {
		switch {
		case errors.Is(err, fs.ErrNotExist):
			return loopInitHolderGone
		case errors.Is(err, goalplan.ErrLockOwnerNotRegular), errors.Is(err, syscall.ELOOP):
			// A FIFO, a link, a directory or a device at the lock path is no holder: nothing there can
			// publish a plan, and waiting on it would be a hang. The caller reports the lock's own error.
			return loopInitHolderDead
		}
		return loopInitHolderUnknown
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil || pid <= 0 {
		return loopInitHolderUnknown
	}
	if loopInitProcessAlive(pid) {
		return loopInitHolderLive
	}
	return loopInitHolderDead
}

// loopInitProcessAlive reports whether pid names a running process.
func loopInitProcessAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// loopInitWriteGoalplanHook, when non-nil, replaces the plan publication of init's creation step so a
// test can drive the case where the plan is published and a step after its rename fails (CRW-646,
// failure class 2). It is nil in production (an uninitialized variable, no package-level work at
// start).
var loopInitWriteGoalplanHook func(string, *goalplan.Goalplan) error

// loopInitWriteGoalplan is init's plan publication: the hook when a test set one,
// goalplan.WriteGoalplan otherwise.
func loopInitWriteGoalplan(cwd string, plan *goalplan.Goalplan) error {
	if loopInitWriteGoalplanHook != nil {
		return loopInitWriteGoalplanHook(cwd, plan)
	}
	return goalplan.WriteGoalplan(cwd, plan)
}

// loopInitAppendLedgerHook, when non-nil, replaces the created-row append of init's creation step so a
// test can drive the case where the plan is published and the row then fails (CRW-646, failure class 2).
// It is nil in production (an uninitialized variable, no package-level work at start).
var loopInitAppendLedgerHook func(string, string, goalplan.GoalplanLedgerEntry) error

// loopInitAppendLedger is init's created-row append: the hook when a test set one,
// goalplan.AppendGoalplanLedger otherwise.
func loopInitAppendLedger(cwd, slug string, entry goalplan.GoalplanLedgerEntry) error {
	if loopInitAppendLedgerHook != nil {
		return loopInitAppendLedgerHook(cwd, slug, entry)
	}
	return goalplan.AppendGoalplanLedger(cwd, slug, entry)
}

// loopInitAppendWarnings joins a warning line onto init's answer for each durability warning its
// writes produced, in the order they happened. No warning leaves the answer's bytes untouched.
func loopInitAppendWarnings(result LoopCliResult, warnings []string) LoopCliResult {
	for _, warning := range warnings {
		if warning != "" {
			result.Output += "\n" + warning
		}
	}
	return result
}

// RunLoopCli is runGoalplanCli (:751-865) for the verbs this issue owns. A non-nil error is the oracle's
// uncaught throw: a write that failed, or a lock status that could not be read.
func RunLoopCli(args LoopCliArgs) (LoopCliResult, error) {
	return RunLoopCliContext(context.Background(), args)
}

// RunLoopCliContext is RunLoopCli for a caller the first SIGINT can end (the loop row of cmd/crw serve,
// CRW-1074). Only steer takes the context: its batch read and its goalplan lock wait end with it, and it
// is read once more with the lock held, before the steering transaction's first write. A steer that ends
// that way returns the context's own error with nothing written; the caller answers Interrupted. Every
// other verb, and a context that can never end, behave as RunLoopCli always did.
func RunLoopCliContext(ctx context.Context, args LoopCliArgs) (LoopCliResult, error) {
	if args.Verb == LoopVerbHelp {
		return LoopCliResult{Output: RenderLoopHelp(), Code: 0}, nil
	}
	// Checked BEFORE the verb dispatch, init included (the mutating verbs excepted: the oracle checks
	// their session itself, with a text per verb, and loop_mutate.go keeps it), and on the TRIMMED value the verbs themselves
	// use: state paths sanitize the id, so a blank or non-canonical one would resolve to a DIFFERENT
	// session's state file and this verb would print or judge a plan the caller never named
	// (docs/port-cxc/known-defects/CRW-646.md, port: fixed). The oracle guards the ready verb alone
	// (:802-810) and tests its raw value, so a blanks-only --session skips its guard, reaches
	// resolveSlug, and reads the 'missing' session's plan (:821-839).
	if args.Session != nil && !loopIsMutatingVerb(args.Verb) && !state.IsCanonicalSessionID(loopSessionID(args)) {
		return LoopCliResult{Output: fmt.Sprintf("loop %s: session id is not canonical", args.Verb), Code: 1}, nil
	}
	if args.Verb == LoopVerbInit {
		return loopInit(args)
	}
	if loopIsMutatingVerb(args.Verb) {
		return loopRunMutating(ctx, args)
	}
	slug := ResolveLoopSlug(args)
	if slug == nil {
		return LoopCliResult{Output: fmt.Sprintf(
			"loop %s: --slug \"<text>\", --objective \"<text>\", or --session <id> (with a bound plan) is required",
			args.Verb), Code: 1}, nil
	}
	plan := goalplan.ReadGoalplan(args.Cwd, *slug)
	if plan == nil {
		// Issue #29: "no plan found" used to hide truncated writes and schema rejects.
		read := goalplan.ReadGoalplanDetailed(args.Cwd, *slug)
		return LoopCliResult{Output: DescribeLoopReadFailure(read, string(args.Verb), *slug), Code: 1}, nil
	}
	if args.Verb == LoopVerbShow {
		lock, err := goalplan.GoalplanWriteLockStatus(args.Cwd, plan.Slug, nil)
		if err != nil {
			return LoopCliResult{}, err
		}
		return LoopCliResult{Output: RenderLoopPlan(plan, &lock), Code: 0}, nil
	}
	if args.Verb == LoopVerbReady {
		return loopReady(args, plan), nil
	}
	return loopValidate(args, plan)
}

// loopInit is the init branch (:753-800): a real objective, no --surface, no plan of that slug yet, a
// resolvable source identity when the plan is bound to a session, then the plan, its created ledger row and
// the session's slug binding.
//
// Two write-order defects of the oracle's branch are fixed here (docs/port-cxc/known-defects/CRW-646.md,
// both port: fixed). The absence check and the plan's publication are one critical section, so of two
// concurrent inits on one slug exactly one creates the plan (c1, a data loss: the oracle checks first and
// its replacing rename then overwrites the plan the other init just published). And when --session is
// given, the session lock comes FIRST and is held across the plan creation, the created ledger row and
// the binding (c2: the oracle writes the plan and the row and only then takes the lock, so a lock it
// cannot take leaves an unbound plan that blocks the retry). The lock order is the bound D-close's:
// session lock outside, goalplan lock inside.
func loopInit(args LoopCliArgs) (LoopCliResult, error) {
	objective := text.Trim(loopOpt(args.Objective))
	if objective == "" {
		return LoopCliResult{Output: "loop init: --objective \"<text>\" is required", Code: 1}, nil
	}
	if args.SurfaceGiven {
		return LoopCliResult{Output: "loop init: --surface is not applied at init; bind the plan with --session and " +
			"register each surfaced criterion with crw pabcd loop add-criterion --session <id> --criterion <text> " +
			"--surface <logic|web|tui|desktop>\nNothing was written.", Code: 1}, nil
	}
	slug := interview.DeriveSlug(objective)
	if refusal, present := loopInitPlanRefusal(args.Cwd, slug); present {
		return refusal, nil
	}
	if loopInitAfterAbsenceCheck != nil {
		loopInitAfterAbsenceCheck()
	}
	sessionID := loopSessionID(args)
	if sessionID == "" {
		return loopInitCreate(args, slug, objective)
	}
	// The bound gate runs before the session lock, once: a refusal there writes nothing, and a .crw that is a
	// link never receives the lock's directory (CRW-982 c1). CheckBound is not repeated under the lock; the
	// state checks run under it (CRW-646 c2). The session lock is still taken ahead of the goalplan lock, the
	// order the bound D-close uses. Nothing is removed afterwards: a pre-lock observation cannot prove this call
	// created a directory or a file, so the refusal names only the artifacts this call did not write:
	// the plan, the created row and the binding.
	var answer LoopCliResult
	running := true
	if refusal, ok := loopInitBoundGate(args.Cwd, sessionID); !ok {
		return refusal, nil
	}
	err := state.WithSessionLock(args.Cwd, sessionID, func() error {
		result, err := loopInitBound(args, slug, objective, sessionID)
		answer = result
		running = false
		return err
	})
	if err != nil {
		// An error the callback returned is the callback's own: it already answers what was written (the
		// published plan, its row, the binding), so the lock-acquisition answers below must not rewrite it
		// as a lock failure or as "Nothing was written" (CRW-646).
		if !running {
			return LoopCliResult{}, err
		}
		// The lock's wait budget ran out (the create's EEXIST is what the lock returns when another
		// holder keeps its file). An error the callback itself returned is not this case.
		if errors.Is(err, fs.ErrExist) {
			return loopInitAfterSessionLock(args, slug, objective, sessionID, err)
		}
		return LoopCliResult{}, err
	}
	return answer, nil
}

// loopInitBoundGate is the bound gate init runs before it takes the session lock, and the only place init
// runs CheckBound. It refuses a .crw that is a symbolic link, because the state package's directory creation
// follows a link and the lock would otherwise create a sessions directory outside the workspace. It also
// refuses a session with no resolvable source identity (#133: a bound plan promises a closable cycle, and
// without a source identity the cycle would strand at C with no testReceiptPath). The source identity is read
// here, before the lock, so a change to the workspace between this check and the first write of the plan is
// not seen by it; that window is recorded in CRW-646.md. It writes nothing.
func loopInitBoundGate(cwd, sessionID string) (LoopCliResult, bool) {
	if info, err := os.Lstat(filepath.Join(cwd, crwdir.DirName)); err == nil && info.Mode()&fs.ModeSymlink != 0 {
		return LoopCliResult{Output: "loop init: " + crwdir.DirName + " is a symbolic link; refusing to create the session lock through it.\nNothing was written.", Code: 1}, false
	}
	if verdict := session.CheckBound(cwd, sessionID); !verdict.OK {
		return LoopCliResult{Output: "loop init: " + verdict.Reason + "\nNothing was written.", Code: 1}, false
	}
	return LoopCliResult{}, true
}

// loopInitAfterSessionLock answers a session lock this init could not take. It follows the competing
// holder's liveness, which is the only thing that tells the three cases apart (CRW-646 c1, d2):
//
//   - a LIVE holder may still publish the plan this init must refuse, so keep looking for it;
//   - a lock file that is GONE is the holder's ordinary release, so this init tries to take the lock
//     itself and run the bound creation, rather than waiting for a plan no one will publish;
//   - a lock file whose owner is DEAD is an abandoned lock, answered with the shared lock's own busy
//     message, that lock's documented recovery.
//
// The wait ends the moment the plan appears (CRW-646 c1), when the holder's process is gone, or at
// loopInitPlanWaitLimit (CRW-982 c2), so a lock whose pid was reused by an unrelated process cannot make
// init hang, and a live holder that outlasts the limit gets the busy answer rather than an endless wait.
func loopInitAfterSessionLock(args LoopCliArgs, slug, objective, sessionID string, lockErr error) (LoopCliResult, error) {
	// The wait's entry seam: this call runs only after the session lock's own acquisition budget ran out,
	// so the seam marks exactly the post-budget moment a test synchronizes a competing publication at
	// (CRW-646 d4).
	if loopInitPlanWaitEntered != nil {
		loopInitPlanWaitEntered()
	}
	deadline := time.Now().Add(loopInitPlanWaitLimit)
	for {
		if result, present := loopInitPlanRefusal(args.Cwd, slug); present {
			return result, nil
		}
		switch loopInitSessionHolder(args.Cwd, sessionID) {
		case loopInitHolderGone:
			// The holder released without publishing a plan, so the lock is free: take it and run the
			// bound creation, the same critical section this init would have entered had it won.
			var answer LoopCliResult
			ran := true
			err := state.WithSessionLock(args.Cwd, sessionID, func() error {
				result, err := loopInitBound(args, slug, objective, sessionID)
				answer = result
				ran = false
				return err
			})
			if err == nil {
				return answer, nil
			}
			// An error the callback returned is the callback's own (a failed binding names the published
			// plan and its row); it must not be rewritten as a lock-acquisition answer here either
			// (CRW-646).
			if !ran {
				return LoopCliResult{}, err
			}
			if !errors.Is(err, fs.ErrExist) {
				return LoopCliResult{}, err
			}
			lockErr = err // another init took the lock in the window; keep waiting for its plan
		case loopInitHolderDead:
			// A plan published while the dead holder was probed is the answer (CRW-982 post-evaluation D2).
			if result, present := loopInitPlanRefusal(args.Cwd, slug); present {
				return result, nil
			}
			return LoopCliResult{}, lockErr
		}
		if time.Now().After(deadline) {
			// The plan is checked once more before the terminal busy answer, so a plan published during the probe
			// wins over it (CRW-982 post-evaluation D2).
			if result, present := loopInitPlanRefusal(args.Cwd, slug); present {
				return result, nil
			}
			return loopInitSessionBusy(args.Cwd, sessionID), nil
		}
		loopInitPlanWaitPause()
	}
}

// loopInitSessionBusy is init's answer when the session lock's holder outlasts the wait limit (CRW-982 c2).
// The lock is held by another writer and nothing was written; this is not the "a plan already exists" answer.
func loopInitSessionBusy(cwd, sessionID string) LoopCliResult {
	return LoopCliResult{Output: fmt.Sprintf(
		"loop init: session %s is held by another writer that did not finish within %s; the session lock %s is held and nothing was written.",
		sessionID, loopInitPlanWaitLimit, state.StatePath(cwd, sessionID)+".lock"), Code: 1}
}

// loopInitGoalplanBusy is init's answer when the slug's goalplan lock outlasts the wait limit (CRW-982 c2).
// The lock is held by another writer and nothing was written; this is not the "a plan already exists" answer.
func loopInitGoalplanBusy(cwd, slug string) LoopCliResult {
	dir, _ := goalplan.GoalplanWriteLockDir(cwd, slug)
	return LoopCliResult{Output: fmt.Sprintf(
		"loop init: the goalplan lock at slug '%s' is held by another writer that did not finish within %s; the lock is held and nothing was written. Lock directory: %s",
		slug, loopInitPlanWaitLimit, dir), Code: 1}
}

// loopInitPlanRefusal is the one "a plan of this slug must not be created" predicate: init's outer
// check and the re-check inside the slug's goalplan write lock both take it, so the answer that
// refused the earlier check is the answer taken at the write. The oracle asks only whether a plan
// LOADED, so a truncated or structurally invalid plan file reads as absent and its rename replaces
// the bytes (docs/port-cxc/known-defects/CRW-646.md, port: fixed); this predicate refuses whenever
// the file is there, so a damaged plan stays for repair.
func loopInitPlanRefusal(cwd, slug string) (LoopCliResult, bool) {
	read := goalplan.ReadGoalplanDetailed(cwd, slug)
	if read.Diagnostic == nil {
		return LoopCliResult{Output: fmt.Sprintf("loop init: a plan already exists at slug '%s' (use show/validate)", slug), Code: 1}, true
	}
	if loopPlanFileExists(cwd, slug) {
		return LoopCliResult{Output: fmt.Sprintf(
			"loop init: a plan file for slug '%s' already exists but could not be read (%s); refusing to overwrite it\nNothing was written.",
			slug, read.Diagnostic.Kind), Code: 1}, true
	}
	return LoopCliResult{}, false
}

// loopInitBound is the --session branch of init, run with the session lock already held: the state gates,
// then the plan's creation, then the slug binding. The source identity was checked once, by loopInitBoundGate,
// before the lock. The state gates run under the lock, so nothing they refuse can leave a half-written binding,
// and a plan it creates is always followed by the binding in the same critical section (CRW-646 c2).
func loopInitBound(args LoopCliArgs, slug, objective, sessionID string) (LoopCliResult, error) {
	// ReadState answers a fresh IDLE state for a file it cannot decode, so binding a slug through it would
	// replace a damaged session state with a default and lose the original bytes
	// (docs/port-cxc/known-defects/CRW-646.md, port: fixed). Refuse before anything is written.
	next, unreadable := state.ReadStateStrict(args.Cwd, sessionID)
	if unreadable {
		return LoopCliResult{Output: "loop init: session " + sessionID + " has unreadable state; refusing to overwrite it\nNothing was written.", Code: 1}, nil
	}
	// The strict reader also rebuilds records it cannot keep whole, so the write-back below would replace
	// them with the rebuilt ones (docs/port-cxc/known-defects/CRW-646.md, port: fixed). Every other writer
	// of the session file refuses such a rewrite by decision; this one follows the same rule, through the
	// shared judgement plus the per-writer legacy refusal.
	if raw, err := os.ReadFile(state.StatePath(args.Cwd, sessionID)); err == nil && (!state.RewriteKeepsStored(raw, next) || state.DcloseRecoveryLegacy(next)) {
		return LoopCliResult{Output: "loop init: session " + sessionID + " holds records a rewrite would change; refusing to overwrite it\nNothing was written.", Code: 1}, nil
	}
	result, err := loopInitCreate(args, slug, objective)
	if err != nil || result.Code != 0 {
		return result, err
	}
	next.Slug = slug
	writeErr := loopInitWriteState(args.Cwd, next)
	// The plan and its created ledger row are published before this write, so a failure here is never
	// "nothing was written": the published plan is a completed effect and is not rolled back (the
	// state.PublishedError rule). A write that reached the final path but could not sync its directory
	// is a binding every reader can see, so it is a warning on the answer, as the memory grant and the
	// D-close answer theirs (CRW-823/CRW-869); a failure before the rename is an error that names the
	// plan left behind, because a retry is refused with "a plan already exists" and would otherwise
	// look like the defect this issue fixes.
	answer := loopInitAppendWarnings(result, nil)
	if writeErr != nil {
		if !state.Published(writeErr) {
			return LoopCliResult{}, fmt.Errorf(
				"loop init: the plan and its created ledger row at slug '%s' are published, but the session %s binding could not be written: %w",
				slug, sessionID, writeErr)
		}
		answer.Output += "\n" + cliPublishedStateWarning(writeErr)
	}
	return answer, nil
}

// loopInitCreate builds slug's plan and publishes it, re-checking inside that slug's goalplan write
// lock that no plan is there. The check and the publish are one critical section, so of two
// concurrent inits on one slug exactly one creates the plan and the other is refused without writing
// (CRW-646 c1); WriteGoalplan's replacing rename is therefore never reached for an existing plan.
// The lock is the one the bound D-close takes, so an init and a close of the same slug queue on the
// same mkdir.
func loopInitCreate(args LoopCliArgs, slug, objective string) (LoopCliResult, error) {
	criteria := make([]goalplan.NewGoalplanCriterion, 0, len(args.Criteria))
	for _, scenario := range args.Criteria {
		criteria = append(criteria, goalplan.NewGoalplanCriterion{Scenario: scenario})
	}
	plan := goalplan.BuildGoalplan(goalplan.NewGoalplanInput{Objective: objective, Criteria: criteria, SchemaVersion: args.SchemaVersion})
	// The creation lock's own budget is short (5+10+20+40 ms), and a competing init holds it across a
	// staged-file write, its fsync and the directory fsync, so the loser's budget can run out while the
	// winner is still publishing. The loser re-checks the plan and re-attempts the acquisition each round
	// within loopInitPlanWaitLimit (CRW-982 c2). A round that takes the lock runs the same check-and-publish
	// body, so a holder that released without publishing is followed. The wait ends when the plan appears
	// ("a plan already exists", CRW-646 c1), when the holder's process is gone (the shared lock's busy
	// message), or when the limit passes, whatever the holder's state: a live holder, a holder whose
	// owner.json is not written yet, and one whose owner cannot be read all get the same bound. The limit
	// answer says the lock is held and nothing was written, so it is never read as an existing plan.
	entered := false
	deadline := time.Now().Add(loopInitPlanWaitLimit)
	for {
		var refusal *LoopCliResult
		warnings := []string{}
		locked, err := goalplan.WithGoalplanCreationLock(args.Cwd, slug, func() error {
			if result, present := loopInitPlanRefusal(args.Cwd, slug); present {
				refusal = &result
				return nil
			}
			// A plan write that published at the final path and then failed the directory sync is a
			// written plan: the bytes and the criteria are there for every reader, so the created row
			// and the binding still run and the durability failure is carried as a warning, exactly as
			// steering and review-round open do (CRW-793/CRW-823). A failure before the rename
			// published nothing and stays an error.
			if err := loopInitWriteGoalplan(args.Cwd, plan); err != nil {
				if !state.Published(err) {
					return err
				}
				warnings = append(warnings, cliPublishedGoalplanWarning(slug, err))
			}
			if err := loopInitAppendLedger(args.Cwd, slug, goalplan.GoalplanLedgerEntry{
				Ts: loopNowISO(), Slug: slug, Event: goalplan.EventCreated,
				Detail: "init objective=\"" + objective + "\" criteria=" + fmt.Sprint(len(args.Criteria)),
			}); err != nil {
				// The plan is published before this row, so a failure here is not "nothing was
				// written": the retry answers "a plan already exists", which would otherwise look like
				// the defect this issue fixes. Name the published plan, as the binding failure does.
				return fmt.Errorf("loop init: the plan at slug '%s' is published, but its created ledger row could not be appended: %w", slug, err)
			}
			return nil
		}, nil)
		if err != nil {
			return LoopCliResult{}, err
		}
		if locked.Kind != "locked" {
			if refusal != nil {
				return *refusal, nil
			}
			return loopInitAppendWarnings(LoopCliResult{Output: RenderLoopPlan(goalplan.ReadGoalplan(args.Cwd, slug), nil), Code: 0}, warnings), nil
		}
		if !entered {
			entered = true
			if loopInitPlanWaitEntered != nil {
				loopInitPlanWaitEntered()
			}
		}
		if result, present := loopInitPlanRefusal(args.Cwd, slug); present {
			return result, nil
		}
		holder := loopInitGoalplanHolder(args.Cwd, slug)
		if holder == loopInitHolderDead {
			if result, present := loopInitPlanRefusal(args.Cwd, slug); present {
				return result, nil
			}
			return LoopCliResult{Output: "loop init: " + locked.Reason, Code: 1}, nil
		}
		if time.Now().After(deadline) {
			if result, present := loopInitPlanRefusal(args.Cwd, slug); present {
				return result, nil
			}
			return loopInitGoalplanBusy(args.Cwd, slug), nil
		}
		if holder == loopInitHolderGone {
			continue // the lock was released without publishing: re-attempt the acquisition now
		}
		loopInitPlanWaitPause()
	}
}

// loopPlanFileExists reports whether anything occupies slug's plan path. A regular file is the plan; a
// symbolic link or another non-directory entry is something a replacing rename would destroy, so it
// counts as present too (the read path refuses a link through O_NOFOLLOW, so without this the predicate
// would read "absent" and the publication would replace the link). A path the slug resolver refuses - a
// linked state root, say - is not an existing plan file: the write path reports that refusal itself,
// exactly as the oracle's writeGoalplan does.
func loopPlanFileExists(cwd, slug string) bool {
	dir, err := goalplan.GoalplanDir(cwd, slug)
	if err != nil {
		return false
	}
	info, err := os.Lstat(filepath.Join(dir, goalplan.GoalplanFile))
	return err == nil && !info.IsDir()
}

// loopReadyPhaseRow, loopReadyTaskRow, loopReadyOpenDecisionRow and loopReadyAwaitingRow are runReady's JSON
// rows (:490-501). They are structs, not maps, because the recorded expectation compares the exact bytes and
// the oracle's key order is the literal's.
type loopReadyPhaseRow struct {
	ID        string   `json:"id"`
	Title     string   `json:"title"`
	Status    string   `json:"status"`
	DependsOn []string `json:"dependsOn"`
}

type loopReadyTaskRow struct {
	WorkPhaseID string `json:"workPhaseId"`
	ID          string `json:"id"`
	Title       string `json:"title"`
}

type loopReadyOpenDecisionRow struct {
	ID             string   `json:"id"`
	Question       string   `json:"question"`
	Recommendation *string  `json:"recommendation,omitempty"`
	Options        []string `json:"options,omitempty"`
	AskedAt        string   `json:"askedAt"`
}

type loopReadyAwaitingRow struct {
	WorkPhaseID string   `json:"workPhaseId"`
	DecisionIDs []string `json:"decisionIds"`
}

// loopReadyDoc is the object runReady's --json form prints (:486-502). The two decision arrays are pointers so
// that "the plan has no decisions key" (absent) stays apart from "it has an empty one" ([]).
type loopReadyDoc struct {
	Slug              string                      `json:"slug"`
	ReadyWorkPhases   []loopReadyPhaseRow         `json:"readyWorkPhases"`
	ReadyTasks        []loopReadyTaskRow          `json:"readyTasks"`
	OpenDecisions     *[]loopReadyOpenDecisionRow `json:"openDecisions,omitempty"`
	AwaitingDecisions *[]loopReadyAwaitingRow     `json:"awaitingDecisions,omitempty"`
}

// loopReady is runReady (:461-525). Integrity is checked FIRST: listing ready items out of a plan with a
// duplicate id or a dangling edge would hand back a confident answer computed from a graph the plan itself
// rejects.
func loopReady(args LoopCliArgs, plan *goalplan.Goalplan) LoopCliResult {
	reasons := append(goalplan.GoalplanDefinitionIntegrityReasons(plan), goalplan.GoalplanDependencyCompletionReasons(plan)...)
	if len(reasons) > 0 {
		lines := []string{"loop ready: " + plan.Slug + " has an invalid dependency graph"}
		for _, reason := range reasons {
			lines = append(lines, "  - "+reason)
		}
		return LoopCliResult{Output: strings.Join(lines, "\n"), Code: 1}
	}
	phases := goalplan.ReadyWorkPhases(plan)
	tasks := goalplan.ReadyTasks(plan)
	phaseRows := make([]loopReadyPhaseRow, 0, len(phases))
	for _, phase := range phases {
		dependsOn := phase.DependsOn
		if dependsOn == nil {
			dependsOn = []string{}
		}
		phaseRows = append(phaseRows, loopReadyPhaseRow{ID: phase.ID, Title: phase.Title, Status: string(phase.Status), DependsOn: dependsOn})
	}
	taskRows := make([]loopReadyTaskRow, 0, len(tasks))
	for _, entry := range tasks {
		taskRows = append(taskRows, loopReadyTaskRow{WorkPhaseID: entry.WorkPhaseID, ID: entry.Task.ID, Title: entry.Task.Title})
	}
	openDecisions := []loopReadyOpenDecisionRow{}
	awaitingDecisions := []loopReadyAwaitingRow{}
	hasDecisions := plan.Decisions != nil
	if hasDecisions {
		for i := range plan.Decisions {
			decision := &plan.Decisions[i]
			if decision.Status != goalplan.DecisionOpen {
				continue
			}
			row := loopReadyOpenDecisionRow{ID: decision.ID, Question: decision.Question, AskedAt: decision.AskedAt}
			if decision.Recommendation != "" {
				recommendation := decision.Recommendation
				row.Recommendation = &recommendation
			}
			if decision.Options != nil {
				row.Options = decision.Options
			}
			openDecisions = append(openDecisions, row)
		}
		for i := range plan.WorkPhases {
			phase := &plan.WorkPhases[i]
			if phase.Status != goalplan.WorkPhasePending && phase.Status != goalplan.WorkPhaseInProgress {
				continue
			}
			ids := []string{}
			for _, id := range goalplan.OpenDecisionIDsForPhase(plan, phase) {
				for j := range plan.Decisions {
					if plan.Decisions[j].ID == id && plan.Decisions[j].Status == goalplan.DecisionOpen {
						ids = append(ids, id)
						break
					}
				}
			}
			if len(ids) > 0 {
				awaitingDecisions = append(awaitingDecisions, loopReadyAwaitingRow{WorkPhaseID: phase.ID, DecisionIDs: ids})
			}
		}
	}
	if args.JSON {
		doc := loopReadyDoc{Slug: plan.Slug, ReadyWorkPhases: phaseRows, ReadyTasks: taskRows}
		if hasDecisions {
			doc.OpenDecisions, doc.AwaitingDecisions = &openDecisions, &awaitingDecisions
		}
		// statusJSON is this package's JSON.stringify: no HTML escaping and U+2028/U+2029 kept, which is what
		// the oracle prints for a title or a question holding those characters.
		encoded, err := statusJSON(doc)
		if err != nil {
			return LoopCliResult{Output: err.Error(), Code: 1}
		}
		return LoopCliResult{Output: encoded, Code: 0}
	}
	lines := []string{"[crw loop ready: " + plan.Slug + "]"}
	lines = append(lines, loopReadyPhaseLine(phases), loopReadyTaskLine(tasks))
	if hasDecisions {
		lines = append(lines, loopOpenDecisionLine(openDecisions), loopAwaitingDecisionLine(awaitingDecisions))
	}
	return LoopCliResult{Output: strings.Join(lines, "\n"), Code: 0}
}

func loopReadyPhaseLine(phases []*goalplan.GoalplanWorkPhase) string {
	if len(phases) == 0 {
		return "readyWorkPhases: none"
	}
	parts := make([]string, 0, len(phases))
	for _, phase := range phases {
		parts = append(parts, phase.ID+" ("+phase.Title+")")
	}
	return "readyWorkPhases: " + strings.Join(parts, "; ")
}

func loopReadyTaskLine(tasks []goalplan.ReadyGoalplanTask) string {
	if len(tasks) == 0 {
		return "readyTasks: none"
	}
	parts := make([]string, 0, len(tasks))
	for _, entry := range tasks {
		parts = append(parts, entry.WorkPhaseID+"/"+entry.Task.ID+" ("+entry.Task.Title+")")
	}
	return "readyTasks: " + strings.Join(parts, "; ")
}

func loopOpenDecisionLine(decisions []loopReadyOpenDecisionRow) string {
	if len(decisions) == 0 {
		return "openDecisions: none"
	}
	parts := make([]string, 0, len(decisions))
	for _, decision := range decisions {
		parts = append(parts, decision.ID+" ("+decision.Question+")")
	}
	return "openDecisions: " + strings.Join(parts, "; ")
}

func loopAwaitingDecisionLine(entries []loopReadyAwaitingRow) string {
	if len(entries) == 0 {
		return "awaitingDecisions: none"
	}
	parts := make([]string, 0, len(entries))
	for _, entry := range entries {
		parts = append(parts, entry.WorkPhaseID+": "+strings.Join(entry.DecisionIDs, ", "))
	}
	return "awaitingDecisions: " + strings.Join(parts, "; ")
}

// loopValidate is the validate branch (:841-864): a read-only E8 context, so a schemaVersion 2 plan is
// reported on rather than refused for a missing context. Nothing here mutates state.
func loopValidate(args LoopCliArgs, plan *goalplan.Goalplan) (LoopCliResult, error) {
	switch sessionID := loopSessionID(args); {
	case sessionID != "":
		if _, err := session.Resolve(args.Cwd, sessionID); err != nil {
			return LoopCliResult{Output: "loop validate: SOURCE-ROOT: " + err.Error(), Code: 1}, nil
		}
	case plan.FinalGate != nil && plan.FinalGate.SourceIdentity != nil && plan.FinalGate.SourceIdentity.SourceRoot != nil:
		return LoopCliResult{Output: "loop validate: SOURCE-ROOT: pass --session <id> to validate a bound source worktree.", Code: 1}, nil
	}
	ctx := &goalplan.GoalplanValidationCtx{
		Cwd:                   args.Cwd,
		CaptureSourceIdentity: func(cwd string) goalplan.SourceIdentity { return loopCaptureSource(args, cwd) },
		CompareSource: func(a, b goalplan.SourceIdentity) source.Comparison {
			return source.Compare(a.Identity(), b.Identity())
		},
		ReadReceipt: func(path string, kind gate.ReceiptKind) (goalplan.GoalplanReceiptEvidence, error) {
			receipt, err := gate.ParseSourceBoundReceipt(path, args.Cwd, kind)
			if err != nil {
				return goalplan.GoalplanReceiptEvidence{}, err
			}
			return goalplan.GoalplanReceiptEvidence{SourceIdentity: receipt.SourceIdentity, ArtifactManifest: receipt.ArtifactManifest}, nil
		},
	}
	verdict := goalplan.ValidateGoalplan(plan, ctx)
	if verdict.OK {
		return LoopCliResult{Output: "[crw loop validate: " + plan.Slug + "] OK " + loopEmDash + " complete + all met criteria carry evidence", Code: 0}, nil
	}
	lines := []string{"[crw loop validate: " + plan.Slug + "] FAIL"}
	for _, reason := range verdict.Reasons {
		lines = append(lines, "  - "+reason)
	}
	return LoopCliResult{Output: strings.Join(lines, "\n"), Code: 1}, nil
}

// loopEmDash is the em dash the oracle's OK line carries (U+2014), written as an escape so the source stays
// pure ASCII.
const loopEmDash = "\u2014"

// loopCaptureSource is captureSourceIdentity: the session's bound worktree when it has one, cwd otherwise.
// The oracle's resolver throws on a broken binding and that throw reaches the validator's catch arm, so this
// panics with the same error, which finalGateCaptureCurrent recovers.
func loopCaptureSource(args LoopCliArgs, cwd string) goalplan.SourceIdentity {
	if sessionID := loopSessionID(args); sessionID != "" {
		identity, err := session.Capture(cwd, sessionID, session.CaptureOptions{})
		if err != nil {
			panic(err)
		}
		return loopSourceIdentity(identity)
	}
	return loopSourceIdentity(source.Capture(cwd, source.Options{}))
}

// loopSourceIdentity is a source identity as the state and goalplan records hold it.
func loopSourceIdentity(identity source.Identity) state.SourceIdentity {
	out := state.SourceIdentity{Kind: identity.Kind, CommitSha: identity.CommitSha, Dirty: identity.Dirty,
		CapturedAt: identity.CapturedAt, SourceRoot: identity.SourceRoot}
	if identity.TreeHash != "" {
		hash := identity.TreeHash
		out.TreeHash = &hash
	}
	return out
}
