package managed

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"

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
// thread/archive accepts an active sub-thread and unloads it, so the idle precondition is read
// again here, immediately before the archive. An archive failure leaves the child as it was and
// answers recipient_not_idle; an unarchive that fails twice answers lifecycle_unknown and names
// the archived thread for an operator. A child still loaded afterwards answers recipient_not_idle.
func (r *startRun) businessResendUnload(ctx context.Context) (string, error) {
	status, code, err := r.businessResendThreadStatus(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return "lifecycle_unknown", nil
	}
	if code != "" {
		return code, nil
	}
	if status != "idle" {
		return "recipient_not_idle", nil
	}
	detail := map[string]any{"threadId": r.task, "attempt": r.businessAttempt}
	if _, err := r.m.Adapter.HostCall(ctx, "thread/archive", map[string]any{"threadId": r.task}); err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		// Nothing was lowered, so nothing is recorded: the answer is the same one a loaded
		// child earns, and no unarchive follows a failed archive.
		return "recipient_not_idle", nil
	}
	detail["archive"] = "ok"
	var failure error
	for attempt := 0; attempt < 2; attempt++ {
		if _, failure = r.m.Adapter.HostCall(ctx, "thread/unarchive", map[string]any{"threadId": r.task}); failure == nil {
			break
		}
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
	}
	if failure != nil {
		detail["unarchive"] = "failed"
		detail["operator"] = "thread/unarchive " + r.task
		if err := r.recordResendUnload(ctx, detail); err != nil {
			return "", err
		}
		return "lifecycle_unknown", nil
	}
	detail["unarchive"] = "ok"
	status, code, err = r.businessResendThreadStatus(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		status, code = "unknown", "lifecycle_unknown"
	}
	detail["after"] = status
	if err := r.recordResendUnload(ctx, detail); err != nil {
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

// recordResendUnload writes the one managed_resend_unloaded row an unload leaves. The row is
// written before any send, and an insert that fails withholds the send: an unrecorded unload is
// never reported as done.
func (r *startRun) recordResendUnload(ctx context.Context, detail map[string]any) error {
	encoded, err := compactPythonJSON(detail)
	if err != nil {
		return err
	}
	_, err = r.m.Store.Querier(ctx).ExecContext(ctx, "INSERT INTO journal(at,kind,subject,detail) VALUES(?,?,?,?)", r.m.now(), "managed_resend_unloaded", r.identity.RequestID, string(encoded))
	return err
}
