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
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"reflect"
	"slices"
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
	// BeforeWrite (CRW-1074), when non-nil, runs once, immediately before the plan write, the transaction's
	// first write, after the last context check. A caller the first SIGINT can end uses it to tell a result
	// reached before any write began (an ended context outranks it) from one of a transaction that has begun
	// to write (it answers as before). It does not run for a batch that writes nothing.
	BeforeWrite func()

	// publish is the CRW-793 durability seam of the plan write this batch performs. It is an
	// argument, never package state, so a test can drive the published-but-unsynced path without
	// changing what any other caller does.
	publish *goalplanPublishedOptions

	// appendLedger replaces the ledger append of the entry's rows, so a test can fail one row of a
	// batch (CRW-1111). nil is AppendGoalplanLedger.
	appendLedger func(cwd, slug string, entry GoalplanLedgerEntry) error
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
// missing. err.Error() is Go's wording for the oracle's err.message. The oracle's last sentence says
// re-running is a no-op; since CRW-1111 re-running the same batch with the same key records the rows
// still missing, so the sentence says that instead.
func steeringLedgerWarning(slug string, err error) string {
	return "the batch was applied but its ledger entry could not be written to " +
		steeringLedgerPathText(slug) +
		" (" + err.Error() + "). " +
		"Re-running the same batch with the same key records the missing rows without applying it again."
}

// steeringKeyReusedReason is the refusal for a key whose recorded batch differs from the one now sent
// (CRW-1111): the key says the batch was applied, so a different batch under it would be answered as
// done without being applied.
func steeringKeyReusedReason(entry SteeringEntry) string {
	return "idempotencyKey " + steeringOpsQuoted(entry.IdempotencyKey) + " was already used at " + entry.AppliedAt +
		" for a different batch - send different content under a new key"
}

// steeringOpRecords is the batch's ops as the entry records them.
func steeringOpRecords(ops []SteerOp) []SteeringOpRecord {
	out := make([]SteeringOpRecord, 0, len(ops))
	for _, op := range ops {
		r := SteeringOpRecord{Kind: op.Kind}
		switch op.Kind {
		case SteerOpAnnotate:
			r.Note = op.Note
		case SteerOpAddCriterion:
			r.Scenario, r.Surface, r.Presented, r.ExpectedEvidence = op.Scenario, op.Surface, op.Presented, op.ExpectedEvidence
		default:
			r.ID, r.Title = op.ID, op.Title
			if len(op.DependsOn) > 0 {
				r.DependsOn = append([]string{}, op.DependsOn...)
			}
		}
		out = append(out, r)
	}
	return out
}

// steeringSameBatch reports whether the recorded entry carries the batch now sent: the same rationale,
// evidence and ops. A work phase's prerequisites are a set (the add verb's own retry compares them so), so
// their order does not tell two batches apart.
func steeringSameBatch(entry SteeringEntry, batch SteerBatch) bool {
	if entry.Rationale != batch.Rationale || entry.Evidence != batch.Evidence {
		return false
	}
	sorted := func(ops []SteeringOpRecord) []SteeringOpRecord {
		out := make([]SteeringOpRecord, len(ops))
		for i, op := range ops {
			if op.DependsOn != nil {
				op.DependsOn = slices.Sorted(slices.Values(op.DependsOn))
			}
			out[i] = op
		}
		return out
	}
	return reflect.DeepEqual(sorted(entry.Ops), sorted(steeringOpRecords(batch.Ops)))
}

// steeringEvents is the rows a batch owes, in the oracle's order, each with its stable id: the steered
// row, then one dependency_registered row per add-work-phase that declared prerequisites (:292-306).
func steeringEvents(key, summary, rationale string, ops []SteerOp) []SteeringEventRecord {
	// The key is quoted in the id, so the id of one batch's row is never the id of another batch's: "k:op1" under key "k" and the
	// steered row of key "k:op1" are told apart by the closing quote.
	id := "steer:" + strconv.Quote(key)
	events := []SteeringEventRecord{{ID: id, Event: EventSteered, Detail: key + ": " + summary + " — " + rationale}}
	for i, op := range ops {
		if op.Kind != SteerOpAddWorkPhase || len(op.DependsOn) == 0 {
			continue
		}
		events = append(events, SteeringEventRecord{
			ID: id + ":op" + strconv.Itoa(i), Event: EventDependencyRegistered,
			Detail: op.ID + " dependsOn=" + strings.Join(op.DependsOn, ","),
		})
	}
	return events
}

// steeringRecorded is what a retry compares an entry's events against: the stable ids of the rows the plan's ledger holds, and, for a
// row that carries no id (written before rows had one), the event and detail it spells at the entry's time.
type steeringRecorded struct {
	ids    map[string]bool
	legacy map[[2]string]bool
}

func (r steeringRecorded) has(ev SteeringEventRecord) bool {
	return r.ids[ev.ID] || r.legacy[[2]string{string(ev.Event), ev.Detail}]
}

// steeringRecordedRows reads the plan's ledger one line at a time for the rows of an entry applied at ts. A line that is not an
// object matches nothing. A row is the entry's by its event id; only a row with no id is matched by what it spells at ts.
func steeringRecordedRows(cwd, slug, ts string) (steeringRecorded, error) {
	seen := steeringRecorded{ids: map[string]bool{}, legacy: map[[2]string]bool{}}
	path, err := goalplanLedgerPath(cwd, slug)
	if err != nil {
		return seen, err
	}
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return seen, nil
	}
	if err != nil {
		return seen, err
	}
	defer f.Close()
	r := bufio.NewReader(f)
	for {
		line, readErr := r.ReadBytes('\n')
		var row struct {
			Ts      string  `json:"ts"`
			Slug    string  `json:"slug"`
			Event   string  `json:"event"`
			Detail  string  `json:"detail"`
			EventID *string `json:"eventId"`
		}
		if json.Unmarshal(line, &row) == nil && row.Slug == slug {
			switch {
			case row.EventID != nil:
				seen.ids[*row.EventID] = true
			case row.Ts == ts:
				seen.legacy[[2]string{row.Event, row.Detail}] = true
			}
		}
		if errors.Is(readErr, io.EOF) {
			return seen, nil
		}
		if readErr != nil {
			return seen, readErr
		}
	}
}

// steeringRecordEvents appends the entry's rows, in order, each with its event id, skipping those recorded already (a retry; a
// fresh entry has none). It answers the warning of the first row that could not be written, "" when every row is in the ledger.
func steeringRecordEvents(cwd, slug string, entry SteeringEntry, recorded steeringRecorded, appendLedger func(cwd, slug string, entry GoalplanLedgerEntry) error) string {
	if appendLedger == nil {
		appendLedger = AppendGoalplanLedger
	}
	for _, ev := range entry.Events {
		if recorded.has(ev) {
			continue
		}
		id := ev.ID
		if err := appendLedger(cwd, slug, GoalplanLedgerEntry{Ts: entry.AppliedAt, Slug: slug, Event: ev.Event, Detail: ev.Detail, EventID: &id}); err != nil {
			return steeringLedgerWarning(slug, err)
		}
	}
	return ""
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
	var beforeWrite func()
	var appendLedger func(cwd, slug string, entry GoalplanLedgerEntry) error
	if o != nil {
		appendLedger = o.appendLedger
		if o.Now != nil {
			now = o.Now
		}
		lockOptions = o.Lock
		publish = o.publish
		beforeWrite = o.BeforeWrite
	}
	if lockOptions != nil {
		ctx = lockOptions.Context
	}
	locked, err := WithGoalplanWriteLock(cwd, slug, func(plan *Goalplan) (SteerResult, error) {
		return steeringApplyLocked(ctx, cwd, slug, plan, batch, now, publish, beforeWrite, appendLedger)
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
// it is not read again: the transaction finishes and answers as before. beforeWrite (nil for none) runs right
// after that last check, as the plan write begins.
func steeringApplyLocked(ctx context.Context, cwd, slug string, plan *Goalplan, batch SteerBatch, now func() string, publish *goalplanPublishedOptions, beforeWrite func(), appendLedger func(cwd, slug string, entry GoalplanLedgerEntry) error) (SteerResult, error) {
	for i := range plan.SteeringLog {
		if plan.SteeringLog[i].IdempotencyKey == batch.IdempotencyKey {
			existing := plan.SteeringLog[i]
			// A legacy entry records no batch and no events: it answers duplicate as before, and nothing it
			// may have lost is guessed at (CRW-1111).
			if existing.Ops == nil {
				return SteerResult{Kind: SteerResultDuplicate, Entry: &existing}, nil
			}
			// The same key with another batch would be answered as done without being applied; it is refused.
			if !steeringSameBatch(existing, batch) {
				return SteerResult{Kind: SteerResultRejected, Entry: &existing, Reason: steeringKeyReusedReason(existing)}, nil
			}
			// The same batch again: nothing is applied again, and a row the first attempt could not write is
			// recorded now, under this lock (CRW-1111). The oracle answers duplicate and the row stays lost.
			if ctx != nil {
				if err := ctx.Err(); err != nil {
					return SteerResult{}, err
				}
			}
			recorded, err := steeringRecordedRows(cwd, slug, existing.AppliedAt)
			if err != nil {
				return SteerResult{Kind: SteerResultDuplicate, Entry: &existing, Warning: steeringLedgerWarning(slug, err)}, nil
			}
			return SteerResult{Kind: SteerResultDuplicate, Entry: &existing, Warning: steeringRecordEvents(cwd, slug, existing, recorded, appendLedger)}, nil
		}
	}
	entry := SteeringEntry{
		IdempotencyKey: batch.IdempotencyKey,
		Rationale:      batch.Rationale,
		Evidence:       batch.Evidence,
		AppliedAt:      now(),
		Summary:        steeringBatchSummary(batch.Ops),
		Ops:            steeringOpRecords(batch.Ops),
	}
	entry.Events = steeringEvents(entry.IdempotencyKey, entry.Summary, entry.Rationale, batch.Ops)
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
	if beforeWrite != nil {
		beforeWrite()
	}
	if err := goalplanPublishedWriteGoalplan(cwd, &next, publish); err != nil {
		if !state.Published(err) {
			return SteerResult{}, err
		}
		warning = goalplanPublishedWarning(slug, err)
	}
	// The rows the entry owes, in the oracle's order: the steered row, then one row per phase that
	// actually declared prerequisites, so the batch that carried the edge is the row above it (:292-306).
	// A fresh entry has no row recorded yet; a row that cannot be written leaves the batch applied with a
	// warning, and the same batch sent again under the same key records it.
	if rowWarning := steeringRecordEvents(cwd, slug, entry, steeringRecorded{}, appendLedger); rowWarning != "" {
		return SteerResult{Kind: SteerResultApplied, Plan: &next, Entry: &entry, Warning: rowWarning}, nil
	}
	return SteerResult{Kind: SteerResultApplied, Plan: &next, Entry: &entry, Warning: warning}, nil
}
