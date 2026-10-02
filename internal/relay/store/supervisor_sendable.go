package store

import (
	"slices"
	"strings"
)

// The supervisor_messages condition "this message can still be sent", written once.
//
// A supervisor message is unsent from its staging until a claim takes it for a send: it is
// queued, or deferred because the recipient was busy, or withheld before a send. Around that
// state the channel asks four further questions, and every statement and every Go check that
// asks one of them is built here, so a change to the condition is one edit:
//
//   - claimable: unsent, carrying no hold, and due (no eligibility time, or one that has come);
//   - ahead: what makes an older message go first for its recipient, claimable or in flight (a
//     send whose lease is still live);
//   - attemptable: what a daemon pass may pick up, claimable or stranded (a send whose lease ran
//     out, which the attempt recovers first);
//   - never sent: unsent with no attempt that may have gone, the only message staging and the
//     fault notices may still rewrite or re-address.
//
// The builders take the alias of the supervisor_messages table in the enclosing statement ("" when
// the statement has none) and each says how many "?" it binds, in text order. They return a
// fragment to place after AND or inside a WHERE, never a statement.

// supervisorUnsentStates are the states in which no send of the message is in flight or settled.
var supervisorUnsentStates = []string{"queued", "deferred_busy", "withheld_pre_send"}

// supervisorStateList is supervisorUnsentStates as the body of a SQL IN list.
var supervisorStateList = func() string {
	quoted := make([]string, len(supervisorUnsentStates))
	for i, state := range supervisorUnsentStates {
		quoted[i] = "'" + state + "'"
	}
	return strings.Join(quoted, ",")
}()

// SupervisorUnsent reports whether state is one a supervisor message holds while no send of it
// is in flight or settled.
func SupervisorUnsent(state string) bool { return slices.Contains(supervisorUnsentStates, state) }

// Unsent reports whether the message is in an unsent state.
func (r SupervisorMessagesRow) Unsent() bool { return SupervisorUnsent(r.State) }

// ClaimableAt reports whether a send of the message may be claimed at now: unsent, no hold, and
// not waiting for a later eligibility time. It is SupervisorClaimableSQL for a row already read.
func (r SupervisorMessagesRow) ClaimableAt(now float64) bool {
	return r.Unsent() && !r.HoldReason.Valid && !(r.NextEligibleAt.Valid && r.NextEligibleAt.Float64 > now)
}

func supervisorColumn(alias, name string) string {
	if alias == "" {
		return name
	}
	return alias + "." + name
}

// SupervisorUnsentSQL is the state test for an unsent message. It binds nothing.
func SupervisorUnsentSQL(alias string) string {
	return supervisorColumn(alias, "state") + " IN (" + supervisorStateList + ")"
}

// SupervisorRestatableSQL is the state test for a message whose packet may still be restated:
// unsent, or claimed and not yet on the transport. It binds nothing.
func SupervisorRestatableSQL(alias string) string {
	return supervisorColumn(alias, "state") + " IN (" + supervisorStateList + ",'sending')"
}

// SupervisorDueSQL is true when the message has no eligibility time or its time has come. It
// binds now once.
func SupervisorDueSQL(alias string) string {
	next := supervisorColumn(alias, "next_eligible_at")
	return "(" + next + " IS NULL OR " + next + "<=?)"
}

// SupervisorClaimableSQL is true for an unsent message with no hold that is due. It binds now once.
func SupervisorClaimableSQL(alias string) string {
	return "(" + SupervisorUnsentSQL(alias) + " AND " + supervisorColumn(alias, "hold_reason") + " IS NULL AND " + SupervisorDueSQL(alias) + ")"
}

// supervisorLeaseLiveSQL is true for a send in flight: sending, with a lease that has not run out.
// A message with no lease is neither live nor expired here. It binds now once.
func supervisorLeaseLiveSQL(alias string) string {
	return "(" + supervisorColumn(alias, "state") + "='sending' AND " + supervisorColumn(alias, "lease_until") + ">?)"
}

// supervisorLeaseExpiredSQL is true for a stranded send: sending, with a lease that ran out. It
// binds now once.
func supervisorLeaseExpiredSQL(alias string) string {
	return "(" + supervisorColumn(alias, "state") + "='sending' AND " + supervisorColumn(alias, "lease_until") + "<=?)"
}

// SupervisorAheadSQL is true for a message that goes ahead of a younger one to the same recipient:
// claimable, or in flight. A hold keeps a message out of the queue only while it is unsent. It
// binds now twice.
func SupervisorAheadSQL(alias string) string {
	return "(" + SupervisorClaimableSQL(alias) + " OR " + supervisorLeaseLiveSQL(alias) + ")"
}

// SupervisorAheadInClaimSQL is SupervisorAheadSQL as the claim asks it under its write lock, where
// a hold excludes a message whatever its state. The two differ only for a sending message that
// carries a hold, which a hold landing between an attempt's read and its claim can produce. It
// binds now twice.
func SupervisorAheadInClaimSQL(alias string) string {
	return "(" + supervisorColumn(alias, "hold_reason") + " IS NULL AND ((" + SupervisorUnsentSQL(alias) + " AND " + SupervisorDueSQL(alias) + ") OR " + supervisorLeaseLiveSQL(alias) + "))"
}

// SupervisorAttemptableSQL is true for a message a daemon pass may attempt: claimable, or stranded
// (the attempt recovers it first). It binds now twice.
func SupervisorAttemptableSQL(alias string) string {
	return "(" + SupervisorClaimableSQL(alias) + " OR " + supervisorLeaseExpiredSQL(alias) + ")"
}

// SupervisorOlderThanSQL is true for a message staged before the one at (staged_at, message_id):
// earlier, or at the same instant with a smaller id. It binds staged_at, staged_at and message_id.
func SupervisorOlderThanSQL(alias string) string {
	at, id := supervisorColumn(alias, "staged_at"), supervisorColumn(alias, "message_id")
	return "(" + at + "<? OR (" + at + "=? AND " + id + "<?))"
}

// SupervisorAttemptMayHaveGoneSQL is true for a supervisor_attempts row whose send may have left:
// it sent something, or its retry was not shown safe. It binds nothing.
func SupervisorAttemptMayHaveGoneSQL(alias string) string {
	return "(" + supervisorColumn(alias, "send_attempted") + "<>'no' OR " + supervisorColumn(alias, "retry_safe") + "=0)"
}

// SupervisorNeverSentSQL is true for an unsent message with no attempt that may have gone. It names
// the table itself, so it belongs in a statement over supervisor_messages without an alias. It
// binds nothing.
func SupervisorNeverSentSQL() string {
	return SupervisorUnsentSQL("supervisor_messages") + " AND NOT EXISTS (SELECT 1 FROM supervisor_attempts a WHERE a.message_id=supervisor_messages.message_id AND " + SupervisorAttemptMayHaveGoneSQL("a") + ")"
}
