package delivery

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"io"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyvalue"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// Code the product no longer calls, kept for the tests that drive it (decision 52).

// ExecuteCLI runs one delivery command as the codex-session-relay console script, with this
// package's own selection refusal. handled is false for any other command.
func ExecuteCLI(ctx context.Context, argv []string, stdout, stderr io.Writer) (int, bool) {
	return ExecuteAs(ctx, "codex-session-relay", argv, stdout, stderr, nil)
}

// CompleteKeptAcknowledgement is _complete_kept_acknowledgement: before a verdict, a kept
// acknowledgement's delivery is confirmed through its turn and the acknowledgement completed.
func CompleteKeptAcknowledgement(ctx context.Context, ack *Ack, rc *Reconciler, adapter Adapter, event string) error {
	if adapter == nil {
		return nil
	}
	turn, err := ack.KeptTurn(ctx, event)
	if err != nil || turn == "" {
		return err
	}
	if rc != nil {
		if _, err := rc.ConfirmDelivery(ctx, event, adapter, turn); err != nil {
			return err
		}
	}
	_, err = ack.CompletePending(ctx, event, adapter)
	return err
}

// Settled is the finished-turn memory, oldest checked first (daemon._turns_settled).
func (tc *TurnChecks) Settled() []string { return append([]string(nil), tc.settled...) }

func (a *Ack) kept(ctx context.Context, eventID string) (Row, error) {
	return one(ctx, a.Store, "SELECT a.event_id, a.ack_turn_id, a.ack_at, a.accepted, COALESCE(e.attempts, 0) AS attempts, e.last_reason, e.fingerprint, e.next_check_at FROM acks a JOIN ack_evidence e ON e.event_id = a.event_id WHERE a.event_id = ? AND a.verified = 'unverified_turn' AND e.last_reason = ?", eventID, DeliveryUnconfirmed)
}

// KeptTurn is kept_turn: the acknowledging turn of a kept acknowledgement, or "".
func (a *Ack) KeptTurn(ctx context.Context, eventID string) (string, error) {
	row, err := a.kept(ctx, eventID)
	if err != nil || row == nil {
		return "", err
	}
	return row.S("ack_turn_id"), nil
}

// CompletePending is complete_pending: one kept acknowledgement completed now, as the pending
// pass would. Nil when nothing is kept for this event.
func (a *Ack) CompletePending(ctx context.Context, eventID string, adapter Adapter) (Obj, error) {
	now := a.Clock.Now()
	pending, err := a.kept(ctx, eventID)
	if err != nil || pending == nil {
		return nil, err
	}
	row, err := a.Delivery.Find(ctx, eventID)
	if err != nil || row == nil {
		return nil, err
	}
	verification, err := a.verifyAckTurn(ctx, row, pending.S("ack_turn_id"), adapter)
	if err != nil {
		if Reason(err) == "" {
			return nil, err
		}
		verification = Reason(err)
	}
	return a.settlePendingAck(ctx, eventID, pending, verification, now, row)
}

// SetMode is set_mode.
func (c *Criteria) SetMode(ctx context.Context, rid, mode string) (Obj, error) {
	if mode != Managed && mode != Legacy {
		return nil, refuse(DispositionConflict, "unknown verification mode %s", pyvalue.StrRepr(mode))
	}
	err := c.Store.Transaction(ctx, func(ctx context.Context, _ *sql.Conn) error { return c.writeMode(ctx, rid, mode, c.Clock.ISO()) })
	return Obj{{Key: "relationshipId", Value: rid}, {Key: "mode", Value: mode}}, err
}

// FindInListing is find_in_listing over pages already read.
func FindInListing(pages []ListingPage, turnID string, sentAt float64) (TurnPresence, error) {
	next := 0
	return FindInListingPaged(func() (ListingPage, error) {
		page := pages[next]
		next++
		return page, nil
	}, len(pages), turnID, sentAt)
}

// FactCovered is covered: this exact fact, by identity AND digest, adjudicated by a resolution.
func FactCovered(fact Obj, resolutions []Obj) bool {
	return factCovered(fact, resolutions, nil)
}

func objMap(o Obj) map[string]any {
	m := map[string]any{}
	for _, f := range o {
		m[f.Key] = f.Value
	}
	return m
}

// MarkSuperseded is mark_superseded: withdraw what the recipient cannot be acting on yet.
func (d *Service) MarkSuperseded(ctx context.Context, eventID, reason string) error {
	if reason == "" {
		reason = SupersededHold
	}
	return d.Store.Transaction(ctx, func(ctx context.Context, _ *sql.Conn) error { return d.supersedeIn(ctx, eventID, reason) })
}

// Reschedule is _reschedule (tests reach it as Python's do).
func (d *Service) Reschedule(ctx context.Context, eventID, state string, when float64, attempts int64) error {
	return d.reschedule(ctx, eventID, state, when, attempts)
}

// DeferBusy is _defer_busy: a busy recipient is left alone and retried later.
func (d *Service) DeferBusy(ctx context.Context, eventID string, row Row, now float64) error {
	return d.deferBusy(ctx, eventID, row, now)
}

// AttemptMessages is attempt_messages: every attempt's bytes with the status those bytes
// actually reached, read from the attempt and never inferred from the bytes existing.
func (d *Service) AttemptMessages(ctx context.Context, eventID string) ([]any, error) {
	rows, err := all(ctx, d.Store, "SELECT a.request_id, a.attempt_no, a.internal_state, a.state, a.record, a.sent_at, m.message FROM attempts a LEFT JOIN attempt_messages m ON m.request_id = a.request_id WHERE a.event_id = ? ORDER BY a.attempt_no", eventID)
	if err != nil {
		return nil, err
	}
	out := make([]any, 0, len(rows))
	for _, row := range rows {
		var record Obj
		if row.S("record") != "" {
			record = loadsObj(row.S("record"))
		}
		status := "uncertain"
		switch state := row.S("state"); {
		case row.N("message"):
			status = "unavailable"
		case row.S("internal_state") != "settled":
			status = "prepared"
		case state == Dispatched || state == Acknowledged:
			status = Dispatched
		case state == HostLostTurn:
			status = HostLostTurn
		case record != nil && str(record, "sendAttempted") == "no":
			status = "confirmed_unsent"
		}
		out = append(out, Obj{{Key: "requestId", Value: row.S("request_id")}, {Key: "attemptNo", Value: row.I("attempt_no")}, {Key: "status", Value: status},
			{Key: "deliveryState", Value: row.Opt("state")}, {Key: "sentAt", Value: row.Opt("sent_at")}, {Key: "message", Value: row.Opt("message")}})
	}
	return out, nil
}

// SyncClaim is sync.claim: a lease and a per-claim token, which fences completion.
func SyncClaim(ctx context.Context, s *store.Store, clock Clock, id, owner string, now float64) (Obj, error) {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return nil, err
	}
	token := hex.EncodeToString(raw)
	err := s.Transaction(ctx, func(ctx context.Context, _ *sql.Conn) error {
		row, err := one(ctx, s, "SELECT * FROM sync_outbox WHERE sync_id = ?", id)
		if err != nil {
			return err
		}
		switch {
		case row == nil:
			return refuse(SyncNotClaimable, "no synchronisation job %s", pyvalue.StrRepr(id))
		case row.S("state") == "confirmed":
			return refuse(SyncNotClaimable, "%s is already confirmed", pyvalue.StrRepr(id))
		case row.S("state") == "failed":
			return refuse(SyncNotClaimable, "%s is failed after %d attempts; call retry to resume it deliberately", pyvalue.StrRepr(id), row.I("attempts"))
		case !row.N("next_attempt_at") && row.F("next_attempt_at") > now:
			return refuse(SyncNotClaimable, "%s is backing off until %s", pyvalue.StrRepr(id), pyvalue.Str(row.Opt("next_attempt_at")))
		case !row.N("lease_until") && row.F("lease_until") > now:
			return refuse(SyncNotClaimable, "%s is leased by %s until %s", pyvalue.StrRepr(id), pyvalue.Repr(row.Opt("lease_owner")), pyvalue.Str(row.Opt("lease_until")))
		}
		_, err = execSQL(ctx, s, "UPDATE sync_outbox SET state = ?, lease_owner = ?, lease_until = ?, claim_token = ?, updated_at = ? WHERE sync_id = ?", "claimed", owner, now+syncLeaseSeconds, token, clock.ISO(), id)
		return err
	})
	if err != nil {
		return nil, err
	}
	return Obj{{Key: "syncId", Value: id}, {Key: "claimToken", Value: token}, {Key: "owner", Value: owner}, {Key: "leaseUntil", Value: now + syncLeaseSeconds}}, nil
}

// SyncFenced is sync._fenced: only the claim currently held may act on a job.
func SyncFenced(ctx context.Context, s *store.Store, id, token string) (Row, error) {
	row, err := one(ctx, s, "SELECT * FROM sync_outbox WHERE sync_id = ?", id)
	if err != nil {
		return nil, err
	}
	if row == nil {
		return nil, refuse(SyncNotClaimable, "no synchronisation job %s", pyvalue.StrRepr(id))
	}
	if row.S("claim_token") != token {
		return nil, refuse(SyncNotClaimable, "this claim token is not the one currently held for %s", pyvalue.StrRepr(id))
	}
	return row, nil
}

// Record is MarkerSelection.to_record.
func (m MarkerSelection) Record() Obj {
	precedence := make([]any, len(MarkerPrecedence))
	for i, p := range MarkerPrecedence {
		precedence[i] = p
	}
	return Obj{{Key: "path", Value: m.Path}, {Key: "source", Value: m.Source}, {Key: "detail", Value: m.Detail}, {Key: "precedence", Value: precedence}}
}

const SyncNotClaimable = "sync_not_claimable"

const syncLeaseSeconds = 300.0

// SupersededHold is policy.py SUPERSEDED, the hold MarkSuperseded writes.
const SupersededHold = "superseded"

// MarkerPrecedence is PRECEDENCE: the order the marker root rules are asked in.
var MarkerPrecedence = []string{"flag", "env", "xdg", "home"}
