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
    # Same string as the finding code the resume verification reports, so a receipt naming
    # this refusal and a delivery journal recording it read alike.
    UNSUPPORTED_APPROVAL_POLICY = "unsupported_approval_policy"

    # Role policy. Separate from the settings group above because these are questions about the
    # role a task HOLDS rather than about the settings a send is preserving, and the two have
    # different recoveries: one is re-recorded, one is a contradiction between how a task was
    # created and how it is being bound, and one is about this process rather than the task.
    ROLE_POLICY_UNCONFIGURED = "role_policy_unconfigured"
    ROLE_BINDING_MISMATCH = "role_binding_mismatch"
    SETTINGS_RECORD_STALE_FOR_ROLE = "settings_record_stale_for_role"

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

    # A correction that declares a restoration block and cannot carry it. Raised BEFORE the
    # next execution generation is opened, because afterwards there is no supported way to
    # send the block again and nothing to roll back to.
    RESTORATION_UNDELIVERABLE = "restoration_undeliverable"

    # Three-level execution linkage: an initiative supervisor over a project parent over an
    # issue child, plus peer links between parents. Each refusal the contract asks to be told
    # apart gets its own reason, because collapsing two of them is indistinguishable from not
    # detecting one of them.
    UNREGISTERED_SCOPE = "unregistered_scope"
    SCOPE_ROLE_MISMATCH = "scope_role_mismatch"
    SCOPE_CYCLE = "scope_cycle"
    FOREIGN_SCOPE = "foreign_scope"
    DUPLICATE_SCOPE_OWNER = "duplicate_scope_owner"
    HANDOVER_UNCONFIRMED = "handover_unconfirmed"
    LINK_CONFLICT = "link_conflict"
    LINK_NOT_ACTIVE = "link_not_active"
    # One task is bound to ONE Linear level by stable id. A second live binding of the same
    # role for the same task is its own refusal, told apart from a second OWNER of one scope.
    ROLE_ALREADY_BOUND = "role_already_bound"
    # Resolving a delivery's recipient THROUGH the linkage, rather than off the relationship row
    # that froze its parent at registration. Three answers the linkage reader keeps apart have to
    # stay apart here too, because each one asks the caller for something different.
    #
    # RELATION_UNREADABLE: the store did not answer. It is never reported as "nothing found" and
    # never as a resolved recipient, so there is no falling back to the frozen row: a store that
    # could not be read has said nothing about who owns the scope.
    # RELATION_OWNER_DRIFT: the linkage names a different owner than the relationship does. That
    # is the late report whose upper relationship changed. Delivering to the frozen parent would
    # credit a task that stepped down; delivering to the new owner would contradict
    # assert_assignment_delivery. Neither is chosen silently.
    RELATION_UNREADABLE = "relation_unreadable"
    RELATION_OWNER_DRIFT = "relation_owner_drift"
    # A handover that would leave work behind it cannot move. Distinct from an unconfirmed
    # one: the caller restated the outstanding set correctly and the operation is still
    # refused, because the endpoint it would have to move is part of an assignment's identity.
    HANDOVER_WOULD_STRAND = "handover_would_strand"


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


class LinkageError(RelayError):
    """A scope binding or a link between scopes was refused, and the reason says why."""
