"""What each live child's current turn did, and whether the store observed it being sent.

AssignmentView answers about ready_for_review and nothing else: _any_receipt queries that outcome
literally and currency.head_revision filters on it. So a child can record blocked_needs_input and no
reader lists it, while a coordinator reading the assignment view concludes nothing is wrong.
Separately, whether such an event was ever DELIVERED was recorded nowhere: DeliveryService.snapshot
shows the deliveries and intents that exist, and the case it cannot show - neither row - is exactly
the silent one.

Two axes, kept apart on purpose.

The SEND axis is derived from records the relay itself wrote, and its narrowest value is unmeasured:
a final event with no delivery row and no intent row. Absence cannot be narrowed further, because it
covers an event emitted with --no-enqueue, an event stranded by an old generation, and a refusal
recorded before delivery intents existed. Merging those into "not delivered" is the claim this
module refuses to make.

The RECEIVING axis is separate because a stored dispatch proves a send was accepted, never that the
recipient observed anything - the reason cxc.NOT_VERIFICATION lists dispatched. So
recipientObservation answers unmeasured whenever no acknowledgement and no acknowledgement evidence
exists, WHATEVER the send state says. It reports that the receiving side is not measured; it never
says a recipient failed a duty, because nothing requires a parent to acknowledge an execution-only
event.

What this module does not do. It does not re-derive delivery._phase: that function owns the
per-delivery phase for rows that have a delivery, and a second implementation of its words would be
a second opinion about them. It carries deliveries.state verbatim instead and points a reader at
status, and where a supersession note exists it follows _phase's own precedence. It does not derive
assignment state, project ownership, a verdict, or which reviewable event is the head - that last one
is currency.head_revision's question, decided from declared lineage rather than from a timestamp, so
the reviewable block lists ids and names no winner.

The correction a needs_changes verdict queued for the child is the one relay-produced event read
here, and it is kept out of events, which stay what the child reported. It gets its own block per
child: whether it was sent, in the same send-axis words, and when it was not, why - through
assignment.undelivered_reason over the same kind of single-statement row assignment-show reads, so
the two surfaces cannot give different reasons. Without it a child whose correction the relay had
withheld every minute read as a child with nothing in its generation (CRW-5 c6).

It constructs no Store. Store.__init__ opens the file O_RDWR, switches on WAL and runs the whole
schema script, so a reader that reached Services.store against a mistyped state directory would
create an empty relay database which then answers "nothing is blocked" honestly. Every read here
goes through store.read_only_rows: one statement over a mode=ro connection opened through a
descriptor held on the database, which is what lets the rows be attributed to the file they came
from rather than to whatever the pathname reaches afterwards. store._hold_database states what
that reaches and the window it does not.
"""

from .assignment import LIFECYCLE_WITHHOLD_JOIN, undelivered_reason
from .receipts import READY
from .transport import (
    ACKNOWLEDGED,
    DEFERRED_BUSY,
    DISPATCHED,
    HELD_UNCERTAIN,
    INBOX_ONLY,
    QUEUED,
    SENDING,
    SUPERSEDED as SUPERSEDED_STATE,
    WITHHELD_PRE_SEND,
)

# delivery.EXECUTION_ONLY_OUTCOMES, spelled here rather than imported: delivery reaches the
# registry, the intake and the transport from inside its own functions, which is far more than a
# read-only reader should pull in. A test asserts the two tuples are still equal - the precedent
# delivery itself uses for linkage's two scope constants.
EXECUTION_ONLY = ("failed", "interrupted", "blocked_needs_input")
BLOCKED = "blocked_needs_input"

FINAL = "final"

# Send-axis words. queued, deferred_busy and withheld_pre_send are NOT_SENT because
# assert_attempt_invariants requires sendAttempted "no" for the retry-safe pair and a queued row has
# had no attempt at all. sending and held_uncertain are SEND_UNCERTAIN because the transport gave no
# usable answer, which is not the same as non-delivery. inbox_only reuses the word _reported_state
# already uses: a durable inbox item is not a successful wake.
NOT_SENT = "not_sent"
SEND_UNCERTAIN = "send_uncertain"
STORED_NOT_WOKEN = "stored_not_woken"
SENT = "dispatched"
WAS_SUPERSEDED = "superseded"
UNRECOGNISED = "state_unrecognised"
REFUSED_PRE_QUEUE = "refused_pre_queue"
SUPPRESSED = "suppressed"
UNMEASURED = "unmeasured"
DISAGREE = "records_disagree"

# Keyed by the imported transport constants, so a state renamed or removed there breaks this import
# rather than quietly landing in the wrong word. A test enumerates transport's own delivery states
# and asserts this covers every one of them.
OBSERVATION_BY_STATE = {
    QUEUED: NOT_SENT,
    DEFERRED_BUSY: NOT_SENT,
    WITHHELD_PRE_SEND: NOT_SENT,
    SENDING: SEND_UNCERTAIN,
    HELD_UNCERTAIN: SEND_UNCERTAIN,
    INBOX_ONLY: STORED_NOT_WOKEN,
    DISPATCHED: SENT,
    ACKNOWLEDGED: SENT,
    SUPERSEDED_STATE: WAS_SUPERSEDED,
}

# The states of an attempt the host lost, or kept no trace of, after which the same event is
# queued again (policy.HOST_LOST_TURN, policy.UNKNOWN_SEND_LOST).
LOST_ATTEMPT_STATES = ("host_lost_turn", "unknown_send_lost")

OBSERVATION_DETAIL = {
    NOT_SENT: "the delivery exists and nothing has been sent yet",
    SEND_UNCERTAIN: "a send is in flight or answered unusably, which is not evidence of "
                    "non-delivery",
    STORED_NOT_WOKEN: "stored where the recipient reads it, with no turn woken; a durable inbox "
                      "item is not a successful wake",
    SENT: None,
    WAS_SUPERSEDED: "this delivery is no longer what the assignment stands on; its state is left "
                    "alone so reconciliation can still settle it",
}

# Receiving-axis words.
OBSERVED_HOST_READ = "observed_host_read"
OBSERVED_CLAIMED = "observed_claimed"
HOST_READ_TIER = "host_read"

UNMEASURED_DETAIL = (
    "nothing in this store establishes whether a delivery was ever attempted or even wanted for "
    "this event: absence of a delivery row covers an event emitted with --no-enqueue, an event "
    "stranded by an old generation, and a refusal recorded before delivery intents existed, and "
    "those are not merged into 'not delivered'"
)
RECIPIENT_UNMEASURED_DETAIL = (
    "no acknowledgement and no acknowledgement evidence is recorded, so whether the recipient "
    "observed this event is not measured here; a dispatched delivery is not an acknowledgement and "
    "not a verification"
)

DISPOSITION_READING_LIMITS = (
    "A reading of what this scope's children last reported and what the store observed about "
    "sending it, not a completion verdict and not an assignment state. The send axis is derived "
    "from the relay's own records and the receiving axis from acknowledgement evidence; unmeasured "
    "means nobody established the fact, never that the fact is false. Which reviewable event is "
    "the current head is not decided here - read it with assignment-show - and neither is "
    "integration or verification. unreadable, an empty list and 'nothing is wrong' are three "
    "different answers and none of them means finished."
)

_META = "meta"
_CHILD = "child"

# One statement, so one snapshot. Reading the relationship, the event, the delivery, the attempt and
# the acknowledgement separately would let a delivery worker commit in between and pair states that
# never coexisted, which is why AssignmentView._anchored is one projection too. The meta arm carries
# the store id so the identity comes back on the SAME read even when there are no children: a query
# that returned it only alongside rows would go silent in exactly the case this reader exists for.
# Both arms declare the same columns, a compound SELECT takes its column names from the first arm,
# and the compound ORDER BY names output aliases only.
_SQL = (
    "SELECT 'meta' AS kind,"
    "       (SELECT value FROM schema_meta WHERE key = 'store_id') AS store_id,"
    "       NULL AS relationship_id, NULL AS issue_key, NULL AS relationship_status,"
    "       NULL AS parent_task_id, NULL AS child_task_id, NULL AS execution_generation,"
    "       NULL AS project_key, NULL AS reviewable_count, NULL AS earlier_events,"
    "       NULL AS event_id, NULL AS outcome, NULL AS producer, NULL AS stage,"
    "       NULL AS suppressed_reason, NULL AS turn_id, NULL AS attempt,"
    "       NULL AS became_final_at, NULL AS ordering_at,"
    "       NULL AS report_submission, NULL AS cxc_status, NULL AS cxc_reason,"
    "       NULL AS next_action,"
    "       NULL AS delivery_event, NULL AS delivery_state, NULL AS delivery_kind,"
    "       NULL AS recipient_task_id, NULL AS attempt_count, NULL AS hold_reason,"
    "       NULL AS dispatch_turn_id, NULL AS dispatch_evidence,"
    "       NULL AS intent_event, NULL AS intent_attempts, NULL AS intent_error,"
    "       NULL AS intent_next_retry_at,"
    "       NULL AS supersession_reason, NULL AS supersession_applied,"
    "       NULL AS request_id, NULL AS attempt_no, NULL AS attempt_internal_state,"
    "       NULL AS attempt_state, NULL AS attempt_sent_at,"
    "       NULL AS ack_event, NULL AS ack_accepted, NULL AS ack_rejection,"
    "       NULL AS ack_verified,"
    "       NULL AS ack_evidence_event, NULL AS ack_tier,"
    "       NULL AS superseded_by, NULL AS correction_event, NULL AS correction_state,"
    "       NULL AS correction_attempts, NULL AS correction_hold,"
    "       NULL AS correction_next_eligible_at, NULL AS correction_lifecycle_withhold,"
    "       NULL AS correction_lifecycle_recorded_at,"
    "       NULL AS correction_lifecycle_next_retry_at,"
    "       NULL AS correction_supersession_reason, NULL AS correction_supersession_applied"
    " UNION ALL"
    " SELECT 'child' AS kind, NULL AS store_id,"
    "        r.relationship_id AS relationship_id, r.issue_key AS issue_key,"
    "        r.status AS relationship_status, r.parent_task_id AS parent_task_id,"
    "        r.child_task_id AS child_task_id,"
    "        r.execution_generation AS execution_generation, s.project_key AS project_key,"
    # Counted rather than adjudicated: which reviewable event is current is not this question.
    "        (SELECT COUNT(*) FROM events rv"
    "          WHERE rv.relationship_id = r.relationship_id"
    "            AND rv.execution_generation = r.execution_generation"
    "            AND rv.outcome = 'ready_for_review' AND rv.stage = 'final'"
    "            AND rv.suppressed_reason IS NULL) AS reviewable_count,"
    # Not listed either, but never silently dropped: the answer is about the current generation.
    "        (SELECT COUNT(*) FROM events eg"
    "          WHERE eg.relationship_id = r.relationship_id"
    "            AND eg.execution_generation <> r.execution_generation"
    "            AND eg.outcome IN ('failed','interrupted','blocked_needs_input'))"
    "            AS earlier_events,"
    "        e.event_id AS event_id, e.outcome AS outcome, e.producer AS producer,"
    "        e.stage AS stage, e.suppressed_reason AS suppressed_reason, e.turn_id AS turn_id,"
    "        e.attempt AS attempt, e.finalized_at AS became_final_at,"
    # finalized_at is NULL for an event that was final on arrival, so ordering falls back to when it
    # was first seen. first_seen_at rather than last_seen_at: the latter is bumped on every
    # re-observation, so re-seeing an old event would make it the current one.
    "        COALESCE(e.finalized_at, e.first_seen_at) AS ordering_at,"
    "        w.submission_no AS report_submission, w.cxc_status AS cxc_status,"
    "        w.cxc_reason AS cxc_reason, w.next_action AS next_action,"
    "        d.event_id AS delivery_event, d.state AS delivery_state, d.kind AS delivery_kind,"
    "        d.recipient_task_id AS recipient_task_id, d.attempt_count AS attempt_count,"
    "        d.hold_reason AS hold_reason, d.dispatch_turn_id AS dispatch_turn_id,"
    "        d.dispatch_evidence AS dispatch_evidence,"
    "        i.event_id AS intent_event, i.attempts AS intent_attempts,"
    "        i.last_error AS intent_error, i.next_retry_at AS intent_next_retry_at,"
    "        x.reason AS supersession_reason, x.applied AS supersession_applied,"
    "        a.request_id AS request_id, a.attempt_no AS attempt_no,"
    "        a.internal_state AS attempt_internal_state, a.state AS attempt_state,"
    "        a.sent_at AS attempt_sent_at,"
    "        k.event_id AS ack_event, k.accepted AS ack_accepted,"
    "        k.rejection_reason AS ack_rejection, k.verified AS ack_verified,"
    "        v.event_id AS ack_evidence_event, v.tier AS ack_tier,"
    "        r.superseded_by AS superseded_by, ce.event_id AS correction_event,"
    "        cd.state AS correction_state, cd.attempt_count AS correction_attempts,"
    "        cd.hold_reason AS correction_hold,"
    "        cd.next_eligible_at AS correction_next_eligible_at,"
    "        cf.error_code AS correction_lifecycle_withhold,"
    "        cf.occurred_at AS correction_lifecycle_recorded_at,"
    "        cf.next_retry_at AS correction_lifecycle_next_retry_at,"
    "        cx.reason AS correction_supersession_reason,"
    "        cx.applied AS correction_supersession_applied"
    "   FROM relationships r"
    "   LEFT JOIN relationship_scope s ON s.relationship_id = r.relationship_id"
    # The event filter lives HERE and not in WHERE, so a child with no matching event still yields
    # exactly one row. A child that has reported nothing is an answer.
    "   LEFT JOIN events e ON e.relationship_id = r.relationship_id"
    "                     AND e.execution_generation = r.execution_generation"
    "                     AND e.outcome IN ('ready_for_review','failed','interrupted',"
    "                                       'blocked_needs_input')"
    "   LEFT JOIN work_reports w ON w.event_id = e.event_id"
    "                           AND w.submission_no = (SELECT MAX(submission_no)"
    "                                                    FROM work_reports"
    "                                                   WHERE event_id = e.event_id)"
    "   LEFT JOIN deliveries d ON d.event_id = e.event_id"
    # On attempt_count, so the request id is the CURRENT attempt's rather than whichever row sorted
    # first after a retry - the join _anchored uses for the same reason.
    "   LEFT JOIN attempts a ON a.event_id = e.event_id AND a.attempt_no = d.attempt_count"
    "   LEFT JOIN delivery_intent i ON i.event_id = e.event_id"
    "   LEFT JOIN delivery_supersession x ON x.event_id = e.event_id"
    "   LEFT JOIN acks k ON k.event_id = e.event_id"
    "   LEFT JOIN ack_evidence v ON v.event_id = e.event_id"
    # The current generation's correction, chosen as AssignmentView._projection chooses it, and
    # its delivery. Each join is at most one row per child, so there is still one row per child
    # and event. e, d and a above are the child's own event, delivery and attempt.
    "   LEFT JOIN events ce ON ce.event_id = (SELECT MIN(rq.event_id) FROM events rq"
    "                                          WHERE rq.relationship_id = r.relationship_id"
    "                                            AND rq.execution_generation"
    "                                                = r.execution_generation"
    "                                            AND rq.outcome = 'revision_request'"
    "                                            AND rq.suppressed_reason IS NULL)"
    "   LEFT JOIN deliveries cd ON cd.event_id = ce.event_id"
    "   LEFT JOIN delivery_supersession cx ON cx.event_id = ce.event_id"
    + LIFECYCLE_WITHHOLD_JOIN.format(alias="cf", event="ce", delivery="cd") +
    # The live predicate belongs to the project selector alone. --relationship names one assignment
    # explicitly and answers about it whatever its status, carrying that status; a global live
    # filter would make an archived assignment unreadable through the selector that named it.
    "  WHERE (? IS NULL OR (s.project_key = ?"
    "                       AND r.status IN ('active','paused') AND r.superseded_by IS NULL))"
    "    AND (? IS NULL OR r.relationship_id = ?)"
    " ORDER BY kind, relationship_id, ordering_at, event_id"
)


def read(selection, *, project_key=None, relationship_id=None) -> dict:
    """Answer for one project or one relationship, constructing nothing.

    read_only_rows reports readable True with a detail and no rows when the SQL itself fails - a
    locked, malformed or legacy database. Passing that through as an empty list would turn "we could
    not look" into "there is nothing there", which is the one merge this contract refuses, so it is
    downgraded to readable False here exactly as nonce_lookup downgrades its own case.

    A database it could not bind to the file it came from is already readable False, with the
    refusal as the detail, so that case arrives here as the same "we could not look".
    """
    from .store import read_only_rows

    selector = ({"projectKey": project_key} if project_key is not None
                else {"relationshipId": relationship_id})
    answer = read_only_rows(
        selection, _SQL, (project_key, project_key, relationship_id, relationship_id),
    )
    blank = {
        "selector": selector, "readable": False, "detail": None,
        "store": {"storeId": None, "dbPath": str(selection.db_path), "device": None,
                  "inode": None, "links": None},
        "children": [], "counts": _counts([]), "limits": DISPOSITION_READING_LIMITS,
    }
    if not answer["readable"]:
        return {**blank,
                "detail": answer["detail"] or "the store could not be opened for reading"}
    if answer["detail"]:
        return {**blank, "detail": answer["detail"]}
    rows = answer["rows"]
    meta = next((row for row in rows if row["kind"] == _META), None)
    if meta is None:
        # The meta arm is a scalar subquery with no FROM, so it returns a row even on an empty
        # store. Its absence means this read did not see the schema it was written against.
        return {**blank, "detail": "the store answered without its own identity, so these rows "
                                   "cannot be attributed to a relay database"}
    derived = derive(rows, selector=selector)
    derived["store"] = {
        "storeId": meta["store_id"], "dbPath": str(selection.db_path),
        "device": answer["device"], "inode": answer["inode"], "links": answer["links"],
    }
    return derived


def derive(rows, *, selector) -> dict:
    """Shape the flat rows. Pure, so the vocabulary can be tested without a store."""
    children = {}
    for row in rows:
        if row["kind"] != _CHILD:
            continue
        child = children.get(row["relationship_id"])
        if child is None:
            child = children[row["relationship_id"]] = {
                "relationshipId": row["relationship_id"],
                "issueKey": row["issue_key"],
                "projectKey": row["project_key"],
                "relationshipStatus": row["relationship_status"],
                "parentTaskId": row["parent_task_id"],
                "childTaskId": row["child_task_id"],
                "executionGeneration": row["execution_generation"],
                "turnDisposition": None,
                "reviewable": {
                    "eventIds": [],
                    "eventsInGeneration": row["reviewable_count"] or 0,
                    # Deliberately not answered here. A generation can hold several reviewable
                    # events and which one is current is decided from declared lineage by
                    # currency.head_revision, not from a timestamp this reader happens to see.
                    "head": "not_derived_here",
                    "readWith": "assignment-show --relationship " + str(row["relationship_id"]),
                },
                "earlierGenerationEvents": row["earlier_events"] or 0,
                "correction": _correction(row),
                "events": [],
            }
        if row["event_id"] is None:
            continue
        child["events"].append(_event(row))
        if row["outcome"] == READY and row["stage"] == FINAL and not row["suppressed_reason"]:
            child["reviewable"]["eventIds"].append(row["event_id"])
    for child in children.values():
        child["turnDisposition"] = _turn_disposition(child["events"])
    ordered = [children[key] for key in sorted(children)]
    return {
        "selector": selector, "readable": True, "detail": None,
        "children": ordered, "counts": _counts(ordered),
        "limits": DISPOSITION_READING_LIMITS,
    }


def _event(row) -> dict:
    return {
        "eventId": row["event_id"],
        "outcome": row["outcome"],
        # Carried so a daemon-observed failure and a child-asserted one stay distinguishable
        # without a second command; a daemon may never synthesize blocked_needs_input at all.
        "producer": row["producer"],
        "stage": row["stage"],
        "suppressedReason": row["suppressed_reason"],
        "turnId": row["turn_id"],
        "attempt": row["attempt"],
        "becameFinalAt": row["became_final_at"],
        "orderingAt": row["ordering_at"],
        "workReport": _work_report(row),
        "delivery": _delivery(row),
        "acknowledgement": _acknowledgement(row),
    }


def _correction(row):
    """The correction this generation's needs_changes verdict queued for the child, or None.

    The send axis uses the words events use, with the same precedence: a supersession note - a
    final event of the generation already answered the correction, and the next attempt
    suppresses it - outranks the state, which is carried beside it unchanged. The reason is
    assignment's own rule over this row, with no refusal to read: a correction is queued in the
    verdict's transaction, so it always has a delivery row. For the per-delivery phase, status
    remains the reader.
    """
    if row["correction_event"] is None:
        return None
    state = row["correction_state"]
    superseded = row["correction_supersession_reason"] is not None
    delivery = None
    if state is not None:
        delivery = {
            "observation": (WAS_SUPERSEDED if superseded
                            else OBSERVATION_BY_STATE.get(state, UNRECOGNISED)),
            "state": state,
            "attemptCount": row["correction_attempts"],
            "holdReason": row["correction_hold"],
            "nextEligibleAt": row["correction_next_eligible_at"],
            "supersession": (
                {"reason": row["correction_supersession_reason"],
                 "applied": bool(row["correction_supersession_applied"])}
                if superseded else None
            ),
        }
    reason = undelivered_reason({
        "delivered": row["correction_event"] if state is not None else None,
        "delivery_state": state,
        "hold_reason": row["correction_hold"],
        "relationship_status": row["relationship_status"],
        "superseded_by": row["superseded_by"],
        "lifecycle_withhold": row["correction_lifecycle_withhold"],
        "lifecycle_recorded_at": row["correction_lifecycle_recorded_at"],
        "lifecycle_next_retry_at": row["correction_lifecycle_next_retry_at"],
        "refusal_reason": None,
    })
    return {"eventId": row["correction_event"], "delivery": delivery,
            "undeliveredReason": reason}


def _work_report(row) -> dict:
    """The only thing that separates BLOCKED, UNSAFE and NEEDS_HUMAN.

    All three collapse onto blocked_needs_input in the frozen outcome enum, so the status is read
    from the report the child wrote and is never inferred from the outcome. No report is recorded
    False with nulls, which says the separation is unavailable rather than guessing one.
    """
    if row["report_submission"] is None:
        return {"recorded": False, "submissionNo": None, "cxcStatus": None,
                "cxcReason": None, "nextAction": None}
    return {
        "recorded": True,
        "submissionNo": row["report_submission"],
        "cxcStatus": row["cxc_status"],
        "cxcReason": row["cxc_reason"],
        "nextAction": row["next_action"],
    }


def _delivery(row) -> dict:
    observation, detail = _observation(row)
    recipient, recipient_detail = _recipient_observation(row)
    return {
        "observation": observation,
        "recipientObservation": recipient,
        # The store's own word, unchanged. delivery._phase owns the phase vocabulary for a row that
        # exists; this carries the state and points there rather than re-deriving it.
        "state": row["delivery_state"],
        "kind": row["delivery_kind"],
        "recipientTaskId": row["recipient_task_id"],
        "attemptCount": row["attempt_count"],
        "holdReason": row["hold_reason"],
        "dispatchTurnId": row["dispatch_turn_id"],
        "dispatchEvidence": row["dispatch_evidence"] is not None,
        "detail": detail,
        "recipientDetail": recipient_detail,
        "readWith": "status --relationship " + str(row["relationship_id"]),
        "intent": (
            {"recorded": True, "attempts": row["intent_attempts"],
             "lastError": row["intent_error"], "nextRetryAt": row["intent_next_retry_at"]}
            if row["intent_event"] is not None else
            {"recorded": False, "attempts": None, "lastError": None, "nextRetryAt": None}
        ),
        # Reported beside the state and never replacing it: an outstanding send keeps its state so
        # reconciliation can still settle it.
        "supersession": (
            {"reason": row["supersession_reason"], "applied": bool(row["supersession_applied"])}
            if row["supersession_reason"] is not None else None
        ),
        "currentAttempt": (
            {"requestId": row["request_id"], "attemptNo": row["attempt_no"],
             "internalState": row["attempt_internal_state"], "state": row["attempt_state"],
             "sentAt": row["attempt_sent_at"]}
            if row["request_id"] is not None else None
        ),
    }


def _observation(row):
    """The send axis. Order matters, because the conditions overlap.

    Suppression is checked before the stage, because resolve_staged_in writes stage suppressed
    together with the reason: checking the stage first would report a suppressed claim as merely
    staged, which is a different fact about a different turn.

    A supersession note outranks the state for the same reason _phase puts it first: an outstanding
    send that a newer generation replaced keeps its state so reconciliation can settle it, and
    reporting it as sent or unsent would describe an obligation nothing can now meet. The state
    itself is still carried verbatim beside this word.
    """
    if row["suppressed_reason"]:
        return SUPPRESSED, (
            "the claim was suppressed, so there is no delivery obligation to measure: "
            + str(row["suppressed_reason"])
        )
    if row["stage"] != FINAL:
        return "not_deliverable:" + str(row["stage"]), (
            "only a final event may be delivered, so an event at stage " + repr(row["stage"])
            + " has no delivery to measure and is never delivery evidence"
        )
    if row["delivery_event"] is not None:
        if row["supersession_reason"] is not None:
            return WAS_SUPERSEDED, OBSERVATION_DETAIL[WAS_SUPERSEDED]
        observation = OBSERVATION_BY_STATE.get(row["delivery_state"])
        if observation is None:
            # A state this version has no word for is not quietly folded into one that would claim
            # something about it.
            return UNRECOGNISED, (
                "the delivery records state " + repr(row["delivery_state"])
                + ", which this reader has no word for; read it with status"
            )
        if observation == NOT_SENT and row["attempt_state"] in LOST_ATTEMPT_STATES:
            # Queued again after its current attempt was lost (hostloss.py): something was sent,
            # and the host lost it or kept no trace of it, so "nothing has been sent" would deny
            # what currentAttempt records (independent review of 668890b0, CRW-231).
            return observation, (
                "an earlier attempt was lost (" + row["attempt_state"] + ", see currentAttempt);"
                " the same event is queued and its next attempt has not been sent"
            )
        return observation, OBSERVATION_DETAIL[observation]
    if row["intent_event"] is not None:
        return REFUSED_PRE_QUEUE, (
            "delivery was wanted for this event and refused for a reason that may not last"
        )
    if (row["ack_event"] is not None or row["ack_evidence_event"] is not None
            or row["supersession_reason"] is not None):
        # Unreachable through the write paths, which create the delivery row before anything can
        # acknowledge or supersede it. Kept because a reader must answer safely on a store that
        # holds one anyway - an older writer, a hand edit, a future bug - and the rule for that is
        # to report the disagreement rather than pick whichever half looks tidier.
        return DISAGREE, (
            "this event has no delivery row, yet the store holds "
            + ", ".join(
                name for name, present in (
                    ("an acknowledgement", row["ack_event"] is not None),
                    ("acknowledgement evidence", row["ack_evidence_event"] is not None),
                    ("a supersession note", row["supersession_reason"] is not None),
                ) if present
            )
            + " for it, so no single answer about its delivery is supportable"
        )
    return UNMEASURED, UNMEASURED_DETAIL


def _recipient_observation(row):
    """The receiving axis, which a send state cannot answer.

    unmeasured survives a dispatched delivery on purpose: a dispatch proves the send was accepted,
    never that the recipient observed anything. host_read is an App Server read of the recipient's
    real turn list; anything else is recorded intent still awaiting that read.
    """
    if row["ack_event"] is not None:
        if row["ack_tier"] == HOST_READ_TIER:
            return OBSERVED_HOST_READ, None
        return OBSERVED_CLAIMED, (
            "an acknowledgement is recorded, but its own turn has not been established by a host "
            "read, so it is the parent's claim rather than an observation of it"
        )
    if row["ack_evidence_event"] is not None:
        # Same defensive shape as the send axis: every write path records evidence in the same
        # transaction as the acknowledgement it describes, so evidence alone cannot be produced by
        # them, and inventing an acknowledgement to explain it would be worse than saying so.
        return DISAGREE, (
            "acknowledgement evidence is recorded for this event with no acknowledgement beside "
            "it, so whether the recipient observed it has no supportable answer"
        )
    return UNMEASURED, RECIPIENT_UNMEASURED_DETAIL


def _acknowledgement(row) -> dict:
    """The descriptive block, carrying the fields _anchored reports, plus whether a row exists.

    settlement says whether the acknowledgement closed and evidenceTier says what it closed on; no
    row at all is unrecorded rather than unverified. recorded is this reader's addition rather than
    part of that copy, because the receiving judgment lives on recipientObservation instead.
    """
    present = row["ack_event"] is not None
    return {
        "recorded": present,
        "accepted": bool(row["ack_accepted"]) if present else None,
        "rejectionReason": row["ack_rejection"] if present else None,
        "settlement": row["ack_verified"] if present else None,
        "evidenceTier": row["ack_tier"] if row["ack_tier"] is not None else "unrecorded",
    }


def _turn_disposition(events) -> dict:
    """What this child's current turn did, over the execution-only outcomes only.

    A reviewable event is not a competitor of these: the store treats an execution-only event as a
    different kind of fact about the same generation, so mixing them would manufacture a contest the
    records do not contain. The reviewable axis is counted and listed separately.

    Where two final dispositions disagree, the contest IS the answer. Naming a winner by timestamp
    would hand a coordinator a guess wearing the shape of a fact, and arrival order decides nothing
    in this store.
    """
    candidates = [
        event for event in events
        if event["outcome"] in EXECUTION_ONLY
        and event["stage"] == FINAL and not event["suppressedReason"]
    ]
    if not candidates:
        return {"outcome": None, "eventId": None, "basis": "none", "candidates": [],
                "detail": "no final execution-only disposition in this generation, which is not "
                          "the same as nothing being wrong"}
    identifiers = sorted(event["eventId"] for event in candidates)
    outcomes = {event["outcome"] for event in candidates}
    if len(outcomes) > 1:
        return {
            "outcome": None, "eventId": None, "basis": "contested",
            "candidates": identifiers,
            "detail": "this generation holds " + str(len(outcomes))
                      + " disagreeing final dispositions (" + ", ".join(sorted(outcomes))
                      + "), and choosing between them would be a guess",
        }
    newest = max(candidates, key=lambda event: (event["orderingAt"] or "", event["eventId"]))
    if len(candidates) == 1:
        return {"outcome": newest["outcome"], "eventId": newest["eventId"], "basis": "sole",
                "candidates": identifiers, "detail": None}
    return {
        "outcome": newest["outcome"], "eventId": newest["eventId"],
        "basis": "latest_of_same_outcome", "candidates": identifiers,
        "detail": "several events report the same outcome in this generation; the newest anchors "
                  "it and every candidate is listed",
    }


def _counts(children) -> dict:
    """Counted from the derived children, so a count cannot disagree with the list it summarises."""
    unmeasured_send = 0
    unmeasured_recipient = 0
    missing_report = 0
    for child in children:
        for event in child["events"]:
            if event["delivery"]["observation"] == UNMEASURED:
                unmeasured_send += 1
            if event["delivery"]["recipientObservation"] == UNMEASURED:
                unmeasured_recipient += 1
            if event["outcome"] in EXECUTION_ONLY and not event["workReport"]["recorded"]:
                missing_report += 1
    return {
        "children": len(children),
        "withExecutionOnlyDisposition": sum(
            1 for child in children if child["turnDisposition"]["basis"] != "none"),
        "blocked": sum(
            1 for child in children if child["turnDisposition"]["outcome"] == BLOCKED),
        "contested": sum(
            1 for child in children if child["turnDisposition"]["basis"] == "contested"),
        "deliveryUnmeasured": unmeasured_send,
        "recipientUnmeasured": unmeasured_recipient,
        "workReportMissing": missing_report,
        # Counted by the delivery's own state, so a correction withheld for a reason this reader
        # cannot name is still counted.
        "correctionNotSent": sum(
            1 for child in children
            if (child["correction"] or {}).get("delivery")
            and child["correction"]["delivery"]["observation"] == NOT_SENT),
        "correctionWithheld": sum(
            1 for child in children
            if (child["correction"] or {}).get("delivery")
            and child["correction"]["delivery"]["state"] == WITHHELD_PRE_SEND
            and child["correction"]["delivery"]["supersession"] is None),
    }
