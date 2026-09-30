package store

import (
	"errors"
	"fmt"
)

// Refusal reasons shared with Python's errors.RefusalReason; the strings are machine-consumed.
const (
	ReasonUnregisteredRelationship = "unregistered_relationship"
	ReasonRelationshipNotActive    = "relationship_not_active"
	ReasonStaleGeneration          = "stale_generation"
	ReasonUnknownGeneration        = "unknown_generation"
	ReasonUnboundGeneration        = "unbound_generation"
	ReasonScopeEscape              = "scope_escape"
	ReasonSymlinkComponent         = "symlink_component"
	ReasonPathChanged              = "path_changed"
	ReasonPathRelocated            = "path_relocated"
	ReasonUnverifiablePathBinding  = "unverifiable_path_binding"
	ReasonInsufficientPathBinding  = "insufficient_path_binding"
	ReasonArtifactMutated          = "artifact_mutated_during_read"
	ReasonArtifactLeaseBroken      = "artifact_lease_broken"
	ReasonNotARegularFile          = "not_a_regular_file"
	ReasonManifestRequired         = "manifest_required"
	ReasonManifestForbidden        = "manifest_forbidden"
	ReasonManifestUnverified       = "manifest_unverified"
	ReasonRevisionMismatch         = "revision_mismatch"
	ReasonEventIDMismatch          = "event_id_mismatch"
	ReasonTurnRefMismatch          = "turnref_mismatch"
	ReasonUnassignedTurn           = "unassigned_turn"
	ReasonContradictoryObservation = "contradictory_observation"
	ReasonMalformedReceipt         = "malformed_receipt"
	ReasonOutcomeInconsistent      = "outcome_inconsistent"
	ReasonProducerNotPermitted     = "producer_not_permitted"
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
