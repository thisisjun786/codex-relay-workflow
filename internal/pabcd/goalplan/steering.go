package goalplan

// CXC v0.2.40 pabcd-state/src/steering.ts:155-163,241-333 (commit 3c1459ac). The transaction
// around B15a's op grammar: the idempotency key that makes a batch run once, the shared goalplan
// write lock that spans the whole read-modify-write, the ledger rows, and the four answers
// applied, duplicate, locked and rejected. goalplan.json is the commit point, because it is what
// idempotency reads; a ledger append that fails after it answers applied with a warning instead
// of pretending nothing happened.
//
// The op grammar, its validation and the pure fold are in steering_ops.go (B15a / CRW-376) and
// are called, never copied. The oracle's own comment there says this half is B15b.
import (
	"strconv"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/crwdir"
)

// SteeringBatchOptions is ApplyOptions (:155-163): the clock the entry stamps, and the shared
// goalplan write lock's own seams. Both are injected so the contention cases drive the retry
// timing and the clock without sleeping or reading the wall clock.
type SteeringBatchOptions struct {
	Now  func() string
	Lock *GoalplanWriteLockOptions
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
	if o != nil {
		if o.Now != nil {
			now = o.Now
		}
		lockOptions = o.Lock
	}
	locked, err := WithGoalplanWriteLock(cwd, slug, func(plan *Goalplan) (SteerResult, error) {
		return steeringApplyLocked(cwd, slug, plan, batch, now)
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
func steeringApplyLocked(cwd, slug string, plan *Goalplan, batch SteerBatch, now func() string) (SteerResult, error) {
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
	if err := WriteGoalplan(cwd, &next); err != nil {
		return SteerResult{}, err
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
	return SteerResult{Kind: SteerResultApplied, Plan: &next, Entry: &entry}, nil
}
