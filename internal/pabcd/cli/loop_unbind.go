// loop unbind: the CRW-146 verb that releases the goalplan slug a session is bound to. It has no oracle counterpart (CXC v0.2.40 has
// no verb that clears state.slug; `orchestrate reset` spreads the state and keeps it), so it is a CRW addition recorded in
// docs/port-cxc/known-defects/CRW-146.md, and it changes no oracle behaviour: reset, the D-close and the goal-complete gate are as
// they were.
//
// What it writes: the session state without its slug and without the work-phase pointer the Stop guard keeps for that slug, and one
// row in the transition ledger (IDLE to IDLE, reason "unbound", the released slug as its evidence). What it never touches: the
// goalplan file, the goalplan's own ledger (the outbox is drained for rows only, so a pending followup that would write a goalplan
// stays pending), any other field of the session. It runs from rest only (the judgement is
// fsm.ApplyHumanTransition's, shared with the chat command `orchestrate unbind`), so it cannot orphan a running cycle: a session
// stranded in a phase is first closed with `crw pabcd orchestrate reset`, which keeps the slug, and then released here.
package cli

import (
	"context"
	"errors"
	"io/fs"
	"os"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/fsm"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/hook"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
)

// loopUnbind runs loop unbind for the trimmed --session. A session with no state file binds nothing, so it is answered before the
// lock is taken and nothing is created.
func loopUnbind(ctx context.Context, args LoopCliArgs) (LoopCliResult, error) {
	sessionID := loopSessionID(args)
	if sessionID == "" {
		return LoopCliResult{Output: "loop unbind: --session <id> is required", Code: 1}, nil
	}
	if _, err := os.Lstat(state.StatePath(args.Cwd, sessionID)); errors.Is(err, fs.ErrNotExist) {
		return loopUnbindNothing(sessionID), nil
	}
	var answer LoopCliResult
	err := state.WithSessionLockContext(ctx, args.Cwd, sessionID, func() error {
		// A ledger row an earlier writer of this session left pending is recorded first, so this verb's row follows it (CRW-1097).
		// Rows only: an event's followup (a re-plan's plan-audit cleanup rewrites the goalplan and appends to its ledger) stays pending
		// for a writer that may run it, because this verb never writes a goalplan.
		hook.DrainSessionLedgerRows(args.Cwd, sessionID)
		var err error
		answer, err = loopUnbindLocked(ctx, args.Cwd, sessionID)
		return err
	})
	if err != nil {
		return LoopCliResult{}, err
	}
	return answer, nil
}

func loopUnbindNothing(sessionID string) LoopCliResult {
	return LoopCliResult{Output: "loop unbind: session " + sessionID + " binds no goalplan; nothing to release.", Code: 0}
}

// loopUnbindLocked is the verb with the session lock held: read, judge, publish the state, then record the row (the commit order of
// every writer in this package, CRW-811).
func loopUnbindLocked(ctx context.Context, cwd, sessionID string) (LoopCliResult, error) {
	cur, unreadable := state.ReadStateStrict(cwd, sessionID)
	if unreadable {
		return LoopCliResult{Output: "loop unbind: session " + sessionID + " has unreadable state; refusing to overwrite it\nNothing was written.", Code: 1}, nil
	}
	res := fsm.ApplyHumanTransition(cur, fsm.VerbUnbind, nil)
	if !res.OK {
		return LoopCliResult{Output: "loop " + workUnbindDiagnostic(res.Reason, cur) + "\nNothing was written.", Code: 1}, nil
	}
	if res.State == nil {
		return loopUnbindNothing(sessionID), nil
	}
	// The strict reader rebuilds records it cannot keep whole, and the write below would publish the rebuilt ones; every writer of the
	// session file refuses such a rewrite (the judgement loop init's binding write uses).
	if raw, err := os.ReadFile(state.StatePath(cwd, sessionID)); err == nil && (!state.RewriteKeepsStored(raw, cur) || state.DcloseRecoveryLegacy(cur)) {
		return LoopCliResult{Output: "loop unbind: session " + sessionID + " holds records a rewrite would change; refusing to overwrite it\nNothing was written.", Code: 1}, nil
	}
	if err := orchestrateInterruptCheck(ctx, nil); err != nil {
		return LoopCliResult{}, err
	}
	ev, err := orchestrateCommitEvent(cwd, cur, *res.State, res.Ledger)
	if err != nil {
		return LoopCliResult{}, err
	}
	published := orchestrateCommitWrite(nil, cwd, *res.State, ev)
	if !published.published {
		return LoopCliResult{}, published.err
	}
	rowErr := loopUnbindRecord(cwd, sessionID, ev)
	answer := "loop unbind: released goalplan '" + cur.Slug + "' from session " + sessionID + ". The goalplan file and its ledger were not changed."
	return LoopCliResult{Output: orchestrateCommitAnswer(answer, orchestrateCommitWarn("loop unbind", published, rowErr, state.PhaseIdle, state.PhaseIdle)...)}, nil
}

// loopUnbindRecord records the row of the published release: the session's outbox is drained for rows only, with ev known as
// published, so an earlier pending row goes first and no followup runs. The error is the reason ev's row is still pending, nil once
// it is in the ledger.
func loopUnbindRecord(cwd, sessionID string, ev *state.LedgerEvent) error {
	if ev == nil {
		return nil
	}
	report := hook.DrainSessionLedgerRows(cwd, sessionID, ev.ID)
	if !hook.LedgerEventStillPending(report, ev.ID) {
		return nil
	}
	if report.Err != nil {
		return report.Err
	}
	return errors.New("it is still pending")
}
