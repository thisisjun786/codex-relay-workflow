package registry

import (
	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
)

// Who recovers a settings hold, and how (settings.py settings_hold_recovery, CRW-235). The
// texts are caller-visible and kept byte-identical to settings.py.
const (
	HoldOperatorThen = "bring the recipient back under its recorded settings - another client loaded it under" +
		" other ones, and it loads under the record once the host has unloaded it and the relay" +
		" loads it again - or, if the user changed the task, re-record it with settings-record" +
		" --source user_transition; a refusal of the record itself names its own recovery. The" +
		" daemon retries on its own each pass until the attempt cap"
	HoldDaemonThen = "the daemon retries on its own each pass; if the host keeps answering without the setting," +
		" the operator compares its reading with settings-show"
	HoldCappedThen = "nothing sends this delivery again: read the report, then open a fresh execution" +
		" generation (generation-open) if the work still needs verifying"
	HoldChannelThen           = "the report is stored where the recipient reads it: read it and acknowledge"
	HoldCorrectionChannelThen = "the revision request is stored where the child reads it, without waking the child, and it" +
		" takes no acknowledgement: read it, then open a fresh execution generation (generation-open)" +
		" if the child still has to be given it"
	HoldUndeterminedCause = "its settings cause was recorded before this revision wrote causes down and is not" +
		" established here, so nothing here claims a settings fix"
	HoldUndeterminedThen = "the daemon retries on its own each pass; " + HoldUndeterminedCause + ": if it stays" +
		" withheld, read the event's attempts and their receipts, then settings-show"
	LaterByOperator = "for later deliveries, the operator brings the recipient back under its recorded settings" +
		" or re-records it (settings-record --source user_transition); settings-show names the" +
		" difference"
	LaterByOwner = "for later deliveries, the thread's owner switches it back to an approval policy this" +
		" transport carries (never or on-request)"
	HoldRoleGateThen = "the role gate refused the recorded authorization, and its refusal names the repair for the" +
		" exact cause: refusalDetail beside this recovery is that refusal as the relay service wrote" +
		" it. Declare the role in this host's execution policy, give the relay process its policy and" +
		" restart it, fix the binding or the creation, or re-record from a user-attributed source," +
		" whichever it names; the code alone does not tell them apart"
	HoldRecordThen = "the recipient's authorization record was refused before any host call (missing, incomplete," +
		" mistyped, or a sandbox it does not state as a policy this transport carries), as" +
		" refusalDetail beside this recovery says: record it again from the creation result or a" +
		" user-attributed source (settings-record --source user_transition), and the next pass reads" +
		" it again"
)

// daemonSettingsCodes is settings.DAEMON_SETTINGS_CODES.
var daemonSettingsCodes = []string{SettingUnobservable, EnvironmentsUnknown}

// holdRefusalThen is settings.HOLD_REFUSAL_THEN.
var holdRefusalThen = map[string]string{
	string(contract.RefusalRolePolicyUnconfigured):     HoldRoleGateThen,
	string(contract.RefusalRoleBindingMismatch):        HoldRoleGateThen,
	string(contract.RefusalSettingsRecordStaleForRole): HoldRoleGateThen,
	SettingsUnavailable:                                HoldRecordThen,
	SettingsIncomplete:                                 HoldRecordThen,
	SettingsMistyped:                                   HoldRecordThen,
	UnsupportedSandboxType:                             HoldRecordThen,
}

func recovery(actor, command string, then string, later any) contract.OrderedObject {
	return contract.OrderedObject{{Key: "actor", Value: actor}, {Key: "command", Value: command}, {Key: "then", Value: then}, {Key: "laterDeliveries", Value: later}}
}

// SettingsHoldRecovery is settings.settings_hold_recovery: {actor, command, then,
// laterDeliveries}. kind is withheld, capped or channel_closed; source attempt, pre_send or
// undetermined; revision says the delivery is a revision request to the child.
func SettingsHoldRecovery(kind, code, source string, revision bool) contract.OrderedObject {
	channelThen := HoldChannelThen
	if revision {
		channelThen = HoldCorrectionChannelThen
	}
	if source == "undetermined" {
		switch kind {
		case "capped":
			return recovery("parent", ShowEvent, HoldCappedThen+"; "+HoldUndeterminedCause, nil)
		case "channel_closed":
			return recovery("parent", ShowEvent, channelThen+"; "+HoldUndeterminedCause, LaterByOwner)
		}
		return recovery("daemon", ShowEvent, HoldUndeterminedThen, nil)
	}
	roleThen := holdRefusalThen[code]
	switch kind {
	case "capped":
		var later any = LaterByOperator
		if roleThen != "" {
			later = "for later deliveries, " + roleThen
		}
		return recovery("parent", ShowEvent, HoldCappedThen, later)
	case "channel_closed":
		return recovery("parent", ShowEvent, channelThen, LaterByOwner)
	}
	if contains(daemonSettingsCodes, code) {
		return recovery("daemon", SettingsShow, HoldDaemonThen, nil)
	}
	if roleThen != "" {
		return recovery("operator", SettingsShow, roleThen, nil)
	}
	return recovery("operator", SettingsShow, HoldOperatorThen, nil)
}

// Next-action words (assignment.py) this package's precedence rule answers with.
const (
	ActionOperatorRestoresSettings = "operator_restores_recipient_settings"
	ActionParentRecoversSettings   = "parent_recovers_settings_hold"
	ActionSendPolicy               = "operator_changes_send_policy"
	ActionAwaitingAck              = "parent_acknowledges"
	ActionVerifyAck                = "daemon_verifies_acknowledgement"
	ActionReacknowledge            = "parent_reacknowledges"
	ActionReconcile                = "daemon_reconciles_delivery"
	ActionHostLostRedelivery       = "daemon_redelivers_host_lost_turn"
	ActionHostLostHeld             = "parent_recovers_host_lost_turn"
	ActionUnknownSendHeld          = "parent_recovers_unknown_send_lost"
	ActionUnknownSendUndecided     = "parent_recovers_unknown_send_undecided"
	ActionCorrectionUnsent         = "daemon_delivers_correction"
	ActionCorrectionUnconfirmed    = "daemon_confirms_correction"
	ActionCorrectionHeld           = "parent_recovers_held_correction"
	ActionCorrectionAnswered       = "parent_reads_child_disposition"
)

// neverReopens is assignment.never_reopens: a cap of zero.
func neverReopens(pacing any) bool {
	p := pyjson.Map(pacing)
	return len(p) > 0 && p["reason"] == "hourly_cap" && p["reopensAt"] == nil
}

// operatorRestoresSettings is assignment.operator_restores_settings.
func operatorRestoresSettings(delivery map[string]any) bool {
	hold := pyjson.Map(delivery["settingsHold"])
	source, _ := hold["source"].(string)
	if delivery["state"] != "withheld_pre_send" || hold["kind"] != "withheld" || (source != "attempt" && source != "pre_send") {
		return false
	}
	reason, _ := hold["reason"].(string)
	actor, _ := SettingsHoldRecovery("withheld", reason, source, false).Lookup("actor")
	return actor == "operator"
}

// CompletionNextAction is assignment.completion_next_action over a decoded projection; "" is
// None (NEXT_ACTION's answer stands).
func CompletionNextAction(state string, projection map[string]any) string {
	if state != "received" && state != "corrected" && state != "verifying" {
		return ""
	}
	completion := pyjson.Map(projection["completion"])
	delivery := pyjson.Map(completion["delivery"])
	if delivery == nil {
		return ""
	}
	ack := pyjson.Map(completion["ack"])
	lost, _ := delivery["hostLostAttempts"].(float64)
	settingsHold := pyjson.Map(delivery["settingsHold"])
	settlement := ack["settlement"]
	if settlement == "verified" {
		if state == "received" && ack["accepted"] == true {
			return "parent_verifies"
		}
		return ""
	}
	if hold, _ := delivery["holdReason"].(string); hold != "" && hold != "push_channel_closed" {
		switch hold {
		case "host_lost_turn":
			return ActionHostLostHeld
		case "unknown_send_lost":
			return ActionUnknownSendHeld
		case "unknown_send_undecided":
			return ActionUnknownSendUndecided
		}
		if settingsHold["kind"] == "capped" {
			return ActionParentRecoversSettings
		}
		if lost > 0 {
			return ActionHostLostHeld
		}
		return ""
	}
	switch delivery["state"] {
	case "held_uncertain", "sending":
		return ActionReconcile
	case "dispatched", "inbox_only":
		if settlement != nil {
			switch ack["lastReason"] {
			case nil, "unverified_turn", "delivery_unconfirmed":
				return ActionVerifyAck
			}
			return ActionReacknowledge
		}
		return ActionAwaitingAck
	case "queued", "deferred_busy", "withheld_pre_send":
		if neverReopens(delivery["pacing"]) {
			return ActionSendPolicy
		}
		if operatorRestoresSettings(delivery) {
			return ActionOperatorRestoresSettings
		}
		if lost > 0 {
			return ActionHostLostRedelivery
		}
		return "daemon_delivers"
	}
	return ""
}

// CorrectionNextAction is assignment.correction_next_action; "" is None.
func CorrectionNextAction(state string, projection map[string]any) string {
	correction := pyjson.Map(projection["correction"])
	delivery := pyjson.Map(correction["delivery"])
	if state != "needs_changes" || delivery == nil {
		return ""
	}
	if correction["supersession"] != nil || delivery["state"] == "superseded" {
		return ActionCorrectionAnswered
	}
	switch delivery["state"] {
	case "dispatched", "acknowledged":
		return ""
	}
	reason := pyjson.Map(correction["undeliveredReason"])
	if delivery["state"] == "inbox_only" || reason["source"] == "deliveries.hold_reason" {
		return ActionCorrectionHeld
	}
	switch delivery["state"] {
	case "sending", "held_uncertain":
		return ActionCorrectionUnconfirmed
	case "queued", "deferred_busy", "withheld_pre_send":
		if neverReopens(delivery["pacing"]) {
			return ActionSendPolicy
		}
		if operatorRestoresSettings(delivery) {
			return ActionOperatorRestoresSettings
		}
		return ActionCorrectionUnsent
	}
	return ""
}

// presendSettingsRefusals is faultsweep.SETTINGS_REFUSALS: a pre-send withhold on one of these
// reasons is a settings hold; a lifecycle or pause reason is none.
var presendSettingsRefusals = []string{
	SettingsUnavailable, SettingsIncomplete, SettingsMistyped, UnsupportedSandboxType, UnsupportedApprovalPolicy,
	string(contract.RefusalRolePolicyUnconfigured), string(contract.RefusalRoleBindingMismatch),
	string(contract.RefusalSettingsRecordStaleForRole), SettingsNotPreserved, SettingUnobservable,
	EnvironmentsUnknown, UnverifiablePermissionProfile,
}

// HoldColumns is one delivery's SETTINGS_HOLD_COLUMNS (delivery.py); "" is SQL NULL.
type HoldColumns struct {
	State, HoldReason, SettledSent, SettledReconciled, Presend, Request string
	SettingsAt, LifecycleAt, InactiveAt                                 string
}

type packedRow struct {
	seq    any
	detail contract.OrderedObject
}

// unpack is delivery._packed: a json_object('seq', 'detail') column, or nil.
func unpack(value string) *packedRow {
	if value == "" {
		return nil
	}
	decoded, err := decodeJSON([]byte(value))
	object, ok := decoded.(contract.OrderedObject)
	if err != nil || !ok {
		return nil
	}
	seq, _ := object.Lookup("seq")
	out := &packedRow{seq: seq, detail: contract.OrderedObject{}}
	if raw, _ := object.Lookup("detail"); raw != nil && raw != "" {
		text, ok := raw.(string)
		if !ok {
			return nil
		}
		inner, err := decodeJSON([]byte(text))
		if err != nil {
			return nil
		}
		if d, ok := inner.(contract.OrderedObject); ok {
			out.detail = d
		}
	}
	return out
}

func seqValue(seq any) float64 {
	switch v := seq.(type) {
	case float64:
		return v
	case interface{ Float64() (float64, error) }:
		f, _ := v.Float64()
		return f
	}
	return 0
}

func nullText(v string) any {
	if v == "" {
		return nil
	}
	return v
}

// SettingsHoldReading is delivery.settings_hold_reading: pure over one row.
func SettingsHoldReading(row *HoldColumns) contract.OrderedObject {
	reading := contract.OrderedObject{{Key: "kind", Value: nil}, {Key: "chosen", Value: nil}, {Key: "definitive", Value: false},
		{Key: "hold", Value: nil}, {Key: "presendOperation", Value: nil}}
	if row == nil {
		return reading
	}
	var kind string
	switch row.State {
	case "withheld_pre_send":
		kind = "withheld"
	case "inbox_only":
		kind = "channel_closed"
	}
	if row.State == "withheld_pre_send" && row.HoldReason != "" {
		kind = ""
		if row.HoldReason == "attempt_cap" {
			kind = "capped"
		}
	}
	if kind == "" {
		return reading
	}
	reading = reading.Set("kind", kind)
	var settlement *packedRow
	for _, one := range []*packedRow{unpack(row.SettledSent), unpack(row.SettledReconciled)} {
		if one != nil && (settlement == nil || seqValue(one.seq) > seqValue(settlement.seq)) {
			settlement = one
		}
	}
	presend := unpack(row.Presend)
	if presend != nil && (settlement == nil || seqValue(presend.seq) > seqValue(settlement.seq)) {
		operation, _ := presend.detail.Lookup("operation")
		reason, _ := presend.detail.Lookup("reason")
		reading = reading.Set("chosen", "pre_send")
		reading = reading.Set("definitive", true)
		if text, ok := operation.(string); ok {
			reading = reading.Set("presendOperation", text)
		}
		if text, ok := reason.(string); ok && contains(presendSettingsRefusals, text) {
			refusal, _ := presend.detail.Lookup("detail")
			if _, ok := refusal.(string); !ok {
				refusal = nil
			}
			reading = reading.Set("hold", contract.OrderedObject{{Key: "source", Value: "pre_send"}, {Key: "reason", Value: text},
				{Key: "field", Value: nil}, {Key: "requestId", Value: nil}, {Key: "detail", Value: refusal}})

		}
		return reading
	}
	if settlement != nil {
		if refusal, present := settlement.detail.Lookup("settingsRefusal"); present {
			reading = reading.Set("chosen", "attempt")
			reading = reading.Set("definitive", true)
			object, _ := refusal.(contract.OrderedObject)
			reason, _ := object.Lookup("reason")
			if text, ok := reason.(string); ok {
				field, _ := object.Lookup("field")
				if _, ok := field.(string); !ok {
					field = nil
				}
				reading = reading.Set("hold", contract.OrderedObject{{Key: "source", Value: "attempt"}, {Key: "reason", Value: text},
					{Key: "field", Value: field}, {Key: "requestId", Value: nullText(row.Request)}})

			}
			return reading
		}
	}
	if row.Request != "" {
		reading = reading.Set("chosen", "legacy")
	}
	cleared := row.SettingsAt != "" && ((row.LifecycleAt != "" && row.LifecycleAt > row.SettingsAt) ||
		(row.InactiveAt != "" && row.InactiveAt > row.SettingsAt))
	if kind == "channel_closed" || (row.SettingsAt != "" && !cleared) {
		reading = reading.Set("hold", contract.OrderedObject{{Key: "source", Value: "undetermined"}, {Key: "reason", Value: nil},
			{Key: "field", Value: nil}, {Key: "requestId", Value: nil}})

	}
	return reading
}
