package managed

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strconv"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
)

// This file is the read-only reader of the unload-and-resend recovery that managed start ran before the
// orphan archive and creation retry replaced it (CRW-1211). That recovery sent a business turn again under
// a successor operation id after a certain pre-turn failure and, for a child the host held loaded under
// other settings, lowered the child first (thread/archive, then thread/unarchive) and wrote
// managed_resend_unloaded rows for it. Nothing here writes a row, archives, unarchives or creates a
// successor: a request that holds such rows keeps the meaning they had, and every other request sends its
// business turn once.
//
//   - The rows of a business attempt are read, not only counted. A begin row without a closing row (an archive that
//     may or may not have happened) is held whatever the host reports now, and so is a closing row that records no
//     completed lowering (nothing archived, an archive of unknown outcome, an unarchive that failed): the child is
//     neither archived again nor sent to.
//   - A closing row of a lowering that archived and unarchived the child is replayed as recorded: the successor
//     operations the earlier calls made are walked, an accepted successor replays its admission and never starts a
//     turn again, and a successor not yet sent is sent once the host reports the child unloaded (notLoaded) with
//     its standby turn as its whole history.
//   - The request, dispatch, relationship and generation identity are untouched: the successor operation ids
//     are derived from the request and the attempt exactly as before, and nothing else changes.

const maxLegacyResendAttempts = 3 // original operation and two derived operations

// legacyResendID is the bridge operation id of business attempt n of a request: attempt 0 is the dispatch id,
// and each successor derives from the request and n, exactly as the earlier calls derived it.
func legacyResendID(request, first string, attempt int) string {
	if attempt == 0 {
		return first
	}
	sum := sha256.Sum256([]byte(request + "\x00resend\x00" + strconv.Itoa(attempt)))
	return "managed-business-" + hex.EncodeToString(sum[:])
}

// Explicit delivery/effect evidence vetoes even a contradictory not_attempted
// status. Malformed evidence cannot establish that no turn/start went out.
func legacyResendTurnPossible(receipt map[string]any) bool {
	if receipt["turnId"] != nil {
		return true
	}
	_, hasEffects := receipt["attemptedEffects"]
	if strings.HasPrefix(pyjson.Text(receipt["error"]), "turn/start:") && (receipt["status"] != "not_attempted" || !hasEffects) {
		return true
	}
	if value, present := receipt["delivery"]; present && value != "not_delivered" {
		return true
	}
	if value, present := receipt["attemptedEffects"]; present {
		switch effects := value.(type) {
		case []any:
			if len(effects) > 1 {
				return true
			}
			for _, effect := range effects {
				text, ok := effect.(string)
				if !ok || text != "thread/resume" {
					return true
				}
			}
		case []string:
			if len(effects) > 1 {
				return true
			}
			for _, effect := range effects {
				if effect != "thread/resume" {
					return true
				}
			}
		default:
			return true
		}
	}
	return false
}

// legacyResendSafe is a business failure that certainly happened before turn/start.
func legacyResendSafe(receipt map[string]any, task string) bool {
	if receipt["status"] != "failed" || receipt["operation"] != "send_message_to_thread" || receipt["threadId"] != task || legacyResendTurnPossible(receipt) {
		return false
	}
	if _, explicit := receipt["attemptedEffects"]; explicit {
		return true // the complete, typed effect list was checked above
	}
	if _, explicit := receipt["delivery"]; explicit {
		return false // a delivery label alone is not an effect trace
	}
	// Older relay receipts have neither effect nor delivery fields. This exact
	// local settings refusal is written after resume verification and before
	// turn/start; arbitrary errors, host rejections and timeouts do not qualify.
	return strings.HasPrefix(pyjson.Text(receipt["error"]), "thread/resume: ") && pyjson.Map(receipt["rpcError"])["code"] == "settings_not_preserved"
}

// What businessOperations decided about the business send.
const (
	businessSend   = iota // the current operation id has no attempt yet: ask the standby and the host, then send
	businessHold          // the receipts so far stand: checkBusiness answers from r.sent
	businessReplay        // an accepted successor: Send proves the exact packet from its ledger and replays, with no effect
)

// legacyResend reports whether the request holds managed_resend_unloaded rows, written by the earlier
// recovery. The rows are the only thing that licenses a successor business operation now.
func (r *startRun) legacyResend(ctx context.Context) (bool, error) {
	var rows int
	err := r.m.Store.Querier(ctx).QueryRowContext(ctx, "SELECT COUNT(*) FROM journal WHERE kind='managed_resend_unloaded' AND subject=?", r.identity.RequestID).Scan(&rows)
	return rows > 0, err
}

// legacyLowered reports whether the rows of business attempt n allow its successor operation: the attempt has no
// managed_resend_unloaded row (the earlier recovery found the child unloaded already and lowered nothing), or its newest
// row is a closing row that records an archive (ok, or reply_lost, where the archived listing showed it) and an unarchive
// that succeeded. A begin row without its closing row, and a closing row of an archive that did not happen, whose outcome
// is unknown or that left the child archived, do not license a successor operation; the host's load status is no
// evidence for them (another client may have unloaded the child).
func (r *startRun) legacyLowered(ctx context.Context, attempt int) (bool, error) {
	rows, err := r.m.Store.Querier(ctx).QueryContext(ctx, "SELECT detail FROM journal WHERE kind='managed_resend_unloaded' AND subject=? ORDER BY seq DESC", r.identity.RequestID)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return false, err
		}
		decoder := json.NewDecoder(bytes.NewBufferString(raw))
		decoder.UseNumber()
		var row map[string]any
		if decoder.Decode(&row) != nil {
			continue // a row this reader cannot read licenses nothing
		}
		if number, ok := row["attempt"].(json.Number); !ok || number.String() != strconv.Itoa(attempt) {
			continue
		}
		archive, unarchive := pyjson.Text(row["archive"]), pyjson.Text(row["unarchive"])
		return row["phase"] == "end" && (archive == "ok" || archive == "reply_lost") && unarchive == "ok", rows.Err()
	}
	return true, rows.Err() // no row of this attempt
}

// businessOperations walks the business operation ids the ledger holds for the request and settles what
// the send does. Attempt 0 is the request's own operation. A certain pre-turn failure is followed by the
// successor operations the earlier recovery derived (at most two), but a successor that has no attempt yet
// is sent only for a request that holds managed_resend_unloaded rows; any other request keeps the failure
// and answers from it.
func (r *startRun) businessOperations(ctx context.Context) (int, error) {
	legacy, err := r.legacyResend(ctx)
	if err != nil {
		return 0, err
	}
	for r.businessAttempt = 0; r.businessAttempt < maxLegacyResendAttempts; r.businessAttempt++ {
		r.businessOperationID = legacyResendID(r.identity.RequestID, r.identity.DispatchRequestID, r.businessAttempt)
		sent, err := r.m.Adapter.GetOperation(ctx, r.businessOperationID)
		if err != nil {
			return 0, err
		}
		r.sent = sent
		if sent == nil || sent["status"] == "not_attempted" && !legacyResendTurnPossible(sent) {
			if r.businessAttempt > 0 && !legacy {
				r.sent = r.resendFailure
				return businessHold, nil
			}
			return businessSend, nil
		}
		safe := legacyResendSafe(sent, r.task)
		if safe {
			// The loop overwrites r.sent with the successor lookup, so the failure that licensed
			// this successor is kept here.
			r.resendFailure = sent
		}
		if !safe || r.businessAttempt+1 == maxLegacyResendAttempts {
			if r.businessAttempt > 0 && sent["status"] == "accepted" {
				return businessReplay, nil
			}
			return businessHold, nil
		}
	}
	return businessHold, nil
}

// standbyOnlyHistory reads the child's turn history: "" when a complete one-row listing holds exactly the
// recorded standby turn, business_identity_unobserved when it shows a turn that is not the standby (conclusive
// even on an incomplete page), lifecycle_unknown when it is empty, malformed or paginated.
//
// This is the precondition of every archive the engine performs on a child (settleOrphan): it archives only a
// child whose history is exactly its standby turn, so no sub-thread can exist to be carried into the archive
// with it. Widening that precondition beyond the standby-only history must first run the sub-thread check
// internal/relay/childcleanup owns; nothing on the archive path inspects sub-threads today.
func (r *startRun) standbyOnlyHistory(ctx context.Context) (string, error) {
	answer, err := r.m.Adapter.HostCall(ctx, "thread/turns/list", map[string]any{"threadId": r.task, "limit": 2, "itemsView": "summary"})
	if err != nil {
		return "", err
	}
	rows, ok := answer["data"].([]any)
	if !ok || len(rows) == 0 {
		return "lifecycle_unknown", nil
	}
	unknown := false
	for _, row := range rows {
		id, ok := pyjson.Map(row)["id"].(string)
		if !ok || id == "" {
			unknown = true
			continue
		}
		if id != r.standby {
			return "business_identity_unobserved", nil
		}
	}
	if unknown || len(rows) != 1 || answer["nextCursor"] != nil && answer["nextCursor"] != "" {
		return "lifecycle_unknown", nil
	}
	return "", nil
}

// threadStatus is the child's load status as one thread/read reports it, with the lifecycle_unknown code a
// read naming another thread earns.
func (r *startRun) threadStatus(ctx context.Context) (string, string, error) {
	answer, err := r.m.Adapter.HostCall(ctx, "thread/read", map[string]any{"threadId": r.task, "includeTurns": false})
	if err != nil {
		return "", "", err
	}
	thread := pyjson.Map(answer["thread"])
	if thread["id"] != r.task {
		return "", "lifecycle_unknown", nil
	}
	return pyjson.Text(pyjson.Map(thread["status"])["type"]), "", nil
}

// legacyResendReady is the gate before a successor business send of a request that holds
// managed_resend_unloaded rows: the attempt's rows must record a finished lowering (legacyLowered), the host must
// list exactly the recorded standby turn, then report the same child as notLoaded. A child the earlier recovery
// lowered and that the host has unloaded is sent to as the closing row recorded; any other (a begin row without its
// closing row, an archive that is not known to have happened, an unarchive that failed, a child still loaded) is
// held, and nothing is archived or unarchived here.
func (r *startRun) legacyResendReady(ctx context.Context) (string, error) {
	lowered, err := r.legacyLowered(ctx, r.businessAttempt)
	if err != nil {
		return "", err
	}
	if !lowered {
		return "recipient_not_idle", nil
	}
	if code, err := r.standbyOnlyHistory(ctx); code != "" || err != nil {
		return code, err
	}
	// Read load status last: if observing history itself loads the thread, this
	// gate notices and withholds before a bridge operation is consumed.
	status, code, err := r.threadStatus(ctx)
	if err != nil || code != "" {
		return code, err
	}
	if status == "notLoaded" {
		return "", nil
	}
	return "recipient_not_idle", nil
}
