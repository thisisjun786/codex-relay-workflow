// Code generated from packages/codex-session-relay/src/codex_session_relay/settings.py; DO NOT EDIT.
// The recovery prose is caller-visible, so it is copied rather than retyped.

package cli

const (
	shHoldOperatorThen          = "bring the recipient back under its recorded settings - another client loaded it under other ones, and it loads under the record once the host has unloaded it and the relay loads it again - or, if the user changed the task, re-record it with settings-record --source user_transition; a refusal of the record itself names its own recovery. The daemon retries on its own each pass until the attempt cap"
	shHoldDaemonThen            = "the daemon retries on its own each pass; if the host keeps answering without the setting, the operator compares its reading with settings-show"
	shHoldCappedThen            = "nothing sends this delivery again: read the report, then open a fresh execution generation (generation-open) if the work still needs verifying"
	shHoldChannelThen           = "the report is stored where the recipient reads it: read it and acknowledge"
	shHoldCorrectionChannelThen = "the revision request is stored where the child reads it, without waking the child, and it takes no acknowledgement: read it, then open a fresh execution generation (generation-open) if the child still has to be given it"
	shHoldUndeterminedCause     = "its settings cause was recorded before this revision wrote causes down and is not established here, so nothing here claims a settings fix"
	shHoldUndeterminedThen      = "the daemon retries on its own each pass; its settings cause was recorded before this revision wrote causes down and is not established here, so nothing here claims a settings fix: if it stays withheld, read the event's attempts and their receipts, then settings-show"
	shLaterByOperator           = "for later deliveries, the operator brings the recipient back under its recorded settings or re-records it (settings-record --source user_transition); settings-show names the difference"
	shLaterByOwner              = "for later deliveries, the thread's owner switches it back to an approval policy this transport carries (never or on-request)"
	shSettingsShow              = "settings-show"
	shShowEvent                 = "show-event"
)

// holdRefusalThen is HOLD_REFUSAL_THEN.
var holdRefusalThen = map[string]string{
	"role_binding_mismatch":          "the role gate refused the recorded authorization, and its refusal names the repair for the exact cause: refusalDetail beside this recovery is that refusal as the relay service wrote it. Declare the role in this host's execution policy, give the relay process its policy and restart it, fix the binding or the creation, or re-record from a user-attributed source, whichever it names; the code alone does not tell them apart",
	"role_policy_unconfigured":       "the role gate refused the recorded authorization, and its refusal names the repair for the exact cause: refusalDetail beside this recovery is that refusal as the relay service wrote it. Declare the role in this host's execution policy, give the relay process its policy and restart it, fix the binding or the creation, or re-record from a user-attributed source, whichever it names; the code alone does not tell them apart",
	"settings_incomplete":            "the recipient's authorization record was refused before any host call (missing, incomplete, mistyped, or a sandbox it does not state as a policy this transport carries), as refusalDetail beside this recovery says: record it again from the creation result or a user-attributed source (settings-record --source user_transition), and the next pass reads it again",
	"settings_mistyped":              "the recipient's authorization record was refused before any host call (missing, incomplete, mistyped, or a sandbox it does not state as a policy this transport carries), as refusalDetail beside this recovery says: record it again from the creation result or a user-attributed source (settings-record --source user_transition), and the next pass reads it again",
	"settings_record_stale_for_role": "the role gate refused the recorded authorization, and its refusal names the repair for the exact cause: refusalDetail beside this recovery is that refusal as the relay service wrote it. Declare the role in this host's execution policy, give the relay process its policy and restart it, fix the binding or the creation, or re-record from a user-attributed source, whichever it names; the code alone does not tell them apart",
	"settings_unavailable":           "the recipient's authorization record was refused before any host call (missing, incomplete, mistyped, or a sandbox it does not state as a policy this transport carries), as refusalDetail beside this recovery says: record it again from the creation result or a user-attributed source (settings-record --source user_transition), and the next pass reads it again",
	"unsupported_sandbox_type":       "the recipient's authorization record was refused before any host call (missing, incomplete, mistyped, or a sandbox it does not state as a policy this transport carries), as refusalDetail beside this recovery says: record it again from the creation result or a user-attributed source (settings-record --source user_transition), and the next pass reads it again",
}

// daemonSettingsCodes is DAEMON_SETTINGS_CODES.
var daemonSettingsCodes = map[string]bool{"setting_unobservable": true, "environments_unknown": true}

// presendSettingsRefusals is faultsweep.SETTINGS_REFUSALS.
var presendSettingsRefusals = map[string]bool{
	"environments_unknown":            true,
	"role_binding_mismatch":           true,
	"role_policy_unconfigured":        true,
	"setting_unobservable":            true,
	"settings_incomplete":             true,
	"settings_mistyped":               true,
	"settings_not_preserved":          true,
	"settings_record_stale_for_role":  true,
	"settings_unavailable":            true,
	"unsupported_approval_policy":     true,
	"unsupported_sandbox_type":        true,
	"unverifiable_permission_profile": true,
}
