package delivery

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Every reason, hold, state and next-action word this package emits that is NOT a member of the
// frozen errors.RefusalReason enum (contract/schema/relay-exit-codes.json). Each stays exact:
// the test asserts the Python source spells it as a quoted literal in the named module.
var literalWords = map[string][]string{
	"cli.py":               {"state_directory_serves_another_socket", "ambiguous_state_directory", "unidentified_state_directory"},
	"ack.py":               {"delivery_unconfirmed", "delivery_state_changed", "ack_predates_attempt", "already_settled", "replaced", "changed", "withheld", "verified", "unverified_turn", "host_read", "unrecorded", "revision_mismatch", "proceed", "already_claimed"},
	"policy.py":            {"attempt_cap", "busy_cap", "push_channel_closed", "host_lost_turn", "turn_check_undecided", "unknown_send_lost", "unknown_send_undecided", "unknown_send_hold_named", "min_send_interval", "hourly_cap"},
	"lifecycle.py":         {"recipient_archived", "recipient_paused", "recipient_usage_limited", "recipient_budget_limited", "recipient_cannot_accept_input", "recipient_busy", "recipient_system_error", "lifecycle_unknown"},
	"transport.py":         {"accepted", "failed", "outcome_unknown", "in_progress_or_unknown", "dispatched", "deferred_busy", "withheld_pre_send", "held_uncertain", "inbox_only", "queued", "acknowledged", "superseded", "sending", "settings_not_preserved", "setting_unobservable", "environments_unknown", "unverifiable_permission_profile", "settings_differ_after_load"},
	"currency.py":          {"sole_revision", "declared_chain", "no_revision", "fork", "cycle", "unknown_predecessor", "disconnected"},
	"delivery.py":          {"completion_event", "revision_request", "merge_turn_grant", "delivery_presend_withheld", "delivery_settings_noted", "dispatched_awaiting_ack", "stored_not_woken", "held_uncertain_awaiting_evidence", "awaiting_ack", "awaiting_child_receipt", "parent_busy", "channel_closed", "turn_accepted", "settings_rejected", "in_flight", "awaiting_send", "awaiting_receipt", "transport_accepted", "settings_check", "lifecycle_read", "parent_busy", "delivery_withheld_inactive", "delivery_withheld", "delivery_deferred_busy", "delivery_attempted", "delivery_attempt_settled_elsewhere", "delivery_queued", "delivery_superseded", "delivery_recipient_resolved", "restoration_attempted", "relationship_row", "linkage"},
	"reconcile.py":         {"turn_found", "receipt_turn_id", "confirmed_pre_send_rejection", "none", "reconciled"},
	"assignment.py":        {"daemon_reconciles_delivery", "daemon_confirms_correction", "parent_recovers_held_correction", "parent_reads_child_disposition", "parent_recovers_unknown_send_lost", "parent_recovers_unknown_send_undecided"},
	"hostloss.py":          {"present", "unknown", "listing_bounded", "listing_empty", "token_scan_bounded", "token_without_turn", "token_in_other_item", "no_send_time", "no_turn", "receipt_unsettled", "receipt_missing", "settled", "unsettled", "missing", "not_moved", "report_only", "held"},
	"criteria.py":          {"managed", "legacy", "covered", "legacy_unregistered"},
	"restoration.py":       {"carried", "truncated", "not_carried", "unmeasured", "relay-message/legacy"},
	"settings.py":          {"approval_policy_differs_from_record", "runtime_roots_narrower_than_record"},
	"registry.py":          {"anchor_pending", "bound", "needs_changes_revision", "initial_assignment", "anchor_conflict", "anchor_bound", "generation_opened", "status_changed"},
	"supervisorchannel.py": {"turn_predates_send"},
	"sync.py":              {"coordination_document", "sync_enqueued", "pending", "claimed", "confirmed"},
	"intent.py":            {"relationship_registered", "identity_bound", "ambiguous_identity", "intent_expired", "creation_unknown", "creation_accepted", "intent_declared", "bound", "unchanged", "conflict", "current", "stale", "absent", "accepted", "unknown", "failed", "in_progress", "blocked_needs_input", "interrupted", "ready_for_review"},
	"marker.py":            {"published", "exists", "flag", "env", "xdg", "home"},
	"declarations.py":      {"declarations/1", "recorded", "unchanged", "conflict", "not_recorded", "failed", "store_disagrees", "no_store_recorded", "store_absent", "store_unreadable", "store_not_a_file", "store_unopenable", "store_write_failed", "store_locked"},
	"cli.py#marker":        {"marker_unreadable", "marker_malformed", "claim_not_standing", "claim_uncorrelated", "store_changed"},
}

func TestLiteralReasons_outside_the_frozen_enum_are_spelled_as_python_spells_them(t *testing.T) {
	enum := map[string]bool{}
	schema, err := os.ReadFile(filepath.Join(repoRoot(t), "contract", "schema", "relay-exit-codes.json"))
	mustDo(t, err)
	for _, r := range refusalEnum(t, schema) {
		enum[r] = true
	}
	src := filepath.Join(repoRoot(t), "packages", "codex-session-relay", "src", "codex_session_relay")
	checked := 0
	for file, words := range literalWords {
		raw, err := os.ReadFile(filepath.Join(src, strings.Split(file, "#")[0]))
		mustDo(t, err)
		text := string(raw)
		for _, w := range words {
			if !strings.Contains(text, `"`+w+`"`) && !strings.Contains(text, `'`+w+`'`) {
				t.Errorf("%s does not spell %q as a literal", file, w)
			}
			checked++
		}
	}
	// And the Go constants carrying them are the same words.
	for _, pair := range [][2]string{
		{AttemptCap, "attempt_cap"}, {PushChannelClosed, "push_channel_closed"}, {HostLostTurn, "host_lost_turn"}, {HourlyCap, "hourly_cap"},
		{LifecycleUnknown, "lifecycle_unknown"}, {RecipientArchived, "recipient_archived"}, {DeliveryUnconfirmed, "delivery_unconfirmed"},
		{AckPredatesAttempt, "ack_predates_attempt"}, {PresendWithheld, "delivery_presend_withheld"}, {SettingsNoted, "delivery_settings_noted"},
		{Sole, "sole_revision"}, {Chain, "declared_chain"}, {TurnFound, "turn_found"}, {ApprovalDiffersFromRecord, "approval_policy_differs_from_record"},
		{TurnPredatesSend, "turn_predates_send"}, {SettingsDifferAfterLoad, "settings_differ_after_load"},
		{RelationshipRegistered, "relationship_registered"}, {IdentityBound, "identity_bound"}, {AmbiguousIdentity, "ambiguous_identity"}, {IntentExpired, "intent_expired"},
		{CreationUnknown, "creation_unknown"}, {CreationAccepted, "creation_accepted"}, {IntentDeclared, "intent_declared"}, {Bound, "bound"}, {Unchanged, "unchanged"}, {Conflict, "conflict"},
		{DispatchCurrent, "current"}, {DispatchStale, "stale"}, {DispatchAbsent, "absent"}, {declarationsCapability, "declarations/1"}, {declRecorded, "recorded"},
		{declNotRecordedState, "not_recorded"}, {declFailed, "failed"}, {Published, "published"}, {Exists, "exists"},
	} {
		if pair[0] != pair[1] {
			t.Errorf("constant %q != %q", pair[0], pair[1])
		}
	}
	// Every refusal reason the package raises IS an enum member (or one of the cli.py literals).
	for _, r := range []string{NotClaimable, RecipientNotAuthorized, ScopeEscape, RelationshipNotActive, UnregisteredRelationship, UnknownGeneration, RelationUnreadable, RelationOwnerDrift, DuplicateScopeOwner, LinkConflict, UnregisteredScope,
		SettingsUnavailable, SettingsIncomplete, SettingsMistyped, UnsupportedSandboxType, UnsupportedApprovalPolicy, RolePolicyUnconfigured, RoleBindingMismatch, CriteriaUnregistered, CriteriaNotCovered, CriteriaSetChanged,
		UnknownCriterion, FindingsRequired, DispositionConflict, ReviewNotBound, WrongDeliveryKind, AckProofMismatch, AckTurnUnverified, NotAcknowledged, RestorationUndeliverable, StaleGeneration, SupersededRevision, RevisionAmbiguous, SyncNotClaimable,
		RelationshipConflict, UnboundGeneration, OutcomeInconsistent} {
		if !enum[r] {
			t.Errorf("%q is raised as a refusal but is not in the frozen enum", r)
		}
	}
	if checked < 145 {
		t.Fatalf("only %d literals checked", checked)
	}
}

// refusalEnum reads the frozen RefusalReason values (refusalReasons: NAME -> value).
func refusalEnum(t *testing.T, raw []byte) []string {
	var doc struct {
		RefusalReasons map[string]string `json:"refusalReasons"`
	}
	mustDo(t, json.Unmarshal(raw, &doc))
	var out []string
	for _, v := range doc.RefusalReasons {
		out = append(out, v)
	}
	if len(out) < 100 {
		t.Fatalf("read %d enum members", len(out))
	}
	return out
}
