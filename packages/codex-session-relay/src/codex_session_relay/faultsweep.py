"""Deriving fault observations from rows this store already holds.

Reading only. Nothing here writes a fault: it answers what the store currently shows, and
FaultLedger.record decides what that means for a fault. Keeping the two apart is what lets
the same derivation run on a daemon tick, from an operator's command, and inside a test
without any of them differing.

Bounded on purpose. This runs on every tick beside passes that are each capped, so every
query carries a LIMIT and the pass as a whole is capped by SWEEP_LIMIT per source. A sweep
that could grow with the store would be the one pass able to starve the others.

Every source is read in ROTATIONS (invariant 14, enforced by _rotation). A rotation captures
the source's upper key when it starts and pages up to it; a short page ends it and the next
sweep starts another. So every row present when a rotation starts is read within that
rotation, however many full pages it takes, and a row that appears behind the cursor or past
the captured bound is read by the next one.

CLEARING is derived the same way, and that is the part worth reading. A class whose source is
this store is cleared by RE-DERIVING it: if a sweep asks the source and it no longer produces a
fault's signature, the sweep positively established the absence and says so. A class whose
source is a reading handed in from outside is never cleared that way, because a reading nobody
supplied establishes nothing - that is the difference between "the thing is gone" and "nobody
looked".
"""

import json

from . import faults
from . import settings as settings_module
from .errors import RefusalReason
from .policy import RetryPolicy

SWEEP_LIMIT = 32
# The most readings one call may be handed, reduced and paged. More is refused rather than
# truncated: a caller that handed in more than it can see answered for has to page itself.
MAX_READINGS = 1000
# How many managed turns one sweep reads through omitted.observe. Each reads marker files and
# opens the store read-only, so this stays small; the rotation still reaches every turn.
MANAGED_READINGS_PER_SWEEP = 8

STORE_SOURCE = "store"
READING_SOURCE = "reading"

# Which classes this module derives, and therefore which ones it may clear by absence.
DERIVED = ("delivery_stalled", "record_sync_failed", "observation_stalled",
           "delivery_refused", "managed_start_failed")

OBSERVATION_SCHEMA = "reporting-observation/1"
UNREPORTED = "unreported"
UNMEASURED = "unmeasured"
REPORTED = "reported"
# Every state that ESTABLISHED something, which is what an unmeasured notice says nobody had.
# in_progress and unmanaged are answers too: the turn is still running, or this relay never
# managed it. Leaving the notice open after one of those keeps a record about a question that
# has since been answered.
ESTABLISHED = (REPORTED, UNREPORTED, "in_progress", "unmanaged")

# A delivery that is nowhere any more, so an unmoving row in one of these is not a fault.
SETTLED_DELIVERY = ("dispatched", "inbox_only", "superseded")
# How many attempts make a delivery one that is RETRYING rather than one in flight. A first
# attempt is ordinary; a second means the first did not land.
RETRYING_ATTEMPTS = 2

# The refusals a send meets before any transport call that say the RECORD is wrong: settings
# that are missing, mistyped or not preservable, a sandbox or approval policy this transport
# cannot carry, and a role binding that contradicts the task's creation. Each needs somebody
# to change something, so repeating is a fault. Deliberately absent: a paused, archived or
# unloaded recipient (it is waiting, and waiting is never a fault), an inactive relationship
# (journaled under its own kind), and the unloaded-thread pair check, which clears when the
# thread is loaded.
SETTINGS_REFUSALS = frozenset({
    RefusalReason.SETTINGS_UNAVAILABLE.value,
    RefusalReason.SETTINGS_INCOMPLETE.value,
    RefusalReason.SETTINGS_MISTYPED.value,
    RefusalReason.UNSUPPORTED_SANDBOX_TYPE.value,
    RefusalReason.UNSUPPORTED_APPROVAL_POLICY.value,
    RefusalReason.ROLE_POLICY_UNCONFIGURED.value,
    RefusalReason.ROLE_BINDING_MISMATCH.value,
    RefusalReason.SETTINGS_RECORD_STALE_FOR_ROLE.value,
    settings_module.SETTINGS_NOT_PRESERVED,
    settings_module.SETTING_UNOBSERVABLE,
    settings_module.ENVIRONMENTS_UNKNOWN,
    settings_module.UNVERIFIABLE_PERMISSION_PROFILE,
})
# Journal kinds that END a refusal streak for their delivery: a send that settled, and a
# withholding because a person paused or archived the assignment. A busy deferral is not
# among them - it says nothing about the settings that were refused.
STREAK_ENDS = ("delivery_attempted", "delivery_withheld_inactive")
# The refusal reason of a journal row, read only where the detail is JSON. Journal details
# are free text for other kinds, and json_extract on text that is not JSON raises.
_REASON = "(CASE WHEN json_valid({t}.detail) THEN json_extract({t}.detail, '$.reason') END)"
# The receipt the registry attaches a managed start on. Anything else recorded against an
# armed request is the host answering without publishing a child.
ACCEPTED_RECEIPT = "accepted"


def _named(value):
    """A name is a non-blank string, which a list and a blank both fail."""
    return isinstance(value, str) and bool(value.strip())


def _position(stored) -> dict:
    """A stored rotation position. JSON {"at", "until"}; anything else is a legacy plain key,
    which resumes where it was and captures its bound on the next page."""
    if stored is None:
        return {"at": None, "until": None}
    if isinstance(stored, dict):
        return {"at": stored.get("at"), "until": stored.get("until")}
    try:
        value = json.loads(stored)
    except (TypeError, ValueError):
        value = None
    if isinstance(value, dict) and set(value) <= {"at", "until"}:
        return {"at": value.get("at"), "until": value.get("until")}
    return {"at": stored, "until": None}


def _rotation(store, stored, upper_sql, *, integer=False):
    """Where one page of a rotation reads: (after, until, starting). Invariant 14.

    A rotation is bounded by the source's upper key captured when it STARTS, and pages read
    key > after AND key <= until. A short page ends it. That is what makes every rotation reach
    the end: a cursor that wrapped after a fixed number of full pages left every row behind
    that many pages unread for as long as the source stayed that large.

    until is None only for an empty source, and then nothing is read.
    """
    position = _position(stored)
    at, until = position["at"], position["until"]
    if until is None:
        row = store.one(upper_sql)
        until = row[0] if row is not None else None
    if integer:
        try:
            at = None if at is None else int(at)
            until = None if until is None else int(until)
        except (TypeError, ValueError):
            # A position this cannot read restarts the rotation rather than guessing.
            at, until = None, store.one(upper_sql)[0]
            until = None if until is None else int(until)
    if until is None:
        at = None
    return at, until, at is None


def _page(observations, rows, key, after, limit, until=None) -> dict:
    """One page of a rotation, with where the next one resumes and whether it saw the whole.

    complete means this call read the source from the START and did not fill its page, which
    is the only shape that establishes an absence. A page that filled, or one that resumed
    from a cursor, has seen part of the source and can clear nothing.
    """
    filled = len(rows) >= limit
    if not rows:
        at = None
    elif key == "anchor":
        # Zero-padded, because the cursor is compared as TEXT and the rows are ordered
        # numerically: without it a cursor ending at generation 9 hid generation 10.
        at = f"{rows[-1]['relationship_id']}:{rows[-1]['execution_generation']:020d}"
    else:
        at = rows[-1][key]
    return {
        "observations": observations,
        # A short page ends the rotation; the next sweep starts another from the beginning.
        "cursor": {"at": at, "until": until} if filled else None,
        "filled": filled,
        "complete": after is None and not filled,
    }


def scope_of(store, relationship_id, base=None, cache=None) -> dict:
    """Where a fault about this relationship is filed, read from the relationship's own scope.

    Derived per row rather than taken from one scope the caller passed in. A daemon sweeps a
    store holding several projects, and a single scope key made every automatic fault file
    under the bare product - so a target configured for crw:CRW matched none of them and the
    publications stayed permanently ineligible. That is the whole automation failing quietly.
    """
    answer = dict(base or {})
    if not _named(relationship_id):
        return answer
    if cache is not None and relationship_id in cache:
        return {**answer, **cache[relationship_id]}
    row = store.one(
        "SELECT r.issue_key, s.project_key FROM relationships r"
        "  LEFT JOIN relationship_scope s ON s.relationship_id = r.relationship_id"
        " WHERE r.relationship_id = ?", (relationship_id,))
    found = {}
    if row is not None:
        if row["project_key"]:
            found["projectKey"] = row["project_key"]
        if row["issue_key"]:
            found["issueKey"] = row["issue_key"]
    if cache is not None:
        cache[relationship_id] = found
    return {**answer, **found}


def _evidence(kind, ref, observed) -> dict:
    """Evidence as a SNAPSHOT. The rows below are all overwritten in place by their owners."""
    return {"kind": kind, "ref": ref, "observed": observed}


def delivery_faults(store, *, product, scope, limit=SWEEP_LIMIT, policy=None,
                    cursor=None) -> dict:
    """Deliveries that are not moving, grouped by the recipient and cause, never by event.

    One unreachable recipient strands every delivery queued for it. Keyed on the event, that
    filed one issue per stranded event for a single broken recipient; keyed on the recipient
    and the cause, it is one fault with one occurrence per stranded delivery.

    The attempt state is part of the signature and not decoration: hold reasons like
    attempt_cap cover every pre-send failure there is, so two genuinely different causes -
    a settings rejection and a transport error - would otherwise merge into one record that
    named neither.
    """
    # Validated before it reaches SQL, where LIMIT -1 means no limit at all.
    limit = faults.bounded(limit, "limit")
    policy = policy or RetryPolicy()
    after, until, _ = _rotation(store, cursor, "SELECT MAX(event_id) FROM deliveries")
    rows = [] if until is None else store.all(
        "SELECT d.event_id, d.relationship_id, d.recipient_task_id, d.state, d.hold_reason,"
        "       d.attempt_count,"
        # The latest SETTLED attempt. An attempt row is written in_flight, carrying a
        # provisional held_uncertain state, before the transport call is made; reading it
        # then named a send still in progress as the cause of the hold.
        "       (SELECT a.request_id FROM attempts a WHERE a.event_id = d.event_id"
        "          AND a.internal_state = 'settled'"
        "         ORDER BY a.attempt_no DESC LIMIT 1) AS last_request,"
        "       (SELECT a.state FROM attempts a WHERE a.event_id = d.event_id"
        "          AND a.internal_state = 'settled'"
        "         ORDER BY a.attempt_no DESC LIMIT 1) AS last_state"
        "  FROM deliveries d"
        # Held deliveries only. A delivery that is retrying before any hold is set is read
        # from its attempt rows by retry_faults, one occurrence per settled failure.
        " WHERE d.hold_reason IS NOT NULL AND d.state NOT IN (?,?,?)"
        "   AND d.event_id > ? AND d.event_id <= ?"
        " ORDER BY d.event_id LIMIT ?",
        (*SETTLED_DELIVERY, after or "", until, limit),
    )
    observations = []
    cache = {}
    for row in rows:
        capped = (row["attempt_count"] or 0) >= policy.max_attempts
        signature = {"recipient": row["recipient_task_id"],
                     "attemptState": row["last_state"]}
        observations.append(faults.observation(
            product=product, fault_class="delivery_stalled",
            severity=faults.BROKEN if capped else faults.DEGRADED,
            signature=signature,
            occurrence_key=f"delivery:{row['last_request'] or row['event_id']}",
            scope=scope_of(store, row["relationship_id"], scope, cache),
            detail=(f"a delivery to {row['recipient_task_id']} is held:"
                    f" {row['hold_reason']}" if row["hold_reason"] else
                    f"a delivery to {row['recipient_task_id']} is on attempt"
                    f" {row['attempt_count']}"),
            evidence=[_evidence("row", f"deliveries:{row['event_id']}", {
                "state": row["state"], "holdReason": row["hold_reason"],
                "attemptCount": row["attempt_count"], "lastAttemptState": row["last_state"],
                "relationship": row["relationship_id"],
            })],
        ))
    return _page(observations, rows, "event_id", after, limit, until)


def retry_faults(store, *, product, scope, limit=SWEEP_LIMIT, cursor=None) -> dict:
    """Failures that have not reached a hold yet, one occurrence per actual failed attempt.

    The hold reason is only set at a cap, so a query that required one saw nothing until a
    delivery had already given up - and the degraded tier, which needs three observations
    inside a window, could never be reached at all. The attempt rows are where a retryable
    failure is written down, and their request ids advance once per attempt rather than once
    per sweep, which is exactly the occurrence identity this needs.

    Paged over the attempt rows in rotations, like every other source.
    """
    # Validated before it reaches SQL, where LIMIT -1 means no limit at all.
    limit = faults.bounded(limit, "limit")
    # Integers, and coerced as such. fault_cursors.position is TEXT, and SQLite orders every
    # integer before every string - so "rowid > '32'" matched nothing and the rotation
    # silently restarted.
    after, until, _ = _rotation(store, cursor, "SELECT MAX(rowid) FROM attempts", integer=True)
    rows = [] if until is None else store.all(
        "SELECT a.rowid AS seq, a.request_id, a.state AS attempt_state, a.event_id,"
        "       d.relationship_id, d.recipient_task_id, d.state AS delivery_state,"
        "       d.hold_reason, d.attempt_count"
        "  FROM attempts a JOIN deliveries d ON d.event_id = a.event_id"
        " WHERE d.state NOT IN (?,?,?)"
        # SETTLED attempts only. delivery.py inserts the row in_flight with a provisional
        # held_uncertain state before the transport call returns, so three healthy sends
        # observed mid-flight used to reach the degraded threshold. A reconciled uncertain
        # outcome is settled too, and stays eligible: nobody could establish that it landed.
        "   AND a.internal_state = 'settled'"
        "   AND a.state IS NOT NULL AND a.state NOT IN (?,?)"
        "   AND a.rowid > ? AND a.rowid <= ?"
        " ORDER BY a.rowid LIMIT ?",
        (*SETTLED_DELIVERY, "dispatched", "inbox_only", after or 0, until, limit),
    )
    cache = {}
    observations = [faults.observation(
        product=product, fault_class="delivery_stalled",
        severity=faults.BROKEN if row["hold_reason"] else faults.DEGRADED,
        # The attempt's own classified state, never the delivery's hold reason. That reason
        # is set when a delivery gives up and it is MUTABLE: deriving identity from it meant
        # that the moment a retrying delivery hit its cap, every historical attempt re-derived
        # under a new signature and one continuous failure owned two faults and two issues.
        # Severity still reads it, because severity is not identity - the fault escalates
        # instead of forking.
        signature={"recipient": row["recipient_task_id"],
                   "attemptState": row["attempt_state"]},
        occurrence_key=f"delivery:{row['request_id']}",
        scope=scope_of(store, row["relationship_id"], scope, cache),
        detail=(f"an attempt to deliver to {row['recipient_task_id']} ended"
                f" {row['attempt_state']}"),
        evidence=[_evidence("row", f"attempts:{row['request_id']}", {
            "attemptState": row["attempt_state"], "deliveryState": row["delivery_state"],
            "holdReason": row["hold_reason"], "attemptCount": row["attempt_count"],
            "event": row["event_id"],
        })],
    ) for row in rows]
    return _page(observations, rows, "seq", after, limit, until)


def sync_faults(store, *, product, scope, limit=SWEEP_LIMIT, cursor=None) -> dict:
    """Coordination writes that gave up, grouped by the document they could not reach.

    Twenty jobs failing against one unreachable document are one problem. The target is the
    domain; the jobs are its occurrences.
    """
    # Validated before it reaches SQL, where LIMIT -1 means no limit at all.
    limit = faults.bounded(limit, "limit")
    after, until, _ = _rotation(store, cursor, "SELECT MAX(sync_id) FROM sync_outbox")
    rows = [] if until is None else store.all(
        "SELECT sync_id, relationship_id, issue_key, target, target_ref, attempts, last_error"
        "  FROM sync_outbox WHERE state = ? AND sync_id > ? AND sync_id <= ?"
        " ORDER BY sync_id LIMIT ?",
        (faults.FAILED, after or "", until, limit),
    )
    cache = {}
    observations = [faults.observation(
        product=product, fault_class="record_sync_failed", severity=faults.BROKEN,
        signature={"target": row["target"], "targetRef": row["target_ref"]},
        occurrence_key=f"sync:{row['sync_id']}:{row['attempts']}",
        scope={**scope_of(store, row["relationship_id"], scope, cache),
               "issueKey": row["issue_key"]},
        detail=f"a {row['target']} write exhausted its attempts",
        evidence=[_evidence("row", f"sync_outbox:{row['sync_id']}", {
            "attempts": row["attempts"], "lastError": row["last_error"],
            "relationship": row["relationship_id"], "targetRef": row["target_ref"],
        })],
    ) for row in rows]
    return _page(observations, rows, "sync_id", after, limit, until)


# The anchor key: text, zero-padded, so it orders the way the generations do.
_ANCHOR_KEY = "(g.relationship_id || ':' || printf('%020d', g.execution_generation))"


def observation_faults(store, *, product, scope, limit=SWEEP_LIMIT, cursor=None) -> dict:
    """Anchors the scheduler is not successfully reading.

    Started from the generations rather than from poll_observations, because that table has no
    row at all until a poll has been ATTEMPTED - so the anchor nobody ever managed to read is
    exactly the one a query over polls cannot see. An anchor whose turn has settled is
    excluded: the scheduler deliberately stops reading it, and ageing it out would report
    every quiet assignment as stalled.

    Never successfully polled is BROKEN rather than degraded, and that is not severity
    inflation. A degraded fault needs three occurrences to be filed, and a never-polled anchor
    produces exactly one - its key cannot change, because there is no successful poll and no
    new attempt time to key on - so at degraded, permanent starvation would be the one failure
    that could never reach the threshold.
    """
    # Validated before it reaches SQL, where LIMIT -1 means no limit at all.
    limit = faults.bounded(limit, "limit")
    after, until, _ = _rotation(
        store, cursor,
        # The key itself, ordered and bounded as text. Ordering by the columns disagrees with
        # the key whenever one relationship id is a prefix of another.
        "SELECT MAX(" + _ANCHOR_KEY + ") FROM generations g")
    rows = [] if until is None else store.all(
        "SELECT g.relationship_id, g.execution_generation, g.dispatch_turn_id,"
        "       p.turn_id, p.last_polled_at, p.last_attempt_at, p.last_error, p.last_status"
        "  FROM generations g"
        "  LEFT JOIN poll_observations p"
        "    ON p.relationship_id = g.relationship_id"
        "   AND p.execution_generation = g.execution_generation"
        # This generation's own anchor, not every poll ever recorded under it: an old failed
        # turn would otherwise report a healthy anchor as stalled forever.
        "   AND p.turn_id = g.dispatch_turn_id"
        " WHERE g.anchor_state = 'bound' AND g.dispatch_turn_id IS NOT NULL"
        "   AND NOT EXISTS (SELECT 1 FROM assignment_settlements s"
        "                    WHERE s.relationship_id = g.relationship_id"
        "                      AND s.turn_id = COALESCE(p.turn_id, g.dispatch_turn_id))"
        # An ATTEMPT must exist. A generation bound a moment ago has no poll row yet and is
        # not stalled - it has not been due yet - and raising on its absence filed a broken
        # fault for every healthy new assignment. What this therefore cannot see is a
        # scheduler that never attempts at all; that absence is recorded in the limits
        # rather than guessed at.
        "   AND p.turn_id IS NOT NULL AND p.last_attempt_at IS NOT NULL"
        "   AND (p.last_polled_at IS NULL OR p.last_error IS NOT NULL)"
        "   AND " + _ANCHOR_KEY + " > ? AND " + _ANCHOR_KEY + " <= ?"
        " ORDER BY " + _ANCHOR_KEY + " LIMIT ?",
        (after or "", until, limit),
    )
    observations = []
    cache = {}
    for row in rows:
        turn = row["turn_id"] or row["dispatch_turn_id"]
        never = row["last_polled_at"] is None
        observations.append(faults.observation(
            product=product, fault_class="observation_stalled",
            severity=faults.BROKEN if never else faults.DEGRADED,
            signature={"relationship": row["relationship_id"],
                       "generation": row["execution_generation"]},
            occurrence_key=(f"poll:{row['relationship_id']}:{row['execution_generation']}"
                            f":{turn}:{row['last_attempt_at']}"),
            scope=scope_of(store, row["relationship_id"], scope, cache),
            detail=("this anchor has never been successfully polled" if never else
                    "the most recent poll of this anchor failed"),
            evidence=[_evidence("row", f"poll_observations:{row['relationship_id']}", {
                "turn": turn, "lastPolledAt": row["last_polled_at"],
                "lastAttemptAt": row["last_attempt_at"], "lastError": row["last_error"],
                "lastStatus": row["last_status"],
            })],
        ))
    return _page(observations, rows, "anchor", after, limit, until)


def _in_streak(alias):
    """SQL: the delivery_withheld row under this alias is in its delivery's CURRENT streak.

    Nothing after it ended the streak: no settled send, no withholding by a person, and no
    withholding for another reason. A busy deferral in between ends nothing.
    """
    return (
        "NOT EXISTS (SELECT 1 FROM journal n WHERE n.subject = " + alias + ".subject"
        "   AND n.seq > " + alias + ".seq"
        "   AND (n.kind IN ('" + "','".join(STREAK_ENDS) + "')"
        "        OR (n.kind = 'delivery_withheld'"
        "            AND COALESCE(" + _REASON.format(t="n") + ", '')"
        "                != COALESCE(" + _REASON.format(t=alias) + ", ''))))"
    )


def refusal_faults(store, *, product, scope, limit=SWEEP_LIMIT, cursor=None) -> dict:
    """Settings and permission refusals before a send, one occurrence per refusal.

    Read from the append-only delivery_withheld journal rows, because the delivery row is
    overwritten on every pass and keeps only its newest state: three refusals leave one row
    and three journal records. Only the delivery's CURRENT streak counts (_in_streak), so a
    refusal that was later followed by a send, a person pausing the work or a different
    refusal is history, and is never counted again.

    Grouped by the relationship and the refusal: one assignment whose settings are wrong is one
    fault however many of its deliveries the same check refuses.
    """
    limit = faults.bounded(limit, "limit")
    after, until, _ = _rotation(store, cursor, "SELECT MAX(seq) FROM journal", integer=True)
    reasons = sorted(SETTINGS_REFUSALS)
    rows = [] if until is None else store.all(
        "SELECT j.seq, j.subject AS event_id, j.at, " + _REASON.format(t="j") + " AS reason,"
        "       CASE WHEN json_valid(j.detail) THEN json_extract(j.detail, '$.detail') END"
        "         AS refusal_detail,"
        "       d.relationship_id, d.recipient_task_id, d.state"
        "  FROM journal j JOIN deliveries d ON d.event_id = j.subject"
        " WHERE j.kind = 'delivery_withheld' AND j.seq > ? AND j.seq <= ?"
        "   AND d.state NOT IN (?,?,?)"
        "   AND " + _REASON.format(t="j") + " IN (" + ",".join("?" * len(reasons)) + ")"
        "   AND " + _in_streak("j") +
        " ORDER BY j.seq LIMIT ?",
        (after or 0, until, *SETTLED_DELIVERY, *reasons, limit),
    )
    cache = {}
    observations = [faults.observation(
        product=product, fault_class="delivery_refused", severity=faults.DEGRADED,
        signature={"relationship": row["relationship_id"], "errorCode": row["reason"]},
        occurrence_key=f"refused:{row['seq']}",
        scope=scope_of(store, row["relationship_id"], scope, cache),
        detail=f"a delivery to {row['recipient_task_id']} was refused before sending:"
               f" {row['reason']}",
        evidence=[_evidence("row", f"journal:{row['seq']}", {
            "event": row["event_id"], "reason": row["reason"],
            "detail": row["refusal_detail"] if isinstance(row["refusal_detail"], str) else None,
            "deliveryState": row["state"], "at": row["at"],
        })],
    ) for row in rows]
    return _page(observations, rows, "seq", after, limit, until)


def managed_start_faults(store, *, product, scope, limit=SWEEP_LIMIT, cursor=None) -> dict:
    """Managed starts the host answered without publishing a child.

    An armed request whose recorded receipt is anything but accepted. The registry replaces a
    non-publishing receipt with a later one and attaches only an accepted one, so the fault
    clears exactly when a later receipt for that request is accepted.
    """
    limit = faults.bounded(limit, "limit")
    after, until, _ = _rotation(store, cursor,
                                "SELECT MAX(request_id) FROM managed_start_requests")
    rows = [] if until is None else store.all(
        "SELECT request_id, issue_key, receipt_status, workspace, revision, updated_at"
        "  FROM managed_start_requests"
        " WHERE state = 'create_armed' AND receipt_status IS NOT NULL AND receipt_status != ?"
        "   AND request_id > ? AND request_id <= ?"
        " ORDER BY request_id LIMIT ?",
        (ACCEPTED_RECEIPT, after or "", until, limit),
    )
    observations = [faults.observation(
        product=product, fault_class="managed_start_failed", severity=faults.BROKEN,
        signature={"issueKey": row["issue_key"]},
        occurrence_key=f"managed:{row['request_id']}:{row['receipt_status']}",
        scope={**dict(scope or {}), "issueKey": row["issue_key"]},
        detail=f"a managed start for {row['issue_key']} was answered"
               f" {row['receipt_status']} without publishing a child",
        evidence=[_evidence("row", f"managed_start_requests:{row['request_id']}", {
            "receiptStatus": row["receipt_status"], "revision": row["revision"],
            "workspace": row["workspace"], "updatedAt": row["updated_at"],
        })],
    ) for row in rows]
    return _page(observations, rows, "request_id", after, limit, until)


def managed_readings(store, selection, *, limit=MANAGED_READINGS_PER_SWEEP, cursor=None,
                     now=None) -> dict:
    """CRW-180 readings for turns settled under attached managed starts, read by this relay.

    omitted.observe is consumed exactly as reporting-show consumes it: it is handed the store
    selection, reads the marker and the store read-only, and writes nothing. Only the current
    generation of a relationship still on its managed start is read, and never the standby
    turn, which is the bootstrap and establishes nothing. An observer error is a gap, never a
    silent skip.

    A settlement is recorded once per terminal status, so a turn read twice in one page would
    be two readings of one turn; only its first settlement row is read.
    """
    from . import marker, omitted

    limit = faults.bounded(limit, "limit")
    after, until, _ = _rotation(store, cursor, "SELECT MAX(rowid) FROM assignment_settlements",
                                integer=True)
    rows = [] if until is None else store.all(
        # CROSS JOIN keeps the managed requests outermost, so the cost follows the managed
        # turns rather than every settlement this store has ever recorded.
        "SELECT s.rowid AS seq, s.relationship_id, s.thread_id, s.turn_id, m.request_id,"
        "       m.marker_root, m.workspace, m.dispatch_request_id"
        "  FROM managed_start_requests m"
        "  CROSS JOIN assignment_settlements s"
        "  JOIN relationships r ON r.relationship_id = m.relationship_id"
        " WHERE m.state = 'attached' AND s.relationship_id = m.relationship_id"
        "   AND s.thread_id = m.child_task_id"
        "   AND r.execution_generation = m.execution_generation AND r.superseded_by IS NULL"
        "   AND (m.standby_turn_id IS NULL OR s.turn_id != m.standby_turn_id)"
        "   AND s.rowid > ? AND s.rowid <= ?"
        "   AND NOT EXISTS (SELECT 1 FROM assignment_settlements e"
        "                    WHERE e.relationship_id = s.relationship_id"
        "                      AND e.thread_id = s.thread_id AND e.turn_id = s.turn_id"
        "                      AND e.rowid < s.rowid)"
        " ORDER BY s.rowid LIMIT ?",
        (after or 0, until, limit),
    )
    readings, gaps = [], []
    for row in rows:
        try:
            reading = omitted.observe(
                selection, root=row["marker_root"], workspace=row["workspace"],
                assignment=marker.assignment_id(row["dispatch_request_id"]),
                session=row["thread_id"], turn=row["turn_id"], now=now or _now(store))
        except Exception as error:  # noqa: BLE001 - an observer error is a gap, not an outage
            gaps.append({"gap": "managed_reading_failed", "relationId": row["relationship_id"],
                         "reason": f"{type(error).__name__}: {error}"})
            continue
        if not isinstance(reading, dict):
            gaps.append({"gap": "managed_reading_failed", "relationId": row["relationship_id"],
                         "reason": "the observer returned no reading"})
            continue
        if not _named(reading.get("relationshipId")):
            # An unmeasured reading can stop before it resolves the relationship. It is still
            # a reading OF this one: the selection that asked for it named it.
            reading = dict(reading, relationshipId=row["relationship_id"])
        readings.append(reading)
    page = _page([], rows, "seq", after, limit, until)
    return {"readings": readings, "gaps": gaps, "cursor": page["cursor"],
            "filled": page["filled"], "complete": page["complete"]}


def _reading_key(reading):
    """(relationship, turn) for a reading this can interpret, or None."""
    if not isinstance(reading, dict) or reading.get("schema") != OBSERVATION_SCHEMA:
        return None
    selectors = reading.get("selectors")
    turn = selectors.get("turn") if isinstance(selectors, dict) else None
    relationship = reading.get("relationshipId")
    if _named(relationship) and _named(turn):
        return relationship, turn
    return None


def _last_per_turn(readings) -> list:
    """The batch reduced to the LAST reading of each relationship and turn, in order.

    Done before paging, so one sweep never emits an active and a clearing observation for the
    same turn: a turn read unreported and then reported is read as reported, whichever page
    either reading would otherwise have fallen on. A reading this cannot interpret keeps its own
    place, so it is still named as a gap.
    """
    order = {}
    for index, reading in enumerate(readings):
        key = _reading_key(reading) or ("#unusable", index)
        order.pop(key, None)
        order[key] = reading
    return list(order.values())


def reading_faults(readings, *, product, scope, store=None, limit=SWEEP_LIMIT,
                   after=0) -> dict:
    """What CRW-180's reporting readings owe, including the ones that clear.

    Bounded like every store source (B8): more than MAX_READINGS are refused, the batch is
    reduced to the last reading per turn, and one call reads at most limit of them from after,
    returning where the next call continues.

    Only a reading that says REPORTED clears an omission. unmeasured does not: a failed
    evidence read is the absence of an answer, and treating it as recovery would close a fault
    on the strength of a file nobody could read.
    """
    limit = faults.bounded(limit, "limit")
    if isinstance(after, bool) or not isinstance(after, int) or not 0 <= after <= MAX_READINGS:
        raise faults.FaultRefused(RefusalReason.FAULT_OBSERVATION_MALFORMED,
                                  f"readings after is an integer from 0 to {MAX_READINGS},"
                                  f" not {after!r}")
    if readings is None:
        readings = []
    if not isinstance(readings, (list, tuple)):
        raise faults.FaultRefused(RefusalReason.FAULT_OBSERVATION_MALFORMED,
                                  "readings are a list of reporting-observation/1 objects")
    if len(readings) > MAX_READINGS:
        raise faults.FaultRefused(
            RefusalReason.FAULT_OBSERVATION_MALFORMED,
            f"{len(readings)} readings is more than the {MAX_READINGS} one call reads; hand"
            f" them in batches")
    reduced = _last_per_turn(readings)
    observations = []
    gaps = []
    cache = {}
    for reading in reduced[after:after + limit]:
        # Named rather than dropped. A sweep that reported success while silently discarding
        # the readings it was handed would be the quiet failure this whole module exists to
        # stop somebody having to notice.
        if not isinstance(reading, dict) or reading.get("schema") != OBSERVATION_SCHEMA:
            gaps.append({"gap": "reading_unusable",
                         "reason": f"not an object under {OBSERVATION_SCHEMA}"})
            continue
        key = _reading_key(reading)
        if key is None:
            relationship = reading.get("relationshipId")
            gaps.append({"gap": "reading_unusable", "relationId": relationship
                         if isinstance(relationship, str) else None,
                         "reason": "the reading names no usable relationship or turn"})
            continue
        relationship, turn = key
        state = reading.get("reportingState")
        signature = {"relationship": relationship, "turn": turn}
        placed = scope_of(store, relationship, scope, cache) if store is not None else scope
        if state in ESTABLISHED:
            # This reading established something about this turn, which is exactly what an
            # unmeasured notice about it says nobody had.
            observations.append(faults.observation(
                product=product, fault_class="observation_unmeasured", severity=faults.NOTICE,
                signature=signature,
                occurrence_key=f"measured:{relationship}:{turn}:{state}",
                scope=placed, cleared=True,
                detail="a later reading of this turn established something",
                evidence=[_evidence("reading", OBSERVATION_SCHEMA,
                                    {"reportingState": state})],
            ))
            # A notice recorded per RELATIONSHIP before notices were per turn is answered by
            # any establishing reading of it, as it always was. One constant key, so it is
            # recorded at most once; with no such notice nothing is recorded at all.
            observations.append(faults.observation(
                product=product, fault_class="observation_unmeasured", severity=faults.NOTICE,
                signature={"relationship": relationship},
                occurrence_key=f"measured:{relationship}",
                scope=placed, cleared=True,
                detail="a later reading of this relationship established something",
                evidence=[_evidence("reading", OBSERVATION_SCHEMA,
                                    {"reportingState": state, "turn": turn})],
            ))
        if state == UNREPORTED:
            observations.append(faults.observation(
                product=product, fault_class="report_omitted", severity=faults.BROKEN,
                signature=signature, occurrence_key=f"observation:{relationship}:{turn}",
                scope=placed,
                detail="an admitted turn settled without a report, so what it owed is owed",
                evidence=[_evidence("reading", OBSERVATION_SCHEMA, {
                    "reportingState": state, "reason": reading.get("reason"),
                    "executionGeneration": reading.get("executionGeneration"),
                })],
            ))
        elif state == REPORTED:
            observations.append(faults.observation(
                product=product, fault_class="report_omitted", severity=faults.BROKEN,
                signature=signature,
                occurrence_key=f"observation:{relationship}:{turn}:reported",
                scope=placed, cleared=True,
                detail="a later reading of this turn found its report",
                evidence=[_evidence("reading", OBSERVATION_SCHEMA, {"reportingState": state})],
            ))
        elif state == UNMEASURED:
            observations.append(faults.observation(
                product=product, fault_class="observation_unmeasured", severity=faults.NOTICE,
                signature=signature,
                occurrence_key=f"unmeasured:{relationship}:{turn}",
                scope=placed,
                detail="nothing was established about whether this turn owed a report",
                evidence=[_evidence("reading", OBSERVATION_SCHEMA, {
                    "reportingState": state, "reason": reading.get("reason")})],
            ))
        elif state not in ESTABLISHED:
            # Well formed, and carrying a state this cannot interpret. Absorbing it would let
            # fault-sweep report success while discarding evidence somebody handed it.
            gaps.append({"gap": "reading_unknown_state", "relationId": relationship,
                         "reason": f"reportingState {state!r} is not one this sweep knows"})
    following = after + limit
    return {"observations": observations, "gaps": gaps,
            "next": following if following < len(reduced) else None,
            "total": len(reduced)}


def _identity(entry):
    return entry["faultClass"], faults.canonical_signature(entry["signature"])


def sweep(store, *, product="crw", scope=None, readings=(), limit=SWEEP_LIMIT,
          policy=None, readings_after=0, selection=None, now=None,
          managed_limit=MANAGED_READINGS_PER_SWEEP) -> dict:
    """Every fault this store currently shows, plus the clears its own absence establishes.

    A source that filled its page is NOT complete, and an incomplete read establishes nothing
    about absence: clearing from it would withdraw a still-broken fault that happened to sort
    past the bound. Clears therefore come only from recovered(), which asks each fault's own
    source directly.

    Invariant 16 is enforced here, on the batch as a whole: no source emits an active and a
    clearing observation for one fault in one sweep. Readings are reduced to the last one per
    turn before paging, and a clear for a fault this same sweep derived as active is dropped -
    the active reading wins until a later sweep no longer produces it.

    With a selection, the relay's own managed turns are read through omitted.observe, a
    bounded number per sweep, beside any readings the caller handed in.
    """
    # Validated before it reaches SQL, where LIMIT -1 means no limit at all.
    limit = faults.bounded(limit, "limit")
    scope = dict(scope or {})
    cursors = read_cursors(store)
    by_class = {
        "delivery_stalled": delivery_faults(
            store, product=product, scope=scope, limit=limit, policy=policy,
            cursor=cursors.get("delivery_stalled")),
        "record_sync_failed": sync_faults(
            store, product=product, scope=scope, limit=limit,
            cursor=cursors.get("record_sync_failed")),
        "observation_stalled": observation_faults(
            store, product=product, scope=scope, limit=limit,
            cursor=cursors.get("observation_stalled")),
        "delivery_retrying": retry_faults(
            store, product=product, scope=scope, limit=limit,
            cursor=cursors.get("delivery_retrying")),
        "delivery_refused": refusal_faults(
            store, product=product, scope=scope, limit=limit,
            cursor=cursors.get("delivery_refused")),
        "managed_start_failed": managed_start_faults(
            store, product=product, scope=scope, limit=limit,
            cursor=cursors.get("managed_start_failed")),
    }
    complete = tuple(name for name, page in by_class.items() if page["complete"])
    if "delivery_retrying" not in complete:
        # Both pages feed delivery_stalled, so an incomplete retry page is an incomplete read
        # of that class however complete the hold-reason page was.
        complete = tuple(name for name in complete if name != "delivery_stalled")
    derived = [entry for page in by_class.values() for entry in page["observations"]]
    read = reading_faults(readings, product=product, scope=scope, store=store, limit=limit,
                          after=readings_after)
    gaps = list(read["gaps"])
    managed = None
    managed_observations = []
    if selection is not None:
        managed = managed_readings(store, selection, limit=managed_limit,
                                   cursor=cursors.get("managed_readings"), now=now)
        gaps.extend(managed["gaps"])
        answered = reading_faults(managed["readings"], product=product, scope=scope,
                                  store=store, limit=max(1, len(managed["readings"])))
        managed_observations = answered["observations"]
        gaps.extend(answered["gaps"])
    observations = derived + managed_observations + read["observations"]
    recovery = recovered(store, derived, product=product, scope=scope, limit=limit,
                         complete=complete, cursor=cursors.get("recovered"))
    active = {_identity(entry) for entry in observations if not entry["cleared"]}
    observations = [entry for entry in observations
                    if not (entry["cleared"] and _identity(entry) in active)]
    clears = [entry for entry in recovery["clears"] if _identity(entry) not in active]
    positions = {name: page["cursor"] for name, page in by_class.items()}
    positions["recovered"] = recovery["cursor"]
    if managed is not None:
        positions["managed_readings"] = managed["cursor"]
    return {
        "observations": observations,
        "clears": clears,
        "gaps": gaps,
        "completeSources": list(complete),
        "cursors": dict(positions),
        "_advanced": positions,
        "readingsNext": read["next"],
        "readingsTotal": read["total"],
        "limits": f"each source is read at most {limit} rows per sweep, in rotations bounded by"
                  f" its upper key when the rotation started, so every rotation reaches the"
                  f" end; readings are reduced to the last per turn and read {limit} at a"
                  f" time from readingsNext. A class this sweep does not derive is never"
                  f" cleared by its absence here",
    }


def recovered(store, derived, *, product, scope, limit=SWEEP_LIMIT, complete=DERIVED,
              cursor=None) -> dict:
    """Open faults whose own source no longer produces them.

    Asked of each fault DIRECTLY rather than by differencing against a page. A page is a
    bounded prefix, so with more rows than one page no page is ever the whole source, and a
    rule that required one would have stopped clearing anything at all the moment a store got
    busy - which is exactly when it matters. An existence query for one signature is exact
    however large the source is.

    The ledger side rotates too, like every source: always reading the first page of open
    faults left later ones open forever behind a persistent prefix.
    """
    # Validated before it reaches SQL, where LIMIT -1 means no limit at all.
    limit = faults.bounded(limit, "limit")
    after, until, _ = _rotation(store, cursor, "SELECT MAX(fault_id) FROM fault_ledger")
    rows = [] if until is None else store.all(
        "SELECT fault_id, fault_class, signature, cycle, scope FROM fault_ledger"
        " WHERE product = ? AND state IN (?,?,?) AND cleared_at IS NULL"
        "   AND fault_class IN (" + ",".join("?" * len(DERIVED)) + ")"
        "   AND fault_id > ? AND fault_id <= ?"
        " ORDER BY fault_id LIMIT ?",
        (product, faults.OBSERVED, faults.OPEN, faults.FIX_PENDING, *DERIVED, after or "",
         until, limit),
    )
    clears = []
    for row in rows:
        if still_present(store, row["fault_class"], json.loads(row["signature"])):
            continue
        last = store.one(
            "SELECT occurrence_id FROM fault_occurrences"
            " WHERE fault_id = ? AND cleared = 0 ORDER BY rowid DESC LIMIT 1",
            (row["fault_id"],))
        try:
            placed = json.loads(row["scope"])
        except (TypeError, ValueError):
            placed = dict(scope or {})
        clears.append(faults.observation(
            product=product, fault_class=row["fault_class"], severity=faults.NOTICE,
            signature=json.loads(row["signature"]),
            occurrence_key=f"cleared:after:{last['occurrence_id'] if last else row['cycle']}",
            # The fault's OWN scope. Clearing it with the sweep's empty scope rewrote a
            # crw:CRW fault to a bare crw, and a later fix then queued against no tracker.
            scope=placed, cleared=True,
            detail="this sweep read the source and no longer derives this fault",
            evidence=[_evidence("sweep", row["fault_class"], {"derived": False})],
        ))
    page = _page(clears, rows, "fault_id", after, limit, until)
    return {"clears": clears, "cursor": page["cursor"]}


def still_present(store, fault_class, signature) -> dict:
    """Does this fault's own source still produce it? Asked as an existence query.

    Returns the row when it does and None when it does not, so a caller can tell an absence
    from a class this cannot ask about - which is never cleared by absence at all.
    """
    if fault_class == "delivery_stalled":
        # Both shapes that derive this class, because either one still produces the fault.
        # The hold-reason page keys on the delivery's LATEST attempt state, which is NULL when
        # a delivery was refused before any attempt; the retry page keys on ANY attempt in
        # that state. Asking only the second one called every held-but-never-attempted
        # delivery recovered, and asking only the first made a fault whose evidence sat
        # further back alternate between withdrawn and reopened on every sweep.
        return store.one(
            "SELECT 1 FROM deliveries d"
            " WHERE d.recipient_task_id = ? AND d.state NOT IN (?,?,?)"
            # Settled attempts in both branches, for the reason the derivations give: an
            # in-flight row's state is provisional, and letting it count as presence would
            # keep a recovered fault open for as long as some unrelated send was running.
            "   AND (COALESCE((SELECT a.state FROM attempts a WHERE a.event_id = d.event_id"
            "                     AND a.internal_state = 'settled'"
            "                   ORDER BY a.attempt_no DESC LIMIT 1), '') = COALESCE(?, '')"
            "        OR EXISTS (SELECT 1 FROM attempts a2 WHERE a2.event_id = d.event_id"
            "                     AND a2.internal_state = 'settled' AND a2.state = ?))"
            " LIMIT 1",
            (signature.get("recipient"), *SETTLED_DELIVERY, signature.get("attemptState"),
             signature.get("attemptState")))
    if fault_class == "record_sync_failed":
        return store.one(
            "SELECT 1 FROM sync_outbox WHERE state = ? AND target = ? AND target_ref = ?"
            " LIMIT 1",
            (faults.FAILED, signature.get("target"), signature.get("targetRef")))
    if fault_class == "observation_stalled":
        return store.one(
            "SELECT 1 FROM generations g"
            "  LEFT JOIN poll_observations p"
            "    ON p.relationship_id = g.relationship_id"
            "   AND p.execution_generation = g.execution_generation"
            "   AND p.turn_id = g.dispatch_turn_id"
            " WHERE g.relationship_id = ? AND g.execution_generation = ?"
            "   AND g.anchor_state = 'bound' AND g.dispatch_turn_id IS NOT NULL"
            "   AND NOT EXISTS (SELECT 1 FROM assignment_settlements s"
            "                    WHERE s.relationship_id = g.relationship_id"
            "                      AND s.turn_id = COALESCE(p.turn_id, g.dispatch_turn_id))"
            "   AND p.turn_id IS NOT NULL AND p.last_attempt_at IS NOT NULL"
            "   AND (p.last_polled_at IS NULL OR p.last_error IS NOT NULL)"
            " LIMIT 1",
            (signature.get("relationship"), signature.get("generation")))
    if fault_class == "delivery_refused":
        # The same streak the derivation counts: a refusal for this reason, on an unsettled
        # delivery of this relationship, that nothing has ended since.
        return store.one(
            "SELECT 1 FROM deliveries d JOIN journal j ON j.subject = d.event_id"
            " WHERE d.relationship_id = ? AND d.state NOT IN (?,?,?)"
            "   AND j.kind = 'delivery_withheld'"
            "   AND " + _REASON.format(t="j") + " = ?"
            "   AND " + _in_streak("j") +
            " LIMIT 1",
            (signature.get("relationship"), *SETTLED_DELIVERY, signature.get("errorCode")))
    if fault_class == "managed_start_failed":
        return store.one(
            "SELECT 1 FROM managed_start_requests WHERE issue_key = ?"
            "   AND state = 'create_armed' AND receipt_status IS NOT NULL"
            "   AND receipt_status != ? LIMIT 1",
            (signature.get("issueKey"), ACCEPTED_RECEIPT))
    # A class this cannot ask about is never cleared by absence.
    return {"unaskable": True}


def read_cursors(store) -> dict:
    """Each source's stored rotation position, as written."""
    return {row["source"]: row["position"]
            for row in store.all("SELECT source, position FROM fault_cursors")}


def write_cursors(store, positions) -> None:
    """Advance each source, including back to the start. A sweep that read nothing new still
    records where it got to, so the rotation cannot stall on one page.

    A position is {"at", "until"} or None. The pages column predates rotations bounded by
    their upper key and is written as zero; nothing reads it.
    """
    now = _now(store)
    with store.transaction() as db:
        for source, position in positions.items():
            stored = None if position is None else json.dumps(position, sort_keys=True)
            db.execute(
                "INSERT INTO fault_cursors (source, position, pages, updated_at)"
                " VALUES (?,?,0,?)"
                " ON CONFLICT(source) DO UPDATE SET position = excluded.position,"
                "   pages = 0, updated_at = excluded.updated_at",
                (source, stored, now))


def _now(store) -> str:
    from datetime import datetime, timezone

    return datetime.now(timezone.utc).isoformat(timespec="microseconds")


def record_all(ledger, batch, *, store=None) -> dict:
    """Record a sweep's answer, THEN advance its cursors.

    In that order on purpose: advancing before the rows are recorded means a failure here
    skips that page until the rotation comes round again.

    One refused observation is ONE gap. The rest of the batch is still recorded, because a
    single malformed row taking every other fault in the sweep down with it is the quiet
    failure this path exists to prevent. A store failure is not a refusal and still stops the
    batch, before any cursor moves.
    """
    results = []
    gaps = list(batch.get("gaps", ()))
    for entry in list(batch.get("observations", ())) + list(batch.get("clears", ())):
        try:
            results.append(ledger.record(entry))
        except faults.FaultRefused as refusal:
            gaps.append({
                "gap": "observation_refused",
                "faultClass": entry.get("faultClass") if isinstance(entry, dict) else None,
                "reason": f"{refusal.reason.value}: {refusal}",
            })
    if store is not None and batch.get("_advanced") is not None:
        write_cursors(store, batch["_advanced"])
    recorded = [entry for entry in results if entry.get("recorded")]
    queued = [entry["publication"] for entry in recorded
              if entry.get("publication") and entry["publication"].get("queued")]
    return {"read": len(results), "recorded": len(recorded), "queued": len(queued),
            "gaps": gaps, "results": results}
