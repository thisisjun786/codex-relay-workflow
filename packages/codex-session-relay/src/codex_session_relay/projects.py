"""Projects: created only under an explicit policy, through the ledger's own create path.

A project is created only when no suitable project exists AND at least minIndependentFixes
distinct defects of one product, all held for want of a project, share one declared user goal
with completion criteria. Counts alone - issues, files or errors - never qualify, and a single
defect goes into its product's existing suitable project instead.

The create is a publication of routing's own project_create kind on the ledger's operation-kind
seam, so it inherits the single-create, uncertain, reconcile and readback rules unchanged. Two
evaluations of one goal converge on one record and one write. Immediately before the write is
issued the kind checks the whole predicate again inside the ledger's own transaction, and cancels
the write if any part no longer holds. When the create confirms, the project is bound and its
member defects move into it.

Importing this module is what registers routing with the ledger in a process: the fault classes
and the project_create kind. A credential holder running the ledger's own fault-* commands on
routed faults passes `--kind-module codex_session_relay.projects`; without it the ledger refuses
a project_create write as unregistered, so no process can skip the check below.

On a checkout without CRW-205's corrected contract nothing is registered, so the ledger never
offers, claims or issues the kind; ledger_port refuses every other step.
"""

import json

from . import ledger_port, products, routes

KIND = "project_create"
# The scope a product's project records file under. Its target names a team and no project,
# which is what a project create needs and what no issue create can use.
PROJECTS_SCOPE = "__projects__"
PAYLOAD_KEYS = ("product", "workspace", "team", "familyLabel", "goal", "criteria", "name",
                "members", "components")


def _validate(payload) -> list:
    if not isinstance(payload, dict):
        return ["a project_create payload is an object"]
    problems = [f"payload lacks {key}" for key in PAYLOAD_KEYS if payload.get(key) in (None, "")]
    if isinstance(payload.get("members"), list) and len(set(payload["members"])) < 2:
        problems.append("a project needs at least two independent fixes")
    return problems


def _confirm(expected, observed) -> list:
    """The ledger reads this write's block; a project readback must also show its team.

    Required, not merely compared: a confirmation that cannot say which team the project was
    made in would bind it to this product on faith, and every member defect would follow it.
    The holder passes the created project's team as the observed fields it read back.
    """
    payload = (expected or {}).get("payload") or {}
    team = observed.get("team") if isinstance(observed, dict) else None
    if not team:
        return ["the readback does not show which team the project was created in"]
    if team != payload.get("team"):
        return [f"the project was created in {team!r}, not {payload.get('team')!r}"]
    return []


def eligibility(db, payload) -> list:
    """Why this project may not be created now, read from the store; empty when it may.

    Called before queuing and again by the kind's pre-issue check inside the ledger's own
    transaction, so a member that left the group, a project bound meanwhile, or a policy
    switched off after the queue all cancel the write before anything is created.
    """
    row = db.execute("SELECT record FROM routing_policy WHERE policy_key = ?",
                     (products.PROJECT_CREATION,)).fetchone()
    policy = json.loads(row["record"]) if row else None
    if policy is None or not policy["enabled"]:
        return ["no explicit project creation policy is enabled"]
    members = []
    for fault_id in payload["members"]:
        route = db.execute("SELECT stage, target, goal FROM incident_routes WHERE fault_id = ?",
                           (fault_id,)).fetchone()
        if route is None or route["stage"] != products.STAGE_HELD:
            continue
        if json.loads(route["target"]).get("hold") != products.NO_PROJECT:
            continue
        if route["goal"] != payload["goal"]:
            continue
        members.append(fault_id)
    problems = []
    if len(members) < policy["minIndependentFixes"]:
        problems.append(f"{len(members)} held defect(s) still share goal {payload['goal']};"
                        f" the policy needs {policy['minIndependentFixes']}")
    if not payload.get("criteria"):
        problems.append("the goal declares no completion criteria")
    # A create carries the team and family label its product had when it was queued; a
    # registry changed since then must not see a project made where the product no longer is.
    row = db.execute("SELECT record FROM product_registry WHERE product_key = ?",
                     (payload["product"],)).fetchone()
    registry = json.loads(row["record"]) if row else None
    if registry is None:
        problems.append(f"{payload['product']} is no longer registered")
    else:
        for key, name in (("team", "team"), ("familyLabel", "family label")):
            if registry[key] != payload.get(key):
                problems.append(f"{payload['product']}'s {name} is now {registry[key]!r},"
                                f" not {payload.get(key)!r}")
    for binding in db.execute("SELECT record FROM product_bindings WHERE product_key = ?"
                              " AND kind = 'project'", (payload["product"],)).fetchall():
        project = json.loads(binding["record"])
        if project["state"] == "active" and set(project["components"]) & set(
                payload["components"]):
            problems.append(f"{project['ref']} now covers {sorted(payload['components'])}")
    return problems


def _pre_issue(context):
    problems = eligibility(context["db"], context["publication"]["payload"])
    return {"cancel": "; ".join(problems)} if problems else None


# Registered per process at import, as the ledger's seam requires. Empty when registered; the
# list of what the contract still lacks on a checkout that cannot register it. The classes come
# first: a holder process that imports only this module still records and renders them.
CLASS_GAPS = ledger_port.register_classes(products.CLASSES)
KIND_GAPS = ledger_port.register_kind(
    KIND, creates=True, requires_issue=False, target="team", evidence="block",
    confirm=_confirm, validate=_validate, pre_issue=_pre_issue)


def _groups(router, product):
    """Held-for-no-project defects of this product, grouped by the goal their incident declares."""
    groups, after = {}, None
    while True:
        page = routes.listing(router.store, product=product, stages=(products.STAGE_HELD,),
                              limit=100, after=after)
        for route in page["routes"]:
            if route["target"]["hold"] != products.NO_PROJECT:
                continue
            stored = routes.incidents(router.store, route["fault_id"])
            goal = (stored[-1]["goal"] if stored else None) or {}
            if not goal.get("key") or not goal.get("criteria"):
                continue
            entry = groups.setdefault(goal["key"], {"criteria": goal["criteria"],
                                                     "members": [], "components": set(),
                                                     "workspace": route["workspace"]})
            entry["members"].append(route["fault_id"])
            entry["components"].add(stored[-1]["component"])
        after = page["next"]
        if after is None:
            return groups


def evaluate(router, product) -> dict:
    """Queue one project create per qualifying goal of this product, or say why none."""
    registry = router.registry(product)
    policy = router.policy()
    if registry is None or policy is None or not policy["enabled"]:
        return {"queued": [], "skipped": [], "reason": "no explicit project creation policy is"
                                                         " enabled"}
    port = router.port
    queued, skipped = [], []
    for goal, group in sorted(_groups(router, product).items()):
        payload = {"product": product, "workspace": group["workspace"],
                   "team": registry["team"], "familyLabel": registry["familyLabel"],
                   "goal": goal, "criteria": group["criteria"],
                   "name": f"{registry['familyLabel']} · {goal}",
                   "members": sorted(set(group["members"])),
                   "components": sorted(group["components"])}
        problems = eligibility(router.store.db, payload)
        if problems:
            skipped.append({"goal": goal, "reasons": problems})
            continue
        fault_id = port.fault_id(product, group["workspace"], products.PROJECT_NEEDED,
                                 {"goal": goal})
        rows = port.publications(fault_id, kind=KIND, limit=100)
        if any(row.get("state") != "cancelled" for row in rows):
            skipped.append({"goal": goal, "reasons": ["a project create for this goal is"
                                                      " already queued or done"]})
            continue
        # A create kind has one row per fault: a goal that qualifies again after a cancelled
        # create revives that row under its id with this payload, and the pre-issue check runs
        # again before it is issued. The trigger names the reason; it does not make a new write.
        trigger = f"need:{goal}"
        # The family label belongs on the project, never on an issue.
        target = routes.plain_target(team=registry["team"], labels=[registry["familyLabel"]])
        with router.store.composing() as db:
            port.ensure_target(product=product, workspace=group["workspace"],
                               project=PROJECTS_SCOPE, team=registry["team"], project_ref=None)
            port.record(port.observation(
                product=product, workspace=group["workspace"],
                fault_class=products.PROJECT_NEEDED, severity="notice",
                signature={"goal": goal}, occurrence_key=trigger, project=PROJECTS_SCOPE,
                detail=f"{len(payload['members'])} independent fixes share {goal}:"
                       f" {group['criteria']}"))
            port.queue(fault_id, kind=KIND, trigger=trigger, payload=payload)
            routes.upsert(db, router.clock, fault_id=fault_id, product=product,
                          workspace=group["workspace"], disposition=products.PROJECT_PROPOSAL,
                          stage=products.STAGE_FILED, target=target, origin=products.OBSERVED,
                          claimed_severity="notice", goal=goal,
                          detail=f"project for {goal} queued under the explicit policy")
        queued.append({"goal": goal, "faultId": fault_id, "trigger": trigger,
                       "members": payload["members"]})
    return {"queued": queued, "skipped": skipped}


REVISABLE = ("pending", "failed", "claimed")


def revise(router, product) -> dict:
    """Cancel the product's project creates that no longer qualify and that nothing has issued,
    then evaluate again, so a goal that still qualifies is queued with the registry as it is.

    The pre-issue check would refuse such a create anyway; this settles it when the registry
    changes rather than whenever a holder next reaches it. A create already issued is the
    ledger's to reconcile and is left alone.
    """
    port = router.port
    cancelled, after = [], None
    while True:
        page = routes.listing(router.store, product=product, stages=(products.STAGE_FILED,),
                              dispositions=(products.PROJECT_PROPOSAL,), limit=100, after=after)
        for route in page["routes"]:
            for row in port.publications(route["fault_id"], kind=KIND, limit=100):
                if row.get("state") not in REVISABLE:
                    continue
                problems = eligibility(router.store.db, row.get("payload") or {})
                if problems:
                    port.cancel(row["publication_id"], reason="; ".join(problems))
                    cancelled.append({"faultId": route["fault_id"],
                                      "publicationId": row["publication_id"],
                                      "reasons": problems})
        after = page["next"]
        if after is None:
            break
    evaluated = evaluate(router, product) if cancelled else {"queued": [], "skipped": []}
    return {"cancelled": cancelled, "queued": evaluated["queued"]}


def bind_confirmed(router, route) -> list:
    """Bind a project a confirmed create made, which moves its member defects into it.

    The binding and every member's move are one transaction: binding decides the held members
    again, and a failure half way would leave a bound project with members still held for want
    of one. Idempotent: a project already bound, by this or by an operator's read-back, is not
    bound again, and the proposal still records which project it became.

    A proposal with no create outstanding any more - its project bound, or every create it
    queued cancelled - is settled: it leaves the filed stage, so the digest and route-reconcile,
    which look only for outstanding proposals, stop reading it. A later evaluation that queues
    a new create files it again.
    """
    rows = router.port.publications(route["fault_id"], kind=KIND, limit=100)
    bound = {b["ref"] for b in router.bindings(route["product_key"]) if b["kind"] == "project"}
    registry = router.registry(route["product_key"]) or {}
    made = []
    stranded = []
    for row in rows:
        ref, payload = row.get("external_ref"), row.get("payload") or {}
        if row.get("state") != "confirmed" or not ref or (
                ref in bound and route["target"]["project"] == ref):
            continue
        if ref not in bound and payload.get("team") != registry.get("team"):
            # Made in a team the product has left since it was queued; an issued create cannot
            # be taken back, and binding it would file the members' issues there. Somebody binds
            # it by hand, or the product names that team again.
            stranded.append(ref)
            continue
        with router.store.composing() as db:
            if ref not in bound:
                router.bind({"schema": products.BINDING_SCHEMA,
                             "product": route["product_key"], "kind": "project", "ref": ref,
                             "title": payload.get("name") or ref, "state": "active",
                             "components": payload.get("components") or [],
                             "goal": payload.get("goal"),
                             "source": f"created by product routing (publication"
                                       f" {row.get('publication_id') or row.get('id')})"})
                made.append(ref)
            routes.set_target(db, router.clock, route["fault_id"],
                              {**route["target"], "project": ref, "hold": None})
    if stranded:
        with router.store.transaction() as db:
            routes.set_target(db, router.clock, route["fault_id"],
                              {**route["target"], "hold": products.PROJECT_TEAM_CHANGED})
        return made
    if rows and all(row.get("state") in ("confirmed", "cancelled") for row in rows):
        created = sorted(row["external_ref"] for row in rows
                         if row.get("state") == "confirmed" and row.get("external_ref"))
        with router.store.transaction() as db:
            routes.settle(db, router.clock, route["fault_id"],
                          f"project {', '.join(created)} created and bound" if created else
                          "every create it queued was cancelled before issue")
    return made
