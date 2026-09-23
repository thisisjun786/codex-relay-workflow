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

An exception counts only when read back, and only for a check that would otherwise stay open or
unverified. A follow-up is an exception for one check of one subject: a different, open issue of
the product that lists this subject and this check among what it took over. An approved scope
reduction needs its approval and its reference. Anything else is exception_unverified, and the
check stays a mismatch. A check that passed is consistent whatever exception is claimed on it.

evaluate() is pure: the bindings, the open mismatches and the recurrences a verdict depends on
are passed in, so the same reading and context always give the same verdict. check() gathers
that context from this store and the ledger and records the verdicts through ledger_port: a
mismatch as a re-verification demand adopted by the subject issue itself, an unverified check
under its own notice identity that never files, and a closure only on the evidence D21 names.
It queues no update of any kind for the subject; the ledger's own rules decide its comments.
"""

import hashlib

from . import ledger_port, products, routes
from .errors import RefusalReason

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
# A claim (Done, merged, ended) withdrawn while its check's mismatch is open: nothing closed it,
# so the mismatch stands, as it does when the requirement is dropped.
CLAIM_WITHDRAWN = "claim_withdrawn_without_closure"
# The overall verdict when every check now agrees but an open mismatch still lacks the fix and
# verification references that would close it: nothing is wrong any more, and nothing is closed.
CLOSURE_PENDING = "closure_pending"
# The verdicts that keep a subject flagged. A claimed exception nobody could verify and a
# requirement dropped after a mismatch both leave the mismatch where it was.
OPEN_VERDICTS = (MISMATCH, EXCEPTION_UNVERIFIED, REQUIREMENT_CHANGED, CLAIM_WITHDRAWN)

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
        "product": products._product(record.get("product"), "product"),
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
    claim = CLAIM_FOR[check]
    if reading["claims"][claim]:
        verdict, reason = _observed(reading, check, context)
    elif check in context["openMismatches"]:
        # Withdrawing the claim answers nothing: a mismatch closes only on a fix and its
        # verification, which the claim made again with the evidence will carry, or on an
        # approved exception.
        verdict, reason = CLAIM_WITHDRAWN, (f"{claim} is no longer claimed, and nothing closed"
                                            f" the open mismatch, so it stands")
    else:
        return NOT_APPLICABLE, f"nothing claims {claim}"
    if verdict not in (MISMATCH, REQUIREMENT_CHANGED, CLAIM_WITHDRAWN, UNVERIFIED):
        return verdict, reason
    excepted = _exception(reading, check, context["bindings"])
    if excepted is None:
        return verdict, reason
    if excepted[0] == EXCEPTION_UNVERIFIED and verdict == UNVERIFIED:
        # Nothing was found wrong, so an exception nobody could verify turns nothing into a
        # mismatch; the check stays as unestablished as it was.
        return UNVERIFIED, f"{reason}; the claimed exception is unverified: {excepted[1]}"
    return excepted


def _observed(reading, check, context):
    """The verdict from the requirement and the observation alone, before any exception."""
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
    recurrences: [{faultId, reason}], recurrenceUnknown?: reason} - the only other facts a
    verdict may depend on. recurrenceUnknown says the subject's records could not all be read,
    so no recurrence can be ruled out."""
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
    if context.get("recurrenceUnknown"):
        recurrences.append({"check": "recurrence", "verdict": UNVERIFIED, "faultId": None,
                            "reason": context["recurrenceUnknown"]})
    verdicts = {entry["verdict"] for entry in checks + recurrences}
    pending = any(not entry["closure"]["ready"] for entry in checks if "closure" in entry)
    if verdicts & set(OPEN_VERDICTS):
        overall = MISMATCH
    elif UNVERIFIED in verdicts:
        overall = UNVERIFIED
    elif pending:
        overall = CLOSURE_PENDING
    else:
        overall = CONSISTENT
    return {"subject": reading["subject"], "product": reading["product"],
            "origin": reading["origin"], "verdict": overall, "checks": checks,
            "recurrences": recurrences}


# ---------------------------------------------------------------------- recording, via the port

# How many of a product's defect records a check reads looking for the subject's recurrences.
# Past it no recurrence can be ruled out, and the recurrence check reads unverified.
RECURRENCE_SCAN = 1000
RECURRENCE_PAGE = 100
# How many resolved mismatch rounds of one subject and check a reading looks past. Each round is
# its own record so the ledger's reopen-on-recurrence never reaches the subject; past this many,
# the next round is somebody's decision.
MAX_ROUNDS = 20


def _signature(reading, check, round_=1):
    signature = {"subject": reading["subject"], "check": check}
    if round_ > 1:
        signature["round"] = round_
    if reading["origin"] == products.SIMULATED:
        # Part of identity, so a simulated reading never lands on a real subject's record.
        signature["simulated"] = True
    return signature


def _mismatch_round(port, product, workspace, reading, check):
    """(fault id, row, signature, closed) of the mismatch record this reading belongs to, with
    the ids of the rounds already closed. After MAX_ROUNDS closed rounds the id is None: the
    reading may still be a replay of one of them, which check() recognises before it refuses.

    The newest round, unless that one was resolved: a mismatch found again after its closure
    starts the next round. Were it the same record coming back, the ledger's rule for a resolved
    fault that recurs would queue a reopen of the subject, and a completion check changes no
    state of the subject, ever; the new round's first write is a comment, as the first was.
    """
    closed = []
    for round_ in range(1, MAX_ROUNDS + 1):
        signature = _signature(reading, check, round_)
        fault_id = port.fault_id(product, workspace, products.MISMATCH, signature)
        row = port.get(fault_id)
        if row is None or row.get("state") != ledger_port.RESOLVED:
            return fault_id, row, signature, closed
        closed.append(fault_id)
    return None, None, None, closed


def reading_key(reading) -> str:
    """One reading's occurrence key: the same reading handed in twice is one occurrence."""
    digest = hashlib.sha256(products.canonical(reading).encode("utf-8")).hexdigest()
    return f"reading:{digest[:24]}"


def _recurrences(port, product, subject):
    """(recurrences, unknown): the subject's own defects that came back after their newest fix.

    A defect the subject owns is recurring when the ledger has it open again after a fix in its
    current cycle, or open in a cycle after a resolution. The ledger already commented and, for
    a resolved one, reopened; this only reports it on the subject, and never files it again.
    """
    recurring, after, read = [], None, 0
    while read < RECURRENCE_SCAN:
        page = port.list(product=product, fault_class=products.DEFECT, limit=RECURRENCE_PAGE,
                         after=after)
        for row in page.get("faults") or []:
            read += 1
            if row.get("external_ref") != subject or row.get("state") != ledger_port.OPEN:
                continue
            if row.get("reopen_count"):
                recurring.append({"faultId": row["fault_id"],
                              "reason": f"came back after it was resolved (cycle"
                                        f" {row.get('cycle')})"})
                continue
            fixes = [r for r in port.remediations(row["fault_id"], limit=20)
                     if r.get("kind") == ledger_port.FIX and r.get("cycle") == row.get("cycle")]
            if fixes:
                recurring.append({"faultId": row["fault_id"],
                              "reason": f"occurred again after the fix {fixes[-1]['ref']}"})
        after = page.get("next")
        if after is None:
            return recurring, None
    return recurring, (f"{product} has more than {RECURRENCE_SCAN} defect records; a recurrence"
                   f" beyond them cannot be ruled out")


def _detail(reading, entry):
    check = entry["check"]
    return "\n".join([
        f"completion check {check} of {reading['subject']}: {entry['verdict']}",
        f"reason: {entry['reason']}",
        f"claims: {', '.join(c for c in CLAIMS if reading['claims'][c]) or 'none'}",
        f"required: {reading['requires'][check]}  observed: {reading['observed'][check]}"
        f"  origin: {reading['origin']}",
        f"next action: re-verify {check} for {reading['subject']} and record the fix and the"
        f" verification, or an approved exception",
        "This asks the owner to re-verify. It changes no state of the subject: a Done stays"
        " Done until somebody who owns it decides otherwise.",
    ])


def _evidence(reading, check, key):
    entries = [{"kind": "completion-reading", "ref": key,
                "observed": {"check": check, "result": reading["observed"][check],
                             "required": reading["requires"][check]}}]
    for role, ref in sorted(reading["evidence"][check].items()):
        entries.append({"kind": role, "ref": ref["ref"], "source": ref["source"]})
    return entries


def check(router, record) -> dict:
    """Evaluate one reading and record what it shows, in one transaction.

    The subject must be an issue bound to the product as read back, because a mismatch is filed
    as a re-verification demand on the subject issue itself: the ledger adopts it, so its first
    write is a comment there, never a new issue and never a state change.
    """
    reading = read_reading(record)
    product = reading["product"]
    registry = router.registry(product)
    if registry is None:
        products.refuse(RefusalReason.ROUTE_PRODUCT_UNKNOWN,
                        f"{product!r} is not a registered product")
    simulated = reading["origin"] == products.SIMULATED
    if simulated and registry["testTarget"] is None:
        products.malformed(f"a simulated reading needs {product}'s test target")
    bindings = {b["ref"]: b for b in router.bindings(product) if b["test"] == simulated}
    subject = bindings.get(reading["subject"])
    if subject is None or subject["kind"] != "issue":
        products.refuse(RefusalReason.ROUTE_STATE_CONFLICT,
                        f"{reading['subject']} is not a bound {'test ' if simulated else ''}issue"
                        f" of {product}; a re-verification demand goes to the subject issue"
                        f" itself, so bind it as read back first")
    port = router.port
    workspace = registry["workspace"]
    records = {}
    for name in products.CHECKS:
        mismatch, mismatch_row, mismatch_signature, closed = _mismatch_round(
            port, product, workspace, reading, name)
        unverified = port.fault_id(product, workspace, products.UNVERIFIED,
                                   _signature(reading, name))
        records[name] = {"mismatch": mismatch, "mismatchRow": mismatch_row,
                         "mismatchSignature": mismatch_signature, "closed": closed,
                         "unverified": unverified, "unverifiedRow": port.get(unverified)}
    open_ = [name for name, entry in records.items()
             if (entry["mismatchRow"] or {}).get("state") in ledger_port.ACTIVE]
    recurrences, unknown = _recurrences(port, product, reading["subject"])
    answer = evaluate(reading, {"bindings": bindings, "openMismatches": open_,
                                "recurrences": recurrences, "recurrenceUnknown": unknown})
    if simulated:
        team, project = registry["testTarget"]["team"], registry["testTarget"]["project"]
    else:
        team, project = registry["team"], subject["project"] or registry["triageProject"]
    scope = {"workspace": workspace, **({"projectKey": project} if project else {})}
    key = reading_key(reading)
    written = []
    with router.store.composing() as db:
        for entry in answer["checks"]:
            name, verdict = entry["check"], entry["verdict"]
            ids = records[name]
            observed = dict(product=product, workspace=workspace, occurrence_key=key,
                            project=project, observed_at=reading["observedAt"],
                            detail=_detail(reading, entry),
                            evidence=_evidence(reading, name, key))
            owner = routes.plain_target(team=team, project=project, owner=reading["subject"])
            replayed = _replayed(router.store, ids["closed"], key)
            if verdict == CLAIM_WITHDRAWN:
                # The open mismatch stands and is reported so, but a withdrawn claim is not a
                # new failure: a subject reopened and being worked on is not observed again.
                written.append({"check": name, "faultId": ids["mismatch"], "recorded": "standing",
                                "ledgerState": (ids["mismatchRow"] or {}).get("state")})
            elif verdict in OPEN_VERDICTS and replayed:
                # A reading an earlier round already recorded, handed in again after that round
                # closed. Old evidence is not a new failure, so it opens nothing.
                written.append({"check": name, "faultId": replayed, "recorded": "replayed"})
            elif verdict in OPEN_VERDICTS:
                if ids["mismatch"] is None:
                    products.refuse(RefusalReason.ROUTE_STATE_CONFLICT,
                                    f"{name} of {reading['subject']} was closed {MAX_ROUNDS}"
                                    f" times and found wrong again; that is a decision for"
                                    f" somebody, not another round")
                adopt = None
                if ids["mismatchRow"] is None:
                    adopt = {"externalRef": reading["subject"], "scope": scope}
                port.record(port.observation(fault_class=products.MISMATCH, severity="degraded",
                                             signature=ids["mismatchSignature"], **observed),
                            adopt=adopt)
                routes.upsert(db, router.clock, fault_id=ids["mismatch"], product=product,
                              workspace=workspace, disposition=products.COMPLETION_MISMATCH,
                              stage=products.STAGE_FILED, target=owner,
                              origin=reading["origin"], claimed_severity="degraded",
                              detail=entry["reason"])
                # Kept on the round's route, every one of them, so the same reading handed in
                # again after this round closes is recognised rather than opening the next one.
                routes.store_incident(db, router.clock, ids["mismatch"], {
                    "occurrenceKey": key, "subject": reading["subject"], "check": name,
                    "verdict": verdict, "observedAt": reading["observedAt"]}, keep=None)
                # The ledger decides whether this reading files: under the class default the
                # first one does; a product whose own policy raises the threshold waits for that
                # many. Its state says which, where "no new write queued by this call" would not.
                written.append({"check": name, "faultId": ids["mismatch"], "recorded": verdict,
                                "ledgerState": (port.get(ids["mismatch"]) or {}).get("state")})
            if verdict == UNVERIFIED:
                port.record(port.observation(fault_class=products.UNVERIFIED, severity="notice",
                                             signature=_signature(reading, name), **observed))
                routes.upsert(db, router.clock, fault_id=ids["unverified"], product=product,
                              workspace=workspace, disposition=products.COMPLETION_UNVERIFIED,
                              stage=products.STAGE_OBSERVED, target=owner,
                              origin=reading["origin"], claimed_severity="notice",
                              detail=entry["reason"])
                written.append({"check": name, "faultId": ids["unverified"],
                                "recorded": UNVERIFIED})
            elif (ids["unverifiedRow"] or {}).get("state") in ledger_port.ACTIVE:
                # Established either way now, so the unverified notice has nothing left to say.
                port.record(port.observation(fault_class=products.UNVERIFIED, severity="notice",
                                             signature=_signature(reading, name), cleared=True,
                                             **observed))
                written.append({"check": name, "faultId": ids["unverified"],
                                "recorded": "unverified_cleared"})
            closure = entry.get("closure")
            if closure and closure["ready"] and name in open_:
                _close(port, ids["mismatch"], ids["mismatchRow"], closure, key)
                written.append({"check": name, "faultId": ids["mismatch"], "recorded": "closed"})
    return {**answer, "recorded": written}


def _close(port, fault_id, row, closure, key):
    """D21: a fix and a verification somebody can point to, or an approved exception."""
    if "exception" in closure:
        fix = f"exception: {closure['exception']}"
        verification = {"method": "observation", "ref": key, "outcome": "absent"}
    else:
        fix = closure["fix"]["ref"]
        verification = {"method": "observation", "ref": closure["verification"]["ref"],
                        "outcome": "passed"}
    if row["state"] != ledger_port.FIX_PENDING:
        port.record_fix(fault_id, ref=fix, detail="closure of a completion mismatch")
    port.record_reverification(fault_id, detail="completion reading", **verification)
    port.resolve(fault_id)


def _replayed(store, closed, key):
    """The closed round that already recorded this reading, or None."""
    for fault_id in closed:
        if routes.stored_incident(store, fault_id, key):
            return fault_id
    return None
