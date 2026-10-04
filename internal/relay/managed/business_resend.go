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
	if value, present := receipt["delivery"]; present && value != "not_delivered" {
		return true
	}
	if value, present := receipt["attemptedEffects"]; present {
		switch effects := value.(type) {
		case []any:
			for _, effect := range effects {
				text, ok := effect.(string)
				if !ok || text == "turn/start" {
					return true
				}
			}
		case []string:
			for _, effect := range effects {
				if effect == "turn/start" {
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
// paginated history is not proof that the child has only its standby turn.
func (r *startRun) businessResendOnlyStandby(ctx context.Context) (string, error) {
	answer, err := r.m.Adapter.HostCall(ctx, "thread/turns/list", map[string]any{"threadId": r.task, "limit": 2, "itemsView": "summary"})
	if err != nil {
		return "", err
	}
	rows, ok := answer["data"].([]any)
	if !ok || len(rows) == 0 || answer["nextCursor"] != nil && answer["nextCursor"] != "" {
		return "lifecycle_unknown", nil
	}
	if len(rows) != 1 {
		return "business_identity_unobserved", nil
	}
	id, ok := pyjson.Map(rows[0])["id"].(string)
	if !ok || id == "" {
		return "lifecycle_unknown", nil
	}
	if id != r.standby {
		return "business_identity_unobserved", nil
	}
	return "", nil
}

func (r *startRun) businessResendReady(ctx context.Context) (string, error) {
	if code, err := r.businessResendOnlyStandby(ctx); code != "" || err != nil {
		return code, err
	}
	// Read load status last: if observing history itself loads the thread, this
	// gate notices and withholds before a bridge operation is consumed.
	answer, err := r.m.Adapter.HostCall(ctx, "thread/read", map[string]any{"threadId": r.task, "includeTurns": false})
	if err != nil {
		return "", err
	}
	thread := pyjson.Map(answer["thread"])
	if thread["id"] != r.task {
		return "lifecycle_unknown", nil
	}
	if pyjson.Map(thread["status"])["type"] != "notLoaded" {
		return "recipient_not_idle", nil
	}
	return "", nil
}
