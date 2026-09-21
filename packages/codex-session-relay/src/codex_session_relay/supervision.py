"""When the level above is told, and what it is still owed.

A supervisor is a communication task with no goal of its own. Waking it costs a turn, and the
rule for when that is worth doing has until now lived in whatever the model happened to
remember. This module reads it off the store instead.

Three things are news for the level above: a completion, a NEW real block, and a decision only
the user can make. A CI run changing, an acknowledgement arriving, a child progressing
normally, a wait that has not changed - each is a real state change somewhere, and none of
them is news. select() answers with a reason, and the reason is the part worth reading.

Nothing here is a new table, a new queue or a second send engine. An obligation is DERIVED
from rows that already exist, which is what lets the same reading survive a restart, a
compaction and a service replacement with nothing remembered in between.

What this module will not claim: that a supervisor received anything. The store holds no
supervisor message, no supervisor acknowledgement and no supervisor receipt, because the relay
carries no channel for one - OPS-7.4 says so and envelope.REACH_SOURCES encodes it. The one
discharge this can actually read is the Linear record the supervisor goes and reads for itself,
and only when the synchronisation row says confirmed. A report whose Linear write failed leaves
the obligation standing, which is the exact failure CRW-148 asks to be regression-tested.
"""

import json

from . import cxc, envelope, sync
from .identity import sha256_hex

SCHEMA = "supervisor-obligation/1"

# The three that are news, plus the one an ABSENT report leaves behind.
COMPLETION = "completion"
BLOCKED = "blocked"
DECISION = "decision_request"
UNREPORTED = "unreported"
KINDS = (COMPLETION, BLOCKED, DECISION, UNREPORTED)

# Which envelope purpose carries each kind upward, so one vocabulary decides both.
PURPOSE = {COMPLETION: "completion", BLOCKED: "blocked", DECISION: "decision_request",
           UNREPORTED: "blocked"}

STANDING = "standing"
DISCHARGED = "discharged"

# Why a wake was not produced. Each one preserves the obligation; none of them discards it.
NOT_NEWS = "no_meaningful_transition"
ALREADY_REPORTED = "already_reported_under_this_obligation"
ALREADY_RECORDED = "already_in_the_record_the_supervisor_reads"
NO_CONTACT = "recipient_is_not_contactable"
# Nobody has observed whether the level above can be reached. Different from NO_CONTACT, which
# is a recipient observed as unreachable, and it fails the same way: without evidence that a
# wake can land, this does not produce one.
CONTACT_UNMEASURED = "recipient_contactability_unmeasured"
REPORTABLE = "reportable"

# What a child's own report status means for the level above. A blocked or exhausted run is a
# block; an unsafe or needs-human one is a decision, because both name a judgment the parent is
# not allowed to make for the user. Done and noop are completions: noop IS a result, and its
# finding is the deliverable.
BY_STATUS = {
    cxc.DONE: COMPLETION,
    cxc.NOOP: COMPLETION,
    cxc.BLOCKED: BLOCKED,
    cxc.BUDGET_EXHAUSTED: BLOCKED,
    cxc.UNSAFE: DECISION,
    cxc.NEEDS_HUMAN: DECISION,
}

# And what the outcome alone means, for an event carrying no work report. failed and
# interrupted are deliberately absent: how a turn ended is the parent's business, and
# re-dispatching it is ordinary work rather than news for the level above.
BY_OUTCOME = {"ready_for_review": COMPLETION, "blocked_needs_input": BLOCKED}

# The one observation state that owes a report. omitted.observe() also answers reported,
# in_progress, unmanaged and unmeasured, and none of those is an omission: accepting the
# record's SHAPE rather than its verdict would manufacture obligations out of readings that
# said the opposite, and an evidence read that failed is not evidence that a report is owed.
OBSERVED_OMISSION = "unreported"

# Who produced an event, for events that travel UPWARD. A correction is written by the relay
# on the parent's verdict and travels down to a child, and it carries a report of its own with
# a status: without this check a needs-human correction - an ordinary parent judgment about a
# child's work - was promoted into a decision the user owed. The daemon's observation of how a
# turn ended is a child-side fact and stays eligible.
UPWARD_PRODUCERS = ("child", "daemon_observation")

# The journal kind a produced report is recorded under. The journal is an existing append-only
# table, so a second observation of one fact - after a restart, after a service replacement -
# finds the first report rather than producing another.
JOURNAL_KIND = "supervisor_report"

ID_WIDTH = 32


def obligation_id(*, kind, relation_id, subject) -> str:
    """One fact, one id, however many times it is read.

    The project key is deliberately not an input, for the same reason the envelope keeps the
    Linear scope out of its message id: a caller that could not read the scope row would
    otherwise derive a different id for the same fact and the convergence would be lost
    exactly when the store is least readable.
    """
    if kind not in KINDS:
        raise ValueError(f"{kind!r} is not one of {KINDS}")
    return sha256_hex(f"{kind}|{relation_id}|{subject}")[:ID_WIDTH]


def _obligation(kind, *, relation_id, subject, basis, detail, generation=None, revision=None,
                issue_key=None) -> dict:
    return {
        "schema": SCHEMA,
        "obligationId": obligation_id(kind=kind, relation_id=relation_id, subject=subject),
        "kind": kind,
        "relationId": relation_id,
        "subject": subject,
        "executionGeneration": generation,
        "revisionHash": revision,
        "issueKey": issue_key,
        # Which row raised it, so a reader can go and check rather than take this on faith.
        "basis": basis,
        "detail": detail,
    }


def from_event(store, event_id, report=None) -> dict | None:
    """The obligation one event raises, or None when it raises none.

    None is the ordinary answer. Most events are a child getting on with its work, and the
    whole point of this function is that it says so instead of producing a wake.
    """
    row = store.one("SELECT * FROM events WHERE event_id = ?", (event_id,))
    if row is None:
        return None
    if row["stage"] != "final":
        # A child emitting from inside its own turn can only observe inProgress, so its claim
        # is staged. A staged claim is not a transition anybody can report upward yet.
        return None
    if row["suppressed_reason"]:
        return None
    if row["producer"] not in UPWARD_PRODUCERS:
        return None
    kind = None
    detail = None
    if report is not None:
        kind = BY_STATUS.get(report.get("cxcStatus"))
        detail = report.get("summary")
    if kind is None:
        kind = BY_OUTCOME.get(row["outcome"])
        detail = detail or f"the turn ended {row['outcome']}"
    if kind is None:
        return None
    issue = store.one(
        "SELECT issue_key FROM relationships WHERE relationship_id = ?",
        (row["relationship_id"],),
    )
    return _obligation(
        kind, relation_id=row["relationship_id"], subject=event_id,
        generation=row["execution_generation"], revision=row["revision_hash"],
        issue_key=issue["issue_key"] if issue is not None else None,
        basis={"table": "events", "eventId": event_id, "outcome": row["outcome"],
               "cxcStatus": (report or {}).get("cxcStatus")},
        detail=detail,
    )


def from_observation(reading) -> dict | None:
    """The obligation an ABSENT report leaves behind.

    A turn that ended without reporting writes no events row, so an event-keyed enumeration
    cannot see it at all. This is the second input path, and it takes CRW-180's
    reporting-observation/1 reading rather than re-deriving one: that module owns the
    diagnosis and this one owns what is owed because of it.

    Only reportingState == unreported raises anything. unmeasured in particular does not: a
    failed evidence read is the absence of an answer, and turning it into an obligation would
    be inventing the one thing the reading refused to assert.
    """
    if not isinstance(reading, dict) or reading.get("schema") != "reporting-observation/1":
        return None
    if reading.get("reportingState") != OBSERVED_OMISSION:
        return None
    relation = reading.get("relationshipId")
    turn = (reading.get("selectors") or {}).get("turn")
    if not relation or not turn:
        return None
    return _obligation(
        UNREPORTED, relation_id=relation, subject=turn,
        generation=reading.get("executionGeneration"),
        basis={"table": None, "schema": reading["schema"], "reason": reading.get("reason"),
               "turn": turn},
        detail="an admitted turn settled without a report, so what it owed is still owed",
    )


def unmeasured_gap(reading) -> dict | None:
    """A reading that could not establish anything, carried as a gap rather than an obligation."""
    if not isinstance(reading, dict) or reading.get("schema") != "reporting-observation/1":
        return None
    if reading.get("reportingState") != "unmeasured":
        return None
    return {"schema": SCHEMA, "gap": "reporting_unmeasured",
            "relationId": reading.get("relationshipId"), "reason": reading.get("reason"),
            "detail": "nothing was established about whether a report was owed here"}


def discharge_of(store, obligation, *, target=sync.COORDINATION_DOCUMENT) -> dict:
    """Whether the record the supervisor actually reads has this yet.

    There is no supervisor receipt in this store and this does not pretend otherwise. What
    exists is the Linear synchronisation row, which the supervisor reads for itself, and it
    counts only at confirmed - the state reached after a readback verified what was written.

    pending, claimed, written and failed all leave the obligation standing, and failed is the
    one worth naming: SyncOutbox.fail flips to it after MAX_ATTEMPTS and drops the row out of
    automatic selection, so it LOOKS terminal while meaning the opposite of done.

    Two things are checked besides the state, because neither is implied by it. The row has to
    be the one that carries this kind of news: a confirmed progress note is a real row about
    the same event and says nothing about whether the outcome was reported, so only a verdict
    row discharges. And it has to have landed in the document anybody is reading now: a
    confirmation written to a target that has since been repointed is a record in a place the
    supervisor no longer looks.
    """
    rows = store.all(
        "SELECT sync_id, state, target, target_ref, external_ref, confirmed_at, last_error"
        "  FROM sync_outbox WHERE relationship_id = ? AND event_id = ? AND subject_kind = ?"
        "   AND target = ?"
        " ORDER BY created_at",
        (obligation["relationId"], obligation["subject"], sync.VERDICT, target),
    )
    records = [dict(row) for row in rows]
    configured = store.one(
        "SELECT target_ref FROM sync_targets WHERE relationship_id = ? AND target = ?",
        (obligation["relationId"], target),
    )
    current = configured["target_ref"] if configured is not None else None
    if current is None:
        # No target is configured, so there is no record the supervisor reads for this
        # relationship at all. A confirmed row from before the target was cleared describes a
        # document nobody is pointed at now, and treating its absence as permissive would
        # discharge an obligation against a place this store cannot name.
        return {"standing": STANDING,
                "reason": f"no {target} target is configured for this relationship, so there"
                          f" is no record the supervisor reads",
                "records": records}
    confirmed = [one for one in records if one["state"] == sync.CONFIRMED
                 and one["target_ref"] == current]
    if confirmed:
        return {"standing": DISCHARGED, "reason": "the Linear record is confirmed",
                "records": records, "externalRef": confirmed[0]["external_ref"],
                "confirmedAt": confirmed[0]["confirmed_at"]}
    if records:
        states = ", ".join(sorted({one["state"] for one in records}))
        elsewhere = [one for one in records if one["state"] == sync.CONFIRMED]
        if elsewhere:
            return {"standing": STANDING,
                    "reason": f"the only confirmed record is on {elsewhere[0]['target_ref']},"
                              f" which is no longer the target for this relationship",
                    "records": records}
        return {"standing": STANDING,
                "reason": f"the Linear record is {states} rather than {sync.CONFIRMED}",
                "records": records}
    return {"standing": STANDING,
            "reason": "no Linear record was enqueued for this, so nothing the supervisor reads"
                      " carries it",
            "records": []}


def prior_report(store, identifier):
    """The first time this obligation produced a report, if it ever did.

    Read from the journal, which is durable and append-only, so a service restart and a second
    observation of one fact converge here instead of each producing a turn. What this proves is
    that a report was PRODUCED, which is not the same as one having arrived; nothing in this
    store could prove the second.
    """
    row = store.one(
        "SELECT seq, at, detail FROM journal WHERE kind = ? AND subject = ?"
        " ORDER BY seq LIMIT 1",
        (JOURNAL_KIND, identifier),
    )
    if row is None:
        return None
    try:
        detail = json.loads(row["detail"]) if row["detail"] else {}
    except (TypeError, ValueError):
        detail = {"detail": row["detail"]}
    return {"seq": row["seq"], "at": row["at"], "detail": detail}


def record_report(store, obligation, *, at, messageId=None, note="") -> dict:
    """Record that a report was produced for this obligation, once.

    Idempotent by reading first inside the same transaction: two callers racing on one fact
    converge on the first entry rather than writing a second, which is what keeps the
    convergence a property of the record instead of of the caller's care.
    """
    identifier = obligation["obligationId"]
    with store.transaction() as db:
        existing = db.execute(
            "SELECT seq FROM journal WHERE kind = ? AND subject = ? LIMIT 1",
            (JOURNAL_KIND, identifier),
        ).fetchone()
        if existing is not None:
            return {"recorded": False, "seq": existing["seq"]}
        store.journal(JOURNAL_KIND, identifier,
                      {"kind": obligation["kind"], "relationId": obligation["relationId"],
                       "subject": obligation["subject"], "messageId": messageId, "note": note},
                      at=at)
        seq = db.execute("SELECT last_insert_rowid() AS seq").fetchone()["seq"]
    return {"recorded": True, "seq": seq}


def contactable(store, task_id) -> dict:
    """Whether the level above can be reached at all, read from the host's own observation.

    A paused or archived recipient is not a failure and is not a reason to drop anything. The
    obligation stays exactly where it was and nobody is woken, which is what a user who paused
    a task asked for.
    """
    if not task_id:
        return {"contactable": None, "reason": "no recipient was named"}
    row = store.one(
        "SELECT deliverable, withhold_reason, detail, observed_at FROM recipient_lifecycle"
        " WHERE task_id = ?", (task_id,))
    if row is None:
        return {"contactable": None,
                "reason": "the host has not been observed for this task, so deliverability is"
                          " unmeasured rather than allowed"}
    return {"contactable": row["deliverable"] == "yes", "deliverable": row["deliverable"],
            "reason": row["withhold_reason"] or row["deliverable"],
            "observedAt": row["observed_at"]}


def select(store, obligation, *, recipient=None) -> dict:
    """Whether this obligation should produce a report now, and why not when it should not.

    Every suppression here preserves the obligation. The two facts are reported separately on
    purpose: an obligation can be suppressed as a duplicate AND still be standing, which is
    precisely the state a report whose Linear write failed leaves behind. Collapsing them
    would let a produced report stand in for a recorded one.
    """
    discharge = discharge_of(store, obligation)
    prior = prior_report(store, obligation["obligationId"])
    contact = contactable(store, recipient)
    decision = {
        "schema": SCHEMA,
        "obligationId": obligation["obligationId"],
        "kind": obligation["kind"],
        "standing": discharge["standing"],
        "dischargeReason": discharge["reason"],
        "priorReport": prior,
        "recipient": contact,
    }
    if discharge["standing"] == DISCHARGED:
        return {**decision, "report": False, "reason": ALREADY_RECORDED}
    if prior is not None:
        return {**decision, "report": False, "reason": ALREADY_REPORTED}
    if contact["contactable"] is None:
        # Not a wake. Deliverability needs positive evidence for the same reason delivery's
        # own lifecycle check does: an unobserved recipient is a question nobody asked the
        # host, and answering it optimistically is how a paused task gets woken anyway.
        return {**decision, "report": False, "reason": CONTACT_UNMEASURED}
    if contact["contactable"] is False:
        return {**decision, "report": False, "reason": NO_CONTACT}
    return {**decision, "report": True, "reason": REPORTABLE}


def suppressed(reason, detail) -> dict:
    """The answer for a candidate that is not one of the three. No obligation is created."""
    return {"schema": SCHEMA, "report": False, "reason": NOT_NEWS, "obligationId": None,
            "detail": detail, "candidate": reason}


def standing_for(store, linkage, project_key, *, observations=()) -> dict:
    """Every standing obligation in one project, derived rather than remembered.

    Scoped to the project rather than to whichever task currently parents each row, for the
    same reason linkage.outstanding is: after a handover, filtering by the parent would report
    a replacement owner as having nothing owed.

    Nor is it scoped to LIVE relationships. A real handover registers a successor and
    supersedes the row it replaces, so filtering on status would drop exactly the obligations a
    replacement owner most needs to see - the ones the outgoing owner left behind. Every
    relationship in the project is read and each obligation carries the status of the row it
    came from.

    Observations are passed IN rather than produced here. A turn that ended without reporting
    writes no events row, so no query over this store can find it; the reading comes from
    CRW-180's observer, which needs a marker root and explicit selectors this module does not
    have. What is owed because of such a reading is decided here, and a reading that
    established nothing is carried as a gap instead.

    This does NOT say the project is complete or close to it. An obligation is raised about the
    issue it came from and is never promoted into a statement about the project, which
    AssignmentView.project_state owns and stops at complete_candidate.
    """
    from .report import read as read_work_report

    rows = store.all(
        "SELECT r.relationship_id, r.status, r.superseded_by FROM relationships r"
        "  JOIN relationship_scope s ON s.relationship_id = r.relationship_id"
        " WHERE s.project_key = ? ORDER BY r.created_at",
        (project_key,),
    )
    relations = {row["relationship_id"]: dict(row) for row in rows}
    obligations = []
    for relation, about in relations.items():
        for row in store.all(
            "SELECT event_id FROM events WHERE relationship_id = ?"
            " ORDER BY first_seen_at", (relation,),
        ):
            one = from_event(store, row["event_id"], read_work_report(store, row["event_id"]))
            if one is None:
                continue
            decided = select(store, one, recipient=None)
            if decided["standing"] == STANDING:
                obligations.append({**one, "decision": decided,
                                    "relationshipStatus": about["status"],
                                    "supersededBy": about["superseded_by"]})
    gaps = []
    seen = {entry["obligationId"] for entry in obligations}
    for reading in observations:
        about = reading.get("relationshipId") if isinstance(reading, dict) else None
        if about not in relations:
            # A reading about somebody else's project is not this project's business, and
            # accepting it would let a caller's list decide what a project owes.
            continue
        one = from_observation(reading)
        if one is not None:
            if one["obligationId"] in seen:
                continue
            seen.add(one["obligationId"])
            obligations.append({**one, "decision": select(store, one, recipient=None),
                                "relationshipStatus": (relations.get(one["relationId"]) or {})
                                .get("status")})
            continue
        gap = unmeasured_gap(reading)
        if gap is not None and gap not in gaps:
            gaps.append(gap)
    return {"schema": SCHEMA, "projectKey": project_key, "relations": list(relations),
            "standing": obligations, "gaps": gaps,
            "limits": "derived from this store's rows only. It says what is owed upward, never"
                      " that a supervisor received anything, and never that the project is"
                      " complete. A turn that ended without reporting has no row here at all,"
                      " so it is present only when its observation was passed in"}


def status_answer(store, linkage, assignments, project_key, *, observations=()) -> dict:
    """The answer to an EXPLICIT request, which automatic suppression does not silence.

    A midpoint check is a question somebody asked, not a notification this module decided to
    send. Skipping it because the automatic channel is quiet would answer a different question
    from the one that was put.
    """
    answer = standing_for(store, linkage, project_key, observations=observations)
    answer["projectState"] = assignments.project_state(project_key)
    answer["answeredBecause"] = (
        "an explicit status request is a separate path from automatic notification; it is"
        " answered whether or not a wake would have been suppressed")
    return answer


def envelope_for(store, obligation, *, sender, recipient, scope=None, observed_at=None,
                 correlation_id=None, decision=None) -> dict:
    """The parent-to-supervisor envelope for one obligation.

    Its reach ladder is the direction's honest default: every stage not_applicable, because no
    channel exists to transport, receive, agree, apply or verify on. What IS known about how
    far this got travels as the obligation's discharge, which names a Linear record rather than
    a receipt nobody holds.
    """
    return envelope.region(
        direction=envelope.PARENT_TO_SUPERVISOR,
        purpose=PURPOSE[obligation["kind"]],
        relation_id=obligation["relationId"],
        sender=sender,
        recipient=recipient,
        subject=obligation["subject"],
        observed_at=observed_at,
        scope=scope,
        basis=(f"generation {obligation['executionGeneration']}, revision "
               f"{str(obligation['revisionHash'])[:12]}") if obligation.get("revisionHash")
        else None,
        correlation_id=correlation_id,
        decision=decision,
        evidence=[f"codex-session-relay show --event {obligation['subject']}"]
        if obligation["kind"] != UNREPORTED else [],
    )
