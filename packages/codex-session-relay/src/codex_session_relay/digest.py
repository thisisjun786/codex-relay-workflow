"""The digest: what changed in routed records since the last digest, for a midpoint check.

Nothing here runs on a clock or calls a model. Somebody asks for a digest; for each route of the
page it reads, it first discharges what the route owes (the obligations of confirmed writes, the
project a confirmed create made), then compares the route with the snapshot it last reported and
answers only the difference:

- a new severe record, whether the ledger raised its severity or a pending incident claimed it;
- a new decision somebody has to make - a pending classification, a hold, an issue whose project
  link is incomplete, an open completion mismatch, a proposed project - each also raised as a
  ledger notification under one reason per decision, so the ledger's per-fault-and-reason
  idempotence makes it one notice however often a digest runs;
- a resolution of a record that owned an issue;
- everything else summarized per product as routine accumulation.

Asked again with nothing changed, it answers quiet and writes nothing. Budgets, eligibility and
delivery of the notices belong to the ledger's one notification path.
"""

from . import intake, ledger_port, products, routes

PAGE = 100


def _entry(route, now):
    target = route["target"]
    return {"faultId": route["fault_id"], "product": route["product_key"],
            "disposition": route["disposition"], "stage": route["stage"],
            "state": now["state"], "severity": now["severity"],
            "claimedSeverity": now["claimedSeverity"], "occurrences": now["occurrenceCount"],
            "issue": now["externalRef"], "project": target["project"], "owner": target["owner"],
            "hold": target["hold"], "origin": route["origin"], "detail": route["detail"]}


def _severe(snapshot):
    return snapshot is not None and "broken" in (snapshot["severity"], snapshot["claimedSeverity"])


def digest(router, *, limit=500, after=None) -> dict:
    """The changes since the last digest, over at most limit routes from after."""
    port = router.port
    port.ready("route-digest")
    limit = min(max(int(limit), 1), 5000)
    linked, bound = [], []
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
                # Reconciled inside the digest's own paging, so every route the digest reaches
                # is also discharged, however many routes come before it.
                done = intake.reconcile_route(router, route)
                linked.extend(done["queued"])
                bound.extend(done["bound"])
                if done["queued"] or done["bound"]:
                    route = routes.get(router.store, route["fault_id"])
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
            "read": read, "next": cursor,
            "limits": "routing's rows and the ledger's state in this store only. A queued write"
                      " is not an issue anybody has written; pass next as after to continue."}
