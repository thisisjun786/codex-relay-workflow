package delivery

import (
	"fmt"
	"slices"
	"strings"
)

// Transport vocabulary (transport.py). These spellings are stored in the database and read by
// every status surface.
const (
	Accepted       = "accepted"
	FailedStatus   = "failed"
	OutcomeUnknown = "outcome_unknown"
	Unfinished     = "in_progress_or_unknown"

	Dispatched      = "dispatched"
	DeferredBusy    = "deferred_busy"
	WithheldPreSend = "withheld_pre_send"
	HeldUncertain   = "held_uncertain"
	InboxOnly       = "inbox_only"
	Queued          = "queued"
	Acknowledged    = "acknowledged"
	Superseded      = "superseded"
	Sending         = "sending"
)

var knownMethods = []string{"initialize", "thread/read", "thread/resume", "turn/start"}

// SettingsRefusals are the resume refusals decided before any turn/start (transport.py).
var SettingsRefusals = []string{"settings_not_preserved", "setting_unobservable", "environments_unknown", "unverifiable_permission_profile", "settings_differ_after_load"}

// Facts is transport.TransportFacts: one receipt turned into delivery facts.
type Facts struct {
	ReceiptStatus   string
	DeliveryState   string
	SendAttempted   string
	RetrySafe       bool
	FailedOperation any // string or nil
	TurnID          any // string or nil
	ApprovalPolicy  any
	RPCErrorCode    any
	ErrorText       any
}

func methodPrefix(errorText any) (string, bool) {
	text, ok := errorText.(string)
	if !ok || !strings.Contains(text, ": ") {
		return "", false
	}
	candidate, _, _ := strings.Cut(text, ": ")
	return candidate, slices.Contains(knownMethods, candidate)
}

func usableTurnID(v any) any {
	if s, ok := v.(string); ok && strings.TrimSpace(s) != "" {
		return s
	}
	return nil
}

func truthy(v any) bool {
	switch t := v.(type) {
	case nil:
		return false
	case bool:
		return t
	case string:
		return t != ""
	case Obj:
		return len(t) > 0
	case []any:
		return len(t) > 0
	case int64:
		return t != 0
	case float64:
		return t != 0
	}
	return true
}

// Classify is transport.classify_operation_receipt. The error CODE is read before the method
// prefix, and an unfinished receipt is never proof of non-delivery.
func Classify(receipt Obj) Facts {
	if receipt == nil {
		return Facts{OutcomeUnknown, HeldUncertain, "unknown", false, "unclassified", nil, nil, nil, nil}
	}
	status, _ := get(receipt, "status")
	errorText, _ := get(receipt, "error")
	rpc, _ := get(receipt, "rpcError")
	var code any
	if o, ok := rpc.(Obj); ok {
		code, _ = get(o, "code")
	}
	resumed, _ := get(receipt, "resumed")
	var policy any
	if o, ok := resumed.(Obj); ok {
		policy, _ = get(o, "approvalPolicy")
	}
	rawTurn, _ := get(receipt, "turnId")
	turn := usableTurnID(rawTurn)
	f := func(rs, ds, sa string, safe bool, op any, turn any) Facts {
		return Facts{rs, ds, sa, safe, op, turn, policy, code, errorText}
	}
	switch status {
	case Accepted:
		if turn != nil {
			return f(Accepted, Dispatched, "yes", false, nil, turn)
		}
		return f(OutcomeUnknown, HeldUncertain, "unknown", false, "unclassified", nil)
	case Unfinished:
		return f(Unfinished, HeldUncertain, "unknown", false, nil, nil)
	case OutcomeUnknown:
		return f(OutcomeUnknown, HeldUncertain, "unknown", false, "transport", nil)
	case FailedStatus:
		if code == "thread_busy" && !truthy(resumed) {
			return f(FailedStatus, DeferredBusy, "no", true, "thread/read", nil)
		}
		if code == "unsupported_approval_policy" && truthy(resumed) {
			return f(FailedStatus, InboxOnly, "no", false, "thread/resume", nil)
		}
		if c, ok := code.(string); ok && slices.Contains(SettingsRefusals, c) && truthy(resumed) {
			return f(FailedStatus, WithheldPreSend, "no", true, "thread/resume", nil)
		}
		prefix, known := methodPrefix(errorText)
		switch {
		case known && prefix == "initialize":
			return f(FailedStatus, HeldUncertain, "unknown", false, "initialize", nil)
		case known && prefix == "turn/start":
			return f(FailedStatus, HeldUncertain, "yes", false, "turn/start", nil)
		case known && (prefix == "thread/read" || prefix == "thread/resume") && !truthy(resumed):
			return f(FailedStatus, WithheldPreSend, "no", true, prefix, nil)
		}
	}
	return f(OutcomeUnknown, HeldUncertain, "unknown", false, "unclassified", nil)
}

// AttemptRecord is transport.attempt_record: the frozen DeliveryAttempt shape.
func AttemptRecord(facts Facts, requestID, eventID string, attemptNo int64, recipient, statusBefore, observedAt string, reconciliation Obj) (Obj, error) {
	record := Obj{
		{Key: "requestId", Value: requestID},
		{Key: "eventId", Value: eventID},
		{Key: "attemptNo", Value: attemptNo},
		{Key: "recipientTaskId", Value: recipient},
		{Key: "deliveryState", Value: facts.DeliveryState},
		{Key: "sendAttempted", Value: facts.SendAttempted},
		{Key: "retrySafe", Value: facts.RetrySafe},
		{Key: "observedAt", Value: observedAt},
		{Key: "recipientStatusBefore", Value: statusBefore},
		{Key: "recipientApprovalPolicy", Value: facts.ApprovalPolicy},
		{Key: "transportReceiptStatus", Value: facts.ReceiptStatus},
		{Key: "failedOperation", Value: facts.FailedOperation},
		{Key: "turnId", Value: facts.TurnID},
	}
	if reconciliation != nil {
		record = append(record, F{Key: "reconciliation", Value: reconciliation})
	}
	return record, AssertAttemptInvariants(record)
}

// AssertAttemptInvariants re-implements the frozen schema's conditional rules before persisting.
func AssertAttemptInvariants(record Obj) error {
	state := str(record, "deliveryState")
	safe, _ := get(record, "retrySafe")
	sent := str(record, "sendAttempted")
	status := str(record, "transportReceiptStatus")
	op, _ := get(record, "failedOperation")
	turn, _ := get(record, "turnId")
	fail := func(rule string) error { return fmt.Errorf("delivery: attempt invariant: %s", rule) }
	if state == HeldUncertain && (safe != false || (sent != "unknown" && sent != "yes")) {
		return fail("an uncertain attempt is never retry-safe")
	}
	if safe == true {
		if sent != "no" || status != FailedStatus || (state != WithheldPreSend && state != DeferredBusy) || (op != "thread/read" && op != "thread/resume") {
			return fail("a retry-safe attempt is a completed pre-send failure")
		}
	}
	if status == Unfinished && (sent != "unknown" || safe != false) {
		return fail("an unfinished receipt is unknown and unsafe")
	}
	if state == Dispatched {
		if t, ok := turn.(string); !ok || t == "" || sent != "yes" || safe != false {
			return fail("a dispatch carries its turn")
		}
	}
	switch op {
	case "turn/start", "transport", "initialize", "unclassified":
		if safe != false || (sent != "unknown" && sent != "yes") {
			return fail("a post-send failure is never retry-safe")
		}
	}
	return nil
}
