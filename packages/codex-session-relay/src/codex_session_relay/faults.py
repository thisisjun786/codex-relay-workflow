"""Operational faults, recorded once and filed once.

A fault is the machinery failing to do its job: the turn that settled without reporting, the
deliveries that will not leave the queue for one recipient, the Linear writes that exhausted
their attempts against one document. It is not a child failing at its task - supervision owns
deciding what the level above is owed about that - and the two records answer different
questions. A supervisor obligation asks whether the level above has been told; a fault asks
whether this system is working, and it is discharged only when the cause has been fixed and
the fix reverified.

Three rules shape everything here.

Identity is the failure DOMAIN, never the incident. One unreachable recipient strands every
delivery queued for it, so keying on the delivery would file one issue per stranded event for
a single broken recipient - exactly the spray this ledger exists to prevent. The individual
deliveries are occurrences of one fault.

Order is this store's own insertion sequence. Every comparison that decides a transition is
made on rowid, because a timestamp a caller supplied is not a fact this store owns: one
occurrence misdated to 2099 would otherwise refuse every honest reverification until then,
and a recurrence carrying a stale timestamp would sort before the check that missed it and
let the fault be resolved anyway. Observed timestamps travel as evidence and decide nothing.
Elapsed windows are measured on the injected clock, which is the clock the delivery backoff
already trusts.

A second create is never issued automatically. The connector's issue create takes no
idempotency key, so a create can succeed and lose its response, and no local identifier fixes
that. Exactly-once creation is not claimed. What is enforced is narrower and real: handing out
a create marks the row issued, and an expiring lease on an issued row goes to uncertain rather
than back to pending, where nothing but an explicit reconciliation reporting what somebody
actually observed can move it.
"""

import json
import re
import secrets

from . import sync
from .errors import RefusalReason, RelayError
from .identity import sha256_hex

SCHEMA = "fault-observation/1"
LEDGER_SCHEMA = "fault-ledger/1"
ID_WIDTH = 32

# Severity is the observer's judgement of what is happening; the threshold table below turns
# it into whether anybody is told.
NOTICE = "notice"
DEGRADED = "degraded"
BROKEN = "broken"
SEVERITIES = (NOTICE, DEGRADED, BROKEN)
SEVERITY_RANK = {NOTICE: 0, DEGRADED: 1, BROKEN: 2}

OBSERVED = "observed"
OPEN = "open"
FIX_PENDING = "fix_pending"
RESOLVED = "resolved"
WITHDRAWN = "withdrawn"
STATES = (OBSERVED, OPEN, FIX_PENDING, RESOLVED, WITHDRAWN)

FIX = "fix"
REVERIFICATION = "reverification"
METHODS = ("suite", "command", "observation")
# What a reverification FOUND, from a closed vocabulary. An open string let
# outcome="still failing" satisfy the resolution gate, which is the one sentence a fault
# ledger must never accept as proof that a fault is gone.
PASSED = "passed"
ABSENT = "absent"
FAILED_CHECK = "failed"
OUTCOMES = (PASSED, ABSENT, FAILED_CHECK)
# The two that establish the fault was looked for and not found. A failed check is recorded
# because it is worth having, and it resolves nothing.
RESOLVING_OUTCOMES = (PASSED, ABSENT)

PENDING = "pending"
CLAIMED = "claimed"
ISSUED = "issued"
CONFIRMED = "confirmed"
FAILED = "failed"
UNCERTAIN = "uncertain"

CANCELLED = "cancelled"

OPEN_RECORD = "open_record"
APPEND_COMMENT = "append_comment"
UPDATE_RECORD = "update_record"
# The one-operation update on an issue the fault already owns. set_project is identified by the
# link revision rather than by its value, so a later target always gets its own write.
UPDATE_OPS = ("set_project", "reopen", "add_relation", "add_label")

# The identity field for an incident whose product workspace is not known yet.
UNASSIGNED = "unassigned"
# A product is a plain identifier: letters, digits, dot, underscore and dash. Without ':', '@'
# and '|' the product always ends where a target key's first separator begins.
PRODUCT_NAME = re.compile(r"^[A-Za-z0-9][A-Za-z0-9._-]*$")

# What the ledger records about the work that follows an issue, each as its own state.
STAGES = ("accepted", "assigned", "merged", "installed")
INSTALLED = "installed"

LINKED = "linked"
UNLINKED = "unlinked"
NO_LINK = "none"

# Upward notifications, and the budget kind they spend.
NOTIFICATION = "notification"
BLOCKING = "blocking"
DECISION = "decision"
RESOLVED_NOTICE = "resolved"
RESERVED = "reserved"
DELIVERED = "delivered"

# Per product and kind, per window: how many writes or notifications may go out.
DEFAULT_LIMITS = {
    OPEN_RECORD: (5, 3600.0),
    APPEND_COMMENT: (20, 3600.0),
    UPDATE_RECORD: (20, 3600.0),
    NOTIFICATION: (10, 3600.0),
}
DEFAULT_KIND_LIMIT = (20, 3600.0)
RELINK_PER_CALL = 100
HOLD_SECONDS = 30.0
MAX_POLICY_THRESHOLD = 100
MIN_POLICY_WINDOW = 60.0
MAX_POLICY_WINDOW = 2592000.0
MAX_BUDGET = 10000

# Why a publication exists. It is part of the publication's identity, so one reason queues one
# write however many times the ledger is swept.
TRIGGER_OPEN = "open"
TRIGGER_ESCALATE = "escalate"
TRIGGER_RECUR = "recur"
TRIGGER_FIX = "fix"
TRIGGER_RESOLVE = "resolve"
TRIGGER_REOPEN = "reopen"

MAX_EVIDENCE = 8
MAX_EVIDENCE_BYTES = 4096
MAX_ATTEMPTS = 8
LEASE_SECONDS = 300.0
BASE_BACKOFF = 30.0
MAX_BACKOFF = 900.0
DEFAULT_WINDOW = 21600.0
RENDERED_OCCURRENCES = 3
# What an operator listing shows. Both bound a query, and both are refused below 1.
SHOWN_PER_PAGE = 20
SHOWN_PER_FAULT = 20

# One observation is enough for something that is not being performed at all; three inside the
# window for something working badly, because once may be weather; a notice is recorded for an
# operator and never filed. A class may override the count, and nothing else may.
THRESHOLD_BY_SEVERITY = {BROKEN: 1, DEGRADED: 3, NOTICE: None}

BLOCK_FORMAT = "v1"
RENDERED_FIELDS = (
    "blockFormat", "publicationId", "faultId", "product", "faultClass", "trigger", "cycle",
    "identityDigest", "summarySha256",
)


class FaultRefused(RelayError):
    """A fault observation, transition or publication was not accepted."""


# Every registered class declares what CLEARS it, and that is not documentation. A fault
# nothing can clear stays open forever, and a ledger full of those tells an operator nothing,
# so a class without a clear source is refused at registration. refusal_recurring is the
# absence worth naming: the refusals table is append-only and carries no later success, so
# nothing in this store could ever clear one.
CLASS_POLICY = {}


def latest_id(db, seq):
    """The remediation a timeline row points at, so a converged call can name it."""
    row = db.execute("SELECT ref_id FROM fault_timeline WHERE seq = ?", (seq,)).fetchone()
    return row["ref_id"] if row else None


def _bounded(value, name, ceiling=1000):
    """A bound has to be a positive integer. SQLite reads LIMIT -1 as no limit at all, so a
    negative one handed in from a command line removed the bound it was asking for."""
    if not isinstance(value, int) or isinstance(value, bool) or value < 1:
        raise FaultRefused(RefusalReason.FAULT_OBSERVATION_MALFORMED,
                           f"{name} is a positive integer, not {value!r}")
    return min(value, ceiling)


# Public name for the other fault modules. One rule for every bound in the fault path, so a
# second copy cannot drift from it.
def bounded(value, name, ceiling=1000):
    return _bounded(value, name, ceiling)


def _named(value):
    """A name is a non-blank string, which a list and a blank both fail."""
    return isinstance(value, str) and bool(value.strip())


def register_class(fault_class, *, component, clears, threshold=None, window=None) -> dict:
    """Declare a fault class. A second product registers its own and nothing else changes.

    threshold=None means the severity table decides, which is the ordinary case. A class
    supplies one only when its own recurrence means something the general rule does not.
    """
    if not _named(fault_class):
        raise ValueError("a fault class is a non-blank string")
    if not _named(component):
        raise ValueError(f"{fault_class!r} declares no component")
    if not _named(clears):
        raise ValueError(
            f"{fault_class!r} declares no clear source. A fault nothing can clear never closes"
        )
    policy = {"component": component, "clears": clears, "threshold": threshold,
              "window": DEFAULT_WINDOW if window is None else float(window)}
    existing = CLASS_POLICY.get(fault_class)
    if existing is not None and existing != policy:
        # Re-registering the same terms is idempotent; changing them is not. A silent
        # replacement would let one extension redefine a built-in class for every ledger in
        # the process, including the threshold that decides what reaches Linear.
        raise ValueError(
            f"{fault_class!r} is already registered with different terms; pick another name"
        )
    CLASS_POLICY[fault_class] = policy
    return {"faultClass": fault_class, **policy}


register_class(
    "delivery_stalled", component="delivery",
    clears="a delivery to the same recipient reaching dispatched",
)
register_class(
    "record_sync_failed", component="sync",
    clears="any synchronisation job on the same target confirming",
)
register_class(
    "observation_stalled", component="observation",
    clears="a successful poll of the same anchor, or its turn settling",
)
register_class(
    "report_omitted", component="reporting",
    clears="a reading of the same turn that no longer says unreported",
)
register_class(
    "observation_unmeasured", component="reporting",
    clears="a later reading of the same turn that establishes something",
)
register_class(
    "delivery_refused", component="delivery",
    clears="the delivery being sent or settling, or its newest withholding naming another"
           " reason",
)
register_class(
    "managed_start_failed", component="managed_start",
    clears="a later receipt for the same request being accepted",
)


def policy_for(fault_class, severity) -> dict:
    """What this class and severity mean for publication, or a refusal for an unknown class."""
    policy = CLASS_POLICY.get(fault_class)
    if policy is None:
        raise FaultRefused(
            RefusalReason.FAULT_CLASS_UNREGISTERED,
            f"{fault_class!r} is not a registered fault class, so nothing declares what would"
            f" clear it",
        )
    if severity not in SEVERITY_RANK:
        raise FaultRefused(
            RefusalReason.FAULT_OBSERVATION_MALFORMED,
            f"severity {severity!r} is not one of {SEVERITIES}",
        )
    threshold = policy["threshold"]
    if threshold is None:
        threshold = THRESHOLD_BY_SEVERITY[severity]
    return {
        "faultClass": fault_class, "component": policy["component"], "severity": severity,
        "threshold": threshold, "window": policy["window"],
        "publish": threshold is not None, "clears": policy["clears"],
    }


def canonical_signature(signature) -> str:
    """The signature as bytes, ordered, so two callers building one dictionary agree.

    Sorted keys and no spaces: a dictionary is unordered and JSON is not, so rendering it
    verbatim would make one fault produce two identities depending on insertion order.
    """
    if not isinstance(signature, dict) or not signature:
        raise FaultRefused(
            RefusalReason.FAULT_OBSERVATION_MALFORMED,
            "a signature is a non-empty object naming the failure domain",
        )
    return json.dumps(signature, sort_keys=True, ensure_ascii=False, separators=(",", ":"))


def fault_id(product, fault_class, signature, *, workspace=None) -> str:
    """One breakage, one id, however many times and by whoever it is observed.

    The time, the occurrence, the attempt, the event and the project are all deliberately
    outside it. Each of them changes while the fault stays the same, and an identity carrying
    any of them files a second issue every time the system fails again.

    The workspace is inside it, because two workspaces are two tenants and the same failure in
    each is two faults. It joins the text only when one is given, so every id computed before
    workspaces existed is unchanged. Computable with no store, so a caller can decide an owner
    or a target before the first record.
    """
    _check_product(product)
    if not _named(fault_class):
        raise FaultRefused(RefusalReason.FAULT_OBSERVATION_MALFORMED,
                           "faultClass must be a non-empty string")
    if "|" in fault_class:
        raise FaultRefused(RefusalReason.FAULT_OBSERVATION_MALFORMED,
                           "faultClass must not contain '|', which is the field separator")
    text = f"{product}|{fault_class}|{canonical_signature(signature)}"
    if workspace is not None:
        if not _named(workspace):
            raise FaultRefused(RefusalReason.FAULT_OBSERVATION_MALFORMED,
                               "a workspace is a non-blank string")
        text += f"|workspace={workspace}"
    return sha256_hex(text)[:ID_WIDTH]


def _check_product(product):
    if not isinstance(product, str) or not PRODUCT_NAME.match(product):
        raise FaultRefused(
            RefusalReason.FAULT_OBSERVATION_MALFORMED,
            f"product {product!r} is not a plain identifier (letters, digits, '.', '_', '-');"
            f" a ':' '@' or '|' would let one product's key read as another's",
        )


def occurrence_id(identifier, occurrence_key, episode=1, cleared=False) -> str:
    """One occurrence, per episode, per direction.

    The episode is what separates "the sweep read this stuck row again" from "the thing that
    was fixed is back". Both arrive under the same key, because the key names the underlying
    fact; only the episode tells them apart.

    The direction is in here for the adapter that clears under the SAME key it raised: this
    package's own reading adapter appends a suffix, but nothing forces one, and without the
    flag such a clear collided with the observation it was meant to close and was dropped as
    already recorded - leaving a fault open while its cause was gone.
    """
    direction = "cleared" if cleared else "active"
    return sha256_hex(f"{identifier}|{episode}|{direction}|{occurrence_key}")[:ID_WIDTH]


def publication_id(identifier, kind, trigger_key) -> str:
    """Identity includes WHY, so one reason queues one write however often it is swept."""
    return sha256_hex(f"{identifier}|{kind}|{trigger_key}")[:ID_WIDTH]


def identity_digest(identifier, kind, trigger_key, cycle) -> str:
    return sha256_hex(f"{identifier}|{kind}|{trigger_key}|{cycle}")


def evidence_digest(evidence) -> str:
    return sha256_hex(json.dumps(evidence, sort_keys=True, ensure_ascii=False,
                                 separators=(",", ":")))


def target_key(product, *, workspace=None, project=None) -> str:
    """The key a target and a fault's scope share. Injective, and unchanged for old scopes.

    Without a workspace it is the merged key, product or product:project, and the product
    cannot contain ':' so the product always ends at the first one. With a workspace every part
    is percent-encoded behind a 'ws|' prefix, which no merged key can begin with because no
    product contains '|'.
    """
    if workspace is None:
        return f"{product}:{project}" if project is not None else product
    return "ws|" + "|".join(_encode("" if part is None else str(part))
                            for part in (product, workspace, project))


def _encode(part):
    """Percent-encode the four characters a key is split on. '%' first, so it stays injective."""
    for character, code in (("%", "%25"), ("|", "%7C"), (":", "%3A"), ("@", "%40")):
        part = part.replace(character, code)
    return part


def scope_key_for(product, scope) -> str:
    """Where this fault is filed. Outside identity, because a re-read scope is one fault."""
    scope = scope or {}
    project = scope.get("projectKey")
    return target_key(product, workspace=scope.get("workspace"),
                      project=project if _named(project) else None)


def _bounded_evidence(evidence):
    """At most MAX_EVIDENCE entries and MAX_EVIDENCE_BYTES, with the truncation recorded.

    Shortened rather than refused, and the record says it was shortened. A fault dropped
    because its evidence was long would be the one observation nobody ever sees.
    """
    if evidence is None:
        return [], False
    if not isinstance(evidence, list):
        raise FaultRefused(
            RefusalReason.FAULT_OBSERVATION_MALFORMED, "evidence is a list of objects",
        )
    entries = [entry for entry in evidence if isinstance(entry, dict)]
    # An entry this cannot record is a shortening like any other, and the record says so.
    # Dropping it quietly made a snapshot that had lost something look complete.
    truncated = len(entries) < len(evidence)
    kept = entries[:MAX_EVIDENCE]
    truncated = truncated or len(kept) < len(entries)
    while kept and len(json.dumps(kept, ensure_ascii=False).encode("utf-8")) > MAX_EVIDENCE_BYTES:
        kept.pop()
        truncated = True
    return kept, truncated


def read_observation(observation_record) -> dict:
    """Validate one observation and derive everything this store keys on.

    observedAt is read and kept. It is never compared; see the module docstring.
    """
    if not isinstance(observation_record, dict):
        raise FaultRefused(
            RefusalReason.FAULT_OBSERVATION_MALFORMED,
            f"an observation is an object, not {type(observation_record).__name__}",
        )
    if observation_record.get("schema") != SCHEMA:
        raise FaultRefused(
            RefusalReason.FAULT_OBSERVATION_MALFORMED,
            f"schema {observation_record.get('schema')!r} is not {SCHEMA}",
        )
    product = observation_record.get("product")
    fault_class = observation_record.get("faultClass")
    severity = observation_record.get("severity")
    policy = policy_for(fault_class, severity)
    _check_product(product)
    occurrence_key = observation_record.get("occurrenceKey")
    if not _named(occurrence_key):
        raise FaultRefused(
            RefusalReason.FAULT_OBSERVATION_MALFORMED,
            "occurrenceKey must be a non-empty string derived from the underlying fact, or a"
            " repeated sweep of one unchanged row counts as many occurrences",
        )
    scope = _read_scope(observation_record.get("scope"))
    identifier = fault_id(product, fault_class, observation_record.get("signature"),
                          workspace=scope.get("workspace"))
    detail = _optional_text(observation_record.get("detail"), "detail")
    observed_at = _optional_text(observation_record.get("observedAt"), "observedAt")
    evidence, truncated = _bounded_evidence(observation_record.get("evidence"))
    return {
        "faultId": identifier,
        "workspace": scope.get("workspace"),
        "signatureObject": dict(observation_record.get("signature")),
        "product": product,
        "faultClass": fault_class,
        "component": policy["component"],
        "severity": severity,
        "signature": canonical_signature(observation_record.get("signature")),
        "scope": scope,
        "scopeKey": scope_key_for(product, scope),
        "occurrenceKey": occurrence_key,
        # Filled in by the ledger, which is what knows the episode.
        "occurrenceId": occurrence_id(identifier, occurrence_key),
        "observedAt": observed_at,
        "detail": detail or "",
        "evidence": evidence,
        "evidenceDigest": evidence_digest(evidence),
        "truncated": truncated,
        "cleared": _flag(observation_record.get("cleared")),
        "policy": policy,
    }


def _optional_text(value, name):
    """A string or nothing. Anything else is refused here, before a value SQLite cannot bind
    turns a malformed observation into what looks like an outage."""
    if value is None or isinstance(value, str):
        return value
    raise FaultRefused(RefusalReason.FAULT_OBSERVATION_MALFORMED,
                       f"{name} is a string, not {type(value).__name__}")


def _read_scope(scope):
    """A scope: an object of JSON scalars, whose workspace and projectKey are non-blank strings.

    A number or an empty string is refused, so no two inputs can name one target.
    """
    if scope is None:
        return {}
    if not isinstance(scope, dict):
        raise FaultRefused(RefusalReason.FAULT_OBSERVATION_MALFORMED, "scope is an object")
    for key, value in scope.items():
        if not isinstance(key, str) or not (value is None or isinstance(
                value, (str, int, float, bool))):
            raise FaultRefused(RefusalReason.FAULT_OBSERVATION_MALFORMED,
                               f"scope.{key} is not a JSON scalar")
    for key in ("workspace", "projectKey"):
        if key in scope and scope[key] is not None and not _named(scope[key]):
            raise FaultRefused(RefusalReason.FAULT_OBSERVATION_MALFORMED,
                               f"scope.{key} is a non-blank string")
    return {key: value for key, value in scope.items() if value is not None}


def _flag(value):
    """An actual boolean. JSON carries the string "false", and bool("false") is True - which
    would have let a malformed clear withdraw a fault that is still happening."""
    if value is None:
        return False
    if not isinstance(value, bool):
        raise FaultRefused(
            RefusalReason.FAULT_OBSERVATION_MALFORMED,
            f"cleared is a boolean, not {type(value).__name__}",
        )
    return value


def observation(*, product, fault_class, severity, signature, occurrence_key, scope=None,
                observed_at=None, detail="", evidence=(), cleared=False) -> dict:
    """Build one observation. Adapters use this so the shape has exactly one author."""
    return {
        "schema": SCHEMA, "product": product, "faultClass": fault_class,
        "component": (CLASS_POLICY.get(fault_class) or {}).get("component"),
        "severity": severity, "signature": dict(signature), "scope": dict(scope or {}),
        "occurrenceKey": occurrence_key, "observedAt": observed_at, "detail": detail,
        "evidence": list(evidence), "cleared": cleared,
    }


def start_marker(identifier) -> str:
    return f"<!-- relay-fault:{identifier} -->"


def end_marker(identifier) -> str:
    return f"<!-- /relay-fault:{identifier} -->"


def render_block(row, *, summary=None) -> str:
    """One publication owns one block, delimited by markers derived from its stable id.

    The summary sits inside a fenced literal block for the reason the coordination document
    learned the hard way: the connector normalises body text - a bare filename becomes an
    autolink, brackets and asterisks are escaped - and a readback then correctly refuses a
    record that no longer matches what was sent.
    """
    text = sync.canonical_summary(row["summary"] if summary is None else summary)
    fence = sync.summary_fence(text)
    fields = [
        ("blockFormat", BLOCK_FORMAT),
        ("publicationId", row["publication_id"]),
        ("faultId", row["fault_id"]),
        ("product", row["product"]),
        ("faultClass", row["fault_class"]),
        ("trigger", row["trigger_key"]),
        ("cycle", row["cycle"]),
        ("identityDigest", row["identity_digest"]),
        ("summarySha256", sha256_hex(text)),
    ]
    lines = [start_marker(row["publication_id"])]
    lines += [f"{key}: {'' if value is None else value}" for key, value in fields]
    lines.append("")
    lines.append(fence + "text")
    lines.extend(text.split("\n"))
    lines.append(fence)
    lines.append(end_marker(row["publication_id"]))
    return "\n".join(lines)


def _fence(line):
    """The backtick run opening or closing a fence on this line, or None."""
    stripped = line.strip()
    count = 0
    while count < len(stripped) and stripped[count] == sync.FENCE_CHARACTER:
        count += 1
    if count < sync.MINIMUM_FENCE:
        return None
    info = stripped[count:]
    return None if sync.FENCE_CHARACTER in info else (count, info)


def read_block(text, identifier) -> dict:
    """This publication's block as observed, or an absence, with what is wrong with it.

    Read line by line rather than searched. Identity comes only from the header region, so a
    summary quoting 'faultId: something' cannot supply one, and the end marker is looked for
    only after the fence closes, so a summary quoting marker text cannot truncate the block.
    """
    answer = {"found": False, "fields": {}, "summary": None, "problems": [],
              "duplicated": False}
    if not text:
        return answer
    opening = start_marker(identifier)
    closing = end_marker(identifier)
    lines = text.split("\n")
    index = 0
    while index < len(lines):
        if lines[index].strip() != opening:
            index += 1
            continue
        if answer["found"]:
            answer["duplicated"] = True
            answer["problems"].append("more than one block for this publication is present")
            break
        index += 1
        fields, problems, seen = {}, [], set()
        while index < len(lines) and lines[index].strip() and lines[index].strip() != closing:
            key, separator, value = lines[index].partition(":")
            key = key.strip()
            if separator and key and " " not in key:
                if key in seen:
                    problems.append(f"duplicate header {key!r}")
                seen.add(key)
                fields[key] = value.strip()
            else:
                problems.append("the header region contains a line that is not a header")
            index += 1
        while index < len(lines) and not lines[index].strip():
            index += 1
        summary = None
        fence = _fence(lines[index]) if index < len(lines) else None
        if fence is None:
            problems.append("the block carries no fenced summary")
        else:
            length = fence[0]
            index += 1
            collected, closed = [], False
            while index < len(lines):
                candidate = _fence(lines[index])
                if candidate is not None and candidate[0] >= length and not candidate[1]:
                    closed = True
                    index += 1
                    break
                collected.append(lines[index])
                index += 1
            if closed:
                summary = "\n".join(collected)
            else:
                problems.append("the fenced summary is not closed")
        while index < len(lines) and not lines[index].strip():
            index += 1
        if index >= len(lines) or lines[index].strip() != closing:
            problems.append("the block is not terminated")
        else:
            index += 1
        answer = {"found": True, "fields": fields, "summary": summary, "problems": problems,
                  "duplicated": False}
    return answer


OCCURRENCE = "occurrence"
CLEARED = "cleared"


def _domain(row) -> str:
    """The failure domain in one line, for a title somebody has to scan."""
    try:
        signature = json.loads(row["signature"])
    except (TypeError, ValueError):
        return str(row["signature"])
    return " ".join(f"{key}={value}" for key, value in sorted(signature.items()))


def title_for(row) -> str:
    return f"[{row['product']}] {row['fault_class']}: {_domain(row)}"


def render_summary(row, *, trigger_key, occurrences=(), remediation=None, clears="",
                   publication=None) -> str:
    """What the Linear record says. Written for somebody who was not watching.

    The occurrence count is a count of what was OBSERVED. Where a source overwrites its own
    history - a poll row keeps only its latest attempt - that is fewer than what happened, and
    saying so here is cheaper than letting a reader take it for a total.
    """
    lines = [title_for(row), ""]
    reason = trigger_key.split(":")[0]
    if reason == TRIGGER_OPEN:
        lines.append(f"The relay recorded a {row['severity']} fault in its {row['component']}"
                     f" path and is filing it once.")
    elif reason == TRIGGER_ESCALATE:
        lines.append(f"This fault escalated to {row['severity']}.")
    elif reason == TRIGGER_RECUR:
        lines.append("This fault happened again after a fix was recorded, so the fix did not"
                     " hold and the record is open again.")
    elif reason == TRIGGER_REOPEN:
        lines.append(f"This fault happened again after it was resolved. It is the same record,"
                     f" reopened for cycle {row['cycle']}.")
    elif reason == TRIGGER_FIX:
        lines.append("A fix was recorded. This does not resolve the fault: a reverification"
                     " recorded after the fix, with no occurrence after it, does.")
    elif reason == TRIGGER_RESOLVE:
        lines.append("Resolved. A fix was recorded, a reverification was recorded after it,"
                     " and nothing has been observed since.")
    lines.append("")
    lines.append(f"fault: {row['fault_id']}")
    lines.append(f"class: {row['fault_class']}  component: {row['component']}"
                 f"  severity: {row['severity']}")
    lines.append(f"domain: {_domain(row)}")
    lines.append(f"observed: {row['occurrence_count']} occurrence(s),"
                 f" first {row['first_seen_at']}, most recent {row['last_seen_at']}")
    if clears:
        lines.append(f"clears when: {clears}")
    if row["detail"]:
        lines.append(f"detail: {row['detail']}")
    if remediation is not None:
        lines.append("")
        lines.append(f"{remediation['kind']}: {remediation['ref']}")
        for key in ("method", "outcome", "detail"):
            if remediation.get(key):
                lines.append(f"  {key}: {remediation[key]}")
    shown = list(occurrences)[:RENDERED_OCCURRENCES]
    if shown:
        lines.append("")
        lines.append("evidence, as observed at the time:")
        for entry in shown:
            lines.append(f"- {entry['recorded_at']} {entry['occurrence_key']}")
            for item in entry.get("evidence") or []:
                lines.append(f"    {json.dumps(item, ensure_ascii=False, sort_keys=True)}")
            if entry.get("truncated"):
                lines.append("    (evidence truncated at the recorded bound)")
    lines.append("")
    lines.append("Recorded by codex-session-relay. The occurrence count counts observations,"
                 " not incidents: a source that overwrites its own history is observed once"
                 " per sweep, not once per failure.")
    if publication is not None:
        lines.append(f"publication: {publication}")
    return "\n".join(lines)


class FaultLedger:
    """The ledger and its outbox. Nothing here performs a network call.

    The contract is docs/faults.md, "Corrected contract". Each invariant there names the one
    method below that enforces it; the others call that method rather than repeating its test.
    """

    def __init__(self, store, clock):
        self.store = store
        self.clock = clock

    # ------------------------------------------------------------------ identity (invariant 9)

    def _canonical(self, db, identifier):
        """The id the store keeps this fault under: an alias resolved, anything else unchanged."""
        row = db.execute("SELECT fault_id FROM fault_aliases WHERE alias_id = ?",
                         (identifier,)).fetchone()
        return row["fault_id"] if row else identifier

    def _legacy(self, db, product, fault_class, signature, workspace):
        """A fault recorded before workspace joined identity, whose stored scope says it
        belongs to this workspace. Such a fault is the one this workspace's id names."""
        if workspace is None:
            return None
        legacy = fault_id(product, fault_class, signature)
        row = db.execute("SELECT scope FROM fault_ledger WHERE fault_id = ?",
                         (legacy,)).fetchone()
        if row is not None and _json(row["scope"]).get("workspace") == workspace:
            return legacy
        return None

    def _resolve(self, db, product, fault_class, signature, workspace):
        identifier = fault_id(product, fault_class, signature, workspace=workspace)
        canonical = self._canonical(db, identifier)
        if canonical != identifier or _exists(db, identifier):
            return identifier, canonical
        legacy = self._legacy(db, product, fault_class, signature, workspace)
        return identifier, (legacy or identifier)

    def canonical_id(self, product, fault_class, signature, *, workspace=None) -> str:
        """The id this store keeps a fault under, before or after its first record."""
        return self._resolve(self.store.db, product, fault_class, signature, workspace)[1]

    def _register_alias(self, db, alias_id, target, now):
        if alias_id == target:
            return
        existing = db.execute("SELECT fault_id FROM fault_aliases WHERE alias_id = ?",
                              (alias_id,)).fetchone()
        if existing is not None:
            if existing["fault_id"] != target:
                raise FaultRefused(RefusalReason.FAULT_SCOPE_CONFLICT,
                                   f"{alias_id} already names fault {existing['fault_id']}")
            return
        if _exists(db, alias_id):
            raise FaultRefused(RefusalReason.FAULT_SCOPE_CONFLICT,
                               f"{alias_id} is already a recorded fault of its own")
        db.execute("INSERT INTO fault_aliases (alias_id, fault_id, created_at) VALUES (?,?,?)",
                   (alias_id, target, now))

    def _fault(self, db, identifier):
        row = db.execute("SELECT * FROM fault_ledger WHERE fault_id = ?",
                         (self._canonical(db, identifier),)).fetchone()
        if row is None:
            raise FaultRefused(RefusalReason.FAULT_UNKNOWN, f"no fault {identifier!r}")
        return row

    # ------------------------------------------------------------------ scope and targets

    def _assign_scope_key(self, db, product, scope_key):
        """Invariant 7: one product per scope key, on every path that assigns one."""
        other = db.execute(
            "SELECT fault_id, product FROM fault_ledger WHERE scope_key = ? AND product != ?"
            " LIMIT 1", (scope_key, product)).fetchone()
        if other is None:
            other = db.execute(
                "SELECT product FROM fault_target_projects WHERE scope_key = ? AND product != ?",
                (scope_key, product)).fetchone()
        if other is not None:
            raise FaultRefused(
                RefusalReason.FAULT_SCOPE_CONFLICT,
                f"scope key {scope_key!r} is already carried by product {other['product']!r};"
                f" one product's issues are never filed through another's key")

    def _owned_target(self, db, product, scope_key):
        """Invariant 8: this product's own target for the key, or None and why not."""
        row = db.execute(
            "SELECT t.tracker_ref, p.project_ref, p.product FROM fault_targets t"
            "  LEFT JOIN fault_target_projects p ON p.scope_key = t.scope_key"
            " WHERE t.scope_key = ?", (scope_key,)).fetchone()
        if row is None:
            return None, "awaiting_target"
        if row["product"] is None:
            return None, "awaiting_target"
        if row["product"] != product:
            return None, "scope_key_contested"
        contested = db.execute(
            "SELECT 1 FROM fault_ledger WHERE scope_key = ? AND product != ? LIMIT 1",
            (scope_key, product)).fetchone()
        if contested is not None:
            return None, "scope_key_contested"
        return {"team": row["tracker_ref"], "projectRef": row["project_ref"]}, None

    def set_target(self, *, product, workspace=None, project=None, team, project_ref=None) -> dict:
        """Where this product's faults in one scope are filed. Unchanged values write nothing.

        A change re-points the unsent writes whose target differs (an uncertain one stays where
        it may already have landed) and relinks issues this scope's faults own, a bounded number
        per call; relink() continues the rest. A target row written before targets had owners
        is claimed by this call, which is therefore not a no-op for it.
        """
        _check_product(product)
        for name, value in (("workspace", workspace), ("project", project),
                            ("project_ref", project_ref)):
            if value is not None and not _named(value):
                raise FaultRefused(RefusalReason.FAULT_OBSERVATION_MALFORMED,
                                   f"{name} is a non-blank string")
        if not _named(team):
            raise FaultRefused(RefusalReason.FAULT_OBSERVATION_MALFORMED,
                               "team is a non-blank string")
        key = target_key(product, workspace=workspace, project=project)
        now = self.clock.iso()
        with self.store.transaction() as db:
            self._assign_scope_key(db, product, key)
            current = db.execute(
                "SELECT t.tracker_ref, p.product, p.project_ref FROM fault_targets t"
                "  LEFT JOIN fault_target_projects p ON p.scope_key = t.scope_key"
                " WHERE t.scope_key = ?", (key,)).fetchone()
            answer = {"scopeKey": key, "product": product, "team": team, "projectRef": project_ref,
                      "changed": False, "backfilled": 0, "backfillPending": 0, "relinked": 0,
                      "relinkPending": 0}
            if (current is not None and current["product"] == product
                    and current["tracker_ref"] == team and current["project_ref"] == project_ref):
                # Unchanged, so nothing is written. What an earlier change left is still
                # counted: a caller retrying a call whose answer was lost must not read the
                # remaining work as done. relink() continues it.
                answer["backfillPending"] = self._repoint_where(db, now, scope_key=key,
                                                                limit=0)[1]
                answer["relinkPending"] = self._relink_where(db, now, scope_key=key,
                                                             limit=0)[1]
                return answer
            db.execute(
                "INSERT INTO fault_targets (scope_key, tracker_ref, recorded_at) VALUES (?,?,?)"
                " ON CONFLICT(scope_key) DO UPDATE SET tracker_ref = excluded.tracker_ref,"
                "   recorded_at = excluded.recorded_at", (key, team, now))
            db.execute(
                "INSERT INTO fault_target_projects (scope_key, product, project_ref, recorded_at)"
                " VALUES (?,?,?,?) ON CONFLICT(scope_key) DO UPDATE SET"
                "   product = excluded.product, project_ref = excluded.project_ref,"
                "   recorded_at = excluded.recorded_at", (key, product, project_ref, now))
            answer["changed"] = True
            answer["backfilled"], answer["backfillPending"] = self._repoint_where(
                db, now, scope_key=key)
            answer["relinked"], answer["relinkPending"] = self._relink_where(
                db, now, scope_key=key, limit=RELINK_PER_CALL)
        return answer

    def targets(self, product=None, *, limit=SHOWN_PER_PAGE, after=None) -> list:
        limit = _bounded(limit, "limit")
        rows = self.store.all(
            "SELECT t.scope_key, t.tracker_ref AS team, p.product, p.project_ref, t.recorded_at"
            "  FROM fault_targets t LEFT JOIN fault_target_projects p"
            "    ON p.scope_key = t.scope_key"
            " WHERE (? IS NULL OR p.product = ?) AND t.scope_key > ?"
            " ORDER BY t.scope_key LIMIT ?", (product, product, after or "", limit))
        return [dict(row) for row in rows]

    def target_for(self, scope_key):
        row = self.store.one(
            "SELECT t.scope_key, t.tracker_ref, p.product, p.project_ref FROM fault_targets t"
            "  LEFT JOIN fault_target_projects p ON p.scope_key = t.scope_key"
            " WHERE t.scope_key = ?", (scope_key,))
        return dict(row) if row else None

    def _repoint(self, db, identifier, now) -> int:
        """Point this fault's unsent target-bound writes at its product's current target."""
        return self._repoint_where(db, now, fault_id=identifier)[0]

    def _repoint_where(self, db, now, *, scope_key=None, fault_id=None, limit=RELINK_PER_CALL):
        """Point unsent target-bound writes at their product's current target: a bounded batch.

        Pending and failed only. An uncertain write may already have landed at its old target,
        and a claimed one is re-checked by operation() before it is issued. That re-check is also
        why a batch is enough: a write this has not reached yet is re-pointed when it is offered
        and never issued against a target its scope has left; relink() continues the rest.
        Returns (re-pointed, still to re-point). The current target is read the way
        _owned_target() reads it: owned by the fault's product, on a key no other product carries.

        Which writes are target-bound, and whether they carry a project, is read from the
        target_mode recorded when each was queued, never from the kinds this process happens to
        have registered: a process without an extension kind's module still re-points its
        writes. A write queued before target_mode was recorded is a built-in kind.
        """
        projected = [name for name, spec in KINDS.items() if spec["target"] == "team+project"]
        teamed = [name for name, spec in KINDS.items() if spec["target"] == "team"]

        def listed(values):
            return "(" + ",".join("?" * len(values)) + ")"

        mode = ("COALESCE(pp.target_mode, CASE WHEN p.kind IN " + listed(projected)
                + " THEN 'team+project' WHEN p.kind IN " + listed(teamed) + " THEN 'team' END)")
        moded = (*projected, *teamed)
        owned = ("(tp.product = f.product AND NOT EXISTS (SELECT 1 FROM fault_ledger o"
                 "  WHERE o.scope_key = f.scope_key AND o.product != f.product))")
        team = "(CASE WHEN " + owned + " THEN t.tracker_ref END)"
        project = ("(CASE WHEN " + mode + " = 'team+project' AND " + owned
                   + " THEN tp.project_ref END)")
        base = (
            " FROM fault_publications p JOIN fault_ledger f ON f.fault_id = p.fault_id"
            "  LEFT JOIN fault_targets t ON t.scope_key = f.scope_key"
            "  LEFT JOIN fault_target_projects tp ON tp.scope_key = f.scope_key"
            "  LEFT JOIN fault_publication_payloads pp ON pp.publication_id = p.publication_id"
            " WHERE p.state IN (?,?) AND " + mode + " IN ('team', 'team+project')"
            + (" AND f.scope_key = ?" if scope_key is not None else "")
            + (" AND f.fault_id = ?" if fault_id is not None else "")
            + " AND (p.tracker_ref IS NOT " + team + " OR pp.project_ref IS NOT " + project + ")")
        where = (PENDING, FAILED, *moded,
                 *(() if scope_key is None else (scope_key,)),
                 *(() if fault_id is None else (fault_id,)), *moded)
        rows = db.execute("SELECT p.publication_id, " + team + " AS team, " + project
                          + " AS project" + base + " ORDER BY p.rowid LIMIT ?",
                          (*moded, *where, limit)).fetchall()
        for row in rows:
            db.execute("UPDATE fault_publications SET tracker_ref = ?, updated_at = ?"
                       " WHERE publication_id = ?", (row["team"], now, row["publication_id"]))
            _payload_set(db, row["publication_id"], now, project_ref=row["project"])
        pending = db.execute("SELECT COUNT(*) AS n" + base, where).fetchone()["n"]
        return len(rows), pending

    def _rescope(self, db, row, scope, now) -> tuple:
        """Invariant 10: every scope change re-points unsent writes and relinks an owned issue.

        Returns (re-pointed, still to re-point): one bounded batch, which relink() continues."""
        key = scope_key_for(row["product"], scope)
        if key != row["scope_key"]:
            self._assign_scope_key(db, row["product"], key)
        stored = json.dumps(scope, ensure_ascii=False, sort_keys=True)
        if key == row["scope_key"] and stored == row["scope"]:
            # Unchanged, so nothing is written. What an earlier move left is still counted: a
            # caller retrying a move whose answer was lost must not read the rest as done.
            return 0, self._repoint_where(db, now, fault_id=row["fault_id"], limit=0)[1]
        db.execute("UPDATE fault_ledger SET scope = ?, scope_key = ?, updated_at = ?"
                   " WHERE fault_id = ?", (stored, key, now, row["fault_id"]))
        repointed = self._repoint_where(db, now, fault_id=row["fault_id"])
        if row["external_ref"]:
            self._link_to_target(db, row["fault_id"], now)
        return repointed

    def move(self, identifier, *, scope) -> dict:
        """Re-scope a fault without replaying an observation.

        Inside its workspace, or out of unassigned into a real one: then the id that workspace
        produces becomes an alias of this fault, after checking it names no other record.
        """
        scope = _read_scope(scope)
        now = self.clock.iso()
        with self.store.transaction() as db:
            row = self._fault(db, identifier)
            answer = self._move(db, row, scope, now)
        return answer

    def _move(self, db, row, scope, now) -> dict:
        current = _json(row["scope"]).get("workspace")
        wanted = scope.get("workspace")
        alias = None
        if wanted != current:
            if current != UNASSIGNED or wanted in (None, UNASSIGNED):
                raise FaultRefused(
                    RefusalReason.FAULT_SCOPE_CONFLICT,
                    f"a fault moves inside its workspace, or out of {UNASSIGNED}; this one is in"
                    f" {current!r} and was asked to move to {wanted!r}")
            signature = _json(row["signature"])
            alias, resolved = self._resolve(db, row["product"], row["fault_class"], signature,
                                            wanted)
            if resolved != row["fault_id"] and _exists(db, resolved):
                raise FaultRefused(
                    RefusalReason.FAULT_SCOPE_CONFLICT,
                    f"workspace {wanted!r} already records this failure as fault {resolved};"
                    f" two records never stand for one failure in one workspace")
            self._register_alias(db, alias, row["fault_id"], now)
        repointed, still = self._rescope(db, row, scope, now)
        fresh = db.execute("SELECT scope_key, scope FROM fault_ledger WHERE fault_id = ?",
                           (row["fault_id"],)).fetchone()
        return {"faultId": row["fault_id"], "scopeKey": fresh["scope_key"],
                "moved": fresh["scope"] != row["scope"], "repointed": repointed,
                "repointPending": still, "alias": alias}

    # ------------------------------------------------------------------ reading

    def get(self, identifier):
        with self.store.transaction() as db:
            row = db.execute("SELECT * FROM fault_ledger WHERE fault_id = ?",
                             (self._canonical(db, identifier),)).fetchone()
            return self._fault_view(db, row) if row else None

    def _fault_view(self, db, row) -> dict:
        record = dict(row)
        link = db.execute("SELECT * FROM fault_links WHERE fault_id = ?",
                          (row["fault_id"],)).fetchone()
        if not row["external_ref"]:
            record["linkState"], record["linkedProject"] = NO_LINK, None
        elif link is None:
            record["linkState"], record["linkedProject"] = UNLINKED, None
        else:
            record["linkState"] = link["state"]
            record["linkedProject"] = link["observed_project_ref"]
        return record

    def occurrences(self, identifier, *, limit=RENDERED_OCCURRENCES, newest=True) -> list:
        limit = _bounded(limit, "limit")
        order = "DESC" if newest else "ASC"
        identifier = self._canonical(self.store.db, identifier)
        rows = self.store.all(
            f"SELECT * FROM fault_occurrences WHERE fault_id = ? ORDER BY rowid {order}"
            " LIMIT ?", (identifier, limit),
        )
        return [_occurrence(row) for row in rows]

    def remediations(self, identifier, *, limit=SHOWN_PER_FAULT) -> list:
        """The newest remediations, oldest first. A fault reopened many times has many."""
        limit = _bounded(limit, "limit")
        identifier = self._canonical(self.store.db, identifier)
        rows = self.store.all(
            "SELECT * FROM fault_remediations WHERE fault_id = ? ORDER BY rowid DESC LIMIT ?",
            (identifier, limit))
        return [dict(row) for row in reversed(rows)]

    def snapshot(self, *, product=None, fault_class=None, scope_key=None, state=None,
                 limit=SHOWN_PER_PAGE, after=None) -> dict:
        """One page of faults, oldest first, with where the next page starts.

        Bounded at every level it reads, continued by rowid, and filterable by product and class
        so a product's own records (a project create, say) can be kept out of defect listings.
        """
        limit = _bounded(limit, "limit")
        clauses, params = [], []
        for column, value in (("product", product), ("fault_class", fault_class),
                              ("scope_key", scope_key), ("state", state)):
            if value is not None:
                clauses.append(f"{column} = ?")
                params.append(value)
        if after is not None:
            if isinstance(after, bool) or not isinstance(after, int) or after < 0:
                raise FaultRefused(RefusalReason.FAULT_OBSERVATION_MALFORMED,
                                   f"after is a non-negative integer, not {after!r}")
            clauses.append("rowid > ?")
            params.append(after)
        where = (" WHERE " + " AND ".join(clauses)) if clauses else ""
        with self.store.transaction() as db:
            fetched = db.execute(
                "SELECT rowid AS seq, * FROM fault_ledger" + where + " ORDER BY rowid LIMIT ?",
                (*params, limit + 1)).fetchall()
            truncated = len(fetched) > limit
            rows = []
            for row in fetched[:limit]:
                record = self._fault_view(db, row)
                record["seq"] = row["seq"]
                rows.append(record)
        for row in rows:
            row["occurrences"] = self.occurrences(row["fault_id"])
            publications = self.store.all(
                "SELECT publication_id, kind, trigger_key, state, attempts, external_ref,"
                "  last_error FROM fault_publications WHERE fault_id = ?"
                " ORDER BY rowid DESC LIMIT ?",
                (row["fault_id"], SHOWN_PER_FAULT + 1))
            row["publicationsTruncated"] = len(publications) > SHOWN_PER_FAULT
            row["publications"] = [dict(entry) for entry in
                                   reversed(publications[:SHOWN_PER_FAULT])]
            row["clears"] = self.store.one(
                "SELECT COUNT(*) AS n FROM fault_timeline WHERE fault_id = ? AND kind = ?",
                (row["fault_id"], CLEARED))["n"]
        return {
            "schema": LEDGER_SCHEMA, "scopeKey": scope_key, "faults": rows,
            "limit": limit,
            "next": rows[-1]["seq"] if truncated else None,
            "limits": "derived from this store only. A queued publication is not an issue"
                      " anybody has written, and a confirmed one is not an issue anybody read."
                      " Pass next as after to continue; nested lists keep the newest"
                      f" {SHOWN_PER_FAULT} per fault",
        }

    # ------------------------------------------------------------------ recording

    def record(self, observation_record, *, adopt=None) -> dict:
        """Record one observation, converge it on its fault, and queue at most one write.

        adopt={"externalRef": ..., "scope": {...}} adopts an existing issue in the same
        transaction as the fault's first record, before suppression can open it.
        """
        fact = read_observation(observation_record)
        adoption = _read_adoption(adopt) if adopt is not None else None
        now_iso = self.clock.iso()
        now = self.clock.now()
        with self.store.transaction() as db:
            alias, identifier = self._resolve(db, fact["product"], fact["faultClass"],
                                              fact["signatureObject"], fact["workspace"])
            if identifier != alias and not _exists(db, alias):
                # The legacy lookup found this workspace's fault under its pre-workspace id.
                self._register_alias(db, alias, identifier, now_iso)
            row = db.execute(
                "SELECT * FROM fault_ledger WHERE fault_id = ?", (identifier,)).fetchone()
            if row is None and fact["cleared"]:
                # Nothing was ever wrong here. A healthy reading is not a fault that has
                # recovered, and recording one would fill the ledger with withdrawn rows for
                # turns that never had a problem.
                return {"faultId": identifier, "recorded": False, "state": None,
                        "occurrenceCount": 0, "publication": None,
                        "reason": "a clearing observation for a fault that was never recorded"}
            if row is None:
                self._assign_scope_key(db, fact["product"], fact["scopeKey"])
                db.execute(
                    "INSERT INTO fault_ledger (fault_id, product, fault_class, component,"
                    "  severity, signature, scope, scope_key, state, cycle, occurrence_count,"
                    "  reopen_count, detail, suppression, first_seen_at, last_seen_at,"
                    "  updated_at)"
                    " VALUES (?,?,?,?,?,?,?,?,?,1,0,0,?,?,?,?,?)",
                    (identifier, fact["product"], fact["faultClass"], fact["component"],
                     fact["severity"], fact["signature"],
                     json.dumps(fact["scope"], ensure_ascii=False, sort_keys=True),
                     fact["scopeKey"], OBSERVED, fact["detail"], None, now_iso, now_iso,
                     now_iso),
                )
            elif (fact["workspace"] == _json(row["scope"]).get("workspace")
                  and fact["scopeKey"] != row["scope_key"]):
                # A scope moves only inside the fault's current workspace. A stale observation
                # still saying unassigned for a fault that has moved leaves it where it is.
                self._rescope(db, row, fact["scope"], now_iso)
            row = db.execute(
                "SELECT * FROM fault_ledger WHERE fault_id = ?", (identifier,)).fetchone()
            adopted = None
            if adoption is not None:
                adopted = self._adopt(db, row, adoption, now_iso)
                row = db.execute(
                    "SELECT * FROM fault_ledger WHERE fault_id = ?", (identifier,)).fetchone()
            if fact["cleared"] and row["state"] in (WITHDRAWN, RESOLVED):
                return {"faultId": identifier, "recorded": False, "state": row["state"],
                        "occurrenceCount": row["occurrence_count"],
                        "reason": "this fault is already closed", "publication": None}
            episode = row["episode"]
            if not fact["cleared"] and row["cleared_at"] is not None:
                # Invariant 6: the first active observation after a clear opens an episode,
                # whatever its key. Keyed on the duplicate alone, a key that was recorded
                # before the clear was dropped as familiar and resolve() then closed a fault
                # whose cause had come back.
                episode += 1
            fact["occurrenceId"] = occurrence_id(identifier, fact["occurrenceKey"], episode,
                                                 fact["cleared"])
            # Asked of the TIMELINE, which is never pruned, so removing evidence cannot make a
            # familiar occurrence look new.
            recorded = db.execute(
                "SELECT 1 FROM fault_timeline WHERE fault_id = ? AND ref_id = ?",
                (identifier, fact["occurrenceId"])).fetchone() is None
            # A clear stored under its own key: the evidence table's uniqueness is per key and
            # episode, and a clear under the key its active observation used would otherwise be
            # dropped from the evidence while the timeline kept it.
            stored_key = (fact["occurrenceKey"] + "#cleared" if fact["cleared"]
                          else fact["occurrenceKey"])
            db.execute(
                "INSERT OR IGNORE INTO fault_occurrences (occurrence_id, fault_id, episode,"
                "  occurrence_key, severity, cleared, detail, evidence, evidence_digest,"
                "  truncated, observed_at, recorded_at, recorded_ts)"
                " VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)",
                (fact["occurrenceId"], identifier, episode, stored_key, fact["severity"],
                 1 if fact["cleared"] else 0, fact["detail"],
                 json.dumps(fact["evidence"], ensure_ascii=False, sort_keys=True),
                 fact["evidenceDigest"], 1 if fact["truncated"] else 0, fact["observedAt"],
                 now_iso, now),
            )
            if not recorded:
                return {"faultId": identifier, "recorded": False, "state": row["state"],
                        "occurrenceCount": row["occurrence_count"],
                        "reason": "this occurrence was already recorded in this episode",
                        "publication": adopted}
            db.execute(
                "INSERT INTO fault_timeline (fault_id, cycle, kind, ref_id, detail,"
                "  recorded_at, recorded_ts) VALUES (?,?,?,?,?,?,?)",
                (identifier, row["cycle"], CLEARED if fact["cleared"] else OCCURRENCE,
                 fact["occurrenceId"], fact["detail"], now_iso, now),
            )
            # Active occurrences only: a clear is not something that went wrong.
            count = row["occurrence_count"] + (0 if fact["cleared"] else 1)
            severity = (fact["severity"]
                        if SEVERITY_RANK[fact["severity"]] > SEVERITY_RANK[row["severity"]]
                        else row["severity"])
            escalated = severity != row["severity"]
            policy = self._policy(db, row["product"], fact["faultClass"], severity)
            suppression = self._suppression(db, identifier, policy, now)
            opened = _issue_slot(db, row)[0] is not None
            state, cycle, reopened, trigger_key = _transition(
                row["state"], row["cycle"], cleared=fact["cleared"],
                publishable=suppression["publish"], escalated=escalated, severity=severity,
                landed=_landed(db, identifier), opened=opened,
            )
            db.execute(
                "UPDATE fault_ledger SET state = ?, cycle = ?, severity = ?, episode = ?,"
                "  occurrence_count = ?, reopen_count = reopen_count + ?, detail = ?,"
                "  suppression = ?, last_seen_at = ?, cleared_at = ?, resolved_at = ?,"
                "  updated_at = ? WHERE fault_id = ?",
                (state, cycle, severity, episode + (1 if state == WITHDRAWN else 0), count,
                 1 if reopened else 0, fact["detail"] or row["detail"],
                 json.dumps(suppression, ensure_ascii=False, sort_keys=True), now_iso,
                 now_iso if fact["cleared"] else None,
                 None if state != RESOLVED else row["resolved_at"], now_iso, identifier),
            )
            if state == WITHDRAWN:
                # Invariant 5: nothing landed, so nothing is owed. Its unissued writes go too.
                self._cancel_where(db, identifier,
                                   "the fault was withdrawn before anything landed", now_iso)
            publication = adopted
            if trigger_key is not None:
                publication = self._enqueue(db, identifier, trigger_key, now_iso,
                                            clears=policy["clears"])
                reason = trigger_key.split(":")[0]
                if reason == TRIGGER_REOPEN:
                    self._queue_update(db, identifier, "reopen", None, now_iso)
                if reason in (TRIGGER_OPEN, TRIGGER_REOPEN) and severity == BROKEN:
                    self._notify(db, identifier, BLOCKING, cycle, now_iso)
            fresh = db.execute(
                "SELECT * FROM fault_ledger WHERE fault_id = ?", (identifier,)).fetchone()
        return {"faultId": identifier, "recorded": True, "state": fresh["state"],
                "cycle": fresh["cycle"], "severity": fresh["severity"],
                "occurrenceCount": count, "suppression": suppression,
                "publication": publication}

    # ------------------------------------------------------------------ adoption

    def adopt(self, identifier, *, external_ref, scope) -> dict:
        """Adopt an existing issue for a recorded fault. See record(adopt=) for a first record."""
        adoption = _read_adoption({"externalRef": external_ref, "scope": scope})
        now = self.clock.iso()
        with self.store.transaction() as db:
            row = self._fault(db, identifier)
            publication = self._adopt(db, row, adoption, now)
            fresh = db.execute("SELECT * FROM fault_ledger WHERE fault_id = ?",
                               (row["fault_id"],)).fetchone()
            stored = db.execute("SELECT state FROM fault_adoptions WHERE fault_id = ?",
                                (row["fault_id"],)).fetchone()
            cancelled = [entry["publication_id"] for entry in db.execute(
                "SELECT publication_id FROM fault_publications WHERE fault_id = ? AND kind = ?"
                " AND state = ?", (row["fault_id"], OPEN_RECORD, CANCELLED))]
        return {"faultId": row["fault_id"], "externalRef": fresh["external_ref"] or external_ref,
                "state": stored["state"] if stored else None, "cancelled": cancelled,
                "publication": publication}

    def _adopt(self, db, row, adoption, now):
        """Invariant 1 applied to an existing issue: the fault owns it, or nothing changes."""
        ref, scope = adoption["externalRef"], adoption["scope"]
        holder, create = _issue_slot(db, row)
        if holder == SLOT_ISSUE:
            if row["external_ref"] != ref:
                raise FaultRefused(RefusalReason.FAULT_ADOPT_CONFLICT,
                                   f"this fault already owns {row['external_ref']!r}")
            return None
        stored = db.execute("SELECT * FROM fault_adoptions WHERE fault_id = ?",
                            (row["fault_id"],)).fetchone()
        if stored is not None and stored["external_ref"] != ref:
            raise FaultRefused(RefusalReason.FAULT_ADOPT_CONFLICT,
                               f"this fault already adopts {stored['external_ref']!r}")
        if create is not None and create["state"] in (ISSUED, UNCERTAIN, CONFIRMED):
            raise FaultRefused(
                RefusalReason.FAULT_ADOPT_CONFLICT,
                f"this fault's create is {create['state']}; reconcile it first, or two records"
                f" would stand for one fault")
        self._move(db, row, scope, now)
        if create is not None and create["state"] in (PENDING, FAILED, CLAIMED):
            self._cancel(db, create, f"adopted {ref}", now)
        db.execute(
            "INSERT INTO fault_adoptions (fault_id, external_ref, scope, state, created_at,"
            "  updated_at) VALUES (?,?,?,?,?,?) ON CONFLICT(fault_id) DO NOTHING",
            (row["fault_id"], ref, json.dumps(scope, ensure_ascii=False, sort_keys=True),
             PENDING, now, now))
        opened = row["state"] == OPEN or create is not None
        if not opened:
            return None
        return self._materialize(db, row["fault_id"], now)

    def _materialize(self, db, identifier, now):
        adoption = db.execute(
            "SELECT * FROM fault_adoptions WHERE fault_id = ? AND state = ?",
            (identifier, PENDING)).fetchone()
        if adoption is None:
            return None
        db.execute("UPDATE fault_ledger SET external_ref = ?, updated_at = ?"
                   " WHERE fault_id = ? AND external_ref IS NULL",
                   (adoption["external_ref"], now, identifier))
        db.execute("UPDATE fault_adoptions SET state = 'materialized', updated_at = ?"
                   " WHERE fault_id = ?", (now, identifier))
        fault = db.execute("SELECT * FROM fault_ledger WHERE fault_id = ?",
                           (identifier,)).fetchone()
        publication = self._insert_publication(db, fault, APPEND_COMMENT, TRIGGER_OPEN, now)
        # Whether the adopted issue sits in this scope's project is read back like any other
        # write, never assumed; with no project to be in it is unlinked, never linked.
        self._link_to_target(db, identifier, now)
        return publication

    # ------------------------------------------------------------------ policy (B12)

    def _policy(self, db, product, fault_class, severity) -> dict:
        policy = policy_for(fault_class, severity)
        override = db.execute(
            "SELECT threshold, window_seconds, reason, updated_at FROM fault_policies"
            " WHERE product = ? AND fault_class = ? AND severity = ?",
            (product, fault_class, severity)).fetchone()
        policy["source"] = "built-in"
        if override is not None:
            if override["threshold"] is not None:
                policy["threshold"] = override["threshold"]
                policy["publish"] = True
            if override["window_seconds"] is not None:
                policy["window"] = override["window_seconds"]
            policy["source"] = "override"
            policy["overrideReason"] = override["reason"]
        return policy

    def set_policy(self, product, fault_class, severity, *, threshold=None, window=None,
                   reason) -> dict:
        """Adjust how many degraded observations file a fault, and inside what window.

        Prospective: it decides the next occurrence recorded. A broken fault files at once and
        a notice never files; changing either is refused, because both are the canonical rule.
        """
        _check_product(product)
        policy_for(fault_class, severity)
        if severity != DEGRADED:
            raise FaultRefused(
                RefusalReason.FAULT_POLICY_FIXED,
                f"a {severity} fault's policy is fixed: a broken fault files at once and a notice"
                f" never files")
        if threshold is not None:
            threshold = _bounded(threshold, "threshold", MAX_POLICY_THRESHOLD)
        if window is not None:
            if isinstance(window, bool) or not isinstance(window, (int, float)) or not (
                    MIN_POLICY_WINDOW <= window <= MAX_POLICY_WINDOW):
                raise FaultRefused(RefusalReason.FAULT_OBSERVATION_MALFORMED,
                                   f"window is {MIN_POLICY_WINDOW:.0f}..{MAX_POLICY_WINDOW:.0f}s")
            window = float(window)
        if not _named(reason):
            raise FaultRefused(RefusalReason.FAULT_OBSERVATION_MALFORMED,
                               "a policy change says why")
        now = self.clock.iso()
        with self.store.transaction() as db:
            previous = db.execute(
                "SELECT threshold, window_seconds, reason FROM fault_policies WHERE product = ?"
                " AND fault_class = ? AND severity = ?",
                (product, fault_class, severity)).fetchone()
            db.execute(
                "INSERT INTO fault_policies (product, fault_class, severity, threshold,"
                "  window_seconds, reason, updated_at) VALUES (?,?,?,?,?,?,?)"
                " ON CONFLICT(product, fault_class, severity) DO UPDATE SET"
                "   threshold = excluded.threshold, window_seconds = excluded.window_seconds,"
                "   reason = excluded.reason, updated_at = excluded.updated_at",
                (product, fault_class, severity, threshold, window, reason, now))
            self.store.journal("fault_policy_set", f"{product}:{fault_class}:{severity}", {
                "threshold": threshold, "window": window, "reason": reason,
                "previous": dict(previous) if previous else None}, at=now)
        return {"product": product, "faultClass": fault_class, "severity": severity,
                "threshold": threshold, "window": window, "reason": reason,
                "previous": dict(previous) if previous else None}

    def policies(self, product) -> list:
        _check_product(product)
        with self.store.transaction() as db:
            return [
                {"product": product, "faultClass": name, "severity": severity,
                 **{key: value for key, value in self._policy(db, product, name, severity).items()
                    if key in ("threshold", "window", "publish", "source", "overrideReason",
                               "clears")}}
                for name in sorted(CLASS_POLICY) for severity in SEVERITIES]

    def _suppression(self, db, identifier, policy, now) -> dict:
        """Whether this fault has earned a Linear record, counted inside the window."""
        source = ("" if policy.get("source") != "override" else
                  f" (product override: {policy.get('overrideReason')})")
        if not policy["publish"] or policy["threshold"] is None:
            return {"publish": False, "threshold": None, "window": policy["window"],
                    "counted": None,
                    "reason": f"a {policy['severity']} is recorded for an operator and never"
                              f" filed{source}"}
        counted = db.execute(
            "SELECT COUNT(*) AS n FROM fault_timeline"
            " WHERE fault_id = ? AND kind = ? AND recorded_ts >= ?",
            (identifier, OCCURRENCE, now - policy["window"]),
        ).fetchone()["n"]
        publish = counted >= policy["threshold"]
        return {
            "publish": publish, "threshold": policy["threshold"], "window": policy["window"],
            "counted": counted,
            "reason": ((f"{counted} observation(s) inside {int(policy['window'])}s reached the"
                        f" threshold of {policy['threshold']}") if publish else
                       (f"{counted} observation(s) inside {int(policy['window'])}s is under the"
                        f" threshold of {policy['threshold']}")) + source,
        }

    # ------------------------------------------------------------------ remediation

    def record_fix(self, identifier, *, ref, detail="") -> dict:
        """Attach the change that is supposed to have fixed this. It resolves nothing."""
        if not _named(ref):
            raise FaultRefused(RefusalReason.FAULT_OBSERVATION_MALFORMED,
                               "a fix names the change it is - a pull request, a commit")
        return self._remediate(identifier, kind=FIX, ref=ref, detail=detail,
                               allowed=(OBSERVED, OPEN, FIX_PENDING), next_state=FIX_PENDING,
                               trigger=TRIGGER_FIX)

    def record_reverification(self, identifier, *, method, ref, outcome, detail="") -> dict:
        """State that the fault was looked for after the fix and what was found."""
        if method not in METHODS:
            raise FaultRefused(RefusalReason.FAULT_OBSERVATION_MALFORMED,
                               f"method {method!r} is not one of {METHODS}")
        if not _named(ref):
            raise FaultRefused(RefusalReason.FAULT_OBSERVATION_MALFORMED,
                               "a reverification names the command or reading it was")
        if outcome not in OUTCOMES:
            raise FaultRefused(
                RefusalReason.FAULT_OBSERVATION_MALFORMED,
                f"outcome {outcome!r} is not one of {OUTCOMES}. An open string let a check"
                f" that reported the fault still happening satisfy the resolution gate",
            )
        return self._remediate(identifier, kind=REVERIFICATION, ref=ref, method=method,
                               outcome=outcome, detail=detail, allowed=(FIX_PENDING,),
                               next_state=FIX_PENDING, trigger=None)

    def record_stage(self, identifier, *, stage, ref, detail="") -> dict:
        """Record acceptance, assignment, merge or installation as its own state.

        Recording one runs, merges and installs nothing: it is what somebody else reports having
        done. Accepted and assigned need an issue the fault owns; merged and installed need a fix
        in this cycle, because a merge of nothing is not a step toward resolution.
        """
        if stage not in STAGES:
            raise FaultRefused(RefusalReason.FAULT_OBSERVATION_MALFORMED,
                               f"stage {stage!r} is not one of {STAGES}")
        if not _named(ref):
            raise FaultRefused(RefusalReason.FAULT_OBSERVATION_MALFORMED,
                               "a stage names what it refers to")
        now = self.clock.iso()
        with self.store.transaction() as db:
            row = self._fault(db, identifier)
            if row["state"] in (RESOLVED, WITHDRAWN):
                raise FaultRefused(RefusalReason.FAULT_STATE_CONFLICT,
                                   f"this fault is {row['state']}")
            if stage in ("accepted", "assigned") and not row["external_ref"]:
                raise FaultRefused(RefusalReason.FAULT_STATE_CONFLICT,
                                   f"{stage} is recorded against an issue the fault owns, and"
                                   f" it owns none yet")
            if stage in ("merged", INSTALLED) and db.execute(
                    "SELECT 1 FROM fault_timeline WHERE fault_id = ? AND cycle = ? AND kind = ?",
                    (row["fault_id"], row["cycle"], FIX)).fetchone() is None:
                raise FaultRefused(RefusalReason.FAULT_STATE_CONFLICT,
                                   f"{stage} follows a fix, and none is recorded this cycle")
            remediation_id = sha256_hex(
                f"{row['fault_id']}|{row['cycle']}|{stage}|{ref}")[:ID_WIDTH]
            recorded = db.execute(
                "INSERT OR IGNORE INTO fault_remediations (remediation_id, fault_id, cycle,"
                "  kind, ref, method, outcome, detail, recorded_at)"
                " VALUES (?,?,?,?,?,NULL,NULL,?,?)",
                (remediation_id, row["fault_id"], row["cycle"], stage, ref, detail, now),
            ).rowcount == 1
            if recorded:
                db.execute(
                    "INSERT INTO fault_timeline (fault_id, cycle, kind, ref_id, detail,"
                    "  recorded_at, recorded_ts) VALUES (?,?,?,?,?,?,?)",
                    (row["fault_id"], row["cycle"], stage, remediation_id, ref, now,
                     self.clock.now()))
        return {"faultId": row["fault_id"], "stage": stage, "ref": ref, "recorded": recorded,
                "remediationId": remediation_id}

    def progress(self, identifier) -> dict:
        """The newest of each stage recorded this cycle."""
        with self.store.transaction() as db:
            row = self._fault(db, identifier)
            answer = {}
            for stage in STAGES:
                found = db.execute(
                    "SELECT r.ref, r.recorded_at FROM fault_timeline t"
                    "  JOIN fault_remediations r ON r.remediation_id = t.ref_id"
                    " WHERE t.fault_id = ? AND t.cycle = ? AND t.kind = ?"
                    " ORDER BY t.seq DESC LIMIT 1", (row["fault_id"], row["cycle"], stage)
                ).fetchone()
                if found is not None:
                    answer[stage] = {"ref": found["ref"], "recordedAt": found["recorded_at"]}
        return answer

    def _remediate(self, identifier, *, kind, ref, allowed, next_state, trigger, method=None,
                   outcome=None, detail="") -> dict:
        now = self.clock.iso()
        with self.store.transaction() as db:
            row = self._fault(db, identifier)
            identifier = row["fault_id"]
            if row["state"] not in allowed:
                raise FaultRefused(
                    RefusalReason.FAULT_STATE_CONFLICT,
                    f"a {kind} is recorded on a fault that is {allowed}, and this one is"
                    f" {row['state']}",
                )
            after = None
            if kind == REVERIFICATION:
                latest = db.execute(
                    "SELECT t.seq AS seq, r.kind AS kind, r.ref AS ref, r.method AS method,"
                    "       r.outcome AS outcome FROM fault_timeline t"
                    "  JOIN fault_remediations r ON r.remediation_id = t.ref_id"
                    " WHERE t.fault_id = ? AND t.cycle = ? AND t.kind IN (?,?)"
                    " ORDER BY t.seq DESC LIMIT 1",
                    (identifier, row["cycle"], FIX, REVERIFICATION)).fetchone()
                if latest is not None:
                    if (latest["kind"] == REVERIFICATION and latest["ref"] == ref
                            and latest["method"] == method and latest["outcome"] == outcome):
                        return {"faultId": identifier, "recorded": False,
                                "remediationId": latest_id(db, latest["seq"]),
                                "state": row["state"],
                                "reason": "this check was already the latest remediation"}
                    after = latest["seq"]
            remediation_id = sha256_hex(
                f"{identifier}|{row['cycle']}|{kind}|{ref}|{method}|{outcome}|{after}"
            )[:ID_WIDTH]
            recorded = db.execute(
                "INSERT OR IGNORE INTO fault_remediations (remediation_id, fault_id, cycle,"
                "  kind, ref, method, outcome, detail, recorded_at) VALUES (?,?,?,?,?,?,?,?,?)",
                (remediation_id, identifier, row["cycle"], kind, ref, method, outcome, detail,
                 now),
            ).rowcount == 1
            if not recorded:
                return {"faultId": identifier, "recorded": False, "remediationId":
                        remediation_id, "state": row["state"],
                        "reason": "this exact remediation was already recorded"}
            db.execute(
                "INSERT INTO fault_timeline (fault_id, cycle, kind, ref_id, detail,"
                "  recorded_at, recorded_ts) VALUES (?,?,?,?,?,?,?)",
                (identifier, row["cycle"], kind, remediation_id, ref, now, self.clock.now()),
            )
            db.execute("UPDATE fault_ledger SET state = ?, updated_at = ? WHERE fault_id = ?",
                       (next_state, now, identifier))
            publication = None
            if trigger is not None:
                remediation = {"kind": kind, "ref": ref, "method": method, "outcome": outcome,
                               "detail": detail}
                publication = self._enqueue(
                    db, identifier, f"{trigger}:{remediation_id[:12]}", now,
                    remediation=remediation,
                )
        return {"faultId": identifier, "recorded": True, "remediationId": remediation_id,
                "state": next_state, "publication": publication}

    def resolve(self, identifier) -> dict:
        """Close the loop, or refuse and say which half is missing."""
        now = self.clock.iso()
        with self.store.transaction() as db:
            row = self._fault(db, identifier)
            identifier = row["fault_id"]
            if row["state"] == RESOLVED:
                return {"faultId": identifier, "state": RESOLVED, "resolved": False,
                        "reason": "already resolved"}
            if row["state"] != FIX_PENDING:
                raise FaultRefused(
                    RefusalReason.FAULT_STATE_CONFLICT,
                    f"a fault is resolved from {FIX_PENDING} and this one is {row['state']}",
                )
            cycle = row["cycle"]
            fix = db.execute(
                "SELECT MAX(seq) AS seq FROM fault_timeline"
                " WHERE fault_id = ? AND cycle = ? AND kind = ?",
                (identifier, cycle, FIX)).fetchone()["seq"]
            if fix is None:
                raise FaultRefused(
                    RefusalReason.FAULT_UNVERIFIED,
                    "no fix is recorded for this cycle, so there is nothing to have verified",
                )
            verification = db.execute(
                "SELECT MAX(seq) AS seq FROM fault_timeline"
                " WHERE fault_id = ? AND cycle = ? AND kind = ? AND seq > ?",
                (identifier, cycle, REVERIFICATION, fix)).fetchone()["seq"]
            installed = db.execute(
                "SELECT MAX(seq) AS seq FROM fault_timeline"
                " WHERE fault_id = ? AND cycle = ? AND kind = ? AND seq > ?",
                (identifier, cycle, INSTALLED, fix)).fetchone()["seq"]
            if installed is not None and (verification is None or verification < installed):
                raise FaultRefused(
                    RefusalReason.FAULT_VERIFICATION_STALE,
                    "the fix was installed after the newest reverification; a check of the"
                    " code before its installation proves nothing about what is running",
                )
            if verification is not None:
                found = db.execute(
                    "SELECT r.outcome AS outcome FROM fault_timeline t"
                    "  JOIN fault_remediations r ON r.remediation_id = t.ref_id"
                    " WHERE t.seq = ?", (verification,)).fetchone()
                if found is None or found["outcome"] not in RESOLVING_OUTCOMES:
                    raise FaultRefused(
                        RefusalReason.FAULT_UNVERIFIED,
                        f"the newest reverification reported"
                        f" {(found or {})['outcome']!r}, which is not one of"
                        f" {RESOLVING_OUTCOMES}",
                    )
            if verification is None:
                earlier = db.execute(
                    "SELECT MAX(seq) AS seq FROM fault_timeline"
                    " WHERE fault_id = ? AND cycle = ? AND kind = ?",
                    (identifier, cycle, REVERIFICATION)).fetchone()["seq"]
                if earlier is not None:
                    raise FaultRefused(
                        RefusalReason.FAULT_VERIFICATION_STALE,
                        "the newest reverification was recorded BEFORE the newest fix. A check"
                        " that ran before the change landed proves nothing about the change",
                    )
                raise FaultRefused(
                    RefusalReason.FAULT_UNVERIFIED,
                    "a fix alone does not resolve a fault. Record a reverification that looked"
                    " for it after the fix",
                )
            recurrence = db.execute(
                "SELECT MIN(seq) AS seq FROM fault_timeline"
                " WHERE fault_id = ? AND kind = ? AND seq > ?",
                (identifier, OCCURRENCE, verification)).fetchone()["seq"]
            if recurrence is not None:
                raise FaultRefused(
                    RefusalReason.FAULT_RECURRED_AFTER_VERIFICATION,
                    "the fault was observed again after that reverification, so it is not"
                    " fixed whatever the check reported",
                )
            db.execute(
                "UPDATE fault_ledger SET state = ?, resolved_at = ?, updated_at = ?,"
                "  episode = episode + 1 WHERE fault_id = ?",
                (RESOLVED, now, now, identifier))
            publication = self._enqueue(db, identifier, f"{TRIGGER_RESOLVE}:{cycle}", now)
            self._notify(db, identifier, RESOLVED_NOTICE, cycle, now)
        return {"faultId": identifier, "state": RESOLVED, "resolved": True, "cycle": cycle,
                "publication": publication}

    def prune(self, identifier, *, keep) -> dict:
        """Drop the middle of a fault's occurrence history, as an explicit operator act."""
        if isinstance(keep, bool) or not isinstance(keep, int) or keep < 1:
            raise FaultRefused(RefusalReason.FAULT_OBSERVATION_MALFORMED, "keep at least one")
        now = self.clock.iso()
        with self.store.transaction() as db:
            identifier = self._fault(db, identifier)["fault_id"]
            removed = db.execute(
                "DELETE FROM fault_occurrences WHERE fault_id = ? AND rowid NOT IN"
                "  (SELECT rowid FROM fault_occurrences WHERE fault_id = ?"
                "    ORDER BY rowid DESC LIMIT ?)",
                (identifier, identifier, keep)).rowcount
            self.store.journal("fault_prune", identifier,
                               {"kept": keep, "removed": removed}, at=now)
        return {"faultId": identifier, "kept": keep, "removed": removed,
                "limits": "the ledger's occurrence_count still counts what was observed;"
                          " these rows are the evidence, not the count"}

    # ------------------------------------------------------------------ queuing

    def _enqueue(self, db, identifier, trigger_key, now, *, remediation=None, clears="") -> dict:
        """The ledger's own writes: the opening one, and comments after it.

        Invariant 1 decides the opening one. A pending adoption materializes as a comment on
        the adopted issue; a fault that owns an issue comments on it; otherwise the one
        open_record row this fault may ever have is queued, or revived if a clear cancelled it.
        """
        row = db.execute("SELECT * FROM fault_ledger WHERE fault_id = ?",
                         (identifier,)).fetchone()
        reason = trigger_key.split(":")[0]
        holder, create = _issue_slot(db, row)
        if reason == TRIGGER_OPEN:
            adopted = self._materialize(db, identifier, now)
            if adopted is not None:
                return adopted
            if holder == SLOT_ISSUE:
                return self._insert_publication(db, row, APPEND_COMMENT, trigger_key, now,
                                                remediation=remediation, clears=clears)
            if holder == SLOT_CREATE:
                return {"publicationId": create["publication_id"], "kind": OPEN_RECORD,
                        "trigger": trigger_key, "queued": False, "awaitingTarget": False,
                        "awaitingRecord": False, "reason": "the create is already queued"}
            return self._insert_publication(db, row, OPEN_RECORD, TRIGGER_OPEN, now,
                                            remediation=remediation, clears=clears)
        if holder is None:
            return {"publicationId": None, "kind": None, "trigger": trigger_key,
                    "queued": False, "awaitingTarget": False, "awaitingRecord": False,
                    "reason": "this fault has never been published, so nothing is written for"
                              " it; suppression decides that, not a remediation"}
        return self._insert_publication(db, row, APPEND_COMMENT, trigger_key, now,
                                        remediation=remediation, clears=clears)

    def _insert_publication(self, db, fault, kind, trigger_key, now, *, payload=None,
                            remediation=None, clears="") -> dict:
        spec = KINDS[kind]
        identifier = fault["fault_id"]
        publication = publication_id(identifier, kind, trigger_key)
        target, why = (self._owned_target(db, fault["product"], fault["scope_key"])
                       if spec["target"] else (None, None))
        team = target["team"] if target else None
        project = target["projectRef"] if (target and spec["target"] == "team+project") else None
        occurrences = [_occurrence(entry) for entry in db.execute(
            "SELECT * FROM fault_occurrences WHERE fault_id = ? ORDER BY rowid DESC LIMIT ?",
            (identifier, RENDERED_OCCURRENCES)).fetchall()]
        summary = render_summary(fault, trigger_key=trigger_key, occurrences=occurrences,
                                 remediation=remediation, clears=clears,
                                 publication=publication)
        existing = db.execute("SELECT state FROM fault_publications WHERE publication_id = ?",
                              (publication,)).fetchone()
        digest = identity_digest(identifier, kind, trigger_key, fault["cycle"])
        if existing is None:
            db.execute(
                "INSERT INTO fault_publications (publication_id, fault_id, kind, trigger_key,"
                "  cycle, tracker_ref, external_ref, summary, identity_digest, state, attempts,"
                "  created_at, updated_at) VALUES (?,?,?,?,?,?,?,?,?,?,0,?,?)",
                (publication, identifier, kind, trigger_key, fault["cycle"], team,
                 fault["external_ref"], summary, digest, PENDING, now, now))
            queued, detail = True, "queued"
        elif existing["state"] == CANCELLED:
            # Revived under the same id: a write that was cancelled before anything reached the
            # connector is offered again, and the fault still has one row for it, ever.
            db.execute(
                "UPDATE fault_publications SET state = ?, cycle = ?, tracker_ref = ?,"
                "  external_ref = ?, summary = ?, identity_digest = ?, attempts = 0,"
                "  next_attempt_at = NULL, claim_token = NULL, lease_owner = NULL,"
                "  lease_until = NULL, issued_at = NULL, last_error = NULL, updated_at = ?"
                " WHERE publication_id = ?",
                (PENDING, fault["cycle"], team, fault["external_ref"], summary, digest, now,
                 publication))
            queued, detail = True, "revived"
        else:
            queued, detail = False, "this reason was already queued"
        if queued:
            _payload_set(db, publication, now, project_ref=project,
                         payload=payload if payload is not None else _UNCHANGED,
                         target_mode=spec["target"] or "none")
        return {
            "publicationId": publication, "kind": kind, "trigger": trigger_key,
            "queued": queued,
            "awaitingTarget": bool(spec["target"]) and target is None,
            "awaitingRecord": spec["requires_issue"] and not fault["external_ref"],
            "reason": detail + ("" if not (spec["target"] and target is None) else
                                f"; no target owned by {fault['product']} for"
                                f" {fault['scope_key']} ({why}), so it waits rather than being"
                                f" filed somewhere guessed"),
        }

    def queue(self, identifier, *, kind, trigger, payload=None) -> dict:
        """Queue any registered kind, on any recorded fault. The issue create is not among them.

        An explicit caller act, so the rule that a remediation on a never-opened fault queues
        nothing does not apply. Only suppression opens a fault's issue (invariant 1).
        """
        spec = _kind(kind)
        if kind == OPEN_RECORD:
            raise FaultRefused(RefusalReason.FAULT_STATE_CONFLICT,
                               "only suppression opens a fault's issue; queue() never creates one")
        if not _named(trigger) or "|" in trigger:
            raise FaultRefused(RefusalReason.FAULT_OBSERVATION_MALFORMED,
                               "a trigger is a non-blank string without '|'")
        problems = spec["validate"](payload) if spec["validate"] else []
        if problems:
            raise FaultRefused(RefusalReason.FAULT_OBSERVATION_MALFORMED, "; ".join(problems))
        now = self.clock.iso()
        with self.store.transaction() as db:
            fault = self._fault(db, identifier)
            trigger_key = "create" if spec["creates"] else trigger
            if spec["creates"]:
                live = db.execute(
                    "SELECT publication_id FROM fault_publications WHERE fault_id = ?"
                    " AND kind = ? AND state != ?", (fault["fault_id"], kind, CANCELLED)).fetchone()
                if live is not None:
                    return {"publicationId": live["publication_id"], "kind": kind,
                            "trigger": trigger_key, "queued": False,
                            "reason": f"one {kind} per fault; this one is already queued"}
            return self._insert_publication(db, fault, kind, trigger_key, now, payload=payload)

    def request_update(self, identifier, *, op, value) -> dict:
        """One idempotent update on the issue this fault owns."""
        problems = _validate_update({"op": op, "value": value})
        if problems:
            raise FaultRefused(RefusalReason.FAULT_OBSERVATION_MALFORMED, "; ".join(problems))
        now = self.clock.iso()
        with self.store.transaction() as db:
            fault = self._fault(db, identifier)
            if op == "set_project":
                return self._relink(db, fault["fault_id"], value, now, force=True)
            return self._queue_update(db, fault["fault_id"], op, value, now)

    def _queue_update(self, db, identifier, op, value, now, *, trigger_key=None):
        fault = db.execute("SELECT * FROM fault_ledger WHERE fault_id = ?",
                           (identifier,)).fetchone()
        if not fault["external_ref"]:
            return None
        if trigger_key is None:
            trigger_key = f"update:{op}:{evidence_digest(value)[:12]}:{fault['cycle']}"
        return self._insert_publication(db, fault, UPDATE_RECORD, trigger_key, now,
                                        payload={"op": op, "value": value})

    def _moving_elsewhere(self, db, identifier, project_ref):
        """Is an issued or uncertain set_project to ANOTHER project outstanding for this fault?

        Such a write may still land, so no earlier readback proves where the issue is.
        """
        return db.execute(
            "SELECT 1 FROM fault_publications p"
            "  LEFT JOIN fault_publication_payloads pp ON pp.publication_id = p.publication_id"
            " WHERE p.fault_id = ? AND p.kind = ? AND p.state IN (?,?)"
            "   AND p.trigger_key LIKE 'update:set_project:%'"
            "   AND (CASE WHEN json_valid(pp.payload)"
            "        THEN json_extract(pp.payload, '$.value') END) IS NOT ?"
            " LIMIT 1",
            (identifier, UPDATE_RECORD, ISSUED, UNCERTAIN, project_ref)).fetchone() is not None

    def _cancel_stale_relinks(self, db, identifier, reason, now):
        """Cancel every unissued set_project of this fault; issued and uncertain ones stay."""
        self._cancel_where(db, identifier, reason, now,
                           extra=" AND p.kind = ? AND p.trigger_key LIKE 'update:set_project:%'",
                           params=(UPDATE_RECORD,))

    def _relink(self, db, identifier, project_ref, now, *, force=False):
        """Invariant 11: queue the write that puts the owned issue in project_ref.

        Each relink increments the link's revision, which is part of the write's identity, and
        cancels any earlier set_project that has not been issued. One already read back in that
        project needs nothing - unless a write to another project is issued or uncertain: that
        write may still land, so the old readback proves nothing. The issue then stays unlinked
        and no second write is queued; the outstanding write's own readback decides, and a
        repair is queued on the same issue from there.
        """
        fault = db.execute("SELECT * FROM fault_ledger WHERE fault_id = ?",
                           (identifier,)).fetchone()
        if not fault["external_ref"] or not project_ref:
            return None
        link = db.execute("SELECT * FROM fault_links WHERE fault_id = ?",
                          (identifier,)).fetchone()
        if link is None:
            db.execute(
                "INSERT INTO fault_links (fault_id, external_ref, project_ref,"
                "  observed_project_ref, state, revision, updated_at) VALUES (?,?,?,?,?,0,?)",
                (identifier, fault["external_ref"], project_ref, None, UNLINKED, now))
            link = db.execute("SELECT * FROM fault_links WHERE fault_id = ?",
                              (identifier,)).fetchone()
        if link["observed_project_ref"] == project_ref and not force:
            moving = self._moving_elsewhere(db, identifier, project_ref)
            self._cancel_stale_relinks(db, identifier, "superseded by a later target", now)
            db.execute("UPDATE fault_links SET project_ref = ?, state = ?, updated_at = ?"
                       " WHERE fault_id = ?",
                       (project_ref, UNLINKED if moving else LINKED, now, identifier))
            return None
        if link["project_ref"] == project_ref and link["state"] == UNLINKED and not force:
            live = db.execute(
                "SELECT p.publication_id FROM fault_publications p"
                "  JOIN fault_publication_payloads pp ON pp.publication_id = p.publication_id"
                " WHERE p.fault_id = ? AND p.kind = ? AND p.state NOT IN (?,?)"
                "   AND p.trigger_key = ?",
                (identifier, UPDATE_RECORD, CANCELLED, CONFIRMED,
                 f"update:set_project:{project_ref}:r{link['revision']}")).fetchone()
            if live is not None:
                return None
        self._cancel_stale_relinks(db, identifier, "superseded by a later target", now)
        revision = link["revision"] + 1
        db.execute(
            "UPDATE fault_links SET project_ref = ?, state = ?, revision = ?, updated_at = ?"
            " WHERE fault_id = ?", (project_ref, UNLINKED, revision, now, identifier))
        return self._queue_update(db, identifier, "set_project", project_ref, now,
                                  trigger_key=f"update:set_project:{project_ref}:r{revision}")

    def _unlink(self, db, identifier, now):
        """Invariant 11 with no project to be in: the owned issue is not in its project.

        A scope whose target names no project its product owns - removed, never set, or owned by
        another product - leaves the owned issue with nowhere it belongs. Reported unlinked
        (awaiting a target) rather than keeping whatever link it had, and any unsent relink to a
        project the scope has left is cancelled. Setting a project again relinks the same issue.
        """
        fault = db.execute("SELECT * FROM fault_ledger WHERE fault_id = ?",
                           (identifier,)).fetchone()
        if not fault["external_ref"]:
            return None
        link = db.execute("SELECT * FROM fault_links WHERE fault_id = ?",
                          (identifier,)).fetchone()
        if link is None:
            db.execute(
                "INSERT INTO fault_links (fault_id, external_ref, project_ref,"
                "  observed_project_ref, state, revision, updated_at) VALUES (?,?,?,?,?,0,?)",
                (identifier, fault["external_ref"], None, None, UNLINKED, now))
        else:
            db.execute("UPDATE fault_links SET project_ref = NULL, state = ?, updated_at = ?"
                       " WHERE fault_id = ?", (UNLINKED, now, identifier))
        self._cancel_stale_relinks(db, identifier, "the scope no longer targets a project", now)
        return None

    def _link_to_target(self, db, identifier, now):
        """Relink the owned issue to its product's current project, or unlink it if none."""
        fault = db.execute("SELECT * FROM fault_ledger WHERE fault_id = ?",
                           (identifier,)).fetchone()
        if not fault["external_ref"]:
            return None
        target, _ = self._owned_target(db, fault["product"], fault["scope_key"])
        if target and target["projectRef"]:
            return self._relink(db, identifier, target["projectRef"], now)
        return self._unlink(db, identifier, now)

    def _relink_where(self, db, now, *, scope_key=None, limit=RELINK_PER_CALL):
        """Relink owned issues whose link is not their product's current target project, and
        unlink those whose scope no longer targets any project their product owns."""
        clause = " AND f.scope_key = ?" if scope_key is not None else ""
        params = (UNLINKED, UNLINKED, scope_key) if scope_key is not None else (UNLINKED,
                                                                                  UNLINKED)
        query = (
            "SELECT f.fault_id, p.project_ref FROM fault_ledger f"
            "  LEFT JOIN fault_target_projects p"
            "    ON p.scope_key = f.scope_key AND p.product = f.product"
            "  LEFT JOIN fault_links l ON l.fault_id = f.fault_id"
            " WHERE f.external_ref IS NOT NULL"
            "   AND ((p.project_ref IS NOT NULL"
            "         AND (l.fault_id IS NULL OR l.project_ref IS NOT p.project_ref"
            # Unlinked while a conflicting write was outstanding, with a readback that already
            # matches: asked again, so it is linked once that write has been settled.
            "              OR (l.state = ? AND l.observed_project_ref IS p.project_ref)))"
            "        OR (p.project_ref IS NULL"
            "            AND (l.fault_id IS NULL OR l.project_ref IS NOT NULL OR l.state != ?)))"
            + clause)
        rows = db.execute(query + " ORDER BY f.rowid LIMIT ?", (*params, limit + 1)).fetchall()
        done = 0
        for row in rows[:limit]:
            if row["project_ref"]:
                self._relink(db, row["fault_id"], row["project_ref"], now)
            else:
                self._unlink(db, row["fault_id"], now)
            done += 1
        pending = db.execute("SELECT COUNT(*) AS n FROM (" + query + ")", params).fetchone()["n"]
        return done, pending

    def relink(self, *, limit=RELINK_PER_CALL) -> dict:
        """Continue what set_target() left over: at most limit owned issues per call."""
        limit = _bounded(limit, "limit")
        now = self.clock.iso()
        with self.store.transaction() as db:
            backfilled, backfill_pending = self._repoint_where(db, now, limit=limit)
            done, pending = self._relink_where(db, now, limit=limit)
        return {"relinked": done, "relinkPending": pending, "backfilled": backfilled,
                "backfillPending": backfill_pending}

    # ------------------------------------------------------------------ reads

    def publication(self, publication) -> dict:
        with self.store.transaction() as db:
            return self._publication_view(db, self._publication_row(db, publication))

    def publications(self, identifier, *, kind=None, state=None, limit=SHOWN_PER_FAULT,
                     after=None) -> list:
        limit = _bounded(limit, "limit")
        with self.store.transaction() as db:
            identifier = self._canonical(db, identifier)
            rows = db.execute(
                "SELECT * FROM fault_publications WHERE fault_id = ?"
                " AND (? IS NULL OR kind = ?) AND (? IS NULL OR state = ?) AND rowid > ?"
                " ORDER BY rowid LIMIT ?",
                (identifier, kind, kind, state, state, after or 0, limit)).fetchall()
            return [self._publication_view(db, row) for row in rows]

    def _publication_view(self, db, row) -> dict:
        record = _publication(row)
        extra = db.execute("SELECT * FROM fault_publication_payloads WHERE publication_id = ?",
                           (row["publication_id"],)).fetchone()
        record["payload"] = (json.loads(extra["payload"]) if extra and extra["payload"]
                             else None)
        record["target"] = {"team": row["tracker_ref"],
                            "projectRef": extra["project_ref"] if extra else None}
        record["holdReason"] = extra["hold_reason"] if extra else None
        record["history"] = self._attempts(db, row["publication_id"], 3)
        return record

    def attempts(self, publication, *, limit=SHOWN_PER_FAULT) -> list:
        limit = _bounded(limit, "limit")
        with self.store.transaction() as db:
            return self._attempts(db, publication, limit)

    def _attempts(self, db, publication, limit):
        rows = db.execute(
            "SELECT attempt, owner, takeover, claimed_at, issued_at, outcome, error, ended,"
            "  ended_at FROM fault_publication_attempts WHERE publication_id = ?"
            " ORDER BY attempt_id DESC LIMIT ?", (publication, limit)).fetchall()
        return [dict(entry) for entry in reversed(rows)]

    # ------------------------------------------------------------------ budgets (B7)

    def _budget(self, db, product, kind, moment) -> dict:
        row = db.execute("SELECT max_count, window_seconds FROM fault_limits"
                         " WHERE product = ? AND kind = ?", (product, kind)).fetchone()
        if row is not None:
            limit, window, source = row["max_count"], row["window_seconds"], "override"
        else:
            limit, window = DEFAULT_LIMITS.get(kind, DEFAULT_KIND_LIMIT)
            source = "default"
        used = db.execute(
            "SELECT COUNT(*) AS n FROM fault_budget_uses WHERE product = ? AND kind = ?"
            " AND used_ts > ?", (product, kind, moment - window)).fetchone()["n"]
        return {"product": product, "kind": kind, "limit": limit, "window": window,
                "used": used, "remaining": max(0, limit - used), "source": source}

    def _consume(self, db, product, kind, ref, moment, stamp) -> dict:
        already = db.execute("SELECT 1 FROM fault_budget_uses WHERE product = ? AND kind = ?"
                             " AND ref = ?", (product, kind, ref)).fetchone()
        budget = self._budget(db, product, kind, moment)
        if already is not None:
            return {"consumed": True, "remaining": budget["remaining"],
                    "reason": "this ref was already consumed"}
        if budget["remaining"] <= 0:
            return {"consumed": False, "remaining": 0, "reason": "budget_spent"}
        db.execute("INSERT INTO fault_budget_uses (product, kind, ref, used_at, used_ts)"
                   " VALUES (?,?,?,?,?)", (product, kind, ref, stamp, moment))
        return {"consumed": True, "remaining": budget["remaining"] - 1, "reason": "consumed"}

    def budget(self, product, kind, *, now=None) -> dict:
        _check_product(product)
        moment = self.clock.now() if now is None else now
        with self.store.transaction() as db:
            return self._budget(db, product, kind, moment)

    def consume(self, product, kind, *, ref, now=None) -> dict:
        """Check and consume one unit of a product's budget. One ref is consumed once."""
        _check_product(product)
        if not _named(kind) or not _named(ref):
            raise FaultRefused(RefusalReason.FAULT_OBSERVATION_MALFORMED,
                               "a budget use names its kind and ref")
        moment = self.clock.now() if now is None else now
        with self.store.transaction() as db:
            return self._consume(db, product, kind, ref, moment, self.clock.iso())

    def set_limit(self, product, kind, *, max_count, window) -> dict:
        _check_product(product)
        if not _named(kind):
            raise FaultRefused(RefusalReason.FAULT_OBSERVATION_MALFORMED, "kind is a name")
        max_count = _bounded(max_count, "max_count", MAX_BUDGET)
        if isinstance(window, bool) or not isinstance(window, (int, float)) or not (
                MIN_POLICY_WINDOW <= window <= MAX_POLICY_WINDOW):
            raise FaultRefused(RefusalReason.FAULT_OBSERVATION_MALFORMED,
                               f"window is {MIN_POLICY_WINDOW:.0f}..{MAX_POLICY_WINDOW:.0f}s")
        now = self.clock.iso()
        with self.store.transaction() as db:
            db.execute(
                "INSERT INTO fault_limits (product, kind, max_count, window_seconds, updated_at)"
                " VALUES (?,?,?,?,?) ON CONFLICT(product, kind) DO UPDATE SET"
                "   max_count = excluded.max_count, window_seconds = excluded.window_seconds,"
                "   updated_at = excluded.updated_at",
                (product, kind, max_count, float(window), now))
            self.store.journal("fault_limit_set", f"{product}:{kind}",
                               {"maxCount": max_count, "window": window}, at=now)
        return {"product": product, "kind": kind, "maxCount": max_count, "window": float(window)}

    def limits(self, product) -> list:
        """Every kind's budget for this product: the kinds this process registered, and any
        kind a limit was stored for, whether or not this process loaded its module."""
        _check_product(product)
        moment = self.clock.now()
        with self.store.transaction() as db:
            stored = {row["kind"] for row in db.execute(
                "SELECT kind FROM fault_limits WHERE product = ?", (product,))}
            kinds = sorted(set(KINDS) | {NOTIFICATION} | stored)
            return [self._budget(db, product, kind, moment) for kind in kinds]

    # ------------------------------------------------------------------ selection (7a)

    def _selectable(self):
        creates = [name for name, spec in KINDS.items()]
        issue = [name for name, spec in KINDS.items() if spec["requires_issue"]]
        targeted = [name for name, spec in KINDS.items() if spec["target"]]
        projected = [name for name, spec in KINDS.items() if spec["target"] == "team+project"]
        return creates, issue, targeted, projected

    @staticmethod
    def _open_budget(product_sql, kind_sql) -> str:
        """SQL: this product and kind still have budget at the moment bound as its one parameter.

        Decided per candidate row inside the selection, so no bounded list of spent pairs can
        miss one: a pair past such a list was offered as ready and then refused at claim,
        forever, while every eligible write behind it starved.
        """
        counts = " ".join(f"WHEN '{kind}' THEN {limit}"
                          for kind, (limit, _window) in DEFAULT_LIMITS.items())
        windows = " ".join(f"WHEN '{kind}' THEN {window}"
                           for kind, (_limit, window) in DEFAULT_LIMITS.items())
        default_count, default_window = DEFAULT_KIND_LIMIT
        lookup = ("(SELECT {col} FROM fault_limits l WHERE l.product = " + product_sql
                  + " AND l.kind = " + kind_sql + ")")
        return (
            "(SELECT COUNT(*) FROM fault_budget_uses u WHERE u.product = " + product_sql
            + " AND u.kind = " + kind_sql + " AND u.used_ts > ? - COALESCE("
            + lookup.format(col="window_seconds") + ", CASE " + kind_sql + " " + windows
            + f" ELSE {default_window} END)) < COALESCE("
            + lookup.format(col="max_count") + ", CASE " + kind_sql + " " + counts
            + f" ELSE {default_count} END)")

    def _spent(self, db, moment) -> dict:
        pairs = db.execute(
            "SELECT DISTINCT f.product, p.kind FROM fault_publications p"
            "  JOIN fault_ledger f ON f.fault_id = p.fault_id WHERE p.state = ? LIMIT 500",
            (PENDING,)).fetchall()
        return {(row["product"], row["kind"]): self._budget(db, row["product"], row["kind"],
                                                           moment)["remaining"]
                for row in pairs}

    def next(self, *, limit=4, now=None) -> list:
        """The writes a caller may act on now, fairly across products.

        A spent product and kind pair is excluded INSIDE the query, and the rest are taken
        round-robin by product, so one capped product's backlog never hides another's work. An
        issued or uncertain row is never among them, nor one of a kind this process has not
        registered.
        """
        moment = self.clock.now() if now is None else now
        limit = _bounded(limit, "limit")
        with self.store.transaction() as db:
            return [self._publication_view(db, row) for row in self._ready(db, moment, limit)]

    def _ready(self, db, moment, limit):
        remaining = {}
        kinds, issue, targeted, projected = self._selectable()

        def listed(values):
            return "(" + ",".join("?" * len(values)) + ")"

        query = (
            "SELECT * FROM (SELECT p.*, f.product AS fault_product, p.rowid AS seq,"
            "   ROW_NUMBER() OVER (PARTITION BY f.product ORDER BY p.rowid) AS turn"
            "  FROM fault_publications p JOIN fault_ledger f ON f.fault_id = p.fault_id"
            "  LEFT JOIN fault_targets t ON t.scope_key = f.scope_key"
            "  LEFT JOIN fault_target_projects tp"
            "    ON tp.scope_key = f.scope_key AND tp.product = f.product"
            " WHERE p.state = ? AND (p.next_attempt_at IS NULL OR p.next_attempt_at <= ?)"
            "   AND p.kind IN " + listed(kinds) +
            "   AND " + self._open_budget("f.product", "p.kind") +
            "   AND (p.kind NOT IN " + listed(issue) + " OR f.external_ref IS NOT NULL)"
            "   AND (p.kind != ? OR f.external_ref IS NULL)"
            "   AND (p.kind NOT IN " + listed(targeted) +
            "        OR (t.scope_key IS NOT NULL AND tp.scope_key IS NOT NULL"
            "            AND NOT EXISTS (SELECT 1 FROM fault_ledger o"
            "                             WHERE o.scope_key = f.scope_key"
            "                               AND o.product != f.product)))"
            "   AND (p.kind NOT IN " + listed(projected) + " OR tp.project_ref IS NOT NULL))"
            " ORDER BY turn, seq LIMIT ?")
        rows = db.execute(query, (PENDING, moment, *kinds, moment, *issue, OPEN_RECORD,
                                  *targeted, *projected, limit * 4)).fetchall()
        chosen = []
        for row in rows:
            pair = (row["fault_product"], row["kind"])
            if pair not in remaining:
                remaining[pair] = self._budget(db, pair[0], pair[1], moment)["remaining"]
            if remaining[pair] <= 0:
                continue
            if row["kind"] == OPEN_RECORD:
                # The query excludes these already; asked again through the one function that
                # decides the slot, so a change to that rule cannot miss this path.
                fault = db.execute("SELECT * FROM fault_ledger WHERE fault_id = ?",
                                   (row["fault_id"],)).fetchone()
                if _issue_slot(db, fault)[0] == SLOT_ISSUE:
                    continue
            remaining[pair] -= 1
            chosen.append(row)
            if len(chosen) >= limit:
                break
        return chosen

    def queue_state(self, *, limit=SHOWN_PER_PAGE, now=None) -> dict:
        """What is ready, what is held and why, and each pending product's budget."""
        moment = self.clock.now() if now is None else now
        limit = _bounded(limit, "limit")
        with self.store.transaction() as db:
            ready = self._ready(db, moment, limit)
            chosen = {row["publication_id"] for row in ready}
            held = []
            for row in db.execute(
                    "SELECT p.*, f.product AS fault_product FROM fault_publications p"
                    "  JOIN fault_ledger f ON f.fault_id = p.fault_id WHERE p.state = ?"
                    " ORDER BY p.rowid LIMIT ?", (PENDING, limit * 4)).fetchall():
                if row["publication_id"] in chosen:
                    continue
                held.append({"publicationId": row["publication_id"], "kind": row["kind"],
                             "product": row["fault_product"],
                             "reason": self._held_reason(db, row, moment) or "budget_spent"})
                if len(held) >= limit:
                    break
            budgets = [self._budget(db, product, kind, moment)
                       for (product, kind) in self._spent(db, moment)]
            return {"ready": [self._publication_view(db, row) for row in ready],
                    "held": held, "budgets": budgets}

    def _held_reason(self, db, row, moment):
        spec = KINDS.get(row["kind"])
        if spec is None:
            return "kind_unregistered"
        if row["next_attempt_at"] is not None and row["next_attempt_at"] > moment:
            return "backing_off"
        fault = db.execute("SELECT * FROM fault_ledger WHERE fault_id = ?",
                           (row["fault_id"],)).fetchone()
        if row["kind"] == OPEN_RECORD and _issue_slot(db, fault)[0] == SLOT_ISSUE:
            return "issue_owned"
        if spec["requires_issue"] and not fault["external_ref"]:
            return "awaiting_record"
        if spec["target"]:
            target, why = self._owned_target(db, fault["product"], fault["scope_key"])
            if target is None:
                return why
            if spec["target"] == "team+project" and not target["projectRef"]:
                return "awaiting_target"
        if self._budget(db, fault["product"], row["kind"], moment)["remaining"] <= 0:
            return "budget_spent"
        return None

    # ------------------------------------------------------------------ writing (invariants 2-4)

    def claim(self, publication, *, owner, takeover=False, now=None) -> dict:
        """A lease plus a per-claim token, which is what fences operation, complete and fail.

        The first claimant is the write's owner. Another needs takeover=True, which is recorded.
        One unit of the product's budget for this kind is consumed; a spent budget holds the
        write pending and drops nothing.
        """
        if not _named(owner):
            raise FaultRefused(RefusalReason.FAULT_OBSERVATION_MALFORMED, "a claim names its owner")
        moment = self.clock.now() if now is None else now
        stamp = self.clock.iso()
        token = secrets.token_hex(8)
        refusal = None
        with self.store.transaction() as db:
            row = self._publication_row(db, publication)
            spec = _kind(row["kind"])
            if row["state"] != PENDING:
                raise FaultRefused(
                    RefusalReason.FAULT_NOT_CLAIMABLE,
                    f"publication {publication} is {row['state']}" + (
                        ". An uncertain write is not reclaimed; reconcile it by reporting what"
                        " you observed" if row["state"] == UNCERTAIN else ""),
                )
            fault = db.execute("SELECT * FROM fault_ledger WHERE fault_id = ?",
                               (row["fault_id"],)).fetchone()
            if row["kind"] == OPEN_RECORD and _issue_slot(db, fault)[0] == SLOT_ISSUE:
                self._cancel(db, row, "the fault already owns an issue", stamp)
                refusal = FaultRefused(RefusalReason.FAULT_STATE_CONFLICT,
                                       "this fault owns an issue, so its create was cancelled")
            else:
                reason = self._held_reason(db, row, moment)
                if reason == "budget_spent":
                    raise FaultRefused(RefusalReason.FAULT_BUDGET_SPENT,
                                       f"{fault['product']}'s {row['kind']} budget is spent;"
                                       f" the write stays pending")
                if reason is not None:
                    raise FaultRefused(RefusalReason.FAULT_NOT_CLAIMABLE,
                                       f"publication {publication} is not claimable: {reason}")
                writer = db.execute(
                    "SELECT owner FROM fault_publication_attempts WHERE publication_id = ?"
                    " ORDER BY attempt_id LIMIT 1", (publication,)).fetchone()
                if writer is not None and writer["owner"] != owner and not takeover:
                    raise FaultRefused(
                        RefusalReason.FAULT_WRITER_CONFLICT,
                        f"this write belongs to {writer['owner']!r}; pass takeover to reassign it")
                attempt = row["attempts"] + 1
                # One claim, one identity: the attempt row it appends. The attempt NUMBER is not
                # one - retry() starts the count again - so the budget unit is charged against
                # the row, and a retried write is charged again instead of matching a unit an
                # earlier claim already spent. A spent budget raises inside this transaction,
                # which takes the row back out.
                claimed = db.execute(
                    "INSERT INTO fault_publication_attempts (publication_id, attempt, owner,"
                    "  takeover, claimed_at, claimed_ts) VALUES (?,?,?,?,?,?)",
                    (publication, attempt, owner,
                     1 if (writer is not None and writer["owner"] != owner) else 0,
                     stamp, moment)).lastrowid
                used = self._consume(db, fault["product"], row["kind"],
                                     f"{publication}:{claimed}", moment, stamp)
                if not used["consumed"]:
                    raise FaultRefused(RefusalReason.FAULT_BUDGET_SPENT,
                                       f"{fault['product']}'s {row['kind']} budget is spent")
                db.execute(
                    "UPDATE fault_publications SET state = ?, claim_token = ?, lease_owner = ?,"
                    "  lease_until = ?, attempts = ?, updated_at = ? WHERE publication_id = ?",
                    (CLAIMED, token, owner, moment + LEASE_SECONDS, attempt, stamp, publication))
        if refusal is not None:
            raise refusal
        return {"publicationId": publication, "claimToken": token, "owner": owner,
                "leaseUntil": moment + LEASE_SECONDS}

    def operation(self, publication, *, claim_token) -> dict:
        """The exact thing to execute, and the point after which a retry is not automatic.

        Before anything is issued: an issue create for a fault that owns an issue is cancelled
        (invariant 1); a target-bound write whose target is no longer its product's current one
        is re-pointed and returned to pending (invariant 8); then the kind's own pre_issue check
        runs, read-only, inside a savepoint that is always rolled back. Only then is the row
        issued, and from there only somebody who has LOOKED can move it.
        """
        moment = self.clock.now()
        now = self.clock.iso()
        outcome = None
        with self.store.transaction() as db:
            row = self._claimed(db, publication, claim_token)
            spec = _kind(row["kind"])
            fault = db.execute("SELECT * FROM fault_ledger WHERE fault_id = ?",
                               (row["fault_id"],)).fetchone()
            extra = db.execute("SELECT * FROM fault_publication_payloads WHERE"
                               " publication_id = ?", (publication,)).fetchone()
            queued_project = extra["project_ref"] if extra else None
            if row["kind"] == OPEN_RECORD and _issue_slot(db, fault)[0] == SLOT_ISSUE:
                self._cancel(db, row, "the fault already owns an issue", now)
                outcome = (RefusalReason.FAULT_STATE_CONFLICT,
                           "this fault owns an issue, so its create was cancelled")
            elif spec["target"]:
                target, why = self._owned_target(db, fault["product"], fault["scope_key"])
                wanted = (target["team"] if target else None,
                          target["projectRef"] if (target and spec["target"] == "team+project")
                          else None)
                if target is None or wanted != (row["tracker_ref"], queued_project) or (
                        spec["target"] == "team+project" and not wanted[1]):
                    db.execute("UPDATE fault_publications SET tracker_ref = ? WHERE"
                               " publication_id = ?", (wanted[0], publication))
                    _payload_set(db, publication, now, project_ref=wanted[1])
                    self._release(db, row, now, outcome="retargeted")
                    outcome = (RefusalReason.FAULT_NOT_CLAIMABLE,
                               f"the target changed since the claim ({why or 'retargeted'}); the"
                               f" write was re-pointed and returned to pending, not issued")
            if outcome is None and spec["requires_issue"] and not fault["external_ref"]:
                raise FaultRefused(RefusalReason.FAULT_NOT_CLAIMABLE,
                                   "this fault owns no issue yet")
            if outcome is None and spec["pre_issue"] is not None:
                context = {"publication": self._publication_view(db, row),
                           "fault": self._fault_view(db, fault), "db": db, "now": moment}
                db.execute("SAVEPOINT fault_pre_issue")
                before = db.total_changes
                try:
                    answer = spec["pre_issue"](context)
                finally:
                    wrote = db.total_changes != before
                    db.execute("ROLLBACK TO fault_pre_issue")
                    db.execute("RELEASE fault_pre_issue")
                if wrote:
                    raise FaultRefused(
                        RefusalReason.FAULT_NOT_CLAIMABLE,
                        f"the {row['kind']} pre-issue check wrote to the store; its writes were"
                        f" discarded and nothing was issued")
                if isinstance(answer, dict) and "cancel" in answer:
                    self._cancel(db, row, str(answer["cancel"]), now)
                    if row["kind"] == UPDATE_RECORD:
                        # A cancelled relink may have been what kept the link unlinked.
                        self._link_to_target(db, row["fault_id"], now)
                    outcome = (RefusalReason.FAULT_NOT_CLAIMABLE,
                               f"cancelled before issue: {answer['cancel']}")
                elif isinstance(answer, dict) and "hold" in answer:
                    seconds = answer.get("seconds", HOLD_SECONDS)
                    if isinstance(seconds, bool) or not isinstance(seconds, (int, float)) or (
                            seconds <= 0):
                        seconds = HOLD_SECONDS
                    self._release(db, row, now, outcome="held",
                                  next_attempt_at=moment + float(seconds),
                                  hold_reason=str(answer["hold"]))
                    outcome = (RefusalReason.FAULT_NOT_CLAIMABLE,
                               f"held before issue: {answer['hold']}")
                elif answer is not None:
                    raise FaultRefused(RefusalReason.FAULT_NOT_CLAIMABLE,
                                       f"the {row['kind']} pre-issue check answered {answer!r}")
            if outcome is None:
                db.execute(
                    "UPDATE fault_publications SET state = ?, issued_at = ?, updated_at = ?"
                    " WHERE publication_id = ?", (ISSUED, now, now, publication))
                db.execute(
                    "UPDATE fault_publication_attempts SET issued_at = ?, issued_ts = ?"
                    " WHERE " + CURRENT_ATTEMPT, (now, moment, publication))
                view = self._publication_view(db, row)
                block = render_block({**dict(row), "product": fault["product"],
                                      "fault_class": fault["fault_class"]})
        if outcome is not None:
            raise FaultRefused(*outcome)
        answer = {
            "publicationId": publication,
            "kind": row["kind"],
            "trackerRef": row["tracker_ref"],
            "projectRef": queued_project,
            "externalRef": fault["external_ref"] or row["external_ref"],
            "title": title_for(fault),
            "identityDigest": row["identity_digest"],
            "protocol": _protocol(row["kind"], spec),
            "note": "the connector's create takes no idempotency key, so a create can succeed"
                    " and lose its response. This row is now issued: if you cannot report an"
                    " outcome it becomes uncertain, and no second create is made until"
                    " somebody reports what they observed.",
        }
        if spec["evidence"] == "block":
            answer.update(block=block, startMarker=start_marker(publication),
                          endMarker=end_marker(publication))
        answer["payload"] = view["payload"]
        if row["kind"] == UPDATE_RECORD:
            answer["update"] = view["payload"]
        return answer

    def _claimed(self, db, publication, claim_token):
        """Invariant 3: a transition that belongs to the claim needs the current token."""
        row = self._publication_row(db, publication)
        if row["state"] != CLAIMED:
            raise FaultRefused(
                RefusalReason.FAULT_NOT_CLAIMABLE,
                f"an operation is handed out for a claimed publication; this one is"
                f" {row['state']}")
        if row["claim_token"] != claim_token:
            raise FaultRefused(RefusalReason.FAULT_CLAIM_STALE,
                               "this claim token is not the current one")
        return row

    def reconcile(self, publication, observed_text=None, *, searched=False, observed=None,
                  prior_ended=False, reason=None) -> dict:
        """Did this write land? Answered from what was observed, before anything is rewritten.

        An absence frees an issued or uncertain write only when somebody attests the issuing
        request has ENDED (invariant 2): its holder's fail(..., ended=True), or prior_ended with
        a reason, which is recorded. A timeout or a plain failure after issue proves nothing.
        """
        now = self.clock.iso()
        moment = self.clock.now()
        with self.store.transaction() as db:
            row = self._publication_row(db, publication)
            spec = _kind(row["kind"])
            fault = db.execute("SELECT * FROM fault_ledger WHERE fault_id = ?",
                               (row["fault_id"],)).fetchone()
            base = {"publicationId": publication, "state": row["state"]}
            if spec["evidence"] == "block":
                found = read_block(observed_text, publication)
                if found["duplicated"]:
                    return {**base, "outcome": "duplicate",
                            "detail": "more than one block for this publication is present;"
                                      " repair it before confirming"}
                if found["found"]:
                    if found["problems"]:
                        return {**base, "outcome": "malformed", "problems": found["problems"],
                                "detail": "a partial block is not proof that nothing landed"}
                    return {**base, "outcome": "present",
                            "detail": "this write already landed; complete from this observation"}
                attested = searched
            else:
                if not isinstance(observed, dict) or observed.get("issue") != fault["external_ref"]:
                    return {**base, "outcome": "absent_unattested",
                            "detail": "fields are attested only by a readback naming the issue"
                                      " this fault owns"}
                problems = spec["confirm"](self._publication_view(db, row), observed)
                if not problems:
                    return {**base, "outcome": "present",
                            "detail": "the owned issue already reads as this update; complete"
                                      " from this observation"}
                attested = True
            if not attested:
                return {**base, "outcome": "absent_unattested",
                        "detail": "no block was observed, and nobody attested that the search"
                                  " covered where it would be"}
            if row["state"] == CLAIMED:
                return {**base, "outcome": "absent",
                        "detail": "nothing was issued under this claim; its holder may write"}
            if row["state"] == ISSUED and row["lease_until"] is not None and (
                    row["lease_until"] > moment):
                return {**base, "outcome": "absent_in_flight",
                        "detail": "the write is still held under a live lease; an absence read"
                                  " now proves nothing about a request that may still land"}
            if row["state"] not in (ISSUED, UNCERTAIN):
                return {**base, "outcome": "absent", "detail": "nothing is outstanding"}
            if prior_ended and not _named(reason):
                raise FaultRefused(RefusalReason.FAULT_OBSERVATION_MALFORMED,
                                   "an attested end says what ended the request")
            latest = db.execute(
                "SELECT attempt_id, attempt, ended FROM fault_publication_attempts"
                " WHERE publication_id = ? ORDER BY attempt_id DESC LIMIT 1",
                (publication,)).fetchone()
            ended = bool(latest and latest["ended"])
            if prior_ended and latest is not None:
                db.execute(
                    "UPDATE fault_publication_attempts SET ended = 1, ended_at = ?,"
                    "  error = COALESCE(error || '; ', '') || ? WHERE attempt_id = ?",
                    (now, f"attested end: {reason}", latest["attempt_id"]))
            if not (ended or prior_ended):
                return {**base, "outcome": "absent_unproven",
                        "detail": "nobody attested that the issuing request ended, and one still"
                                  " travelling can land after this search; the write stays"
                                  " uncertain"}
            db.execute(
                "UPDATE fault_publications SET state = ?, claim_token = NULL, lease_owner = NULL,"
                "  lease_until = NULL, updated_at = ? WHERE publication_id = ?",
                (PENDING, now, publication))
            if latest is not None:
                db.execute("UPDATE fault_publication_attempts SET outcome = 'reconciled_absent'"
                           " WHERE attempt_id = ?", (latest["attempt_id"],))
            self._repoint(db, row["fault_id"], now)
            return {**base, "outcome": "absent", "state": PENDING,
                    "detail": "the attested search found nothing after the request ended, so one"
                              " further write is permitted"}

    def complete(self, publication, *, readback=None, claim_token=None, external_ref=None,
                 project_ref=None, observed=None, now=None) -> dict:
        """Confirmed against this publication's own evidence.

        A block kind confirms from the readback text by exact field; a fields kind from what was
        read back of the owned issue. An issue create also needs the project the saved issue
        reads back as, and is linked against the project its scope targets NOW.
        """
        moment = self.clock.iso()
        with self.store.transaction() as db:
            row = self._publication_row(db, publication)
            if row["state"] == CONFIRMED:
                return _publication(row) | {"confirmed": False, "reason": "already confirmed"}
            if row["state"] not in (CLAIMED, ISSUED, UNCERTAIN):
                raise FaultRefused(
                    RefusalReason.FAULT_STATE_CONFLICT,
                    f"a {row['state']} publication has no write outstanding to confirm;"
                    f" claim it and write it again",
                )
            if row["state"] in (CLAIMED, ISSUED) and row["claim_token"] != claim_token:
                raise FaultRefused(RefusalReason.FAULT_CLAIM_STALE,
                                   "this claim token is not the current one")
            spec = _kind(row["kind"])
            fault = db.execute("SELECT * FROM fault_ledger WHERE fault_id = ?",
                               (row["fault_id"],)).fetchone()
            view = self._publication_view(db, row)
            if spec["evidence"] == "block":
                problems = _block_mismatch(row, fault, read_block(readback, publication))
                if problems:
                    raise FaultRefused(RefusalReason.FAULT_READBACK_MISMATCH, "; ".join(problems))
                reference = external_ref or row["external_ref"]
                if row["kind"] == APPEND_COMMENT:
                    owned = fault["external_ref"]
                    reference = reference or owned
                    if not owned:
                        raise FaultRefused(RefusalReason.FAULT_STATE_CONFLICT,
                                           "this fault owns no issue yet, so a comment on it"
                                           " cannot be confirmed")
                    if reference != owned:
                        raise FaultRefused(RefusalReason.FAULT_READBACK_MISMATCH,
                                           f"this comment names {reference!r}, and the fault"
                                           f" owns {owned!r}")
                elif spec["creates"] and not reference:
                    raise FaultRefused(RefusalReason.FAULT_READBACK_MISMATCH,
                                       "a confirmed create must name what it created")
                if row["kind"] == OPEN_RECORD and not _named(project_ref):
                    raise FaultRefused(
                        RefusalReason.FAULT_READBACK_MISMATCH,
                        "a confirmed issue create must name the project the saved issue reads"
                        " back as; a create is not confirmed into an unknown project")
                if row["kind"] not in (OPEN_RECORD, APPEND_COMMENT):
                    problems = spec["confirm"](view, {"externalRef": reference,
                                                      **(observed or {})})
                    if problems:
                        raise FaultRefused(RefusalReason.FAULT_READBACK_MISMATCH,
                                           "; ".join(problems))
            else:
                if not isinstance(observed, dict) or observed.get("issue") != fault["external_ref"]:
                    raise FaultRefused(RefusalReason.FAULT_READBACK_MISMATCH,
                                       "an update is confirmed from a readback naming the issue"
                                       " this fault owns")
                problems = spec["confirm"](view, observed)
                if problems:
                    raise FaultRefused(RefusalReason.FAULT_READBACK_MISMATCH, "; ".join(problems))
                reference = fault["external_ref"]
            db.execute(
                "UPDATE fault_publications SET state = ?, external_ref = ?, confirmed_at = ?,"
                "  claim_token = NULL, lease_owner = NULL, lease_until = NULL,"
                "  last_error = NULL, updated_at = ? WHERE publication_id = ?",
                (CONFIRMED, reference, moment, moment, publication))
            db.execute(
                "UPDATE fault_publication_attempts SET outcome = 'confirmed', ended = 1,"
                "  ended_at = ? WHERE " + CURRENT_ATTEMPT, (moment, publication))
            if row["kind"] == OPEN_RECORD:
                db.execute(
                    "UPDATE fault_ledger SET external_ref = ?, published_at = ?,"
                    "  updated_at = ? WHERE fault_id = ? AND external_ref IS NULL",
                    (reference, moment, moment, row["fault_id"]))
                self._observe_link(db, row["fault_id"], reference, project_ref, moment)
            elif row["kind"] == UPDATE_RECORD and (view["payload"] or {}).get("op") == "set_project":
                self._observe_link(db, row["fault_id"], reference, view["payload"]["value"],
                                   moment)
            fresh = self._publication_row(db, publication)
        return _publication(fresh) | {"confirmed": True}

    def _observe_link(self, db, identifier, reference, observed_project, now):
        """What the owned issue read back as, compared with the target as it is NOW."""
        fault = db.execute("SELECT * FROM fault_ledger WHERE fault_id = ?",
                           (identifier,)).fetchone()
        target, _ = self._owned_target(db, fault["product"], fault["scope_key"])
        wanted = target["projectRef"] if target else None
        link = db.execute("SELECT * FROM fault_links WHERE fault_id = ?", (identifier,)).fetchone()
        # With no current project owned by the fault's product there is nothing the issue could
        # be linked TO, so any readback leaves it unlinked, awaiting a target.
        state = LINKED if (wanted is not None and observed_project == wanted
                           and not self._moving_elsewhere(db, identifier, wanted)) else UNLINKED
        if link is None:
            db.execute(
                "INSERT INTO fault_links (fault_id, external_ref, project_ref,"
                "  observed_project_ref, state, revision, updated_at) VALUES (?,?,?,?,?,0,?)",
                (identifier, reference, wanted, observed_project, state, now))
        else:
            db.execute(
                "UPDATE fault_links SET external_ref = ?, observed_project_ref = ?, state = ?,"
                "  updated_at = ? WHERE fault_id = ?",
                (reference, observed_project, state if link["project_ref"] in (None, wanted)
                 else UNLINKED, now, identifier))
        if state == UNLINKED:
            if wanted is None:
                self._unlink(db, identifier, now)
            else:
                self._relink(db, identifier, wanted, now)

    def fail(self, publication, *, claim_token, error, ended=False, now=None) -> dict:
        """This write did not report success. What may follow depends on whether it was issued.

        A claimed row never reached the connector: it goes back to pending with backoff. An
        issued one may have landed and becomes uncertain. ended=True is the holder attesting that
        the connector answered with a definitive refusal, which is what later lets an attested
        absence free it.
        """
        moment = self.clock.now() if now is None else now
        stamp = self.clock.iso()
        with self.store.transaction() as db:
            row = self._publication_row(db, publication)
            if row["state"] not in (CLAIMED, ISSUED):
                raise FaultRefused(
                    RefusalReason.FAULT_NOT_CLAIMABLE,
                    f"only a claimed or issued publication fails; this one is {row['state']}")
            if row["claim_token"] != claim_token:
                raise FaultRefused(RefusalReason.FAULT_CLAIM_STALE,
                                   "this claim token is not the current one")
            if row["state"] == ISSUED:
                db.execute(
                    "UPDATE fault_publications SET state = ?, last_error = ?, claim_token ="
                    "  NULL, lease_owner = NULL, lease_until = NULL, updated_at = ?"
                    " WHERE publication_id = ?", (UNCERTAIN, str(error), stamp, publication))
                db.execute(
                    "UPDATE fault_publication_attempts SET outcome = 'uncertain', error = ?,"
                    "  ended = ?, ended_at = ? WHERE " + CURRENT_ATTEMPT,
                    (str(error), 1 if ended else 0, stamp if ended else None, publication))
                state = UNCERTAIN
                self._notify(db, row["fault_id"], DECISION, row["cycle"], stamp,
                             reason=f"write:{publication}:{UNCERTAIN}")
            else:
                backoff = min(MAX_BACKOFF, BASE_BACKOFF * (2 ** max(0, row["attempts"] - 1)))
                terminal = row["attempts"] >= MAX_ATTEMPTS
                state = FAILED if terminal else PENDING
                db.execute(
                    "UPDATE fault_publications SET state = ?, last_error = ?,"
                    "  next_attempt_at = ?, claim_token = NULL, lease_owner = NULL,"
                    "  lease_until = NULL, updated_at = ? WHERE publication_id = ?",
                    (state, str(error), None if terminal else moment + backoff, stamp,
                     publication))
                db.execute(
                    "UPDATE fault_publication_attempts SET outcome = 'failed_before_issue',"
                    "  error = ?, ended = 1, ended_at = ? WHERE " + CURRENT_ATTEMPT,
                    (str(error), stamp, publication))
                if terminal:
                    self._notify(db, row["fault_id"], DECISION, row["cycle"], stamp,
                                 reason=f"write:{publication}:{FAILED}")
        return {"publicationId": publication, "state": state, "error": str(error),
                "detail": "an issued write may have landed, so it is uncertain rather than"
                          " retried" if state == UNCERTAIN else ""}

    def cancel(self, publication, *, reason) -> dict:
        """Cancel a write nothing has reached the connector with (invariant 4)."""
        if not _named(reason):
            raise FaultRefused(RefusalReason.FAULT_OBSERVATION_MALFORMED, "a cancel says why")
        now = self.clock.iso()
        with self.store.transaction() as db:
            row = self._publication_row(db, publication)
            self._cancel(db, row, reason, now)
            if row["kind"] == UPDATE_RECORD:
                self._link_to_target(db, row["fault_id"], now)
        return {"publicationId": publication, "state": CANCELLED, "reason": reason}

    def _cancel(self, db, row, reason, now):
        """Invariant 4: pending, failed and claimed-not-issued only. A claimed one refunds its
        own attempt and budget; nothing earlier is refunded."""
        if row["state"] not in (PENDING, FAILED, CLAIMED):
            raise FaultRefused(
                RefusalReason.FAULT_NOT_CLAIMABLE,
                f"a {row['state']} write may already have reached the connector; it is"
                f" reconciled, never cancelled")
        if row["state"] == CLAIMED:
            self._refund(db, row, "cancelled", now)
        db.execute(
            "UPDATE fault_publications SET state = ?, claim_token = NULL, lease_owner = NULL,"
            "  lease_until = NULL, last_error = ?, updated_at = ?,"
            "  attempts = ? WHERE publication_id = ?",
            (CANCELLED, reason, now,
             row["attempts"] - (1 if row["state"] == CLAIMED else 0), row["publication_id"]))

    def _cancel_where(self, db, identifier, reason, now, *, extra="", params=()):
        """_cancel() for every unissued write of one fault matching extra, set-based.

        The same outcome as calling _cancel() on each - a claimed one gives back its current
        claim's unit and ends that attempt, and nothing earlier is refunded - in three
        statements, so no number of writes is read into this process. extra and params narrow
        the writes, over the alias p.
        """
        chosen = ("SELECT p.publication_id FROM fault_publications p WHERE p.fault_id = ?"
                  " AND p.state = ?" + extra)
        claimed = (identifier, CLAIMED, *params)
        current = ("SELECT MAX(a.attempt_id) FROM fault_publication_attempts a"
                   " WHERE a.publication_id IN (" + chosen + ") GROUP BY a.publication_id")
        db.execute(
            "DELETE FROM fault_budget_uses"
            " WHERE product = (SELECT product FROM fault_ledger WHERE fault_id = ?)"
            "   AND ref IN (SELECT a.publication_id || ':' || a.attempt_id"
            "                 FROM fault_publication_attempts a"
            "                WHERE a.attempt_id IN (" + current + "))", (identifier, *claimed))
        db.execute("UPDATE fault_publication_attempts SET outcome = 'cancelled', ended = 1,"
                   "  ended_at = ? WHERE attempt_id IN (" + current + ")", (now, *claimed))
        return db.execute(
            "UPDATE fault_publications SET state = ?, claim_token = NULL, lease_owner = NULL,"
            "  lease_until = NULL, last_error = ?, updated_at = ?,"
            "  attempts = attempts - (CASE WHEN state = ? THEN 1 ELSE 0 END)"
            " WHERE publication_id IN (SELECT p.publication_id FROM fault_publications p"
            "   WHERE p.fault_id = ? AND p.state IN (?,?,?)" + extra + ")",
            (CANCELLED, reason, now, CLAIMED, identifier, PENDING, FAILED, CLAIMED,
             *params)).rowcount

    def _refund(self, db, row, outcome, now=None):
        """Give back the CURRENT claim's unit and end its attempt row; nothing earlier."""
        fault = db.execute("SELECT product FROM fault_ledger WHERE fault_id = ?",
                           (row["fault_id"],)).fetchone()
        current = db.execute("SELECT attempt_id FROM fault_publication_attempts WHERE "
                             + CURRENT_ATTEMPT, (row["publication_id"],)).fetchone()
        if current is None:
            return
        db.execute("DELETE FROM fault_budget_uses WHERE product = ? AND kind = ? AND ref = ?",
                   (fault["product"], row["kind"],
                    f"{row['publication_id']}:{current['attempt_id']}"))
        db.execute(
            "UPDATE fault_publication_attempts SET outcome = ?, ended = 1,"
            "  ended_at = COALESCE(?, ended_at) WHERE attempt_id = ?",
            (outcome, now, current["attempt_id"]))

    def _release(self, db, row, now, *, outcome, next_attempt_at=None, hold_reason=None):
        """Back to pending before anything was issued, with this claim's attempt refunded."""
        self._refund(db, row, outcome, now)
        db.execute(
            "UPDATE fault_publications SET state = ?, claim_token = NULL, lease_owner = NULL,"
            "  lease_until = NULL, attempts = ?, next_attempt_at = ?, updated_at = ?"
            " WHERE publication_id = ?",
            (PENDING, row["attempts"] - 1, next_attempt_at, now, row["publication_id"]))
        _payload_set(db, row["publication_id"], now, hold_reason=hold_reason)

    def expire_leases(self, *, now=None) -> dict:
        """A lapsed claim was never issued and is offered again; a lapsed issue is uncertain."""
        moment = self.clock.now() if now is None else now
        stamp = self.clock.iso()
        released = uncertain = 0
        with self.store.transaction() as db:
            # A bounded batch, oldest lease first; a later call takes the rest. A lapsed row
            # waiting its turn is still refused by claim() and still reconciled as issued.
            for row in db.execute(
                    "SELECT * FROM fault_publications WHERE state IN (?,?)"
                    " AND lease_until IS NOT NULL AND lease_until <= ?"
                    " ORDER BY lease_until, rowid LIMIT ?",
                    (CLAIMED, ISSUED, moment, RELINK_PER_CALL)).fetchall():
                if row["state"] == CLAIMED:
                    # Never issued, so nothing reached the connector: released like every other
                    # unissued claim, with its own unit and attempt given back. Keeping them let
                    # workers that crashed before operation() spend the product's budget with no
                    # write ever made.
                    self._release(db, row, stamp, outcome="lease_lapsed")
                    released += 1
                else:
                    db.execute(
                        "UPDATE fault_publications SET state = ?, claim_token = NULL,"
                        "  lease_owner = NULL, lease_until = NULL, updated_at = ?,"
                        "  last_error = COALESCE(last_error, 'the lease expired after the write"
                        " was issued') WHERE publication_id = ?",
                        (UNCERTAIN, stamp, row["publication_id"]))
                    db.execute("UPDATE fault_publication_attempts SET outcome = 'uncertain'"
                               " WHERE " + CURRENT_ATTEMPT, (row["publication_id"],))
                    self._notify(db, row["fault_id"], DECISION, row["cycle"], stamp,
                                 reason=f"write:{row['publication_id']}:{UNCERTAIN}")
                    uncertain += 1
            more = db.execute(
                "SELECT 1 FROM fault_publications WHERE state IN (?,?)"
                " AND lease_until IS NOT NULL AND lease_until <= ? LIMIT 1",
                (CLAIMED, ISSUED, moment)).fetchone() is not None
        return {"released": released, "uncertain": uncertain, "more": more}

    def retry(self, publication) -> dict:
        """An operator's decision to offer a failed write again. Never reaches uncertain."""
        stamp = self.clock.iso()
        with self.store.transaction() as db:
            row = self._publication_row(db, publication)
            if row["state"] != FAILED:
                raise FaultRefused(
                    RefusalReason.FAULT_NOT_CLAIMABLE,
                    f"retry offers a failed publication again; this one is {row['state']}" + (
                        ". An uncertain write is reconciled, not retried"
                        if row["state"] == UNCERTAIN else ""))
            owner = db.execute("SELECT * FROM fault_ledger WHERE fault_id = ?",
                               (row["fault_id"],)).fetchone()
            if row["kind"] == OPEN_RECORD and _issue_slot(db, owner)[0] == SLOT_ISSUE:
                raise FaultRefused(RefusalReason.FAULT_STATE_CONFLICT,
                                   "this fault already owns an issue; its create is not retried")
            db.execute(
                "UPDATE fault_publications SET state = ?, attempts = 0, next_attempt_at ="
                "  NULL, updated_at = ? WHERE publication_id = ?", (PENDING, stamp, publication))
        return {"publicationId": publication, "state": PENDING}

    def _publication_row(self, db, publication):
        row = db.execute(
            "SELECT * FROM fault_publications WHERE publication_id = ?",
            (publication,)).fetchone()
        if row is None:
            raise FaultRefused(RefusalReason.FAULT_UNKNOWN,
                               f"no publication {publication!r}")
        return row

    # ------------------------------------------------------------------ attention (B11)

    def attention(self, *, now=None) -> dict:
        """Everything unsent, and a warning an operator sees on the next status or tick."""
        moment = self.clock.now() if now is None else now
        with self.store.transaction() as db:
            count = {}
            for state in (FAILED, UNCERTAIN, ISSUED):
                count[state] = db.execute(
                    "SELECT COUNT(*) AS n FROM fault_publications WHERE state = ?",
                    (state,)).fetchone()["n"]
            claimed = db.execute(
                "SELECT COUNT(*) AS n, SUM(CASE WHEN lease_until <= ? THEN 1 ELSE 0 END) AS lapsed"
                " FROM fault_publications WHERE state = ?", (moment, CLAIMED)).fetchone()
            # An issued write in flight is normal and warns nobody. One whose lease has lapsed,
            # or that no holder leases at all, may or may not have landed and only a reconcile
            # settles it - whether or not anything has expired leases yet.
            issued_lapsed = db.execute(
                "SELECT COUNT(*) AS n FROM fault_publications WHERE state = ?"
                " AND (lease_until IS NULL OR lease_until <= ?)", (ISSUED, moment)).fetchone()["n"]
            pending = {"ready": 0, "awaitingTarget": 0, "held": 0, "awaitingRecord": 0,
                       "scopeKeyContested": 0, "backingOff": 0, "kindUnregistered": 0,
                       "issueOwned": 0}
            names = {None: "ready", "awaiting_target": "awaitingTarget",
                     "budget_spent": "held", "awaiting_record": "awaitingRecord",
                     "scope_key_contested": "scopeKeyContested", "backing_off": "backingOff",
                     "kind_unregistered": "kindUnregistered", "issue_owned": "issueOwned"}
            classified = 0
            for row in db.execute("SELECT * FROM fault_publications WHERE state = ?"
                                  " ORDER BY rowid LIMIT 1000", (PENDING,)).fetchall():
                pending[names[self._held_reason(db, row, moment)]] += 1
                classified += 1
            # Classified up to a bound, counted without one: a larger queue is not reported as
            # smaller than it is.
            pending["unclassified"] = db.execute(
                "SELECT COUNT(*) AS n FROM fault_publications WHERE state = ?",
                (PENDING,)).fetchone()["n"] - classified
            unlinked = db.execute("SELECT COUNT(*) AS n FROM fault_links WHERE state = ?",
                                  (UNLINKED,)).fetchone()["n"]
            notifications = {state: db.execute(
                "SELECT COUNT(*) AS n FROM fault_notifications WHERE state = ?",
                (state,)).fetchone()["n"] for state in (PENDING, RESERVED, UNCERTAIN)}
            # Uncertain already, though nothing has lapsed it yet: the same as an issued write
            # whose lease ran out.
            notifications["reservedLapsed"] = db.execute(
                "SELECT COUNT(*) AS n FROM fault_notifications WHERE state = ?"
                " AND (lease_until IS NULL OR lease_until <= ?)",
                (RESERVED, moment)).fetchone()["n"]
        unsent = {**pending, "claimed": claimed["n"], "claimedLapsed": claimed["lapsed"] or 0,
                  "issued": count[ISSUED], "issuedLapsed": issued_lapsed,
                  "failed": count[FAILED], "uncertain": count[UNCERTAIN]}
        total = sum(value for key, value in unsent.items()
                    if key not in ("claimedLapsed", "issued"))
        warning = None
        doubtful = notifications[UNCERTAIN] + notifications["reservedLapsed"]
        if total or unlinked or doubtful:
            parts = [f"{value} {key}" for key, value in unsent.items() if value]
            if unlinked:
                parts.append(f"{unlinked} issue(s) not in their project")
            if doubtful:
                parts.append(f"{doubtful} notification(s) uncertain")
            warning = "fault writes need attention: " + ", ".join(parts)
        return {"unsent": unsent, "unlinked": unlinked, "notifications": notifications,
                "warning": warning}

    # ------------------------------------------------------------------ notifications (B11)

    def _notify(self, db, identifier, kind, cycle, now, *, reason=None, ref=None):
        fault = db.execute("SELECT product FROM fault_ledger WHERE fault_id = ?",
                           (identifier,)).fetchone()
        key = f"{identifier}|{kind}|{reason}" if reason else f"{identifier}|{kind}|{cycle}"
        notification = sha256_hex(key)[:ID_WIDTH]
        db.execute(
            "INSERT OR IGNORE INTO fault_notifications (notification_id, fault_id, product,"
            "  kind, reason, cycle, ref, state, created_at, updated_at)"
            " VALUES (?,?,?,?,?,?,?,?,?,?)",
            (notification, identifier, fault["product"], kind, reason, cycle, ref, PENDING,
             now, now))
        return notification

    def raise_notification(self, identifier, *, reason, ref=None) -> dict:
        """A caller's own decision, on the one notification path. Idempotent per fault and reason."""
        if not _named(reason):
            raise FaultRefused(RefusalReason.FAULT_OBSERVATION_MALFORMED,
                               "a raised notification names its reason")
        now = self.clock.iso()
        with self.store.transaction() as db:
            fault = self._fault(db, identifier)
            notification = self._notify(db, fault["fault_id"], DECISION, fault["cycle"], now,
                                        reason=f"raised:{reason}", ref=ref)
        return {"notificationId": notification, "faultId": fault["fault_id"], "kind": DECISION,
                "reason": reason}

    def _eligibility(self, db, identifier, moment) -> dict:
        from . import supervision

        fault = db.execute("SELECT * FROM fault_ledger WHERE fault_id = ?",
                           (identifier,)).fetchone()
        relationship = _json(fault["signature"]).get("relationship")
        row = None
        if _named(relationship):
            row = db.execute("SELECT status, parent_task_id FROM relationships"
                             " WHERE relationship_id = ?", (relationship,)).fetchone()
        issue = _json(fault["scope"]).get("issueKey")
        if row is None and _named(issue):
            row = db.execute(
                "SELECT status, parent_task_id FROM relationships WHERE issue_key = ?"
                " AND superseded_by IS NULL ORDER BY created_at DESC LIMIT 1",
                (issue,)).fetchone()
        if row is None:
            return {"eligible": True, "reason": "no relationship whose wishes apply"}
        if row["status"] in ("paused", "cancelled", "archived"):
            return {"eligible": False, "reason": f"the relationship is {row['status']}"}
        contact = supervision.contactable(self.store, row["parent_task_id"], now=moment)
        if contact.get("contactable") is False:
            return {"eligible": False, "reason": f"no contact: {contact.get('reason')}"}
        if contact.get("contactable") is None and contact.get("asked", True):
            return {"eligible": False,
                    "reason": f"contact unmeasured: {contact.get('reason')}"}
        return {"eligible": True, "reason": "reportable"}

    def _lapse_notifications(self, db, moment, stamp):
        db.execute("UPDATE fault_notifications SET state = ?, token = NULL, updated_at = ?"
                   " WHERE state = ? AND lease_until <= ?", (UNCERTAIN, stamp, RESERVED, moment))

    def notifications(self, *, state=None, limit=SHOWN_PER_PAGE, after=None) -> dict:
        """Notifications in one state (pending by default), each with its delivery key and, for
        pending ones, whether it may be delivered now."""
        limit = _bounded(limit, "limit")
        moment = self.clock.now()
        with self.store.transaction() as db:
            self._lapse_notifications(db, moment, self.clock.iso())
            rows = db.execute(
                "SELECT rowid AS seq, * FROM fault_notifications WHERE state = ? AND rowid > ?"
                " ORDER BY rowid LIMIT ?", (state or PENDING, after or 0, limit + 1)).fetchall()
            listed = []
            for row in rows[:limit]:
                entry = _notification(row)
                if row["state"] == PENDING:
                    entry["eligibility"] = self._eligibility(db, row["fault_id"], moment)
                listed.append(entry)
        return {"notifications": listed,
                "next": rows[limit - 1]["seq"] if len(rows) > limit else None}

    def reserve_notifications(self, *, owner, limit=SHOWN_PER_PAGE) -> dict:
        """Take eligible notifications, consuming their product's budget, under a lease.

        Eligibility and budget are decided here, atomically, so two reservers cannot together
        exceed a budget and a withheld one is never taken.
        """
        if not _named(owner):
            raise FaultRefused(RefusalReason.FAULT_OBSERVATION_MALFORMED, "a reservation has an owner")
        limit = _bounded(limit, "limit")
        moment = self.clock.now()
        stamp = self.clock.iso()
        reserved, withheld, held = [], 0, 0
        with self.store.transaction() as db:
            self._lapse_notifications(db, moment, stamp)
            # Round-robin by product, a spent product excluded inside the query, and the least
            # recently examined first: every candidate examined and not taken is stamped, so a
            # later call - however long after - reaches the ones behind it. A fixed head of the
            # queue used to hide every eligible notification behind it.
            candidates = db.execute(
                "SELECT * FROM (SELECT n.*, n.rowid AS seq,"
                "   ROW_NUMBER() OVER (PARTITION BY n.product"
                "                      ORDER BY COALESCE(n.examined_seq, 0), n.rowid) AS turn"
                "  FROM fault_notifications n WHERE n.state = ?"
                "   AND " + self._open_budget("n.product", "'" + NOTIFICATION + "'") + ")"
                " ORDER BY turn, COALESCE(examined_seq, 0), seq LIMIT ?",
                (PENDING, moment, limit * 4)).fetchall()
            # A sequence rather than a time: two examinations never tie, so however fast or
            # slow the calls, the ones examined last are examined last again.
            examined = db.execute("SELECT COALESCE(MAX(examined_seq), 0) AS n"
                                  " FROM fault_notifications").fetchone()["n"]
            for row in candidates:
                if len(reserved) >= limit:
                    break
                examined += 1
                db.execute("UPDATE fault_notifications SET examined_seq = ?"
                           " WHERE notification_id = ?", (examined, row["notification_id"]))
                if not self._eligibility(db, row["fault_id"], moment)["eligible"]:
                    withheld += 1
                    continue
                used = self._consume(db, row["product"], NOTIFICATION,
                                     f"{row['notification_id']}:{row['attempts'] + 1}",
                                     moment, stamp)
                if not used["consumed"]:
                    held += 1
                    continue
                token = secrets.token_hex(8)
                db.execute(
                    "UPDATE fault_notifications SET state = ?, token = ?, owner = ?,"
                    "  lease_until = ?, attempts = attempts + 1, updated_at = ?"
                    " WHERE notification_id = ?",
                    (RESERVED, token, owner, moment + LEASE_SECONDS, stamp,
                     row["notification_id"]))
                entry = _notification(db.execute(
                    "SELECT * FROM fault_notifications WHERE notification_id = ?",
                    (row["notification_id"],)).fetchone())
                entry["token"] = token
                reserved.append(entry)
        return {"reserved": reserved, "withheld": withheld, "held": held}

    def ack_notification(self, notification, *, token, ref) -> dict:
        """Delivered. Accepted for the current token whatever happened to eligibility since."""
        return self._settle_notification(notification, token=token, delivered=True, ref=ref)

    def fail_notification(self, notification, *, token, error) -> dict:
        """Not sent, as the deliverer knows. Back to pending with the error; nothing dropped."""
        return self._settle_notification(notification, token=token, delivered=False, ref=error)

    def reconcile_notification(self, notification, *, delivered, ref) -> dict:
        """Settle an uncertain notification from what the deliverer can read back."""
        return self._settle_notification(notification, token=None, delivered=delivered, ref=ref)

    def _settle_notification(self, notification, *, token, delivered, ref):
        stamp = self.clock.iso()
        with self.store.transaction() as db:
            self._lapse_notifications(db, self.clock.now(), stamp)
            row = db.execute("SELECT * FROM fault_notifications WHERE notification_id = ?",
                             (notification,)).fetchone()
            if row is None:
                raise FaultRefused(RefusalReason.FAULT_UNKNOWN, f"no notification {notification}")
            if token is None:
                if row["state"] != UNCERTAIN:
                    raise FaultRefused(RefusalReason.FAULT_STATE_CONFLICT,
                                       f"only an uncertain notification is reconciled; this one"
                                       f" is {row['state']}")
            elif row["state"] != RESERVED or row["token"] != token:
                raise FaultRefused(RefusalReason.FAULT_CLAIM_STALE,
                                   "this reservation token is not the current one")
            if delivered:
                db.execute("UPDATE fault_notifications SET state = ?, token = NULL,"
                           " delivered_at = ?, ack_ref = ?, updated_at = ?"
                           " WHERE notification_id = ?",
                           (DELIVERED, stamp, ref, stamp, notification))
            else:
                db.execute("UPDATE fault_notifications SET state = ?, token = NULL,"
                           " last_error = ?, updated_at = ? WHERE notification_id = ?",
                           (PENDING, ref, stamp, notification))
            fresh = db.execute("SELECT * FROM fault_notifications WHERE notification_id = ?",
                               (notification,)).fetchone()
        return _notification(fresh)

def _json(text):
    try:
        value = json.loads(text) if text else {}
    except (TypeError, ValueError):
        return {}
    return value if isinstance(value, dict) else {}


def _exists(db, identifier):
    return db.execute("SELECT 1 FROM fault_ledger WHERE fault_id = ?",
                      (identifier,)).fetchone() is not None


# The current claim's attempt row, parameterised by the publication id. One claim is live at a
# time and every claim appends one row, so the newest row is the claim a transition concerns.
# Its attempt number is not an identity: retry() starts the count again, and after a retry
# "attempt 1" names two claims - an update by number rewrote the earlier one's history too.
CURRENT_ATTEMPT = ("attempt_id = (SELECT MAX(attempt_id) FROM fault_publication_attempts"
                   " WHERE publication_id = ?)")

SLOT_ISSUE = "issue"
SLOT_CREATE = "create"


def _issue_slot(db, fault):
    """Invariant 1: who holds this fault's one issue slot.

    Returns (SLOT_ISSUE, None) when the fault owns an issue, (SLOT_CREATE, row) when a
    non-cancelled open_record holds it, and (None, row_or_None) when the slot is free - the row
    then being a cancelled create, which is revived under its own id and never joined by a
    second one. Every path that decides whether a fault is opened, whether a create may be
    queued, offered, claimed, issued or retried, and whether an issue may be adopted asks this.
    """
    if fault["external_ref"]:
        return SLOT_ISSUE, None
    create = db.execute("SELECT * FROM fault_publications WHERE fault_id = ? AND kind = ?",
                        (fault["fault_id"], OPEN_RECORD)).fetchone()
    if create is not None and create["state"] != CANCELLED:
        return SLOT_CREATE, create
    return None, create


def _landed(db, identifier):
    """Invariant 5: a write has landed, or may have, once it is issued."""
    return db.execute(
        "SELECT 1 FROM fault_publications WHERE fault_id = ? AND state IN (?,?,?) LIMIT 1",
        (identifier, ISSUED, UNCERTAIN, CONFIRMED)).fetchone() is not None


def _read_adoption(adopt):
    if not isinstance(adopt, dict):
        raise FaultRefused(RefusalReason.FAULT_OBSERVATION_MALFORMED,
                           "an adoption is {externalRef, scope}")
    reference = adopt.get("externalRef")
    if not _named(reference):
        raise FaultRefused(RefusalReason.FAULT_OBSERVATION_MALFORMED,
                           "an adoption names the issue it adopts")
    scope = _read_scope(adopt.get("scope"))
    if not _named(scope.get("projectKey")):
        raise FaultRefused(RefusalReason.FAULT_OBSERVATION_MALFORMED,
                           "an adoption's scope carries the owner's projectKey")
    return {"externalRef": reference, "scope": scope}


_UNCHANGED = object()


def _payload_set(db, publication, now, *, project_ref=_UNCHANGED, payload=_UNCHANGED,
                 hold_reason=_UNCHANGED, target_mode=_UNCHANGED):
    row = db.execute("SELECT * FROM fault_publication_payloads WHERE publication_id = ?",
                     (publication,)).fetchone()
    values = {"project_ref": row["project_ref"] if row else None,
              "payload": row["payload"] if row else None,
              "hold_reason": row["hold_reason"] if row else None,
              "target_mode": row["target_mode"] if row else None}
    if project_ref is not _UNCHANGED:
        values["project_ref"] = project_ref
    if payload is not _UNCHANGED:
        values["payload"] = (None if payload is None else
                             json.dumps(payload, ensure_ascii=False, sort_keys=True))
    if hold_reason is not _UNCHANGED:
        values["hold_reason"] = hold_reason
    if target_mode is not _UNCHANGED:
        values["target_mode"] = target_mode
    db.execute(
        "INSERT INTO fault_publication_payloads (publication_id, project_ref, payload,"
        "  hold_reason, updated_at, target_mode) VALUES (?,?,?,?,?,?)"
        " ON CONFLICT(publication_id) DO UPDATE"
        " SET project_ref = excluded.project_ref, payload = excluded.payload,"
        "   hold_reason = excluded.hold_reason, updated_at = excluded.updated_at,"
        "   target_mode = excluded.target_mode",
        (publication, values["project_ref"], values["payload"], values["hold_reason"], now,
         values["target_mode"]))


def _notification(row) -> dict:
    record = dict(row)
    record.pop("token", None)
    return {
        "notificationId": record["notification_id"], "faultId": record["fault_id"],
        "product": record["product"], "kind": record["kind"],
        "reason": (record["reason"] or "").removeprefix("raised:") or None,
        "cycle": record["cycle"], "ref": record["ref"], "state": record["state"],
        "deliveryKey": f"relay-notification:{record['notification_id']}",
        "owner": record["owner"], "attempts": record["attempts"],
        "lastError": record["last_error"], "createdAt": record["created_at"],
        "deliveredAt": record["delivered_at"], "ackRef": record["ack_ref"],
    }


# ------------------------------------------------------------------ publication kinds

# Registered per process, by importing the module that declares a kind. A publication whose kind
# is not registered in the acting process is never offered, claimed or issued (invariant 13).
KINDS = {}
BUILT_IN_KINDS = (OPEN_RECORD, APPEND_COMMENT, UPDATE_RECORD)


def register_kind(name, *, creates, requires_issue, target, evidence, confirm, validate=None,
                  pre_issue=None) -> dict:
    """Declare a publication kind. What a registered create makes is recorded on its own
    publication and never becomes the fault's issue: only open_record creates that."""
    if not _named(name) or "|" in name:
        raise ValueError("a kind is a non-blank name without '|'")
    if name in BUILT_IN_KINDS and name in KINDS:
        raise ValueError(f"{name!r} is built in")
    if target not in (None, "team", "team+project"):
        raise ValueError("target is None, 'team' or 'team+project'")
    if evidence not in ("block", "fields"):
        raise ValueError("evidence is 'block' or 'fields'")
    for label, value in (("confirm", confirm), ("validate", validate), ("pre_issue", pre_issue)):
        if value is not None and not callable(value):
            raise ValueError(f"{label} is callable")
    if not callable(confirm):
        raise ValueError("a kind declares how a readback confirms it")
    spec = {"name": name, "creates": bool(creates), "requires_issue": bool(requires_issue),
            "target": target, "evidence": evidence, "confirm": confirm, "validate": validate,
            "pre_issue": pre_issue}
    KINDS[name] = spec
    return {key: value for key, value in spec.items() if not callable(value)}


def _kind(name):
    spec = KINDS.get(name)
    if spec is None:
        raise FaultRefused(
            RefusalReason.FAULT_KIND_UNREGISTERED,
            f"kind {name!r} is not registered in this process; load the module that declares it"
            f" (--kind-module) before acting on its writes")
    return spec


def _validate_update(payload):
    if not isinstance(payload, dict):
        return ["an update is {op, value}"]
    op, value = payload.get("op"), payload.get("value")
    if op not in UPDATE_OPS:
        return [f"op {op!r} is not one of {UPDATE_OPS}"]
    if op == "set_project" and not _named(value):
        return ["set_project names a project"]
    if op == "reopen" and value is not None:
        return ["reopen takes no value"]
    if op == "add_relation" and not (isinstance(value, dict) and _named(value.get("type"))
                                     and _named(value.get("issue"))):
        return ["add_relation is {type, issue}"]
    if op == "add_label" and not _named(value):
        return ["add_label names a label"]
    return []


def _confirm_update(expected, observed):
    payload = expected.get("payload") or {}
    op, value = payload.get("op"), payload.get("value")
    if op == "set_project":
        found = observed.get("projectId")
        return [] if found == value else [f"the issue reads project {found!r}, not {value!r}"]
    if op == "reopen":
        return [] if observed.get("open") is True else ["the issue does not read as open"]
    if op == "add_relation":
        relations = observed.get("relations") or []
        return [] if value in relations else [f"the issue has no relation {value!r}"]
    if op == "add_label":
        labels = observed.get("labels") or []
        return [] if value in labels else [f"the issue has no label {value!r}"]
    return [f"unknown update {op!r}"]


def _set_project_is_current(context):
    """set_project's built-in pre-issue check: a project the scope has left is cancelled."""
    payload = context["publication"].get("payload") or {}
    if payload.get("op") != "set_project":
        return None
    db, fault = context["db"], context["fault"]
    row = db.execute(
        "SELECT p.project_ref, p.product FROM fault_target_projects p WHERE p.scope_key = ?",
        (fault["scope_key"],)).fetchone()
    current = row["project_ref"] if row and row["product"] == fault["product"] else None
    if current is None:
        # Removed, never owned by this product, or owned by another: a relink queued for the
        # old project is stale either way, and issuing it would move the issue there and record
        # the link as healthy. Setting a target again queues a fresh set_project.
        return {"cancel": f"the scope no longer targets a project {fault['product']} owns,"
                          f" so {payload.get('value')} is not where the issue belongs"}
    if current != payload.get("value"):
        return {"cancel": f"the scope now targets {current}, not {payload.get('value')}"}
    return None


KINDS[OPEN_RECORD] = {"name": OPEN_RECORD, "creates": True, "requires_issue": False,
                      "target": "team+project", "evidence": "block",
                      "confirm": lambda expected, observed: [], "validate": None,
                      "pre_issue": None}
KINDS[APPEND_COMMENT] = {"name": APPEND_COMMENT, "creates": False, "requires_issue": True,
                         "target": None, "evidence": "block",
                         "confirm": lambda expected, observed: [], "validate": None,
                         "pre_issue": None}
KINDS[UPDATE_RECORD] = {"name": UPDATE_RECORD, "creates": False, "requires_issue": True,
                        "target": None, "evidence": "fields", "confirm": _confirm_update,
                        "validate": _validate_update, "pre_issue": _set_project_is_current}


def target_missing(db, row):
    """Whether this fault's scope has a target its product owns."""
    found = db.execute(
        "SELECT p.product FROM fault_targets t JOIN fault_target_projects p"
        "  ON p.scope_key = t.scope_key WHERE t.scope_key = ?", (row["scope_key"],)).fetchone()
    return found is None or found["product"] != row["product"]


def _protocol(kind, spec=None) -> list:
    spec = spec or KINDS.get(kind) or {}
    if kind == OPEN_RECORD:
        return [
            "search the tracker for this publication's start marker BEFORE creating anything",
            "marker found: the create already landed. Complete from that observation and do"
            " NOT create again",
            "marker absent: create one issue IN projectRef whose description carries this block"
            " verbatim",
            "read the created issue back and pass its full text to complete, with its"
            " identifier as the external reference and the project it reads back as",
            "response lost, or you cannot tell: report failure. The row becomes uncertain and"
            " no second create is made until somebody attests the request ended and nothing"
            " landed",
        ]
    if kind == UPDATE_RECORD:
        return [
            "read the owned issue's current fields",
            "already as requested: complete from that observation",
            "otherwise apply the one update and read the issue back",
            "pass what was read back to complete as observed, naming the issue",
        ]
    if kind == APPEND_COMMENT:
        return [
            "read the issue's comments and look for this publication's start marker",
            "marker found: this comment already landed. Complete from that observation",
            "marker absent: add one comment carrying this block verbatim",
            "read it back and pass the comment text to complete",
        ]
    return [f"{kind}: follow the protocol its registering module documents",
            "complete from what was read back; a lost response is uncertain, never repeated"]


def _block_mismatch(row, fault, found) -> list:
    """The WHOLE rendered identity section, then the summary, then its digest."""
    if not found["found"]:
        return ["no block for this publication was observed in the readback"]
    if found["duplicated"]:
        return ["more than one block for this publication is present"]
    if found["problems"]:
        return list(found["problems"])
    expected = {
        "blockFormat": BLOCK_FORMAT,
        "publicationId": row["publication_id"],
        "faultId": row["fault_id"],
        "product": fault["product"],
        "faultClass": fault["fault_class"],
        "trigger": row["trigger_key"],
        "cycle": str(row["cycle"]),
        "identityDigest": row["identity_digest"],
        "summarySha256": sha256_hex(sync.canonical_summary(row["summary"])),
    }
    problems = []
    for key in RENDERED_FIELDS:
        actual = found["fields"].get(key)
        if actual != expected[key]:
            problems.append(f"{key} reads {actual!r}, not {expected[key]!r}")
    observed = sync.canonical_summary(found["summary"])
    if observed != sync.canonical_summary(row["summary"]):
        problems.append("the summary in the record is not the summary this job carries")
    return problems


def _transition(state, cycle, *, cleared, publishable, escalated, severity, landed=False,
                opened=False):
    """The next state, and the one reason a write is owed. Returns no reason for most calls.

    Two different questions. landed - has any write for this fault been issued, may it have,
    or has one confirmed - decides only what a CLEAR does: owning an issue is not landing, so an
    adopted issue nobody has written to yet is withdrawn by a clear exactly like a create nobody
    has claimed. opened - does the fault own an issue or have a live create - decides everything
    else, because a fault whose create is queued has been opened and must not be opened again.
    """
    if cleared:
        if landed or state in (RESOLVED, WITHDRAWN):
            return state, cycle, False, None
        return WITHDRAWN, cycle, False, None
    if state == WITHDRAWN:
        state = OBSERVED
    if state == RESOLVED:
        if not opened:
            return OPEN, cycle + 1, True, (TRIGGER_OPEN if publishable else None)
        return OPEN, cycle + 1, True, f"{TRIGGER_REOPEN}:{cycle + 1}"
    if state == FIX_PENDING:
        if not opened:
            return OPEN, cycle, False, (TRIGGER_OPEN if publishable else None)
        return OPEN, cycle, False, f"{TRIGGER_RECUR}:{cycle}"
    if state == OPEN:
        if not opened and publishable:
            return OPEN, cycle, False, TRIGGER_OPEN
        return OPEN, cycle, False, (f"{TRIGGER_ESCALATE}:{severity}" if escalated else None)
    if publishable:
        return OPEN, cycle, False, TRIGGER_OPEN
    return OBSERVED, cycle, False, None


def _occurrence(row) -> dict:
    record = dict(row)
    try:
        record["evidence"] = json.loads(record["evidence"])
    except (TypeError, ValueError):
        record["evidence"] = []
    record["truncated"] = bool(record["truncated"])
    record["cleared"] = bool(record["cleared"])
    return record


def _publication(row) -> dict:
    record = dict(row)
    record.pop("claim_token", None)
    for helper in ("fault_product", "seq", "turn"):
        record.pop(helper, None)
    return record
