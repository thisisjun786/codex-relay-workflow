package managed

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"strconv"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
)

const maxBusinessResendAttempts = 3 // original operation and two derived operations

func businessResendID(request, first string, attempt int) string {
	if attempt == 0 {
		return first
	}
	sum := sha256.Sum256([]byte(request + "\x00resend\x00" + strconv.Itoa(attempt)))
	return "managed-business-" + hex.EncodeToString(sum[:])
}

// Explicit delivery/effect evidence vetoes even a contradictory not_attempted
// status. Malformed evidence cannot establish that no turn/start went out.
func businessResendTurnPossible(receipt map[string]any) bool {
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

func businessResendSafe(receipt map[string]any, task string) bool {
	if receipt["status"] != "failed" || receipt["operation"] != "send_message_to_thread" || receipt["threadId"] != task || businessResendTurnPossible(receipt) {
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

// A one-row complete listing is affirmative evidence. Empty, malformed or
// paginated history is not proof that the child has only its standby turn, but
// a visible foreign turn is conclusive even on an incomplete page.
//
// This is also the unload's precondition, and the reason it is safe to archive the child:
// businessResendUnload lowers only a child whose history is exactly its standby turn, so no
// sub-thread can exist to be carried into the archive with it. Widening that precondition beyond
// the standby-only history must first run the sub-thread check internal/relay/childcleanup owns;
// nothing on the unload path inspects sub-threads today.
func (r *startRun) businessResendOnlyStandby(ctx context.Context) (string, error) {
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

// businessResendSettingsMismatch is the retained pre-turn failure that licenses the narrow
// unload: the same refusal businessResendSafe accepts, carrying the structured settings code.
func businessResendSettingsMismatch(receipt map[string]any, task string) bool {
	return businessResendSafe(receipt, task) && pyjson.Map(receipt["rpcError"])["code"] == "settings_not_preserved"
}

// businessResendThreadStatus is the child's load status as one thread/read reports it, with the
// lifecycle_unknown code a read naming another thread earns.
func (r *startRun) businessResendThreadStatus(ctx context.Context) (string, string, error) {
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

// businessResendReady is the gate before a bounded successor send. A notLoaded child goes on as
// it always did; an idle child the host holds loaded under other MCP settings, whose
// immediately preceding business failure is the structured settings refusal and whose history is
// exactly its standby turn, is lowered once and judged again. Every other state holds.
func (r *startRun) businessResendReady(ctx context.Context) (string, error) {
	if code, err := r.businessResendOnlyStandby(ctx); code != "" || err != nil {
		return code, err
	}
	// Read load status last: if observing history itself loads the thread, this
	// gate notices and withholds before a bridge operation is consumed.
	status, code, err := r.businessResendThreadStatus(ctx)
	if err != nil || code != "" {
		return code, err
	}
	if status == "notLoaded" {
		return "", nil
	}
	if status != "idle" || !businessResendSettingsMismatch(r.resendFailure, r.task) {
		return "recipient_not_idle", nil
	}
	return r.businessResendUnload(ctx)
}

// businessResendUnload lowers an idle child the host holds loaded under other MCP settings, once
// per business attempt, and reports why it could not. It never sends: the ordinary resend path
// does, and only after the child is notLoaded again with the standby-only history.
//
// thread/archive accepts an active sub-thread and unloads it, so the idle read comes last: the
// journal lookup, the readiness and ledger checks and the begin mark all precede it, and nothing
// but the host call sits between that read and the archive. The begin mark is written before any
// effect and a failed write archives nothing; the result row written afterwards may fail without
// reopening the bound, because the begin mark alone stops a second lowering. An archive error the
// host answered is a refusal, so this call did not apply the archive and the child holds as a
// loaded one does. An error with no host answer may still have applied, so the archived listing is
// asked once and a confirmed archive continues as a successful one; a complete listing that does
// not hold the child leaves it as it was and answers recipient_not_idle; a failed or incomplete
// listing leaves the archive unknown, closes the mark with archive "unknown" and keeps the attempt
// spent. Every way out that archived nothing (a cancelled or failed last read included) closes the
// begin mark with a row that says so (archive "none"), so it does not spend the attempt. An unarchive that fails twice answers lifecycle_unknown and names the archived thread
// for an operator. A child still loaded afterwards answers recipient_not_idle.
func (r *startRun) businessResendUnload(ctx context.Context) (string, error) {
	// One lowering per business attempt is a durable bound, not a per-invocation one: a replay of
	// an attempt whose unload did not unload the child reconstructs the same attempt from the
	// retained failure, so the rows an earlier lowering wrote are what stop the second one.
	lowered, err := r.resendUnloadLowered(ctx)
	if err != nil {
		return "", err
	}
	if lowered {
		return "recipient_not_idle", nil
	}
	// The engine's contract is that readiness is asked again before each host effect, and the
	// archive below is one. The ledger is asked with it, as the business send asks it.
	readiness, err := r.m.ready(ctx, r.req)
	if err != nil {
		return "", err
	}
	if readiness != "" {
		return readiness, nil
	}
	if err := r.m.Adapter.RequireLedger(ctx, r.ledger); err != nil {
		return "", err
	}
	if err := r.recordResendUnload(ctx, map[string]any{"threadId": r.task, "attempt": r.businessAttempt, "phase": "begin"}); err != nil {
		return "", err
	}
	// closeBegin closes the begin mark with a context that survives cancellation so the closing
	// row is not lost to it. archive "none" says nothing was archived and leaves the attempt
	// available; archive "unknown" says the archive may have applied and keeps it spent.
	closeBegin := func(archive, reason, code string) (string, error) {
		if err := r.recordResendUnload(context.WithoutCancel(ctx), map[string]any{"threadId": r.task, "attempt": r.businessAttempt, "phase": "end", "archive": archive, "reason": reason}); err != nil {
			return "", err
		}
		return code, nil
	}
	notArchived := func(reason, code string) (string, error) { return closeBegin("none", reason, code) }
	status, code, err := r.businessResendThreadStatus(ctx)
	if err != nil {
		if ctx.Err() != nil {
			// The read failed before any archive was sent, so nothing was archived: the attempt is
			// not spent, and the closing row says so before the context error is returned.
			if _, rerr := notArchived("status_unreadable", ""); rerr != nil {
				return "", rerr
			}
			return "", ctx.Err()
		}
		return notArchived("status_unreadable", "lifecycle_unknown")
	}
	if code != "" {
		return notArchived("status_unreadable", code)
	}
	if status != "idle" {
		return notArchived("not_idle", "recipient_not_idle")
	}
	detail := map[string]any{"threadId": r.task, "attempt": r.businessAttempt, "phase": "end"}
	if _, err := r.m.Adapter.HostCall(ctx, "thread/archive", map[string]any{"threadId": r.task}); err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		// The host's own error response is a refusal of this call, so this invocation did not apply
		// the archive: another client may have archived the child first. An archived listing cannot
		// tell which request archived it, and unarchiving here would undo that client's action, so
		// the child holds exactly as a loaded one does.
		var rpcErr *appserver.RPCError
		if errors.As(err, &rpcErr) {
			return notArchived("archive_refused", "recipient_not_idle")
		}
		// With no host answer read - a transport error, a closed connection, a deadline - the
		// archive may have applied and only its reply been lost, which would leave the child
		// archived until an operator unarchived it. Ask the host once, with the same complete
		// archived scan the resend guard uses and nothing else: whether the archive applied is a
		// fact of the archived listing alone, and a later state or goal read has no bearing on it.
		// Continue exactly as after a successful archive when the listing holds the child. A
		// complete listing that does not hold the child shows the archive did not apply: the child
		// answers as a loaded one does, the begin mark closes as archive "none" and no unarchive
		// follows. A failed or incomplete listing leaves the archive unknown: the child may be
		// archived, so the mark closes as archive "unknown" and the attempt stays spent, and an
		// operator who unarchives the child does not see the same attempt archive it a second time.
		archived, complete, checkErr := scanThreadListing(ctx, r.m.Adapter, r.task, true)
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		if checkErr != nil || !complete {
			return closeBegin("unknown", "archive_unconfirmed", "recipient_not_idle")
		}
		if !archived {
			return notArchived("archive_unconfirmed", "recipient_not_idle")
		}
		detail["archive"] = "reply_lost"
	} else {
		detail["archive"] = "ok"
	}
	// From here the child is archived, and every way out records the lowering with a context that
	// survives the caller's cancellation, as the creation attempt marker is: an archived child with
	// no row would leave an operator nothing to read, and an unarchive whose outcome a cancellation
	// made unknown is exactly that case.
	record := func() error { return r.recordResendUnload(context.WithoutCancel(ctx), detail) }
	var failure error
	for attempt := 0; attempt < 2; attempt++ {
		if _, failure = r.m.Adapter.HostCall(ctx, "thread/unarchive", map[string]any{"threadId": r.task}); failure == nil {
			break
		}
		if ctx.Err() != nil {
			break
		}
	}
	if failure != nil {
		detail["unarchive"] = "failed"
		detail["operator"] = "thread/unarchive " + r.task
		if err := record(); err != nil {
			return "", err
		}
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return "lifecycle_unknown", nil
	}
	detail["unarchive"] = "ok"
	status, code, err = r.businessResendThreadStatus(ctx)
	if err != nil {
		if ctx.Err() != nil {
			detail["after"] = "unknown"
			if rerr := record(); rerr != nil {
				return "", rerr
			}
			return "", ctx.Err()
		}
		status, code = "unknown", "lifecycle_unknown"
	}
	detail["after"] = status
	if err := record(); err != nil {
		return "", err
	}
	if code != "" {
		return code, nil
	}
	if status != "notLoaded" {
		return "recipient_not_idle", nil
	}
	return r.businessResendOnlyStandby(ctx)
}

// recordResendUnload writes one managed_resend_unloaded row of an unload: the begin mark before
// the archive, then the row that closes it. The begin row is written before any effect and an
// insert that fails withholds the archive; the closing row is written before any send and an
// insert that fails withholds the send: an unrecorded unload is never reported as done.
func (r *startRun) recordResendUnload(ctx context.Context, detail map[string]any) error {
	encoded, err := compactPythonJSON(detail)
	if err != nil {
		return err
	}
	_, err = r.m.Store.Querier(ctx).ExecContext(ctx, "INSERT INTO journal(at,kind,subject,detail) VALUES(?,?,?,?)", r.m.now(), "managed_resend_unloaded", r.identity.RequestID, string(encoded))
	return err
}

// resendUnloadLowered reports whether this business attempt already started a lowering, read from
// the rows the lowering wrote: the begin mark counts as the lowering itself, because it is written
// before the archive and a failed result row must not reopen the bound. The one exception is a
// lowering whose latest row says nothing was archived (archive "none"), which leaves the attempt
// as it was. The detail is the compact, key-sorted object recordResendUnload writes, so the
// attempt is the field "attempt":<n> followed by its separator.
func (r *startRun) resendUnloadLowered(ctx context.Context) (bool, error) {
	var detail string
	err := r.m.Store.Querier(ctx).QueryRowContext(ctx, "SELECT detail FROM journal WHERE kind='managed_resend_unloaded' AND subject=? AND detail LIKE ? ORDER BY seq DESC LIMIT 1", r.identity.RequestID, `%"attempt":`+strconv.Itoa(r.businessAttempt)+`,%`).Scan(&detail)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return !strings.Contains(detail, `"archive":"none"`), nil
}
