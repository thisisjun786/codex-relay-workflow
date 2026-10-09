package hook

// ledger_outbox.go is the one place the hook and the orchestrate CLI drain a session's pending transition-ledger events
// (CRW-1097, state/outbox.go): every writer that records a transition prepares its row as a pending event before it publishes the
// state and drains it afterwards, and every locked writer of the session drains whatever an earlier writer left, so a row whose
// writer died or whose append failed is recorded by the next one, exactly once.

import (
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
)

// DrainSessionLedger finishes the session's pending ledger events. The caller holds the session lock. published names the events
// the caller has just published itself, so the drain does not judge them from the state.
func DrainSessionLedger(cwd, sessionID string, published ...string) state.LedgerDrainReport {
	known := map[string]bool{}
	for _, id := range published {
		known[id] = true
	}
	return state.DrainLedgerOutbox(cwd, sessionID, state.LedgerDrainOptions{Published: known, Followup: ledgerOutboxFollowup(cwd, sessionID)})
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

// ledgerOutboxFollowup is the followup handler of the session's events. No event carries a followup yet.
func ledgerOutboxFollowup(_, _ string) func(state.LedgerEvent) error {
	return nil
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
