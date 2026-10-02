package dagsched

import "strings"

// Dispositions: what one reading says of one live node.
const (
	DispReady   = "ready"
	DispWait    = "wait"
	DispDefer   = "defer"
	DispBlocked = "blocked"
	DispSkip    = "skip"
	DispDone    = "done"
)

// The closed set of reasons a reading gives (contract 7.1, 3.2, 7.6, 4.4). A reason is "<class>:<name>"; the one
// parameterised member is "wait:edge:<edge_id>" (WaitEdge).
const (
	DeferNoCapacity          = "defer:no_capacity"
	DeferCapacityUnmeasured  = "defer:capacity_unmeasured"
	DeferEditOverlap         = "defer:edit_overlap"
	DeferMergeWindow         = "defer:merge_window"
	DeferOwnershipUnverified = "defer:ownership_unverified"
	DeferAuthorityPending    = "defer:authority_pending"

	SkipAlreadyOwned = "skip:already_owned"

	BlockedManifestIncomplete    = "blocked:manifest_incomplete"
	BlockedManifestTampered      = "blocked:manifest_tampered"
	BlockedInputMissing          = "blocked:input_missing"
	BlockedInputHashMismatch     = "blocked:input_hash_mismatch"
	BlockedInputOutOfScope       = "blocked:input_out_of_scope"
	BlockedInputUnaccepted       = "blocked:input_unaccepted"
	BlockedAcceptanceTampered    = "blocked:acceptance_tampered"
	BlockedAcceptanceIncomplete  = "blocked:acceptance_incomplete"
	BlockedStaleHead             = "blocked:stale_head"
	BlockedStaleCriteria         = "blocked:stale_criteria"
	BlockedDecisionMismatch      = "blocked:decision_mismatch"
	BlockedIntegrationUnprovable = "blocked:integration_unprovable"
	BlockedEvidenceMismatch      = "blocked:evidence_mismatch"
	BlockedInconsistentInputs    = "blocked:inconsistent_inputs"
	BlockedInputUnverifiedAtUse  = "blocked:input_unverified_at_consumption"
	BlockedStalePredecessor      = "blocked:stale_predecessor"
	BlockedCreationUnknown       = "blocked:creation_unknown"
	BlockedEffectUnknown         = "blocked:effect_unknown"
	BlockedPredecessorCancelled  = "blocked:predecessor_cancelled"
	BlockedReleaseAbandoned      = "blocked:release_abandoned"
	BlockedEvicted               = "blocked:evicted"
	BlockedAmbiguousHead         = "blocked:ambiguous_head"

	DoneAccepted   = "done:accepted"
	DoneIntegrated = "done:integrated"

	// BlockedStaleEpoch is reserved for the coordinator fencing of CRW-185: nothing in this package emits it.
	BlockedStaleEpoch = "blocked:stale_epoch"

	// WaitEdgePrefix starts the one parameterised reason, wait:edge:<edge_id>.
	WaitEdgePrefix = "wait:edge:"
)

// WaitEdge is the reason of a node whose incoming edge is not yet satisfied.
func WaitEdge(edgeID string) string { return WaitEdgePrefix + edgeID }

// emittedReasons are the fixed members of the closed set; ReservedReasons are named in the contract but never emitted here.
var emittedReasons = []string{
	DeferNoCapacity, DeferCapacityUnmeasured, DeferEditOverlap, DeferMergeWindow, DeferOwnershipUnverified, DeferAuthorityPending,
	SkipAlreadyOwned,
	BlockedManifestIncomplete, BlockedManifestTampered, BlockedInputMissing, BlockedInputHashMismatch, BlockedInputOutOfScope,
	BlockedInputUnaccepted, BlockedAcceptanceTampered, BlockedAcceptanceIncomplete, BlockedStaleHead, BlockedStaleCriteria,
	BlockedDecisionMismatch, BlockedIntegrationUnprovable, BlockedEvidenceMismatch, BlockedInconsistentInputs,
	BlockedInputUnverifiedAtUse, BlockedStalePredecessor, BlockedCreationUnknown, BlockedEffectUnknown,
	BlockedPredecessorCancelled, BlockedReleaseAbandoned, BlockedEvicted, BlockedAmbiguousHead,
	DoneAccepted, DoneIntegrated,
}

// ReservedReasons are in the contract's vocabulary and not emitted by this build.
var ReservedReasons = []string{BlockedStaleEpoch}

// EmittedReasons is the fixed part of the closed set, in declaration order.
func EmittedReasons() []string { return append([]string(nil), emittedReasons...) }

// ReasonsClosed reports whether r is a member of the closed set a reading may emit: a fixed member, or wait:edge:<id>
// with a non-empty id. Reserved reasons are not members: a reading that carries one is a bug.
func ReasonsClosed(r string) bool {
	if rest, ok := strings.CutPrefix(r, WaitEdgePrefix); ok {
		return rest != ""
	}
	for _, e := range emittedReasons {
		if r == e {
			return true
		}
	}
	return false
}

// BlockedPath is one row of contract 4.4: the B-code, the reason a reading gives, and the existing refusal reason a command
// that must refuse uses (D-02: no new reason is added for these; the closed reason travels in the refusal's detail).
type BlockedPath struct {
	Code    string
	Reason  string
	Refusal string
}

// BlockedPaths are the seventeen paths of contract 4.4, in order.
var BlockedPaths = []BlockedPath{
	{"B-01", BlockedManifestIncomplete, "malformed_receipt"},
	{"B-02", BlockedManifestTampered, "revision_mismatch"},
	{"B-03", BlockedInputMissing, "manifest_unverified"},
	{"B-04", BlockedInputHashMismatch, "manifest_unverified"},
	{"B-05", BlockedInputOutOfScope, "scope_escape"},
	{"B-06", BlockedInputUnaccepted, "disposition_conflict"},
	{"B-07", BlockedAcceptanceTampered, "revision_mismatch"},
	{"B-08", BlockedAcceptanceIncomplete, "disposition_conflict"},
	{"B-09", BlockedStaleHead, "merge_candidate_moved"},
	{"B-10", BlockedStaleCriteria, "criteria_set_changed"},
	{"B-11", BlockedDecisionMismatch, "disposition_conflict"},
	{"B-12", BlockedIntegrationUnprovable, "disposition_conflict"},
	{"B-13", BlockedEvidenceMismatch, "revision_mismatch"},
	{"B-14", BlockedInconsistentInputs, "disposition_conflict"},
	{"B-15", BlockedInputUnverifiedAtUse, "disposition_conflict"},
	{"B-16", BlockedManifestIncomplete, "malformed_receipt"},
	{"B-17", BlockedInputMissing, "manifest_unverified"},
}
