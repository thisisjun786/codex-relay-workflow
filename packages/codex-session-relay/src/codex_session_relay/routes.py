"""The rows routing keeps about each routed fault: where it went, why, and what it still owes.

One incident_routes row per ledger fault routing filed, and the newest incidents behind it in
route_incidents. The fault itself - its state, occurrences, publications and the issue it owns -
lives in the ledger and is read through ledger_port; nothing here repeats it.
"""

import json

from . import products
from .identity import sha256_hex

# How many incidents a route keeps: enough for a later binding to decide it again from its
# latest input, and for a classification to replay what the pending record saw. Older ones are
# dropped oldest first; the ledger still counts every occurrence it recorded.
MAX_STORED_INCIDENTS = 16
TARGET_KEYS = ("team", "project", "owner", "relate", "hold", "labels", "cause", "obligations")
# The decisions a route can be waiting on, named once. Intake raises the severe ones at once,
# the digest raises every new one, and route-show --attention lists them; one name per decision
# keeps the ledger's per-fault-and-reason notification idempotence meaning one notice per
# decision.
AWAITING_CLASSIFICATION = "awaiting_classification"
LINK_INCOMPLETE = "link_incomplete"
MISMATCH_OPEN = "completion_mismatch_open"
PROJECT_PROPOSED = "project_proposed"


def held_reason(hold):
    return f"held_{hold}"


def _decode(row):
    if row is None:
        return None
    answer = dict(row)
    for key in ("target", "classification", "reported"):
        answer[key] = json.loads(answer[key]) if answer.get(key) else None
    unknown = set(answer["target"] or {}) - set(TARGET_KEYS)
    if unknown:
        # Written by this module only; anything else is a store somebody edited by hand, and
        # routing on a target it does not understand would be a guess.
        products.refuse(products.RefusalReason.ROUTE_STATE_CONFLICT,
                        f"route {answer['fault_id']} carries target keys {sorted(unknown)}")
    return answer


def get(store, fault_id):
    return _decode(store.one("SELECT * FROM incident_routes WHERE fault_id = ?", (fault_id,)))


def target(decision, registry, incident, *, cause=None, obligations=()):
    """What a route records about where its fault goes, in one shape."""
    simulated = incident["origin"] == products.SIMULATED
    team = registry["testTarget"]["team"] if simulated and registry else (
        registry["team"] if registry else None)
    return {"team": team, "project": decision.get("project"), "owner": decision.get("owner"),
            "relate": list(decision.get("relate") or []), "hold": decision.get("hold"),
            "labels": [incident["repository"]] if incident["repository"] else [],
            "cause": cause, "obligations": list(obligations)}


def plain_target(**fields):
    """A target for a route no incident decided - a project proposal, a completion check."""
    unknown = set(fields) - set(TARGET_KEYS)
    if unknown:
        raise ValueError(f"not target keys: {sorted(unknown)}")
    answer = {"team": None, "project": None, "owner": None, "relate": [], "hold": None,
              "labels": [], "cause": None, "obligations": []}
    answer.update(fields)
    return answer


def snapshot(route, row) -> dict:
    """What the digest compares: the route's own decision and the ledger's state of its fault."""
    row = row or {}
    return {"stage": route["stage"], "disposition": route["disposition"],
            "hold": route["target"]["hold"], "project": route["target"]["project"],
            "claimedSeverity": route["claimed_severity"],
            "state": row.get("state"), "severity": row.get("severity"),
            "occurrenceCount": row.get("occurrence_count") or 0,
            "externalRef": row.get("external_ref"), "linkState": row.get("linkState")}


def attention(snapshot):
    """The decision a route is waiting on, or None. Stated from its snapshot alone, so what
    route-show lists, what the digest reports and what intake notifies cannot disagree."""
    from . import ledger_port

    if snapshot["stage"] == products.STAGE_PENDING:
        return AWAITING_CLASSIFICATION
    if snapshot["stage"] == products.STAGE_HELD:
        return held_reason(snapshot["hold"])
    if snapshot["disposition"] == products.PROJECT_PROPOSAL:
        # Waiting while its create is outstanding; once the project is bound, or every create
        # it queued was cancelled, the proposal is settled and history.
        if (snapshot["stage"] == products.STAGE_FILED and snapshot["project"] is None
                and snapshot["state"] in ledger_port.ACTIVE):
            return PROJECT_PROPOSED
        return None
    if snapshot["linkState"] == "unlinked":
        return LINK_INCOMPLETE
    if (snapshot["disposition"] == products.COMPLETION_MISMATCH
            and snapshot["state"] in ledger_port.ACTIVE):
        return MISMATCH_OPEN
    return None


def upsert(db, clock, *, fault_id, product, workspace, disposition, stage, target, origin,
           claimed_severity, goal=None, detail="", classification=None, superseded_by=None):
    now = clock.iso()
    db.execute(
        "INSERT INTO incident_routes (fault_id, product_key, workspace, disposition, stage,"
        "  target, origin, claimed_severity, goal, classification, superseded_by, detail,"
        "  created_at, updated_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)"
        " ON CONFLICT(fault_id) DO UPDATE SET product_key = excluded.product_key,"
        "   workspace = excluded.workspace, disposition = excluded.disposition,"
        "   stage = excluded.stage, target = excluded.target, origin = excluded.origin,"
        "   claimed_severity = CASE WHEN excluded.claimed_severity = 'broken'"
        "     THEN 'broken' ELSE incident_routes.claimed_severity END,"
        "   goal = COALESCE(excluded.goal, incident_routes.goal),"
        "   classification = COALESCE(excluded.classification, incident_routes.classification),"
        "   superseded_by = COALESCE(excluded.superseded_by, incident_routes.superseded_by),"
        "   detail = excluded.detail, updated_at = excluded.updated_at",
        (fault_id, product, workspace, disposition, stage, products.canonical(target), origin,
         claimed_severity, goal,
         products.canonical(classification) if classification is not None else None,
         superseded_by, detail, now, now))


def set_target(db, clock, fault_id, target):
    db.execute("UPDATE incident_routes SET target = ?, updated_at = ? WHERE fault_id = ?",
               (products.canonical(target), clock.iso(), fault_id))


def settle(db, clock, fault_id, detail):
    """A route with nothing left to wait for leaves the filed stage, so the paths that look
    for outstanding work stop reading it."""
    db.execute("UPDATE incident_routes SET stage = ?, detail = ?, updated_at = ?"
               " WHERE fault_id = ?", (products.STAGE_OBSERVED, detail, clock.iso(), fault_id))


def outstanding_proposals(store, limit):
    """(checked now, the product and goal of every one not reached): at most limit outstanding
    project proposals, least recently checked first. The second part is a small grouped read
    of routing's own rows, so a caller can tell which held defects a proposal it did not reach
    might still move."""
    rows = store.all(
        "SELECT rowid AS seq, * FROM incident_routes WHERE stage = ? AND disposition = ?"
        " ORDER BY checked_seq, rowid LIMIT ?",
        (products.STAGE_FILED, products.PROJECT_PROPOSAL, min(max(int(limit), 1), 5000)))
    reached = [_decode(row) for row in rows]
    placeholders = ",".join("?" * len(reached)) or "''"
    waiting = store.all(
        "SELECT DISTINCT product_key, goal FROM incident_routes WHERE stage = ?"
        " AND disposition = ? AND fault_id NOT IN (" + placeholders + ") LIMIT 5000",
        (products.STAGE_FILED, products.PROJECT_PROPOSAL, *(r["fault_id"] for r in reached)))
    return reached, {(row["product_key"], row["goal"]) for row in waiting}


def checked(db, fault_id):
    """Move a proposal to the back of the digest's rotation."""
    db.execute("UPDATE incident_routes SET checked_seq ="
               " (SELECT COALESCE(MAX(checked_seq), 0) + 1 FROM incident_routes)"
               " WHERE fault_id = ?", (fault_id,))


def set_reported(db, clock, fault_id, snapshot):
    db.execute("UPDATE incident_routes SET reported = ?, updated_at = ? WHERE fault_id = ?",
               (products.canonical(snapshot), clock.iso(), fault_id))


def _incident_id(fault_id, key):
    return sha256_hex(f"{fault_id}|{key}")[:32]


def store_incident(db, clock, fault_id, incident, *, keep=MAX_STORED_INCIDENTS):
    """Keep this incident as the route's newest input; drop the oldest past the bound.

    keep=None keeps every one. A completion mismatch round keeps every failing reading it
    recorded, because recognising a reading handed in again must not depend on how many others
    came after it; each is one small row, and a round ends when its mismatch is closed.
    """
    incident_id = _incident_id(fault_id, incident["occurrenceKey"])
    seq = db.execute("SELECT COALESCE(MAX(recorded_seq), 0) + 1 AS n FROM route_incidents"
                     " WHERE fault_id = ?", (fault_id,)).fetchone()["n"]
    db.execute("INSERT INTO route_incidents (incident_id, fault_id, record, recorded_at,"
               "  recorded_seq) VALUES (?,?,?,?,?) ON CONFLICT(incident_id) DO UPDATE SET"
               "   record = excluded.record, recorded_at = excluded.recorded_at,"
               "   recorded_seq = excluded.recorded_seq",
               (incident_id, fault_id, products.canonical(incident), clock.iso(), seq))
    if keep is not None:
        db.execute("DELETE FROM route_incidents WHERE fault_id = ? AND incident_id NOT IN"
                   "  (SELECT incident_id FROM route_incidents WHERE fault_id = ?"
                   "    ORDER BY recorded_seq DESC LIMIT ?)",
                   (fault_id, fault_id, keep))


def stored_incident(store, fault_id, key):
    """The id of the input this route recorded under this occurrence key, or None. One key
    lookup, no scan."""
    row = store.one("SELECT incident_id FROM route_incidents WHERE incident_id = ?",
                    (_incident_id(fault_id, key),))
    return row["incident_id"] if row else None


def incidents(store, fault_id) -> list:
    rows = store.all("SELECT record FROM route_incidents WHERE fault_id = ?"
                     " ORDER BY recorded_seq", (fault_id,))
    return [json.loads(row["record"]) for row in rows]


def listing(store, *, product=None, stages=None, dispositions=None, limit=20,
            after=None) -> dict:
    """One page of routes, oldest first, continued by rowid like the ledger's own listing."""
    limit = min(max(int(limit), 1), 1000)
    clauses, params = [], []
    if product is not None:
        clauses.append("product_key = ?")
        params.append(product)
    if stages:
        clauses.append("stage IN (" + ",".join("?" * len(stages)) + ")")
        params.extend(stages)
    if dispositions:
        clauses.append("disposition IN (" + ",".join("?" * len(dispositions)) + ")")
        params.extend(dispositions)
    if after is not None:
        clauses.append("rowid > ?")
        params.append(int(after))
    where = (" WHERE " + " AND ".join(clauses)) if clauses else ""
    rows = store.all("SELECT rowid AS seq, * FROM incident_routes" + where
                     + " ORDER BY rowid LIMIT ?", (*params, limit + 1))
    page = [_decode(row) for row in rows[:limit]]
    return {"routes": page, "next": page[-1]["seq"] if len(rows) > limit else None}
