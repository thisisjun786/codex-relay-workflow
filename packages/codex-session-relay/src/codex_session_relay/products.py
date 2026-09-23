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
import math
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
# A product is a plain identifier: the ledger ends a product where a target key's first
# separator begins, so ':', '@', '|' and '/' can never be part of one.
PRODUCT = re.compile(r"^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$")
MAX_TEXT = 600
# Evidence is a few references, never a transcript. An incident carrying more than this is
# refused rather than stored: the bound is what keeps an intake from becoming a copy of whatever
# conversation, file or personal data a source happened to hold.
MAX_EVIDENCE_ENTRIES = 16
MAX_EVIDENCE_BYTES = 4096
EVIDENCE_KEYS = ("kind", "ref", "source", "observed")
MAX_SIGNATURE_FIELDS = 16
MAX_SIGNATURE_BYTES = 1024
# The largest cursor a listing can have returned: SQLite's rowid is a signed 64-bit integer.
MAX_CURSOR = 2 ** 63 - 1

# Fault classes this module defines. They are registered with the ledger through ledger_port,
# which is the only module allowed to reach it; each names what clears it, because a class
# nothing can clear never closes. Only the completion mismatch sets its own threshold: one
# reading that looked for the evidence and did not find it is the whole proof, so it is recorded
# at degraded and files on that first reading, where a product defect at degraded waits for the
# ledger's repetition rule. Nothing records it at another severity.
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
               "clears": "the evidence the completion lacked being observed and reverified",
               "threshold": 1},
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
# Not a hold: the decision a named cause nobody could verify stands as, apart from where the
# defect itself is placed (routes.CAUSE_UNVERIFIED).
CAUSE_UNVERIFIED = "cause_unverified"
OWNER_FOUND_AFTER_CREATE = "owner_found_after_create"
# A project a confirmed create made in a team the product's registry no longer names.
PROJECT_TEAM_CHANGED = "project_team_changed"
HOLDS = (NO_PROJECT, OWNER_PROJECT_MISSING, AMBIGUOUS_OWNER, AMBIGUOUS_PROJECT,
         OWNER_FOUND_AFTER_CREATE, PROJECT_TEAM_CHANGED)


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


def _absent(value, default):
    """The default only for a field that is absent or null. Any other value, an empty list or
    string included, stays the caller's to type-check: reading [] as {} would accept a
    malformed record as missing data and change the verdict built on it."""
    return default if value is None else value


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


def _product(value, name, *, optional=False):
    if value is None and optional:
        return None
    if not isinstance(value, str) or not PRODUCT.match(value):
        malformed(f"{name} is a plain product identifier ({PRODUCT.pattern}), not {value!r}")
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
    product = _product(record.get("product"), "product")
    if product == UNCLASSIFIED:
        malformed(f"{UNCLASSIFIED!r} is the pending-classification bucket, not a product")
    surfaces = _absent(record.get("surfaces"), {})
    if not isinstance(surfaces, dict):
        malformed("surfaces is an object keyed by surface")
    watched = {}
    for surface, spec in sorted(surfaces.items()):
        _choice(surface, SURFACES, "a surface")
        _closed(spec, ("method", "active"), f"surfaces.{surface}")
        watched[surface] = {"method": _text(spec.get("method"), f"surfaces.{surface}.method"),
                            "active": _flag(spec.get("active"), f"surfaces.{surface}.active")}
    answer = {
        "schema": REGISTRY_SCHEMA, "product": product,
        "workspace": _key(record.get("workspace"), "workspace"),
        "team": _text(record.get("team"), "team"),
        "familyLabel": _text(record.get("familyLabel"), "familyLabel"),
        "repositories": _keys(record.get("repositories"), "repositories"),
        "surfaces": watched,
        "triageProject": _text(record.get("triageProject"), "triageProject", optional=True),
        "testTarget": _target(record.get("testTarget"), "testTarget"),
    }
    target = answer["testTarget"]
    if target is not None and answer["triageProject"] == target["project"]:
        # The ledger keeps one owned target per product, workspace and project. A test target
        # sharing a real project would let a simulated record repoint that project's real
        # writes to the test team, so the two stay apart.
        malformed(f"the test target project {target['project']!r} is also the triage project;"
                  f" a simulated record and a real one would share one project's target")
    return answer


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
        "schema": BINDING_SCHEMA, "product": _product(record.get("product"), "product"),
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
        elif registry["testTarget"] is not None:
            # The converse: a real project or issue on the test target project would share its
            # owned target with simulated records, which set that target's team to the test one.
            where = answer["ref"] if kind == "project" else answer.get("project")
            if where == registry["testTarget"]["project"]:
                malformed(f"{where!r} is the test target project; only a test binding sits"
                          f" there, or a simulated record and a real one would share one"
                          f" project's target")
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
    context = _absent(record.get("context"), {})
    _closed(context, CONTEXT_KEYS, "context")
    cause = record.get("cause")
    if cause is not None:
        _closed(cause, CAUSE_KEYS, "cause")
        signature = cause.get("signature")
        cause = {"product": _product(cause.get("product"), "cause.product"),
                 "faultId": _text(cause.get("faultId"), "cause.faultId", optional=True),
                 "signature": None if signature is None else read_cause_signature(signature)}
    goal = record.get("goal")
    if goal is not None:
        _closed(goal, GOAL_KEYS, "goal")
        goal = {"key": _key(goal.get("key"), "goal.key"),
                "criteria": _text(goal.get("criteria"), "goal.criteria", optional=True)}
    detail = _absent(record.get("detail"), {})
    _closed(detail, DETAIL_KEYS, "detail")
    return {
        "schema": INCIDENT_SCHEMA,
        "product": _product(record.get("product"), "product", optional=True),
        "repository": _key(record.get("repository"), "repository", optional=True),
        "workspace": _key(record.get("workspace"), "workspace", optional=True),
        "surface": _choice(record.get("surface"), SURFACES, "surface"),
        "phase": _choice(record.get("phase"), PHASES, "phase"),
        "component": _key(record.get("component"), "component"),
        "symptom": _key(record.get("symptom"), "symptom"),
        "severity": _choice(record.get("severity"), SEVERITIES, "severity"),
        "expected": _choice(record.get("expected"), EXPECTED, "expected", optional=True),
        "origin": _choice(_absent(record.get("origin"), OBSERVED), ORIGINS, "origin"),
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
        "evidence": read_evidence(record.get("evidence")),
    }


def read_evidence(values) -> list:
    """References to what was observed, bounded in count, shape and size.

    Each entry names its kind and a reference, optionally where it came from and a flat object
    of scalar readings. Nested structure, long text and anything past the byte bound are
    refused: evidence says where to look, it does not carry the thing looked at.
    """
    values = _absent(values, [])
    if not isinstance(values, list):
        malformed("evidence is a list")
    if len(values) > MAX_EVIDENCE_ENTRIES:
        malformed(f"evidence has {len(values)} entries; at most {MAX_EVIDENCE_ENTRIES}")
    entries = []
    for index, value in enumerate(values):
        name = f"evidence[{index}]"
        _closed(value, EVIDENCE_KEYS, name)
        entry = {"kind": _key(value.get("kind"), f"{name}.kind"),
                 "ref": _text(value.get("ref"), f"{name}.ref", limit=256)}
        if value.get("source") is not None:
            entry["source"] = _text(value.get("source"), f"{name}.source", limit=128)
        observed = value.get("observed")
        if observed is not None:
            if not isinstance(observed, dict) or len(observed) > 16:
                malformed(f"{name}.observed is an object of at most 16 readings")
            for key, reading in observed.items():
                _key(key, f"{name}.observed key")
                if isinstance(reading, str):
                    _text(reading, f"{name}.observed.{key}", limit=256)
                else:
                    _scalar(reading, f"{name}.observed.{key} is a scalar reading")
            entry["observed"] = dict(observed)
        entries.append(entry)
    if len(canonical(entries).encode("utf-8")) > MAX_EVIDENCE_BYTES:
        malformed(f"evidence is larger than {MAX_EVIDENCE_BYTES} bytes; it names where to look,"
                  f" not the thing looked at")
    return entries


def read_cause_signature(value) -> dict:
    """The signature an incident gives for the cause it names, to compare with the ledger's own.

    A signature names a failure domain and carries nothing, so it is read like every signature
    the ledger records: a flat object of at most sixteen scalar fields and 1024 bytes. Its values
    are checked, never rewritten, so it still compares equal to the stored signature it names.
    Anything nested, long or past the bound is refused before anything is written, because an
    incident held for an unverified cause keeps exactly what it was given.
    """
    name = "cause.signature"
    if not isinstance(value, dict) or not value or len(value) > MAX_SIGNATURE_FIELDS:
        malformed(f"{name} is a non-empty object of at most {MAX_SIGNATURE_FIELDS} fields")
    for key, reading in value.items():
        _key(key, f"{name} key")
        if isinstance(reading, str):
            if len(reading) > 256:
                malformed(f"{name}.{key} is longer than 256 characters")
        else:
            _scalar(reading, f"{name}.{key} is a scalar; a signature names a failure domain")
    if len(canonical(value).encode("utf-8")) > MAX_SIGNATURE_BYTES:
        malformed(f"{name} is larger than {MAX_SIGNATURE_BYTES} bytes")
    return dict(value)


def _scalar(value, refusal):
    """Null, a boolean or a finite number, else refused with the given text. NaN and the
    infinities are floats JSON has no text for."""
    if value is None or isinstance(value, int):
        return value
    if isinstance(value, float) and math.isfinite(value):
        return value
    malformed(refusal)


def canonical(value) -> str:
    """Sorted keys and no spaces, so one record is one string whoever built it. A value with no
    JSON text (NaN, an infinity) is refused rather than stored as text JSON readers reject."""
    try:
        return json.dumps(value, ensure_ascii=False, sort_keys=True, separators=(",", ":"),
                          allow_nan=False)
    except ValueError as error:
        malformed(f"a value has no JSON text: {error}")


def read_page(limit, after, *, ceiling) -> tuple:
    """(limit, after) of a caller's page: a positive whole number, and no cursor or a
    non-negative whole number inside SQLite's rowid range. Anything else is refused rather than
    repaired, because a bound on what is read is a bound on what is written too, and a nonsense
    one is no permission to do one. A limit above the ceiling is cut to the ceiling, the rule
    the ledger's own faults.bounded applies: every caller continues from where it stopped, by
    the next it returns or by rotating what it read, so a larger ask is only a slower one. A
    cursor in range is only a position: one no listing returned starts the page at that rowid
    and reads nothing it should not."""
    if isinstance(limit, bool) or not isinstance(limit, int) or limit < 1:
        malformed(f"limit is a positive whole number, not {limit!r}")
    if after is not None and (isinstance(after, bool) or not isinstance(after, int)
                              or not 0 <= after <= MAX_CURSOR):
        malformed(f"after is a non-negative rowid cursor, not {after!r}")
    return min(limit, ceiling), after
