"""Deriving fault observations from rows this store already holds.

Reading only. Nothing here writes: it answers what the store currently shows, and
FaultLedger.record decides what that means for a fault. Keeping the two apart is what lets
the same derivation run on a daemon tick, from an operator's command, and inside a test
without any of them differing.

Bounded on purpose. This runs on every tick beside passes that are each capped, so every
query carries a LIMIT and the pass as a whole is capped by SWEEP_LIMIT per source. A sweep
that could grow with the store would be the one pass able to starve the others.

CLEARING is derived the same way, and that is the part worth reading. A class whose source is
this store is cleared by RE-DERIVING it: if a sweep reads the delivery rows and no longer
produces a fault's signature, the sweep positively established the absence and says so. A
class whose source is a reading handed in from outside is never cleared that way, because a
reading nobody supplied establishes nothing - that is the difference between "the thing is
gone" and "nobody looked".
"""

import json

from . import faults
from .policy import RetryPolicy

SWEEP_LIMIT = 32

STORE_SOURCE = "store"
READING_SOURCE = "reading"

# Which classes this module derives, and therefore which ones it may clear by absence.
DERIVED = ("delivery_stalled", "record_sync_failed", "observation_stalled")

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


def _named(value):
    """A name is a non-blank string, which a list and a blank both fail."""
    return isinstance(value, str) and bool(value.strip())


def _page(observations, rows, key, cursor, limit) -> dict:
    """One page of a source, with where it got to and whether it saw the whole thing.

    complete means this call read the source from the START and did not fill its page, which
    is the only shape that establishes an absence. A page that filled, or one that resumed
    from a cursor, has seen part of the source and can clear nothing.
    """
    if key == "anchor":
        position = (f"{rows[-1]['relationship_id']}:{rows[-1]['execution_generation']:020d}"
                    if rows else None)
    else:
        position = rows[-1][key] if rows else None
    filled = len(rows) >= limit
    return {
        "observations": observations,
        # Wrap to the start when the page was short: the next sweep then re-reads from the
        # beginning rather than sitting past the end seeing nothing forever.
        "cursor": position if filled else None,
        "complete": cursor is None and not filled,
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
    policy = policy or RetryPolicy()
    rows = store.all(
        "SELECT d.event_id, d.relationship_id, d.recipient_task_id, d.state, d.hold_reason,"
        "       d.attempt_count,"
        "       (SELECT a.request_id FROM attempts a WHERE a.event_id = d.event_id"
        "         ORDER BY a.attempt_no DESC LIMIT 1) AS last_request,"
        "       (SELECT a.state FROM attempts a WHERE a.event_id = d.event_id"
        "         ORDER BY a.attempt_no DESC LIMIT 1) AS last_state"
        "  FROM deliveries d"
        " WHERE d.hold_reason IS NOT NULL AND d.state NOT IN (?,?,?)"
        "   AND d.event_id > ?"
        " ORDER BY d.event_id LIMIT ?",
        (*SETTLED_DELIVERY, cursor or "", limit),
    )
    observations = []
    cache = {}
    for row in rows:
        capped = (row["attempt_count"] or 0) >= policy.max_attempts
        signature = {"recipient": row["recipient_task_id"], "cause": row["hold_reason"],
                     "attemptState": row["last_state"]}
        observations.append(faults.observation(
            product=product, fault_class="delivery_stalled",
            severity=faults.BROKEN if capped else faults.DEGRADED,
            signature=signature,
            occurrence_key=f"delivery:{row['last_request'] or row['event_id']}",
            scope=scope_of(store, row["relationship_id"], scope, cache),
            detail=f"a delivery to {row['recipient_task_id']} is held: {row['hold_reason']}",
            evidence=[_evidence("row", f"deliveries:{row['event_id']}", {
                "state": row["state"], "holdReason": row["hold_reason"],
                "attemptCount": row["attempt_count"], "lastAttemptState": row["last_state"],
                "relationship": row["relationship_id"],
            })],
        ))
    return _page(observations, rows, "event_id", cursor, limit)


def sync_faults(store, *, product, scope, limit=SWEEP_LIMIT, cursor=None) -> dict:
    """Coordination writes that gave up, grouped by the document they could not reach.

    Twenty jobs failing against one unreachable document are one problem. The target is the
    domain; the jobs are its occurrences.
    """
    rows = store.all(
        "SELECT sync_id, relationship_id, issue_key, target, target_ref, attempts, last_error"
        "  FROM sync_outbox WHERE state = ? AND sync_id > ? ORDER BY sync_id LIMIT ?",
        (faults.FAILED, cursor or "", limit),
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
    return _page(observations, rows, "sync_id", cursor, limit)


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
    rows = store.all(
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
        # Zero-padded, because the cursor is compared as TEXT and the rows are ordered
        # numerically: without it a cursor ending at generation 9 hid generation 10.
        "   AND (g.relationship_id || ':' || printf('%020d', g.execution_generation)) > ?"
        " ORDER BY g.relationship_id, g.execution_generation LIMIT ?",
        (cursor or "", limit),
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
    return _page(observations, rows, "anchor", cursor, limit)


def reading_faults(readings, *, product, scope, store=None) -> dict:
    """What CRW-180's reporting readings owe, including the ones that clear.

    Only a reading that says REPORTED clears an omission. unmeasured does not: a failed
    evidence read is the absence of an answer, and treating it as recovery would close a fault
    on the strength of a file nobody could read.
    """
    observations = []
    gaps = []
    cache = {}
    for reading in readings or ():
        # Named rather than dropped. A sweep that reported success while silently discarding
        # the readings it was handed would be the quiet failure this whole module exists to
        # stop somebody having to notice.
        if not isinstance(reading, dict) or reading.get("schema") != OBSERVATION_SCHEMA:
            gaps.append({"gap": "reading_unusable",
                         "reason": f"not an object under {OBSERVATION_SCHEMA}"})
            continue
        state = reading.get("reportingState")
        relationship = reading.get("relationshipId")
        selectors = reading.get("selectors")
        turn = selectors.get("turn") if isinstance(selectors, dict) else None
        if not (_named(relationship) and _named(turn)):
            gaps.append({"gap": "reading_unusable", "relationId": relationship
                         if isinstance(relationship, str) else None,
                         "reason": "the reading names no usable relationship or turn"})
            continue
        signature = {"relationship": relationship, "turn": turn}
        placed = scope_of(store, relationship, scope, cache) if store is not None else scope
        if state in ESTABLISHED:
            # This reading established something about the relationship, which is exactly
            # what an unmeasured notice says nobody had. Leaving it open would keep a notice
            # about a question that has since been answered.
            observations.append(faults.observation(
                product=product, fault_class="observation_unmeasured", severity=faults.NOTICE,
                signature={"relationship": relationship},
                occurrence_key=f"measured:{relationship}:{turn}:{state}",
                scope=placed, cleared=True,
                detail="a later reading of this relationship established something",
                evidence=[_evidence("reading", OBSERVATION_SCHEMA,
                                    {"reportingState": state})],
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
                signature={"relationship": relationship},
                occurrence_key=f"unmeasured:{relationship}:{turn}",
                scope=placed,
                detail="nothing was established about whether a report was owed here",
                evidence=[_evidence("reading", OBSERVATION_SCHEMA, {
                    "reportingState": state, "reason": reading.get("reason")})],
            ))
    return {"observations": observations, "gaps": gaps}


def sweep(store, *, product="crw", scope=None, readings=(), limit=SWEEP_LIMIT,
          policy=None) -> dict:
    """Every fault this store currently shows, plus the clears its own absence establishes.

    A source that filled its page is NOT complete, and an incomplete read establishes nothing
    about absence: clearing from it would withdraw a still-broken fault that happened to sort
    past the bound. Only the classes whose source was read to the end can clear.
    """
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
    }
    complete = tuple(name for name, page in by_class.items() if page["complete"])
    derived = [entry for page in by_class.values() for entry in page["observations"]]
    read = reading_faults(readings, product=product, scope=scope, store=store)
    observations = derived + read["observations"]
    recovery = recovered(store, derived, product=product, scope=scope, limit=limit,
                         complete=complete, cursor=cursors.get("recovered"))
    positions = {name: page["cursor"] for name, page in by_class.items()}
    positions["recovered"] = recovery["cursor"]
    return {
        "observations": observations,
        "clears": recovery["clears"],
        "gaps": read["gaps"],
        "completeSources": list(complete),
        "cursors": positions,
        "limits": f"each source is read at most {limit} rows, resuming where the last sweep"
                  f" stopped and wrapping at the end, so nothing past one page is starved."
                  f" A source that did not read the whole thing from the start clears"
                  f" nothing, and a class this sweep does not derive is never cleared by its"
                  f" absence here",
    }


def recovered(store, derived, *, product, scope, limit=SWEEP_LIMIT, complete=DERIVED,
              cursor=None) -> dict:
    """Open faults whose own source no longer produces them.

    Asked of each fault DIRECTLY rather than by differencing against a page. A page is a
    bounded prefix, so with more rows than one page no page is ever the whole source, and a
    rule that required one would have stopped clearing anything at all the moment a store got
    busy - which is exactly when it matters. An existence query for one signature is exact
    however large the source is.

    The ledger side rotates too: always reading the first page of open faults left later ones
    open forever behind a persistent prefix.
    """
    rows = store.all(
        "SELECT fault_id, fault_class, signature, cycle, scope FROM fault_ledger"
        " WHERE product = ? AND state IN (?,?,?) AND cleared_at IS NULL"
        "   AND fault_class IN (" + ",".join("?" * len(DERIVED)) + ")"
        "   AND fault_id > ?"
        " ORDER BY fault_id LIMIT ?",
        (product, faults.OBSERVED, faults.OPEN, faults.FIX_PENDING, *DERIVED, cursor or "",
         limit),
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
    return {"clears": clears,
            "cursor": rows[-1]["fault_id"] if len(rows) >= limit else None}


def still_present(store, fault_class, signature) -> dict:
    """Does this fault's own source still produce it? Asked as an existence query.

    Returns the row when it does and None when it does not, so a caller can tell an absence
    from a class this cannot ask about - which is never cleared by absence at all.
    """
    if fault_class == "delivery_stalled":
        return store.one(
            "SELECT 1 FROM deliveries d WHERE d.recipient_task_id = ? AND d.hold_reason = ?"
            "  AND d.state NOT IN (?,?,?)"
            "  AND COALESCE((SELECT a.state FROM attempts a WHERE a.event_id = d.event_id"
            "                 ORDER BY a.attempt_no DESC LIMIT 1), '') = COALESCE(?, '')"
            " LIMIT 1",
            (signature.get("recipient"), signature.get("cause"), *SETTLED_DELIVERY,
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
    # A class this cannot ask about is never cleared by absence.
    return {"unaskable": True}


def read_cursors(store) -> dict:
    return {row["source"]: row["position"]
            for row in store.all("SELECT source, position FROM fault_cursors")}


def write_cursors(store, positions) -> None:
    """Advance each source, including back to the start. A sweep that read nothing new still
    records where it got to, so the rotation cannot stall on one page."""
    now = _now(store)
    with store.transaction() as db:
        for source, position in positions.items():
            db.execute(
                "INSERT INTO fault_cursors (source, position, updated_at) VALUES (?,?,?)"
                " ON CONFLICT(source) DO UPDATE SET position = excluded.position,"
                "   updated_at = excluded.updated_at",
                (source, position, now))


def _now(store) -> str:
    from datetime import datetime, timezone

    return datetime.now(timezone.utc).isoformat(timespec="microseconds")


def record_all(ledger, batch, *, store=None) -> dict:
    """Record a sweep's answer, THEN advance its cursors.

    In that order on purpose: advancing before the rows are recorded means a failure here
    skips that page until the rotation comes round again.
    """
    results = []
    for entry in list(batch.get("observations", ())) + list(batch.get("clears", ())):
        results.append(ledger.record(entry))
    if store is not None and batch.get("cursors") is not None:
        write_cursors(store, batch["cursors"])
    recorded = [entry for entry in results if entry.get("recorded")]
    queued = [entry["publication"] for entry in recorded
              if entry.get("publication") and entry["publication"].get("queued")]
    return {"read": len(results), "recorded": len(recorded), "queued": len(queued),
            "gaps": list(batch.get("gaps", ())), "results": results}
