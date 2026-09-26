package delivery

import (
	"errors"
	"fmt"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// Refusal reasons this package raises (errors.RefusalReason values, spelled as Python spells them).
const (
	NotClaimable              = "not_claimable"
	RecipientNotAuthorized    = "recipient_not_authorized"
	ScopeEscape               = "scope_escape"
	RelationshipNotActive     = "relationship_not_active"
	UnregisteredRelationship  = "unregistered_relationship"
	UnknownGeneration         = "unknown_generation"
	RelationUnreadable        = "relation_unreadable"
	RelationOwnerDrift        = "relation_owner_drift"
	DuplicateScopeOwner       = "duplicate_scope_owner"
	LinkConflict              = "link_conflict"
	UnregisteredScope         = "unregistered_scope"
	SettingsUnavailable       = "settings_unavailable"
	SettingsIncomplete        = "settings_incomplete"
	SettingsMistyped          = "settings_mistyped"
	UnsupportedSandboxType    = "unsupported_sandbox_type"
	UnsupportedApprovalPolicy = "unsupported_approval_policy"
	RolePolicyUnconfigured    = "role_policy_unconfigured"
	RoleBindingMismatch       = "role_binding_mismatch"
)

// Refused is a RelayError: a machine reason and the human detail beside it. It reuses the store's
// refusal type, so a refusal from either package reads the same way.
type Refused = store.RefusedError

func refuse(reason, format string, args ...any) error {
	return &store.RefusedError{Reason: reason, Detail: fmt.Sprintf(format, args...)}
}

// Reason is the refusal reason carried by err, or "".
func Reason(err error) string { return store.RefusalReason(err) }

// Detail is the refusal detail carried by err.
func Detail(err error) string {
	var r *store.RefusedError
	if errors.As(err, &r) {
		return r.Detail
	}
	return ""
}
