"""Products: what routing knows about each product, and the shapes it accepts.

CRW-205 made CRW's own breakages converge onto one ledger record and one Linear issue. This
module is the product half of CRW-206: it holds what routing READ about each product - its
Linear team, workspace, family label, repositories, which surfaces are watched and how, its
triage and test targets, and the project and issue bindings a credential holder read back from
Linear - and validates every shape routing accepts.

It writes nothing to the ledger and imports nothing from it. Identity, suppression, targets,
limits and the outbox belong to the fault ledger and are reached only through ledger_port, so
this module cannot drift into a second copy of any of them. Where an incident belongs is
decided in placement.py from the readings validated here.

Every input shape has a closed key set. An intake that kept whatever it was handed would be
exactly the indiscriminate collection this feature must not do, so an unknown key is refused
rather than ignored.
"""

import json
import re

from .errors import RefusalReason, RelayError

REGISTRY_SCHEMA = "product-registry/1"
BINDING_SCHEMA = "product-binding/1"
INCIDENT_SCHEMA = "product-incident/1"
POLICY_SCHEMA = "routing-policy/1"

SURFACES = ("dev_run", "verification", "user_report", "real_use")
PHASES = ("development", "in_use")
EXPECTED = ("cancelled", "awaiting_approval", "unsupported")
OBSERVED = "observed"
SIMULATED = "simulated"
ORIGINS = (OBSERVED, SIMULATED)
SEVERITIES = ("notice", "degraded", "broken")
# The completion checks a reading can make, and the ones a follow-up issue can take over.
CHECKS = ("acceptance", "install", "realUse", "handoff")

UNCLASSIFIED = "unclassified"
PROJECT_CREATION = "project_creation"

OPEN_STATES = ("open", "in_progress")
DONE_STATES = ("done",)
ISSUE_STATES = ("open", "in_progress", "done", "canceled")
PROJECT_STATES = ("active", "completed")
ISSUE_REF = re.compile(r"^[A-Z][A-Z0-9]*-[0-9]+$")
KEY = re.compile(r"^[A-Za-z0-9][A-Za-z0-9._/-]{0,127}$")
MAX_TEXT = 600

# Fault classes this module defines. They are registered with the ledger through ledger_port,
# which is the only module allowed to reach it; each names what clears it, because a class
# nothing can clear never closes.
DEFECT = "product_defect"
EXPECTED_STATE = "product_expected"
PENDING = "unclassified_incident"
MISMATCH = "completion_mismatch"
UNVERIFIED = "completion_unverified"
PROJECT_NEEDED = "project_needed"
CLASSES = {
    DEFECT: {"component": "product",
             "clears": "a later reading of the same product, component and symptom that finds it"
                       " gone, or the fix and reverification loop"},
    EXPECTED_STATE: {"component": "product",
                     "clears": "the expected state ending: the cancellation settled, the approval"
                               " given, or the support added"},
    PENDING: {"component": "triage",
              "clears": "classification into a registered product, which re-files every stored"
                        " incident there"},
    MISMATCH: {"component": "completion",
               "clears": "the evidence the completion lacked being observed and reverified"},
    UNVERIFIED: {"component": "completion",
                 "clears": "a later reading that establishes the check either way"},
    PROJECT_NEEDED: {"component": "planning",
                     "clears": "the project being created and bound; the record stays as that"
                               " project's creation record"},
}

ATTACH_CURRENT = "attach_current"
ACCUMULATE = "accumulate"
REOPEN = "reopen"
FOLLOW_UP = "follow_up"
NEW_ISSUE = "new_issue"
OBSERVE = "observe"
SHARED_CAUSE = "shared_cause"
PENDING_CLASSIFICATION = "pending_classification"
HELD = "held"
PROJECT_PROPOSAL = "project_proposal"
COMPLETION_MISMATCH = "completion_mismatch"
COMPLETION_UNVERIFIED = "completion_unverified"
DISPOSITIONS = (ATTACH_CURRENT, ACCUMULATE, REOPEN, FOLLOW_UP, NEW_ISSUE, OBSERVE, SHARED_CAUSE,
                PENDING_CLASSIFICATION, HELD, PROJECT_PROPOSAL, COMPLETION_MISMATCH,
                COMPLETION_UNVERIFIED)
OWNED = (ATTACH_CURRENT, ACCUMULATE, REOPEN)

STAGE_PENDING = "pending_classification"
STAGE_HELD = "held"
STAGE_FILED = "filed"
STAGE_OBSERVED = "observed"
STAGE_SUPERSEDED = "superseded"
STAGES = (STAGE_PENDING, STAGE_HELD, STAGE_FILED, STAGE_OBSERVED, STAGE_SUPERSEDED)

NO_PROJECT = "no_project"
OWNER_PROJECT_MISSING = "owner_project_missing"
AMBIGUOUS_OWNER = "ambiguous_owner"
AMBIGUOUS_PROJECT = "ambiguous_project"
CAUSE_UNVERIFIED = "cause_unverified"
OWNER_FOUND_AFTER_CREATE = "owner_found_after_create"
HOLDS = (NO_PROJECT, OWNER_PROJECT_MISSING, AMBIGUOUS_OWNER, AMBIGUOUS_PROJECT, CAUSE_UNVERIFIED,
         OWNER_FOUND_AFTER_CREATE)


class RouteRefused(RelayError):
    """An incident, a registry record or a routing transition was not accepted."""


def refuse(reason, detail):
    raise RouteRefused(reason, detail)


def malformed(detail):
    refuse(RefusalReason.ROUTE_INPUT_MALFORMED, detail)


def _object(record, schema, keys, what):
    if not isinstance(record, dict):
        malformed(f"{what} is an object, not {type(record).__name__}")
    if record.get("schema") != schema:
        malformed(f"{what} carries schema {record.get('schema')!r}, not {schema}")
    _closed(record, keys, what)


def _closed(record, keys, what):
    """Unknown keys are refused, not ignored. An intake that accepted whatever it was handed
    would be exactly the indiscriminate collection this feature is not allowed to do."""
    if not isinstance(record, dict):
        malformed(f"{what} is an object")
    unknown = sorted(set(record) - set(keys))
    if unknown:
        malformed(f"{what} carries keys this contract does not define: {unknown}")


def _text(value, name, *, optional=False, limit=MAX_TEXT):
    if value is None and optional:
        return None
    if not isinstance(value, str) or not value.strip():
        malformed(f"{name} is a non-blank string")
    if len(value) > limit:
        malformed(f"{name} is longer than {limit} characters")
    return value.strip()


def _key(value, name, *, optional=False):
    """An identifying key: a component, a symptom, a product. Keys decide identity, so they are
    constrained; prose never is one."""
    if value is None and optional:
        return None
    if not isinstance(value, str) or not KEY.match(value):
        malformed(f"{name} is a key ({KEY.pattern}), not {value!r}")
    return value


def _keys(values, name):
    if values is None:
        return []
    if not isinstance(values, list):
        malformed(f"{name} is a list")
    return sorted({_key(value, f"{name}[]") for value in values})


def _issue(value, name, *, optional=False):
    if value is None and optional:
        return None
    if not isinstance(value, str) or not ISSUE_REF.match(value):
        malformed(f"{name} is a Linear issue identifier like ABC-12, not {value!r}")
    return value


def _flag(value, name, default=None):
    if value is None and default is not None:
        return default
    if not isinstance(value, bool):
        malformed(f"{name} is a boolean")
    return value


def _choice(value, choices, name, *, optional=False):
    if value is None and optional:
        return None
    if value not in choices:
        malformed(f"{name} is one of {choices}, not {value!r}")
    return value


def _target(value, name):
    if value is None:
        return None
    _closed(value, ("team", "project"), name)
    return {"team": _text(value.get("team"), f"{name}.team"),
            "project": _text(value.get("project"), f"{name}.project")}


REGISTRY_KEYS = ("schema", "product", "workspace", "team", "familyLabel", "repositories",
                 "surfaces", "triageProject", "testTarget")


def read_registry(record) -> dict:
    """One product as routing knows it. Every surface it does not list reads unobserved."""
    _object(record, REGISTRY_SCHEMA, REGISTRY_KEYS, "a registry record")
    product = _key(record.get("product"), "product")
    if product == UNCLASSIFIED:
        malformed(f"{UNCLASSIFIED!r} is the pending-classification bucket, not a product")
    surfaces = record.get("surfaces") or {}
    if not isinstance(surfaces, dict):
        malformed("surfaces is an object keyed by surface")
    watched = {}
    for surface, spec in sorted(surfaces.items()):
        _choice(surface, SURFACES, "a surface")
        _closed(spec, ("method", "active"), f"surfaces.{surface}")
        watched[surface] = {"method": _text(spec.get("method"), f"surfaces.{surface}.method"),
                            "active": _flag(spec.get("active"), f"surfaces.{surface}.active")}
    return {
        "schema": REGISTRY_SCHEMA, "product": product,
        "workspace": _key(record.get("workspace"), "workspace"),
        "team": _text(record.get("team"), "team"),
        "familyLabel": _text(record.get("familyLabel"), "familyLabel"),
        "repositories": _keys(record.get("repositories"), "repositories"),
        "surfaces": watched,
        "triageProject": _text(record.get("triageProject"), "triageProject", optional=True),
        "testTarget": _target(record.get("testTarget"), "testTarget"),
    }


def coverage(registry) -> dict:
    """What is watched, how, and what is not: a surface nobody connected is unobserved, which is
    a different fact from a surface that was watched and stayed quiet."""
    answer = {}
    for surface in SURFACES:
        spec = registry["surfaces"].get(surface)
        if spec is not None and spec["active"]:
            answer[surface] = {"state": "watched", "method": spec["method"]}
        elif spec is not None:
            answer[surface] = {"state": "unobserved", "method": spec["method"],
                               "reason": "declared and switched off"}
        else:
            answer[surface] = {"state": "unobserved", "method": None,
                               "reason": "no collection method is connected"}
    return answer


BINDING_KEYS = ("schema", "product", "kind", "ref", "title", "state", "project", "components",
                "symptoms", "goal", "fixRef", "followUpOf", "test", "observedAt", "source")


def read_binding(record, registry=None) -> dict:
    """A project or issue as a credential holder read it back from Linear.

    A binding marked test must sit on the registry's designated test target, or a simulated
    incident could adopt a real issue and comment on it.
    """
    _object(record, BINDING_SCHEMA, BINDING_KEYS, "a binding")
    kind = _choice(record.get("kind"), ("project", "issue"), "kind")
    test = _flag(record.get("test"), "test", default=False)
    answer = {
        "schema": BINDING_SCHEMA, "product": _key(record.get("product"), "product"),
        "kind": kind, "title": _text(record.get("title"), "title"),
        "components": _keys(record.get("components"), "components"), "test": test,
        "observedAt": _text(record.get("observedAt"), "observedAt", optional=True),
        "source": _text(record.get("source"), "source"),
    }
    if kind == "project":
        for key in ("project", "symptoms", "fixRef", "followUpOf"):
            if record.get(key) is not None:
                malformed(f"a project binding carries no {key}")
        answer.update({
            "ref": _text(record.get("ref"), "ref"),
            "state": _choice(record.get("state"), PROJECT_STATES, "state"),
            "goal": _key(record.get("goal"), "goal", optional=True),
        })
    else:
        if record.get("goal") is not None:
            malformed("an issue binding carries no goal; its project does")
        answer.update({
            "ref": _issue(record.get("ref"), "ref"),
            "state": _choice(record.get("state"), ISSUE_STATES, "state"),
            "project": _text(record.get("project"), "project", optional=True),
            "symptoms": _keys(record.get("symptoms"), "symptoms"),
            "fixRef": _text(record.get("fixRef"), "fixRef", optional=True),
            "followUpOf": _follow_ups(record.get("followUpOf")),
        })
    if registry is not None:
        if registry["product"] != answer["product"]:
            malformed(f"this binding names {answer['product']}, not {registry['product']}")
        if test:
            target = registry["testTarget"]
            if target is None:
                malformed(f"{answer['product']} has no test target, so nothing of it is a test"
                          f" binding")
            where = answer["ref"] if kind == "project" else answer.get("project")
            if where != target["project"]:
                malformed(f"a test binding sits on the test target project"
                          f" {target['project']!r}, not {where!r}")
            if kind == "issue" and answer["ref"].split("-")[0] != target["team"]:
                malformed(f"a test issue belongs to the test target team {target['team']!r}")
    return answer


def _follow_ups(values):
    """What this issue took over, as a credential holder read it back: which subject issue, and
    which of its completion checks. A follow-up is an exception for one check of one subject and
    nothing wider, so the checks are part of the relation rather than implied by it."""
    if values is None:
        return []
    if not isinstance(values, list):
        malformed("followUpOf is a list of {issue, checks}")
    answer = []
    for entry in values:
        _closed(entry, ("issue", "checks"), "followUpOf[]")
        checks = entry.get("checks")
        if not isinstance(checks, list) or not checks:
            malformed("followUpOf[].checks names at least one check")
        answer.append({"issue": _issue(entry.get("issue"), "followUpOf[].issue"),
                       "checks": sorted({_choice(check, CHECKS, "followUpOf[].checks[]")
                                         for check in checks})})
    return sorted(answer, key=lambda entry: entry["issue"])


POLICY_KEYS = ("schema", "policy", "enabled", "minIndependentFixes", "requireSharedGoal",
               "requireCompletionCriteria", "basis")


def read_policy(record) -> dict:
    """The project creation policy, which exists only when somebody configured it.

    The two requirements are fixed at true: the request this feature implements forbids creating
    a project from counts alone, so a policy that switched them off would be a different feature.
    """
    _object(record, POLICY_SCHEMA, POLICY_KEYS, "a routing policy")
    _choice(record.get("policy"), (PROJECT_CREATION,), "policy")
    minimum = record.get("minIndependentFixes")
    if not isinstance(minimum, int) or isinstance(minimum, bool) or minimum < 2:
        malformed("minIndependentFixes is an integer of at least 2; one fix is not a project")
    for key in ("requireSharedGoal", "requireCompletionCriteria"):
        if record.get(key) is not True:
            malformed(f"{key} is true: a project is never created from counts alone")
    return {"schema": POLICY_SCHEMA, "policy": PROJECT_CREATION,
            "enabled": _flag(record.get("enabled"), "enabled"),
            "minIndependentFixes": minimum, "requireSharedGoal": True,
            "requireCompletionCriteria": True, "basis": _text(record.get("basis"), "basis")}


INCIDENT_KEYS = ("schema", "product", "repository", "workspace", "surface", "phase", "component",
                 "symptom",
                 "severity", "expected", "origin", "occurrenceKey", "observedAt", "context",
                 "cause", "goal", "detail", "evidence")
CONTEXT_KEYS = ("currentIssue", "run", "session", "regressionOf")
CAUSE_KEYS = ("product", "faultId", "signature")
GOAL_KEYS = ("key", "criteria")
DETAIL_KEYS = ("impact", "expected", "actual", "reproduction", "owner", "nextAction")


def read_incident(record) -> dict:
    """One incident from any of the four surfaces, in one shape.

    Severity is the observing source's own reading: these sources are CRW-managed runs and events
    a product explicitly connected, both on the harness side. Classification can set the product,
    component or goal later; it never sets severity, because the ledger keeps the highest
    severity it has seen and nothing could lower one a classifier raised.
    """
    _object(record, INCIDENT_SCHEMA, INCIDENT_KEYS, "an incident")
    context = record.get("context") or {}
    _closed(context, CONTEXT_KEYS, "context")
    cause = record.get("cause")
    if cause is not None:
        _closed(cause, CAUSE_KEYS, "cause")
        signature = cause.get("signature")
        if signature is not None and (not isinstance(signature, dict) or not signature):
            malformed("cause.signature is a non-empty object")
        cause = {"product": _key(cause.get("product"), "cause.product"),
                 "faultId": _text(cause.get("faultId"), "cause.faultId", optional=True),
                 "signature": dict(signature) if signature else None}
    goal = record.get("goal")
    if goal is not None:
        _closed(goal, GOAL_KEYS, "goal")
        goal = {"key": _key(goal.get("key"), "goal.key"),
                "criteria": _text(goal.get("criteria"), "goal.criteria", optional=True)}
    detail = record.get("detail") or {}
    _closed(detail, DETAIL_KEYS, "detail")
    evidence = record.get("evidence") or []
    if not isinstance(evidence, list):
        malformed("evidence is a list")
    return {
        "schema": INCIDENT_SCHEMA,
        "product": _key(record.get("product"), "product", optional=True),
        "repository": _key(record.get("repository"), "repository", optional=True),
        "workspace": _key(record.get("workspace"), "workspace", optional=True),
        "surface": _choice(record.get("surface"), SURFACES, "surface"),
        "phase": _choice(record.get("phase"), PHASES, "phase"),
        "component": _key(record.get("component"), "component"),
        "symptom": _key(record.get("symptom"), "symptom"),
        "severity": _choice(record.get("severity"), SEVERITIES, "severity"),
        "expected": _choice(record.get("expected"), EXPECTED, "expected", optional=True),
        "origin": _choice(record.get("origin") or OBSERVED, ORIGINS, "origin"),
        "occurrenceKey": _text(record.get("occurrenceKey"), "occurrenceKey", limit=256),
        "observedAt": _text(record.get("observedAt"), "observedAt", optional=True, limit=64),
        "context": {
            "currentIssue": _issue(context.get("currentIssue"), "context.currentIssue",
                                   optional=True),
            "run": _text(context.get("run"), "context.run", optional=True, limit=128),
            "session": _text(context.get("session"), "context.session", optional=True,
                             limit=128),
            "regressionOf": _text(context.get("regressionOf"), "context.regressionOf",
                                  optional=True, limit=256),
        },
        "cause": cause,
        "goal": goal,
        "detail": {key: _text(detail.get(key), f"detail.{key}", optional=True)
                   for key in DETAIL_KEYS},
        "evidence": evidence,
    }


def canonical(value) -> str:
    """Sorted keys and no spaces, so one record is one string whoever built it."""
    return json.dumps(value, ensure_ascii=False, sort_keys=True, separators=(",", ":"))
