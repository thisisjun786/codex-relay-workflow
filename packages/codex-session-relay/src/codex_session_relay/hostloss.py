"""Noticing that the host lost a turn it accepted, and recovering the obligation once (CRW-224).

A dispatch is a transport fact: turn/start returned a turn id. It is not proof that the host still
has that turn. CRW-124's H0 rehearsal killed the App Server right after it accepted turn/start for a
completion to the parent; the rollout kept only task_started, the restarted host listed no such
turn, and the relay held the delivery as dispatched_awaiting_ack for ever. Nothing read the
parent's turns, so the child's report was lost silently and looked exactly like a lost ACK.

The reading here asks the recipient's own turn list for the dispatched turn among the turns begun
since the send (hostadapter.find_in_listing). A loss is concluded only from the host's answer, only
once the send is older than the allowance a whole-second start time and a clock skew need, and only
when a scan of every item of the turns begun since the send shows this attempt's token is not
there (hostadapter.find_token_in): a found token means the message reached the recipient whatever
happened to the turn row, and a scan that stopped at its bound has shown nothing (I-42). A lost
ACK keeps its turn in the list, interrupted, with the message in it, so it reads present and its
path does not change.

A reading that cannot reach an answer by waiting - the listing never reached the send, the listing
is empty once the send is past the allowance, the token scan could not cover the turns since it,
the attempt has no send time - is recorded on the attempt as turn_check_undecided:<reason>, which
status names, rather than left to look like an ordinary wait. It is cleared when a later reading
decides.

Recovery is the ordinary claim path, once. The delivery goes back to queued and the next attempt of
the same event is sent under a new request id; if the host loses that turn too, the delivery is
held under host_lost_turn and nothing sends it again. Only a completion is recovered: it is the one
kind waiting for an acknowledgement, and a revision's dispatch turn is its generation's anchor,
which is never rebound. Other kinds are read and reported, and changed in nothing.
"""

import json
from datetime import datetime

from .delivery import COMPLETION
from .hostadapter import DISPATCH_TURN_SKEW_SECONDS, TURN_PRESENT, TURN_START_PRECISION_SECONDS
from .hostadapter import ListingBounded, ListingEmpty
from .policy import HOST_LOST_TURN, TURN_CHECK_UNDECIDED
from .transport import DISPATCHED, QUEUED, assert_attempt_invariants

PRESENT = "present"
UNKNOWN = "unknown"
# A turn listed in one of these is persisted: a later restart reads it back, interrupted at worst,
# so it cannot be lost any more and need not be read again.
TERMINAL = ("completed", "interrupted", "failed")
TOKEN_SCAN_LIMIT = 200
# The redelivery a loss leads to, or why none followed.
REQUEUED = "queued"
HELD = "held"
NOT_MOVED = "not_moved"
REPORT_ONLY = "report_only"
# Why an unknown reading will not resolve by waiting. These are recorded by name; every other
# unknown - an unreadable host, a send too recent - is simply read again later.
LISTING_BOUNDED = "listing_bounded"
LISTING_EMPTY = "listing_empty"
TOKEN_SCAN_BOUNDED = "token_scan_bounded"
NO_SEND_TIME = "no_send_time"
NO_TURN = "no_turn"
UNDECIDED_MARK = TURN_CHECK_UNDECIDED + ":"


class _Raced(Exception):
    """The attempt changed between the delivery's guarded update and its own."""


def _epoch(stamp):
    try:
        return datetime.fromisoformat(stamp).timestamp()
    except (TypeError, ValueError):
        return None


def read_recipient_turn(adapter, clock, attempt, delivery, turn_id) -> dict:
    """What the recipient's own turns say about this attempt's turn. Reads only; writes nothing.

    finding is present, host_lost_turn or unknown. unknown is not an answer, and detail says why
    no answer was reached: an adapter that cannot list turns, an attempt without a send time, an
    unreadable host, or a send too recent to call its turn lost.
    undecided names the reasons waiting will not fix (listing_bounded, listing_empty,
    token_scan_bounded, no_send_time, no_turn), and is None otherwise.
    """
    reading = {"turnId": turn_id, "finding": UNKNOWN, "status": None, "detail": None,
               "undecided": None}
    thread = delivery["recipient_thread_id"]
    lookup = getattr(adapter, "find_dispatched_turn", None)
    token_since = getattr(adapter, "find_token_since", None)
    if lookup is None or token_since is None:
        reading["detail"] = "this host adapter cannot list the recipient's turns"
        return reading
    if not turn_id:
        reading.update(detail="the attempt names no turn", undecided=NO_TURN)
        return reading
    # When the send STARTED. observed_at is its settlement, later than the send, and would move
    # the cutoff the wrong way; an attempt without the stamp is left unjudged instead.
    sent_at = _epoch(attempt["sent_at"])
    if sent_at is None:
        reading.update(detail="the attempt has no send time, so absence cannot be bounded",
                       undecided=NO_SEND_TIME)
        return reading
    allowance = TURN_START_PRECISION_SECONDS + DISPATCH_TURN_SKEW_SECONDS
    try:
        presence = lookup(thread, turn_id, sent_at=sent_at)
    except ListingBounded as error:
        reading.update(detail=f"undecided: {error}", undecided=LISTING_BOUNDED)
        return reading
    except ListingEmpty as error:
        # A parent whose only turn was the lost delivery lists nothing, and will go on listing
        # nothing: name it rather than read it as a transient failure for ever (review 4). Not
        # before the allowance, when an empty list may still be the host catching up.
        if clock.now() < sent_at + allowance:
            reading["detail"] = (
                f"the recipient lists no turns yet, and the send is less than {allowance:.0f} s "
                f"old: {error}"
            )
        else:
            reading.update(detail=f"undecided: {error}", undecided=LISTING_EMPTY)
        return reading
    except Exception as error:  # noqa: BLE001 - an unreadable host is its own answer
        reading["detail"] = f"unreadable: {type(error).__name__}: {error}"
        return reading
    if presence.finding == TURN_PRESENT:
        reading.update(finding=PRESENT, status=presence.turn.status,
                       detail=f"the recipient lists this turn ({presence.turn.status})")
        return reading
    if clock.now() < sent_at + allowance:
        reading["detail"] = (
            f"the recipient does not list this turn yet ({presence.stop}), but the send is less "
            f"than {allowance:.0f} s old, too recent to call the turn lost"
        )
        return reading
    try:
        scan = token_since(thread, attempt["request_id"], turns=presence.seen + (turn_id,),
                           limit=TOKEN_SCAN_LIMIT)
    except Exception as error:  # noqa: BLE001
        reading["detail"] = (
            f"the recipient does not list this turn, and its items could not be read for this "
            f"attempt's token: {type(error).__name__}: {error}"
        )
        return reading
    if scan.found:
        reading.update(
            finding=PRESENT,
            detail=f"the recipient does not list this turn, but this attempt's token is in its "
                   f"items (turn {scan.turn_id})",
        )
        return reading
    if not scan.exhausted:
        reading.update(
            detail=f"undecided: the recipient does not list this turn, and {scan.scanned} items "
                   f"did not cover the {len(presence.seen)} turns begun since the send, so the "
                   f"token's absence is not shown",
            undecided=TOKEN_SCAN_BOUNDED,
        )
        return reading
    reading.update(
        finding=HOST_LOST_TURN,
        detail=f"the recipient's turn list has no such turn ({presence.stop} after "
               f"{presence.scanned} turns) and this attempt's token is not among the "
               f"{scan.scanned} items of the {len(presence.seen)} turns begun since the send",
    )
    return reading


def record_undecided(store, request_id, reading) -> int:
    """Name an undecided reading on its attempt, or clear the name once a reading decides.

    Written only while the attempt is still a settled dispatch, and only when the recorded value
    changes, so a tick that learns nothing new writes nothing. Returns the rows changed.
    """
    if reading["finding"] == PRESENT:
        wanted = None
    elif reading["finding"] == UNKNOWN and reading.get("undecided"):
        wanted = UNDECIDED_MARK + reading["undecided"]
    else:
        return 0
    with store.transaction() as db:
        row = db.execute(
            "SELECT internal_state, state, recipient_scan FROM attempts WHERE request_id = ?",
            (request_id,),
        ).fetchone()
        if row is None or row["internal_state"] != "settled" or row["state"] != DISPATCHED:
            return 0
        current = row["recipient_scan"]
        if wanted is None and not (current or "").startswith(UNDECIDED_MARK):
            return 0
        if wanted is not None and current == wanted:
            return 0
        return db.execute(
            "UPDATE attempts SET recipient_scan = ? WHERE request_id = ?", (wanted, request_id)
        ).rowcount


def settle(store, clock, request_id, reading, *, observation=None) -> dict:
    """Record the loss and put the obligation back, in one guarded transaction.

    The delivery moves only while it is still a completion dispatched on THIS attempt, unheld and
    unacknowledged; the attempt only while it is still settled as dispatched. Anything else means
    somebody moved first - a recorded acknowledgement, a later attempt - and nothing is written.
    An earlier attempt of the same event already lost to the host turns the requeue into a hold:
    one redelivery per obligation, never a third send.
    """
    now_iso = clock.iso()
    try:
        with store.transaction() as db:
            attempt = db.execute(
                "SELECT * FROM attempts WHERE request_id = ?", (request_id,)
            ).fetchone()
            earlier = db.execute(
                "SELECT COUNT(*) AS c FROM attempts"
                " WHERE event_id = ? AND state = ? AND request_id <> ?",
                (attempt["event_id"], HOST_LOST_TURN, request_id),
            ).fetchone()["c"]
            hold = HOST_LOST_TURN if earlier else None
            moved = db.execute(
                "UPDATE deliveries SET state = ?, hold_reason = ?, next_eligible_at = NULL,"
                " dispatch_evidence = ?, lease_owner = NULL, lease_until = NULL, updated_at = ?"
                " WHERE event_id = ? AND kind = ? AND state = ? AND attempt_count = ?"
                "   AND hold_reason IS NULL"
                "   AND NOT EXISTS (SELECT 1 FROM acks k WHERE k.event_id = deliveries.event_id)",
                (QUEUED, hold, HOST_LOST_TURN, now_iso, attempt["event_id"], COMPLETION,
                 DISPATCHED, attempt["attempt_no"]),
            ).rowcount
            if moved != 1:
                return {"redelivery": NOT_MOVED,
                        "redeliveryDetail": "the delivery is no longer this attempt's unheld,"
                                            " unacknowledged dispatch"}
            record = json.loads(attempt["record"])
            record["reconciliation"] = {
                "operationReceiptChecked": observation is not None,
                "recipientTurnsChecked": True,
                # What the transport showed, unchanged: turn/start returned this turn id. The
                # loss is the attempt's state, not a rewrite of its evidence.
                "affirmativeEvidence": "receipt_turn_id",
                "checkedAt": now_iso,
            }
            assert_attempt_invariants(record)
            marked = db.execute(
                "UPDATE attempts SET state = ?, record = ?,"
                " operation_observation = COALESCE(?, operation_observation),"
                " recipient_scan = ?, affirmative_evidence = ?, reconciled_at = ?"
                " WHERE request_id = ? AND internal_state = 'settled' AND state = ?",
                (HOST_LOST_TURN, json.dumps(record), observation, reading["detail"],
                 "receipt_turn_id", now_iso, request_id, DISPATCHED),
            ).rowcount
            if marked != 1:
                raise _Raced()
            store.journal(
                HOST_LOST_TURN, attempt["event_id"],
                {"requestId": request_id, "turnId": reading["turnId"],
                 "redelivery": HELD if hold else REQUEUED, "detail": reading["detail"]},
                at=now_iso,
            )
    except _Raced:
        return {"redelivery": NOT_MOVED,
                "redeliveryDetail": "the attempt changed while the loss was being recorded"}
    return {"state": HOST_LOST_TURN, "redelivery": HELD if hold else REQUEUED,
            "holdReason": hold, "record": record}


_AWAITING_SQL = (
    "SELECT d.event_id, a.request_id FROM deliveries d"
    " JOIN attempts a ON a.event_id = d.event_id AND a.attempt_no = d.attempt_count"
    " WHERE d.kind = ? AND d.state = ? AND d.hold_reason IS NULL"
    "   AND a.internal_state = 'settled' AND a.state = ?"
    "   AND NOT EXISTS (SELECT 1 FROM acks k WHERE k.event_id = d.event_id)"
)


def awaiting_ack(store, *, after=None, limit):
    """Delivered completions still owed an acknowledgement, keyed by event, one page.

    The attempt is the delivery's CURRENT one, joined on the event as well as the number: every
    event's first attempt is number 1. A held, acknowledged or already-lost row is not a candidate.
    """
    sql = _AWAITING_SQL
    params = [COMPLETION, DISPATCHED, DISPATCHED]
    if after is not None:
        sql += " AND d.event_id > ?"
        params.append(after)
    sql += " ORDER BY d.event_id LIMIT ?"
    params.append(limit)
    return store.all(sql, tuple(params))


def still_awaiting(store, request_ids):
    """Which of these request ids are still a candidate of awaiting_ack, by the same test.

    The caller bounds the list; the request id is the attempts table's key.
    """
    request_ids = list(request_ids)
    if not request_ids:
        return set()
    marks = ", ".join("?" for _ in request_ids)
    rows = store.all(_AWAITING_SQL + f" AND a.request_id IN ({marks})",
                     (COMPLETION, DISPATCHED, DISPATCHED, *request_ids))
    return {row["request_id"] for row in rows}
