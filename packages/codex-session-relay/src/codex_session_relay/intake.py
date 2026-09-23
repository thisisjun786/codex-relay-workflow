"""Intake: an incident in, one ledger observation out, filed where placement decided.

Every path here pairs a ledger call with routing's own rows, and every such pair runs inside
store.composing(), so a refusal from the ledger leaves neither half behind. The ledger is reached
only through the router's port; on a checkout without CRW-205's corrected contract every path
refuses before it writes anything.
"""

import json

from . import placement, products, routes
from .errors import RefusalReason, RelayError

# Ledger refusals that mean "this owner cannot take the fault now", which routing turns into a
# hold somebody decides rather than an error that loses the incident.
OWNER_REFUSALS = ("fault_adopt_conflict", "fault_scope_conflict")
CLASSIFICATION_KEYS = ("product", "component", "symptom", "goal", "by")


def scope(workspace, project):
    return {"workspace": workspace, **({"projectKey": project} if project else {})}


def _stored(value):
    """A ledger row's JSON column, whether it arrives as text or already decoded."""
    if isinstance(value, str):
        return json.loads(value) if value else {}
    return dict(value or {})


def intake(router, record) -> dict:
    """Route one incident. Refuses, writing nothing, when it cannot be collected here."""
    incident = products.read_incident(record)
    registries = router.registries()
    product, why = placement.resolve_product(registries, incident)
    registry = registries.get(product) if product else None
    if registry is not None:
        _watched(registry, incident)
    workspace = placement.workspace_for(incident, registry)
    if registry is None:
        return _unresolved(router, incident, workspace, why)
    cause = incident["cause"]
    if cause is not None and cause["product"] != product:
        return _shared_cause(router, incident, registry, workspace)
    return file(router, incident, registry, workspace)


def _watched(registry, incident):
    spec = registry["surfaces"].get(incident["surface"])
    if spec is None or not spec["active"]:
        products.refuse(RefusalReason.ROUTE_SURFACE_UNWATCHED,
                        f"{registry['product']} does not watch {incident['surface']}; nothing"
                        f" from an unconnected surface is collected")
    if incident["origin"] == products.SIMULATED and registry["testTarget"] is None:
        products.refuse(RefusalReason.ROUTE_INPUT_MALFORMED,
                        f"a simulated incident needs {registry['product']}'s test target")


def _unresolved(router, incident, workspace, why) -> dict:
    """One pending-classification record per identity, never filed, and never assigned."""
    port = router.port
    signature = placement.pending_signature(incident)
    fault_id = port.fault_id(products.UNCLASSIFIED, workspace, products.PENDING, signature)
    route = routes.get(router.store, fault_id)
    if route is not None and route["stage"] == products.STAGE_SUPERSEDED:
        return _forward(router, route, incident)
    with router.store.composing() as db:
        port.record(port.observation(
            product=products.UNCLASSIFIED, workspace=workspace, fault_class=products.PENDING,
            severity="notice", signature=signature, occurrence_key=incident["occurrenceKey"],
            observed_at=incident["observedAt"], detail=placement.detail_text(incident),
            evidence=incident["evidence"]))
        routes.upsert(db, router.clock, fault_id=fault_id, product=products.UNCLASSIFIED,
                      workspace=workspace, disposition=products.PENDING_CLASSIFICATION,
                      stage=products.STAGE_PENDING,
                      target=routes.target({}, None, incident), origin=incident["origin"],
                      claimed_severity=incident["severity"],
                      goal=(incident["goal"] or {}).get("key"), detail=why)
        routes.store_incident(db, router.clock, fault_id, incident)
        if incident["severity"] == "broken":
            # A severe incident nobody can place is a decision for somebody, not an issue in
            # whichever team sorts first. The digest raises the same decision for every
            # pending record under the same reason, so this is one notice, raised early.
            port.notify(fault_id, reason=routes.AWAITING_CLASSIFICATION,
                        ref=incident["occurrenceKey"])
    return {"faultId": fault_id, "product": None, "workspace": workspace,
            "disposition": products.PENDING_CLASSIFICATION, "stage": products.STAGE_PENDING,
            "reason": why}


def _classified(incident, classification):
    applied = dict(incident)
    applied["product"] = classification["product"]
    for key in ("component", "symptom"):
        if classification.get(key):
            applied[key] = classification[key]
    if classification.get("goal"):
        applied["goal"] = classification["goal"]
    return applied


def _forward(router, route, incident) -> dict:
    """A later incident with a classified pending identity goes to the product it was given."""
    classification = route["classification"]
    registry = router.registry(classification["product"])
    applied = _classified(incident, classification)
    # The product it was classified into decides what is collected, exactly as for an intake
    # that named it: a surface it does not watch is refused, not filed through the side door.
    _watched(registry, applied)
    answer = file(router, applied, registry, placement.workspace_for(applied, registry))
    return {**answer, "forwardedFrom": route["fault_id"]}


def _shared_cause(router, incident, registry, workspace) -> dict:
    """A cause in another product: verified, it gains an occurrence at its own severity and the
    affected product's defect is filed as usual and linked; unverified, nothing merges."""
    port = router.port
    cause = incident["cause"]
    row = port.get(cause["faultId"]) if cause["faultId"] else None
    verified = (row is not None and row.get("product") == cause["product"]
                and (cause["signature"] is None
                     or _stored(row["signature"]) == cause["signature"]))
    if not verified:
        return file(router, incident, registry, workspace, hold=products.CAUSE_UNVERIFIED)
    placed = _stored(row.get("scope"))
    # Two writes, deliberately not one transaction: the cause occurrence is evidence on another
    # product's record and stands on its own; the affected product's filing below composes its
    # own ledger and routing writes.
    with router.store.composing():
        port.record(port.observation(
            product=row["product"], workspace=placed.get("workspace"),
            fault_class=row["fault_class"], severity=row["severity"],
            signature=_stored(row["signature"]),
            occurrence_key=f"affected:{registry['product']}:{incident['occurrenceKey']}",
            project=placed.get("projectKey"), observed_at=incident["observedAt"],
            detail=f"{registry['product']} was affected by this fault",
            evidence=incident["evidence"]))
    return file(router, incident, registry, workspace, cause_fault=cause["faultId"])


def file(router, incident, registry, workspace, *, hold=None, cause_fault=None) -> dict:
    """Decide, then record: the one place a product incident becomes a ledger observation."""
    port = router.port
    product = registry["product"]
    decision = placement.decide(incident, registry, router.bindings(product),
                                router.run_issue(incident["context"]["run"]))
    if hold is not None:
        decision = {**decision, "disposition": products.HELD, "stage": products.STAGE_HELD,
                    "hold": hold, "project": None, "owner": None,
                    "reason": f"{hold}; " + decision["reason"]}
    observe = decision["disposition"] == products.OBSERVE
    fault_class = products.EXPECTED_STATE if observe else products.DEFECT
    signature = placement.defect_signature(incident)
    fault_id = port.fault_id(product, workspace, fault_class, signature)
    existing = routes.get(router.store, fault_id)
    if existing is not None and existing["stage"] == products.STAGE_FILED:
        # Filed once, the fault's record belongs to the ledger; a later occurrence changes
        # nothing about where it went.
        kept = existing["target"]
        decision = {**decision, "disposition": existing["disposition"],
                    "stage": products.STAGE_FILED, "project": kept["project"],
                    "owner": kept["owner"], "hold": None, "relate": kept["relate"],
                    "reason": "filed earlier; the ledger's record carries this occurrence"}
    obligations = _obligations(decision, existing, cause_fault,
                               labels=placement.issue_labels(incident))
    target = routes.target(decision, registry, incident, obligations=obligations,
                           cause=cause_fault)
    first = port.get(fault_id) is None
    adopt = None
    if decision["owner"] and first:
        adopt = {"externalRef": decision["owner"], "scope": scope(workspace, decision["project"])}
    try:
        with router.store.composing() as db:
            # The one call that can refuse on the ledger's own terms goes first, before this
            # attempt has written anything, so a refusal leaves nothing behind even when this
            # runs inside a caller's composed transaction.
            if decision["owner"] and not first and not (existing and existing["target"]["owner"]):
                port.adopt(fault_id, external_ref=decision["owner"],
                           scope=scope(workspace, decision["project"]))
            if decision["project"]:
                port.ensure_target(product=product, workspace=workspace,
                                   project=decision["project"], team=target["team"],
                                   project_ref=decision["project"])
            result = port.record(port.observation(
                product=product, workspace=workspace, fault_class=fault_class,
                severity="notice" if observe else incident["severity"], signature=signature,
                occurrence_key=incident["occurrenceKey"], project=decision["project"],
                observed_at=incident["observedAt"], detail=placement.detail_text(incident),
                evidence=incident["evidence"]), adopt=adopt)
            _save(db, router, fault_id, product, workspace, decision, target, incident)
    except RelayError as refusal:
        if getattr(refusal.reason, "value", None) not in OWNER_REFUSALS:
            raise
        # The fault already files its own record, or lives in another workspace: holding it
        # for a decision keeps both records from existing for one defect. The occurrence is
        # recorded where the fault already is: a record() that named no project, or another
        # one, would move its scope and re-point its unsent writes on the way to a hold.
        kept = _stored((port.get(fault_id) or {}).get("scope")).get("projectKey")
        decision = {**decision, "disposition": products.HELD, "stage": products.STAGE_HELD,
                    "hold": products.OWNER_FOUND_AFTER_CREATE, "project": None,
                    "reason": f"{decision['owner']} owns this, but {refusal}"}
        target = routes.target(decision, registry, incident, obligations=(), cause=cause_fault)
        with router.store.composing() as db:
            result = port.record(port.observation(
                product=product, workspace=workspace, fault_class=fault_class,
                severity=incident["severity"], signature=signature,
                occurrence_key=incident["occurrenceKey"], project=kept,
                observed_at=incident["observedAt"], detail=placement.detail_text(incident),
                evidence=incident["evidence"]))
            _save(db, router, fault_id, product, workspace, decision, target, incident)
    discharge(router, fault_id)
    if decision["stage"] == products.STAGE_HELD:
        _held(router, fault_id, decision, incident, product)
    return {"faultId": fault_id, "product": product, "workspace": workspace,
            "disposition": decision["disposition"], "stage": decision["stage"],
            "project": decision["project"], "owner": decision["owner"],
            "hold": decision["hold"], "recorded": result.get("recorded"),
            "publication": result.get("publication"), "reason": decision["reason"]}


def _save(db, router, fault_id, product, workspace, decision, target, incident):
    routes.upsert(db, router.clock, fault_id=fault_id, product=product, workspace=workspace,
                  disposition=decision["disposition"], stage=decision["stage"], target=target,
                  origin=incident["origin"], claimed_severity=incident["severity"],
                  goal=(incident["goal"] or {}).get("key"), detail=decision["reason"])
    routes.store_incident(db, router.clock, fault_id, incident)


def _obligations(decision, existing, cause_fault, *, labels=()):
    """What this route still owes once its fault owns an issue, merged with what it owed.

    An issue the fault creates owes its repository label, which the ledger's create does not
    carry; an issue it adopts is somebody else's and keeps the labels it has, so an adopted
    route owes none and an open label obligation from before the adoption is dropped.
    """
    owed = list((existing or {}).get("target", {}).get("obligations") or [])
    if decision.get("owner"):
        owed = [o for o in owed if not (o["kind"] == "add_label" and o["state"] == "open")]
    wanted = []
    if decision["disposition"] == products.REOPEN:
        wanted.append(_owed("reopen"))
    if decision["disposition"] in (products.NEW_ISSUE, products.FOLLOW_UP) and not decision.get(
            "owner"):
        wanted.extend(_owed("add_label", label=label) for label in labels)
    for issue in decision.get("relate") or []:
        wanted.append(_owed("add_relation", to_issue=issue))
    if cause_fault:
        wanted.append(_owed("add_relation", to_fault=cause_fault))
    keys = {_owed_key(o) for o in owed}
    owed.extend(o for o in wanted if _owed_key(o) not in keys)
    return owed


def _owed(kind, *, to_issue=None, to_fault=None, label=None):
    return {"kind": kind, "toIssue": to_issue, "toFault": to_fault, "label": label,
            "state": "open"}


def _owed_key(obligation):
    return (obligation["kind"], obligation["toIssue"], obligation["toFault"],
            obligation.get("label"))


def _held(router, fault_id, decision, incident, product):
    from . import projects

    if incident["severity"] == "broken":
        router.port.notify(fault_id, reason=routes.held_reason(decision["hold"]),
                           ref=incident["occurrenceKey"])
    if decision["hold"] == products.NO_PROJECT:
        projects.evaluate(router, product)


def discharge(router, fault_id) -> list:
    """Queue what a route owes once its fault owns an issue. Idempotent: the ledger keeps one
    update per operation, value and cycle, and a queued obligation is not queued again."""
    route = routes.get(router.store, fault_id)
    if route is None:
        return []
    target = route["target"]
    pending = [o for o in target["obligations"] if o["state"] == "open"]
    if not pending:
        return []
    port = router.port
    owned = (port.get(fault_id) or {}).get("external_ref")
    if not owned:
        return []
    queued = []
    with router.store.composing() as db:
        for obligation in pending:
            if obligation["kind"] == "reopen":
                if owned != target["owner"]:
                    continue
                port.update(fault_id, op="reopen", value=None)
            elif obligation["kind"] == "add_label":
                if target["owner"]:
                    continue
                port.update(fault_id, op="add_label", value=obligation["label"])
            else:
                other = obligation["toIssue"] or (
                    (port.get(obligation["toFault"]) or {}).get("external_ref")
                    if obligation["toFault"] else None)
                if not other:
                    continue
                port.update(fault_id, op="add_relation",
                            value={"type": "related", "issue": other})
            obligation["state"] = "queued"
            queued.append(obligation)
        if queued:
            routes.set_target(db, router.clock, fault_id, target)
    return queued


def redecide(router, product) -> list:
    """Decide held routes, and filed ones whose fault owns no issue yet, again from their
    latest incident against the bindings as they are now."""
    registry = router.registry(product)
    if registry is None:
        return []
    bindings = router.bindings(product)
    changed, after = [], None
    while True:
        page = routes.listing(router.store, product=product,
                              stages=(products.STAGE_HELD, products.STAGE_FILED),
                              limit=100, after=after)
        for route in page["routes"]:
            answer = _again(router, route, registry, bindings)
            if answer is not None:
                changed.append(answer)
        after = page["next"]
        if after is None:
            return changed


def _again(router, route, registry, bindings):
    if route["disposition"] in (products.PROJECT_PROPOSAL, products.COMPLETION_MISMATCH,
                                products.COMPLETION_UNVERIFIED):
        return None
    port = router.port
    fault_id = route["fault_id"]
    if route["stage"] == products.STAGE_FILED and (port.get(fault_id) or {}).get("external_ref"):
        return None
    stored = routes.incidents(router.store, fault_id)
    if not stored:
        return None
    incident = stored[-1]
    decision = placement.decide(incident, registry, bindings,
                                router.run_issue(incident["context"]["run"]))
    current = route["target"]
    if (decision["project"], decision["owner"], decision["hold"]) == (
            current["project"], current["owner"], current["hold"]):
        return None
    target = routes.target(decision, registry, incident,
                           obligations=_obligations(decision, route, current.get("cause"),
                                                    labels=placement.issue_labels(incident)),
                           cause=current.get("cause"))
    place = scope(route["workspace"], decision["project"])
    try:
        with router.store.composing() as db:
            if decision["owner"]:
                port.adopt(fault_id, external_ref=decision["owner"], scope=place)
            if decision["project"]:
                port.ensure_target(product=registry["product"], workspace=route["workspace"],
                                   project=decision["project"], team=target["team"],
                                   project_ref=decision["project"])
            if decision["project"] and not decision["owner"]:
                port.move(fault_id, scope=place)
            routes.upsert(db, router.clock, fault_id=fault_id, product=registry["product"],
                          workspace=route["workspace"], disposition=decision["disposition"],
                          stage=decision["stage"], target=target, origin=route["origin"],
                          claimed_severity=route["claimed_severity"],
                          detail=decision["reason"])
    except RelayError as refusal:
        if getattr(refusal.reason, "value", None) not in OWNER_REFUSALS:
            raise
        held = {**current, "hold": products.OWNER_FOUND_AFTER_CREATE, "owner": decision["owner"]}
        with router.store.transaction() as db:
            db.execute("UPDATE incident_routes SET stage = ?, disposition = ?, target = ?,"
                       "  detail = ?, updated_at = ? WHERE fault_id = ?",
                       (products.STAGE_HELD, products.HELD, products.canonical(held),
                        str(refusal), router.clock.iso(), fault_id))
        return {"faultId": fault_id, "hold": products.OWNER_FOUND_AFTER_CREATE}
    discharge(router, fault_id)
    return {"faultId": fault_id, "disposition": decision["disposition"],
            "project": decision["project"], "owner": decision["owner"], "hold": decision["hold"]}


def read_classification(record) -> dict:
    """Who a pending incident belongs to. Severity is not a key: nobody classifies it."""
    products._closed(record, CLASSIFICATION_KEYS, "a classification")
    goal = record.get("goal")
    if goal is not None:
        products._closed(goal, ("key", "criteria"), "classification.goal")
        goal = {"key": products._key(goal.get("key"), "goal.key"),
                "criteria": products._text(goal.get("criteria"), "goal.criteria",
                                           optional=True)}
    by = products._text(record.get("by"), "by", limit=128)
    if by != "operator" and not by.startswith("llm:"):
        products.malformed("by is operator or llm:<model>")
    return {"product": products._product(record.get("product"), "product"),
            "component": products._key(record.get("component"), "component", optional=True),
            "symptom": products._key(record.get("symptom"), "symptom", optional=True),
            "goal": goal, "by": by}


def classify(router, fault_id, record) -> dict:
    """Give a pending record its product: replay its incidents there and withdraw it."""
    classification = read_classification(record)
    route = routes.get(router.store, fault_id)
    if route is None or route["product_key"] != products.UNCLASSIFIED:
        products.refuse(RefusalReason.ROUTE_STATE_CONFLICT,
                        f"{fault_id} is not a pending-classification record")
    if route["stage"] == products.STAGE_SUPERSEDED:
        return {"faultId": fault_id, "successor": route["superseded_by"], "replayed": 0,
                "changed": False}
    registry = router.registry(classification["product"])
    if registry is None:
        products.refuse(RefusalReason.ROUTE_PRODUCT_UNKNOWN,
                        f"{classification['product']!r} is not a registered product")
    stored = routes.incidents(router.store, fault_id)
    if not stored:
        products.refuse(RefusalReason.ROUTE_STATE_CONFLICT, f"{fault_id} keeps no incident")
    for incident in stored:
        # Every replayed incident must be one the classified product collects: classifying
        # into a product that does not watch the surface would file it through the side door.
        _watched(registry, _classified(incident, classification))
    port = router.port
    port.ready("route-classify")
    with router.store.composing() as db:
        successor = None
        for incident in stored:
            applied = _classified(incident, classification)
            successor = file(router, applied, registry,
                             placement.workspace_for(applied, registry))["faultId"]
        port.record(port.observation(
            product=products.UNCLASSIFIED, workspace=route["workspace"],
            fault_class=products.PENDING, severity="notice",
            signature=placement.pending_signature(stored[0]),
            occurrence_key=f"classified:{successor}", detail="classified", cleared=True))
        routes.upsert(db, router.clock, fault_id=fault_id, product=products.UNCLASSIFIED,
                      workspace=route["workspace"], disposition=route["disposition"],
                      stage=products.STAGE_SUPERSEDED, target=route["target"],
                      origin=route["origin"], claimed_severity=route["claimed_severity"],
                      classification={**classification, "at": router.clock.iso()},
                      superseded_by=successor, detail=f"classified by {classification['by']}")
    return {"faultId": fault_id, "successor": successor, "replayed": len(stored),
            "changed": True}


def reconcile_route(router, route) -> dict:
    """What one filed route owes now: its obligations, or the project its proposal created."""
    from . import projects

    if route["stage"] != products.STAGE_FILED:
        return {"queued": [], "bound": []}
    if route["disposition"] == products.PROJECT_PROPOSAL:
        return {"queued": [], "bound": projects.bind_confirmed(router, route)}
    if any(o["state"] == "open" for o in route["target"]["obligations"]):
        return {"queued": discharge(router, route["fault_id"]), "bound": []}
    return {"queued": [], "bound": []}


def reconcile(router, *, product=None, limit=50, after=None) -> dict:
    """Discharge obligations whose ends now own issues, and bind projects that were created,
    over at most limit filed routes after the cursor; pass next back as after to continue."""
    queued, bound, seen = [], [], 0
    limit = min(max(int(limit), 1), 5000)
    while seen < limit:
        page = routes.listing(router.store, product=product, stages=(products.STAGE_FILED,),
                              limit=min(100, limit - seen), after=after)
        for route in page["routes"]:
            seen += 1
            done = reconcile_route(router, route)
            queued.extend(done["queued"])
            bound.extend(done["bound"])
        after = page["next"]
        if after is None:
            break
    return {"queued": queued, "bound": bound, "read": seen, "next": after}
