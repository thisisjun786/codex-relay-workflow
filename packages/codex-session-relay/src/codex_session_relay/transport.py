"""Turning one transport receipt into delivery facts.

The ordering matters more than the table. A busy refusal and an approval-policy refusal both
arrive as an RPC error whose method prefix would misclassify them, so the error CODE is read
first. And an unfinished receipt is never proof of non-delivery: the transport writes a
receipt before its calls run, so a missing field can simply mean it has not got there yet.
"""

from dataclasses import dataclass, field

KNOWN_METHODS = ("initialize", "thread/read", "thread/resume", "turn/start")

# Refusals decided from a resume response, before any turn/start. Each keeps its own code so an
# operator sees which one fired; they share one classification because they share one fact:
# nothing was sent. An UNRECOGNISED code is deliberately not in this set - an unknown refusal is
# not a known non-delivery, and it must stay uncertain.
SETTINGS_REFUSALS = (
    "settings_not_preserved",
    # The host reported no value for a requested setting. Decided from the resume response before
    # any turn/start, so nothing was sent: it belongs with the completed pre-send refusals. An
    # ABSENT approval policy therefore lands here rather than in inbox_only, because not seeing a
    # policy is not the same as seeing an interactive one and does not prove the push channel is
    # permanently closed. A REPORTED non-never policy is unchanged and still inbox_only.
    "setting_unobservable",
    "environments_unknown",
    "unverifiable_permission_profile",
)

ACCEPTED = "accepted"
FAILED = "failed"
OUTCOME_UNKNOWN = "outcome_unknown"
UNFINISHED = "in_progress_or_unknown"

DISPATCHED = "dispatched"
DEFERRED_BUSY = "deferred_busy"
WITHHELD_PRE_SEND = "withheld_pre_send"
HELD_UNCERTAIN = "held_uncertain"
INBOX_ONLY = "inbox_only"
QUEUED = "queued"
ACKNOWLEDGED = "acknowledged"
SUPERSEDED = "superseded"


@dataclass(frozen=True)
class TransportFacts:
    transport_receipt_status: str
    delivery_state: str
    send_attempted: str
    retry_safe: bool
    failed_operation: str | None
    turn_id: str | None = None
    approval_policy: str | None = None
    rpc_error_code: str | None = None
    error_text: str | None = None
    notes: tuple = field(default_factory=tuple)


def method_prefix(error_text) -> str | None:
    """RPC errors render as '<method>: <message>'. Anything else is not a method."""
    if not isinstance(error_text, str) or ": " not in error_text:
        return None
    candidate = error_text.split(": ", 1)[0]
    return candidate if candidate in KNOWN_METHODS else None


def _usable_turn_id(value) -> str | None:
    # A dispatched record needs a non-empty turn id; null or blank is not a dispatch.
    return value if isinstance(value, str) and value.strip() else None


def classify_operation_receipt(receipt) -> TransportFacts:
    if not isinstance(receipt, dict):
        return TransportFacts(OUTCOME_UNKNOWN, HELD_UNCERTAIN, "unknown", False, "unclassified")
    status = receipt.get("status")
    error_text = receipt.get("error")
    rpc_error = receipt.get("rpcError") or {}
    code = rpc_error.get("code")
    resumed = receipt.get("resumed")
    policy = (resumed or {}).get("approvalPolicy") if isinstance(resumed, dict) else None
    turn_id = _usable_turn_id(receipt.get("turnId"))

    if status == ACCEPTED:
        if turn_id:
            return TransportFacts(
                ACCEPTED, DISPATCHED, "yes", False, None, turn_id, policy, code, error_text
            )
        # Defensive: the current transport always sets a turn id before reporting acceptance,
        # so this is unreachable through it. Refusing an inconsistent receipt is still better
        # than inventing a dispatch that has no turn to point at.
        return TransportFacts(
            OUTCOME_UNKNOWN, HELD_UNCERTAIN, "unknown", False, "unclassified", None, policy,
            code, error_text, ("accepted receipt carried no usable turn id",),
        )
    if status == UNFINISHED:
        return TransportFacts(
            UNFINISHED, HELD_UNCERTAIN, "unknown", False, None, None, policy, code, error_text
        )
    if status == OUTCOME_UNKNOWN:
        return TransportFacts(
            OUTCOME_UNKNOWN, HELD_UNCERTAIN, "unknown", False, "transport", None, policy,
            code, error_text,
        )
    if status == FAILED:
        if code == "thread_busy" and not resumed:
            return TransportFacts(
                FAILED, DEFERRED_BUSY, "no", True, "thread/read", None, policy, code, error_text
            )
        if code == "unsupported_approval_policy" and resumed:
            return TransportFacts(
                FAILED, INBOX_ONLY, "no", False, "thread/resume", None, policy, code, error_text
            )
        if code in SETTINGS_REFUSALS and resumed:
            # A settings refusal is decided FROM the resume response, so resumed is present and
            # the generic pre-send branch below (which requires resumed to be absent) would miss
            # it. Left unmapped these fall through to held_uncertain, which is the worst answer
            # available: nothing was started, yet the attempt would be parked as possibly
            # delivered and reconciliation could never establish non-delivery from the receipt.
            #
            # retrySafe is true strictly as duplicate-delivery safety: this is a completed,
            # attributable local refusal before turn/start. It asserts nothing about whether the
            # resume itself changed host state.
            return TransportFacts(
                FAILED, WITHHELD_PRE_SEND, "no", True, "thread/resume", None, policy, code,
                error_text,
            )
        prefix = method_prefix(error_text)
        if prefix == "initialize":
            return TransportFacts(
                FAILED, HELD_UNCERTAIN, "unknown", False, "initialize", None, policy, code,
                error_text,
            )
        if prefix == "turn/start":
            return TransportFacts(
                FAILED, HELD_UNCERTAIN, "yes", False, "turn/start", None, policy, code, error_text
            )
        if prefix in ("thread/read", "thread/resume") and not resumed:
            return TransportFacts(
                FAILED, WITHHELD_PRE_SEND, "no", True, prefix, None, policy, code, error_text
            )
    return TransportFacts(
        OUTCOME_UNKNOWN, HELD_UNCERTAIN, "unknown", False, "unclassified", None, policy, code,
        error_text, ("receipt shape not recognised",),
    )


def attempt_record(facts: TransportFacts, *, request_id, event_id, attempt_no, recipient,
                   status_before, observed_at, reconciliation=None) -> dict:
    """The frozen DeliveryAttempt shape. Relay-internal notes never appear here."""
    record = {
        "requestId": request_id,
        "eventId": event_id,
        "attemptNo": attempt_no,
        "recipientTaskId": recipient,
        "deliveryState": facts.delivery_state,
        "sendAttempted": facts.send_attempted,
        "retrySafe": facts.retry_safe,
        "observedAt": observed_at,
        "recipientStatusBefore": status_before,
        "recipientApprovalPolicy": facts.approval_policy,
        "transportReceiptStatus": facts.transport_receipt_status,
        "failedOperation": facts.failed_operation,
        "turnId": facts.turn_id,
    }
    if reconciliation is not None:
        record["reconciliation"] = reconciliation
    assert_attempt_invariants(record)
    return record


def assert_attempt_invariants(record: dict) -> None:
    """Re-implement the frozen schema's conditional rules, in Python, before persisting."""
    state = record["deliveryState"]
    if state == HELD_UNCERTAIN:
        assert record["retrySafe"] is False, "an uncertain attempt is never retry-safe"
        assert record["sendAttempted"] in ("unknown", "yes")
    if record["retrySafe"]:
        assert record["sendAttempted"] == "no"
        assert record["transportReceiptStatus"] == FAILED
        assert state in (WITHHELD_PRE_SEND, DEFERRED_BUSY)
        assert record["failedOperation"] in ("thread/read", "thread/resume")
    if record["transportReceiptStatus"] == UNFINISHED:
        assert record["sendAttempted"] == "unknown"
        assert record["retrySafe"] is False
    if state == DISPATCHED:
        assert isinstance(record["turnId"], str) and record["turnId"]
        assert record["sendAttempted"] == "yes"
        assert record["retrySafe"] is False
    if record["failedOperation"] in ("turn/start", "transport", "initialize", "unclassified"):
        assert record["retrySafe"] is False
        assert record["sendAttempted"] in ("unknown", "yes")
