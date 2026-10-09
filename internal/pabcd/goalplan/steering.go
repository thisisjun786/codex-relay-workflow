package goalplan

// CXC v0.2.40 pabcd-state/src/steering.ts:155-163,241-333 (commit 3c1459ac). The transaction
// around B15a's op grammar: the idempotency key that makes a batch run once, the shared goalplan
// write lock that spans the whole read-modify-write, the ledger rows, and the four answers
// applied, duplicate, locked and rejected. goalplan.json is the commit point, because it is what
// idempotency reads; a ledger append that fails after it answers applied with a warning instead
// of pretending nothing happened.
// A plan write that published and then failed to sync the plan's directory is the same shape one
// level down (CRW-793): the key is recorded, so the batch is answered applied with a durability
// warning and its ledger rows are written rather than lost.
//
// The op grammar, its validation and the pure fold are in steering_ops.go (B15a / CRW-376) and
// are called, never copied. The oracle's own comment there says this half is B15b.
import (
	"context"
	"strconv"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/crwdir"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
)

// SteeringBatchOptions is ApplyOptions (:155-163): the clock the entry stamps, and the shared
// goalplan write lock's own seams. Both are injected so the contention cases drive the retry
// timing and the clock without sleeping or reading the wall clock.
type SteeringBatchOptions struct {
	Now  func() string
	Lock *GoalplanWriteLockOptions

	// publish is the CRW-793 durability seam of the plan write this batch performs. It is an
	// argument, never package state, so a test can drive the published-but-unsynced path without
	// changing what any other caller does.
	publish *goalplanPublishedOptions
}

// steeringBatchSummary is the oracle's entry summary (:275): how many ops the batch carried and
// the kinds in the order the batch declared them.
func steeringBatchSummary(ops []SteerOp) string {
	kinds := make([]string, 0, len(ops))
	for _, op := range ops {
		kinds = append(kinds, string(op.Kind))
	}
	return strconv.Itoa(len(ops)) + " op(s): " + strings.Join(kinds, ", ")
}

// steeringLedgerPathText is the relative display path the failed-append warning names (:314): the
// oracle's string with the state directory substituted. It is built with a slash rather than
// filepath.Join because it is display text, not a path this code opens.
func steeringLedgerPathText(slug string) string {
	return crwdir.DirName + "/" + GoalplansSubdir + "/" + slug + "/" + GoalplanLedgerFile
}

// steeringLedgerWarning is the oracle's warning (:312-316): the batch stands and its audit row is
// missing, so re-running is safe. err.Error() is Go's wording for the oracle's err.message.
func steeringLedgerWarning(slug string, err error) string {
	return "the batch was applied but its ledger entry could not be written to " +
		steeringLedgerPathText(slug) +
		" (" + err.Error() + "). " +
		"Re-running is a no-op because the key is recorded."
}

// goalplanPublishedWarning is CRW-793's durability warning: the plan at the final path is the new one,
// so the batch stands and its key is recorded, but the directory that holds it could not be synced and
// the publication may not survive a crash. The oracle never syncs that directory, so it has no
// counterpart; the wording is the issue's own.
func goalplanPublishedWarning(slug string, err error) string {
	return "goalplan '" + slug + "' was published but its directory could not be synced: " + err.Error()
}

// ApplySteeringBatch ports applySteeringBatch (:253-333): one application per idempotency key,
// under the shared goalplan write lock, with the oracle's four answers and its ledger rows.
//
// The batch is validated before the lock is taken (:259-260 precede :264), so a batch the
// validator refuses performs no filesystem work at all. The returned error is the oracle's throw
// path: an invalid slug, a lock error that is not contention, or a failed plan write.
func ApplySteeringBatch(cwd, slug string, rawBatch any, o *SteeringBatchOptions) (SteerResult, error) {
	batch, reason := steeringOpsValidateBatch(rawBatch)
	if reason != "" {
		return SteerResult{Kind: SteerResultRejected, Reason: reason}, nil
	}
	now := writeTimestamp
	var lockOptions *GoalplanWriteLockOptions
	var publish *goalplanPublishedOptions
	var ctx context.Context
	if o != nil {
		if o.Now != nil {
			now = o.Now
		}
		lockOptions = o.Lock
		publish = o.publish
	}
	if lockOptions != nil {
		ctx = lockOptions.Context
	}
	locked, err := WithGoalplanWriteLock(cwd, slug, func(plan *Goalplan) (SteerResult, error) {
		return steeringApplyLocked(ctx, cwd, slug, plan, batch, now, publish)
	}, lockOptions)
	if err != nil {
		return SteerResult{}, err
	}
	switch locked.Kind {
	case "locked":
		return SteerResult{Kind: SteerResultLocked, Reason: locked.Reason}, nil
	case "unreadable":
		// The absent-plan refusal predates the shared lock and is asserted by name. The lock
		// reports absence as one unreadable reason among several, so it is mapped back rather
		// than folded into the unusable wording (:324-331). The compare is against the reason
		// WithGoalplanWriteLock builds for an absent plan, exactly as the oracle compares its
		// own (:327).
		if locked.Reason == "goalplan '"+slug+"' does not exist" {
			return SteerResult{Kind: SteerResultRejected, Reason: "no goalplan found at slug '" + slug + "'"}, nil
		}
		return SteerResult{Kind: SteerResultRejected, Reason: "goalplan at slug '" + slug + "' is unusable - " + locked.Reason}, nil
	}
	if locked.Value == nil {
		return SteerResult{}, nil
	}
	return *locked.Value, nil
}

// steeringApplyLocked is the oracle's callback (:264-320): the whole read-modify-write, run while
// the shared goalplan write lock is held. The duplicate scan comes first, so an injected clock is
// not consulted for a batch that will not be recorded.
//
// ctx (CRW-1074, nil for a caller the first SIGINT cannot end) is read once more after the batch is prepared
// (duplicate scan, clock, ops applied) and immediately before the plan write, the transaction's first write, so
// a first SIGINT that lands while the change is being prepared publishes nothing. After that write has begun
// it is not read again: the transaction finishes and answers as before.
func steeringApplyLocked(ctx context.Context, cwd, slug string, plan *Goalplan, batch SteerBatch, now func() string, publish *goalplanPublishedOptions) (SteerResult, error) {
	for i := range plan.SteeringLog {
		if plan.SteeringLog[i].IdempotencyKey == batch.IdempotencyKey {
			existing := plan.SteeringLog[i]
			return SteerResult{Kind: SteerResultDuplicate, Entry: &existing}, nil
		}
	}
	entry := SteeringEntry{
		IdempotencyKey: batch.IdempotencyKey,
		Rationale:      batch.Rationale,
		Evidence:       batch.Evidence,
		AppliedAt:      now(),
		Summary:        steeringBatchSummary(batch.Ops),
	}
	applied, reason := steeringOpsApplyOps(plan, batch.Ops)
	if reason != "" {
		return SteerResult{Kind: SteerResultRejected, Reason: reason}, nil
	}
	// The commit point: a fresh slice, so the plan the lock read is never mutated in place.
	next := *applied
	next.SteeringLog = append(append([]SteeringEntry{}, plan.SteeringLog...), entry)
	// A write that published and then failed to sync the plan's directory is a written plan: the key is
	// already visible to idempotency, so a retry would answer duplicate and this batch's rows could never
	// be written at all. The durability failure is carried as a warning and the ledger work below runs as
	// on a clean write. A failure before the rename published nothing and stays an error.
	warning := ""
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return SteerResult{}, err
		}
	}
	if err := goalplanPublishedWriteGoalplan(cwd, &next, publish); err != nil {
		if !state.Published(err) {
			return SteerResult{}, err
		}
		warning = goalplanPublishedWarning(slug, err)
	}
	if err := AppendGoalplanLedger(cwd, slug, GoalplanLedgerEntry{
		Ts: entry.AppliedAt, Slug: slug, Event: EventSteered,
		Detail: entry.IdempotencyKey + ": " + entry.Summary + " — " + entry.Rationale,
	}); err != nil {
		return SteerResult{Kind: SteerResultApplied, Plan: &next, Entry: &entry, Warning: steeringLedgerWarning(slug, err)}, nil
	}
	// One row per phase that actually declared prerequisites, emitted after the steered row so
	// the batch that carried the edge is the row above it (:292-306).
	for _, op := range batch.Ops {
		if op.Kind != SteerOpAddWorkPhase || len(op.DependsOn) == 0 {
			continue
		}
		if err := AppendGoalplanLedger(cwd, slug, GoalplanLedgerEntry{
			Ts: entry.AppliedAt, Slug: slug, Event: EventDependencyRegistered,
			Detail: op.ID + " dependsOn=" + strings.Join(op.DependsOn, ","),
		}); err != nil {
			return SteerResult{Kind: SteerResultApplied, Plan: &next, Entry: &entry, Warning: steeringLedgerWarning(slug, err)}, nil
		}
	}
	return SteerResult{Kind: SteerResultApplied, Plan: &next, Entry: &entry, Warning: warning}, nil
}
