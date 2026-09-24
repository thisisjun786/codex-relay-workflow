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
    # A required field is PRESENT and is not the type the resume contract declares. Kept apart
    # from SETTINGS_INCOMPLETE, which prescribes recording the absent field: that is not the
    # repair here, and a row refused for this reason reports an EMPTY missing list beside it.
    SETTINGS_MISTYPED = "settings_mistyped"
    UNSUPPORTED_SANDBOX_TYPE = "unsupported_sandbox_type"
    # One code for one situation, on both sides of it: require_usable() raises this for a
    # RECORD asking for a policy this transport cannot carry, and the resume verification
    # reports the same string for a RESPONSE reporting one, so a receipt naming the refusal and
    # a delivery journal recording it read alike.
    UNSUPPORTED_APPROVAL_POLICY = "unsupported_approval_policy"

    # Role policy. Separate from the settings group above because these are questions about the
    # role a task HOLDS rather than about the settings a send is preserving, and the two have
    # different recoveries: one is re-recorded, one is a contradiction between how a task was
    # created and how it is being bound, and one is about this process rather than the task.
    ROLE_POLICY_UNCONFIGURED = "role_policy_unconfigured"
    ROLE_BINDING_MISMATCH = "role_binding_mismatch"
    SETTINGS_RECORD_STALE_FOR_ROLE = "settings_record_stale_for_role"
    # The same string the bridge's own tool path reports for this case, so one situation does
    # not read as two different causes depending on which surface refused it.
    UNVERIFIED_PAIR_FOR_UNLOADED_THREAD = "unverified_pair_for_unloaded_thread"

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
    # Coordination between parents: whose turn it is to merge into a shared target, how much
    # concurrent execution a scope has, and what two peers agreed about a shared edit region.
    # Appended as one block at the END of the enum so a sibling inserting members mid-enum and
    # this work never produce an overlapping hunk.
    #
    # A caller's next action differs per reason, which is why none of these folds into
    # another: waiting helps a held target and never helps an unresolved one, an unmeasured
    # bound is not a reached one, and a region nobody may claim is not a region somebody else
    # already claimed.
    MERGE_TURN_NOT_HELD = "merge_turn_not_held"
    MERGE_TURN_UNRESOLVED = "merge_turn_unresolved"
    MERGE_CANDIDATE_MOVED = "merge_candidate_moved"
    MERGE_CURRENCY_STALE = "merge_currency_stale"
    MERGE_REVIEW_INCOMPLETE = "merge_review_incomplete"
    MERGE_EVIDENCE_REQUIRED = "merge_evidence_required"
    # A count this store can take is never evidence about a file-descriptor or spend bound it
    # cannot. "We never measured" and "it is full" are different answers and get different
    # reasons.
    CAPACITY_EXHAUSTED = "capacity_exhausted"
    CAPACITY_UNMEASURED = "capacity_unmeasured"
    SLOT_UNKNOWN = "slot_unknown"
    REGION_TOO_BROAD = "region_too_broad"
    REGION_OVERLAP = "region_overlap"
    AGREEMENT_NOT_OPEN = "agreement_not_open"
    AGREEMENT_REVISION_STALE = "agreement_revision_stale"
    FOLLOWUP_UNASSIGNED = "followup_unassigned"

    # Operational faults. Appended as one block at the END for the reason the block above
    # states: a sibling lane inserting members mid-enum and this work never produce an
    # overlapping hunk.
    #
    # Each names a different next action, which is why none folds into another. A malformed
    # observation is repaired by its author; an unregistered class is a class nobody declared
    # a clear source for; an uncertain write needs somebody to go and LOOK rather than retry;
    # and the three verification refusals ask for three different things - a reverification
    # at all, one recorded after the fix it verifies, and an explanation of the recurrence
    # that followed it.
    FAULT_OBSERVATION_MALFORMED = "fault_observation_malformed"
    FAULT_CLASS_UNREGISTERED = "fault_class_unregistered"
    FAULT_UNKNOWN = "fault_unknown"
    FAULT_STATE_CONFLICT = "fault_state_conflict"
    FAULT_NOT_CLAIMABLE = "fault_not_claimable"
    FAULT_CLAIM_STALE = "fault_claim_stale"
    FAULT_WRITE_UNCERTAIN = "fault_write_uncertain"
    FAULT_READBACK_MISMATCH = "fault_readback_mismatch"
    FAULT_UNVERIFIED = "fault_unverified"
    FAULT_VERIFICATION_STALE = "fault_verification_stale"
    FAULT_RECURRED_AFTER_VERIFICATION = "fault_recurred_after_verification"
    # The corrected contract's refusals, appended for the same reason as the block above. Each
    # names a different next action: reconcile a write before adopting over it, keep one
    # product per scope key, take a write over explicitly, wait for the budget window, leave a
    # fixed policy alone, and load the module that registers a kind before acting on it.
    FAULT_ADOPT_CONFLICT = "fault_adopt_conflict"
    FAULT_SCOPE_CONFLICT = "fault_scope_conflict"
    FAULT_WRITER_CONFLICT = "fault_writer_conflict"
    FAULT_BUDGET_SPENT = "fault_budget_spent"
    FAULT_POLICY_FIXED = "fault_policy_fixed"
    FAULT_KIND_UNREGISTERED = "fault_kind_unregistered"

    # Product routing (CRW-206). Appended at the end for the same merge reason as the block
    # above. Each names what to do next: repair the input; register the product or leave the
    # incident to classification; connect the surface or stop sending it; read the route again;
    # or wait for the ledger capability this checkout does not provide yet.
    ROUTE_INPUT_MALFORMED = "route_input_malformed"
    ROUTE_PRODUCT_UNKNOWN = "route_product_unknown"
    ROUTE_SURFACE_UNWATCHED = "route_surface_unwatched"
    ROUTE_STATE_CONFLICT = "route_state_conflict"
    ROUTE_LEDGER_PENDING = "route_ledger_pending"

    # CRW-229: the base a merge turn records is read from the target, never taken from a caller.
    # Appended as one block at the END for the reason the blocks above state. Each names a
    # different next action: make the target readable (or wait for it), read the branch again
    # because it disagrees with what was stated, and merge before landing (or record a merge
    # that changed nothing through report-unknown and resolve).
    MERGE_TARGET_UNREADABLE = "merge_target_unreadable"
    MERGE_BASE_MISMATCH = "merge_base_mismatch"
    MERGE_BASE_NOT_ADVANCED = "merge_base_not_advanced"



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


class CoordinationError(RelayError):
    """A merge turn, an execution slot or an edit-region agreement was refused."""
