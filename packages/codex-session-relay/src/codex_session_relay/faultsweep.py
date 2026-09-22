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

# A delivery that is nowhere any more, so an unmoving row in one of these is not a fault.
SETTLED_DELIVERY = ("dispatched", "inbox_only", "superseded")


def _named(value):
    """A name is a non-blank string, which a list and a blank both fail."""
    return isinstance(value, str) and bool(value.strip())


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


def delivery_faults(store, *, product, scope, limit=SWEEP_LIMIT, policy=None) -> list:
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
        " ORDER BY d.event_id LIMIT ?",
        (*SETTLED_DELIVERY, limit),
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
    return observations


def sync_faults(store, *, product, scope, limit=SWEEP_LIMIT) -> list:
    """Coordination writes that gave up, grouped by the document they could not reach.

    Twenty jobs failing against one unreachable document are one problem. The target is the
    domain; the jobs are its occurrences.
    """
    rows = store.all(
        "SELECT sync_id, relationship_id, issue_key, target, target_ref, attempts, last_error"
        "  FROM sync_outbox WHERE state = ? ORDER BY sync_id LIMIT ?",
        (faults.FAILED, limit),
    )
    cache = {}
    return [faults.observation(
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


def observation_faults(store, *, product, scope, limit=SWEEP_LIMIT) -> list:
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
        "   AND (p.turn_id IS NULL OR p.last_polled_at IS NULL OR p.last_error IS NOT NULL)"
        " ORDER BY g.relationship_id, g.execution_generation LIMIT ?",
        (limit,),
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
    return observations


def reading_faults(readings, *, product, scope, store=None) -> list:
    """What CRW-180's reporting readings owe, including the ones that clear.

    Only a reading that says REPORTED clears an omission. unmeasured does not: a failed
    evidence read is the absence of an answer, and treating it as recovery would close a fault
    on the strength of a file nobody could read.
    """
    observations = []
    cache = {}
    for reading in readings or ():
        if not isinstance(reading, dict) or reading.get("schema") != OBSERVATION_SCHEMA:
            continue
        state = reading.get("reportingState")
        relationship = reading.get("relationshipId")
        selectors = reading.get("selectors")
        turn = selectors.get("turn") if isinstance(selectors, dict) else None
        if not (_named(relationship) and _named(turn)):
            continue
        signature = {"relationship": relationship, "turn": turn}
        placed = scope_of(store, relationship, scope, cache) if store is not None else scope
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
    return observations


def sweep(store, *, product="crw", scope=None, readings=(), limit=SWEEP_LIMIT,
          policy=None) -> dict:
    """Every fault this store currently shows, plus the clears its own absence establishes.

    A source that filled its page is NOT complete, and an incomplete read establishes nothing
    about absence: clearing from it would withdraw a still-broken fault that happened to sort
    past the bound. Only the classes whose source was read to the end can clear.
    """
    scope = dict(scope or {})
    by_class = {
        "delivery_stalled": delivery_faults(store, product=product, scope=scope, limit=limit,
                                            policy=policy),
        "record_sync_failed": sync_faults(store, product=product, scope=scope, limit=limit),
        "observation_stalled": observation_faults(store, product=product, scope=scope,
                                                  limit=limit),
    }
    complete = tuple(name for name, rows in by_class.items() if len(rows) < limit)
    derived = [entry for rows in by_class.values() for entry in rows]
    observations = derived + reading_faults(readings, product=product, scope=scope,
                                            store=store)
    return {
        "observations": observations,
        "clears": recovered(store, derived, product=product, scope=scope, limit=limit,
                            complete=complete),
        "completeSources": list(complete),
        "limits": f"each source is read at most {limit} rows. A source that filled its page"
                  f" cannot clear anything, and a class this sweep does not derive is never"
                  f" cleared by its absence here",
    }


def recovered(store, derived, *, product, scope, limit=SWEEP_LIMIT, complete=DERIVED) -> list:
    """Open faults this sweep read the source for, to the end, and no longer produces.

    The absence is POSITIVE: these are the classes whose rows this pass actually read in full.
    A class the pass does not derive, or whose read stopped at its bound, is not in this answer
    at all - which is the difference between having looked and not having looked.
    """
    complete = tuple(name for name in complete if name in DERIVED)
    if not complete:
        return []
    present = {entry["faultClass"] + "|" + faults.canonical_signature(entry["signature"])
               for entry in derived}
    rows = store.all(
        "SELECT fault_id, fault_class, signature, cycle FROM fault_ledger"
        " WHERE product = ? AND state IN (?,?,?) AND cleared_at IS NULL"
        "   AND fault_class IN (" + ",".join("?" * len(complete)) + ")"
        " ORDER BY rowid LIMIT ?",
        (product, faults.OBSERVED, faults.OPEN, faults.FIX_PENDING, *complete, limit),
    )
    clears = []
    for row in rows:
        if row["fault_class"] + "|" + row["signature"] in present:
            continue
        last = store.one(
            "SELECT occurrence_id FROM fault_occurrences"
            " WHERE fault_id = ? AND cleared = 0 ORDER BY rowid DESC LIMIT 1",
            (row["fault_id"],))
        clears.append(faults.observation(
            product=product, fault_class=row["fault_class"], severity=faults.NOTICE,
            signature=json.loads(row["signature"]),
            occurrence_key=f"cleared:after:{last['occurrence_id'] if last else row['cycle']}",
            scope=scope, cleared=True,
            detail="this sweep read the source and no longer derives this fault",
            evidence=[_evidence("sweep", row["fault_class"], {"derived": False})],
        ))
    return clears


def record_all(ledger, batch) -> dict:
    """Record a sweep's answer. Returns what actually changed, which is usually nothing."""
    results = []
    for entry in list(batch.get("observations", ())) + list(batch.get("clears", ())):
        results.append(ledger.record(entry))
    recorded = [entry for entry in results if entry.get("recorded")]
    queued = [entry["publication"] for entry in recorded
              if entry.get("publication") and entry["publication"].get("queued")]
    return {"read": len(results), "recorded": len(recorded), "queued": len(queued),
            "results": results}
