"""Acknowledgement, verdicts, and the correction that goes back to the child.

An acknowledgement is authored by the recipient. The proof is what makes that checkable: it is
computed over the recipient's OWN turn id, which is absent from the message it received, so a
reply that quotes every delivered field back still cannot produce it.

Contract v1 defines its acknowledgement for the child-to-parent direction only. The revision
request that travels the other way is therefore a relay-owned record, and no child-authored
parent acknowledgement is invented for it. Its receipt is evidenced the way the contract already
provides for: the dispatch turn binds the new generation's anchor, and the child's next
completion receipt is the real answer.
"""

import json

from . import NO_DELIVERABLE
from .criteria import CriteriaService, normalise_findings
from .currency import (
    AMBIGUOUS_REASON,
    NOT_ACTIVE,
    STALE_GENERATION,
    SUPERSEDED,
    currency_of,
)
from .delivery import COMPLETION, REVISION
from .errors import AckRefused, RefusalReason, RelayError
from .identity import (
    ack_proof as derive_ack_proof,
    revision_request_event_id,
    sha256_hex,
)
from .transport import ACKNOWLEDGED, DISPATCHED, INBOX_ONLY

VERDICTS = ("verified", "needs_changes", "unverified", "aborted")

# A currency answer names its own reason; this maps it onto the refusal taxonomy so a caller
# that cannot act on an exception type can still act on the reason string.
_CURRENCY_REASONS = {
    STALE_GENERATION: RefusalReason.STALE_GENERATION,
    SUPERSEDED: RefusalReason.SUPERSEDED_REVISION,
    AMBIGUOUS_REASON: RefusalReason.REVISION_AMBIGUOUS,
    NOT_ACTIVE: RefusalReason.RELATIONSHIP_NOT_ACTIVE,
}
REJECTIONS = (
    "stale_generation", "unknown_generation", "duplicate_event", "relationship_not_active",
    "revision_mismatch",
)


class AckService:
    def __init__(self, store, registry, intake, delivery, clock, *, criteria=None, sync=None):
        self.store = store
        self.registry = registry
        self.intake = intake
        self.delivery = delivery
        self.clock = clock
        # Keyword-only with defaults: every existing construction site passes five positional
        # arguments, and none of them should have to change to gain these.
        self.criteria = criteria or CriteriaService(store, clock)
        self.sync = sync

    # ------------------------------------------------------------ disposition

    def evaluate(self, event_id: str):
        """The rejection reason a parent should use, or None if the event is acceptable.

        Computed fresh, inside the acknowledgement transaction, so a generation that advanced
        between dispatch and acknowledgement cannot be accepted by a caller working from a
        stale view.
        """
        row = self.delivery.find(event_id)
        if row is not None and row["kind"] != COMPLETION:
            raise AckRefused(
                RefusalReason.WRONG_DELIVERY_KIND,
                f"{event_id!r} is a {row['kind']}, which is not acknowledged by a parent",
            )
        event = self.intake.row(event_id)
        if event is None:
            return "unknown_generation"
        relationship = self.registry.get(event["relationship_id"])
        if relationship["status"] != "active":
            return "relationship_not_active"
        known = {g["executionGeneration"] for g in relationship["generations"]}
        if event["execution_generation"] not in known:
            return "unknown_generation"
        if event["execution_generation"] < relationship["executionGeneration"]:
            return "stale_generation"
        existing = self.store.one("SELECT * FROM acks WHERE event_id = ?", (event_id,))
        if existing is not None and existing["verified"] == "verified":
            return "duplicate_event"
        return None

    def claim_verification(self, event_id: str, *, turn_id=None) -> str:
        """Idempotent per event, so one revision is never verified twice.

        This is what makes a duplicate delivery harmless: a second arrival of the same event
        cannot obtain the claim, so the parent does not run the work again.
        """
        now = self.clock.iso()
        with self.store.transaction() as db:
            cursor = db.execute(
                "INSERT OR IGNORE INTO verification_claims (event_id, claim_turn_id, claimed_at)"
                " VALUES (?,?,?)",
                (event_id, turn_id, now),
            )
            claimed = cursor.rowcount == 1
            if claimed:
                event = self.intake.row(event_id)
                if event is not None:
                    # Bind this review to the criteria set as it stands now. Editing a
                    # criterion's text later then invalidates the review instead of being
                    # silently certified by findings made against the earlier wording.
                    self.criteria.bind_review(db, event["relationship_id"], event_id)
        return "proceed" if claimed else "already_claimed"

    def claim_holder(self, event_id: str):
        return self.store.one(
            "SELECT * FROM verification_claims WHERE event_id = ?", (event_id,)
        )

    # --------------------------------------------------------- acknowledgement

    def acknowledge(self, event_id, *, ack_turn_id, ack_proof, accepted,
                    rejection_reason=None, adapter=None) -> dict:
        row = self.delivery.find(event_id)
        if row is None:
            raise AckRefused(RefusalReason.NOT_CLAIMABLE, f"no delivery for {event_id!r}")
        if row["kind"] != COMPLETION:
            raise AckRefused(
                RefusalReason.WRONG_DELIVERY_KIND,
                f"{event_id!r} is a {row['kind']}; contract v1 acknowledgements are parent-authored"
                " for a completion event",
            )
        existing = self.store.one("SELECT * FROM acks WHERE event_id = ?", (event_id,))
        if existing is not None and existing["verified"] == "verified":
            return json.loads(existing["record"])
        self._upgrading = existing is not None
        if row["state"] not in (DISPATCHED, INBOX_ONLY):
            raise AckRefused(
                RefusalReason.NOT_CLAIMABLE,
                f"{event_id!r} is {row['state']!r}; only a delivered event is acknowledged",
            )

        expected = derive_ack_proof(event_id, ack_turn_id)
        if ack_proof != expected:
            raise AckRefused(
                RefusalReason.ACK_PROOF_MISMATCH,
                "the proof does not match this event and turn; quoting the delivered fields back "
                "cannot produce it",
            )

        # The adapter read happens before the transaction, because it is I/O and must not be
        # held inside a write lock.
        verification = self._verify_ack_turn(row, ack_turn_id, adapter)
        event = self.intake.row(event_id)
        now = self.clock.iso()

        # Everything that decides the disposition happens INSIDE the write transaction, so a
        # generation that advances between the caller's view and this write cannot be accepted.
        with self.store.transaction() as db:
            fresh = self.delivery.find(event_id)
            if fresh is None or fresh["state"] not in (DISPATCHED, INBOX_ONLY, ACKNOWLEDGED):
                raise AckRefused(
                    RefusalReason.NOT_CLAIMABLE,
                    f"{event_id!r} became {fresh['state'] if fresh else 'absent'!r} before this "
                    "acknowledgement could be written",
                )
            # Always evaluated, including when upgrading an earlier unverified ack:
            # skipping it let a generation that advanced in between be accepted.
            computed = self.evaluate(event_id)
            if accepted and computed:
                raise AckRefused(
                    RefusalReason.DISPOSITION_CONFLICT,
                    f"this event cannot be accepted: {computed}",
                )
            if not accepted and not rejection_reason:
                rejection_reason = computed or "revision_mismatch"
            if accepted:
                rejection_reason = None
            elif rejection_reason not in REJECTIONS:
                raise AckRefused(
                    RefusalReason.DISPOSITION_CONFLICT, f"unknown rejection {rejection_reason!r}"
                )
            record = {
                "eventId": event_id,
                "relationshipId": event["relationship_id"],
                "executionGeneration": event["execution_generation"],
                "revisionHash": event["revision_hash"],
                "ackTurnId": ack_turn_id,
                "accepted": bool(accepted),
                "rejectionReason": rejection_reason,
                "ackAt": now,
                "ackProof": ack_proof,
            }
            db.execute(
                "INSERT INTO acks (event_id, record, ack_turn_id, accepted, verified,"
                " rejection_reason, ack_at) VALUES (?,?,?,?,?,?,?)"
                " ON CONFLICT(event_id) DO UPDATE SET record=excluded.record,"
                " ack_turn_id=excluded.ack_turn_id, accepted=excluded.accepted,"
                " verified=excluded.verified, rejection_reason=excluded.rejection_reason,"
                " ack_at=excluded.ack_at",
                (
                    event_id, json.dumps(record), ack_turn_id, int(bool(accepted)),
                    verification, rejection_reason, now,
                ),
            )
            if verification == "verified":
                db.execute(
                    "UPDATE deliveries SET state = ?, updated_at = ? WHERE event_id = ?",
                    (ACKNOWLEDGED, now, event_id),
                )
            self._write_ack_evidence(
                db, event_id,
                tier="host_read" if verification == "verified" else "unverified",
                detail=None if verification == "verified" else (
                    "no host adapter in this process" if adapter is None
                    else "the host did not confirm this turn"
                ),
                reason=None if verification == "verified" else verification,
                now=now,
            )
            self.store.journal(
                "acknowledged", event_id,
                {"accepted": bool(accepted), "verified": verification, "reason": rejection_reason},
                at=now,
            )
        result = dict(record)
        result["_verified"] = verification
        return result

    def _verify_ack_turn(self, row, ack_turn_id, adapter) -> str:
        """An acknowledgement has to come from a real turn that started after the delivery.

        A turn that cannot be found, or that predates the delivery, does not close the
        attempt. The acknowledgement is still stored and reported, because hiding it would
        lose information, but the delivery stays where it was.
        """
        if adapter is None:
            # No host in this process, so the turn cannot be established. The acknowledgement
            # is still recorded as the parent's own authored intent; what is missing is only
            # the evidence that the turn is real, and that is supplied later by the process
            # that does have host access. Nothing is ever derived from what the relay itself
            # sent: a stored accepted dispatch proves a send was accepted, never that the
            # parent observed and acknowledged it.
            return "unverified_turn"
        attempt = self.store.one(
            "SELECT * FROM attempts WHERE event_id = ? ORDER BY attempt_no DESC LIMIT 1",
            (row["event_id"],),
        )
        try:
            turn = adapter.read_turn(row["recipient_thread_id"], ack_turn_id)
        except Exception:
            return "unverified_turn"
        if turn is None:
            return "unverified_turn"
        if turn.started_at is None:
            # The host did not say when this turn began, so its chronology is unknown and an
            # unknown chronology is not a verification.
            return "unverified_turn"
        if attempt is not None:
            # sent_at is when the send STARTED. Using the settlement time would make a slow
            # transport response turn the dispatch turn itself into a pre-delivery turn.
            sent_at = attempt["sent_at"] or attempt["observed_at"]
            dispatched_turn = None
            if attempt["record"]:
                dispatched_turn = json.loads(attempt["record"]).get("turnId")
            if ack_turn_id != dispatched_turn and certainly_before(turn.started_at, sent_at):
                raise AckRefused(
                    RefusalReason.ACK_TURN_UNVERIFIED,
                    f"turn {ack_turn_id!r} started before the delivery, so it cannot be its "
                    "acknowledgement",
                )
        return "verified"

    # ---------------------------------------------------------------- verdict

    def record_verdict(self, event_id, *, verdict, verdict_turn_id, criteria=None,
                       findings=None, reason=None, expect_criteria_digest=None) -> dict:
        """One rollback-safe operation: the verdict, the generation it opens, and the correction.

        These three cannot be separate writes. Advancing the execution and then failing to
        record the verdict or queue the correction would leave the child on a new generation
        with nothing telling it what to change, which is worse than not advancing at all.
        Re-reading inside the transaction also makes a concurrent replay idempotent, because
        BEGIN IMMEDIATE serialises writers and the second one sees the first one's verdict.

        Everything that decides whether this event is still the thing being completed is read
        INSIDE the transaction. It used to be read before it, which made the decision a
        preflight check, and a preflight check can be raced: verdict=verified was accepted for
        an already acknowledged OLD event after the generation advanced, after a different
        ready revision was accepted in the same generation, and after the relationship was
        paused. acknowledge() already does this correctly and says why; this now matches it.

        No parameter disables the check. An override on the public API writes exactly the stale
        completion this guard exists to prevent, whether or not a CLI flag exposes it.
        """
        if verdict not in VERDICTS:
            raise AckRefused(RefusalReason.DISPOSITION_CONFLICT, f"unknown verdict {verdict!r}")
        findings = normalise_findings(criteria, findings)
        now = self.clock.iso()

        with self.store.transaction() as db:
            settled = db.execute(
                "SELECT record FROM verdicts WHERE event_id = ?", (event_id,)
            ).fetchone()
            if settled is not None:
                record = json.loads(settled["record"])
                # Historical, and marked as such. A replay returns what was decided; it is
                # never a fresh completion, and the assignment state is read from the head
                # revision rather than from the existence of some old verdict.
                record["_replay"] = True
                return record

            ack = self.store.one("SELECT * FROM acks WHERE event_id = ?", (event_id,))
            if ack is None or not ack["accepted"] or ack["verified"] != "verified":
                raise AckRefused(
                    RefusalReason.NOT_ACKNOWLEDGED,
                    f"{event_id!r} has no verified acceptance, so there is nothing to rule on",
                )
            event = self.intake.row(event_id)
            # require_active rather than get: a paused, cancelled or archived assignment is not
            # a thing to rule on at all, for any of the four dispositions.
            relationship = self.registry.require_active(event["relationship_id"])
            relationship_row = self.store.one(
                "SELECT * FROM relationships WHERE relationship_id = ?",
                (event["relationship_id"],),
            )
            state = currency_of(db, relationship_row, event)
            if verdict in ("verified", "needs_changes") and not state["current"]:
                # unverified and aborted are deliberately not gated: neither completes the
                # assignment nor advances the execution, and a stale event is exactly a thing a
                # parent may need to record as unverified. Their context carries the currency
                # they were decided under, so nothing reads as current that was not.
                raise AckRefused(
                    _CURRENCY_REASONS[state["reason"]],
                    f"{event_id!r} cannot be ruled {verdict!r}: {state['detail']}",
                )
            cover = self.criteria.coverage(
                event["relationship_id"], event_id, verdict, findings, reason=reason,
                expected_digest=expect_criteria_digest,
            )
            if verdict == "needs_changes":
                child = relationship["child"]["taskId"]
                if child not in relationship["authorizedScope"]["allowedRecipients"]:
                    raise AckRefused(
                        RefusalReason.RECIPIENT_NOT_AUTHORIZED,
                        f"child {child!r} is not an allowed recipient, so a revision cannot be "
                        "routed to it",
                    )

            record = {
                "eventId": event_id,
                "relationshipId": event["relationship_id"],
                "executionGeneration": event["execution_generation"],
                "verdict": verdict,
                "verdictTurnId": verdict_turn_id,
                "decidedAt": now,
            }
            if findings:
                record["criteria"] = findings

            if verdict == "needs_changes":
                rid = relationship["relationshipId"]
                child = relationship["child"]["taskId"]
                revision_event = revision_request_event_id(rid, event_id, verdict_turn_id)
                next_generation = self.registry.open_generation_in(
                    db, rid, dispatch_request_id=f"revision-{revision_event}",
                    reason="needs_changes_revision",
                )
                payload = {
                    "eventId": revision_event,
                    "relationshipId": rid,
                    "executionGeneration": next_generation,
                    "kind": REVISION,
                    "supersedesEvent": event_id,
                    "supersedesRevisionHash": event["revision_hash"],
                    "verdict": verdict,
                    "verdictTurnId": verdict_turn_id,
                    # What the child is actually being asked to change. A correction with no
                    # findings is a correction nobody can act on.
                    "criteria": findings or [],
                    "childTaskId": child,
                    "emittedAt": now,
                    "note": "relay-owned revision request; contract v1 defines no record for "
                            "this direction",
                }
                db.execute(
                    "INSERT OR IGNORE INTO events (event_id, relationship_id,"
                    " execution_generation, revision_hash, outcome, producer, attempt,"
                    " turn_thread_id, turn_id, turn_status, receipt, stage, first_seen_at,"
                    " last_seen_at, observation_count)"
                    " VALUES (?,?,?,?,?,?,NULL,?,?,?,?, 'final', ?,?,1)",
                    (
                        revision_event, rid, next_generation, NO_DELIVERABLE,
                        "revision_request", "relay", relationship["parent"]["taskId"],
                        verdict_turn_id, "completed", json.dumps(payload), now, now,
                    ),
                )
                self.delivery.enqueue_in(
                    db, revision_event, relationship_id=rid, kind=REVISION,
                    recipient_task_id=child,
                )
                record["nextExecutionGeneration"] = next_generation

            db.execute(
                "INSERT INTO verdicts (event_id, record, verdict, next_generation,"
                " verdict_turn_id, decided_at) VALUES (?,?,?,?,?,?)",
                (
                    event_id, json.dumps(record), verdict,
                    record.get("nextExecutionGeneration"), verdict_turn_id, now,
                ),
            )
            self.store.journal("verdict_recorded", event_id, {"verdict": verdict}, at=now)
            # Everything the frozen contract has no room for. verdicts.record stays exactly
            # what verification-verdict.json allows; this is the relay-owned sidecar.
            db.execute(
                "INSERT INTO verdict_context (event_id, set_digest, coverage, findings, reason,"
                " currency, head_event_id, head_revision, ack_evidence, recorded_at)"
                " VALUES (?,?,?,?,?,?,?,?,?,?)"
                " ON CONFLICT(event_id) DO NOTHING",
                (
                    event_id, cover.get("setDigest"), cover["coverage"],
                    json.dumps(findings) if findings else None, reason,
                    "current" if state["current"] else (state["reason"] or "unknown"),
                    state.get("headEventId"), state.get("headRevisionHash"),
                    self._ack_evidence_tier(event_id), now,
                ),
            )
            if self.sync is not None:
                # Inside this same transaction, so the verdict and the summary owed for it
                # commit together or not at all. It returns None when no target is configured,
                # and it raises nothing a caller must handle: a synchronisation concern must
                # never be able to refuse a verification.
                self.sync.enqueue_verdict_in(
                    db, relationship=relationship, event=event, verdict=verdict,
                    findings=findings, record=record,
                )
        return record

    # ------------------------------------------------ deferred acknowledgement

    def _write_ack_evidence(self, db, event_id, *, tier, detail=None, reason=None,
                            fingerprint=None, next_check_at=None, now=None, bump=False) -> None:
        stamp = now or self.clock.iso()
        db.execute(
            "INSERT INTO ack_evidence (event_id, tier, detail, attempts, last_reason,"
            " fingerprint, next_check_at, observed_at) VALUES (?,?,?,?,?,?,?,?)"
            " ON CONFLICT(event_id) DO UPDATE SET tier = excluded.tier,"
            " detail = excluded.detail, attempts = ack_evidence.attempts + ?,"
            " last_reason = excluded.last_reason, fingerprint = excluded.fingerprint,"
            " next_check_at = excluded.next_check_at, observed_at = excluded.observed_at",
            (
                event_id, tier, detail, 1 if bump else 0, reason, fingerprint, next_check_at,
                stamp, 1 if bump else 0,
            ),
        )

    def _ack_evidence_tier(self, event_id: str) -> str:
        row = self.store.one("SELECT tier FROM ack_evidence WHERE event_id = ?", (event_id,))
        return row["tier"] if row else "unrecorded"

    def _pending_acks(self, *, limit, now) -> list:
        rows = self.store.all(
            "SELECT a.event_id, a.ack_turn_id, a.accepted,"
            "       COALESCE(e.attempts, 0) AS attempts, e.last_reason, e.fingerprint,"
            "       e.next_check_at"
            "  FROM acks a LEFT JOIN ack_evidence e ON e.event_id = a.event_id"
            " WHERE a.verified = 'unverified_turn'"
            "   AND (e.next_check_at IS NULL OR e.next_check_at <= ?)"
            " ORDER BY COALESCE(e.next_check_at, 0), a.event_id LIMIT ?",
            (now, limit),
        )
        return [dict(row) for row in rows]

    def _pending_backoff(self, attempt_no: int) -> float:
        return min(900.0, 30.0 * (2 ** max(0, attempt_no - 1)))

    def _pending_fingerprint(self, event_id: str, delivery_row) -> str:
        """The facts whose change makes a withheld promotion worth reconsidering."""
        event = self.intake.row(event_id)
        relationship = None
        if event is not None:
            relationship = self.store.one(
                "SELECT status, execution_generation FROM relationships"
                " WHERE relationship_id = ?",
                (event["relationship_id"],),
            )
        return sha256_hex("|".join([
            delivery_row["state"] if delivery_row is not None else "absent",
            relationship["status"] if relationship is not None else "absent",
            str(relationship["execution_generation"]) if relationship is not None else "0",
            str(event["execution_generation"]) if event is not None else "0",
        ]))

    def verify_pending_acks(self, adapter, *, limit: int = 8, now=None) -> list:
        """Complete an acknowledgement that was authored without a host, once one is available.

        A parent with no socket can still author its acknowledgement: the record is its own
        intent, carrying its own proof over its own turn id. What it cannot do is establish
        that the turn is real. This supplies that one missing piece of evidence later, from the
        process that does have host access, against the recipient's actual turn list.

        It is a DEFERRED COMPLETION of acknowledge(), so it repeats acknowledge()'s full
        in-transaction disposition discipline rather than only re-reading the turn. The
        evidence that was missing is the turn; the facts that may have moved while it was
        missing are the generation, the relationship status and the delivery state. Promoting
        on turn evidence alone would accept, at this seam, exactly the stale acceptance the
        verdict path was fixed for.

        The parent's authored intent is preserved exactly as written. An intent that can no
        longer be accepted is never rewritten into a rejection the parent did not author: it
        stays recorded, stays unverified, and reports why it could not be completed.
        """
        now = self.clock.now() if now is None else now
        results = []
        for pending in self._pending_acks(limit=limit, now=now):
            event_id = pending["event_id"]
            row = self.delivery.find(event_id)
            if row is None:
                continue
            # I/O FIRST, outside any write lock, exactly as acknowledge() does and for the same
            # reason. Anything that moves while this read is in flight is caught below.
            try:
                verification = self._verify_ack_turn(row, pending["ack_turn_id"], adapter)
            except AckRefused as refusal:
                verification = refusal.reason.value if refusal.reason else "ack_turn_unverified"
            results.append(self._settle_pending_ack(event_id, pending, verification, now))
        return results

    def _settle_pending_ack(self, event_id, pending, verification, now) -> dict:
        stamp = self.clock.iso()
        with self.store.transaction() as db:
            fresh_ack = self.store.one("SELECT * FROM acks WHERE event_id = ?", (event_id,))
            if fresh_ack is None or fresh_ack["verified"] == "verified":
                return {"eventId": event_id, "outcome": "already_settled"}
            fresh = self.delivery.find(event_id)
            blocker = None
            if fresh is None or fresh["state"] not in (DISPATCHED, INBOX_ONLY, ACKNOWLEDGED):
                blocker = "delivery_state_changed"
            else:
                try:
                    computed = self.evaluate(event_id)
                except AckRefused as refusal:
                    computed = refusal.reason.value if refusal.reason else "not_claimable"
                if fresh_ack["accepted"] and computed:
                    blocker = computed
            fingerprint = self._pending_fingerprint(event_id, fresh)

            if blocker is None and verification == "verified":
                db.execute(
                    "UPDATE acks SET verified = 'verified' WHERE event_id = ?", (event_id,)
                )
                db.execute(
                    "UPDATE deliveries SET state = ?, updated_at = ? WHERE event_id = ?",
                    (ACKNOWLEDGED, stamp, event_id),
                )
                self._write_ack_evidence(
                    db, event_id, tier="host_read", fingerprint=fingerprint,
                    next_check_at=None, now=stamp, bump=True,
                )
                self.store.journal("ack_verified", event_id, {"tier": "host_read"}, at=stamp)
                return {"eventId": event_id, "outcome": "verified"}

            reason = blocker or verification
            unchanged = (
                pending["last_reason"] == reason and pending["fingerprint"] == fingerprint
            )
            self._write_ack_evidence(
                db, event_id, tier="unverified",
                detail="the acknowledgement stands exactly as authored; it was not promoted",
                reason=reason, fingerprint=fingerprint,
                next_check_at=now + self._pending_backoff(pending["attempts"] + 1),
                now=stamp, bump=True,
            )
            if not unchanged:
                # A refusal that is still true on every tick is not news. Journalling it once
                # per tick per stuck acknowledgement turns a bounded loop into a growing file.
                self.store.journal(
                    "ack_verification_withheld", event_id, {"reason": reason}, at=stamp
                )
            return {"eventId": event_id, "outcome": "withheld", "reason": reason}


    def bind_dispatched_revision(self, revision_event_id: str):
        """Bind the new generation to the turn the revision dispatch actually started."""
        row = self.delivery.find(revision_event_id)
        if row is None or row["state"] != DISPATCHED or not row["dispatch_turn_id"]:
            return None
        event = self.intake.row(revision_event_id)
        return self.registry.bind_anchor(
            event["relationship_id"], event["execution_generation"],
            dispatch_turn_id=row["dispatch_turn_id"], source="dispatch_receipt",
        )

    def bind_pending_anchors(self, *, limit: int = 50) -> list:
        """Bind every generation still anchor_pending whose revision actually dispatched.

        Binding used to be a hook on ONE path - the daemon's own new dispatch - so a revision
        that reached dispatched any other way left its generation unbound, and by I-06 every
        later receipt for that generation was refused. The routes that missed it are ordinary:
        the deliver command, either reconcile promotion, and a dispatch committed in the last
        tick before a shutdown.

        Recovery over state covers all of them at once, and it repairs a generation that was
        left pending before this existed rather than only preventing new ones. bind_anchor is
        idempotent for the same turn and refuses a conflicting rebind (I-05), so this can
        never move an anchor that is already bound.
        """
        rows = self.store.all(
            "SELECT d.event_id FROM deliveries d"
            "  JOIN events e ON e.event_id = d.event_id"
            "  JOIN generations g ON g.relationship_id = e.relationship_id"
            "   AND g.execution_generation = e.execution_generation"
            " WHERE d.kind = ? AND d.state IN (?,?) AND d.dispatch_turn_id IS NOT NULL"
            "   AND g.anchor_state = ?"
            " ORDER BY d.updated_at LIMIT ?",
            (REVISION, DISPATCHED, ACKNOWLEDGED, "anchor_pending", limit),
        )
        bound = []
        for row in rows:
            try:
                result = self.bind_dispatched_revision(row["event_id"])
            except RelayError:
                # A conflicting rebind stays refused and stays reportable; it is not this
                # pass's business to resolve, and swallowing the others would hide them.
                continue
            if result is not None:
                bound.append(row["event_id"])
        return bound


# The host reports a turn's start as WHOLE SECONDS, while we record the send with microsecond
# precision. A reported start of N therefore means the turn really began somewhere in
# [N, N+1), so it is certainly earlier than a send at S only when N + 1 <= S. Comparing the
# rounded value directly would reject an acknowledgement whose turn started in the same second
# as its own delivery, which is the common case for a fast recipient.
TURN_START_PRECISION_SECONDS = 1.0


def certainly_before(started_at, sent_at_iso, *, precision=TURN_START_PRECISION_SECONDS) -> bool:
    """True only when the turn provably began before the send, given the host's precision."""
    from datetime import datetime

    try:
        sent = datetime.fromisoformat(sent_at_iso).timestamp()
    except (TypeError, ValueError):
        return False
    try:
        started = float(started_at)
    except (TypeError, ValueError):
        return False
    return started + precision <= sent
