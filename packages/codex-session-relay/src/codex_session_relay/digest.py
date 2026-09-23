"""The digest: what changed in routed records since the last digest, for a midpoint check.

Nothing here runs on a clock or calls a model. Somebody asks for a digest. It first binds the
projects confirmed creates made, because binding one moves its member defects wherever they sit
in the listing, and a member reported as held in the same answer that moved it would be a
decision already made. Then, for each route of the page it reads, it discharges what that route
owes (the obligations of its confirmed writes, which change only that route), reads it again, and
compares it with the snapshot it last reported, answering only the difference:

- a new severe record, whether the ledger raised its severity or a pending incident claimed it;
- a new decision somebody has to make - a pending classification, a hold, an issue whose project
  link is incomplete, an open completion mismatch, a proposed project - each also raised as a
  ledger notification under one reason per decision, so the ledger's per-fault-and-reason
  idempotence makes it one notice however often a digest runs;
- a resolution of a record that owned an issue;
- everything else summarized per product as routine accumulation.

Asked again with nothing changed, it answers quiet and reports nothing; the only thing it writes
then is the rotation position of each outstanding project proposal it checked. Budgets,
eligibility and delivery of the notices belong to the ledger's one notification path.
"""

from . import intake, ledger_port, products, routes

PAGE = 100


def _bind_created_projects(router, limit):
    """(bound, reached): outstanding project proposals whose create confirmed, bound before
    anything is reported, and the ids of every proposal this digest checked.

    At most limit are checked, least recently checked first, and each checked one goes to the
    back of the rotation, so a proposal waiting on a slow create cannot keep a later confirmed
    one from being bound: successive digests reach every one. A bound or wholly cancelled
    proposal leaves the filed stage, so only the ones still waiting are read at all.
    """
    bound = []
    reached = routes.outstanding_proposals(router.store, limit)
    for route in reached:
        bound.extend(intake.reconcile_route(router, route)["bound"])
        with router.store.transaction() as db:
            routes.checked(db, route["fault_id"])
    return bound, [route["fault_id"] for route in reached]


def _entry(route, now):
    target = route["target"]
    return {"faultId": route["fault_id"], "product": route["product_key"],
            "disposition": route["disposition"], "stage": route["stage"],
            "state": now["state"], "severity": now["severity"],
            "claimedSeverity": now["claimedSeverity"], "occurrences": now["occurrenceCount"],
            "issue": now["externalRef"], "project": target["project"], "owner": target["owner"],
            "hold": target["hold"], "unverifiedCause": target.get("unverifiedCause"),
            "origin": route["origin"], "detail": route["detail"]}


def _severe(snapshot):
    return snapshot is not None and "broken" in (snapshot["severity"], snapshot["claimedSeverity"])


def digest(router, *, limit=500, after=None) -> dict:
    """The changes since the last digest, over at most limit routes from after."""
    limit, after = products.read_page(limit, after, ceiling=5000)
    port = router.port
    port.ready("route-digest")
    bound, reached = _bind_created_projects(router, limit)
    linked = []
    severe, decisions, resolutions, routine = [], [], [], {}
    read, cursor = 0, after
    while read < limit:
        page = routes.listing(router.store, limit=min(PAGE, limit - read), after=cursor)
        with router.store.composing() as db:
            for route in page["routes"]:
                read += 1
                if route["stage"] == products.STAGE_SUPERSEDED:
                    # Its incidents live on under the product it was classified into.
                    continue
                # Discharged inside the digest's own paging, so every route the digest reaches
                # is reconciled however many come before it, and read again afterwards: the
                # page was read before the projects above were bound and before this discharge.
                if route["disposition"] != products.PROJECT_PROPOSAL:
                    linked.extend(intake.reconcile_route(router, route)["queued"])
                route = routes.get(router.store, route["fault_id"])
                if (route["stage"] == products.STAGE_HELD
                        and route["target"]["hold"] == products.NO_PROJECT
                        and routes.unreached_proposal(router.store, route["product_key"],
                                                      route["goal"], reached) is not None):
                    # A proposal for this defect's own goal that this digest did not reach may
                    # be about to move it; its hold is reported by the digest that reaches that
                    # proposal, not announced stale now. Every other hold is reported.
                    continue
                now = routes.snapshot(route, port.get(route["fault_id"]))
                before = route["reported"]
                if now == before:
                    continue
                entry = _entry(route, now)
                reported = False
                if _severe(now) and not _severe(before) and now["state"] not in ledger_port.ENDED:
                    severe.append(entry)
                    reported = True
                decision = routes.attention(now)
                if decision is not None and decision != (routes.attention(before)
                                                         if before else None):
                    port.notify(route["fault_id"], reason=decision,
                                ref=f"route:{route['fault_id']}")
                    decisions.append({**entry, "decision": decision})
                    reported = True
                if (now["state"] == ledger_port.RESOLVED and before is not None
                        and before["state"] != ledger_port.RESOLVED):
                    resolutions.append(entry)
                    reported = True
                if not reported:
                    summary = routine.setdefault(route["product_key"],
                                                 {"records": 0, "newOccurrences": 0})
                    summary["records"] += 1
                    summary["newOccurrences"] += max(
                        0, now["occurrenceCount"] - ((before or {}).get("occurrenceCount") or 0))
                routes.set_reported(db, router.clock, route["fault_id"], now)
        cursor = page["next"]
        if cursor is None:
            break
    changed = bool(severe or decisions or resolutions or routine or linked or bound)
    return {"quiet": not changed, "severe": severe, "decisions": decisions,
            "resolutions": resolutions, "routine": routine,
            "linked": linked, "projectsBound": bound,
            "proposalsUnreached": routes.unreached_count(router.store, reached),
            "read": read, "next": cursor,
            "limits": "routing's rows and the ledger's state in this store only. A queued write"
                      " is not an issue anybody has written; pass next as after to continue."}
