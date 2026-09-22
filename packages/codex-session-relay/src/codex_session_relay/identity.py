"""Every identifier in the protocol, derived exactly as the frozen contract specifies.

Canonical rendering throughout: fields joined by a pipe, integers unpadded, a null
attempt as the literal string "null", sha256 lowercase hex truncated where stated.
"""

import hashlib
import re

from . import NO_DELIVERABLE

READY_FOR_REVIEW = "ready_for_review"
OUTCOMES = ("ready_for_review", "failed", "interrupted", "blocked_needs_input")

RELATIONSHIP_ID_RE = re.compile(r"^rel-[0-9a-f]{16}$")
EVENT_ID_RE = re.compile(r"^[0-9a-f]{32}$")
REQUEST_ID_RE = re.compile(r"^del-[0-9a-f]{12}-a([0-9]+)$")
# An envelope message id, which is the same width as an event id and is NOT one: it is derived
# from a direction, a relation, a purpose and a subject rather than from a revision. Spelled
# separately so a reader of either regex can see which identity is being checked.
MESSAGE_ID_RE = re.compile(r"^[0-9a-f]{32}$")
SUPERVISOR_REQUEST_ID_RE = re.compile(r"^sup-[0-9a-f]{12}-a([0-9]+)$")
DIGEST_RE = re.compile(r"^[0-9a-f]{64}$")


def sha256_hex(text: str) -> str:
    return hashlib.sha256(text.encode("utf-8")).hexdigest()


def relationship_id(parent_task_id: str, child_task_id: str, issue_key: str) -> str:
    for name, value in (
        ("parent_task_id", parent_task_id),
        ("child_task_id", child_task_id),
        ("issue_key", issue_key),
    ):
        if not isinstance(value, str) or not value.strip():
            raise ValueError(f"{name} must be a non-empty string")
        if "|" in value:
            raise ValueError(f"{name} must not contain '|', which is the field separator")
    return "rel-" + sha256_hex(f"{parent_task_id}|{child_task_id}|{issue_key}")[:16]


def render_attempt(attempt: int | None) -> str:
    """A null attempt renders as the literal 'null'; an integer renders unpadded.

    Written as an explicit None test rather than a falsy test, so a zero attempt would
    render as "0" instead of silently becoming "null". Zero is invalid per the schema,
    but a derivation must not depend on that being enforced elsewhere.
    """
    if attempt is None:
        return "null"
    if not isinstance(attempt, int) or isinstance(attempt, bool):
        raise ValueError("attempt must be an integer or None")
    return str(attempt)


def event_id(
    relationship: str,
    generation: int,
    revision_hash: str,
    outcome: str,
    *,
    turn_id: str | None = None,
    attempt: int | None = None,
) -> str:
    """Split derivation: product-level for a reviewable revision, execution-level otherwise.

    A reviewable revision collapses on re-observation, because two observations of one
    revision are one fact. Two interrupted turns in one generation are two facts, so the
    execution-level form carries the turn and the attempt.
    """
    if outcome not in OUTCOMES:
        raise ValueError(f"unknown outcome {outcome!r}")
    if not isinstance(generation, int) or generation < 1:
        raise ValueError("generation must be a positive integer")
    if outcome == READY_FOR_REVIEW:
        if not DIGEST_RE.match(revision_hash or ""):
            raise ValueError("revision_hash must be 64 lowercase hex characters")
        if revision_hash == NO_DELIVERABLE:
            raise ValueError("a reviewable receipt cannot carry the no-deliverable sentinel")
        payload = f"{relationship}|{generation}|{revision_hash}|{outcome}"
    else:
        if not isinstance(turn_id, str) or not turn_id.strip():
            raise ValueError(f"outcome {outcome!r} needs the turn id it was observed on")
        payload = f"{relationship}|{generation}|{outcome}|{turn_id}|{render_attempt(attempt)}"
    return sha256_hex(payload)[:32]


def request_id(event: str, attempt_no: int) -> str:
    if not EVENT_ID_RE.match(event or ""):
        raise ValueError("event id must be 32 lowercase hex characters")
    if not isinstance(attempt_no, int) or isinstance(attempt_no, bool) or attempt_no < 1:
        raise ValueError("attempt_no must be a positive integer")
    return f"del-{event[:12]}-a{attempt_no}"


def parse_request_id(value: str) -> tuple[str, int]:
    match = REQUEST_ID_RE.match(value or "")
    if not match:
        raise ValueError(f"malformed request id {value!r}")
    return value[4:16], int(match.group(1))


def ack_proof(event: str, ack_turn_id: str) -> str:
    """Computed by the parent over its OWN turn id, which is absent from the message.

    That absence is the whole point: a recipient that merely quotes the delivered fields
    back cannot produce this value, so an echo cannot pass for an acknowledgement.
    """
    if not EVENT_ID_RE.match(event or ""):
        raise ValueError("event id must be 32 lowercase hex characters")
    if not isinstance(ack_turn_id, str) or not ack_turn_id.strip():
        raise ValueError("ack_turn_id must be a non-empty string")
    return sha256_hex(f"{event}|{ack_turn_id}")


def supervisor_request_id(message: str, attempt_no: int) -> str:
    """One transport attempt at one supervisor-bound message.

    Its own prefix rather than del-, because these attempts live in their own table and are
    never claimed by the parent-child engine. A shared spelling would make a request id from
    one queue look like a row the other could settle.
    """
    if not MESSAGE_ID_RE.match(message or ""):
        raise ValueError("message id must be 32 lowercase hex characters")
    if not isinstance(attempt_no, int) or isinstance(attempt_no, bool) or attempt_no < 1:
        raise ValueError("attempt_no must be a positive integer")
    return f"sup-{message[:12]}-a{attempt_no}"


def supervisor_read_proof(message: str, read_turn_id: str) -> str:
    """Computed by the supervisor over its OWN turn id, which the message cannot contain.

    The same construction as ack_proof and for the same reason. The delivered bytes carry the
    message id, because a recipient has to be able to quote it; they cannot carry the turn the
    recipient will read them in, because that turn does not exist until it reads them. So a
    reply that echoes every delivered field still cannot produce this value, and a stored
    dispatch cannot be turned into evidence that anybody read anything.
    """
    if not MESSAGE_ID_RE.match(message or ""):
        raise ValueError("message id must be 32 lowercase hex characters")
    if not isinstance(read_turn_id, str) or not read_turn_id.strip():
        raise ValueError("read_turn_id must be a non-empty string")
    return sha256_hex(f"{message}|{read_turn_id}")


def revision_request_event_id(relationship: str, source_event_id: str, verdict_turn_id: str) -> str:
    """Identity for a parent-to-child revision request.

    Uses the contract's execution-level shape so identity, request ids, deduplication,
    retry, reconciliation and restart recovery are the same code as the completion
    direction. The outcome slot carries a discriminator that is not a contract outcome,
    so a revision id can never collide with a completion id.
    """
    if not isinstance(verdict_turn_id, str) or not verdict_turn_id.strip():
        raise ValueError("verdict_turn_id must be a non-empty string")
    if not isinstance(source_event_id, str) or not source_event_id.strip():
        raise ValueError("source_event_id must be a non-empty string")
    # Keyed on the event being corrected rather than on a generation counter, so replaying one
    # verdict resolves to the same revision and therefore the same generation.
    payload = (
        f"{relationship}|{source_event_id}|needs_changes_revision|{verdict_turn_id}|null"
    )
    return sha256_hex(payload)[:32]
