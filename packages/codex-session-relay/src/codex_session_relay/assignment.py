"""One issue, one responsible child, and where that assignment actually stands.

Derived rather than stored, except for the one fact the relay cannot observe. The relay sees
registration, receipts, claims, verdicts and generations; it cannot see a merge, so merged is the
single explicit mark. Deriving the rest means the state cannot drift away from the records it is
supposed to summarise.

A mark is bound to the exact revision it is about. Keyed on the relationship alone, one old merge
would have labelled every later generation merged, which is the opposite of what a coordinator
needs from this view. A mark that no longer matches the current head is history, and history is
returned as history.
"""

import json

from .currency import AMBIGUOUS, head_revision
from .errors import RefusalReason
from .transport import (
    ACKNOWLEDGED,
    DEFERRED_BUSY,
    DISPATCHED,
    HELD_UNCERTAIN,
    INBOX_ONLY,
    QUEUED,
    SENDING,
    SUPERSEDED,
    WITHHELD_PRE_SEND,
)

REQUESTED = "requested"
RECEIVED = "received"
VERIFYING = "verifying"
NEEDS_CHANGES = "needs_changes"
CORRECTED = "corrected"
VERIFIED = "verified"
MERGED = "merged"
PAUSED = "paused"
AMBIGUOUS_STATE = "ambiguous"
REREVIEW_NEEDED = "re_review_needed"
CLOSED = "closed"
ABANDONED = "abandoned"

MARKS = ("merged",)

# linkage.PROJECT, spelled here for the same reason delivery spells it: linkage reaches this
# module from inside its own functions, so a module-level import would close a ring.
PROJECT_SCOPE_KIND = "project"

PROJECT_READING_LIMITS = (
    "A reading of what this project's children report, not a completion verdict. The strongest "
    "state is complete_candidate: integration and verification are the parent's judgment and are "
    "not visible here. A child's own goal status is never consulted; each assignment's state is "
    "derived from receipts, verdicts and marks against the current head, generation and "
    "revision. unreadable, unregistered and ambiguous are three different answers and none of "
    "them means finished."
)

# What each state is waiting for. Derived from the same records, never stored, so it cannot go
# stale: this is the field that answers "what happens next" without a second lookup.
NEXT_ACTION = {
    REQUESTED: "child_emits",
    RECEIVED: "daemon_delivers",
    VERIFYING: "parent_verifies",
    NEEDS_CHANGES: "child_corrects",
    CORRECTED: "parent_verifies",
    VERIFIED: "coordinator_integrates",
    MERGED: "none",
    PAUSED: "owner_resumes",
    AMBIGUOUS_STATE: "child_declares_supersession",
    REREVIEW_NEEDED: "parent_verifies",
    CLOSED: "none",
    ABANDONED: "none",
}

# What happens next while a needs_changes verdict's correction has not reached the child. The
# child cannot correct what it was never sent, so child_corrects is kept for a correction whose
# delivery reached a turn. An unsent correction is the relay's to deliver; one whose send is in
# flight or answered unusably is the relay's to confirm (reconciliation settles it); and a held
# one, including one stored where the child reads without waking it, is never retried (no
# command clears a hold; its recovery is a fresh execution generation), so it is the parent's to
# recover. Derived, like NEXT_ACTION, from the one statement that reads the correction's delivery.
CORRECTION_UNSENT_ACTION = "daemon_delivers_correction"
CORRECTION_UNCONFIRMED_ACTION = "daemon_confirms_correction"
CORRECTION_HELD_ACTION = "parent_recovers_held_correction"
# A final event of the correction's generation already answered it (a failed, interrupted or
# blocked reply, which leaves the assignment in needs_changes): the relay suppresses the
# correction instead of sending it, and what is owed is the parent reading that answer, which
# dispositions-show lists. The only supersession a correction delivery can carry is this one
# (delivery._supersession_reason for a revision request).
CORRECTION_ANSWERED_ACTION = "parent_reads_child_disposition"

# delivery states in which nothing has been sent (dispositions.NOT_SENT uses the same three).
NOT_SENT_STATES = (QUEUED, DEFERRED_BUSY, WITHHELD_PRE_SEND)
# delivery states whose send may or may not have reached the child (dispositions.SEND_UNCERTAIN).
UNCONFIRMED_STATES = (SENDING, HELD_UNCERTAIN)
# delivery states that establish the correction reached a turn.
REACHED_STATES = (DISPATCHED, ACKNOWLEDGED)

LIFECYCLE_WITHHOLD_SOURCE = "failed_operations.lifecycle_read"

# The failure record a lifecycle withhold writes, and the read that finds it. Placed after the
# delivery join it compares with, because an ON clause may only name tables to its left.
# {event} and {delivery} are the aliases of the event and its delivery in the statement that
# uses it; the record counts only when it was written by the transition the delivery row
# currently shows (same stamp, same deadline) and no other operation carries that stamp.
LIFECYCLE_WITHHOLD_JOIN = (
    " LEFT JOIN failed_operations {alias} ON {alias}.scope_key = {event}.event_id"
    "       AND {alias}.operation = 'lifecycle_read'"
    "       AND {alias}.occurred_at = {delivery}.updated_at"
    "       AND {alias}.next_retry_at = {delivery}.next_eligible_at"
    "       AND NOT EXISTS (SELECT 1 FROM failed_operations other"
    "                        WHERE other.scope_key = {event}.event_id"
    "                          AND other.operation <> {alias}.operation"
    "                          AND other.occurred_at = {alias}.occurred_at)"
)


def undelivered_reason(row):
    """Why an event's delivery has not gone out, copied from the row that recorded it.

    row carries: delivered (the delivery's event id or None), delivery_state, hold_reason,
    relationship_status, superseded_by, lifecycle_withhold, lifecycle_recorded_at,
    lifecycle_next_retry_at and refusal_reason, all read by ONE statement so the reason cannot
    come from a later snapshot than the delivery it explains. Most specific first:

    - a hold is about THIS delivery;
    - an assignment somebody paused, cancelled or archived stops an unsent delivery before any
      host read (delivery.attempt), and writes no failure row for it, so its status is the
      reason. Superseded assignments keep their own vocabulary and are not named here;
    - a lifecycle withhold of this very event, when it is what set the delivery's current
      state. The failure row is keyed by the event and written by _withhold in the same
      transaction as the delivery's transition, with the stamp that transition wrote as
      updated_at; every other writer of a delivery row sets its own updated_at, so a later
      transition of any kind (a settings withhold, a busy deferral, a claim, a settle, a
      recovery, a pacing reschedule, a pause) ends the match. It is the reason that withhold
      recorded, archived, paused or lifecycle_unknown among them, with its time;
    - a refusal, only while no delivery row exists: once one exists the refusal that preceded
      it is no longer why anything is undelivered.

    A settings or send-path withhold is not explained here; status reads those.
    """
    delivered = row["delivered"] is not None
    state = row["delivery_state"]
    if delivered and row["hold_reason"]:
        return {"source": "deliveries.hold_reason", "value": row["hold_reason"]}
    if (delivered and state in NOT_SENT_STATES and row["relationship_status"] is not None
            and row["relationship_status"] != "active" and row["superseded_by"] is None):
        return {"source": "relationships.status",
                "value": RefusalReason.RELATIONSHIP_NOT_ACTIVE.value,
                "relationshipStatus": row["relationship_status"]}
    if delivered and state == WITHHELD_PRE_SEND and row["lifecycle_withhold"] is not None:
        return {"source": LIFECYCLE_WITHHOLD_SOURCE, "value": row["lifecycle_withhold"],
                "recordedAt": row["lifecycle_recorded_at"],
                "nextRetryAt": row["lifecycle_next_retry_at"]}
    if delivered:
        return None
    if row["refusal_reason"] is not None:
        return {"source": "refusals.reason", "value": row["refusal_reason"]}
    return None


def correction_next_action(state, projection):
    """The next action for needs_changes, from the correction the same projection read.

    None leaves NEXT_ACTION's answer. A correction event always has its delivery row, because
    ack.record_verdict inserts the event and queues it in one transaction. A correction with a
    supersession note, or one the note has already been applied to, was answered by a final
    event of its generation; that is checked first, because the note outranks the delivery
    state (the state is left alone so reconciliation can still settle an outstanding send). A
    state this rule has no word for leaves NEXT_ACTION's answer.
    """
    correction = projection["correction"]
    delivery = correction["delivery"]
    if state != NEEDS_CHANGES or delivery is None:
        return None
    if correction.get("supersession") is not None or delivery["state"] == SUPERSEDED:
        return CORRECTION_ANSWERED_ACTION
    if delivery["state"] in REACHED_STATES:
        return None
    reason = correction["undeliveredReason"] or {}
    if delivery["state"] == INBOX_ONLY or reason.get("source") == "deliveries.hold_reason":
        return CORRECTION_HELD_ACTION
    if delivery["state"] in UNCONFIRMED_STATES:
        return CORRECTION_UNCONFIRMED_ACTION
    if delivery["state"] in NOT_SENT_STATES:
        return CORRECTION_UNSENT_ACTION
    return None


class AssignmentView:
    def __init__(self, store, registry, clock, *, criteria=None, linkage=None, policy=None):
        self.store = store
        self.registry = registry
        self.clock = clock
        if criteria is None:
            from .criteria import CriteriaService

            criteria = CriteriaService(store, clock)
        self.criteria = criteria

        # Optional, and read-only. linkage.outstanding() already derives its unfinished set
        # through this class's own state(), so the project-level reading below is the same
        # derivation lifted one level rather than a second opinion about it. Absent, the
        # project reading says it cannot answer instead of guessing.
        self.linkage = linkage
        # The send budget a delivery's pacing is read against (CRW-231). The same default every
        # other reader of the budget uses when none is configured.
        if policy is None:
            from .policy import RetryPolicy

            policy = RetryPolicy()
        self.policy = policy

    def project_state(self, project_key: str) -> dict:
        """What an approved project scope's children actually say, as a reading not a verdict.

        A parent's completion is computed from its children's real state, so this returns the
        unfinished set and the basis for it and stops there. It never returns "complete": the
        strongest thing it says is complete_candidate, because integration and verification are
        the parent's judgment and this class cannot see them.

        The four answers linkage keeps apart are kept apart here, and none of them is completion:

        unreadable    the store did not answer. Not "nothing outstanding".
        unregistered  the project has no live attached assignment at all. Not "all done".
        ambiguous     more than one candidate owner. The reader will not choose.
        incomplete    at least one live assignment is unfinished, named row by row.

        Partial Done cannot close a parent, because complete_candidate requires the outstanding
        set to be EMPTY rather than small. A shared project cannot close one either: attached()
        counts every live assignment in the project, so work parked on another parent still
        counts. And a child's own goal being complete is not consulted at all - state() is
        derived from receipts, verdicts and marks against the current head, generation and
        revision, so a goal a child closed on its own is not evidence here.
        """
        if self.linkage is None:
            return {"state": "unreadable", "readable": False, "projectKey": project_key,
                    "attached": [], "outstanding": [],
                    "basis": "no linkage reader was supplied, so the project's assignments "
                             "could not be enumerated",
                    "limits": PROJECT_READING_LIMITS}
        try:
            attached = list(self.linkage.attached(project_key))
            outstanding = list(self.linkage.outstanding(project_key))
            # Inside the boundary, not after it. Owner enumeration reads the same store and is
            # required to CLASSIFY the answer, so a failure here has to produce the documented
            # unreadable shape rather than propagate a database error to a caller that asked a
            # question about completion.
            owners = self.linkage.owners(PROJECT_SCOPE_KIND, project_key)
        except Exception as error:  # noqa: BLE001 - an unreadable store is an answer, not a crash
            return {"state": "unreadable", "readable": False, "projectKey": project_key,
                    "attached": [], "outstanding": [],
                    "basis": f"the project's assignments could not be read: "
                             f"{type(error).__name__}: {error}",
                    "limits": PROJECT_READING_LIMITS}
        if len(owners) > 1:
            return {"state": "ambiguous", "readable": True, "projectKey": project_key,
                    "attached": attached, "outstanding": outstanding,
                    "competingOwners": sorted(o["taskId"] for o in owners),
                    "basis": "the project has more than one live owner, so which parent this "
                             "reading is about is not decided here",
                    "limits": PROJECT_READING_LIMITS}
        if not attached:
            return {"state": "unregistered", "readable": True, "projectKey": project_key,
                    "attached": [], "outstanding": [],
                    "basis": "no live assignment is attached to this project, which is not the "
                             "same as every assignment being finished",
                    "limits": PROJECT_READING_LIMITS}
        # Expanded here, AFTER the two classifications that owners() and attached() already
        # settle, and inside a boundary of its own. outstanding() returns ids; naming each one's
        # state reads the registry and store again, so a failure there still has to produce the
        # promised unreadable reading rather than raise at a caller asking about completion.
        # But it must not run any earlier: ambiguity is establishable from owners() alone, and
        # expanding first let an unrelated state-read failure replace a definite ambiguous
        # answer with unreadable - masking the more specific fact with the vaguer one.
        try:
            unfinished = [{"relationshipId": rid, "state": self.state(rid)["state"]}
                          for rid in outstanding]
        except Exception as error:  # noqa: BLE001 - same reason as the boundary above
            return {"state": "unreadable", "readable": False, "projectKey": project_key,
                    "attached": attached, "outstanding": outstanding,
                    "basis": f"the unfinished set could not be expanded: "
                             f"{type(error).__name__}: {error}",
                    "limits": PROJECT_READING_LIMITS}
        if unfinished:
            return {"state": "incomplete", "readable": True, "projectKey": project_key,
                    "attached": attached, "outstanding": outstanding,
                    "unfinished": unfinished,
                    "basis": f"{len(unfinished)} of {len(attached)} live assignments are "
                             "unfinished",
                    "limits": PROJECT_READING_LIMITS}
        return {"state": "complete_candidate", "readable": True, "projectKey": project_key,
                "attached": attached, "outstanding": [], "unfinished": [],
                "basis": f"all {len(attached)} live assignments in this project report a "
                         "finished state",
                "limits": PROJECT_READING_LIMITS}

    # -------------------------------------------------------------------- read

    def state(self, relationship_id: str) -> dict:
        relationship = self.registry.get(relationship_id)
        row = self.store.one(
            "SELECT * FROM relationships WHERE relationship_id = ?", (relationship_id,)
        )
        generation = relationship["executionGeneration"]
        head = head_revision(self.store.db, relationship_id, generation)
        verdict = self._verdict_for(head["eventId"]) if head["eventId"] else None
        current_digest = self._current_digest(relationship_id)
        criteria_current = self._criteria_current(verdict, current_digest)
        marks = self._marks(relationship_id)
        current_mark = (
            self._current_mark(marks, head, generation, verdict) if criteria_current else None
        )

        state = self._resolve(
            row, head, verdict, relationship_id, generation, current_mark, criteria_current
        )
        record = {
            "relationshipId": relationship_id,
            "issueKey": relationship["issueKey"],
            "parentTaskId": relationship["parent"]["taskId"],
            "childTaskId": relationship["child"]["taskId"],
            "relationshipStatus": relationship["status"],
            "state": state,
            "executionGeneration": generation,
            "head": {
                "eventId": head["eventId"],
                "revisionHash": head["revisionHash"],
                "evidence": head["evidence"],
                "competitors": head["competitors"],
                "detail": head["detail"],
            },
            "lastVerdict": verdict,
            "mark": current_mark,
            # Every mark that no longer describes the current head. Retained, and never state.
            "markHistory": [m for m in marks if m is not current_mark],
            "nextExpectedAction": NEXT_ACTION.get(state, "none"),
        }
        registered = self.criteria.get(relationship_id)
        record["criteria"] = {
            "mode": self.criteria.mode(relationship_id),
            "setDigest": current_digest,
            "registered": len(registered["criteria"]) if registered else 0,
            # The set the current verdict was actually decided against. When it differs from
            # the registered one, that verdict certifies wording nobody is judging by any more.
            "reviewedSetDigest": verdict["setDigest"] if verdict else None,
            "current": criteria_current,
        }
        record["projection"] = self._projection(
            relationship_id, generation, head, verdict, state
        )
        record["nextExpectedAction"] = (
            # Disjoint states: the correction's answer is for needs_changes (CRW-222), the
            # completion's for received, corrected and verifying (CRW-224); NEXT_ACTION answers
            # the rest.
            correction_next_action(state, record["projection"])
            or completion_next_action(state, record["projection"])
            or record["nextExpectedAction"]
        )
        recovery = parent_recovery(record["nextExpectedAction"], record["projection"])
        if recovery is not None:
            record["recovery"] = recovery
        return record

    def _projection(self, relationship_id, generation, head, verdict, state) -> dict:
        """Five vocabularies, kept apart, each anchored to the event it was read under.

        They are not interchangeable and collapsing any two loses the distinction a caller
        needs. `staged` is an EVENT stage and never a delivery state; `acknowledged` IS a
        delivery state; and an acknowledgement settles on one axis while carrying its evidence
        on another, so a bridge receipt saying an attempt was accepted still says nothing about
        receipt, application or verification.

        Anchored, because an unanchored read pairs whatever each table happens to hold. After a
        needs_changes verdict the assignment has a completion in the previous generation and a
        queued correction in the current one; reporting the old acknowledgement beside the new
        delivery would describe a state that never existed. The two get separate anchors rather
        than one, because head_revision only ever names a ready_for_review event - so while the
        correction is the thing everyone is waiting for, it would otherwise be invisible here.
        """
        correction = self.store.one(
            "SELECT event_id FROM events"
            " WHERE relationship_id = ? AND execution_generation = ?"
            "   AND outcome = 'revision_request' AND suppressed_reason IS NULL"
            " ORDER BY event_id LIMIT 1",
            (relationship_id, generation),
        )
        return {
            "completion": self._anchored(head["eventId"], generation),
            "correction": self._anchored(
                correction["event_id"] if correction else None, generation
            ),
            # Referenced, never recomputed. state() derived both of these above and a second
            # derivation here would be a second source of truth for one fact.
            "verdict": verdict,
            "assignment": {"state": state},
        }

    def _anchored(self, event_id, generation) -> dict:
        record = {"eventId": event_id, "executionGeneration": generation,
                  "event": None, "delivery": None, "ack": None, "undeliveredReason": None,
                  "supersession": None}
        if event_id is None:
            # A null is an answer. A row borrowed from another generation is not.
            record["detail"] = "this generation has no such event"
            return record
        # ONE statement, so ONE snapshot. SQLite gives every autocommit SELECT its own, and
        # reading the event, the delivery, the attempt and the acknowledgement separately lets
        # a delivery worker commit in between: the answer would then pair a delivery state with
        # an acknowledgement that never coexisted with it, which is exactly the combination
        # this projection exists to make impossible. The attempt joins on attempt_count so the
        # request id is the CURRENT attempt's, not whichever row sorted first after a retry.
        now = self.clock.now()
        window, earliest = self.policy.rate_windows(now)
        row = self.store.one(
            "SELECT e.stage AS stage,"
            "       d.event_id AS delivered, d.state AS delivery_state,"
            "       d.hold_reason AS hold_reason, d.dispatch_evidence AS dispatch_evidence,"
            "       a.request_id AS request_id, a.attempt_no AS attempt_no,"
            "       a.state AS attempt_state, a.recipient_scan AS attempt_turn_check,"
            "       v.last_reason AS ack_last_reason,"
            "       (SELECT COUNT(*) FROM attempts h WHERE h.event_id = e.event_id"
            "         AND h.state = 'host_lost_turn') AS host_lost_attempts,"
            "       (SELECT COUNT(*) FROM attempts u WHERE u.event_id = e.event_id"
            "         AND u.state = 'unknown_send_lost') AS unknown_lost_attempts,"
            # The recipient's send budget, read in the same snapshot as the delivery it paces
            # (delivery.send_pacing reads the same two rows; RetryPolicy.pacing judges them).
            "       (SELECT sends FROM recipient_rate WHERE recipient_task_id = d.recipient_task_id"
            "         AND window_start = ?) AS rate_sends,"
            "       (SELECT MAX(last_send_at) FROM recipient_rate"
            "         WHERE recipient_task_id = d.recipient_task_id"
            "           AND window_start BETWEEN ? AND ?) AS rate_last,"
            "       k.event_id AS acked, k.verified AS ack_verified,"
            "       k.accepted AS ack_accepted, k.rejection_reason AS ack_rejection,"
            "       v.tier AS ack_tier,"
            "       r.status AS relationship_status, r.superseded_by AS superseded_by,"
            "       lf.error_code AS lifecycle_withhold,"
            "       lf.occurred_at AS lifecycle_recorded_at,"
            "       lf.next_retry_at AS lifecycle_next_retry_at,"
            "       sx.reason AS supersession_reason, sx.applied AS supersession_applied,"
            "       (SELECT reason FROM refusals WHERE event_id = e.event_id"
            "         ORDER BY id DESC LIMIT 1) AS refusal_reason"
            "  FROM events e"
            "  LEFT JOIN deliveries d ON d.event_id = e.event_id"
            "  LEFT JOIN attempts a ON a.event_id = e.event_id"
            "                      AND a.attempt_no = d.attempt_count"
            "  LEFT JOIN acks k ON k.event_id = e.event_id"
            "  LEFT JOIN ack_evidence v ON v.event_id = e.event_id"
            "  LEFT JOIN delivery_supersession sx ON sx.event_id = e.event_id"
            "  LEFT JOIN relationships r ON r.relationship_id = e.relationship_id"
            + LIFECYCLE_WITHHOLD_JOIN.format(alias="lf", event="e", delivery="d") +
            " WHERE e.event_id = ?",
            (window, earliest, window, event_id),
        )
        if row is None:
            record["detail"] = "the store holds no such event"
            return record
        record["event"] = {"stage": row["stage"]}
        if row["delivered"] is not None:
            record["delivery"] = {
                "state": row["delivery_state"],
                "requestId": row["request_id"],
                "attemptNo": row["attempt_no"],
                # The current attempt's own state, the delivery's evidence and hold, and how many
                # of this event's attempts the host lost (hostloss.py). The count outlives the
                # redelivery, so a completion that reached its parent the second time still says
                # the first turn was lost.
                "attemptState": row["attempt_state"],
                "dispatchEvidence": row["dispatch_evidence"],
                "holdReason": row["hold_reason"],
                "hostLostAttempts": row["host_lost_attempts"],
                # How many of this event's uncertain sends the recipient kept no trace of
                # (hostloss.read_unknown_send, CRW-231). Like the count above, it outlives the
                # redelivery.
                "unknownSendLostAttempts": row["unknown_lost_attempts"],
                # The current attempt's recipient check, when it could not decide: the turn
                # check's turn_check_undecided:<reason>, or an uncertain send's
                # unknown_send_undecided:<reason>; None otherwise.
                "turnCheck": (row["attempt_turn_check"]
                              if (row["attempt_turn_check"] or "").startswith(UNDECIDED_CHECKS)
                              else None),
                # Why an unsent delivery waits on its recipient's send budget, and when that
                # reopens (RetryPolicy.pacing); None when the budget is not what holds it.
                "pacing": (self.policy.pacing(now, sends=row["rate_sends"],
                                              last=row["rate_last"])
                           if row["delivery_state"] in NOT_SENT_STATES and not row["hold_reason"]
                           else None),
            }
        record["ack"] = {
            # The parent's DISPOSITION, independent of whether the acknowledging turn could be
            # verified. A verified rejection and a verified acceptance settle identically on
            # the axis below, so reading only that one reports them alike.
            "accepted": bool(row["ack_accepted"]) if row["acked"] is not None else None,
            "rejectionReason": row["ack_rejection"] if row["acked"] is not None else None,
            "settlement": row["ack_verified"] if row["acked"] is not None else None,
            # Why verification last declined to promote it, when it did (ack_evidence).
            "lastReason": row["ack_last_reason"] if row["acked"] is not None else None,
            # A second axis, not a rewording of the first: settlement says whether the
            # acknowledgement closed, the tier says what it closed on, and no row at all is
            # unrecorded rather than unverified.
            "evidenceTier": row["ack_tier"] if row["ack_tier"] is not None else "unrecorded",
        }
        record["undeliveredReason"] = undelivered_reason(row)
        # A newer final event of the same generation replaced this delivery. Reported beside the
        # delivery state, which is left alone so reconciliation can still settle an outstanding
        # send; the note comes from the same statement as the state it annotates.
        record["supersession"] = (
            {"reason": row["supersession_reason"], "applied": bool(row["supersession_applied"])}
            if row["supersession_reason"] is not None else None
        )
        return record

    def _resolve(self, row, head, verdict, relationship_id, generation, current_mark,
                 criteria_current=True) -> str:
        if row["status"] == "cancelled":
            return ABANDONED
        if row["status"] == "archived" or row["superseded_by"]:
            return CLOSED
        if row["status"] == "paused":
            # A paused assignment still owns its child and is not a completed one. Showing an
            # older verified here would hide the very thing an operator needs to see.
            return PAUSED
        if head["evidence"] in AMBIGUOUS:
            # Ambiguity is observable rather than masked by whatever the last verdict said.
            return AMBIGUOUS_STATE
        if verdict is not None and verdict["verdict"] == VERIFIED and not criteria_current:
            # The criteria changed after this revision was certified. The verdict is real
            # history, but it certified wording nobody is judging by now, so it is not a
            # current completion and it is not something to integrate.
            return REREVIEW_NEEDED
        if current_mark is not None:
            return current_mark["mark"]
        if verdict is not None and verdict["verdict"] == VERIFIED:
            return VERIFIED
        if head["eventId"] is not None:
            previous = self._previous_needs_changes(relationship_id, generation)
            if previous is not None:
                return CORRECTED
            if self._claimed(head["eventId"]):
                return VERIFYING
            return RECEIVED
        if self._previous_needs_changes(relationship_id, generation) is not None:
            return NEEDS_CHANGES
        if self._any_receipt(relationship_id):
            return RECEIVED
        return REQUESTED

    def for_issue(self, issue_key: str) -> dict:
        """What linear-run asks BEFORE creating anything."""
        rows = self.store.all(
            "SELECT relationship_id FROM relationships WHERE issue_key = ?"
            " ORDER BY created_at",
            (issue_key,),
        )
        assignments = [self.state(row["relationship_id"]) for row in rows]
        owning = [
            a for a in assignments
            if a["relationshipStatus"] in ("active", "paused") and a["state"] != CLOSED
        ]
        record = {
            "issueKey": issue_key,
            "assignments": assignments,
            # The one to reuse, if there is one. A paused assignment is still the owner.
            "responsibleChild": owning[0]["childTaskId"] if owning else None,
            "responsibleRelationship": owning[0]["relationshipId"] if owning else None,
        }
        record.update(self._project_context(owning))
        record["relay"] = self._relay_provenance(bool(owning))
        return record

    def _relay_provenance(self, holds: bool) -> dict:
        """Which store this answer came from, so it can be compared with the packet's.

        for_issue can answer "responsibleRelationship: null" perfectly honestly and still be
        the wrong answer, because a mistyped state directory creates an empty store and an
        empty store has no assignments. OPS-3.4 makes the proof a conjunction - doctor
        reporting the packet's state directory AND this lookup naming the expected
        relationship - and a reading that cannot say which file it read cannot take part in
        that comparison. Carrying the provenance is what lets the caller notice it asked the
        wrong store instead of concluding the issue is unowned and opening a second writer.

        holds is the predicate itself, stated once here rather than re-derived by every
        caller from the shape of responsibleRelationship. It is scoped to THIS store, which
        is the only honest scope for the claim.

        These values are provenance, never proof. A copy of the store carries the same store
        id and the same recorded socket, which is why compare_store grades a found nonce
        against device and inode instead of trusting an identifier. The field name says
        recorded for the same reason: this is the value to compare, not the evidence that
        settles the comparison.
        """
        # The id is read through the connection this process opened; the device and inode are
        # stat-ed from the path. Those are two different files if the path is replaced in
        # between, and pairing them would hand out provenance no single store ever had. So the
        # path is identified on both sides of the read and a disagreement returns unknown
        # rather than a hybrid.
        #
        # This leg still measures AT THE PATH, so it keeps what that can promise and no more:
        # it catches a replacement that persists past the read and misses one reverted inside
        # the window, because both observations then report the original inode. read_only_rows
        # and nonce_lookup no longer work this way - they hold the file open and identify it by
        # that descriptor - but this one reads through a live Store whose connection owns the
        # only descriptor there is, and sqlite3 exposes none. Moving it means giving Store a
        # held descriptor, which is the whole write path rather than a diagnostic read.
        before = self._path_identity()
        located = self.store.locate()
        recorded_socket = self.store.meta("socket_path")
        after = self._path_identity()
        if before is None or after is None or before != after:
            return {
                "holds": holds,
                "store": {"identified": False, "storeId": None, "dbPath": located["dbPath"],
                          "realPath": None, "device": None, "inode": None,
                          "recordedSocket": None,
                          "detail": "the database at this path was replaced while it was read"},
            }
        return {
            "holds": holds,
            "store": {
                "identified": True,
                "storeId": located["storeId"],
                "dbPath": located["dbPath"],
                "realPath": located["realPath"],
                "device": located["device"],
                "inode": located["inode"],
                # First-write-wins provenance out of schema_meta, and deliberately NOT the
                # socket this process resolved: a store records the socket that created it,
                # so a participant pointing at a different socket still reads this value and
                # can see that the two disagree.
                "recordedSocket": recorded_socket,
                "detail": None,
            },
        }

    def _path_identity(self):
        import os

        try:
            info = os.stat(self.store.path)
        except OSError:
            return None
        return (info.st_dev, info.st_ino)

    def _project_context(self, owning) -> dict:
        """Which project owns this issue, additively.

        scopeState tells the three answers apart. scoped means the store named a project,
        unscoped means it answered that there is none - which is every assignment registered
        before the three-level linkage existed, and a normal answer rather than an error - and
        unreadable means the store did not answer at all. A caller can distinguish them, and
        none of them is completion.
        """
        import sqlite3

        blank = {"projectKey": None, "projectParentTaskId": None, "scopeState": "unscoped"}
        if not owning:
            return blank
        try:
            scoped = self.store.one(
                "SELECT project_key FROM relationship_scope WHERE relationship_id = ?",
                (owning[0]["relationshipId"],),
            )
            if scoped is None:
                return blank
            from .linkage import Linkage, PARENT as PARENT_ROLE, PROJECT as PROJECT_SCOPE

            held = Linkage(self.store, self.clock).owners(
                PROJECT_SCOPE, scoped["project_key"])
            parent_task = owning[0]["parentTaskId"]
            if len(held) > 1:
                # Two live owners, which only a store whose unique index could not be
                # installed can hold. This view is what a coordinator reads before acting on
                # an issue, so naming whichever sorted first would hand it a guessed parent
                # wearing the same shape as a known one. The contest is the answer.
                return {
                    "projectKey": scoped["project_key"],
                    "projectParentTaskId": None,
                    "scopeState": "ambiguous",
                    "projectParentCandidates": sorted(
                        record["taskId"] for record in held),
                    "parentOwnsProject": None,
                }
            holder = held[0] if held else None
            return {
                "projectKey": scoped["project_key"],
                "projectParentTaskId": holder["taskId"] if holder else None,
                "scopeState": "scoped",
                # A parent handover moves the SCOPE and not the assignments under it, so these
                # two can legitimately disagree. Surfaced here rather than left to be noticed,
                # because this view is what a coordinator reads before acting on an issue: the
                # assignment still answers to the parent named on its own row, and the project
                # is owned by somebody else.
                "parentOwnsProject": (holder is not None
                                      and holder["taskId"] == parent_task),
            }
        except sqlite3.Error:
            return {"projectKey": None, "projectParentTaskId": None,
                    "scopeState": "unreadable"}

    # ------------------------------------------------------------------- write

    def mark(self, relationship_id: str, mark: str, *, evidence: str, actor: str,
             expected_event: str) -> dict:
        """Record the one assignment fact the relay cannot observe for itself.

        The caller states WHICH revision it integrated, and that is compared with the current
        verified head inside this write transaction. Without it, an operator who integrated an
        older revision would silently bind the merge to whatever became current in between,
        which is the opposite of a record. An event id is enough on its own: it already pins
        the generation and the revision hash.

        Every read that decides the outcome happens inside the transaction, for the same reason
        the verdict path was changed: a preflight read can be raced.
        """
        from .errors import AckRefused, RefusalReason

        if mark not in MARKS:
            raise AckRefused(RefusalReason.DISPOSITION_CONFLICT, f"unknown mark {mark!r}")
        if not str(evidence or "").strip():
            raise AckRefused(RefusalReason.FINDINGS_REQUIRED, "a mark carries its evidence")
        if not str(expected_event or "").strip():
            raise AckRefused(
                RefusalReason.STALE_MARK_CONTEXT,
                "a mark names the exact event it integrated",
            )
        now = self.clock.iso()
        with self.store.transaction() as db:
            row = db.execute(
                "SELECT * FROM relationships WHERE relationship_id = ?", (relationship_id,)
            ).fetchone()
            if row is None:
                raise AckRefused(
                    RefusalReason.UNREGISTERED_RELATIONSHIP, f"no relationship {relationship_id!r}"
                )
            if row["status"] != "active" or row["superseded_by"]:
                raise AckRefused(
                    RefusalReason.RELATIONSHIP_NOT_ACTIVE,
                    f"relationship {relationship_id!r} is {row['status']!r}",
                )
            generation = row["execution_generation"]
            head = head_revision(db, relationship_id, generation)
            if head["eventId"] is None or head["evidence"] in AMBIGUOUS:
                raise AckRefused(
                    RefusalReason.REVISION_AMBIGUOUS,
                    f"generation {generation} has no single current revision to mark: "
                    f"{head['detail'] or head['evidence']}",
                )
            if head["eventId"] != expected_event:
                raise AckRefused(
                    RefusalReason.STALE_MARK_CONTEXT,
                    f"this mark names {expected_event!r}, but the current revision of "
                    f"generation {generation} is {head['eventId']!r}; re-read the assignment "
                    "before recording what was integrated",
                )
            verdict = self._verdict_for(head["eventId"])
            if verdict is None or verdict["verdict"] != VERIFIED:
                raise AckRefused(
                    RefusalReason.NOT_ACKNOWLEDGED,
                    f"the current revision of generation {generation} is not verified, so it "
                    "cannot be marked merged",
                )
            current_digest = self._current_digest(relationship_id)
            if not self._criteria_current(verdict, current_digest):
                raise AckRefused(
                    RefusalReason.CRITERIA_SET_CHANGED,
                    "the canonical criteria changed after this revision was verified, so it "
                    "needs re-review before it can be recorded as integrated",
                )
            db.execute(
                "INSERT INTO assignment_marks (relationship_id, mark, event_id,"
                " execution_generation, revision_hash, evidence, actor, marked_at)"
                " VALUES (?,?,?,?,?,?,?,?)"
                " ON CONFLICT(relationship_id, mark, event_id) DO UPDATE SET"
                "   evidence = excluded.evidence, actor = excluded.actor,"
                "   marked_at = excluded.marked_at",
                (
                    relationship_id, mark, head["eventId"], generation, head["revisionHash"],
                    evidence, actor, now,
                ),
            )
            self.store.journal(
                "assignment_marked", relationship_id,
                {"mark": mark, "eventId": head["eventId"], "generation": generation}, at=now,
            )
        return self.state(relationship_id)

    # ------------------------------------------------------------------ pieces

    def _marks(self, relationship_id) -> list:
        return self._marks_from(self.store, relationship_id)

    def _current_digest(self, relationship_id):
        registered = self.criteria.get(relationship_id)
        return registered["setDigest"] if registered else None

    @staticmethod
    def _criteria_current(verdict, current_digest) -> bool:
        """Was this verdict decided against the criteria now in force?

        A verdict carries the digest of the set it actually ruled on. When that differs from
        the registered one, the verdict is history rather than a current completion: findings
        made against one wording do not certify another, even when the ids are identical.
        """
        if verdict is None:
            return True
        return verdict.get("setDigest") == current_digest

    @staticmethod
    def _marks_from(store, relationship_id) -> list:
        return [
            {
                "mark": row["mark"], "eventId": row["event_id"],
                "executionGeneration": row["execution_generation"],
                "revisionHash": row["revision_hash"], "evidence": row["evidence"],
                "actor": row["actor"], "markedAt": row["marked_at"],
            }
            for row in store.all(
                "SELECT * FROM assignment_marks WHERE relationship_id = ?"
                " ORDER BY marked_at",
                (relationship_id,),
            )
        ]

    @staticmethod
    def _current_mark(marks, head, generation, verdict):
        """A mark counts as state only where it describes the CURRENT verified head."""
        if head["eventId"] is None or verdict is None or verdict["verdict"] != VERIFIED:
            return None
        for candidate in marks:
            if (candidate["eventId"] == head["eventId"]
                    and candidate["executionGeneration"] == generation
                    and candidate["revisionHash"] == head["revisionHash"]):
                return candidate
        return None

    def _verdict_for(self, event_id):
        row = self.store.one("SELECT * FROM verdicts WHERE event_id = ?", (event_id,))
        if row is None:
            return None
        record = json.loads(row["record"])
        context = self.store.one(
            "SELECT set_digest, coverage FROM verdict_context WHERE event_id = ?", (event_id,)
        )
        return {
            "verdict": row["verdict"], "eventId": event_id,
            "executionGeneration": record.get("executionGeneration"),
            "nextExecutionGeneration": row["next_generation"],
            "decidedAt": row["decided_at"],
            "setDigest": context["set_digest"] if context else None,
            "coverage": context["coverage"] if context else None,
        }

    def _claimed(self, event_id) -> bool:
        return self.store.one(
            "SELECT 1 FROM verification_claims WHERE event_id = ?", (event_id,)
        ) is not None

    def _previous_needs_changes(self, relationship_id, generation):
        return self.store.one(
            "SELECT v.event_id FROM verdicts v JOIN events e ON e.event_id = v.event_id"
            " WHERE e.relationship_id = ? AND e.execution_generation < ?"
            "   AND v.verdict = 'needs_changes'"
            " ORDER BY e.execution_generation DESC LIMIT 1",
            (relationship_id, generation),
        )

    def _any_receipt(self, relationship_id) -> bool:
        return self.store.one(
            "SELECT 1 FROM events WHERE relationship_id = ? AND outcome = 'ready_for_review'",
            (relationship_id,),
        ) is not None

# What completion_next_action answers. Each names the actor that moves the obligation next.
AWAITING_ACK_ACTION = "parent_acknowledges"
VERIFY_ACK_ACTION = "daemon_verifies_acknowledgement"
REACKNOWLEDGE_ACTION = "parent_reacknowledges"
RECONCILE_ACTION = "daemon_reconciles_delivery"
HOST_LOST_REDELIVERY_ACTION = "daemon_redelivers_host_lost_turn"
HOST_LOST_HELD_ACTION = "parent_recovers_host_lost_turn"
# An uncertain send the recipient kept no trace of (hostloss.read_unknown_send, CRW-231): sent once
# more by the daemon, held for the parent after a second loss, or held for the parent when no wait
# can decide it.
UNKNOWN_SEND_REDELIVERY_ACTION = "daemon_redelivers_unknown_send_lost"
UNKNOWN_SEND_HELD_ACTION = "parent_recovers_unknown_send_lost"
UNKNOWN_SEND_UNDECIDED_ACTION = "parent_recovers_unknown_send_undecided"
# What the parent does to recover a completion nothing will deliver automatically any more. The
# report is in this store whatever happened to the message, so the parent reads it here and, if
# the work still needs verifying, opens a fresh execution generation.
PARENT_RECOVERY_ACTIONS = (HOST_LOST_HELD_ACTION, UNKNOWN_SEND_HELD_ACTION,
                           UNKNOWN_SEND_UNDECIDED_ACTION)
PARENT_RECOVERY_THEN = ("read the report, then open a fresh execution generation"
                        " (generation-open) if the work still needs verifying")
# The recipient-check names a current attempt can carry (hostloss): the turn check's, and an
# uncertain send's.
UNDECIDED_CHECKS = ("turn_check_undecided:", "unknown_send_undecided:")
# Verification's own "not yet": the host has not confirmed the acknowledging turn, and a process
# with host access will try again; or the acknowledgement was kept while the relay had not
# confirmed the delivery (ack.DELIVERY_UNCONFIRMED), and the daemon completes it now that it has.
# Any other reason it recorded is a refusal only a new acknowledgement can answer.
_VERIFICATION_PENDING = (None, "unverified_turn", "delivery_unconfirmed")


def completion_next_action(state, projection):
    """Who moves an unanswered completion next, read from its delivery row (CRW-224, H0-O1).

    NEXT_ACTION answers from the assignment state alone, so a completion already delivered and
    waiting for the parent's acknowledgement read daemon_delivers. This answers from the
    completion's own delivery row and acknowledgement, which the one projection statement read
    together, for the states in which the parent has not ruled on the report: received and
    corrected, and verifying - claimed, which is not acknowledged: a parent can claim inside a
    delivery turn the relay has not confirmed, and its acknowledgement then waits on the relay
    (CRW-124 R3, H0R3-F2). First match:

    - acknowledged: an accepted one waits for the parent to verify; a rejection is an answer and
      is left to NEXT_ACTION, as is every acknowledged claim (parent_verifies);
    - held, other than a closed push channel: nothing moves it automatically. A hold named for a
      loss is the parent's to recover under that loss's name (host_lost_turn, unknown_send_lost,
      unknown_send_undecided, CRW-231); any other hold after a loss is the parent's under the loss
      the event recorded, a host loss first; a completion never lost keeps today's answer;
    - held_uncertain or sending: an uncertain send or an unsettled claim, which reconciliation
      settles; it is not a send;
    - dispatched or inbox_only with an acknowledgement recorded: verification's, unless
      verification refused it, which only a new acknowledgement answers;
    - dispatched or inbox_only: the parent's to acknowledge;
    - queued, deferred or withheld: the relay's to send, and after a loss its to send again, named
      for the loss (a host loss first).

    None leaves NEXT_ACTION's answer.
    """
    from .policy import (
        HOST_LOST_TURN, PUSH_CHANNEL_CLOSED, UNKNOWN_SEND_LOST, UNKNOWN_SEND_UNDECIDED,
    )
    from .transport import (
        DEFERRED_BUSY, DISPATCHED, HELD_UNCERTAIN, INBOX_ONLY, QUEUED, SENDING, WITHHELD_PRE_SEND,
    )

    if state not in (RECEIVED, CORRECTED, VERIFYING):
        return None
    completion = projection["completion"]
    delivery = completion["delivery"]
    if delivery is None:
        return None
    ack = completion["ack"] or {}
    host_lost = (delivery.get("hostLostAttempts") or 0) > 0
    unknown_lost = (delivery.get("unknownSendLostAttempts") or 0) > 0
    settlement = ack.get("settlement")
    if settlement == "verified":
        return NEXT_ACTION[VERIFYING] if state == RECEIVED and ack.get("accepted") else None
    hold = delivery.get("holdReason")
    if hold and hold != PUSH_CHANNEL_CLOSED:
        named = {HOST_LOST_TURN: HOST_LOST_HELD_ACTION,
                 UNKNOWN_SEND_LOST: UNKNOWN_SEND_HELD_ACTION,
                 UNKNOWN_SEND_UNDECIDED: UNKNOWN_SEND_UNDECIDED_ACTION}.get(hold)
        if named is not None:
            return named
        if host_lost:
            return HOST_LOST_HELD_ACTION
        return UNKNOWN_SEND_HELD_ACTION if unknown_lost else None
    where = delivery["state"]
    if where in (HELD_UNCERTAIN, SENDING):
        return RECONCILE_ACTION
    if where in (DISPATCHED, INBOX_ONLY):
        if settlement is not None:
            if ack.get("lastReason") in _VERIFICATION_PENDING:
                return VERIFY_ACK_ACTION
            return REACKNOWLEDGE_ACTION
        return AWAITING_ACK_ACTION
    if where in (QUEUED, DEFERRED_BUSY, WITHHELD_PRE_SEND):
        if host_lost:
            return HOST_LOST_REDELIVERY_ACTION
        return UNKNOWN_SEND_REDELIVERY_ACTION if unknown_lost else NEXT_ACTION[RECEIVED]
    return None


def parent_recovery(action, projection):
    """How the parent recovers a completion nothing will deliver automatically any more, or None.

    Named beside nextExpectedAction so the actor comes with the command that supports it (CRW-231
    criterion 1): the report is kept in this store whatever happened to the message, so show
    --event reads it; a fresh generation is the parent's next step when the work still needs
    verifying.
    """
    if action not in PARENT_RECOVERY_ACTIONS:
        return None
    completion = projection["completion"]
    delivery = completion["delivery"] or {}
    return {
        "actor": "parent",
        "reason": delivery.get("holdReason"),
        "command": f"codex-session-relay show --event {completion['eventId']}",
        "then": PARENT_RECOVERY_THEN,
    }
