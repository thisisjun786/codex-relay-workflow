"""Completion checks: does what a subject claims match what was observed?

A Linear issue marked Done, a merged pull request and an ended session each claim something.
This module compares the claim with the evidence a credential holder read back, inside the
bounds of what can be observed, and says which checks agree, which contradict, and which could
not be established. It never changes the subject: a completion check proposes no state change of
any kind, so a legitimate Done stays exactly as it is and a false one is flagged, not cancelled.

Three rules keep the verdicts honest.

A requirement is required only when the reading says so. Deployment is never assumed mandatory,
and an unknown requirement is unverified rather than guessed either way.

Unobservable is not absent. A result nobody could read is unverified; only a result that was
looked for and not found is a mismatch.

An exception counts only when read back. A follow-up is an exception for one check of one
subject: a different, open issue of the product that lists this subject and this check among
what it took over. An approved scope reduction needs its approval and its reference. Anything
else is exception_unverified, and the check stays a mismatch.

This module is pure. The bindings, the open mismatches and the recurrences a verdict depends on
are passed in, so the same reading and context always give the same verdict.
"""

from . import products

READING_SCHEMA = "completion-reading/1"
CLAIMS = ("linearDone", "prMerged", "sessionEnded")
# Which claim makes which check applicable.
CLAIM_FOR = {"acceptance": "linearDone", "install": "prMerged", "realUse": "prMerged",
             "handoff": "sessionEnded"}
ACCEPTANCE_OBSERVED = ("passed", "failed", "absent", "unobservable")
RESULT_OBSERVED = ("present", "absent", "unobservable")
UNKNOWN = "unknown"
EXCEPTION_KINDS = ("scope_reduction", "follow_up")

CONSISTENT = "consistent"
MISMATCH = "mismatch"
UNVERIFIED = "unverified"
EXCEPTED = "excepted"
EXCEPTION_UNVERIFIED = "exception_unverified"
NOT_APPLICABLE = "not_applicable"
REQUIREMENT_CHANGED = "requirement_changed_without_approval"
# The verdicts that keep a subject flagged. A claimed exception nobody could verify and a
# requirement dropped after a mismatch both leave the mismatch where it was.
OPEN_VERDICTS = (MISMATCH, EXCEPTION_UNVERIFIED, REQUIREMENT_CHANGED)

READING_KEYS = ("schema", "product", "subject", "claims", "requires", "observed", "evidence",
                "exceptions", "origin", "observedAt")


def _ref(value, name):
    products._closed(value, ("ref", "source"), name)
    return {"ref": products._text(value.get("ref"), f"{name}.ref"),
            "source": products._text(value.get("source"), f"{name}.source")}


def read_reading(record) -> dict:
    """One completion reading, validated. Missing requirements read unknown and missing
    observations unobservable, because an absent field is not evidence either way."""
    products._object(record, READING_SCHEMA, READING_KEYS, "a completion reading")
    claims = record.get("claims") or {}
    products._closed(claims, CLAIMS, "claims")
    requires = record.get("requires") or {}
    products._closed(requires, products.CHECKS, "requires")
    observed = record.get("observed") or {}
    products._closed(observed, products.CHECKS, "observed")
    evidence = record.get("evidence") or {}
    products._closed(evidence, products.CHECKS, "evidence")
    exceptions = record.get("exceptions") or []
    if not isinstance(exceptions, list):
        products.malformed("exceptions is a list")
    reading = {
        "schema": READING_SCHEMA,
        "product": products._key(record.get("product"), "product"),
        "subject": products._issue(record.get("subject"), "subject"),
        "claims": {claim: products._flag(claims.get(claim), f"claims.{claim}", default=False)
                   for claim in CLAIMS},
        "requires": {}, "observed": {}, "evidence": {}, "exceptions": [],
        "origin": products._choice(record.get("origin") or products.OBSERVED, products.ORIGINS,
                                   "origin"),
        "observedAt": products._text(record.get("observedAt"), "observedAt", optional=True,
                                     limit=64),
    }
    for check in products.CHECKS:
        required = requires.get(check, UNKNOWN)
        if required not in (True, False, UNKNOWN):
            products.malformed(f"requires.{check} is true, false or {UNKNOWN!r}")
        reading["requires"][check] = required
        vocabulary = ACCEPTANCE_OBSERVED if check == "acceptance" else RESULT_OBSERVED
        reading["observed"][check] = products._choice(
            observed.get(check, "unobservable"), vocabulary, f"observed.{check}")
        entry = evidence.get(check) or {}
        products._closed(entry, ("fix", "verification"), f"evidence.{check}")
        reading["evidence"][check] = {
            key: _ref(entry[key], f"evidence.{check}.{key}")
            for key in ("fix", "verification") if entry.get(key) is not None}
    for index, exception in enumerate(exceptions):
        name = f"exceptions[{index}]"
        products._closed(exception, ("check", "kind", "ref", "approvedBy"), name)
        reading["exceptions"].append({
            "check": products._choice(exception.get("check"), products.CHECKS, f"{name}.check"),
            "kind": products._choice(exception.get("kind"), EXCEPTION_KINDS, f"{name}.kind"),
            "ref": products._text(exception.get("ref"), f"{name}.ref"),
            "approvedBy": products._text(exception.get("approvedBy"), f"{name}.approvedBy",
                                         optional=True),
        })
    return reading


def _exception(reading, check, bindings):
    """(verdict, reason) for the exception claimed on this check, or None when there is none."""
    claimed = [e for e in reading["exceptions"] if e["check"] == check]
    if not claimed:
        return None
    exception = claimed[-1]
    if exception["kind"] == "scope_reduction":
        if exception["approvedBy"]:
            return EXCEPTED, (f"scope reduction approved by {exception['approvedBy']}"
                              f" ({exception['ref']})")
        return EXCEPTION_UNVERIFIED, "a scope reduction without a named approval"
    ref, subject = exception["ref"], reading["subject"]
    if ref == subject:
        return EXCEPTION_UNVERIFIED, "a subject cannot be its own follow-up"
    binding = bindings.get(ref)
    if binding is None or binding["kind"] != "issue":
        return EXCEPTION_UNVERIFIED, f"{ref} is not a bound issue of {reading['product']}"
    if binding["state"] not in products.OPEN_STATES:
        return EXCEPTION_UNVERIFIED, f"{ref} is {binding['state']}, so it carries nothing forward"
    took = [entry for entry in binding["followUpOf"] if entry["issue"] == subject]
    if not any(check in entry["checks"] for entry in took):
        return EXCEPTION_UNVERIFIED, (f"{ref} does not record taking over {check} of {subject}")
    return EXCEPTED, f"{ref} took over {check} of {subject}"


def _check(reading, check, context):
    if not reading["claims"][CLAIM_FOR[check]]:
        return NOT_APPLICABLE, f"nothing claims {CLAIM_FOR[check]}"
    excepted = _exception(reading, check, context["bindings"])
    if excepted is not None:
        return excepted
    required = reading["requires"][check]
    if required is False:
        if check in context["openMismatches"]:
            return REQUIREMENT_CHANGED, ("the requirement was dropped after a mismatch with no"
                                         " approved exception, so the mismatch stands")
        return CONSISTENT, "not required"
    if required == UNKNOWN:
        return UNVERIFIED, "whether this is required is unknown"
    observed = reading["observed"][check]
    if observed == "unobservable":
        return UNVERIFIED, "the result could not be observed"
    if observed in ("failed", "absent"):
        return MISMATCH, f"required and {observed}"
    return CONSISTENT, f"required and {observed}"


def evaluate(reading, context) -> dict:
    """Verdicts for one reading. context: {bindings: {ref: binding}, openMismatches: [check],
    recurrences: [{faultId, reason}]} - the only other facts a verdict may depend on."""
    checks = []
    for check in products.CHECKS:
        verdict, reason = _check(reading, check, context)
        entry = {"check": check, "verdict": verdict, "reason": reason}
        evidence = reading["evidence"][check]
        if verdict == CONSISTENT and check in context["openMismatches"]:
            # A mismatch that has become consistent closes only on a fix and a verification
            # somebody can point to; the verdict says which of the two is still missing.
            missing = [key for key in ("fix", "verification") if key not in evidence]
            entry["closure"] = {"ready": not missing, "missing": missing, **evidence}
        if verdict == EXCEPTED and check in context["openMismatches"]:
            entry["closure"] = {"ready": True, "missing": [], "exception": reason}
        checks.append(entry)
    recurrences = [{"check": "recurrence", "verdict": MISMATCH, "faultId": item["faultId"],
                    "reason": item["reason"]} for item in context["recurrences"]]
    verdicts = {entry["verdict"] for entry in checks + recurrences}
    if verdicts & set(OPEN_VERDICTS):
        overall = MISMATCH
    elif UNVERIFIED in verdicts:
        overall = UNVERIFIED
    else:
        overall = CONSISTENT
    return {"subject": reading["subject"], "product": reading["product"],
            "origin": reading["origin"], "verdict": overall, "checks": checks,
            "recurrences": recurrences}
