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

What this module will not claim: that a supervisor agreed to anything. It decides what is owed
and nothing else - it opens no queue, reads no host and sends nothing. supervisorchannel.py is
what carries a report upward and records the recipient's own readback, and even that stops at
this attempt's request id being in the recipient's thread: arrival, not reading. The one
discharge this can actually read is the Linear record the supervisor goes and reads for itself,
and only when the synchronisation row says confirmed. A report whose Linear write failed
leaves the obligation standing, which is the exact failure CRW-148 asks to be regression-tested,
and a report that was read leaves it standing too.
"""

import json

from . import cxc, envelope, intent, sync
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
# The caller named no recipient, so deliverability was not part of the question it asked. This
# is not a suppression: answering it as one made every recipient-less reading - which is what
# a project enumeration does - report that nothing was worth telling anybody.
NOT_ASKED = "deliverability_was_not_asked_about"
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
# The schema those readings carry. omitted.SCHEMA is where it is defined and a test pins the
# two equal; it is repeated here rather than imported because importing the observer for one
# string would pull its whole evidence closure - guard, marker, admission, store, receipts -
# into every import of this module, and would close a cycle the day any of them needs to know
# what a reading owes.
OBSERVATION_SCHEMA = "reporting-observation/1"
# A reading that established nothing. It is carried as a gap and never as an obligation.
OBSERVED_UNMEASURED = "unmeasured"
# The states that SETTLED the question without an omission: the turn reported, it is still
# running, or this relay never managed it at all. Each is an ordinary midpoint reading and owes
# nothing, so each is absorbed rather than answered. Without a branch of their own they fell
# through to unusable_reading, and a project where every child had reported read back as a
# project whose readings could not be read.
OBSERVED_SETTLED = ("reported", "in_progress", "unmanaged")
# Everything omitted.observe() can answer, so a value outside it is a reading this cannot
# interpret rather than one it may quietly absorb.
OBSERVED_STATES = (OBSERVED_OMISSION, OBSERVED_UNMEASURED, *OBSERVED_SETTLED)

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

# How old an observation of the recipient may be and still authorize a wake. The delivery path
# re-observes the host before every send for the same reason: a stored yes is a fact about the
# moment somebody looked.
CONTACT_FRESH_FOR = 900.0

# How far ahead of this reading an observation may be dated and still be read as current. A
# host and this process can disagree by a little; a timestamp beyond that is not evidence
# about now, and treating it as fresh let a clock that went backwards authorize wakes on
# lifecycle evidence nobody currently holds.
CONTACT_FUTURE_TOLERANCE = 60.0


def _subject(kind, row, report) -> str:
    """What the obligation is ABOUT, which is not always the event that raised it.

    A completion is about one revision, so the event identifies it. A block is not. Each
    re-emission of an unchanged block is a new event - the execution-level id carries the turn
    and the attempt - so keying on it made one unresolved block a fresh obligation every time
    the child said so again, and every one of those got its own wake. The issue's words for
    this are a NEW real block, not the same block re-explained.

    So a block and a decision are keyed on the generation they arose in and on what they say.
    A different cause in the same generation is a different block and does wake; the same cause
    said twice converges on the first, where the prior-report record then suppresses it.
    """
    if kind in (BLOCKED, DECISION):
        # Encoded rather than joined. The fields are prose and contain spaces of their own, so
        # a space-joined string does not preserve the boundaries between them: "waiting on API"
        # plus "schema update" hashed the same as "waiting on" plus "API schema update", and
        # the second blocker then read as one already reported. JSON keeps the order and the
        # boundaries without needing a separator the text cannot contain.
        cause = json.dumps(
            [(report or {}).get("cxcStatus") or row["outcome"],
             (report or {}).get("cxcReason") or "",
             (report or {}).get("summary") or ""],
            ensure_ascii=False, separators=(",", ":"),
        )
        return f"g{row['execution_generation']}:{sha256_hex(cause)[:16]}"
    return row["event_id"]


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
        kind, relation_id=row["relationship_id"], subject=_subject(kind, row, report),
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
    if not isinstance(reading, dict) or reading.get("schema") != OBSERVATION_SCHEMA:
        return None
    if reading.get("reportingState") != OBSERVED_OMISSION:
        return None
    relation = reading.get("relationshipId")
    turn = _turn_of(reading)
    # Both have to be names, not merely present. A list here is truthy, so it used to reach
    # the identifier derivation and produce a well-formed id for a reading nobody could act
    # on; upstream it also reached a dict membership test and raised on being unhashable,
    # which took the whole project query down with it.
    if not _named(relation) or not _named(turn):
        return None
    return _obligation(
        UNREPORTED, relation_id=relation, subject=turn,
        generation=reading.get("executionGeneration"),
        basis={"table": None, "schema": reading["schema"], "reason": reading.get("reason"),
               "turn": turn},
        detail="an admitted turn settled without a report, so what it owed is still owed",
    )


def _named(value) -> bool:
    """A name is a non-blank string. A list is truthy and a blank passes a presence check."""
    return isinstance(value, str) and bool(value.strip())


def _turn_of(reading):
    """The turn a reading names, or None when its selectors are not an object at all.

    Read through here rather than as (reading.get("selectors") or {}).get("turn"). A list is
    truthy, so that expression called .get on it and raised AttributeError out of the whole
    project answer - the same way an unhashable relationshipId once did, and with the same
    consequence: one malformed file deciding what a project owed by taking the query down.
    """
    selectors = reading.get("selectors")
    return selectors.get("turn") if isinstance(selectors, dict) else None


def observed_state(reading):
    """Which of the observer's five states this reading declares, or None when it is not one.

    None is the answer for a reading this cannot interpret at all: something that is not an
    object, a record under another schema, or a reportingState nobody here knows. Absorbing an
    unknown state would answer that nothing is owed on evidence this module cannot read, which
    is what from_observation already refuses to do for unmeasured.
    """
    if not isinstance(reading, dict) or reading.get("schema") != OBSERVATION_SCHEMA:
        return None
    state = reading.get("reportingState")
    return state if state in OBSERVED_STATES else None


def unmeasured_gap(reading) -> dict | None:
    """A reading that could not establish anything, carried as a gap rather than an obligation."""
    if observed_state(reading) != OBSERVED_UNMEASURED:
        return None
    return {"schema": SCHEMA, "gap": "reporting_unmeasured",
            "relationId": reading.get("relationshipId"), "reason": reading.get("reason"),
            "detail": "nothing was established about whether a report was owed here"}


def unusable_reading(reading) -> dict:
    """A reading this cannot read, named rather than dropped and never raised.

    Refusing it quietly would answer that a project owes nothing on the strength of a file
    nobody could read, and raising would take the whole query down with one bad entry. The
    caller gets its answer AND is told which of its readings was not usable.

    The reason names the cause it actually has. It used to say the reading named no usable
    relationship and turn whatever was wrong with it, so a record under another schema was
    described by fields it never claimed to carry - and so was the ordinary reading that
    reached here only because no other branch would take it.
    """
    if not isinstance(reading, dict):
        return {"schema": SCHEMA, "gap": "reading_unusable", "relationId": None,
                "reason": f"a reading is an object, not {type(reading).__name__}",
                "detail": "this reading was not placed in any project"}
    scope = reading.get("relationshipId")
    if reading.get("schema") != OBSERVATION_SCHEMA:
        reason = f"schema {reading.get('schema')!r} is not {OBSERVATION_SCHEMA}"
    elif reading.get("reportingState") not in OBSERVED_STATES:
        reason = (f"reportingState {reading.get('reportingState')!r} is not one a"
                  f" {OBSERVATION_SCHEMA} reading carries")
    else:
        missing = [name for name, value in
                   (("relationship", scope), ("turn", _turn_of(reading))) if not _named(value)]
        reason = ("the reading names no usable " + " or ".join(missing) if missing else
                  "the reading is well formed, and nothing said why it could not be placed")
    return {"schema": SCHEMA, "gap": "reading_unusable",
            "relationId": scope if isinstance(scope, str) else None,
            "reason": reason,
            "detail": "this reading was not placed in any project"}


def _carry(gaps, gap) -> None:
    """Record a gap once. One bad reading handed in twice is one thing wrong, not two."""
    if gap not in gaps:
        gaps.append(gap)


def discharge_of(store, obligation, *, target=sync.COORDINATION_DOCUMENT) -> dict:
    """Whether the record the supervisor actually reads has this yet.

    There is no supervisor acknowledgement in this store and this does not pretend otherwise.
    A readback on the supervisor channel says one attempt's request id is in the recipient's
    thread and that the turn it names is real there - not that anybody read it - and it
    discharges nothing. What discharges one is the Linear synchronisation row, which the
    supervisor reads for itself, and it counts only at confirmed - the state reached after a
    readback verified what was written.

    pending, claimed, written and failed all leave the obligation standing, and failed is the
    one worth naming: SyncOutbox.fail flips to it after MAX_ATTEMPTS and drops the row out of
    automatic selection, so it LOOKS terminal while meaning the opposite of done.

    Two things are checked besides the state, because neither is implied by it. The row has to
    be the one that carries this kind of news: a confirmed progress note is a real row about
    the same event and says nothing about whether the outcome was reported, so only a verdict
    row discharges. And it has to have landed in the document anybody is reading now: a
    confirmation written to a target that has since been repointed is a record in a place the
    supervisor no longer looks.

    One verdict row per event used to be all there was, which made "any confirmed row" and "the
    row for the ruling that stands" the same sentence. Identity now separates rulings made
    against different criteria, so an event can own several, and the difference matters: an old
    confirmed row would otherwise report this discharged while the row carrying the current
    ruling is still pending or failed - hiding exactly the synchronisation failure that
    separating them exists to surface. Only the newest ruling answers, and it answers for the
    document being read now: newer on a target since repointed away is not landed here either.

    Newest is by rowid, which is insertion order. created_at is a wall clock, and this package
    does not decide from wall clocks: moved backwards between two rulings it would sort an
    earlier confirmed row after a later pending one and reopen the masking through the ordering.
    """
    rows = store.all(
        "SELECT sync_id, state, target, target_ref, external_ref, confirmed_at, last_error"
        "  FROM sync_outbox WHERE relationship_id = ? AND event_id = ? AND subject_kind = ?"
        "   AND target = ?"
        " ORDER BY rowid",
        (obligation["relationId"], _event_of(obligation), sync.VERDICT, target),
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
    if not records:
        return {"standing": STANDING,
                "reason": "no Linear record was enqueued for this, so nothing the supervisor"
                          " reads carries it",
                "records": []}
    latest = records[-1]
    if latest["target_ref"] != current:
        # Asked of the newest ruling rather than of any confirmed row, or an older confirmation
        # sitting on the current target would be described as the one nobody reads.
        return {"standing": STANDING,
                "reason": f"the current ruling's record is on {latest['target_ref']}, which is"
                          f" no longer the target for this relationship",
                "records": records}
    if latest["state"] == sync.CONFIRMED:
        return {"standing": DISCHARGED, "reason": "the Linear record is confirmed",
                "records": records, "externalRef": latest["external_ref"],
                "confirmedAt": latest["confirmed_at"]}
    return {"standing": STANDING,
            "reason": f"the Linear record is {latest['state']} rather than {sync.CONFIRMED}",
            "records": records}


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


def _event_of(obligation):
    """The event a synchronisation row would be keyed on, which a block's subject is not."""
    return (obligation.get("basis") or {}).get("eventId") or obligation["subject"]


def contactable(store, task_id, *, now=None, fresh_for=CONTACT_FRESH_FOR) -> dict:
    """Whether the level above can be reached at all, read from the host's own observation.

    A paused or archived recipient is not a failure and is not a reason to drop anything. The
    obligation stays exactly where it was and nobody is woken, which is what a user who paused
    a task asked for.

    A stored yes is a fact about the moment somebody looked at the host, not about now. Without
    a clock to measure its age against, and past the window, it reads unmeasured rather than
    permitting: the same direction the delivery path takes when it cannot establish
    deliverability, and for the same reason.
    """
    if not task_id:
        return {"contactable": None, "asked": False,
                "reason": "no recipient was named, so deliverability was not part of this"
                          " question"}
    row = store.one(
        "SELECT deliverable, withhold_reason, detail, observed_at FROM recipient_lifecycle"
        " WHERE task_id = ?", (task_id,))
    if row is None:
        return {"contactable": None,
                "reason": "the host has not been observed for this task, so deliverability is"
                          " unmeasured rather than allowed"}
    answer = {"deliverable": row["deliverable"], "observedAt": row["observed_at"]}
    if row["deliverable"] != "yes":
        return {**answer, "contactable": False,
                "reason": row["withhold_reason"] or row["deliverable"]}
    moment = intent.moment(row["observed_at"])
    if moment is None:
        return {**answer, "contactable": None,
                "reason": "the observation carries no readable time, so its age is unmeasured"}
    if now is None:
        return {**answer, "contactable": None,
                "reason": "no clock was supplied, so whether this observation is still current"
                          " is unmeasured"}
    # intent.moment answers an aware datetime and the clock answers epoch seconds, so the
    # comparison is made in one of them rather than between the two.
    age = now - moment.timestamp()
    if age < -CONTACT_FUTURE_TOLERANCE:
        return {**answer, "contactable": None, "ageSeconds": age,
                "reason": f"the observation is dated {int(-age)}s in the future, so it is not"
                          f" evidence about now"}
    if age > fresh_for:
        return {**answer, "contactable": None, "ageSeconds": age,
                "reason": f"the observation is {int(age)}s old, past the {int(fresh_for)}s this"
                          f" reading treats as current"}
    return {**answer, "contactable": True, "ageSeconds": age, "reason": row["deliverable"]}


def select(store, obligation, *, recipient=None, now=None,
           fresh_for=CONTACT_FRESH_FOR) -> dict:
    """Whether this obligation should produce a report now, and why not when it should not.

    Every suppression here preserves the obligation. The two facts are reported separately on
    purpose: an obligation can be suppressed as a duplicate AND still be standing, which is
    precisely the state a report whose Linear write failed leaves behind. Collapsing them
    would let a produced report stand in for a recorded one.
    """
    discharge = discharge_of(store, obligation)
    prior = prior_report(store, obligation["obligationId"])
    contact = contactable(store, recipient, now=now, fresh_for=fresh_for)
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
        if contact.get("asked") is False:
            # Nobody asked about a recipient, so this answers about the obligation alone: it
            # stands, nothing has reported it, and whether anybody can be reached is a
            # separate question this call was not given the means to ask.
            return {**decision, "report": True, "reason": NOT_ASKED}
        # A recipient WAS named and could not be established. Deliverability needs positive
        # evidence for the same reason delivery's own lifecycle check does: an unobserved or
        # stale recipient is a question nobody has answered recently, and answering it
        # optimistically is how a paused task gets woken anyway.
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

    A reading is PLACED and then INTERPRETED, in that order, and the two questions are kept
    apart because only one of the five states needs an owner. A relationship that is present
    and is not a name is a corrupt record; a relationship naming another project is not this
    project's business whatever the record says; and an absent or null relationship is neither,
    because the observer answers unmanaged before it has resolved a relationship at all. Only
    then is the state read: the three that settled the question are absorbed, unmeasured is a
    gap, unreported raises the obligation, and anything outside that vocabulary is a reading
    this cannot read. Deriving the order from that table rather than from the branches is what
    two audit rounds on this function asked for; each of them found a case the branch order
    had missed.

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
    # Held across BOTH loops. A block's subject is its cause rather than its event, so two
    # events stating one unresolved block derive one obligation - and appending per event
    # returned that obligation twice, which is the duplicate this keying exists to remove.
    seen = set()
    for relation, about in relations.items():
        for row in store.all(
            "SELECT event_id FROM events WHERE relationship_id = ?"
            " ORDER BY first_seen_at", (relation,),
        ):
            one = from_event(store, row["event_id"], read_work_report(store, row["event_id"]))
            if one is None:
                continue
            if one["obligationId"] in seen:
                continue
            decided = select(store, one, recipient=None)
            if decided["standing"] == STANDING:
                seen.add(one["obligationId"])
                obligations.append({**one, "decision": decided,
                                    "relationshipStatus": about["status"],
                                    "supersededBy": about["superseded_by"]})
    gaps = []
    for reading in observations:
        if not isinstance(reading, dict):
            _carry(gaps, unusable_reading(reading))
            continue
        about = reading.get("relationshipId")
        if about is not None and not _named(about):
            # Unhashable before it is foreign: a list here raised on the membership test below
            # and aborted the whole answer, so one malformed entry decided what a project owed.
            # An absent or null relationship is a different thing and is left to the state:
            # omitted.observe answers unmanaged before it has resolved one, so refusing every
            # scope-less reading here is what called the ordinary reading unusable.
            _carry(gaps, unusable_reading(reading))
            continue
        if _named(about) and about not in relations:
            # A reading about somebody else's project is not this project's business, whatever
            # it declares, and accepting it would let a caller's list decide what a project
            # owes - including by putting another project's unreadable file in this answer.
            continue
        state = observed_state(reading)
        if state is None:
            # Not an object under this schema, or a reportingState nobody here can interpret.
            _carry(gaps, unusable_reading(reading))
            continue
        if state in OBSERVED_SETTLED:
            # The ordinary reading: the turn reported, it is still running, or this relay never
            # managed it. Nothing is owed because of it and nothing failed to be established,
            # so it is neither an obligation nor a gap.
            continue
        if state == OBSERVED_UNMEASURED:
            _carry(gaps, unmeasured_gap(reading))
            continue
        one = from_observation(reading)
        if one is None:
            # unreported is the one state that needs an owner, because it is the one that
            # raises something somebody owes, and this reading names no usable one.
            _carry(gaps, unusable_reading(reading))
            continue
        if one["obligationId"] in seen:
            continue
        decided = select(store, one, recipient=None)
        if decided["standing"] != STANDING:
            # The same rule the event loop above applies. A discharged omission - its Linear
            # record confirmed on the target the supervisor currently reads - is not standing,
            # and a list called standing that carried it asked its reader to check every
            # entry's decision to learn what the list's own name had already claimed.
            continue
        seen.add(one["obligationId"])
        obligations.append({**one, "decision": decided,
                            "relationshipStatus": (relations.get(one["relationId"]) or {})
                            .get("status")})
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
