"""Completion receipts, and telling five different endings apart.

A turn reaching "completed" is the trigger to look, never the answer. Without a receipt
from the child, a completed turn is an ordinary turn end: the task finished a turn, which
is not the same as producing something to review. That distinction is why this module
exists, and it is why a daemon may synthesize only failure and interruption.

Identity is checked against the REGISTRY, not against the payload alone. A receipt that
agrees with its own accompanying observation proves nothing if both name a task nobody
registered or a turn nobody assigned.
"""

import json
from enum import Enum

from . import NO_DELIVERABLE
from .currency import record_lineage
from .errors import ReceiptRefused, RefusalReason, RelayError
from .identity import DIGEST_RE, EVENT_ID_RE, event_id as derive_event_id
from .manifest import Entry, revision_hash, verify_against_disk, verify_frozen
from .models import TurnRef
from .admission import AnchorOrExplicit, ContinuationClaim, record_admission
from .registry import ANCHOR_BOUND
from .scope import PathBindingMode, assert_within, at_least

TERMINAL = ("completed", "failed", "interrupted")
TURN_STATUSES = ("completed", "interrupted", "failed", "inProgress")
CHILD = "child"
DAEMON = "daemon_observation"
READY = "ready_for_review"
OUTCOMES = (READY, "failed", "interrupted", "blocked_needs_input")
PRODUCERS = (CHILD, DAEMON)

# Which host turn status can accompany which asserted outcome. A host that observed a
# failed or interrupted turn contradicts a claim that something is ready to review, and a
# contradiction is refused rather than resolved in the claimant's favour.
COMPATIBLE_TURN_STATUS = {
    # A positive claim may be made from a turn that ended normally, or from the child's own
    # still-running turn. The second case is not a loophole: a child calling the CLI from
    # inside its turn can only ever observe inProgress, and the frozen schema allows it. Such
    # a claim is STAGED and is not deliverable until an independent observation sees that turn
    # end normally. The asymmetry that matters is preserved: an observed failure or
    # interruption can never carry a positive claim, and can never promote a staged one.
    READY: {"completed", "inProgress"},
    "blocked_needs_input": {"completed", "inProgress"},
    # A negative claim may accompany a normally ended turn, because a child that finishes its
    # turn reporting "I could not do this" is telling the truth about its own execution.
    "failed": {"completed", "failed"},
    "interrupted": {"completed", "interrupted"},
}
# A daemon has no claim of its own: it may only restate the terminal status it observed.
DAEMON_TURN_STATUS = {"failed": {"failed"}, "interrupted": {"interrupted"}}

RECEIPT_FIELDS = {
    "eventId", "relationshipId", "executionGeneration", "attempt", "revisionHash",
    "outcome", "producer", "turnRef", "criteria", "emittedAt", "manifest", "manifestRef",
}
REQUIRED_FIELDS = {
    "eventId", "relationshipId", "executionGeneration", "revisionHash", "outcome",
    "producer", "turnRef", "emittedAt",
}
ENTRY_FIELDS = {"path", "sha256", "bytes"}


STAGED = "staged"
FINAL = "final"
SUPPRESSED = "suppressed"


class ObservationOutcome(str, Enum):
    READY_FOR_REVIEW = "ready_for_review"
    FAILED = "failed"
    INTERRUPTED = "interrupted"
    BLOCKED_NEEDS_INPUT = "blocked_needs_input"
    ORDINARY_TURN_END = "ordinary_turn_end"
    IN_PROGRESS = "in_progress"
    CONTRADICTORY = "contradictory"


def classify_observation(turn_status: str, child_receipt: dict | None) -> ObservationOutcome:
    """What a turn status plus an optional child receipt actually means.

    A completed turn with no child receipt is ORDINARY_TURN_END: not success, not failure,
    and never something to verify. A child claiming a reviewable result on a turn the host
    reports as failed or interrupted is CONTRADICTORY, because an observed failure must not
    be promoted by the claim attached to it.
    """
    if turn_status not in TERMINAL:
        return ObservationOutcome.IN_PROGRESS
    if child_receipt is not None:
        if child_receipt.get("producer") != CHILD:
            raise ReceiptRefused(
                RefusalReason.PRODUCER_NOT_PERMITTED,
                "only the child produces an asserted outcome",
            )
        outcome = child_receipt.get("outcome")
        if outcome not in OUTCOMES:
            raise ReceiptRefused(
                RefusalReason.OUTCOME_INCONSISTENT, f"unknown outcome {outcome!r}"
            )
        if turn_status not in COMPATIBLE_TURN_STATUS[outcome]:
            return ObservationOutcome.CONTRADICTORY
        return ObservationOutcome(outcome)
    if turn_status == "failed":
        return ObservationOutcome.FAILED
    if turn_status == "interrupted":
        return ObservationOutcome.INTERRUPTED
    return ObservationOutcome.ORDINARY_TURN_END


class ReceiptIntake:
    def __init__(
        self,
        store,
        registry,
        clock,
        *,
        minimum_path_binding: PathBindingMode = PathBindingMode.BEST_EFFORT_DETECTION,
        admission=None,
    ):
        self.store = store
        self.registry = registry
        self.clock = clock
        self.minimum_path_binding = minimum_path_binding
        # The anchor is always admitted. Any other turn needs an explicit continuation
        # admission, which the child supplies in this same call. Host ordering may corroborate
        # that claim when an adapter is available; it never substitutes for it.
        self.admission = admission or AnchorOrExplicit()

    # ------------------------------------------------------------- refusals

    def record_refusal(self, reason, *, relationship_id=None, event=None, detail="", payload=None):
        now = self.clock.iso()
        with self.store.transaction() as db:
            db.execute(
                "INSERT INTO refusals (at, relationship_id, event_id, reason, detail, payload)"
                " VALUES (?,?,?,?,?,?)",
                (
                    now, relationship_id, event,
                    reason.value if hasattr(reason, "value") else str(reason),
                    detail, json.dumps(payload, default=str) if payload is not None else None,
                ),
            )

    def refusals(self, relationship_id=None):
        if relationship_id:
            return self.store.all(
                "SELECT * FROM refusals WHERE relationship_id = ? ORDER BY id", (relationship_id,)
            )
        return self.store.all("SELECT * FROM refusals ORDER BY id")

    # -------------------------------------------------------------- reading

    def get(self, event: str) -> dict | None:
        row = self.store.one("SELECT * FROM events WHERE event_id = ?", (event,))
        return json.loads(row["receipt"]) if row else None

    def row(self, event: str):
        return self.store.one("SELECT * FROM events WHERE event_id = ?", (event,))

    # -------------------------------------------------------------- writing

    def accept_child_receipt(self, payload, *, observation: TurnRef, continuation=None,
                             supersedes_revision=None) -> dict:
        """Validate a child's asserted outcome and store it, or refuse and record why.

        The observation is required. A receipt is a claim about a specific turn, so there
        has to be an observation of that turn to compare it with.
        """
        rid = payload.get("relationshipId") if isinstance(payload, dict) else None
        event = payload.get("eventId") if isinstance(payload, dict) else None
        try:
            return self._accept(payload, observation, continuation, supersedes_revision)
        except RelayError as error:
            self.record_refusal(
                error.reason, relationship_id=rid, event=event,
                detail=error.detail, payload=payload if isinstance(payload, dict) else str(payload),
            )
            raise

    def _accept(self, payload, observation: TurnRef, continuation=None,
                supersedes_revision=None) -> dict:
        self._validate_shape(payload)
        rid = payload["relationshipId"]
        relationship = self.registry.require_active(rid)

        generation_record = self._check_generation(relationship, payload["executionGeneration"])

        outcome = payload["outcome"]
        producer = payload["producer"]
        if outcome in (READY, "blocked_needs_input") and producer != CHILD:
            raise ReceiptRefused(
                RefusalReason.PRODUCER_NOT_PERMITTED,
                f"only the child may assert {outcome!r}; no terminal turn status identifies it",
            )
        if producer == CHILD:
            attempt = payload.get("attempt")
            if not isinstance(attempt, int) or isinstance(attempt, bool) or attempt < 1:
                raise ReceiptRefused(
                    RefusalReason.OUTCOME_INCONSISTENT,
                    "a child receipt carries its own rerun counter as a positive integer",
                )
        elif payload.get("attempt") is not None:
            raise ReceiptRefused(
                RefusalReason.OUTCOME_INCONSISTENT,
                "a daemon observation cannot supply the child's rerun counter",
            )

        turn = payload["turnRef"]
        if observation is None or turn != observation.to_record():
            raise ReceiptRefused(
                RefusalReason.TURNREF_MISMATCH,
                "turnRef must equal the observation that accompanied this receipt",
            )
        try:
            claim = ContinuationClaim.from_record(continuation)
        except ValueError as error:
            raise ReceiptRefused(RefusalReason.MALFORMED_RECEIPT, str(error)) from error
        self._check_turn_identity(relationship, generation_record, turn, claim)
        allowed = (
            DAEMON_TURN_STATUS[outcome] if producer == DAEMON else COMPATIBLE_TURN_STATUS[outcome]
        )
        if turn["turnStatus"] not in allowed:
            raise ReceiptRefused(
                RefusalReason.CONTRADICTORY_OBSERVATION,
                f"the host observed a {turn['turnStatus']!r} turn, which cannot carry an "
                f"outcome of {outcome!r}",
            )

        manifest_records = payload.get("manifest")
        claimed_digest = payload["revisionHash"]
        binding_mode = None

        # Branch on OUTCOME before producer. The frozen schema only constrains a daemon
        # observation here, but the contract prose binds any execution-only receipt,
        # including one the child wrote, to a null manifest and the sentinel digest.
        if outcome == READY:
            if not isinstance(manifest_records, list) or not manifest_records:
                raise ReceiptRefused(
                    RefusalReason.MANIFEST_REQUIRED,
                    "a reviewable receipt carries the manifest it hashed",
                )
            if claimed_digest == NO_DELIVERABLE:
                raise ReceiptRefused(
                    RefusalReason.OUTCOME_INCONSISTENT,
                    "a reviewable receipt cannot carry the no-deliverable sentinel",
                )
            entries = self._validate_manifest(manifest_records)
            roots = relationship["authorizedScope"]["artifactRoots"]
            for entry in entries:
                assert_within(entry.path, roots)
            binding_mode = self._verify_bytes(entries, roots, payload.get("manifestRef"))
            recomputed = revision_hash(entries)
            if recomputed != claimed_digest:
                raise ReceiptRefused(
                    RefusalReason.REVISION_MISMATCH,
                    f"manifest hashes to {recomputed} but the receipt claims {claimed_digest}",
                )
        else:
            if manifest_records not in (None, []):
                raise ReceiptRefused(
                    RefusalReason.MANIFEST_FORBIDDEN,
                    f"an execution-only receipt ({outcome}) carries no manifest, for any producer",
                )
            if claimed_digest != NO_DELIVERABLE:
                raise ReceiptRefused(
                    RefusalReason.OUTCOME_INCONSISTENT,
                    f"an execution-only receipt ({outcome}) carries the no-deliverable sentinel",
                )

        expected = derive_event_id(
            rid, payload["executionGeneration"], claimed_digest, outcome,
            turn_id=turn["turnId"], attempt=payload.get("attempt"),
        )
        if payload["eventId"] != expected:
            raise ReceiptRefused(
                RefusalReason.EVENT_ID_MISMATCH,
                f"event id should be {expected} for these fields",
            )
        return self._store_event(
            payload, expected, binding_mode, supersedes_revision=supersedes_revision
        )

    # ----------------------------------------------------------- validation

    @staticmethod
    def _validate_shape(payload) -> None:
        """Structure before meaning, so a malformed field becomes a refusal, not a crash."""
        if not isinstance(payload, dict):
            raise ReceiptRefused(RefusalReason.MALFORMED_RECEIPT, "a receipt is a JSON object")
        unknown = set(payload) - RECEIPT_FIELDS
        if unknown:
            raise ReceiptRefused(
                RefusalReason.MALFORMED_RECEIPT, f"unknown fields {sorted(unknown)}"
            )
        missing = REQUIRED_FIELDS - set(payload)
        if missing:
            raise ReceiptRefused(
                RefusalReason.MALFORMED_RECEIPT, f"missing fields {sorted(missing)}"
            )
        if not EVENT_ID_RE.match(str(payload["eventId"])):
            raise ReceiptRefused(
                RefusalReason.MALFORMED_RECEIPT, "eventId must be 32 lowercase hex characters"
            )
        if not isinstance(payload["relationshipId"], str) or not payload["relationshipId"]:
            raise ReceiptRefused(RefusalReason.MALFORMED_RECEIPT, "relationshipId must be a string")
        generation = payload["executionGeneration"]
        if not isinstance(generation, int) or isinstance(generation, bool) or generation < 1:
            raise ReceiptRefused(
                RefusalReason.MALFORMED_RECEIPT, "executionGeneration must be a positive integer"
            )
        if not DIGEST_RE.match(str(payload["revisionHash"])):
            raise ReceiptRefused(
                RefusalReason.MALFORMED_RECEIPT, "revisionHash must be 64 lowercase hex characters"
            )
        if payload["outcome"] not in OUTCOMES:
            raise ReceiptRefused(
                RefusalReason.MALFORMED_RECEIPT, f"unknown outcome {payload['outcome']!r}"
            )
        if payload["producer"] not in PRODUCERS:
            raise ReceiptRefused(
                RefusalReason.MALFORMED_RECEIPT, f"unknown producer {payload['producer']!r}"
            )
        if not isinstance(payload["emittedAt"], str) or not payload["emittedAt"].strip():
            raise ReceiptRefused(RefusalReason.MALFORMED_RECEIPT, "emittedAt must be a timestamp")
        attempt = payload.get("attempt")
        if attempt is not None and (
            not isinstance(attempt, int) or isinstance(attempt, bool) or attempt < 1
        ):
            raise ReceiptRefused(
                RefusalReason.MALFORMED_RECEIPT, "attempt must be a positive integer or null"
            )
        reference = payload.get("manifestRef")
        if reference is not None and (not isinstance(reference, str) or not reference.strip()):
            raise ReceiptRefused(RefusalReason.MALFORMED_RECEIPT, "manifestRef must be a path")
        turn = payload["turnRef"]
        if not isinstance(turn, dict):
            raise ReceiptRefused(RefusalReason.MALFORMED_RECEIPT, "turnRef must be an object")
        if set(turn) != {"threadId", "turnId", "turnStatus"}:
            raise ReceiptRefused(
                RefusalReason.MALFORMED_RECEIPT, f"turnRef has fields {sorted(turn)}"
            )
        for field in ("threadId", "turnId"):
            if not isinstance(turn[field], str) or not turn[field].strip():
                raise ReceiptRefused(
                    RefusalReason.MALFORMED_RECEIPT, f"turnRef.{field} must be a non-empty string"
                )
        if turn["turnStatus"] not in TURN_STATUSES:
            raise ReceiptRefused(
                RefusalReason.MALFORMED_RECEIPT, f"unknown turnStatus {turn['turnStatus']!r}"
            )

    @staticmethod
    def _validate_manifest(records) -> list:
        entries = []
        for index, record in enumerate(records):
            if not isinstance(record, dict):
                raise ReceiptRefused(
                    RefusalReason.MALFORMED_RECEIPT, f"manifest[{index}] must be an object"
                )
            unknown = set(record) - ENTRY_FIELDS
            if unknown:
                raise ReceiptRefused(
                    RefusalReason.MALFORMED_RECEIPT,
                    f"manifest[{index}] has unknown fields {sorted(unknown)}",
                )
            missing = {"path", "sha256"} - set(record)
            if missing:
                raise ReceiptRefused(
                    RefusalReason.MALFORMED_RECEIPT,
                    f"manifest[{index}] is missing {sorted(missing)}",
                )
            if not isinstance(record["path"], str) or not record["path"].startswith("/"):
                raise ReceiptRefused(
                    RefusalReason.MALFORMED_RECEIPT,
                    f"manifest[{index}].path must be an absolute path",
                )
            if not DIGEST_RE.match(str(record["sha256"])):
                raise ReceiptRefused(
                    RefusalReason.MALFORMED_RECEIPT,
                    f"manifest[{index}].sha256 must be 64 lowercase hex characters",
                )
            size = record.get("bytes")
            if size is not None and (
                not isinstance(size, int) or isinstance(size, bool) or size < 0
            ):
                raise ReceiptRefused(
                    RefusalReason.MALFORMED_RECEIPT,
                    f"manifest[{index}].bytes must be a non-negative integer or null",
                )
            entries.append(Entry.from_record(record))
        return entries

    def _check_generation(self, relationship: dict, generation) -> dict:
        known = {g["executionGeneration"]: g for g in relationship["generations"]}
        if generation not in known:
            raise ReceiptRefused(
                RefusalReason.UNKNOWN_GENERATION,
                f"generation {generation!r} was never opened on this relationship",
            )
        if generation < relationship["executionGeneration"]:
            raise ReceiptRefused(
                RefusalReason.STALE_GENERATION,
                f"generation {generation} is older than the current "
                f"{relationship['executionGeneration']}",
            )
        if known[generation]["anchorState"] != ANCHOR_BOUND:
            raise ReceiptRefused(
                RefusalReason.UNBOUND_GENERATION,
                f"generation {generation} has no bound anchor; it stays pending and reportable "
                "until an exact dispatch turn id is supplied",
            )
        return known[generation]

    def _check_turn_identity(self, relationship, generation_record, turn, claim=None) -> str:
        """The turn has to belong to the registered child and to this execution.

        Payload and observation agreeing with each other proves only that a claimant is
        self-consistent, so the registry decides both questions. The thread check is absolute.
        The turn check asks the admission strategy, because an execution starts at its anchor
        but does not end there: a loop spans later turns on the same task, and refusing those
        would admit only a first-turn completion.
        """
        expected_thread = relationship["child"]["taskId"]
        if turn["threadId"] != expected_thread:
            raise ReceiptRefused(
                RefusalReason.UNASSIGNED_TURN,
                f"turnRef names thread {turn['threadId']!r}, but the registered child of this "
                f"relationship is {expected_thread!r}",
            )
        admission = self.admission.admit(
            self.store, relationship, generation_record, turn["turnId"], claim
        )
        if not admission.admitted:
            raise ReceiptRefused(
                RefusalReason.UNASSIGNED_TURN,
                f"turn {turn['turnId']!r} is not admitted to generation "
                f"{generation_record['executionGeneration']} (anchor "
                f"{generation_record['dispatchTurnId']!r}): {admission.detail}",
            )
        record_admission(
            self.store, self.clock, relationship["relationshipId"],
            generation_record["executionGeneration"], turn["turnId"], admission,
        )
        return admission.evidence

    def _verify_bytes(self, entries, roots, manifest_ref) -> PathBindingMode:
        # Asking for a lease costs a concurrent writer real time, so it is requested only
        # when this store actually requires the stronger tier.
        want_lease = self.minimum_path_binding is PathBindingMode.LEASE_ENFORCED
        problems, bindings = verify_against_disk(entries, roots, allow_lease=want_lease)
        if problems and manifest_ref:
            _frozen_digest, frozen_problems = verify_frozen(manifest_ref, entries)
            if not frozen_problems:
                # The live files moved on; the frozen copy is what this receipt is about.
                # A frozen copy establishes no live path binding, so it can only ever meet
                # the best-effort tier, and it is checked against the minimum like any other.
                return self._require_minimum(PathBindingMode.BEST_EFFORT_DETECTION)
            problems = problems + frozen_problems
        if problems:
            raise ReceiptRefused(RefusalReason.MANIFEST_UNVERIFIED, "; ".join(problems[:5]))
        mode = PathBindingMode.LEASE_ENFORCED
        for binding in bindings.values():
            if not at_least(binding.mode, mode):
                mode = binding.mode
        return self._require_minimum(mode)

    def _require_minimum(self, mode: PathBindingMode) -> PathBindingMode:
        if not at_least(mode, self.minimum_path_binding):
            raise ReceiptRefused(
                RefusalReason.INSUFFICIENT_PATH_BINDING,
                f"artifacts were read at {mode.value!r} but this store requires "
                f"{self.minimum_path_binding.value!r}",
            )
        return mode

    # ----------------------------------------------------------- persistence

    def _store_event(self, payload: dict, event: str, binding_mode,
                     supersedes_revision=None) -> dict:
        now = self.clock.iso()
        existing = self.store.one("SELECT * FROM events WHERE event_id = ?", (event,))
        if existing is not None:
            # Re-observing one revision is one fact seen twice, not a second verification.
            with self.store.transaction() as db:
                db.execute(
                    "UPDATE events SET observation_count = observation_count + 1, last_seen_at = ?"
                    " WHERE event_id = ?",
                    (now, event),
                )
                self.store.journal("event_reobserved", event, at=now)
            stored = json.loads(existing["receipt"])
            stored["_duplicate"] = True
            return stored
        record = dict(payload)
        record["eventId"] = event
        record.setdefault("emittedAt", now)
        turn = record["turnRef"]
        stage = STAGED if turn["turnStatus"] == "inProgress" else FINAL
        with self.store.transaction() as db:
            db.execute(
                "INSERT INTO events (event_id, relationship_id, execution_generation,"
                " revision_hash, outcome, producer, attempt, turn_thread_id, turn_id, turn_status,"
                " receipt, manifest_ref, path_binding_mode, stage, staged_at, first_seen_at,"
                " last_seen_at, observation_count) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,1)",
                (
                    event, record["relationshipId"], record["executionGeneration"],
                    record["revisionHash"], record["outcome"], record["producer"],
                    record.get("attempt"), turn["threadId"], turn["turnId"], turn["turnStatus"],
                    json.dumps(record), record.get("manifestRef"),
                    binding_mode.value if binding_mode else None,
                    stage, now if stage == STAGED else None, now, now,
                ),
            )
            self.store.journal(
                "event_accepted", event, {"outcome": record["outcome"], "stage": stage}, at=now
            )
            if record["outcome"] == READY:
                # Atomic with the receipt on purpose. A revision stored without its declaration
                # reads as undeclared, and two undeclared revisions in one generation are a
                # fork, so losing the declaration would quietly withhold a completion that
                # should have been possible.
                record_lineage(
                    db, self.clock,
                    relationship_id=record["relationshipId"],
                    generation=record["executionGeneration"],
                    event_id=event,
                    revision_hash=record["revisionHash"],
                    supersedes_hash=supersedes_revision,
                )
        result = dict(record)
        result["_duplicate"] = False
        result["_pathBindingMode"] = binding_mode.value if binding_mode else None
        result["_stage"] = stage
        return result

    # ------------------------------------------------------------- staging

    def staged_events(self, *, thread_id=None, turn_id=None, relationship_id=None):
        sql = "SELECT * FROM events WHERE stage = ?"
        params = [STAGED]
        if thread_id is not None:
            sql += " AND turn_thread_id = ?"
            params.append(thread_id)
        if turn_id is not None:
            sql += " AND turn_id = ?"
            params.append(turn_id)
        if relationship_id is not None:
            # A child thread can serve several assignments, so a staged claim on one of its
            # turns belongs to exactly one of them. Asking by thread alone hands another
            # assignment's work to whoever polls first.
            sql += " AND relationship_id = ?"
            params.append(relationship_id)
        return self.store.all(sql + " ORDER BY first_seen_at", tuple(params))

    def resolve_staged(self, turn: TurnRef) -> dict:
        """Settle every staged claim on a turn that has now reached a terminal state.

        A normal ending finalizes the claim, which is the only way a claim emitted from a
        live turn becomes deliverable. A failed or interrupted ending suppresses it. Nothing
        here can turn an interrupted execution into something ready for review, and because
        a reviewable event id does not include the turn, finalizing does not mint a second
        event: the same one becomes deliverable exactly once.
        """
        if turn.turn_status not in TERMINAL:
            return {"finalized": [], "suppressed": [], "pending": True}
        rows = self.staged_events(thread_id=turn.thread_id, turn_id=turn.turn_id)
        if not rows:
            return {"finalized": [], "suppressed": [], "pending": False}
        with self.store.transaction() as db:
            return self.resolve_staged_in(db, turn)

    def resolve_staged_in(self, db, turn: TurnRef, relationship_id=None) -> dict:
        """The same settlement inside a caller's transaction.

        Exists so that finalizing a claim, recording the observation that finalized it, and
        queuing what it produced can be ONE commit. Split across three, a failure in the third
        leaves a final event nobody will ever look at again.

        Scoped to one assignment when the caller names it. A turn belonging to a shared child
        can carry claims from several assignments, and settling all of them on behalf of
        whichever one happened to poll first suppressed the others without ever synthesizing
        their receipts - so their parents waited on an outcome that had already been thrown
        away. Each assignment settles its own.
        """
        if turn.turn_status not in TERMINAL:
            return {"finalized": [], "suppressed": [], "pending": True}
        finalized, suppressed = [], []
        now = self.clock.iso()
        rows = self.staged_events(
            thread_id=turn.thread_id, turn_id=turn.turn_id, relationship_id=relationship_id,
        )
        if not rows:
            return {"finalized": [], "suppressed": [], "pending": False}
        if True:
            for row in rows:
                if turn.turn_status == "completed":
                    db.execute(
                        "UPDATE events SET stage = ?, finalized_at = ?, finalizing_status = ?"
                        " WHERE event_id = ?",
                        (FINAL, now, turn.turn_status, row["event_id"]),
                    )
                    finalized.append(row["event_id"])
                else:
                    db.execute(
                        "UPDATE events SET stage = ?, finalized_at = ?, finalizing_status = ?,"
                        " suppressed_reason = ? WHERE event_id = ?",
                        (
                            SUPPRESSED, now, turn.turn_status,
                            f"the turn ended {turn.turn_status}, so the staged claim is not "
                            "promoted",
                            row["event_id"],
                        ),
                    )
                    suppressed.append(row["event_id"])
            self.store.journal(
                "staged_resolved", turn.turn_id,
                {"finalized": finalized, "suppressed": suppressed, "status": turn.turn_status},
                at=now,
            )
        return {"finalized": finalized, "suppressed": suppressed, "pending": False}

    def deliverable(self, event: str) -> bool:
        """Only a final event may be handed to a parent."""
        row = self.row(event)
        return bool(row) and row["stage"] == FINAL

    def daemon_observation(self, relationship_id: str, turn: TurnRef) -> dict:
        """Synthesize a receipt from an observed terminal turn.

        Only failure and interruption. A daemon cannot know that a completed turn produced
        something reviewable, and it cannot know that a task is waiting for an approval, so
        it is not allowed to say either.
        """
        if turn.turn_status not in ("failed", "interrupted"):
            raise ReceiptRefused(
                RefusalReason.PRODUCER_NOT_PERMITTED,
                f"a daemon observation cannot assert anything from a {turn.turn_status!r} turn",
            )
        relationship = self.registry.require_active(relationship_id)
        generation = relationship["executionGeneration"]
        generation_record = self._check_generation(relationship, generation)
        self._check_turn_identity(relationship, generation_record, turn.to_record())
        payload = {
            "relationshipId": relationship_id,
            "executionGeneration": generation,
            "attempt": None,
            "revisionHash": NO_DELIVERABLE,
            "outcome": turn.turn_status,
            "producer": DAEMON,
            "turnRef": turn.to_record(),
            "manifest": None,
            "emittedAt": self.clock.iso(),
        }
        payload["eventId"] = derive_event_id(
            relationship_id, generation, NO_DELIVERABLE, turn.turn_status,
            turn_id=turn.turn_id, attempt=None,
        )
        return self._store_event(payload, payload["eventId"], None)

    def record_observation(self, turn: TurnRef, classification, *, relationship_id=None, event=None):
        """The daemon's own key, (thread, turn, terminal status), deduplicating its stream."""
        with self.store.transaction() as db:
            self.record_observation_in(
                db, turn, classification, relationship_id=relationship_id, event=event,
            )

    def record_observation_in(self, db, turn: TurnRef, classification, *, relationship_id=None,
                              event=None):
        now = self.clock.iso()
        db.execute(
            "INSERT OR IGNORE INTO observations (thread_id, turn_id, terminal_status,"
            " relationship_id, classification, event_id, observed_at) VALUES (?,?,?,?,?,?,?)",
            (
                turn.thread_id, turn.turn_id, turn.turn_status, relationship_id,
                classification.value if hasattr(classification, "value") else str(classification),
                event, now,
            ),
        )
        if relationship_id is not None:
            # observations is keyed by the turn alone, so the row above belongs to whichever
            # assignment settled it first. This is the per-assignment fact, and it is what
            # the scheduler and the health block ask.
            db.execute(
                "INSERT OR IGNORE INTO assignment_settlements (relationship_id, thread_id,"
                " turn_id, terminal_status, settled_at) VALUES (?,?,?,?,?)",
                (relationship_id, turn.thread_id, turn.turn_id, turn.turn_status, now),
            )


def contract_record(receipt: dict) -> dict:
    """The receipt as the frozen schema defines it, without internal annotations."""
    clean = {k: v for k, v in receipt.items() if not k.startswith("_")}
    if clean.get("manifestRef") is None:
        clean.pop("manifestRef", None)
    if clean.get("criteria") is None:
        clean.pop("criteria", None)
    return clean
