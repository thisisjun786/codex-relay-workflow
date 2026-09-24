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

from . import NO_DELIVERABLE, restoration
from .criteria import CriteriaService, normalise_findings
from .currency import (
    AMBIGUOUS_REASON,
    NOT_ACTIVE,
    STALE_GENERATION,
    SUPERSEDED,
    currency_of,
)
from .delivery import COMPLETION, MANIFEST_LINES, REVISION, SENDING
from .errors import AckRefused, RefusalReason, RelayError
from .identity import (
    ack_proof as derive_ack_proof,
    revision_request_event_id,
    sha256_hex,
)
from .transport import ACKNOWLEDGED, DISPATCHED, HELD_UNCERTAIN, INBOX_ONLY

VERDICTS = ("verified", "needs_changes", "unverified", "aborted")
# A delivery the relay has not confirmed yet: an uncertain send reconciliation has not settled,
# or a send still in flight. The parent may already be acting on it - the host ran the delivery
# turn and only the turn/start receipt was lost (CRW-124 R3, H0R3-F2) - so its acknowledgement is
# kept as authored rather than refused, and completed once the delivery is confirmed.
UNCONFIRMED = (HELD_UNCERTAIN, SENDING)
DELIVERY_UNCONFIRMED = "delivery_unconfirmed"
_PENDING_COLUMNS = (
    "SELECT a.event_id, a.ack_turn_id, a.ack_at, a.accepted,"
    "       COALESCE(e.attempts, 0) AS attempts, e.last_reason, e.fingerprint,"
    "       e.next_check_at"
)


def _delivery_basis(row) -> tuple:
    """What a turn check was made against: the delivery's state, its current attempt and the
    turn that attempt reached. A check read against one of these is evidence about it only."""
    return (row["state"], row["attempt_count"], row["dispatch_turn_id"])

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

        The one thing that reopens it is the canonical criteria moving. Editing a criterion
        invalidates the review bound to the old wording, and the refusal that follows says to
        claim the review again - which an unconditional INSERT OR IGNORE made impossible. The
        review could then never be finished, and an event id is derived from the artifact
        revision, so re-emitting unchanged bytes produced no new event to claim either: the
        assignment stayed blocked until the child changed a byte it had no reason to change.
        Re-claiming is allowed exactly where the set moved and nothing else did, so a duplicate
        delivery still cannot obtain the claim (I-54).

        Re-claiming rebinds to the set in force, and that is what keeps this idempotent again
        afterwards: the next claim finds the binding current and is refused like any other
        duplicate. Leaving it unbound would make every later claim look like another re-review
        and hand the event to whoever asked last, which is I-54 undone.
        """
        now = self.clock.iso()
        with self.store.transaction() as db:
            cursor = db.execute(
                "INSERT OR IGNORE INTO verification_claims (event_id, claim_turn_id, claimed_at)"
                " VALUES (?,?,?)",
                (event_id, turn_id, now),
            )
            claimed = cursor.rowcount == 1
            reclaimed = False
            if (not claimed and not self._ruling_is_current(db, event_id)
                    and self._re_review_open(
                        db, event_id, self.criteria.bound_digest(event_id)
                    )):
                db.execute(
                    "UPDATE verification_claims SET claim_turn_id = ?, claimed_at = ?"
                    " WHERE event_id = ?",
                    (turn_id, now, event_id),
                )
                # bind_review below is INSERT OR IGNORE, so the stale binding has to go first.
                db.execute("DELETE FROM claim_context WHERE event_id = ?", (event_id,))
                reclaimed = True
                self.store.journal(
                    "review_reclaimed", event_id, {"claimTurnId": turn_id}, at=now
                )
            if claimed or reclaimed:
                event = self.intake.row(event_id)
                if event is not None:
                    # Bind this review to the criteria set as it stands now. Editing a
                    # criterion's text later then invalidates the review instead of being
                    # silently certified by findings made against the earlier wording.
                    self.criteria.bind_review(db, event["relationship_id"], event_id)
        # "proceed" either way: a re-claim means the caller holds the review and may rule on
        # it, which is exactly what this word tells every existing caller. That a review was
        # reopened is recorded in the journal rather than smuggled into a return value callers
        # compare against a fixed string.
        return "proceed" if (claimed or reclaimed) else "already_claimed"

    def _ruling_is_current(self, db, event_id) -> bool:
        """Has a ruling already been made against the set in force?

        There are two records of what a review was decided against, because there are two ways
        to decide one. Claiming binds the set, and the ruling inherits that binding; stating
        the digest is the alternative record_verdict accepts instead, and that path never
        touches claim_context. So an attested re-review leaves the binding naming a set nobody
        judges by any more, and a claim reading only the binding would call that review stale
        forever and hand out the claim again on an assignment the ruling already brought up to
        date.

        Only claiming asks this. Ruling asks whether the RULING is stale, which is a different
        question with a different answer once a review has been claimed again.
        """
        row = db.execute(
            "SELECT set_digest FROM verdict_context WHERE event_id = ?", (event_id,)
        ).fetchone()
        if row is None:
            return False
        event = self.intake.row(event_id)
        if event is None:
            return False
        registered = self.criteria.get(event["relationship_id"])
        return row["set_digest"] == (registered["setDigest"] if registered else None)

    def _re_review_open(self, db, event_id, decided_digest) -> bool:
        """Has the criteria set moved out from under a review already decided against it?

        One condition for both entry points - claiming and ruling - so the two can never
        disagree about whether a re-review is open. What differs is the digest each one hands
        in: claiming passes the set the review is BOUND to, ruling passes the set the recorded
        ruling was DECIDED on. Everything else is shared: the set in force differs from that
        one, the event is still the revision this generation stands on, and any ruling already
        recorded for it is a verified one.

        Where a verified ruling exists, that is exactly the state AssignmentView reports as
        re_review_needed. Where a review was claimed but never ruled, the view still says
        verifying, because nothing has been certified to re-review; what has happened is only
        that its binding no longer matches, and its ruling stays refused with
        criteria_set_changed until the review is claimed again. The two cases are one condition
        because the remedy is one thing - claim it again - not because the view names them
        alike.

        needs_changes is excluded because it already moved the assignment to a new generation,
        whose revision arrives as its own event with its own claim; aborted ends the assignment.
        For either of those, a second ruling on the old event would be a second allocation
        rather than a re-review.

        Currency is required for the same reason record_verdict requires it: without it a
        verified ruling on an event some later generation left behind could be rewritten, and
        the view does not call that event re_review_needed in the first place.
        """
        settled = db.execute(
            "SELECT verdict FROM verdicts WHERE event_id = ?", (event_id,)
        ).fetchone()
        if settled is not None and settled["verdict"] != "verified":
            return False
        event = self.intake.row(event_id)
        if event is None:
            return False
        registered = self.criteria.get(event["relationship_id"])
        if decided_digest == (registered["setDigest"] if registered else None):
            return False
        relationship = db.execute(
            "SELECT * FROM relationships WHERE relationship_id = ?",
            (event["relationship_id"],),
        ).fetchone()
        if relationship is None:
            return False
        return bool(currency_of(db, relationship, event)["current"])

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
            return self._settled_ack(existing)
        if row["state"] not in (DISPATCHED, INBOX_ONLY) + UNCONFIRMED:
            raise AckRefused(
                RefusalReason.NOT_CLAIMABLE,
                f"{event_id!r} is {row['state']!r}; only a delivered event, or one whose send is"
                " still being confirmed, is acknowledged",
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
        try:
            verification = self._verify_ack_turn(row, ack_turn_id, adapter)
        except AckRefused as refusal:
            # While the delivery is unconfirmed, the turn it reached is not known yet: a send can
            # be folded into a parent turn that began before it, and that turn is where the parent
            # acknowledges (review 2). Such an acknowledgement is kept below, never promoted
            # here; the pending pass checks the chronology again once the delivery is confirmed.
            if (row["state"] not in UNCONFIRMED
                    or refusal.reason != RefusalReason.ACK_TURN_UNVERIFIED):
                raise
            verification = refusal.reason.value
        event = self.intake.row(event_id)
        now = self.clock.iso()

        # Everything that decides the disposition happens INSIDE the write transaction, so a
        # generation that advances between the caller's view and this write cannot be accepted.
        with self.store.transaction() as db:
            # Re-read FIRST, for the same reason the disposition below is computed here: the
            # read above happens outside the lock and can be raced. Two processes disposing of
            # one event both passed it, and whichever committed second overwrote the first.
            # The taxonomy decided which one that was: evaluate() reports duplicate_event for a
            # settled event, but the conflict guard refused it only for accepted=True, so a
            # rejection carried on into the upsert and replaced a verified acceptance. A
            # verified acknowledgement is settled, whatever the second caller asked for, and it
            # is told what stands instead of being allowed to replace it.
            already = db.execute(
                "SELECT * FROM acks WHERE event_id = ?", (event_id,)
            ).fetchone()
            if already is not None and already["verified"] == "verified":
                return self._settled_ack(already)
            fresh = self.delivery.find(event_id)
            if fresh is None or fresh["state"] not in (
                    DISPATCHED, INBOX_ONLY, ACKNOWLEDGED) + UNCONFIRMED:
                raise AckRefused(
                    RefusalReason.NOT_CLAIMABLE,
                    f"{event_id!r} became {fresh['state'] if fresh else 'absent'!r} before this "
                    "acknowledgement could be written",
                )
            # Decided from this row, under the lock, never from the read above: a delivery the
            # relay has not confirmed is not closed by an acknowledgement, whatever the turn
            # check said, and neither is one that moved while the turn was read - confirmed,
            # sent again, or reaching another turn (review 3). The check was evidence about the
            # delivery as it was read, so the acknowledgement is kept as authored and checked
            # again against the delivery as it is: the pending pass takes it at once once the
            # delivery is confirmed, and a verdict completes it first (H0R3-F2).
            unconfirmed = fresh["state"] in UNCONFIRMED
            moved = _delivery_basis(fresh) != _delivery_basis(row)
            kept = unconfirmed or moved
            why = fresh["state"] if unconfirmed else "changed while its turn was read"
            stored = "unverified_turn" if kept else verification
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
                    stored, rejection_reason, now,
                ),
            )
            if stored == "verified":
                db.execute(
                    "UPDATE deliveries SET state = ?, updated_at = ? WHERE event_id = ?",
                    (ACKNOWLEDGED, now, event_id),
                )
            if kept:
                self._write_ack_evidence(
                    db, event_id, tier="unverified",
                    detail=f"kept as authored: the relay could not yet confirm this delivery for"
                           f" the turn it read ({why}), and completes the acknowledgement once it"
                           f" does",
                    reason=DELIVERY_UNCONFIRMED, now=now,
                )
            else:
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
            journalled = {"accepted": bool(accepted), "verified": stored,
                          "reason": rejection_reason}
            if kept:
                journalled["deliveryUnconfirmed"] = why
            self.store.journal(
                "acknowledged", event_id, journalled, at=now,
            )
        result = dict(record)
        result["_verified"] = stored
        if kept:
            result["_deliveryUnconfirmed"] = why
        return result

    @staticmethod
    def _settled_ack(existing) -> dict:
        """What a caller gets when this acknowledgement was already settled, possibly by someone else.

        The stored record exactly as written, plus the two facts a caller cannot read off it:
        that it is verified, and that it is not the disposition this call asked for. Without the
        first, a caller checking _verified attaches a "not established yet" note to an
        acknowledgement that is in fact established.
        """
        record = json.loads(existing["record"])
        record["_verified"] = existing["verified"]
        record["_replay"] = True
        return record

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
            # A send confirmed from its message keeps its honest transport snapshot, which names no
            # turn; the turn the message was found in is the delivery's (reconcile._settle_from_scan),
            # and it belongs to this attempt only when this attempt was confirmed that way. A send
            # folded into a turn begun before it is acknowledged from that turn (review 2); a turn
            # an earlier attempt reached is not this attempt's (review 3).
            if not dispatched_turn and attempt["affirmative_evidence"] == "turn_found":
                dispatched_turn = row["dispatch_turn_id"]
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

        A replay is not the same thing as a re-review. Editing the canonical criteria after a
        verified ruling is already reported as re_review_needed, but every attempt to actually
        re-review returned the historical record before it looked at the new set, so the
        assignment could not be completed against the criteria it was now being judged by. The
        replay stands for everything that has not moved; where the set moved under a verified
        ruling on the current revision, this rules again.
        """
        if verdict not in VERDICTS:
            raise AckRefused(RefusalReason.DISPOSITION_CONFLICT, f"unknown verdict {verdict!r}")
        findings = normalise_findings(criteria, findings)
        now = self.clock.iso()

        with self.store.transaction() as db:
            settled = db.execute(
                "SELECT v.record AS record, c.set_digest AS set_digest FROM verdicts v"
                "  LEFT JOIN verdict_context c ON c.event_id = v.event_id"
                " WHERE v.event_id = ?",
                (event_id,),
            ).fetchone()
            re_review = settled is not None and self._re_review_open(
                db, event_id, settled["set_digest"]
            )
            if settled is not None and not re_review:
                record = json.loads(settled["record"])
                # Historical, and marked as such. A replay returns what was decided; it is
                # never a fresh completion, and the assignment state is read from the head
                # revision rather than from the existence of some old verdict.
                record["_replay"] = True
                return record

            if re_review and verdict not in ("verified", "needs_changes"):
                # A re-review exists to resolve re_review_needed, and only these two do.
                # unverified and aborted have no assignment state of their own, so replacing a
                # standing certification with either would drop the assignment back to
                # verifying with the claim still held - and _re_review_open would then refuse
                # to reopen it, because the ruling of record is no longer a verified one. That
                # is the deadlock this change exists to remove, rebuilt one disposition over.
                # Neither is lost: both remain available on an event that has no ruling yet,
                # and an assignment nobody intends to finish is paused or cancelled on the
                # relationship rather than annotated on one of its events.
                raise AckRefused(
                    RefusalReason.DISPOSITION_CONFLICT,
                    f"{verdict!r} cannot replace the verified ruling this re-review is "
                    "reopening: it would leave the assignment with no state to act on. Rule "
                    "verified or needs_changes, or change the relationship's status",
                )

            if re_review and expect_criteria_digest is None:
                # The ruling being replaced was decided against a different set. A caller that
                # does not name the set it read cannot be told apart from one re-submitting the
                # old ruling unchanged, and that is not a review of anything: it would clear
                # re_review_needed without anybody having read the new wording. Naming the
                # digest is the attestation criteria.coverage already accepts in place of a
                # binding, and the CLI already carries it as --expect-criteria-digest.
                raise AckRefused(
                    RefusalReason.CRITERIA_SET_CHANGED,
                    f"{event_id!r} was ruled against a different criteria set, so this is a "
                    "re-review, and a re-review names the set it read: pass the reviewed "
                    "digest explicitly",
                )

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
                # Decided HERE, before open_generation_in, because this is the last moment at
                # which there is anything to go back to. A few statements below, the
                # generation the child was working in has been superseded and the correction
                # has been queued, and a block discovered missing after that has no channel
                # left: the verdict does not resend and a parallel path is not allowed. So the
                # question is asked while refusing still means something. Refusing rolls the
                # whole transaction back - no generation opens, nothing is queued - and the
                # parent moves the block and rules again.
                projected = restoration.project_cap(findings, cap=MANIFEST_LINES)
                if projected["outcome"] in restoration.UNDELIVERABLE:
                    raise AckRefused(
                        RefusalReason.RESTORATION_UNDELIVERABLE,
                        f"this correction declares a restoration block on "
                        f"{projected['criterion']!r} that the revision message would not "
                        f"carry: {projected['detail']}. Move it within the first "
                        f"{MANIFEST_LINES} findings and rule again. No execution generation "
                        "has been opened",
                    )
            else:
                # Named rather than left blank. A verdict that opens no correction has no
                # message for a block to travel in, and a coordinator that attached one to a
                # verified ruling learns it here instead of from its absence.
                projected = restoration.not_carried(
                    basis=restoration.LEGACY_BASIS,
                    detail=f"a {verdict} verdict opens no correction, so no message carries a "
                           "restoration block",
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
                # The same finding, filed under the event the CHILD is sent to. The revision
                # message ends with "Full record: ... show --event <revision>", so that is
                # where a recipient looks, and filing this only under the superseded
                # completion left that lookup empty until an attempt or a report existed. An
                # empty list there cannot be told from a correction nobody measured, which is
                # the distinction this whole path exists to make.
                correction_event = revision_event
            else:
                correction_event = None

            db.execute(
                "INSERT INTO verdicts (event_id, record, verdict, next_generation,"
                " verdict_turn_id, decided_at) VALUES (?,?,?,?,?,?)"
                " ON CONFLICT(event_id) DO UPDATE SET record = excluded.record,"
                " verdict = excluded.verdict, next_generation = excluded.next_generation,"
                " verdict_turn_id = excluded.verdict_turn_id, decided_at = excluded.decided_at",
                (
                    event_id, json.dumps(record), verdict,
                    record.get("nextExecutionGeneration"), verdict_turn_id, now,
                ),
            )
            self.store.journal("verdict_recorded", event_id, {"verdict": verdict}, at=now)
            # In the same transaction as the ruling it describes. The journal is where this
            # persists: verdicts.record must stay exactly what verification-verdict.json
            # allows, that schema freezes additionalProperties on the record, and this store
            # has no migration path for a new verdict_context column.
            self.store.journal("restoration_projected", event_id, projected, at=now)
            if correction_event is not None:
                self.store.journal(
                    "restoration_projected", correction_event, projected, at=now,
                )
            if re_review:
                # The schema has one verdict row per event and this change adds no table, so
                # the ruling being replaced is kept where an append-only record already exists.
                # Both digests travel with it: the pair is what made the old ruling history.
                self.store.journal(
                    "verdict_superseded", event_id,
                    {
                        "supersededVerdict": json.loads(settled["record"]),
                        "reviewedSetDigest": settled["set_digest"],
                        "currentSetDigest": cover.get("setDigest"),
                    },
                    at=now,
                )
            # Everything the frozen contract has no room for. verdicts.record stays exactly
            # what verification-verdict.json allows; this is the relay-owned sidecar.
            db.execute(
                "INSERT INTO verdict_context (event_id, set_digest, coverage, findings, reason,"
                " currency, head_event_id, head_revision, ack_evidence, recorded_at)"
                " VALUES (?,?,?,?,?,?,?,?,?,?)"
                # A conflict here is now reachable in exactly one way, a re-review, and leaving
                # the old digest in place would leave the assignment reading re_review_needed
                # forever - the deadlock wearing a different hat. A concurrent replay never
                # reaches this statement: it returns above, serialised by BEGIN IMMEDIATE.
                " ON CONFLICT(event_id) DO UPDATE SET set_digest = excluded.set_digest,"
                " coverage = excluded.coverage, findings = excluded.findings,"
                " reason = excluded.reason, currency = excluded.currency,"
                " head_event_id = excluded.head_event_id,"
                " head_revision = excluded.head_revision,"
                " ack_evidence = excluded.ack_evidence, recorded_at = excluded.recorded_at",
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
                #
                # The ruling's own ordinal, counted AFTER the verdict_superseded entry this
                # ruling may just have written and inside the same transaction, so it counts
                # this ruling too: the first is 1, each re-review is one more. It has to be
                # relay-generated. verdict_turn_id cannot do it - verdicts is keyed by event
                # alone, nothing requires a re-review's turn to differ from the ruling it
                # replaces, and claim_verification can re-claim under the same turn - so two
                # rulings could share one, and then a criteria set edited away and back would
                # recompute the first ruling's sync id and be dropped exactly as before.
                #
                # Passed only when it exceeds 1. A first ruling has nothing to be told apart
                # from, and leaving it off is what keeps a verdict with no canonical criteria -
                # no digest either - producing the identity it produced before this existed.
                # enqueue_verdict_in owns that rule, because the summary names the ordinal
                # even when identity leaves it out.
                ruling = 1 + db.execute(
                    "SELECT COUNT(*) AS seen FROM journal WHERE kind = ? AND subject = ?",
                    ("verdict_superseded", event_id),
                ).fetchone()["seen"]
                self.sync.enqueue_verdict_in(
                    db, relationship=relationship, event=event, verdict=verdict,
                    findings=findings, record=record,
                    # Subscript, not get: coverage() sets this key on both of its return paths,
                    # so get() could only turn a future contract break into a silent None.
                    criteria_digest=cover["setDigest"],
                    ruling=ruling,
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

    def restoration_of(self, event_id: str) -> dict:
        """What the ruling of record established about its restoration block.

        Deliberately NOT returned inside the verdict record. verification-verdict.json freezes
        additionalProperties on that object, and the conformance suite validates the record
        this method's caller returns, so a relay-owned annotation added there would be a
        contract violation dressed as observability. The journal holds it, this reads it back,
        and the command surface reports it beside the record rather than inside it.

        A ruling made before this relay measured anything has no entry, and that is reported
        as unmeasured rather than as a block that was fine. The two are not the same claim and
        only one of them is evidence.
        """
        row = self.store.one(
            "SELECT detail FROM journal WHERE kind = ? AND subject = ?"
            " ORDER BY seq DESC LIMIT 1",
            ("restoration_projected", event_id),
        )
        if row is None or not row["detail"]:
            return restoration.unmeasured(
                "this ruling was recorded before the relay measured restoration delivery"
            )
        return json.loads(row["detail"])

    def _pending_acks(self, *, limit, now) -> list:
        """Acknowledgements still to complete, due by their backoff.

        One kept while its delivery was unconfirmed is taken as soon as the delivery is
        confirmed, backoff or not: that was the one fact it waited for, not a refusal to repeat.
        """
        rows = self.store.all(
            _PENDING_COLUMNS +
            "  FROM acks a LEFT JOIN ack_evidence e ON e.event_id = a.event_id"
            "  LEFT JOIN deliveries d ON d.event_id = a.event_id"
            " WHERE a.verified = 'unverified_turn'"
            "   AND (e.next_check_at IS NULL OR e.next_check_at <= ?"
            "        OR (e.last_reason = ? AND d.state IN (?, ?)))"
            " ORDER BY COALESCE(e.next_check_at, 0), a.event_id LIMIT ?",
            (now, DELIVERY_UNCONFIRMED, DISPATCHED, INBOX_ONLY, limit),
        )
        return [dict(row) for row in rows]

    def kept_unconfirmed(self, now=None, *, limit: int = 8) -> list:
        """(event id, acknowledging turn) of each acknowledgement kept while its delivery is
        still unconfirmed and whose check is due.

        What the daemon confirms through the acknowledging turn (Reconciler.confirm_delivery)
        before completing it; the pending pass's backoff paces it.
        """
        now = self.clock.now() if now is None else now
        rows = self.store.all(
            "SELECT a.event_id, a.ack_turn_id FROM acks a"
            "  JOIN ack_evidence e ON e.event_id = a.event_id"
            "  JOIN deliveries d ON d.event_id = a.event_id"
            " WHERE a.verified = 'unverified_turn' AND e.last_reason = ? AND d.state = ?"
            "   AND (e.next_check_at IS NULL OR e.next_check_at <= ?)"
            " ORDER BY COALESCE(e.next_check_at, 0), a.event_id LIMIT ?",
            (DELIVERY_UNCONFIRMED, HELD_UNCERTAIN, now, limit),
        )
        return [(row["event_id"], row["ack_turn_id"]) for row in rows]

    def _kept(self, event_id):
        return self.store.one(
            _PENDING_COLUMNS +
            "  FROM acks a JOIN ack_evidence e ON e.event_id = a.event_id"
            " WHERE a.event_id = ? AND a.verified = 'unverified_turn' AND e.last_reason = ?",
            (event_id, DELIVERY_UNCONFIRMED),
        )

    def kept_turn(self, event_id: str):
        """The acknowledging turn of an acknowledgement kept while its delivery was unconfirmed,
        or None when this event has no such acknowledgement."""
        row = self._kept(event_id)
        return row["ack_turn_id"] if row is not None else None

    def complete_pending(self, event_id: str, adapter, *, now=None):
        """Complete one kept acknowledgement now, exactly as the pending pass would.

        A verdict on an event whose acknowledgement was kept while its delivery was unconfirmed
        calls this first (after confirming the delivery), so the parent's ruling does not wait
        for the daemon's next pass. None when there is nothing kept for this event.
        """
        now = self.clock.now() if now is None else now
        pending = self._kept(event_id)
        row = self.delivery.find(event_id)
        if pending is None or row is None:
            return None
        pending = dict(pending)
        try:
            verification = self._verify_ack_turn(row, pending["ack_turn_id"], adapter)
        except AckRefused as refusal:
            verification = refusal.reason.value if refusal.reason else "ack_turn_unverified"
        return self._settle_pending_ack(event_id, pending, verification, now, basis=row)

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
            results.append(self._settle_pending_ack(event_id, pending, verification, now,
                                                    basis=row))
        return results

    def _settle_pending_ack(self, event_id, pending, verification, now, *, basis=None) -> dict:
        stamp = self.clock.iso()
        with self.store.transaction() as db:
            fresh_ack = self.store.one("SELECT * FROM acks WHERE event_id = ?", (event_id,))
            if fresh_ack is None or fresh_ack["verified"] == "verified":
                return {"eventId": event_id, "outcome": "already_settled"}
            if (fresh_ack["ack_turn_id"] != pending["ack_turn_id"]
                    or fresh_ack["ack_at"] != pending["ack_at"]):
                # The parent acknowledged again while the old turn was being read. What was read
                # is evidence about the old acknowledgement only, so the new one is left exactly
                # as written, for its own pass.
                return {"eventId": event_id, "outcome": "replaced"}
            fresh = self.delivery.find(event_id)
            if (basis is not None and fresh is not None
                    and _delivery_basis(fresh) != _delivery_basis(basis)):
                # The delivery moved while the turn was read (review 3). The reading is evidence
                # about the delivery as it was, so nothing is written and the next pass reads the
                # turn again against the delivery as it is.
                return {"eventId": event_id, "outcome": "changed"}
            blocker = None
            if fresh is not None and fresh["state"] in UNCONFIRMED:
                blocker = DELIVERY_UNCONFIRMED
            elif fresh is None or fresh["state"] not in (DISPATCHED, INBOX_ONLY, ACKNOWLEDGED):
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
