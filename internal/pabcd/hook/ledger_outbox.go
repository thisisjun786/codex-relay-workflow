package hook

// ledger_outbox.go is the one place the hook and the orchestrate CLI drain a session's pending transition-ledger events
// (CRW-1097, state/outbox.go): every writer that records a transition prepares its row as a pending event before it publishes the
// state and drains it afterwards. Every holder of the session lock first judges what an earlier writer left (state.JudgeLedgerOutbox,
// called by the lock itself: published or never published, and the verdict is kept; a verdict that cannot be kept refuses the
// holder), so no write of the session, the memory, scan, evidence and idle-edit writers included, can be mistaken for an event's
// transition; the prompt hook, the Stop and
// PostCompact hooks and every orchestrate command then drain it, so a row whose writer died or whose append failed is recorded by
// the next one, exactly once. Status only reports the pending count.

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/goalplan"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/review"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
)

// DrainSessionLedger finishes the session's pending ledger events. The caller holds the session lock and no goalplan write lock
// (a followup takes one). published names the events the caller has just published itself, so the drain does not judge them from
// the state.
func DrainSessionLedger(cwd, sessionID string, published ...string) state.LedgerDrainReport {
	return DrainSessionLedgerWith(cwd, sessionID, published, nil)
}

// DrainSessionLedgerWith is DrainSessionLedger with the events whose followup the caller completed itself.
func DrainSessionLedgerWith(cwd, sessionID string, published, followupDone []string) state.LedgerDrainReport {
	known, done := map[string]bool{}, map[string]bool{}
	for _, id := range published {
		known[id] = true
	}
	for _, id := range followupDone {
		done[id] = true
	}
	return state.DrainLedgerOutbox(cwd, sessionID, state.LedgerDrainOptions{Published: known, FollowupDone: done, Followup: ledgerOutboxFollowup(cwd, sessionID)})
}

// DrainSessionLedgerRows is DrainSessionLedger for a caller that holds a goalplan write lock: it records
// rows only, and an event that carries followup work stays pending for a drain that can take the locks the
// followup needs.
func DrainSessionLedgerRows(cwd, sessionID string, published ...string) state.LedgerDrainReport {
	known := map[string]bool{}
	for _, id := range published {
		known[id] = true
	}
	return state.DrainLedgerOutbox(cwd, sessionID, state.LedgerDrainOptions{Published: known})
}

// ledgerOutboxFollowup is the followup handler of the session's events: the plan-audit cleanup of a P>A
// re-plan (CRW-1100), the one followup an event carries today, run under the bound plan's write lock.
func ledgerOutboxFollowup(cwd, sessionID string) func(state.LedgerEvent) error {
	return func(ev state.LedgerEvent) error {
		var cleanup PlanAuditCleanup
		if err := json.Unmarshal(ev.Followup, &cleanup); err != nil || cleanup.Kind != PlanAuditCleanupKind {
			return errors.New("the event carries a followup this build does not know")
		}
		locked, err := goalplan.WithGoalplanWriteLock(cwd, cleanup.Slug, func(plan *goalplan.Goalplan) (struct{}, error) {
			return struct{}{}, SupersedePlanAuditRounds(cwd, sessionID, cleanup, plan)
		}, nil)
		switch {
		case err != nil:
			return err
		case locked.Kind == "ok":
			return nil
		case locked.Kind == "unreadable" && locked.Reason == "goalplan '"+cleanup.Slug+"' does not exist":
			// The plan is gone, and its rounds with it: nothing is left to close.
			return nil
		}
		return errors.New(locked.Reason)
	}
}

// PlanAuditCleanupKind names the followup of a P>A event.
const PlanAuditCleanupKind = "plan-audit-supersede"

// PlanAuditCleanup is the followup a P>A event of a bound session carries (CRW-1100): the plan it minted the
// epoch in, the epoch itself and the exact plan_audit rounds of this session that the new epoch strands,
// listed under the plan's write lock before the state was published, the stamp the cleanup gives every
// round it closes, and the cleanup's own id, which every round it closes records as supersededBy. Recording
// them keeps one epoch and one round list across every retry: a reconcile never mints an epoch and never
// widens the list, and the id tells a round this cleanup closed from one that was aborted on its own, which
// is closed the same way (inconclusive, no verdict) and may carry the same millisecond stamp.
type PlanAuditCleanup struct {
	Kind     string   `json:"kind"`
	ID       string   `json:"id,omitempty"`
	Slug     string   `json:"slug"`
	Epoch    string   `json:"epoch"`
	Rounds   []string `json:"rounds"`
	ClosedAt string   `json:"closedAt,omitempty"`
}

// NewPlanAuditCleanup is the cleanup of rounds under epoch on slug, with its id minted and its closing stamp fixed now.
func NewPlanAuditCleanup(slug, epoch string, rounds []string) PlanAuditCleanup {
	var raw [8]byte
	_, _ = rand.Read(raw[:]) // crypto/rand.Read does not fail (Go 1.24 and later)
	return PlanAuditCleanup{Kind: PlanAuditCleanupKind, ID: "pac-" + hex.EncodeToString(raw[:]), Slug: slug, Epoch: epoch, Rounds: rounds,
		ClosedAt: time.Now().UTC().Format("2006-01-02T15:04:05.000Z")}
}

// planAuditSync is the durability step that ends a cleanup; a variable so a test can fail it.
var planAuditSync = goalplan.SyncGoalplanArtifacts

// SupersedePlanAuditRounds closes the rounds of c on plan, which the caller's write lock of c.Slug read, and
// records one review_round_superseded row per round this cleanup closed, now or in an earlier attempt (the
// round records the cleanup's id): a row the plan's ledger already holds is not written again, and a round
// that closed some other way (an abort, whatever its stamp) is never given a row. A plan write that published and then failed its directory sync, and a row that is
// visible but was never fsynced, are not taken for done: the plan's ledger and directory are made durable
// before the cleanup counts as finished, and the first failure is returned, so the caller keeps the work
// pending.
func SupersedePlanAuditRounds(cwd, sessionID string, c PlanAuditCleanup, plan *goalplan.Goalplan) error {
	swept, closed := review.SupersedeRounds(plan, goalplan.PurposePlanAudit, sessionID, c.Epoch, c.Rounds, c.ClosedAt, c.ID)
	var first error
	if len(closed) > 0 {
		// A publication whose directory sync failed is visible; the sync is made again below and its failure is reported there.
		if err := goalplan.WriteGoalplan(cwd, swept); err != nil && !state.Published(err) {
			return err
		}
	}
	owed := []string{}
	for _, r := range swept.ReviewRounds {
		if r.Purpose == goalplan.PurposePlanAudit && r.Status == goalplan.ReviewInconclusive && slices.Contains(c.Rounds, r.RoundID) &&
			!slices.Contains(owed, r.RoundID) && (slices.Contains(closed, r.RoundID) || c.ID != "" && r.SupersededBy == c.ID) {
			owed = append(owed, r.RoundID)
		}
	}
	if len(owed) > 0 {
		recorded, err := planAuditSupersededRows(cwd, c.Slug)
		if err != nil {
			return err
		}
		for _, roundID := range owed {
			if recorded[roundID] {
				continue
			}
			row := roundID
			if err := goalplan.AppendGoalplanLedger(cwd, c.Slug, goalplan.GoalplanLedgerEntry{
				Ts: time.Now().UTC().Format("2006-01-02T15:04:05.000Z"), Slug: c.Slug, Event: goalplan.EventReviewRoundSuperseded,
				Detail: "the plan was re-planned, so this round can no longer be spent", RoundID: &row,
			}); err != nil {
				return err
			}
		}
	}
	if len(c.Rounds) > 0 {
		first = planAuditSync(cwd, c.Slug)
	}
	return first
}

// planAuditSupersededRows is the set of round ids the plan's ledger already holds a review_round_superseded
// row for. The ledger is read one line at a time; a line that is not an object matches nothing.
func planAuditSupersededRows(cwd, slug string) (map[string]bool, error) {
	dir, err := goalplan.GoalplanDir(cwd, slug)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(filepath.Join(dir, goalplan.GoalplanLedgerFile))
	if errors.Is(err, fs.ErrNotExist) {
		return map[string]bool{}, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	seen := map[string]bool{}
	r := bufio.NewReader(f)
	for {
		line, readErr := r.ReadBytes('\n')
		var row struct {
			Event   string  `json:"event"`
			RoundID *string `json:"roundId"`
		}
		if json.Unmarshal(line, &row) == nil && row.Event == string(goalplan.EventReviewRoundSuperseded) && row.RoundID != nil {
			seen[*row.RoundID] = true
		}
		if errors.Is(readErr, io.EOF) {
			return seen, nil
		}
		if readErr != nil {
			return nil, readErr
		}
	}
}

// LedgerEventStillPending reports whether the event is among the ones a drain left pending.
func LedgerEventStillPending(report state.LedgerDrainReport, id string) bool {
	for _, ev := range report.Pending {
		if ev.ID == id {
			return true
		}
	}
	return false
}

// ledgerPendingReason is the reason a drain gives for a row it left pending.
func ledgerPendingReason(report state.LedgerDrainReport) string {
	if report.Err != nil {
		return report.Err.Error()
	}
	return "the row is still pending"
}

// promptOrchestrateRowPendingWarning is the line a chat command adds when its transition was applied and its ledger row could not
// be written yet: the row stays pending in the session's outbox and the next locked write of the session records it.
func promptOrchestrateRowPendingWarning(report state.LedgerDrainReport) string {
	return "[crw — warning: the transition was applied, but its ledger row could not be written yet (" +
		strings.TrimSpace(ledgerPendingReason(report)) + "); the row is kept pending and the next hook or orchestrate command of this session records it.]"
}
