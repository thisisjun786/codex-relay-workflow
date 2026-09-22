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

OPEN_RECORD = "open_record"
APPEND_COMMENT = "append_comment"

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
    clears="a later reading of the same relationship that establishes something",
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


def fault_id(product, fault_class, signature) -> str:
    """One breakage, one id, however many times and by whoever it is observed.

    The time, the occurrence, the attempt, the event and the scope are all deliberately
    outside it. Each of them changes while the fault stays the same, and an identity carrying
    any of them files a second issue every time the system fails again.
    """
    for name, value in (("product", product), ("faultClass", fault_class)):
        if not _named(value):
            raise FaultRefused(
                RefusalReason.FAULT_OBSERVATION_MALFORMED,
                f"{name} must be a non-empty string",
            )
        if "|" in value:
            raise FaultRefused(
                RefusalReason.FAULT_OBSERVATION_MALFORMED,
                f"{name} must not contain '|', which is the field separator",
            )
    return sha256_hex(f"{product}|{fault_class}|{canonical_signature(signature)}")[:ID_WIDTH]


def occurrence_id(identifier, occurrence_key) -> str:
    return sha256_hex(f"{identifier}|{occurrence_key}")[:ID_WIDTH]


def publication_id(identifier, kind, trigger_key) -> str:
    """Identity includes WHY, so one reason queues one write however often it is swept."""
    return sha256_hex(f"{identifier}|{kind}|{trigger_key}")[:ID_WIDTH]


def identity_digest(identifier, kind, trigger_key, cycle) -> str:
    return sha256_hex(f"{identifier}|{kind}|{trigger_key}|{cycle}")


def evidence_digest(evidence) -> str:
    return sha256_hex(json.dumps(evidence, sort_keys=True, ensure_ascii=False,
                                 separators=(",", ":")))


def scope_key_for(product, scope) -> str:
    """Where this fault is filed. Outside identity, because a re-read scope is one fault."""
    project = (scope or {}).get("projectKey")
    return f"{product}:{project}" if _named(project) else product


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
    identifier = fault_id(product, fault_class, observation_record.get("signature"))
    occurrence_key = observation_record.get("occurrenceKey")
    if not _named(occurrence_key):
        raise FaultRefused(
            RefusalReason.FAULT_OBSERVATION_MALFORMED,
            "occurrenceKey must be a non-empty string derived from the underlying fact, or a"
            " repeated sweep of one unchanged row counts as many occurrences",
        )
    scope = observation_record.get("scope") or {}
    if not isinstance(scope, dict):
        raise FaultRefused(RefusalReason.FAULT_OBSERVATION_MALFORMED, "scope is an object")
    evidence, truncated = _bounded_evidence(observation_record.get("evidence"))
    return {
        "faultId": identifier,
        "product": product,
        "faultClass": fault_class,
        "component": policy["component"],
        "severity": severity,
        "signature": canonical_signature(observation_record.get("signature")),
        "scope": scope,
        "scopeKey": scope_key_for(product, scope),
        "occurrenceKey": occurrence_key,
        "occurrenceId": occurrence_id(identifier, occurrence_key),
        "observedAt": observation_record.get("observedAt"),
        "detail": observation_record.get("detail") or "",
        "evidence": evidence,
        "evidenceDigest": evidence_digest(evidence),
        "truncated": truncated,
        "cleared": _flag(observation_record.get("cleared")),
        "policy": policy,
    }


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
        "evidence": list(evidence), "cleared": bool(cleared),
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
    """The ledger and its outbox. Nothing here performs a network call."""

    def __init__(self, store, clock):
        self.store = store
        self.clock = clock

    # ------------------------------------------------------------------ targets

    def set_target(self, scope_key, tracker_ref) -> dict:
        """Where this scope's fault issues are filed, plus whatever was waiting for one.

        Backfilling matters: a fault raised before anybody configured a target is a real
        fault, and leaving its publication pointing nowhere would make configuring the target
        silently insufficient.
        """
        now = self.clock.iso()
        with self.store.transaction() as db:
            db.execute(
                "INSERT INTO fault_targets (scope_key, tracker_ref, recorded_at)"
                " VALUES (?,?,?) ON CONFLICT(scope_key) DO UPDATE SET"
                "   tracker_ref = excluded.tracker_ref, recorded_at = excluded.recorded_at",
                (scope_key, tracker_ref, now),
            )
            # Every pending write for this scope, not only the ones pointing nowhere. A
            # retarget left writes queued against the tracker the scope no longer uses, so
            # they would have been filed where nobody is looking any more.
            waiting = db.execute(
                "UPDATE fault_publications SET tracker_ref = ?, updated_at = ?"
                " WHERE state = ? AND (tracker_ref IS NULL OR tracker_ref != ?)"
                "   AND fault_id IN"
                "   (SELECT fault_id FROM fault_ledger WHERE scope_key = ?)",
                (tracker_ref, now, PENDING, tracker_ref, scope_key),
            ).rowcount
        return {"scopeKey": scope_key, "trackerRef": tracker_ref, "backfilled": waiting}

    def target_for(self, scope_key):
        row = self.store.one(
            "SELECT * FROM fault_targets WHERE scope_key = ?", (scope_key,))
        return dict(row) if row else None

    # ------------------------------------------------------------------ reading

    def get(self, identifier):
        row = self.store.one("SELECT * FROM fault_ledger WHERE fault_id = ?", (identifier,))
        return dict(row) if row else None

    def _row(self, identifier, db=None):
        reader = db.execute if db is not None else None
        row = (reader("SELECT * FROM fault_ledger WHERE fault_id = ?", (identifier,)).fetchone()
               if reader else
               self.store.one("SELECT * FROM fault_ledger WHERE fault_id = ?", (identifier,)))
        if row is None:
            raise FaultRefused(RefusalReason.FAULT_UNKNOWN, f"no fault {identifier!r}")
        return row

    def occurrences(self, identifier, *, limit=RENDERED_OCCURRENCES, newest=True) -> list:
        order = "DESC" if newest else "ASC"
        rows = self.store.all(
            f"SELECT * FROM fault_occurrences WHERE fault_id = ? ORDER BY rowid {order}"
            " LIMIT ?", (identifier, limit),
        )
        return [_occurrence(row) for row in rows]

    def remediations(self, identifier) -> list:
        return [dict(row) for row in self.store.all(
            "SELECT * FROM fault_remediations WHERE fault_id = ? ORDER BY rowid", (identifier,))]

    def snapshot(self, *, scope_key=None, state=None) -> dict:
        clauses, params = [], []
        if scope_key is not None:
            clauses.append("scope_key = ?")
            params.append(scope_key)
        if state is not None:
            clauses.append("state = ?")
            params.append(state)
        where = (" WHERE " + " AND ".join(clauses)) if clauses else ""
        rows = [dict(row) for row in self.store.all(
            "SELECT * FROM fault_ledger" + where + " ORDER BY rowid", tuple(params))]
        for row in rows:
            row["occurrences"] = self.occurrences(row["fault_id"])
            row["publications"] = [dict(entry) for entry in self.store.all(
                "SELECT publication_id, kind, trigger_key, state, attempts, external_ref,"
                "  last_error FROM fault_publications WHERE fault_id = ? ORDER BY rowid",
                (row["fault_id"],))]
        return {
            "schema": LEDGER_SCHEMA, "scopeKey": scope_key, "faults": rows,
            "limits": "derived from this store only. A queued publication is not an issue"
                      " anybody has written, and a confirmed one is not an issue anybody read",
        }

    # ------------------------------------------------------------------ recording

    def record(self, observation_record) -> dict:
        """Record one observation, converge it on its fault, and queue at most one write."""
        fact = read_observation(observation_record)
        now_iso = self.clock.iso()
        now = self.clock.now()
        identifier = fact["faultId"]
        with self.store.transaction() as db:
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
                row = db.execute(
                    "SELECT * FROM fault_ledger WHERE fault_id = ?", (identifier,)).fetchone()
            recorded = db.execute(
                "INSERT OR IGNORE INTO fault_occurrences (occurrence_id, fault_id,"
                "  occurrence_key, severity, cleared, detail, evidence, evidence_digest,"
                "  truncated, observed_at, recorded_at, recorded_ts)"
                " VALUES (?,?,?,?,?,?,?,?,?,?,?,?)",
                (fact["occurrenceId"], identifier, fact["occurrenceKey"], fact["severity"],
                 1 if fact["cleared"] else 0, fact["detail"],
                 json.dumps(fact["evidence"], ensure_ascii=False, sort_keys=True),
                 fact["evidenceDigest"], 1 if fact["truncated"] else 0, fact["observedAt"],
                 now_iso, now),
            ).rowcount == 1
            if not recorded:
                # The same underlying fact, read again. Nothing about the fault changed, so
                # nothing is queued: this is the case a repeated sweep produces on every tick.
                return {"faultId": identifier, "recorded": False, "state": row["state"],
                        "occurrenceCount": row["occurrence_count"],
                        "reason": "this occurrence was already recorded",
                        "publication": None}
            db.execute(
                "INSERT INTO fault_timeline (fault_id, cycle, kind, ref_id, detail,"
                "  recorded_at, recorded_ts) VALUES (?,?,?,?,?,?,?)",
                (identifier, row["cycle"], CLEARED if fact["cleared"] else OCCURRENCE,
                 fact["occurrenceId"], fact["detail"], now_iso, now),
            )
            # Counted forward rather than recounted from the evidence rows. fault-prune
            # removes evidence an operator no longer needs to read, and a recount would let
            # that lower the number of occurrences this fault is known to have had.
            count = row["occurrence_count"] + 1
            severity = (fact["severity"]
                        if SEVERITY_RANK[fact["severity"]] > SEVERITY_RANK[row["severity"]]
                        else row["severity"])
            escalated = severity != row["severity"]
            policy = policy_for(fact["faultClass"], severity)
            suppression = self._suppression(db, identifier, policy, now)
            opened = db.execute(
                "SELECT publication_id FROM fault_publications"
                " WHERE fault_id = ? AND kind = ?", (identifier, OPEN_RECORD)).fetchone()
            state, cycle, reopened, trigger_key = _transition(
                row["state"], row["cycle"], cleared=fact["cleared"],
                publishable=suppression["publish"], escalated=escalated, severity=severity,
                published=bool(row["external_ref"]) or opened is not None,
            )
            db.execute(
                "UPDATE fault_ledger SET state = ?, cycle = ?, severity = ?,"
                "  occurrence_count = ?, reopen_count = reopen_count + ?, detail = ?,"
                "  suppression = ?, last_seen_at = ?, cleared_at = ?, resolved_at = ?,"
                "  updated_at = ?, scope = ?, scope_key = ?"
                " WHERE fault_id = ?",
                (state, cycle, severity, count, 1 if reopened else 0,
                 fact["detail"] or row["detail"],
                 json.dumps(suppression, ensure_ascii=False, sort_keys=True), now_iso,
                 # Reset by a real occurrence, so a fault that recovers, happens again and
                 # recovers again is cleared once per episode rather than once ever.
                 now_iso if fact["cleared"] else None,
                 None if state != RESOLVED else row["resolved_at"], now_iso,
                 json.dumps(fact["scope"], ensure_ascii=False, sort_keys=True),
                 fact["scopeKey"], identifier),
            )
            if fact["scopeKey"] != row["scope_key"]:
                # The fault moved. A write queued against the old scope's tracker would be
                # filed in a project this fault no longer belongs to, and one queued while no
                # tracker was configured would never become eligible.
                moved = db.execute(
                    "SELECT tracker_ref FROM fault_targets WHERE scope_key = ?",
                    (fact["scopeKey"],)).fetchone()
                db.execute(
                    "UPDATE fault_publications SET tracker_ref = ?, updated_at = ?"
                    " WHERE fault_id = ? AND state = ?",
                    (moved["tracker_ref"] if moved else None, now_iso, identifier, PENDING))
            publication = None
            if trigger_key is not None:
                publication = self._enqueue(db, identifier, trigger_key, now_iso,
                                            clears=policy["clears"])
            fresh = db.execute(
                "SELECT * FROM fault_ledger WHERE fault_id = ?", (identifier,)).fetchone()
        return {"faultId": identifier, "recorded": True, "state": fresh["state"],
                "cycle": fresh["cycle"], "severity": fresh["severity"],
                "occurrenceCount": count, "suppression": suppression,
                "publication": publication}

    def _suppression(self, db, identifier, policy, now) -> dict:
        """Whether this fault has earned a Linear record, counted inside the window.

        The window is measured on the injected clock, which is the clock the delivery backoff
        already schedules from. The ORDER of events is a separate question and is never asked
        of a clock.
        """
        if policy["threshold"] is None:
            return {"publish": False, "threshold": None, "window": policy["window"],
                    "counted": None,
                    "reason": f"a {policy['severity']} is recorded for an operator and never"
                              f" filed"}
        # From the timeline, which is never pruned, so removing evidence an operator has
        # finished reading cannot change what the next observation decides.
        counted = db.execute(
            "SELECT COUNT(*) AS n FROM fault_timeline"
            " WHERE fault_id = ? AND kind = ? AND recorded_ts >= ?",
            (identifier, OCCURRENCE, now - policy["window"]),
        ).fetchone()["n"]
        publish = counted >= policy["threshold"]
        return {
            "publish": publish, "threshold": policy["threshold"], "window": policy["window"],
            "counted": counted,
            "reason": (f"{counted} observation(s) inside {int(policy['window'])}s reached the"
                       f" threshold of {policy['threshold']}") if publish else
                      (f"{counted} observation(s) inside {int(policy['window'])}s is under the"
                       f" threshold of {policy['threshold']}"),
        }

    # ------------------------------------------------------------------ remediation

    def record_fix(self, identifier, *, ref, detail="") -> dict:
        """Attach the change that is supposed to have fixed this. It resolves nothing.

        Deliberately separate from resolve(): a fix is a claim about a change, and whether the
        change worked is a different observation that has not been made yet.
        """
        if not _named(ref):
            raise FaultRefused(RefusalReason.FAULT_OBSERVATION_MALFORMED,
                               "a fix names the change it is - a pull request, a commit")
        return self._remediate(identifier, kind=FIX, ref=ref, detail=detail,
                               allowed=(OBSERVED, OPEN, FIX_PENDING), next_state=FIX_PENDING,
                               trigger=TRIGGER_FIX)

    def record_reverification(self, identifier, *, method, ref, outcome, detail="") -> dict:
        """State that the fault was looked for after the fix and what was found.

        Structured rather than a sentence, because resolve() has to be able to tell a check
        that ran from a claim that one did.
        """
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

    def _remediate(self, identifier, *, kind, ref, allowed, next_state, trigger, method=None,
                   outcome=None, detail="") -> dict:
        now = self.clock.iso()
        with self.store.transaction() as db:
            row = self._row(identifier, db)
            if row["state"] not in allowed:
                raise FaultRefused(
                    RefusalReason.FAULT_STATE_CONFLICT,
                    f"a {kind} is recorded on a fault that is {allowed}, and this one is"
                    f" {row['state']}",
                )
            # The fix this remediation FOLLOWS is part of its identity. Without it, running
            # the same command again after a second fix produced the first run's id,
            # INSERT OR IGNORE dropped it, and resolve() then refused forever against a
            # verification that predated the fix it was supposed to verify. Recording the
            # identical check twice with no fix in between still converges, which is the
            # idempotence worth keeping.
            # Only a REVERIFICATION's meaning depends on which fix it follows. A fix's own
            # identity must not, or recording the same fix twice would write two rows.
            after = db.execute(
                "SELECT MAX(seq) AS seq FROM fault_timeline"
                " WHERE fault_id = ? AND cycle = ? AND kind = ?",
                (identifier, row["cycle"], FIX)).fetchone()["seq"] \
                if kind == REVERIFICATION else None
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
                (identifier, row["cycle"], kind, remediation_id, ref, now,
                 self.clock.now()),
            )
            db.execute(
                "UPDATE fault_ledger SET state = ?, updated_at = ? WHERE fault_id = ?",
                (next_state, now, identifier),
            )
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
        """Close the loop, or refuse and say which half is missing.

        Three refusals, because a caller's next action differs for each: there is no fix, the
        only verification predates the fix it claims to verify, or the fault happened again
        after that verification and is therefore not fixed whatever the check said.
        """
        now = self.clock.iso()
        with self.store.transaction() as db:
            row = self._row(identifier, db)
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
            if verification is not None:
                # The NEWEST one answers, and it has to have found the fault gone. A later
                # check that reported it still happening is the most recent thing anybody
                # knows, and resolving over it would be the false report this refuses.
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
                "UPDATE fault_ledger SET state = ?, resolved_at = ?, updated_at = ?"
                " WHERE fault_id = ?", (RESOLVED, now, now, identifier))
            publication = self._enqueue(db, identifier, f"{TRIGGER_RESOLVE}:{cycle}", now)
        return {"faultId": identifier, "state": RESOLVED, "resolved": True, "cycle": cycle,
                "publication": publication}

    def prune(self, identifier, *, keep) -> dict:
        """Drop the middle of a fault's occurrence history, as an explicit operator act.

        Not automatic and not silent. A store that discards its own evidence on a schedule is
        worse than a large one, so this records in the journal how many rows it removed and
        keeps the newest, which are the ones an operator is reading.
        """
        if keep < 1:
            raise FaultRefused(RefusalReason.FAULT_OBSERVATION_MALFORMED, "keep at least one")
        now = self.clock.iso()
        with self.store.transaction() as db:
            self._row(identifier, db)
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

    # ------------------------------------------------------------------ publication

    def _enqueue(self, db, identifier, trigger_key, now, *, remediation=None, clears="") -> dict:
        """Queue at most one write for this reason, inside the caller's transaction.

        The kind is decided by whether this fault already owns an issue. The first publication
        creates it; every later one is a comment on it, which is what keeps one fault to one
        issue for its whole life.
        """
        row = db.execute(
            "SELECT * FROM fault_ledger WHERE fault_id = ?", (identifier,)).fetchone()
        # At most ONE create per fault for its whole life. Choosing the kind from external_ref
        # alone was not enough: between queuing the create and confirming it, the ledger has
        # no reference yet, so a fix or a resolve arriving in that window queued a SECOND
        # create under its own trigger and the fault would have owned two issues.
        opened = db.execute(
            "SELECT publication_id FROM fault_publications WHERE fault_id = ? AND kind = ?",
            (identifier, OPEN_RECORD)).fetchone()
        if trigger_key != TRIGGER_OPEN and not row["external_ref"] and opened is None:
            # This fault has never earned a Linear record: the threshold never let it open
            # one. Recording a fix or a resolution against it is useful locally and must not
            # be the thing that files the issue suppression already refused - a notice would
            # otherwise reach Linear through the back door.
            return {"publicationId": None, "kind": None, "trigger": trigger_key,
                    "queued": False, "awaitingTarget": target_missing(db, row),
                    "awaitingRecord": False,
                    "reason": "this fault has never been published, so nothing is written for"
                              " it; suppression decides that, not a remediation"}
        kind = OPEN_RECORD if (not row["external_ref"] and opened is None) else APPEND_COMMENT
        publication = publication_id(identifier, kind, trigger_key)
        target = db.execute(
            "SELECT tracker_ref FROM fault_targets WHERE scope_key = ?",
            (row["scope_key"],)).fetchone()
        occurrences = [_occurrence(entry) for entry in db.execute(
            "SELECT * FROM fault_occurrences WHERE fault_id = ? ORDER BY rowid DESC LIMIT ?",
            (identifier, RENDERED_OCCURRENCES)).fetchall()]
        summary = render_summary(row, trigger_key=trigger_key, occurrences=occurrences,
                                 remediation=remediation, clears=clears,
                                 publication=publication)
        queued = db.execute(
            "INSERT OR IGNORE INTO fault_publications (publication_id, fault_id, kind,"
            "  trigger_key, cycle, tracker_ref, external_ref, summary, identity_digest,"
            "  state, attempts, created_at, updated_at) VALUES (?,?,?,?,?,?,?,?,?,?,0,?,?)",
            (publication, identifier, kind, trigger_key, row["cycle"],
             target["tracker_ref"] if target else None, row["external_ref"], summary,
             identity_digest(identifier, kind, trigger_key, row["cycle"]), PENDING, now, now),
        ).rowcount == 1
        return {
            "publicationId": publication, "kind": kind, "trigger": trigger_key,
            "queued": queued,
            "awaitingTarget": target is None,
            "awaitingRecord": kind == APPEND_COMMENT and not row["external_ref"],
            "reason": ("queued" if queued else "this reason was already queued") + (
                "" if target is not None else
                f"; no tracker is configured for {row['scope_key']}, so it waits rather than"
                f" being filed somewhere guessed"),
        }

    def next(self, *, limit=4, now=None) -> list:
        """The publications a caller may act on. An issued or uncertain row is never among them."""
        moment = self.clock.now() if now is None else now
        # A comment on an issue that does not exist yet is not work anybody can do. It waits
        # here rather than being handed out and failing at the connector.
        rows = self.store.all(
            "SELECT p.* FROM fault_publications p"
            "  JOIN fault_ledger f ON f.fault_id = p.fault_id"
            " WHERE p.state = ? AND p.tracker_ref IS NOT NULL"
            "   AND (p.kind = ? OR f.external_ref IS NOT NULL)"
            "   AND (p.next_attempt_at IS NULL OR p.next_attempt_at <= ?)"
            " ORDER BY p.rowid LIMIT ?", (PENDING, OPEN_RECORD, moment, limit))
        return [_publication(row) for row in rows]

    def claim(self, publication, *, owner, now=None) -> dict:
        """A lease plus a per-claim token, which is what fences complete and fail."""
        moment = self.clock.now() if now is None else now
        token = secrets.token_hex(8)
        with self.store.transaction() as db:
            row = self._publication_row(db, publication)
            if row["state"] != PENDING:
                raise FaultRefused(
                    RefusalReason.FAULT_NOT_CLAIMABLE,
                    f"publication {publication} is {row['state']}" + (
                        ". An uncertain create is not reclaimed; reconcile it by reporting"
                        " what you observed" if row["state"] == UNCERTAIN else ""),
                )
            if row["tracker_ref"] is None:
                raise FaultRefused(
                    RefusalReason.FAULT_NOT_CLAIMABLE,
                    "no tracker is configured for this fault's scope yet",
                )
            if row["next_attempt_at"] is not None and row["next_attempt_at"] > moment:
                # next() already refuses to offer this row. Checking it here too, because a
                # caller holding an identifier can claim without asking the queue, and a
                # backoff only one of the two paths honours is not a backoff.
                raise FaultRefused(
                    RefusalReason.FAULT_NOT_CLAIMABLE,
                    f"this publication backs off until {row['next_attempt_at']}",
                )
            fault = db.execute("SELECT external_ref FROM fault_ledger WHERE fault_id = ?",
                               (row["fault_id"],)).fetchone()
            if row["kind"] == APPEND_COMMENT and not fault["external_ref"]:
                raise FaultRefused(
                    RefusalReason.FAULT_NOT_CLAIMABLE,
                    "this fault owns no issue yet, so there is nothing to comment on. Its"
                    " create is queued and this waits for it",
                )
            db.execute(
                "UPDATE fault_publications SET state = ?, claim_token = ?, lease_owner = ?,"
                "  lease_until = ?, attempts = attempts + 1, updated_at = ?"
                " WHERE publication_id = ?",
                (CLAIMED, token, owner, moment + LEASE_SECONDS, self.clock.iso(), publication),
            )
        return {"publicationId": publication, "claimToken": token, "owner": owner,
                "leaseUntil": moment + LEASE_SECONDS}

    def operation(self, publication, *, claim_token) -> dict:
        """The exact thing to execute, and the point after which a retry is not automatic.

        Handing this out marks the row issued. That is the whole mechanism behind the one
        promise this module makes about creates: from here on, only somebody who has LOOKED
        can move the row, because a create whose response was lost and a create that never
        happened are the same observation from in here.
        """
        now = self.clock.iso()
        with self.store.transaction() as db:
            row = self._publication_row(db, publication)
            if row["state"] != CLAIMED:
                raise FaultRefused(
                    RefusalReason.FAULT_NOT_CLAIMABLE,
                    f"an operation is handed out for a claimed publication; this one is"
                    f" {row['state']}",
                )
            if row["claim_token"] != claim_token:
                raise FaultRefused(RefusalReason.FAULT_CLAIM_STALE,
                                   "this claim token is not the current one")
            db.execute(
                "UPDATE fault_publications SET state = ?, issued_at = ?, updated_at = ?"
                " WHERE publication_id = ?", (ISSUED, now, now, publication))
            fault = db.execute(
                "SELECT * FROM fault_ledger WHERE fault_id = ?", (row["fault_id"],)).fetchone()
            # The row's own cycle, not the ledger's current one: this write describes the
            # moment it was queued, and its identity digest was computed then.
            block = render_block({**dict(row), "product": fault["product"],
                                  "fault_class": fault["fault_class"]})
        return {
            "publicationId": publication,
            "kind": row["kind"],
            "trackerRef": row["tracker_ref"],
            # From the ledger, not from the row: a comment queued before the issue existed
            # carries no reference of its own, and the ledger is where the issue this fault
            # owns is recorded once its create confirmed.
            "externalRef": fault["external_ref"] or row["external_ref"],
            "title": title_for(fault),
            "block": block,
            "startMarker": start_marker(publication),
            "endMarker": end_marker(publication),
            "identityDigest": row["identity_digest"],
            "protocol": _protocol(row["kind"]),
            "note": "the connector's create takes no idempotency key, so a create can succeed"
                    " and lose its response. This row is now issued: if you cannot report an"
                    " outcome it becomes uncertain, and no second create is made until"
                    " somebody reports what they observed.",
        }

    def reconcile(self, publication, observed_text, *, searched=False) -> dict:
        """Did this write already land? Answered from what was observed, before rewriting."""
        now = self.clock.iso()
        with self.store.transaction() as db:
            row = self._publication_row(db, publication)
            found = read_block(observed_text, publication)
            if found["duplicated"]:
                return {"publicationId": publication, "outcome": "duplicate",
                        "state": row["state"],
                        "detail": "more than one block for this publication is present; that is"
                                  " not one unique record and must be repaired before"
                                  " confirming"}
            if found["found"]:
                if found["problems"]:
                    return {"publicationId": publication, "outcome": "malformed",
                            "state": row["state"], "problems": found["problems"],
                            "detail": "a partial block is not proof that nothing landed, and"
                                      " must not be written over"}
                return {"publicationId": publication, "outcome": "present",
                        "state": row["state"],
                        "detail": "this write already landed; complete from this observation"
                                  " rather than writing again"}
            if not searched:
                return {"publicationId": publication, "outcome": "absent_unattested",
                        "state": row["state"],
                        "detail": "no block was observed, and nobody attested that the search"
                                  " covered where it would be. A negative read is not proof of"
                                  " absence unless somebody says what they read"}
            if row["state"] in (ISSUED, UNCERTAIN):
                db.execute(
                    "UPDATE fault_publications SET state = ?, claim_token = NULL,"
                    "  lease_owner = NULL, lease_until = NULL, updated_at = ?"
                    " WHERE publication_id = ?", (PENDING, now, publication))
            return {"publicationId": publication, "outcome": "absent", "state": PENDING,
                    "detail": "the attested search found nothing, so one further write is"
                              " permitted"}

    def complete(self, publication, *, readback, claim_token=None, external_ref=None,
                 now=None) -> dict:
        """Confirmed against this publication's own block, by exact field.

        A claim token is required while the row is claimed or issued, and NOT required from
        uncertain: there the proof is the readback itself, and demanding a lease nobody holds
        any more would leave a write that provably landed permanently unconfirmable.
        """
        moment = self.clock.iso()
        with self.store.transaction() as db:
            row = self._publication_row(db, publication)
            if row["state"] == CONFIRMED:
                return _publication(row) | {"confirmed": False,
                                            "reason": "already confirmed"}
            if row["state"] not in (CLAIMED, ISSUED, UNCERTAIN):
                # PENDING in particular. A reconciliation that attested the block was ABSENT
                # returns the row to pending, and accepting a readback captured before that
                # would confirm a write somebody has already established is not there.
                raise FaultRefused(
                    RefusalReason.FAULT_STATE_CONFLICT,
                    f"a {row['state']} publication has no write outstanding to confirm;"
                    f" claim it and write it again",
                )
            if row["state"] in (CLAIMED, ISSUED) and row["claim_token"] != claim_token:
                raise FaultRefused(RefusalReason.FAULT_CLAIM_STALE,
                                   "this claim token is not the current one")
            found = read_block(readback, publication)
            fault = db.execute(
                "SELECT * FROM fault_ledger WHERE fault_id = ?", (row["fault_id"],)).fetchone()
            problems = _block_mismatch(row, fault, found)
            if problems:
                raise FaultRefused(RefusalReason.FAULT_READBACK_MISMATCH, "; ".join(problems))
            reference = external_ref or row["external_ref"]
            if row["kind"] == OPEN_RECORD and not reference:
                # Confirming a create without the identifier it created leaves the ledger
                # owning no issue, and every later comment then waits forever for a reference
                # that will never arrive. The write landed; what is missing is the caller
                # saying WHAT it landed as.
                raise FaultRefused(
                    RefusalReason.FAULT_READBACK_MISMATCH,
                    "a confirmed create must name the issue it created, or every later"
                    " comment on this fault is queued against nothing",
                )
            db.execute(
                "UPDATE fault_publications SET state = ?, external_ref = ?, confirmed_at = ?,"
                "  claim_token = NULL, lease_owner = NULL, lease_until = NULL,"
                "  last_error = NULL, updated_at = ? WHERE publication_id = ?",
                (CONFIRMED, reference, moment, moment, publication))
            if row["kind"] == OPEN_RECORD and reference:
                # The issue this fault now owns. Every later publication is a comment on it,
                # which is what makes "one fault, one issue" survive a restart.
                db.execute(
                    "UPDATE fault_ledger SET external_ref = ?, published_at = ?,"
                    "  updated_at = ? WHERE fault_id = ? AND external_ref IS NULL",
                    (reference, moment, moment, row["fault_id"]))
            fresh = self._publication_row(db, publication)
        return _publication(fresh) | {"confirmed": True}

    def fail(self, publication, *, claim_token, error, now=None) -> dict:
        """Record that this write did not report success, and decide what may follow it.

        A row that was only claimed never reached the connector, so it goes back to pending
        with backoff. A row that was ISSUED may have landed, and that is the whole difference:
        it becomes uncertain, and stays there until somebody reports what they observed.
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
                state = UNCERTAIN
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
        return {"publicationId": publication, "state": state, "error": str(error),
                "detail": "an issued write may have landed, so it is uncertain rather than"
                          " retried" if state == UNCERTAIN else ""}

    def expire_leases(self, *, now=None) -> dict:
        """What an expired lease means, which is not the same answer for both states.

        A claimed row never reached the connector, so it is safe to offer again. An issued one
        may have landed, and offering it again is exactly how one fault becomes two issues.
        """
        moment = self.clock.now() if now is None else now
        stamp = self.clock.iso()
        with self.store.transaction() as db:
            released = db.execute(
                "UPDATE fault_publications SET state = ?, claim_token = NULL,"
                "  lease_owner = NULL, lease_until = NULL, updated_at = ?"
                " WHERE state = ? AND lease_until IS NOT NULL AND lease_until <= ?",
                (PENDING, stamp, CLAIMED, moment)).rowcount
            uncertain = db.execute(
                "UPDATE fault_publications SET state = ?, claim_token = NULL,"
                "  lease_owner = NULL, lease_until = NULL, updated_at = ?,"
                "  last_error = COALESCE(last_error, 'the lease expired after the write was"
                " issued')"
                " WHERE state = ? AND lease_until IS NOT NULL AND lease_until <= ?",
                (UNCERTAIN, stamp, ISSUED, moment)).rowcount
        return {"released": released, "uncertain": uncertain}

    def retry(self, publication) -> dict:
        """An operator's decision to offer a failed row again. Never reaches uncertain."""
        stamp = self.clock.iso()
        with self.store.transaction() as db:
            row = self._publication_row(db, publication)
            if row["state"] != FAILED:
                raise FaultRefused(
                    RefusalReason.FAULT_NOT_CLAIMABLE,
                    f"retry offers a failed publication again; this one is {row['state']}" + (
                        ". An uncertain write is reconciled, not retried"
                        if row["state"] == UNCERTAIN else ""))
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


def target_missing(db, row):
    """Whether this fault's scope has a tracker, asked inside the caller's transaction."""
    return db.execute("SELECT tracker_ref FROM fault_targets WHERE scope_key = ?",
                      (row["scope_key"],)).fetchone() is None


def _protocol(kind) -> list:
    if kind == OPEN_RECORD:
        return [
            "search the tracker for this publication's start marker BEFORE creating anything",
            "marker found: the create already landed. Complete from that observation and do"
            " NOT create again",
            "marker absent: create one issue whose description carries this block verbatim",
            "read the created issue back and pass its full text to complete, with its"
            " identifier as the external reference",
            "response lost, or you cannot tell: report failure. The row becomes uncertain and"
            " no second create is made until somebody reconciles it with what they observed",
        ]
    return [
        "read the issue's comments and look for this publication's start marker",
        "marker found: this comment already landed. Complete from that observation",
        "marker absent: add one comment carrying this block verbatim",
        "read it back and pass the comment text to complete",
    ]


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


def _transition(state, cycle, *, cleared, publishable, escalated, severity, published=False):
    """The next state, and the one reason a write is owed. Returns no reason for most calls.

    Most observations change nothing anybody has to be told about, and saying so is the point:
    a ledger that queued a write per observation would be the spray it exists to prevent.
    """
    if cleared:
        # Clearing withdraws a fault nobody was told about - including one a locally recorded
        # fix moved to fix_pending, which is still a fault no Linear record carries. It does
        # NOT close a published one: the cause may have stopped without having been fixed,
        # and the closed loop is what decides that.
        if published or state in (RESOLVED, WITHDRAWN):
            return state, cycle, False, None
        return WITHDRAWN, cycle, False, None
    if state == WITHDRAWN:
        state = OBSERVED
    if state == RESOLVED:
        return OPEN, cycle + 1, True, f"{TRIGGER_REOPEN}:{cycle + 1}"
    if state == FIX_PENDING:
        if not published:
            # Never filed, so there is no record to tell that the fix did not hold. It goes
            # back to open and the threshold decides, exactly as it would have.
            return OPEN, cycle, False, (TRIGGER_OPEN if publishable else None)
        return OPEN, cycle, False, f"{TRIGGER_RECUR}:{cycle}"
    if state == OPEN:
        if not published and publishable:
            # Open but never filed: a fix recorded before the threshold moved it here, and
            # the OPEN branch alone would have left it forever open with no record. Reaching
            # the threshold is what opens the record, whenever that happens.
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
    return record
