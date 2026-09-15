"""One error taxonomy for the whole package.

Every refusal names a reason. A caller that cannot act on an exception type can still
act on the reason, and the reason is what gets stored in the refusals table and shown
by the CLI, so an operator never has to read a traceback to learn what was refused.
"""

from enum import Enum


class RefusalReason(str, Enum):
    # Registration and generations
    UNREGISTERED_RELATIONSHIP = "unregistered_relationship"
    RELATIONSHIP_NOT_ACTIVE = "relationship_not_active"
    RELATIONSHIP_CONFLICT = "relationship_conflict"
    STALE_GENERATION = "stale_generation"
    UNKNOWN_GENERATION = "unknown_generation"
    UNBOUND_GENERATION = "unbound_generation"
    ANCHOR_ALREADY_BOUND = "anchor_already_bound"

    # Scope and artifact authorization
    SCOPE_ESCAPE = "scope_escape"
    RECIPIENT_NOT_AUTHORIZED = "recipient_not_authorized"
    SYMLINK_COMPONENT = "symlink_component"
    PATH_CHANGED = "path_changed"
    PATH_RELOCATED = "path_relocated"
    ROOT_MOVED = "root_moved"
    UNVERIFIABLE_PATH_BINDING = "unverifiable_path_binding"
    INSUFFICIENT_PATH_BINDING = "insufficient_path_binding"
    ARTIFACT_MUTATED_DURING_READ = "artifact_mutated_during_read"
    ARTIFACT_LEASE_BROKEN = "artifact_lease_broken"
    NOT_A_REGULAR_FILE = "not_a_regular_file"

    # Receipts
    MANIFEST_REQUIRED = "manifest_required"
    MANIFEST_FORBIDDEN = "manifest_forbidden"
    MANIFEST_UNVERIFIED = "manifest_unverified"
    REVISION_MISMATCH = "revision_mismatch"
    EVENT_ID_MISMATCH = "event_id_mismatch"
    TURNREF_MISMATCH = "turnref_mismatch"
    UNASSIGNED_TURN = "unassigned_turn"
    CONTRADICTORY_OBSERVATION = "contradictory_observation"
    MALFORMED_RECEIPT = "malformed_receipt"
    OUTCOME_INCONSISTENT = "outcome_inconsistent"
    PRODUCER_NOT_PERMITTED = "producer_not_permitted"
    DUPLICATE_EVENT = "duplicate_event"

    # Delivery, acknowledgement, operator actions
    NOT_CLAIMABLE = "not_claimable"
    WRONG_DELIVERY_KIND = "wrong_delivery_kind"
    ACK_PROOF_MISMATCH = "ack_proof_mismatch"
    ACK_TURN_UNVERIFIED = "ack_turn_unverified"
    DISPOSITION_CONFLICT = "disposition_conflict"
    NOT_ACKNOWLEDGED = "not_acknowledged"
    OPERATOR_RELEASE_DISABLED = "operator_release_disabled"
    RELEASE_EVIDENCE_MISSING = "release_evidence_missing"

    # Authorized execution settings. A send that cannot establish what it is preserving is
    # refused before any transport call rather than allowed to inherit a host default.
    SETTINGS_UNAVAILABLE = "settings_unavailable"
    SETTINGS_INCOMPLETE = "settings_incomplete"
    UNSUPPORTED_SANDBOX_TYPE = "unsupported_sandbox_type"

    # Verification currency and canonical criteria. A verdict is a claim about a specific
    # revision judged against a specific set of obligations, so both have to still hold at the
    # moment it is written, not at the moment the caller started reading.
    SUPERSEDED_REVISION = "superseded_revision"
    REVISION_AMBIGUOUS = "revision_ambiguous"
    REVISION_LINEAGE_INVALID = "revision_lineage_invalid"
    CRITERIA_UNREGISTERED = "criteria_unregistered"
    CRITERIA_NOT_COVERED = "criteria_not_covered"
    CRITERIA_SET_CHANGED = "criteria_set_changed"
    UNKNOWN_CRITERION = "unknown_criterion"
    FINDINGS_REQUIRED = "findings_required"
    DUPLICATE_ASSIGNMENT = "duplicate_assignment"

    # Coordination-document synchronisation. Its failures are its own: none of them re-runs a
    # verification or resends a correction.
    READBACK_MISMATCH = "readback_mismatch"
    SYNC_NOT_CLAIMABLE = "sync_not_claimable"
    SYNC_TARGET_MISMATCH = "sync_target_mismatch"

    # A review is a claim about a specific revision judged against a specific set of criteria.
    # Both halves have to be pinned, and an integrator has to say which revision it integrated.
    REVIEW_NOT_BOUND = "review_not_bound"
    STALE_MARK_CONTEXT = "stale_mark_context"


class RelayError(Exception):
    """Base for every refusal this package raises."""

    reason: RefusalReason | None = None

    def __init__(self, reason: RefusalReason | None = None, detail: str = ""):
        self.reason = reason if reason is not None else self.reason
        self.detail = detail
        label = self.reason.value if self.reason else self.__class__.__name__
        super().__init__(f"{label}: {detail}" if detail else label)


class ScopeError(RelayError):
    """A path or a recipient lies outside the relationship's authorized scope."""


class RegistrationError(RelayError):
    """The relationship or generation is unknown, inactive, stale or conflicting."""


class ReceiptRefused(RelayError):
    """A completion receipt was not accepted, and the reason says why."""


class DeliveryRefused(RelayError):
    """A delivery could not be claimed, attempted or advanced."""


class AckRefused(RelayError):
    """An acknowledgement or verdict was not accepted."""


class StoreFault(RelayError):
    """Raised only by an injected fault hook, to prove a transaction rolls back."""
