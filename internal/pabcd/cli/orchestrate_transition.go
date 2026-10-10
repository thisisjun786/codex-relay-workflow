package cli

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/attest"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/fsm"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/goalplan"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/hook"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/host"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/interview"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/interview/ledger"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/review"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/source"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/source/session"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/text"
)

// The transition half of CXC v0.2.40 orchestrate-cli.ts (3c1459ac, :549-1131): what a mutation
// RunOrchestrateRead delegated (OrchestrateReadResult.SessionID) does to the session. The read prefix
// (:466-548) is that library's; this file starts at the state it reads. Ported as-is, texts with the
// authorized name substitution only (.codexclaw -> .crw, cxc orchestrate -> crw pabcd orchestrate).
//
// Two pieces belong to other issues: the D close (:725-1064) is a refusal CRW-756 and CRW-757 fill, and the
// verb row that reaches this code is CRW-758's. No verb row reaches it yet, so nothing user-visible depends on
// those gaps. The A>B review binding (:49-95, called at :688-691) and the B>C source gate (:701-717) are ported
// in orchestrate_review_binding.go (CRW-755).

// orchestrateTransitionTimestampLayout is Date.prototype.toISOString, as a ledger or goalplan row prints a time.
const orchestrateTransitionTimestampLayout = "2006-01-02T15:04:05.000Z"

// orchestrateTransitionPlanBinding is the {unit, epoch} the P>A edge minted, so A can bind a review round to
// the unit that edge validated (REVIEW-BINDING-01) instead of deriving a unit the caller names.
type orchestrateTransitionPlanBinding struct {
	unit  string
	epoch string
}

func orchestrateTransitionTimestamp() string {
	return time.Now().UTC().Format(orchestrateTransitionTimestampLayout)
}

// orchestrateTransitionMintEpoch is mintEpoch (:96-103): one nonce per edge, the UTC instant to the second
// plus three random bytes. The instant alone would collide on a fast re-plan, the case the epoch tells apart.
func orchestrateTransitionMintEpoch(prefix string) (string, error) {
	var raw [3]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return prefix + "-" + time.Now().UTC().Format("20060102150405") + "-" + hex.EncodeToString(raw[:]), nil
}

// orchestrateTransitionPlaceholderDid is PLACEHOLDER_DID of attest.ts:66, which the oracle keeps private to the
// I>P override path: ASCII folding, as a JavaScript /i without the u flag compares.
func orchestrateTransitionPlaceholderDid(s string) bool {
	folded := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		folded = append(folded, c)
	}
	switch string(folded) {
	case "tbd", "todo", "na", "n/a", "none", "done", "ok":
		return true
	}
	return s != "" && (strings.Trim(s, ".") == "" || strings.Trim(s, "-") == "")
}

// orchestrateTransitionStoredIdentity is a capture in the form a session file stores (state.SourceIdentity).
func orchestrateTransitionStoredIdentity(id source.Identity) state.SourceIdentity {
	out := state.SourceIdentity{Kind: id.Kind, CommitSha: id.CommitSha, Dirty: id.Dirty, CapturedAt: id.CapturedAt, SourceRoot: id.SourceRoot}
	if id.TreeHash != "" {
		out.TreeHash = &id.TreeHash
	}
	return out
}

// orchestrateTransitionWithArchitectHint is withArchitectHint (:456-464): a terminal entry reaches P without a
// UserPromptSubmit turn, so the formal-P consultation pointer is repeated there. Advice only; no gate.
func orchestrateTransitionWithArchitectHint(phase state.Phase, output string) string {
	if phase != state.PhaseP {
		return output
	}
	return output + " [formal P: architect proposal -> main executable plan -> same-architect reflection before A (crw-pabcd phase-plan)]"
}

// orchestrateTransitionPlanBindingOf is :1073-1074: the P>A edge keeps the unit and epoch it just minted,
// entering A another way keeps the session's own, and anywhere else drops both.
func orchestrateTransitionPlanBindingOf(binding *orchestrateTransitionPlanBinding, cur state.State, phase state.Phase) (unit, epoch *string) {
	if binding != nil {
		u, e := binding.unit, binding.epoch
		return &u, &e
	}
	if phase == state.PhaseA {
		return cur.PlanUnit, cur.PlanEpoch
	}
	return nil, nil
}

// orchestrateTransitionCheckEpoch is :1075-1077: entering C mints a check epoch, staying in C keeps the
// session's own, and anywhere else drops it, so re-checking invalidates the earlier receipt.
func orchestrateTransitionCheckEpoch(from, to state.Phase, current *string) (*string, error) {
	if to != state.PhaseC {
		return nil, nil
	}
	if from == state.PhaseC {
		return current, nil
	}
	epoch, err := orchestrateTransitionMintEpoch("c")
	if err != nil {
		return nil, err
	}
	return &epoch, nil
}

// orchestrateTransitionDClose is the D close (:725-1064), ported in orchestrate_dclose.go. This body stays a
// one-line call so a sibling issue's rewrite of the ordinary edge of this file merges without touching the D
// place, and the commit-hook seam lives on the inner function the D close tests call directly.
func orchestrateTransitionDClose(ctx context.Context, seams *orchestrateCommitSeams, cwd, sessionID, closePhaseID string, cur state.State, att *attest.Attestation, recovering bool) (CliResult, error) {
	seam := orchestrateDcloseSeam{}
	if seams != nil {
		seam.interrupt = seams.interrupt
	}
	return orchestrateDcloseContext(ctx, cwd, sessionID, closePhaseID, cur, att, recovering, seam)
}

// orchestrateTransitionStateWritable is this writer's half of the port's data-loss rule for the session file
// (docs/port-cxc/known-defects.md, "Found by the CRW-609 lossless rewrite guard"): the oracle publishes the
// state the reader rebuilt, so a stored lone surrogate, invalid UTF-8, a receipt past the cap or a record past
// the list cap is replaced or dropped. The port refuses instead, as every other session writer here does.
// The interview tracker is the second loss the rebuilt read would publish: ReconstructInterview caps every array at
// interview.MaxTrackerArray and drops an unnamed ontology entity, so the same write is refused when cliInterviewIntact
// says the file holds a record the rebuild cannot keep. The reason names which record the caller would lose.
func orchestrateTransitionStateWritable(cwd, sessionID string, kept state.State) (string, bool) {
	// The interview check runs first because cliVerdictsIntact now covers the tracker too, and the interview
	// loss must keep its own reason (the shared judgement's interview class, reached through that guard).
	if !cliInterviewIntact(cwd, sessionID) {
		return cliInterviewRefusalReason, false
	}
	if !cliVerdictsIntact(cwd, sessionID, len(kept.UnverifiedSubagents)) {
		return orchestrateTransitionUnverifiedRefusalReason, false
	}
	return "", true
}

// orchestrateTransitionUnverifiedRefusalReason is the reason for a rewrite that would cut or retype a stored unverified record.
const orchestrateTransitionUnverifiedRefusalReason = "session state holds records this rewrite would change; refusing to overwrite it"

// sessionAliasRefusalText is the one answer a mutating entry gives an explicit --session for which
// state.IsCanonicalSessionID is false (CRW-871). The read side and the library entry share it so the
// two cannot drift; the state library itself keeps sanitising, as its recorded oracle rows pin.
const sessionAliasRefusalText = "session id is not canonical"

// sessionAliasRefuse is that answer for one verb, byte for byte the text RunOrchestrateRead returns.
func sessionAliasRefuse(verb fsm.OrchestrateVerb) CliResult {
	return CliResult{Code: 1, Output: sessionAliasRefusalOutput(verb)}
}

// sessionAliasRefusalOutput is the text both entries answer with, so the read side and the library
// entry cannot drift apart.
func sessionAliasRefusalOutput(verb fsm.OrchestrateVerb) string {
	return "orchestrate " + VerbText(verb) + ": " + sessionAliasRefusalText
}

// orchestrateTransitionStateRefusal is the refusal shared by the three writes this file owns; reason is why the state is not writable.
func orchestrateTransitionStateRefusal(verb fsm.OrchestrateVerb, reason string) CliResult {
	return CliResult{Code: 1, Output: "orchestrate " + VerbText(verb) + ": " + reason + ". Nothing was written."}
}

// The commit order of this file's three write paths (CRW-811): the session state is published first and
// the transition-ledger row second, which is the oracle's own order (orchestrate-cli.ts :562-563, :653-655
// and :1119 then :1123). A failure before the publication is returned as an error and writes no row, so no
// row ever describes a transition that did not happen; once the state is at the final path - a success, or a
// *state.PublishedError when only the directory sync failed - the transition counts as done, the row is
// appended, and a row that cannot be written is a warning on a success answer, never a rollback. The work-phase
// gate of a bound session runs inside the goalplan write lock, so a plan another writer republishes between the
// gate's read and the state write is revalidated.
//
// CRW-1097: the row is prepared as a pending event of the session's ledger outbox before the publication,
// inside the session lock, and recorded by draining that outbox after it. A row that cannot be appended, or
// whose writer dies after the publication, stays pending and the next locked writer of the session (the next
// orchestrate call, the next hook) records it exactly once; the oracle's row is lost for good there, because the
// phase has moved and the same request is refused. Every call drains what an earlier one left before it
// writes, so the rows of one session keep the order the session went through its states.

// orchestrateCommitSeams carries the two dependencies the commit order needs a test to stage: the state
// publication and the goalplan write lock. They are fields, never package-level variables, as
// EvidenceResolveArgs.writeState is; nil means the real implementation.
type orchestrateCommitSeams struct {
	writeState   func(cwd string, next state.State) error
	lockGoalplan orchestrateCommitLockFunc
	// interrupt runs immediately before the pre-write cancellation check of the branch that is about
	// to write, so a test can cancel the invocation's context exactly between the session lock and the
	// first durable effect (CRW-871). It is a field, never package state; nil means no hook.
	interrupt func()
	// cleanup replaces the P>A plan-audit cleanup the publication runs in its goalplan lock (CRW-1100), so
	// a test can fail it. nil is hook.SupersedePlanAuditRounds.
	cleanup func(cwd, sessionID string, c hook.PlanAuditCleanup, plan *goalplan.Goalplan) error
}

// orchestrateCommitCleanup runs the P>A plan-audit cleanup, the test's seam when it set one.
func orchestrateCommitCleanup(seams *orchestrateCommitSeams, cwd, sessionID string, c hook.PlanAuditCleanup, plan *goalplan.Goalplan) error {
	if seams != nil && seams.cleanup != nil {
		return seams.cleanup(cwd, sessionID, c, plan)
	}
	return hook.SupersedePlanAuditRounds(cwd, sessionID, c, plan)
}

// orchestrateInterruptCheck is the pre-write cancellation check of CRW-871: the invocation's context
// is read once more after the session lock is held and immediately before the branch's first durable
// effect, so a first SIGINT that lands in that window leaves nothing written and the caller answers
// Interrupted (130). The seam runs first, so a test can cancel the context exactly here; the returned
// error is the context's own.
func orchestrateInterruptCheck(ctx context.Context, seams *orchestrateCommitSeams) error {
	var interrupt func()
	if seams != nil {
		interrupt = seams.interrupt
	}
	return orchestrateInterruptCheckWith(ctx, interrupt)
}

// orchestrateInterruptCheckWith is orchestrateInterruptCheck for a caller that holds the hook itself
// (the D close's own seam). The hook runs first, so a test can cancel the invocation exactly here.
func orchestrateInterruptCheckWith(ctx context.Context, interrupt func()) error {
	if interrupt != nil {
		interrupt()
	}
	return ctx.Err()
}

// orchestrateCommitLockFunc is goalplan.WithGoalplanWriteLock at one concrete result type, so a test can
// order a concurrent rebind between the unlocked gate read and the lock without a sleep.
type orchestrateCommitLockFunc func(cwd, slug string, fn func(*goalplan.Goalplan) (orchestrateCommitOutcome, error)) (goalplan.GoalplanWriteLockResult[orchestrateCommitOutcome], error)

// orchestrateCommitOutcome is one publication: whether the state reached the final path, the
// post-publication failure to warn about, the pre-publication failure to return, or the refusal the
// gate answered with instead of publishing.
type orchestrateCommitOutcome struct {
	published bool
	warning   error
	err       error
	refusal   *CliResult
	// cleanupDone is set when the P>A plan-audit cleanup the event carries was completed inside the
	// publication's goalplan lock (CRW-1100); cleanupErr is why it was not, nil when it was or when there is none. The reason
	// is kept apart from the row's, so an answer reports both when the ledger fails as well.
	cleanupDone bool
	cleanupErr  error
}

// orchestrateCommitEvent prepares, without writing it, the outbox event of the row a write records: the row
// the transition from cur to next appends. nil row is no event.
func orchestrateCommitEvent(cwd string, cur, next state.State, row *state.LedgerEntry) (*state.LedgerEvent, error) {
	if row == nil {
		return nil, nil
	}
	ev, err := state.NewLedgerEvent(cwd, cur, next, row, nil)
	if err != nil {
		return nil, err
	}
	return &ev, nil
}

// orchestrateCommitRecord records the row of a published write: the session's outbox is drained with ev
// known as published, so an earlier pending row goes first. The error is the reason ev's row is still
// pending, nil once it is in the ledger.
func orchestrateCommitRecord(cwd, sessionID string, ev *state.LedgerEvent) error {
	rowErr, _ := orchestrateCommitRecordAll(cwd, sessionID, ev, false, nil)
	return rowErr
}

// orchestrateCommitRecordAll is orchestrateCommitRecord for an event that may carry the P>A plan-audit
// cleanup (CRW-1100): rowErr is the reason the row is still pending, cleanupErr the reason the cleanup is,
// each nil once done and each reported on its own: a ledger that cannot be written does not hide a cleanup
// that did not finish. cleanupDone says the publication completed the cleanup itself, publishedCleanupErr is
// why it did not.
func orchestrateCommitRecordAll(cwd, sessionID string, ev *state.LedgerEvent, cleanupDone bool, publishedCleanupErr error) (rowErr, cleanupErr error) {
	if ev == nil {
		return nil, nil
	}
	var done []string
	if cleanupDone {
		done = []string{ev.ID}
	}
	report := hook.DrainSessionLedgerWith(cwd, sessionID, []string{ev.ID}, done)
	for _, pending := range report.Pending {
		if pending.ID != ev.ID {
			continue
		}
		reason := report.Err
		if reason == nil {
			reason = errors.New("it is still pending")
		}
		if pending.RowRecorded {
			return nil, reason
		}
		// The drain stopped at the row, so it never reached the cleanup: its reason is the publication's own.
		if len(ev.Followup) > 0 && !cleanupDone {
			cleanupErr = publishedCleanupErr
			if cleanupErr == nil {
				cleanupErr = errors.New("it is still pending")
			}
		}
		return reason, cleanupErr
	}
	return nil, nil
}

// orchestrateCommitCleanupWarn is the line a P>A answer carries when the re-plan's cleanup of this
// session's earlier plan_audit rounds could not finish (CRW-1100): the transition stands, and the cleanup
// is pending in the session's outbox.
func orchestrateCommitCleanupWarn(label string, err error) []string {
	if err == nil {
		return nil
	}
	return []string{label + ": warning: the re-plan's cleanup of this session's earlier plan_audit rounds is pending: " + err.Error() +
		"; the next orchestrate call or hook of this session finishes it"}
}

// orchestrateCommitWrite publishes next and reports whether it reached the final path. A failure before
// the rename (state.Published(err) false) is returned as an error: nothing was published, so the caller
// writes no row and the same verb can be retried. A post-rename failure is a warning: the state is visible
// to every reader, so the transition counts as done and the row is still appended (CRW-744).
//
// ev, when not nil, is the row's outbox event (CRW-1097): it is made pending immediately before the write, so
// a publication always has its row on record, and dropped again when the write fails before the rename. A
// pending event that cannot be recorded is a failure before the publication: nothing is published.
func orchestrateCommitWrite(seams *orchestrateCommitSeams, cwd string, next state.State, ev *state.LedgerEvent) orchestrateCommitOutcome {
	write := state.WriteState
	if seams != nil && seams.writeState != nil {
		write = seams.writeState
	}
	if ev != nil {
		if err := state.PrepareLedgerEvent(cwd, *ev); err != nil {
			return orchestrateCommitOutcome{err: err}
		}
	}
	err := write(cwd, next)
	switch {
	case err == nil:
		return orchestrateCommitOutcome{published: true}
	case state.Published(err):
		return orchestrateCommitOutcome{published: true, warning: err}
	}
	if ev != nil {
		_ = state.AbortLedgerEvent(cwd, *ev)
	}
	return orchestrateCommitOutcome{err: err}
}

// orchestrateCommitLock resolves the goalplan write lock, the real one unless a test supplied a seam.
func orchestrateCommitLock(seams *orchestrateCommitSeams) orchestrateCommitLockFunc {
	if seams != nil && seams.lockGoalplan != nil {
		return seams.lockGoalplan
	}
	return func(cwd, slug string, fn func(*goalplan.Goalplan) (orchestrateCommitOutcome, error)) (goalplan.GoalplanWriteLockResult[orchestrateCommitOutcome], error) {
		return goalplan.WithGoalplanWriteLock(cwd, slug, fn, nil)
	}
}

// orchestrateCommitPublish publishes the session state. On a gated edge of a bound session, and not a
// recovering D close, it runs inside the goalplan write lock: the plan is re-read and the work-phase
// binding revalidated there, and the state is published in the same lock, so a plan another writer
// republishes in between cannot approve a stale workPhaseId. Lock order is the session lock the caller
// holds, then this goalplan lock; no path in this tree takes them the other way. A busy lock refuses with
// its reason and publishes nothing; an absent or unreachable goalplan stays fail-open, as the unlocked read
// did; a plan the lock withholds from writers refuses and publishes nothing; any other lock failure is a Go
// error, as it is for the other writers of this package. A gated A>B edge also revalidates the review
// binding on the plan read inside the lock (CRW-975).
func orchestrateCommitPublish(ctx context.Context, seams *orchestrateCommitSeams, a OrchestrateCliArgs, cwd, sessionID string, cur, next state.State, to state.Phase, recoveringDclose bool, binding *orchestrateTransitionPlanBinding, ev *state.LedgerEvent) orchestrateCommitOutcome {
	if !attest.IsGated(cur.Phase, to) || cur.Slug == "" || recoveringDclose {
		return orchestrateCommitWrite(seams, cwd, next, ev)
	}
	locked, err := orchestrateCommitLock(seams)(cwd, cur.Slug, func(plan *goalplan.Goalplan) (orchestrateCommitOutcome, error) {
		// CRW-871: this callback runs inside the goalplan write lock, which is not context-aware, so the
		// invocation's context is read once more immediately before the callback's first durable effect
		// (the 032 housekeeping's plan write, or the state publication below). Cancelled while the caller
		// waited for this lock, nothing is written and the caller answers Interrupted (130).
		if err := orchestrateInterruptCheck(ctx, seams); err != nil {
			return orchestrateCommitOutcome{}, err
		}
		// CRW-1113: the reviewer sign-offs the observer kept (it met a busy lock or a failed write) are applied here, inside the lock
		// this publication holds (the session lock is held by the caller), so the binding below is judged on the plan as the drain
		// leaves it: a kept FAIL refuses, a kept PASS counts, and none is left to be deleted after the session has moved to B. The
		// drain is the first durable effect of an A>B edge: it checks the invocation's context itself before its first change (a
		// cancelled invocation writes nothing and answers Interrupted), and once it has written, the edge finishes as any command
		// whose first write has started does (CRW-871), so the second check below is not taken after it.
		var drained hook.ReviewObserverDrain
		if cur.Phase == state.PhaseA && to == state.PhaseB {
			var err error
			if drained, err = hook.DrainReviewObserverInboxInLock(cwd, sessionID, plan, ctx.Err); err != nil {
				return orchestrateCommitOutcome{}, err
			}
			plan = drained.Plan
			if drained.Kept > 0 {
				return orchestrateCommitOutcome{refusal: &CliResult{Code: 1, Output: "orchestrate " + VerbText(a.Verb) + ": " + RenderPhaseContext(cur, sessionID) +
					"; a reviewer sign-off was kept but could not be applied (its verdict could not be written to the plan, or the review inbox could not be read), so the review binding cannot be judged. Retry the transition. The session did not move."}}, nil
			}
		}
		if bindCheck := attest.ValidateWorkPhaseBinding(a.Attest, goalplan.EffectiveActiveWorkPhaseID(plan)); !bindCheck.OK {
			return orchestrateCommitOutcome{refusal: &CliResult{Code: 1, Output: "orchestrate " + VerbText(a.Verb) + ": " + RenderPhaseContext(cur, sessionID) + "; " + bindCheck.Reason}}, nil
		}
		// CRW-975: the A>B review binding is judged again on the plan this lock read, so a reviewer verdict
		// recorded between the first check (before the lock) and this publication is not ignored. The
		// oracle runs validateReviewBinding and the write without the lock and has the same race.
		if cur.Phase == state.PhaseA && to == state.PhaseB {
			if refusal := orchestrateReviewBindingCheckPlan(plan, cur, a, sessionID); refusal != nil {
				return orchestrateCommitOutcome{refusal: refusal}, nil
			}
		}
		// CRW-975: the review binding re-check above re-reads and re-hashes the plan files, which is I/O the
		// first check did not cover, so the invocation's context is read once more immediately before the
		// first durable effect below (CRW-871). A drain that wrote above was that first effect: the edge finishes.
		if !drained.Wrote {
			if err := orchestrateInterruptCheck(ctx, seams); err != nil {
				return orchestrateCommitOutcome{}, err
			}
		}
		// 032: a fresh epoch orphans every open plan_audit round this session owns under another epoch. The
		// oracle closes the first stranded epoch's rounds only, before the state is published, and drops a
		// cleanup failure (:1085-1119). CRW-1100: every such round is listed here, under this lock and after
		// the binding is revalidated, and the list rides the transition's outbox event with the new epoch, so
		// the binding and the cleanup are one recorded piece of work. The state is published first; the
		// rounds are closed after it, in this same lock, and a cleanup that fails stays pending with the
		// event for the next writer of the session, which replays the same epoch and the same list. A
		// refusal above, or a write that fails before the rename, closes no round.
		var cleanup *hook.PlanAuditCleanup
		if binding != nil && ev != nil {
			if rounds := review.ObsoleteRounds(plan, goalplan.PurposePlanAudit, sessionID, binding.epoch); len(rounds) > 0 {
				c := hook.NewPlanAuditCleanup(cur.Slug, binding.epoch, rounds)
				cleanup = &c
				payload, err := json.Marshal(cleanup)
				if err != nil {
					return orchestrateCommitOutcome{}, err
				}
				ev.Followup = payload
			}
		}
		outcome := orchestrateCommitWrite(seams, cwd, next, ev)
		if outcome.err != nil {
			return orchestrateCommitOutcome{}, outcome.err
		}
		if cleanup != nil {
			outcome.cleanupErr = orchestrateCommitCleanup(seams, cwd, sessionID, *cleanup, plan)
			outcome.cleanupDone = outcome.cleanupErr == nil
		}
		return outcome, nil
	})
	if err != nil {
		return orchestrateCommitOutcome{err: err}
	}
	switch locked.Kind {
	case "ok":
		if locked.Value == nil {
			return orchestrateCommitOutcome{err: errors.New("goalplan write lock reported ok without a value")}
		}
		return *locked.Value
	case "locked":
		// Nothing was published, so a context that ended while the lock was held answers Interrupted
		// rather than the busy refusal (CRW-871).
		if err := ctx.Err(); err != nil {
			return orchestrateCommitOutcome{err: err}
		}
		return orchestrateCommitOutcome{refusal: &CliResult{Code: 1, Output: "orchestrate " + VerbText(a.Verb) + ": " + RenderPhaseContext(cur, sessionID) + "; " + locked.Reason}}
	}
	// A plan the lock read and withholds from writers (bytes that are not UTF-8, a repeated key, stored data
	// a write would lose) is a refusal with the lock's reason: nothing was judged under the lock for it, so
	// nothing is published (CRW-975).
	if locked.Refused {
		if err := ctx.Err(); err != nil {
			return orchestrateCommitOutcome{err: err}
		}
		return orchestrateCommitOutcome{refusal: &CliResult{Code: 1, Output: "orchestrate " + VerbText(a.Verb) + ": " + RenderPhaseContext(cur, sessionID) + "; " + locked.Reason + ". Nothing was written."}}
	}
	// unreadable: an absent or unreachable plan is fail-open, as the unlocked read did and as the oracle
	// does when it cannot open the plan (orchestrate-cli.ts 606-622). The publication runs outside the lock.
	if err := ctx.Err(); err != nil {
		return orchestrateCommitOutcome{err: err}
	}
	return orchestrateCommitWrite(seams, cwd, next, ev)
}

// orchestrateCommitWarn is the two post-publication warnings of one write path, in the order its steps ran:
// the directory sync first, then the ledger row. Each is empty when its step succeeded. This command has no
// earlier warning place, so the answer carries them as its last lines.
func orchestrateCommitWarn(label string, out orchestrateCommitOutcome, rowErr error, from, to state.Phase) []string {
	warnings := []string{}
	if out.warning != nil {
		warnings = append(warnings, label+": warning: session state was published but its directory could not be synced: "+out.warning.Error())
	}
	if rowErr != nil {
		warnings = append(warnings, label+": warning: ledger row for "+string(from)+" -> "+string(to)+" could not be written: "+rowErr.Error()+
			"; the row is kept pending and the next orchestrate call or hook of this session records it")
	}
	return warnings
}

// orchestrateCommitAnswer appends warnings to a success answer, one per line.
func orchestrateCommitAnswer(output string, warnings ...string) string {
	for _, warning := range warnings {
		if warning != "" {
			output += "\n" + warning
		}
	}
	return output
}

// RunOrchestrateTransition ports the transition half of orchestrate-cli.ts (:549-1131) on the mutation
// RunOrchestrateRead delegated. It does its own state, goalplan and ledger IO and returns the oracle's
// CliResult; an error is a Go-level IO failure the oracle would have thrown, which no refusal path uses.
//
// The whole read, gate and write runs under the session's exclusive lock. The oracle takes no lock, so a hook
// that writes the same session meanwhile is overwritten by this command's earlier copy and its update is lost;
// the port fixes that (the data-loss class the parity rule revision of 2026-10-03 fixes during the port). A
// lock that cannot be taken is an error, never a silent success.
func RunOrchestrateTransition(a OrchestrateCliArgs, sessionID string) (CliResult, error) {
	return RunOrchestrateTransitionContext(context.Background(), a, sessionID)
}

// RunOrchestrateTransitionContext is RunOrchestrateTransition for a caller that can be interrupted: the
// orchestrate row of cmd/crw serve passes the invocation context the first SIGINT cancels (CRW-871). A
// context cancelled before or during the lock wait writes nothing and returns its own error; one cancelled
// after the lock and before the command's first write writes nothing either. Once the first write has
// started the command finishes and answers as it always did.
func RunOrchestrateTransitionContext(ctx context.Context, a OrchestrateCliArgs, sessionID string) (CliResult, error) {
	return orchestrateCommitRunContext(ctx, a, sessionID, nil)
}

// orchestrateCommitRun is the context-free form of orchestrateCommitRunContext, which the exported entry
// point and the commit-order tests call.
func orchestrateCommitRun(a OrchestrateCliArgs, sessionID string, seams *orchestrateCommitSeams) (CliResult, error) {
	return orchestrateCommitRunContext(context.Background(), a, sessionID, seams)
}

// orchestrateCommitRunContext is RunOrchestrateTransitionContext with the commit order's two seams, which
// only a test supplies. It makes the canonical-id judgement itself, before it takes the lock, so a direct
// caller of RunOrchestrateTransition is covered as well as the terminal row: a non-canonical id would
// otherwise resolve to a DIFFERENT session's file, which this command would then read and rewrite.
func orchestrateCommitRunContext(ctx context.Context, a OrchestrateCliArgs, sessionID string, seams *orchestrateCommitSeams) (CliResult, error) {
	if !state.IsCanonicalSessionID(sessionID) {
		return sessionAliasRefuse(a.Verb), nil
	}
	var out CliResult
	err := state.WithSessionLockContext(ctx, a.Cwd, sessionID, func() error {
		// CRW-1097: a ledger row an earlier writer of this session left pending is recorded first, so this
		// command's own row follows it. A drain that cannot finish leaves the rows pending; it never refuses
		// the command.
		hook.DrainSessionLedger(a.Cwd, sessionID)
		var inner error
		out, inner = orchestrateTransitionApply(ctx, a, sessionID, seams)
		return inner
	})
	if err != nil {
		return CliResult{}, err
	}
	return out, nil
}

// orchestrateTransitionGoalStatus is the host goal status of the session's thread, read from the goals database
// the hooks read (CODEX_SQLITE_HOME, else CODEX_HOME, else the account home). A path that cannot be resolved is
// unreadable, which suppresses the Interview as it does for the hooks. Unlike the hooks it also reads a goals path
// that cannot be inspected (not merely absent) as unreadable, because a write is gated on the answer.
func orchestrateTransitionGoalStatus(sessionID string) host.GoalStatus {
	path, err := host.GoalsDBPath(os.LookupEnv)
	if err != nil {
		return host.GoalUnreadable
	}
	return host.GoalActiveStatusFailClosed(sessionID, path)
}

// orchestrateTransitionGoalRefusal is the refusal of an entry to I while goal mode owns the thread: it names the
// goal-mode next command (P to start a cycle from rest, the current phase otherwise). It leaves the phase and the
// state file as they were; recovery of an earlier writer's pending ledger row (CRW-1097) has already run.
func orchestrateTransitionGoalRefusal(verb fsm.OrchestrateVerb, cur state.State, sessionID string, status host.GoalStatus) CliResult {
	why := "an active host goal owns this session"
	if status == host.GoalUnreadable {
		why = "the host goals database cannot be read, so an active host goal is assumed"
	}
	next := "Continue the " + string(cur.Phase) + " phase of the current cycle."
	if cur.Phase == state.PhaseIdle {
		next = "Goal mode is PABCD-only: start the cycle with `crw pabcd orchestrate P --session " + sessionID + "`."
	}
	return CliResult{Code: 1, Output: "orchestrate " + VerbText(verb) + ": " + RenderPhaseContext(cur, sessionID) +
		"; the Interview (I) is HITL-only and never runs while a host goal is active: " + why + ". " + next + " The session state was not changed."}
}

// orchestrateTransitionApply is the ported body, run while the caller holds the session lock.
func orchestrateTransitionApply(ctx context.Context, a OrchestrateCliArgs, sessionID string, seams *orchestrateCommitSeams) (CliResult, error) {
	cwd, verb := a.Cwd, a.Verb
	cur := state.ReadState(cwd, sessionID)
	// 050 wp5 §5: the fixed close target, and whether this D request is finishing a close that already started.
	// A matching marker means the first attempt spent the binding, transition and receipt gates already.
	closePhaseID := ""
	if verb == fsm.VerbD && a.Attest != nil {
		closePhaseID = text.Trim(a.Attest.WorkPhaseID)
	}
	recoveringDclose := verb == fsm.VerbD && state.MatchesDcloseRecovery(cur, closePhaseID)

	// reset: a control override, the same cleared-IDLE write as the human path.
	if verb == fsm.VerbReset {
		res := fsm.ApplyHumanTransition(cur, fsm.VerbReset, a.Attest)
		if res.Noop {
			return CliResult{Code: 0, Output: "orchestrate reset: " + RenderPhaseContext(cur, sessionID) + "; already IDLE"}, nil
		}
		if res.State != nil {
			if reason, ok := orchestrateTransitionStateWritable(cwd, sessionID, cur); !ok {
				return orchestrateTransitionStateRefusal(verb, reason), nil
			}
			next := *res.State
			next.OrchestrationActive, next.LastInjectedPhase = false, nil
			next.StopBlockPhase, next.StopBlockCount = nil, 0
			// CRW-871: the pre-write cancellation check. Cancelled here, the reset writes nothing and the
			// row answers Interrupted (130).
			if err := orchestrateInterruptCheck(ctx, seams); err != nil {
				return CliResult{}, err
			}
			// Publish the state first, then the row: a failure before the publication writes no row, so no
			// row describes a reset that did not happen, and a row that cannot be written warns on a success
			// (CRW-811, the CRW-744/793 rule).
			ev, err := orchestrateCommitEvent(cwd, cur, next, res.Ledger)
			if err != nil {
				return CliResult{}, err
			}
			published := orchestrateCommitWrite(seams, cwd, next, ev)
			if !published.published {
				return CliResult{}, published.err
			}
			rowErr := orchestrateCommitRecord(cwd, sessionID, ev)
			answer := "orchestrate reset: current=" + string(cur.Phase) + " -> IDLE (session " + sessionID + ")"
			return CliResult{Code: 0, Output: orchestrateCommitAnswer(answer,
				orchestrateCommitWarn("orchestrate reset", published, rowErr, cur.Phase, state.PhaseIdle)...)}, nil
		}
		return CliResult{Code: 0, Output: "orchestrate reset: current=" + string(cur.Phase) + " -> IDLE (session " + sessionID + ")"}, nil
	}

	// A phase verb: agent-gated through the un-weakened transition(), validated before any write.
	to := state.Phase(verb)
	// CRW-1179: an active host goal owns the thread, and the Interview never fires under it (crw-pabcd, crw-loop). The
	// UserPromptSubmit hook already withholds the I directive and the chat command; this is the same firewall on the
	// CLI entry, so an agent cannot enter I by running the command and then climb out by override. An unreadable goals
	// database fails closed, as every other goal-mode reader does. Only entering I is judged: leaving it is never refused.
	if to == state.PhaseI && cur.Phase != state.PhaseI {
		if status := orchestrateTransitionGoalStatus(sessionID); host.SuppressesInterview(status) {
			return orchestrateTransitionGoalRefusal(verb, cur, sessionID, status), nil
		}
	}
	if _, err := session.Resolve(cwd, sessionID); err != nil {
		return CliResult{Code: 1, Output: "orchestrate " + VerbText(verb) + ": SOURCE-ROOT: " + err.Error()}, nil
	}
	// #133: entry refusal for a goalplan-bound cycle with no resolvable source identity, so a bound non-git
	// session cannot enter P and strand at C, whose close needs a receipt that cannot be bound. Guarded on
	// state.slug, so unbound HITL cycles are untouched, and on the ENTRY edges only: A>P is a re-plan.
	if to == state.PhaseP && (cur.Phase == state.PhaseIdle || cur.Phase == state.PhaseI) && cur.Slug != "" {
		if gate := session.CheckBound(cwd, sessionID); !gate.OK {
			return CliResult{Code: 1, Output: "orchestrate " + VerbText(verb) + ": " + gate.Reason + "\nNothing was written."}, nil
		}
	}
	// P>A plan-artifact gate (DIFFLEVEL-ROADMAP-01): the plan must exist as numbered on-disk docs before Audit.
	// It runs even with no attestation, so the first error names planUnit. Fail-closed on this edge only.
	var binding *orchestrateTransitionPlanBinding
	if cur.Phase == state.PhaseP && to == state.PhaseA {
		planCheck := attest.ValidatePlanArtifacts(a.Attest, cwd)
		if !planCheck.OK {
			return CliResult{Code: 1, Output: "orchestrate " + VerbText(verb) + ": " + RenderPhaseContext(cur, sessionID) + "; " + planCheck.Reason}, nil
		}
		epoch, err := orchestrateTransitionMintEpoch("e")
		if err != nil {
			return CliResult{}, err
		}
		binding = &orchestrateTransitionPlanBinding{unit: planCheck.Unit, epoch: epoch}
	}
	// Work-phase binding gate (LOOP-UNIT-CHAIN-01): on a gated edge of a bound session the attestation must
	// name the one effective active work-phase. Fail-open when no goalplan resolves. This first read keeps the
	// oracle's refusal precedence (it runs before the review binding and the source gate) and the C>D arm the
	// unported D close owns; the binding that the state write actually lands under is revalidated at the
	// publication, inside the goalplan write lock, by orchestrateCommitPublish (CRW-811).
	if attest.IsGated(cur.Phase, to) && cur.Slug != "" && !recoveringDclose {
		var effective *string
		if plan := goalplan.ReadGoalplan(cwd, cur.Slug); plan != nil {
			effective = goalplan.EffectiveActiveWorkPhaseID(plan)
		}
		if bindCheck := attest.ValidateWorkPhaseBinding(a.Attest, effective); !bindCheck.OK {
			return CliResult{Code: 1, Output: "orchestrate " + VerbText(verb) + ": " + RenderPhaseContext(cur, sessionID) + "; " + bindCheck.Reason}, nil
		}
	}
	// I>P: the interview soft gate. transition() has no override support, so this adds the equivalent of
	// applyHumanTransition's override for I>P only.
	if cur.Phase == state.PhaseI && to == state.PhaseP {
		gate := interview.EvaluateInterviewGate(cur.Interview, &interview.GateEvidence{
			BackedDimensions: ledger.DimensionsBackedByAnswers(cwd, sessionID),
		})
		switch {
		case gate.Ready:
			// Ready: the normal transition() path handles it (it derives flags.interview from the tracker).
		case a.Attest != nil && a.Attest.Override:
			if a.Attest.Did == "" || orchestrateTransitionPlaceholderDid(a.Attest.Did) {
				return CliResult{Code: 1, Output: "orchestrate " + VerbText(verb) + ": " + RenderPhaseContext(cur, sessionID) +
					"; I\u2192P override requires a specific \"did\" narrative explaining why the interview is complete (not empty or placeholder)."}, nil
			}
			if a.Attest.From != state.PhaseI || a.Attest.To != state.PhaseP {
				return CliResult{Code: 1, Output: "orchestrate " + VerbText(verb) + ": " + RenderPhaseContext(cur, sessionID) +
					"; I\u2192P override attest from/to must be I/P, got " + string(a.Attest.From) + "/" + string(a.Attest.To) + "."}, nil
			}
			next := cur
			next.Flags.Interview = true
			if ok, reason := fsm.CanEnter(to, next); !ok {
				return CliResult{Code: 1, Output: "orchestrate " + VerbText(verb) + ": " + RenderPhaseContext(cur, sessionID) + "; " + reason}, nil
			}
			// I>P is never B or A, so the snapshot and both bindings are cleared explicitly: this writer
			// bypasses transition() and would otherwise spread stale values.
			next.Phase, next.OrchestrationActive, next.LastInjectedPhase = to, true, &to
			next.StopBlockPhase, next.StopBlockCount = nil, 0
			next.PhaseEntrySource, next.PlanUnit, next.PlanEpoch, next.CheckEpoch = nil, nil, nil, nil
			if reason, ok := orchestrateTransitionStateWritable(cwd, sessionID, cur); !ok {
				return orchestrateTransitionStateRefusal(verb, reason), nil
			}
			// CRW-871: the same pre-write check as the reset branch, before the override's state write.
			if err := orchestrateInterruptCheck(ctx, seams); err != nil {
				return CliResult{}, err
			}
			// The override publishes first and appends its row second, as the reset above does.
			scan := state.ScanEvidence{HighContradictionCount: float64(gate.HighContradictionCount)}
			if cur.Interview != nil {
				scan.ScanRounds = float64(cur.Interview.ScanRounds)
			}
			yes := true
			did := a.Attest.Did
			from := cur.Phase
			ev, err := orchestrateCommitEvent(cwd, cur, next, &state.LedgerEntry{
				TS: orchestrateTransitionTimestamp(), SessionID: cur.SessionID, From: &from, To: to, Reason: "cli",
				Actor: "agent", Override: &yes, ScanEvidence: &scan, Evidence: &did,
			})
			if err != nil {
				return CliResult{}, err
			}
			published := orchestrateCommitWrite(seams, cwd, next, ev)
			if !published.published {
				return CliResult{}, published.err
			}
			hook.ResetRenderLedger(cwd)
			rowErr := orchestrateCommitRecord(cwd, sessionID, ev)
			answer := orchestrateTransitionWithArchitectHint(state.PhaseP,
				"orchestrate P: I \u2192 P (agent override, session "+sessionID+")")
			return CliResult{Code: 0, Output: orchestrateCommitAnswer(answer,
				orchestrateCommitWarn("orchestrate P", published, rowErr, from, to)...)}, nil
		default:
			return CliResult{Code: 1, Output: "orchestrate " + VerbText(verb) + ": " + RenderPhaseContext(cur, sessionID) +
				"; interview soft-gate: " + strings.Join(gate.Warnings, "; ") + ". Pass override:true in --attest to proceed."}, nil
		}
	}
	// 050 wp5 §5: a marker-matched D retry resumes a transition the first attempt already made legally;
	// state.phase is IDLE by then, so transition() would refuse C>D on a session whose cycle is mid-close.
	var result fsm.TransitionResult
	if recoveringDclose {
		next := cur
		next.Phase = state.PhaseD
		result = fsm.TransitionResult{OK: true, State: &next}
	} else {
		result = fsm.Transition(cur, to, a.Attest)
	}
	if !result.OK || result.State == nil {
		reason := result.Reason
		if reason == "" {
			reason = "transition refused"
		}
		return CliResult{Code: 1, Output: "orchestrate " + VerbText(verb) + ": " + RenderPhaseContext(cur, sessionID) + "; " + reason}, nil
	}
	// LEAN-REVIEW-01: an open round is honoured, never required; the B>C source gates follow, then the D close.
	if cur.Phase == state.PhaseA && to == state.PhaseB && cur.Slug != "" {
		// CRW-1113: the sign-offs the observer kept are applied inside the goalplan lock of the publication (orchestrateCommitPublish),
		// where the binding is judged again on the drained plan; this first check reads the plan as it stands.
		if refusal := orchestrateReviewBindingCheck(cur, a, sessionID); refusal != nil {
			return *refusal, nil
		}
	}
	if cur.Phase == state.PhaseB && to == state.PhaseC {
		refusal, err := orchestrateReviewBindingSourceGate(cwd, sessionID, cur)
		if err != nil {
			return CliResult{}, err
		}
		if refusal != nil {
			return *refusal, nil
		}
	}
	if to == state.PhaseD {
		// CRW-871: the D close's first write is its recovery marker or its state publication, both inside
		// the close. Cancelled before entering it, the close writes nothing and the row answers Interrupted.
		return orchestrateTransitionDClose(ctx, seams, cwd, sessionID, closePhaseID, cur, a.Attest, recoveringDclose)
	}
	// The data-loss refusal, before any of this edge's writes, the goalplan housekeeping included.
	if reason, ok := orchestrateTransitionStateWritable(cwd, sessionID, cur); !ok {
		return orchestrateTransitionStateRefusal(verb, reason), nil
	}

	// L6: a real transition is progress, so the Stop stagnation guard resets. SOURCE-DELTA-01: snapshot the
	// source on entry to B and clear it on every other edge, so no stale snapshot outlives its phase.
	var entrySource *state.SourceIdentity
	if result.State.Phase == state.PhaseB {
		exclude := true
		id, err := session.Capture(cwd, sessionID, session.CaptureOptions{ExcludeStateArtifacts: &exclude})
		if err != nil {
			return CliResult{}, err
		}
		stored := orchestrateTransitionStoredIdentity(id)
		entrySource = &stored
	}
	unit, epoch := orchestrateTransitionPlanBindingOf(binding, cur, result.State.Phase)
	checkEpoch, err := orchestrateTransitionCheckEpoch(cur.Phase, result.State.Phase, cur.CheckEpoch)
	if err != nil {
		return CliResult{}, err
	}
	next := *result.State
	next.OrchestrationActive, next.LastInjectedPhase = result.State.Phase != state.PhaseIdle, &result.State.Phase
	next.StopBlockPhase, next.StopBlockCount = nil, 0
	next.PhaseEntrySource = entrySource
	if entrySource != nil && entrySource.SourceRoot != nil && *entrySource.SourceRoot != "" {
		next.BoundSourceRoot = entrySource.SourceRoot
	}
	next.PlanUnit, next.PlanEpoch, next.CheckEpoch = unit, epoch, checkEpoch
	from := cur.Phase
	row := state.LedgerEntry{TS: orchestrateTransitionTimestamp(), SessionID: cur.SessionID, From: &from, To: result.State.Phase, Reason: "cli"}
	if a.Attest != nil && a.Attest.Did != "" {
		did := a.Attest.Did
		row.Evidence = &did
	}
	// The state is published first and the row appended second, the oracle's own order: a failure before the
	// publication writes no row, so no row describes a transition that did not happen and the caller can
	// retry this verb without a duplicate; once published the transition counts as done and a row that
	// cannot be written warns on a success answer (CRW-811, the CRW-744/793 rule). On a gated edge of a
	// bound session the publication runs inside the goalplan write lock, where the binding is revalidated.
	// The 032 stale-round housekeeping runs inside this publication's goalplan lock, so a refused edge closes
	// no round.
	// CRW-871: the pre-write cancellation check of the ordinary transition, immediately before its first
	// durable effect (the state publication, and the goalplan housekeeping that runs inside it).
	if err := orchestrateInterruptCheck(ctx, seams); err != nil {
		return CliResult{}, err
	}
	ev, err := orchestrateCommitEvent(cwd, cur, next, &row)
	if err != nil {
		return CliResult{}, err
	}
	published := orchestrateCommitPublish(ctx, seams, a, cwd, sessionID, cur, next, result.State.Phase, recoveringDclose, binding, ev)
	if published.refusal != nil {
		return *published.refusal, nil
	}
	if !published.published {
		return CliResult{}, published.err
	}
	// C-RENDER-GROUNDING-01: a new cycle starts at P, so the render ledger is cleared (stale rows misfire).
	if result.State.Phase == state.PhaseP {
		hook.ResetRenderLedger(cwd)
	}
	rowErr, cleanupErr := orchestrateCommitRecordAll(cwd, sessionID, ev, published.cleanupDone, published.cleanupErr)
	arrow := string(cur.Phase) + " \u2192 " + string(result.State.Phase)
	answer := orchestrateTransitionWithArchitectHint(result.State.Phase,
		"orchestrate "+VerbText(verb)+": current="+string(cur.Phase)+" -> "+string(result.State.Phase)+" ("+arrow+", session "+sessionID+")")
	warnings := orchestrateCommitWarn("orchestrate "+VerbText(verb), published, rowErr, cur.Phase, result.State.Phase)
	warnings = append(warnings, orchestrateCommitCleanupWarn("orchestrate "+VerbText(verb), cleanupErr)...)
	return CliResult{Code: 0, Output: orchestrateCommitAnswer(answer, warnings...)}, nil
}
