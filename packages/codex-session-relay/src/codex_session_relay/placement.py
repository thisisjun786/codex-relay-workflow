"""Placement: where an incident belongs, decided from readings alone.

Three rules shape every answer here.

A defect is never filed under the product that happened to observe it. A tool failure seen in a
CRW-managed run of another repository belongs to that repository's product; CRW is one more
registered product, and nothing defaults to it.

Two defects are merged only on evidence. An existing issue owns an incident when it names the
same component AND the same symptom key; a shared component alone is two defects that happen to
live near each other. The current issue of a managed run is the one exception, and it needs the
relay's own record that the run belongs to that issue - never the incident's word for it.

Unclear ownership is a decision, not a guess. An incident whose product cannot be resolved, or
whose owner or project is ambiguous, is held and says why, rather than being assigned to
whichever candidate sorts first.

Everything here is a pure function of its arguments. The store reads - which run belongs to
which issue, which bindings exist - happen in routing, and their results are passed in, so the
same inputs always give the same placement.
"""

from . import products
from .errors import RefusalReason

# The workspace an incident takes while its product, and therefore its workspace, is unknown.
# The ledger accepts this sentinel for exactly that case; ledger_port checks the two agree.
UNASSIGNED = "unassigned"
# Surfaces that come from a managed run and can therefore be the current issue's own evidence.
RUN_SURFACES = ("dev_run", "verification")


def resolve_product(registries, incident):
    """(product, reason). Declared wins unless its repository belongs to another product; else a
    repository only one product claims; else None.

    None is not a failure. It is the pending-classification path, which holds one record and
    assigns nothing.
    """
    declared = incident["product"]
    repository = incident["repository"]
    if declared:
        if declared not in registries:
            return None, f"{declared} is not a registered product"
        if repository:
            owners = sorted(key for key, registry in registries.items()
                            if repository in registry["repositories"])
            if owners and declared not in owners:
                # Two readings name two owners. Filing under either would be a guess about
                # whose team gets the issue, so it waits for somebody to classify it.
                return None, (f"{declared} was declared, but repository {repository} is"
                              f" registered to {owners}")
        return declared, f"{declared} was declared by the source"
    if repository:
        owners = sorted(key for key, registry in registries.items()
                        if repository in registry["repositories"])
        if len(owners) == 1:
            return owners[0], f"repository {repository} is registered to {owners[0]}"
        if owners:
            return None, f"repository {repository} is registered to several products: {owners}"
        return None, f"repository {repository} is registered to no product"
    return None, "the incident names neither a product nor a repository"


def workspace_for(incident, registry=None) -> str:
    """The Linear workspace this incident's identity carries.

    A resolved product files in its own workspace, and an incident claiming another is refused
    rather than filed across tenants. An unresolved one keeps the workspace it declares, or the
    sentinel, so two workspaces never share one pending record.
    """
    declared = incident["workspace"]
    if registry is None:
        return declared or UNASSIGNED
    if declared and declared != registry["workspace"]:
        products.refuse(RefusalReason.ROUTE_INPUT_MALFORMED,
                        f"{registry['product']} files in workspace {registry['workspace']!r};"
                        f" the incident declares {declared!r}")
    return registry["workspace"]


def defect_signature(incident, attached=None) -> dict:
    """The failure domain: component and symptom keys, never prose, never the time.

    Failure evidence attached to a current issue is that issue's own record: the same symptom
    tracked by another issue is a different record the two are linked through, so attaching
    never takes a fault another issue already owns.
    """
    signature = {"component": incident["component"], "symptom": incident["symptom"]}
    if attached:
        signature["issue"] = attached
    if incident["origin"] == products.SIMULATED:
        # Part of identity, so a simulated event never converges with a real observation.
        signature["simulated"] = True
    return signature


def pending_signature(incident) -> dict:
    """What one pending-classification record is about, including whatever the source named."""
    signature = {"surface": incident["surface"], "component": incident["component"],
                 "symptom": incident["symptom"]}
    for key in ("product", "repository"):
        if incident[key]:
            signature[key] = incident[key]
    if incident["origin"] == products.SIMULATED:
        signature["simulated"] = True
    return signature


def _decision(disposition, *, project=None, owner=None, hold=None, relate=(), reopen=False,
              reason=""):
    stage = {products.HELD: products.STAGE_HELD,
             products.OBSERVE: products.STAGE_OBSERVED}.get(disposition, products.STAGE_FILED)
    return {"disposition": disposition, "stage": stage, "project": project, "owner": owner,
            "hold": hold, "relate": list(relate), "reopen": reopen, "reason": reason}


def _owned(disposition, binding, registry, incident, reason, *, reopen=False, relate=()):
    """An owner is scoped by its own project so its comment can be handed out; without one the
    triage project stands in, and without either the route is held and says why."""
    if incident["origin"] == products.SIMULATED:
        project = registry["testTarget"]["project"]
    else:
        project = binding.get("project") or registry["triageProject"]
    if project is None:
        return _decision(products.HELD, owner=binding["ref"],
                         hold=products.OWNER_PROJECT_MISSING,
                         reason=f"{binding['ref']} owns this but belongs to no project and"
                                f" {registry['product']} has no triage project")
    return _decision(disposition, project=project, owner=binding["ref"], reopen=reopen,
                     relate=relate, reason=reason)


def decide(incident, registry, bindings, run_issue=None) -> dict:
    """Where this incident belongs. Pure: the same readings give the same answer.

    run_issue is the issue the relay itself recorded for the incident's run (relationships), or
    None when the run is unknown here. The incident's own currentIssue is a claim; run_issue is
    the evidence that decides whether the claim holds.
    """
    simulated = incident["origin"] == products.SIMULATED
    usable = [b for b in bindings if b["test"] == simulated]
    issues = {b["ref"]: b for b in usable if b["kind"] == "issue"}
    component, symptom = incident["component"], incident["symptom"]
    if incident["expected"]:
        return _decision(products.OBSERVE,
                         reason=f"an expected state ({incident['expected']}) is recorded for an"
                                f" operator and never filed")
    same = [b for ref, b in sorted(issues.items())
            if component in b["components"] and symptom in b["symptoms"]]
    notes = []
    # The current issue first: a failure in the current issue's own managed run, inside its
    # scope, is that issue's failure evidence and rework. Another issue owning the same symptom
    # is linked to it, never merged into it and never preferred over it.
    attached = _current(incident, issues, run_issue, notes)
    if attached is not None:
        others = [b["ref"] for b in same if b["ref"] != attached["ref"]]
        return _owned(products.ATTACH_CURRENT, attached, registry, incident,
                      f"failure evidence for the current issue {attached['ref']}, from its own"
                      f" managed run" + (f"; linked to {others}, which own the same symptom"
                                         if others else ""), relate=others)
    open_ = [b for b in same if b["state"] in products.OPEN_STATES]
    if len(open_) == 1:
        return _owned(products.ACCUMULATE, open_[0], registry, incident,
                      f"{open_[0]['ref']} is open for the same component and symptom")
    if len(open_) > 1:
        return _decision(products.HELD, hold=products.AMBIGUOUS_OWNER,
                         reason=f"several open issues claim this symptom:"
                                f" {[b['ref'] for b in open_]}")
    done = [b for b in same if b["state"] in products.DONE_STATES]
    if len(done) == 1:
        return _owned(products.REOPEN, done[0], registry, incident,
                      f"{done[0]['ref']} was completed and the same defect came back",
                      reopen=True)
    if len(done) > 1:
        return _decision(products.HELD, hold=products.AMBIGUOUS_OWNER,
                         reason=f"several completed issues claim this symptom:"
                                f" {[b['ref'] for b in done]}")
    relate, disposition = [], products.NEW_ISSUE
    regression = incident["context"]["regressionOf"]
    if regression:
        fixed = sorted((b for b in issues.values()
                        if b["state"] in products.DONE_STATES and b["fixRef"] == regression),
                       key=lambda b: b["ref"])
        if len(fixed) == 1:
            relate, disposition = [fixed[0]["ref"]], products.FOLLOW_UP
            notes.append(f"a regression of {regression}, which closed {fixed[0]['ref']}")
        else:
            notes.append(f"regressionOf {regression!r} names no single completed issue, so"
                         f" nothing is linked without evidence")
    project, hold, why = _project(incident, registry, usable)
    reason = "; ".join(notes + [why])
    if hold:
        return _decision(products.HELD, hold=hold, relate=relate, reason=reason)
    return _decision(disposition, project=project, relate=relate, reason=reason)


def _current(incident, issues, run_issue, notes):
    """The current issue, only when the relay's own record says the run belongs to it."""
    current = incident["context"]["currentIssue"]
    if not current:
        return None
    if incident["surface"] not in RUN_SURFACES:
        notes.append(f"a {incident['surface']} incident is never attached to a current issue")
        return None
    if run_issue is None:
        notes.append(f"no managed run recorded here belongs to {current}, so nothing is"
                     f" attached to it")
        return None
    if run_issue != current:
        notes.append(f"the run belongs to {run_issue}, not {current}, so nothing is attached")
        return None
    binding = issues.get(current)
    if binding is None:
        notes.append(f"the current issue {current} is not bound to this product")
        return None
    if binding["state"] not in products.OPEN_STATES:
        notes.append(f"the current issue {current} is {binding['state']}")
        return None
    if incident["component"] not in binding["components"]:
        notes.append(f"{incident['component']} is outside the current issue's scope")
        return None
    return binding


def _project(incident, registry, bindings):
    """(project, hold, reason). One suitable project, else the triage project, else held."""
    if incident["origin"] == products.SIMULATED:
        return registry["testTarget"]["project"], None, "simulated: the test target project"
    component = incident["component"]
    covering = sorted((b for b in bindings if b["kind"] == "project" and b["state"] == "active"
                       and component in b["components"]), key=lambda b: b["ref"])
    if len(covering) == 1:
        return covering[0]["ref"], None, f"{covering[0]['ref']} covers {component}"
    goal = (incident["goal"] or {}).get("key")
    if len(covering) > 1:
        chosen = [b for b in covering if goal and b["goal"] == goal]
        if len(chosen) == 1:
            return chosen[0]["ref"], None, f"{chosen[0]['ref']} covers {component} and {goal}"
        # Several projects claim it and no goal chooses: a decision, never the triage project,
        # which is for work no project covers.
        return None, products.AMBIGUOUS_PROJECT, (f"several projects cover {component}:"
                                                  f" {[b['ref'] for b in covering]}")
    triage = registry["triageProject"]
    if triage:
        return triage, None, ("no project covers it; the triage project holds it until a"
                              " project owns it")
    return None, products.NO_PROJECT, (f"no project covers {component} and"
                                       f" {registry['product']} has no triage project")


def issue_labels(incident) -> list:
    """An issue carries the label of the repository it is about; the family label belongs on
    the product's projects, never on an issue."""
    return [incident["repository"]] if incident["repository"] else []


def detail_text(incident) -> str:
    """What the record says beyond the ledger's own summary, for somebody who was not there."""
    detail = incident["detail"]
    lines = [f"surface: {incident['surface']}  phase: {incident['phase']}"
             f"  origin: {incident['origin']}"]
    for key, label in (("impact", "impact"), ("expected", "expected"), ("actual", "actual"),
                       ("reproduction", "reproduce with"), ("owner", "current owner"),
                       ("nextAction", "next action")):
        if detail.get(key):
            lines.append(f"{label}: {detail[key]}")
    lines.append("execution: not approved. Filing this starts no work; approval, assignment,"
                 " the fix and reverification are separate steps.")
    return "\n".join(lines)
