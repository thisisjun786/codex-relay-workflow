"""Which revision is current, read from lineage the owner actually declared.

What a declared predecessor IS: the child's own assertion that its new revision replaces a named
earlier one. What it is NOT: evidence of when any bytes were produced. Knowing another revision's
digest shows acquaintance with it, and acquaintance can be acquired any number of ways, so a
declaration is treated as an owner assertion about lineage and validated against the graph it
claims to extend.

Arrival order decides nothing here. Where the declared graph does not identify a single unique
tip, the answer is ambiguous and completion is withheld. An unresolved fork does not become
resolved because one side showed up second, and an unknown predecessor is an unresolved edge
rather than the absence of one: reading it as "no predecessor" is exactly how an out-of-order
arrival would quietly become a root.
"""

from .errors import ReceiptRefused, RefusalReason
from .identity import revision_request_event_id

SOLE = "sole_revision"
CHAIN = "declared_chain"
NONE = "no_revision"
FORK = "fork"
CYCLE = "cycle"
UNKNOWN_PREDECESSOR = "unknown_predecessor"
DISCONNECTED = "disconnected"

AMBIGUOUS = (FORK, CYCLE, UNKNOWN_PREDECESSOR, DISCONNECTED)

NOT_ACTIVE = "relationship_not_active"
STALE_GENERATION = "stale_generation"
SUPERSEDED = "superseded_revision"
AMBIGUOUS_REASON = "revision_ambiguous"

REVIEWABLE = "ready_for_review"


def record_lineage(db, clock, *, relationship_id, generation, event_id, revision_hash,
                   supersedes_hash=None) -> dict:
    """Record the declaration. Whole-graph validation happens at read time, not here.

    Refused here only for what is decidable locally: a revision declaring itself. A fork or an
    unknown predecessor is RECORDED and surfaces as ambiguity when the head is read. Refusing a
    second declaration at write time would be arrival-order authority wearing a different hat:
    whoever wrote first would win, which is the property this module exists to deny.
    """
    declared = str(supersedes_hash).strip() if supersedes_hash else None
    if declared and declared == revision_hash:
        raise ReceiptRefused(
            RefusalReason.REVISION_LINEAGE_INVALID,
            "a revision cannot supersede itself",
        )
    db.execute(
        "INSERT OR IGNORE INTO revision_lineage (relationship_id, execution_generation,"
        " event_id, revision_hash, supersedes_hash, declared_by, recorded_at)"
        " VALUES (?,?,?,?,?,?,?)",
        (
            relationship_id, generation, event_id, revision_hash, declared,
            "child_declared" if declared else "undeclared", clock.iso(),
        ),
    )
    return {"eventId": event_id, "supersedesHash": declared}


def _ambiguous(evidence, nodes, detail):
    return {
        "eventId": None,
        "revisionHash": None,
        "evidence": evidence,
        "competitors": sorted(nodes),
        "detail": detail,
    }


def _requested_predecessors(db, relationship_id, generation):
    """Only the result whose ruling opened this correction is an external root.

    The verdict, revision request and generation are committed together by AckService.
    A manually opened generation, or an unrelated historical digest, grants no edge.
    Historical roots cannot become a current head and their own older lineage is not
    imported into this generation's graph.
    """
    rows = db.execute(
        "SELECT p.event_id, p.revision_hash, v.verdict_turn_id, r.event_id AS request_id,"
        " g.dispatch_request_id"
        " FROM generations g"
        " JOIN verdicts v ON v.next_generation = g.execution_generation"
        " JOIN events p ON p.event_id = v.event_id AND p.relationship_id = g.relationship_id"
        " JOIN events r ON r.relationship_id = g.relationship_id"
        " AND r.execution_generation = g.execution_generation"
        " WHERE g.relationship_id = ? AND g.execution_generation = ?"
        " AND g.reason = 'needs_changes_revision' AND v.verdict = 'needs_changes'"
        " AND p.execution_generation = g.execution_generation - 1"
        " AND p.outcome = ? AND p.stage = 'final' AND p.suppressed_reason IS NULL"
        " AND r.outcome = 'revision_request' AND r.producer = 'relay'"
        " AND r.stage = 'final' AND r.suppressed_reason IS NULL",
        (relationship_id, generation, REVIEWABLE),
    ).fetchall()
    anchors = {}
    for row in rows:
        request_id = revision_request_event_id(
            relationship_id, row["event_id"], row["verdict_turn_id"],
        )
        if (row["request_id"] == request_id
                and row["dispatch_request_id"] == f"revision-{request_id}"):
            anchors.setdefault(row["revision_hash"], []).append(row["event_id"])
    return anchors


def head_revision(db, relationship_id, generation) -> dict:
    """The one revision this generation currently stands on, or why there is not one."""
    rows = db.execute(
        "SELECT e.event_id, e.revision_hash, l.supersedes_hash"
        "  FROM events e"
        "  LEFT JOIN revision_lineage l ON l.event_id = e.event_id"
        " WHERE e.relationship_id = ? AND e.execution_generation = ?"
        "   AND e.outcome = ? AND e.suppressed_reason IS NULL"
        " ORDER BY e.event_id",
        (relationship_id, generation, REVIEWABLE),
    ).fetchall()
    nodes = {
        row["event_id"]: {
            "eventId": row["event_id"],
            "revisionHash": row["revision_hash"],
            "supersedesHash": row["supersedes_hash"],
        }
        for row in rows
    }
    if not nodes:
        return {
            "eventId": None, "revisionHash": None, "evidence": NONE, "competitors": [],
            "detail": "no reviewable revision in this generation",
        }

    by_hash = {}
    for node in nodes.values():
        by_hash.setdefault(node["revisionHash"], []).append(node["eventId"])
    anchors = _requested_predecessors(db, relationship_id, generation)

    edges = {}
    unresolved = []
    for node in nodes.values():
        declared = node["supersedesHash"]
        if not declared:
            continue
        targets = by_hash.get(declared, []) + anchors.get(declared, [])
        if len(targets) != 1:
            # A predecessor must identify one local revision or the exact result requested
            # for correction. Unknown or non-unique digests remain unresolved edges.
            unresolved.append({"eventId": node["eventId"], "supersedesHash": declared})
            continue
        edges[node["eventId"]] = targets[0]

    if unresolved:
        return _ambiguous(
            UNKNOWN_PREDECESSOR, nodes,
            "a declared predecessor is neither a unique revision of this generation "
            "nor its requested correction predecessor: "
            + ", ".join(f"{u['eventId']} -> {u['supersedesHash']}" for u in unresolved),
        )

    for start in nodes:
        seen = {start}
        current = start
        while current in edges:
            current = edges[current]
            if current in seen:
                return _ambiguous(
                    CYCLE, nodes, f"the declared chain from {start} returns to {current}"
                )
            seen.add(current)

    predecessors = {}
    for source, target in edges.items():
        predecessors.setdefault(target, []).append(source)
    forked = sorted(target for target, sources in predecessors.items() if len(sources) > 1)
    if forked:
        return _ambiguous(
            FORK, nodes,
            "more than one revision declares the same predecessor: " + ", ".join(forked),
        )

    tips = sorted(set(nodes) - set(edges.values()))
    if len(tips) != 1:
        return _ambiguous(
            FORK, nodes,
            f"{len(tips)} revisions in this generation are unsuperseded, so none of them is "
            "the head; a revision that replaces another says so when it is emitted",
        )

    tip = tips[0]
    covered, current = [tip], tip
    while current in edges:
        current = edges[current]
        covered.append(current)
    if set(covered).intersection(nodes) != set(nodes):
        return _ambiguous(
            DISCONNECTED, nodes,
            "the declared chain from the tip does not reach every revision in this generation",
        )

    return {
        "eventId": tip,
        "revisionHash": nodes[tip]["revisionHash"],
        "evidence": SOLE if len(nodes) == 1 and not edges else CHAIN,
        "competitors": [],
        "detail": "",
    }


def currency_of(db, relationship_row, event_row) -> dict:
    """Is this event the thing the assignment currently stands on?

    Read inside the caller's transaction, never before it. Everything it consults can move
    between a preflight read and a commit, which is the whole reason this function exists.
    """
    relationship_id = event_row["relationship_id"]
    if relationship_row["status"] != "active" or relationship_row["superseded_by"]:
        return {
            "current": False, "reason": NOT_ACTIVE, "evidence": None,
            "headEventId": None, "headRevisionHash": None,
            "detail": f"relationship {relationship_id!r} is {relationship_row['status']!r}",
        }
    generation = relationship_row["execution_generation"]
    if event_row["execution_generation"] != generation:
        return {
            "current": False, "reason": STALE_GENERATION, "evidence": None,
            "headEventId": None, "headRevisionHash": None,
            "detail": f"this event is generation {event_row['execution_generation']} and the "
                      f"assignment is on generation {generation}",
        }
    head = head_revision(db, relationship_id, generation)
    if head["evidence"] in AMBIGUOUS:
        return {
            "current": False, "reason": AMBIGUOUS_REASON, "evidence": head["evidence"],
            "headEventId": None, "headRevisionHash": None,
            "detail": head["detail"], "competitors": head["competitors"],
        }
    if head["eventId"] != event_row["event_id"]:
        return {
            "current": False, "reason": SUPERSEDED, "evidence": head["evidence"],
            "headEventId": head["eventId"], "headRevisionHash": head["revisionHash"],
            "detail": f"the current revision of generation {generation} is {head['eventId']}",
        }
    return {
        "current": True, "reason": None, "evidence": head["evidence"],
        "headEventId": head["eventId"], "headRevisionHash": head["revisionHash"], "detail": "",
    }
