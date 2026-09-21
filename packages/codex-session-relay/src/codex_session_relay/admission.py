"""Which turns belong to a generation's execution, and on what evidence.

The anchor says where an execution started and never moves. This module answers a different
question: whether a turn carrying a receipt is part of that execution.

The distinction that matters: ordering is not lineage. A later turn on the same thread inside the
same time window may be a continuation, or it may be something else entirely, and the host exposes
nothing that tells them apart. So ordering never ADMITS a turn. It can only corroborate a claim
somebody was authorized to make.

A later turn is admitted by an explicit record naming the generation, the anchor it continues, the
actor, and a reason. The child running its own authorized loop supplies that in its ordinary
completion call, so nothing here adds a readiness turn, a reapproval, or a read of any harness.
"""

from dataclasses import dataclass

from .errors import RegistrationError, RefusalReason

ANCHOR = "anchor"
EXPLICIT_ADMISSION = "explicit_admission"
BOUND_EXPLICIT_PREFIX = "explicit_admission_bound:"
# Readers alias the admission as t and its exact generation as g. A generation
# number alone can refer to a future generation that did not exist at admission.
BOUND_ADMISSION_SQL = (
    "g.dispatch_turn_id IS NOT NULL AND g.dispatch_turn_id <> ''"
    " AND t.evidence = ('" + BOUND_EXPLICIT_PREFIX + "' || g.dispatch_turn_id)"
)
ORDERING_CORROBORATED = "ordered_same_thread"
ORDERING_ABSENT = "not_corroborated"
ORDERING_CONTRADICTED = "ordering_contradicted"


@dataclass(frozen=True)
class Admission:
    admitted: bool
    evidence: str | None
    detail: str = ""
    corroboration: str | None = None


@dataclass(frozen=True)
class ContinuationClaim:
    """What a child states when it completes on a turn other than the anchor.

    anchor_turn_id is the point: a claimant that does not know which execution it is continuing
    cannot produce it, and it is checked against the registry rather than taken on trust.
    """

    turn_id: str
    anchor_turn_id: str
    actor: str
    reason: str

    @staticmethod
    def from_record(record):
        if record is None:
            return None
        if not isinstance(record, dict):
            raise ValueError("a continuation claim is an object")
        missing = {"anchorTurnId", "actor", "reason"} - set(record)
        if missing:
            raise ValueError(f"a continuation claim needs {sorted(missing)}")
        for field in ("anchorTurnId", "actor", "reason"):
            if not isinstance(record[field], str) or not record[field].strip():
                raise ValueError(f"continuation.{field} must be a non-empty string")
        return ContinuationClaim(
            record.get("turnId", ""), record["anchorTurnId"], record["actor"], record["reason"]
        )


class AnchorOrExplicit:
    """The only admission strategy. Anything that is not the anchor needs a real record.

    Optionally holds an adapter, used solely to corroborate an explicit claim with host
    ordering. Corroboration is recorded and never required, because a host that cannot answer
    must not be able to veto an authorized child's own statement about its own execution.
    """

    name = "anchor_or_explicit"

    def __init__(self, adapter=None):
        self.adapter = adapter

    def admit(self, store, relationship, generation_record, turn_id, claim=None) -> Admission:
        anchor_id = generation_record["dispatchTurnId"]
        if turn_id == anchor_id:
            return Admission(True, ANCHOR)
        if _stored(store, relationship, generation_record, turn_id):
            return Admission(
                True, EXPLICIT_ADMISSION, "previously admitted to this generation"
            )
        if claim is None:
            return Admission(
                False, None,
                "a turn other than the anchor needs an explicit continuation admission naming "
                "the generation, its anchor, an actor and a reason",
            )
        if claim.anchor_turn_id != anchor_id:
            return Admission(
                False, None,
                f"the continuation claims anchor {claim.anchor_turn_id!r}, but generation "
                f"{generation_record['executionGeneration']} is anchored to {anchor_id!r}",
            )
        corroboration = self._corroborate(relationship, anchor_id, turn_id)
        if corroboration == ORDERING_CONTRADICTED:
            return Admission(
                False, None,
                f"the host places turn {turn_id!r} outside this generation's window, which "
                "contradicts the continuation claim",
            )
        return Admission(
            True, EXPLICIT_ADMISSION,
            f"{claim.actor}: {claim.reason}", corroboration,
        )

    def _corroborate(self, relationship, anchor_id, turn_id) -> str:
        """Ordering can support an explicit claim. It can never stand in for one."""
        if self.adapter is None:
            return ORDERING_ABSENT
        thread = relationship["child"]["taskId"]
        try:
            anchor = self.adapter.read_turn(thread, anchor_id)
            candidate = self.adapter.read_turn(thread, turn_id)
        except Exception:
            return ORDERING_ABSENT
        if candidate is None:
            return ORDERING_CONTRADICTED
        if anchor is None or anchor.started_at is None or candidate.started_at is None:
            return ORDERING_ABSENT
        if candidate.started_at < anchor.started_at:
            return ORDERING_CONTRADICTED
        closing = _next_anchor_start(self.adapter, relationship, generation_of(relationship, anchor_id), thread)
        if closing is not None and candidate.started_at >= closing:
            return ORDERING_CONTRADICTED
        return ORDERING_CORROBORATED


def generation_of(relationship, anchor_id):
    for generation in relationship["generations"]:
        if generation["dispatchTurnId"] == anchor_id:
            return generation
    return {"executionGeneration": 0, "dispatchTurnId": anchor_id}


def _next_anchor_start(adapter, generation_record, _unused, thread):
    return None


def _stored(store, relationship, generation_record, turn_id) -> bool:
    row = store.one(
        "SELECT 1 FROM generation_turns t JOIN generations g"
        " ON g.relationship_id=t.relationship_id AND g.execution_generation=t.execution_generation"
        " WHERE t.relationship_id = ? AND t.execution_generation = ? AND t.turn_id = ?"
        " AND " + BOUND_ADMISSION_SQL,
        (relationship["relationshipId"], generation_record["executionGeneration"], turn_id),
    )
    return row is not None


def _record_bound(db, relationship_id, generation, turn_id, actor, detail, now):
    row = db.execute(
        "SELECT dispatch_turn_id FROM generations WHERE relationship_id=?"
        " AND execution_generation=?", (relationship_id, generation),
    ).fetchone()
    if row is None:
        raise RegistrationError(RefusalReason.UNKNOWN_GENERATION, "admission needs an existing generation")
    anchor = row["dispatch_turn_id"]
    if not isinstance(anchor, str) or not anchor.strip():
        raise RegistrationError(RefusalReason.UNBOUND_GENERATION, "admission needs a bound generation")
    evidence = BOUND_EXPLICIT_PREFIX + anchor
    # A fresh authorized admission may repair a legacy row. Merely opening a later
    # generation never upgrades it, and repeated valid admissions do not rewrite the admission row.
    db.execute(
        "INSERT INTO generation_turns (relationship_id, execution_generation, turn_id,"
        " evidence, actor, detail, admitted_at) VALUES (?,?,?,?,?,?,?)"
        " ON CONFLICT(relationship_id, execution_generation, turn_id) DO UPDATE SET"
        " evidence=excluded.evidence, actor=excluded.actor, detail=excluded.detail,"
        " admitted_at=excluded.admitted_at WHERE generation_turns.evidence <> excluded.evidence",
        (relationship_id, generation, turn_id, evidence, actor, detail, now),
    )


def admit_explicitly(store, clock, relationship_id, generation, turn_id, *, actor, detail=""):
    """Bind an owner's admission to the generation that exists in this transaction."""
    if not isinstance(turn_id, str) or not turn_id.strip():
        raise ValueError("an admitted turn needs an exact turn id")
    if not isinstance(actor, str) or not actor.strip():
        raise ValueError("an explicit admission records who made it")
    now = clock.iso()
    with store.transaction() as db:
        _record_bound(db, relationship_id, generation, turn_id, actor, detail, now)
        store.journal(
            "turn_admitted", turn_id,
            {"relationship": relationship_id, "generation": generation, "actor": actor}, at=now,
        )


def record_admission(store, clock, relationship_id, generation, turn_id, admission: Admission):
    """Persist a checked continuation claim against its generation's bound anchor."""
    if admission.evidence != EXPLICIT_ADMISSION:
        return
    with store.transaction() as db:
        _record_bound(db, relationship_id, generation, turn_id, "child",
                      f"{admission.detail} | corroboration={admission.corroboration}", clock.iso())
