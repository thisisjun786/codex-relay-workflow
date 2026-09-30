package store

import (
	"errors"
	"fmt"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
)

// Refusal reasons of the relay exit-code contract (contract/schema/relay-exit-codes.json, generated
// into internal/contract); the strings are machine-consumed.
const (
	ReasonUnregisteredRelationship = string(contract.RefusalUnregisteredRelationship)
	ReasonRelationshipNotActive    = string(contract.RefusalRelationshipNotActive)
	ReasonStaleGeneration          = string(contract.RefusalStaleGeneration)
	ReasonUnknownGeneration        = string(contract.RefusalUnknownGeneration)
	ReasonUnboundGeneration        = string(contract.RefusalUnboundGeneration)
	ReasonScopeEscape              = string(contract.RefusalScopeEscape)
	ReasonSymlinkComponent         = string(contract.RefusalSymlinkComponent)
	ReasonPathChanged              = string(contract.RefusalPathChanged)
	ReasonPathRelocated            = string(contract.RefusalPathRelocated)
	ReasonUnverifiablePathBinding  = string(contract.RefusalUnverifiablePathBinding)
	ReasonInsufficientPathBinding  = string(contract.RefusalInsufficientPathBinding)
	ReasonArtifactMutated          = string(contract.RefusalArtifactMutatedDuringRead)
	ReasonArtifactLeaseBroken      = string(contract.RefusalArtifactLeaseBroken)
	ReasonNotARegularFile          = string(contract.RefusalNotARegularFile)
	ReasonManifestRequired         = string(contract.RefusalManifestRequired)
	ReasonManifestForbidden        = string(contract.RefusalManifestForbidden)
	ReasonManifestUnverified       = string(contract.RefusalManifestUnverified)
	ReasonRevisionMismatch         = string(contract.RefusalRevisionMismatch)
	ReasonEventIDMismatch          = string(contract.RefusalEventIdMismatch)
	ReasonTurnRefMismatch          = string(contract.RefusalTurnrefMismatch)
	ReasonUnassignedTurn           = string(contract.RefusalUnassignedTurn)
	ReasonContradictoryObservation = string(contract.RefusalContradictoryObservation)
	ReasonMalformedReceipt         = string(contract.RefusalMalformedReceipt)
	ReasonOutcomeInconsistent      = string(contract.RefusalOutcomeInconsistent)
	ReasonProducerNotPermitted     = string(contract.RefusalProducerNotPermitted)
	// ReasonStoreAbsent is cli.py Services.store's refusal of a read-only command on a store
	// that does not exist (no D, takeover.json or write-gate.lock); like "store_absent" in
	// declarations.py it is a literal there, not a member of errors.RefusalReason.
	ReasonStoreAbsent = "store_absent"
)

// RefusedError is a refusal with a machine-readable reason, as Python's RelayError.
type RefusedError struct {
	Reason string
	Detail string
	cause  error
}

func (e *RefusedError) Error() string { return e.Reason + ": " + e.Detail }

func (e *RefusedError) Unwrap() error { return e.cause }

// RefusedBecause is a refusal that keeps the failure it was decided on reachable through
// errors.As, as Python chains a RelayError `from` its cause: a receipt refused because the host
// could not confirm the turn stays a HostUnavailable underneath.
func RefusedBecause(reason, detail string, cause error) *RefusedError {
	return &RefusedError{Reason: reason, Detail: detail, cause: cause}
}

func refuse(reason, format string, args ...any) error {
	return &RefusedError{Reason: reason, Detail: fmt.Sprintf(format, args...)}
}

// RefusalReason returns the reason carried by err, or "" when err is not a refusal.
func RefusalReason(err error) string {
	var refused *RefusedError
	if errors.As(err, &refused) {
		return refused.Reason
	}
	return ""
}
