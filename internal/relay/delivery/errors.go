package delivery

import (
	"errors"
	"fmt"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// Refusal reasons this package raises. Each is a member of the contract's generated refusal enum
// (contract/schema/relay-exit-codes.json), so its spelling has one source.
const (
	NotClaimable              = string(contract.RefusalNotClaimable)
	RecipientNotAuthorized    = string(contract.RefusalRecipientNotAuthorized)
	ScopeEscape               = string(contract.RefusalScopeEscape)
	RelationshipNotActive     = string(contract.RefusalRelationshipNotActive)
	UnregisteredRelationship  = string(contract.RefusalUnregisteredRelationship)
	UnknownGeneration         = string(contract.RefusalUnknownGeneration)
	RelationUnreadable        = string(contract.RefusalRelationUnreadable)
	RelationOwnerDrift        = string(contract.RefusalRelationOwnerDrift)
	DuplicateScopeOwner       = string(contract.RefusalDuplicateScopeOwner)
	LinkConflict              = string(contract.RefusalLinkConflict)
	UnregisteredScope         = string(contract.RefusalUnregisteredScope)
	SettingsUnavailable       = string(contract.RefusalSettingsUnavailable)
	SettingsIncomplete        = string(contract.RefusalSettingsIncomplete)
	SettingsMistyped          = string(contract.RefusalSettingsMistyped)
	UnsupportedSandboxType    = string(contract.RefusalUnsupportedSandboxType)
	UnsupportedApprovalPolicy = string(contract.RefusalUnsupportedApprovalPolicy)
	RolePolicyUnconfigured    = string(contract.RefusalRolePolicyUnconfigured)
	RoleBindingMismatch       = string(contract.RefusalRoleBindingMismatch)
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
