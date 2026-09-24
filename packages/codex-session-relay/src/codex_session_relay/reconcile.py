"""Finding out what actually happened to an uncertain send.

The order is fixed and the evidence list is closed. Re-read the operation receipt for the SAME
request id first, then read the recipient's real items. Exactly three things are affirmative
evidence: the receipt now carries a turn id, a turn carrying this attempt's token exists in the
recipient's items, or a confirmed pre-send rejection.

Elapsed time is not on that list, and there is no function here that takes a duration. An
attempt with no affirmative evidence stays held and says precisely what it is missing.

A receipt carrying a turn id is followed by one more read: the recipient's own turns, for that
turn (hostloss.py, CRW-224). The receipt proves turn/start was answered, not that the host kept
the turn. What the read finds is reported as recipientTurn; a completion whose turn the host lost
is recorded host_lost_turn and queued once more. That is not evidence that a send landed, so it
is not a fourth item on the list above: it is the host's own answer that the accepted turn is
gone.
"""

import json
from enum import Enum

from . import hostloss
from .delivery import COMPLETION, REVISION, SENDING
from .policy import HOST_LOST_TURN, TURN_CHECK_UNDECIDED
from .transport import (
    DEFERRED_BUSY,
    DISPATCHED,
    HELD_UNCERTAIN,
    UNFINISHED,
    WITHHELD_PRE_SEND,
    attempt_record,
    classify_operation_receipt,
)

SCAN_LIMIT = 200


def _with_anchor(outcome: dict, anchor) -> dict:
    """Carry the binding outcome out with the settlement, so a conflict is not only journalled.

    Absent when there was nothing to bind, which is every completion and every settlement that
    promoted nothing.
    """
    if anchor is not None:
        outcome["anchor"] = anchor
    return outcome


class Evidence(str, Enum):
    TURN_FOUND = "turn_found"
    RECEIPT_TURN_ID = "receipt_turn_id"
    CONFIRMED_PRE_SEND_REJECTION = "confirmed_pre_send_rejection"
    NONE = "none"


class _AttemptLost(Exception):
    """The host-loss check recorded this attempt after reconciliation read it (PR #156 review).

    Reconciliation reads first and writes later, outside one transaction; a daemon pass can
    record the loss in between. Writing the receipt's settlement then would set the attempt back
    to dispatched and erase the count that stops a third send, so the write refuses instead.
    """


class Reconciler:
    def __init__(self, store, registry, delivery, clock, *, policy=None):
        self.store = store
        self.registry = registry
        self.delivery = delivery
        self.clock = clock
        self.policy = policy or delivery.policy

    UNRESOLVED = (
        " WHERE (a.internal_state = 'in_flight'"
        "        OR (a.state = ? AND d.state IN (?, ?)))"
    )

    def open_attempts(self, *, limit=None, parents=None, offset=0) -> list:
        """Everything a restart has to look at: in-flight sends and unresolved attempts.

        With no arguments this stays exhaustive, because recover_on_start has to see all of
        it. The bounded, parent-filtered form is what a tick uses, so one parent's backlog of
        unchanged attempts cannot hide another parent's actionable one. Deliberately NOT
        filtered on active status: an unresolved send belonging to a cancelled assignment
        still needs its evidence settled.

        offset is what keeps the bounded form from being a fixed prefix. Attempts whose
        fingerprint has not changed are skipped by the caller's gate but still occupy their
        place, so without it a parent with more unresolved attempts than its share would
        re-read the same leading ones on every tick and never reach the rest.
        """
        sql = (
            "SELECT a.*, r.parent_task_id AS parent_task_id FROM attempts a"
            " JOIN deliveries d ON d.event_id = a.event_id"
            " JOIN relationships r ON r.relationship_id = d.relationship_id"
            + self.UNRESOLVED
        )
        params = [HELD_UNCERTAIN, HELD_UNCERTAIN, SENDING]
        if parents:
            sql += " AND r.parent_task_id IN (" + ",".join("?" * len(parents)) + ")"
            params.extend(parents)
        sql += " ORDER BY a.observed_at"
        if limit is not None:
            sql += " LIMIT ?"
            params.append(limit)
            if offset:
                sql += " OFFSET ?"
                params.append(offset)
        return self.store.all(sql, tuple(params))

    def open_attempt_count(self, parent) -> int:
        """How many unresolved attempts one parent has, so a cursor over them can wrap."""
        row = self.store.one(
            "SELECT COUNT(*) AS c FROM attempts a"
            " JOIN deliveries d ON d.event_id = a.event_id"
            " JOIN relationships r ON r.relationship_id = d.relationship_id"
            + self.UNRESOLVED +
            " AND r.parent_task_id = ?",
            (HELD_UNCERTAIN, HELD_UNCERTAIN, SENDING, parent),
        )
        return row["c"] if row else 0

    def open_parents(self) -> list:
        """Which parents have unresolved work, independent of how much each of them has."""
        rows = self.store.all(
            "SELECT DISTINCT r.parent_task_id AS parent_task_id FROM attempts a"
            " JOIN deliveries d ON d.event_id = a.event_id"
            " JOIN relationships r ON r.relationship_id = d.relationship_id"
            + self.UNRESOLVED +
            " ORDER BY r.parent_task_id",
            (HELD_UNCERTAIN, HELD_UNCERTAIN, SENDING),
        )
        return [row["parent_task_id"] for row in rows]

    def reconcile_attempt(self, request_id: str, adapter, *, now=None) -> dict:
        try:
            return self._reconcile_attempt(request_id, adapter, now=now)
        except _AttemptLost:
            return self._recorded_loss(request_id)

    def _recorded_loss(self, request_id: str, reading=None) -> dict:
        """What a loss already recorded on this attempt says, without writing anything.

        redelivery is what the recording journalled for this attempt - queued or held - and the
        reading, when the caller took one, is reported beside it.
        """
        attempt = self.store.one("SELECT * FROM attempts WHERE request_id = ?", (request_id,))
        redelivery = None
        for row in self.store.all(
            "SELECT detail FROM journal WHERE kind = ? AND subject = ? ORDER BY seq DESC",
            (HOST_LOST_TURN, attempt["event_id"]),
        ):
            detail = json.loads(row["detail"])
            if detail.get("requestId") == request_id:
                redelivery = detail.get("redelivery")
                break
        outcome = {
            "evidence": Evidence.RECEIPT_TURN_ID.value, "state": HOST_LOST_TURN,
            "redelivery": redelivery, "record": json.loads(attempt["record"]),
            "detail": "the host lost this attempt's turn and that is already recorded; nothing"
                      " further was written",
        }
        if reading is not None:
            outcome["recipientTurn"] = reading
        return outcome

    def _reconcile_attempt(self, request_id: str, adapter, *, now=None) -> dict:
        now = self.clock.now() if now is None else now
        attempt = self.store.one("SELECT * FROM attempts WHERE request_id = ?", (request_id,))
        if attempt is None:
            raise KeyError(request_id)
        delivery = self.delivery.get(attempt["event_id"])

        # Step one, always: the operation receipt for this exact request id.
        operation_observation = "missing"
        receipt = None
        try:
            receipt = adapter.get_operation(request_id)
        except Exception as error:
            operation_observation = f"unreadable: {type(error).__name__}: {error}"
        if receipt is not None:
            facts = classify_operation_receipt(receipt)
            operation_observation = f"{facts.transport_receipt_status}:{facts.delivery_state}"
            if facts.delivery_state == DISPATCHED:
                return self._settle_dispatched(
                    attempt, delivery, facts, operation_observation, adapter, now
                )
            if facts.retry_safe:
                return self._settle_from_receipt(
                    attempt, delivery, facts, Evidence.CONFIRMED_PRE_SEND_REJECTION,
                    operation_observation, now,
                )
            if facts.transport_receipt_status != UNFINISHED:
                operation_observation += " (not affirmative)"

        # Step two, only now: the recipient's real items.
        scan_detail = "not scanned"
        try:
            scan = adapter.find_token(
                delivery["recipient_thread_id"], request_id, limit=SCAN_LIMIT
            )
            scan_detail = (
                f"found={scan.found} exhausted={scan.exhausted} scanned={scan.scanned}"
            )
            if scan.found:
                return self._settle_from_scan(
                    attempt, delivery, scan, operation_observation, scan_detail, now
                )
        except Exception as error:
            scan_detail = f"unreadable: {type(error).__name__}: {error}"

        # A bounded scan that did not exhaust the history has not shown absence, and even an
        # exhausted one is not affirmative evidence of non-delivery. Either way: stay held.
        return self._stay_held(attempt, delivery, operation_observation, scan_detail, now)

    def _settle_dispatched(self, attempt, delivery, facts, observation, adapter, now) -> dict:
        """A receipt with a turn id, then the recipient's own turns for that turn (CRW-224).

        The read comes first and outside any transaction, as every host read here does. An
        attempt already recorded as lost - before this call or while it read - is reported and
        left alone: _write refuses to settle it from its receipt again, which would put it back
        to dispatched and erase the one count that stops a third send.
        """
        reading = hostloss.read_recipient_turn(
            adapter, self.clock, attempt, delivery, facts.turn_id
        )
        try:
            outcome = self._settle_from_receipt(
                attempt, delivery, facts, Evidence.RECEIPT_TURN_ID, observation, now,
                turns_checked=reading["finding"] != hostloss.UNKNOWN,
            )
        except _AttemptLost:
            # Recorded already, or by the daemon between this read and this write.
            return self._recorded_loss(attempt["request_id"], reading)
        outcome["recipientTurn"] = reading
        if reading["finding"] == HOST_LOST_TURN:
            if delivery["kind"] == COMPLETION:
                outcome.update(hostloss.settle(
                    self.store, self.clock, attempt["request_id"], reading,
                    observation=observation,
                ))
            else:
                outcome["redelivery"] = hostloss.REPORT_ONLY
        elif delivery["kind"] == COMPLETION:
            hostloss.record_undecided(self.store, attempt["request_id"], reading)
        return outcome

    def check_dispatched_turn(self, request_id: str, adapter) -> dict:
        """The daemon's question for one delivered completion: does the host still have its turn?

        Writes nothing unless the answer is that the host lost it.
        """
        attempt = self.store.one("SELECT * FROM attempts WHERE request_id = ?", (request_id,))
        if attempt is None:
            raise KeyError(request_id)
        delivery = self.delivery.get(attempt["event_id"])
        record = json.loads(attempt["record"]) if attempt["record"] else {}
        turn_id = record.get("turnId") or delivery["dispatch_turn_id"]
        reading = hostloss.read_recipient_turn(adapter, self.clock, attempt, delivery, turn_id)
        outcome = {"eventId": attempt["event_id"], "requestId": request_id,
                   "state": attempt["state"], "recipientTurn": reading}
        if reading["finding"] == HOST_LOST_TURN:
            outcome.update(hostloss.settle(self.store, self.clock, request_id, reading))
        else:
            outcome["undecidedChanged"] = hostloss.record_undecided(self.store, request_id, reading)
        return outcome

    # --------------------------------------------------------------- outcomes

    def _is_current(self, attempt, delivery) -> bool:
        """Only the delivery's CURRENT attempt may move the delivery.

        Reconciling an older attempt after a later one already succeeded would otherwise drag a
        dispatched delivery back to a retryable state and produce a second send.
        """
        if attempt["attempt_no"] != delivery["attempt_count"]:
            return False
        return delivery["state"] not in (DISPATCHED, "acknowledged", "superseded")

    def _settle_from_receipt(self, attempt, delivery, facts, evidence, observation, now, *,
                             turns_checked=False) -> dict:
        """The transport itself settled, so the attempt record is re-derived honestly."""
        record = attempt_record(
            facts,
            request_id=attempt["request_id"], event_id=attempt["event_id"],
            attempt_no=attempt["attempt_no"], recipient=delivery["recipient_task_id"],
            status_before="unknown", observed_at=self.clock.iso(),
            reconciliation={
                "operationReceiptChecked": True,
                "recipientTurnsChecked": turns_checked,
                "affirmativeEvidence": evidence.value,
                "checkedAt": self.clock.iso(),
            },
        )
        next_eligible = None
        hold = None
        if facts.retry_safe:
            reason = "busy" if facts.delivery_state == DEFERRED_BUSY else "presend"
            next_eligible = now + self.policy.delay_for(attempt["attempt_no"] + 1, reason)
            # The same cap as an ordinary settlement. A reconciled pre-send failure is still an
            # attempt, and letting it skip the cap was a way to loop past the bound.
            if attempt["attempt_no"] >= self.policy.cap_for(reason):
                hold = self.policy.cap_reason(reason)
                next_eligible = None
        anchor = self._write(
            attempt, delivery, record, facts.delivery_state, evidence, observation,
            "not scanned", next_eligible, hold=hold,
            dispatch_evidence="transport_accepted" if facts.delivery_state == DISPATCHED else None,
            dispatch_turn_id=facts.turn_id,
        )
        return _with_anchor(
            {"evidence": evidence.value, "state": facts.delivery_state, "record": record},
            anchor,
        )

    def _settle_from_scan(self, attempt, delivery, scan, observation, scan_detail, now) -> dict:
        """The token is in the recipient's items, but the transport never confirmed.

        The stored attempt therefore KEEPS its honest transport snapshot and gains a
        reconciliation record. Rewriting it to look like an accepted dispatch would invent a
        transport status that never existed, and the frozen schema would reject the result.
        The aggregate delivery state is what advances.
        """
        record = json.loads(attempt["record"]) if attempt["record"] else _unfinished_record(
            attempt, delivery, self.clock.iso()
        )
        record["reconciliation"] = {
            "operationReceiptChecked": True,
            "recipientTurnsChecked": True,
            "affirmativeEvidence": Evidence.TURN_FOUND.value,
            "checkedAt": self.clock.iso(),
        }
        anchor = self._write(
            attempt, delivery, record, record["deliveryState"], Evidence.TURN_FOUND,
            observation, scan_detail, None, aggregate=DISPATCHED,
            dispatch_evidence="turn_found", dispatch_turn_id=scan.turn_id,
        )
        return _with_anchor(
            {"evidence": Evidence.TURN_FOUND.value, "state": DISPATCHED, "record": record},
            anchor,
        )

    def _stay_held(self, attempt, delivery, observation, scan_detail, now) -> dict:
        record = json.loads(attempt["record"]) if attempt["record"] else _unfinished_record(
            attempt, delivery, self.clock.iso()
        )
        record["reconciliation"] = {
            "operationReceiptChecked": True,
            "recipientTurnsChecked": "not scanned" not in scan_detail,
            "affirmativeEvidence": Evidence.NONE.value,
            "checkedAt": self.clock.iso(),
        }
        self._write(
            attempt, delivery, record, HELD_UNCERTAIN, Evidence.NONE, observation, scan_detail,
            None,
        )
        return {
            "evidence": Evidence.NONE.value,
            "state": HELD_UNCERTAIN,
            "missing": (
                "no turn id in the operation receipt, no matching turn in the recipient's items, "
                "and no confirmed pre-send rejection"
            ),
            "operationObservation": observation,
            "recipientScan": scan_detail,
            "record": record,
        }

    def _write(self, attempt, delivery, record, state, evidence, observation, scan_detail,
               next_eligible, *, aggregate=None, dispatch_evidence=None, dispatch_turn_id=None,
               hold=None):
        now_iso = self.clock.iso()
        current = self._is_current(attempt, delivery)
        anchor = None
        with self.store.transaction() as db:
            updated = db.execute(
                "UPDATE attempts SET internal_state = 'settled', state = ?, record = ?,"
                " operation_observation = ?,"
                # hostloss.record_undecided owns an undecided name on a dispatched attempt;
                # settling the same dispatch from its receipt again keeps it, and only a reading
                # that decides clears it.
                " recipient_scan = CASE WHEN ? = ? AND recipient_scan LIKE ?"
                "                       THEN recipient_scan ELSE ? END,"
                " affirmative_evidence = ?,"
                " reconciled_at = ? WHERE request_id = ? AND (state IS NULL OR state <> ?)",
                (
                    state, json.dumps(record), observation,
                    state, DISPATCHED, TURN_CHECK_UNDECIDED + ":%", scan_detail,
                    evidence.value, now_iso, attempt["request_id"], HOST_LOST_TURN,
                ),
            ).rowcount
            if updated != 1:
                # Rolled back with the transaction: nothing of this settlement is written.
                raise _AttemptLost()
            if current:
                promoted = db.execute(
                    "UPDATE deliveries SET state = ?, next_eligible_at = ?, hold_reason = ?,"
                    " dispatch_evidence = ?,"
                    " dispatch_turn_id = COALESCE(?, dispatch_turn_id), lease_owner = NULL,"
                    " lease_until = NULL, updated_at = ? WHERE event_id = ? AND attempt_count = ?"
                    "   AND state NOT IN (?, 'acknowledged', 'superseded')",
                    (
                        aggregate or state, next_eligible, hold,
                        dispatch_evidence or delivery["dispatch_evidence"], dispatch_turn_id,
                        now_iso, attempt["event_id"], attempt["attempt_no"], DISPATCHED,
                    ),
                ).rowcount
                # The guarded UPDATE is the authoritative race check, not the snapshot
                # _is_current read before this transaction opened. If another worker settled
                # this delivery and dispatched a later attempt in between, it matches no rows
                # - and binding there would hand the generation the obsolete attempt's turn,
                # after which the turn the real dispatch reached can never bind.
                if promoted == 1 and (aggregate or state) == DISPATCHED:
                    anchor = self._bind_promoted_anchor(db, attempt, delivery, dispatch_turn_id)
            self.store.journal(
                "reconciled", attempt["request_id"],
                {"evidence": evidence.value, "state": aggregate or state}, at=now_iso,
            )
        return anchor

    def _bind_promoted_anchor(self, db, attempt, delivery, dispatch_turn_id):
        """Bind the revision's generation in the transaction that just promoted it.

        Reconciliation is one of the routes that reaches dispatched without going through the
        daemon's own dispatch, and the tick's repair pass runs in a DIFFERENT transaction. A
        child in a third process that emits between the two commits has its completion refused
        as unbound_generation even though the dispatch evidence is already durable. The turn id
        arrived with the receipt or the recipient scan, before this transaction opened, so
        closing that interval costs nothing.

        Only a revision anchors a generation, which is the same condition the repair pass uses.
        """
        if delivery["kind"] != REVISION:
            return None
        turn_id = dispatch_turn_id or delivery["dispatch_turn_id"]
        if not turn_id:
            return None
        event = db.execute(
            "SELECT relationship_id, execution_generation FROM events WHERE event_id = ?",
            (attempt["event_id"],),
        ).fetchone()
        if event is None:
            return None
        return self.registry.bind_anchor_in(
            db, event["relationship_id"], event["execution_generation"],
            dispatch_turn_id=turn_id, source="dispatch_receipt",
        )

    # ---------------------------------------------------------------- restart

    def recover_on_start(self, adapter, *, now=None) -> dict:
        """After a process, app or connection restart, look at everything unresolved.

        Nothing here sends. Recovery establishes what happened; only an ordinary claim can
        ever produce a new attempt, and only when the evidence allows one.
        """
        now = self.clock.now() if now is None else now
        reconciled, held, dispatched = [], [], []
        for attempt in self.open_attempts():
            outcome = self.reconcile_attempt(attempt["request_id"], adapter, now=now)
            reconciled.append({"requestId": attempt["request_id"], **{
                k: v for k, v in outcome.items() if k != "record"
            }})
            if outcome["state"] == HELD_UNCERTAIN:
                held.append(attempt["request_id"])
        for row in self.store.all(
            "SELECT d.event_id FROM deliveries d LEFT JOIN acks a ON a.event_id = d.event_id"
            " WHERE d.state = ? AND d.kind = ? AND a.event_id IS NULL",
            # Only a completion is waiting for an acknowledgement. A revision request travels
            # parent to child and the contract defines no record for that direction, so listing
            # one here reports an obligation nothing can ever meet. Report only; delivery state,
            # leases and retry eligibility are untouched.
            (DISPATCHED, COMPLETION),
        ):
            dispatched.append(row["event_id"])
        return {
            "reconciled": reconciled,
            "heldUncertain": held,
            "awaitingAck": dispatched,
            "resent": [],
        }


def _unfinished_record(attempt, delivery, observed_at) -> dict:
    """The schema-valid projection of an attempt that never settled."""
    return {
        "requestId": attempt["request_id"],
        "eventId": attempt["event_id"],
        "attemptNo": attempt["attempt_no"],
        "recipientTaskId": delivery["recipient_task_id"],
        "deliveryState": HELD_UNCERTAIN,
        "sendAttempted": "unknown",
        "retrySafe": False,
        "transportReceiptStatus": UNFINISHED,
        "failedOperation": None,
        "turnId": None,
        "recipientStatusBefore": "unknown",
        "recipientApprovalPolicy": None,
        "observedAt": observed_at,
    }
