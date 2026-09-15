"""Finding out what actually happened to an uncertain send.

The order is fixed and the evidence list is closed. Re-read the operation receipt for the SAME
request id first, then read the recipient's real items. Exactly three things are affirmative
evidence: the receipt now carries a turn id, a turn carrying this attempt's token exists in the
recipient's items, or a confirmed pre-send rejection.

Elapsed time is not on that list, and there is no function here that takes a duration. An
attempt with no affirmative evidence stays held and says precisely what it is missing.
"""

import json
from enum import Enum

from .delivery import COMPLETION, SENDING
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


class Evidence(str, Enum):
    TURN_FOUND = "turn_found"
    RECEIPT_TURN_ID = "receipt_turn_id"
    CONFIRMED_PRE_SEND_REJECTION = "confirmed_pre_send_rejection"
    NONE = "none"


class Reconciler:
    def __init__(self, store, registry, delivery, clock, *, policy=None):
        self.store = store
        self.registry = registry
        self.delivery = delivery
        self.clock = clock
        self.policy = policy or delivery.policy

    def open_attempts(self) -> list:
        """Everything a restart has to look at: in-flight sends and unresolved attempts."""
        return self.store.all(
            "SELECT a.* FROM attempts a JOIN deliveries d ON d.event_id = a.event_id"
            " WHERE a.internal_state = 'in_flight'"
            "    OR (a.state = ? AND d.state IN (?, ?))"
            " ORDER BY a.observed_at",
            (HELD_UNCERTAIN, HELD_UNCERTAIN, SENDING),
        )

    def reconcile_attempt(self, request_id: str, adapter, *, now=None) -> dict:
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
                return self._settle_from_receipt(
                    attempt, delivery, facts, Evidence.RECEIPT_TURN_ID, operation_observation, now
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

    # --------------------------------------------------------------- outcomes

    def _is_current(self, attempt, delivery) -> bool:
        """Only the delivery's CURRENT attempt may move the delivery.

        Reconciling an older attempt after a later one already succeeded would otherwise drag a
        dispatched delivery back to a retryable state and produce a second send.
        """
        if attempt["attempt_no"] != delivery["attempt_count"]:
            return False
        return delivery["state"] not in (DISPATCHED, "acknowledged", "superseded")

    def _settle_from_receipt(self, attempt, delivery, facts, evidence, observation, now) -> dict:
        """The transport itself settled, so the attempt record is re-derived honestly."""
        record = attempt_record(
            facts,
            request_id=attempt["request_id"], event_id=attempt["event_id"],
            attempt_no=attempt["attempt_no"], recipient=delivery["recipient_task_id"],
            status_before="unknown", observed_at=self.clock.iso(),
            reconciliation={
                "operationReceiptChecked": True,
                "recipientTurnsChecked": False,
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
        self._write(
            attempt, delivery, record, facts.delivery_state, evidence, observation,
            "not scanned", next_eligible, hold=hold,
            dispatch_evidence="transport_accepted" if facts.delivery_state == DISPATCHED else None,
            dispatch_turn_id=facts.turn_id,
        )
        return {"evidence": evidence.value, "state": facts.delivery_state, "record": record}

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
        self._write(
            attempt, delivery, record, record["deliveryState"], Evidence.TURN_FOUND,
            observation, scan_detail, None, aggregate=DISPATCHED,
            dispatch_evidence="turn_found", dispatch_turn_id=scan.turn_id,
        )
        return {"evidence": Evidence.TURN_FOUND.value, "state": DISPATCHED, "record": record}

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
        with self.store.transaction() as db:
            db.execute(
                "UPDATE attempts SET internal_state = 'settled', state = ?, record = ?,"
                " operation_observation = ?, recipient_scan = ?, affirmative_evidence = ?,"
                " reconciled_at = ? WHERE request_id = ?",
                (
                    state, json.dumps(record), observation, scan_detail, evidence.value, now_iso,
                    attempt["request_id"],
                ),
            )
            if current:
                db.execute(
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
                )
            self.store.journal(
                "reconciled", attempt["request_id"],
                {"evidence": evidence.value, "state": aggregate or state}, at=now_iso,
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
