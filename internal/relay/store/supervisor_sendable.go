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

// The two holds a fault notice sets and releases itself. A hold keeps an unsent message out of the
// queue (see SupervisorClaimableSQL); a notice parks its message under SupervisorHoldSuperseded when
// what it said went up another way, and the channel holds a message under SupervisorHoldUnaddressed
// while the hierarchy no longer names its endpoints. Staging releases either of them and no other.
const (
	SupervisorHoldSuperseded  = "superseded_by_report"
	SupervisorHoldUnaddressed = "hierarchy_unresolved"
)

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

// SupervisorNoticeObligationKind is the obligation_kind a fault notice's message carries. The
// notice channel is the one relay-owned channel that yields the recipient's line to a delivery
// waiting out a busy backoff (I-216's notice part, section 82 decision 2); every other message
// keeps the oldest-of-the-claimable rule the rest of this file writes.
const SupervisorNoticeObligationKind = "fault_notification"

// YieldsToBusyHead reports whether this message yields the recipient's line to a delivery that
// waits out a busy backoff at now: only a fault notice does. Whether the line is held at all is the
// delivery path's own question (delivery.Service.BusyHeadHoldsRecipientLine), so the two channels
// cannot disagree about which deliveries hold a line.
func (r SupervisorMessagesRow) YieldsToBusyHead() bool {
	return r.ObligationKind == SupervisorNoticeObligationKind
}

// supervisorHeldNoticeExclusion is the exception an order condition carries for the recipients named in
// held, whose line a delivery holds under a busy backoff at now. The notice channel yields such a
// recipient's line to that delivery (I-216's notice part, section 82 decision 2), and a notice that
// yields cannot be claimed where its claim happens. So it is not a claimable row either, and a
// judgement that counts it as one lets it stand in front of a younger message to that recipient:
// the whole supervisor channel then yields with it, and an ordinary report waits for a fact that is
// not going to move.
//
// held is the caller's reading of the delivery path's own rule
// (delivery.Service.BusyHeadHoldsRecipientLine), so the two channels cannot disagree about which
// deliveries hold a line. It is written as an exception to the CLAIMABLE test alone and never to the
// in-flight or stranded ones, because that is the whole of what the yield takes away: a notice whose
// send is already under way goes ahead whatever the line does (I-216's in-flight clause), and one
// whose lease ran out is still recovered by the attempt that follows.
//
// It is a fragment to place after AND, not a condition: it binds one "?" per recipient, in the order
// given, exactly where it is placed. With none named it is empty and binds nothing, so a caller that
// holds no such reading asks exactly what it asked before this exclusion existed.
func supervisorHeldNoticeExclusion(alias string, held []string) string {
	if len(held) == 0 {
		return ""
	}
	marks := strings.TrimSuffix(strings.Repeat("?,", len(held)), ",")
	return " AND NOT (" + supervisorColumn(alias, "obligation_kind") + "='" + SupervisorNoticeObligationKind + "' AND " + supervisorColumn(alias, "recipient_task_id") + " IN (" + marks + "))"
}

// SupervisorHeldNoticeArgs are the values the three order conditions below bind for held, in text
// order: now once (the claimable test's own due), then one per recipient named (the yield
// exception), then now once (the in-flight or the stranded test). With none named it is now twice
// and nothing else, which is exactly what those conditions bound before the yield was excepted.
func SupervisorHeldNoticeArgs(held []string, now float64) []any {
	args := make([]any, 0, len(held)+2)
	args = append(args, now)
	for _, recipient := range held {
		args = append(args, recipient)
	}
	return append(args, now)
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
func SupervisorAheadSQL(alias string) string { return SupervisorAheadExceptYieldingSQL(alias, nil) }

// SupervisorAheadExceptYieldingSQL is SupervisorAheadSQL for a recipient whose line a delivery holds
// under a busy backoff at now: a claimable fault notice to that recipient is left out, because it
// cannot be claimed and so is not a row that goes ahead of the one behind it (I-216's notice part,
// CRW-943). held is the caller's reading of the delivery path's own rule
// (delivery.Service.BusyHeadHoldsRecipientLine), so the two channels cannot disagree about which
// deliveries hold a line. It binds SupervisorHeldNoticeArgs(held, now); with none named it is
// byte-for-byte the condition it was before this exclusion existed.
func SupervisorAheadExceptYieldingSQL(alias string, held []string) string {
	return "(" + SupervisorClaimableSQL(alias) + supervisorHeldNoticeExclusion(alias, held) + " OR " + supervisorLeaseLiveSQL(alias) + ")"
}

// SupervisorAheadInClaimSQL is SupervisorAheadSQL as the claim asks it under its write lock, where
// a hold excludes a message whatever its state. The two differ only for a sending message that
// carries a hold, which a hold landing between an attempt's read and its claim can produce. It
// binds now twice.
func SupervisorAheadInClaimSQL(alias string) string {
	return SupervisorAheadInClaimExceptYieldingSQL(alias, nil)
}

// SupervisorAheadInClaimExceptYieldingSQL is the same exclusion on the claim's own form of the
// condition, so the selection that led to the claim and the claim itself agree about what goes
// first. It binds SupervisorHeldNoticeArgs(held, now).
func SupervisorAheadInClaimExceptYieldingSQL(alias string, held []string) string {
	return "(" + supervisorColumn(alias, "hold_reason") + " IS NULL AND ((" + SupervisorUnsentSQL(alias) + " AND " + SupervisorDueSQL(alias) + supervisorHeldNoticeExclusion(alias, held) + ") OR " + supervisorLeaseLiveSQL(alias) + "))"
}

// SupervisorAttemptableSQL is true for a message a daemon pass may attempt: claimable, or stranded
// (the attempt recovers it first). It binds now twice.
func SupervisorAttemptableSQL(alias string) string {
	return SupervisorAttemptableExceptYieldingSQL(alias, nil)
}

// SupervisorAttemptableExceptYieldingSQL is SupervisorAttemptableSQL for a recipient whose line a
// delivery holds under a busy backoff at now: a claimable fault notice to that recipient is left
// out, so a daemon pass reads past a row it would only be refused by to the message behind it,
// instead of spending its turn on a fact that cannot move while the line is held (I-216's notice
// part, CRW-943). It binds SupervisorHeldNoticeArgs(held, now).
func SupervisorAttemptableExceptYieldingSQL(alias string, held []string) string {
	return "(" + SupervisorClaimableSQL(alias) + supervisorHeldNoticeExclusion(alias, held) + " OR " + supervisorLeaseExpiredSQL(alias) + ")"
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
