"""What a message has to CARRY, once the envelope has said what it is.

relay-envelope/1 answers which relation a message belongs to, which direction it travels,
what the recipient owes because it arrived, and how far it actually got. It deliberately
stops there. It does not know that an assignment without a criteria digest is an
instruction nobody can be judged against, or that a correction without the generation it
opens produces a receipt the relay then refuses, or that a resume which omits the workflow
has silently dropped the one field no transport carries.

This module is the other half, for the parent-child relation. A purpose declares the typed
data it cannot do without; a packet missing any of it is refused BY FIELD NAME rather than
accepted and discovered later. And a packet that carries everything is still only a claim,
so the second half of this module compares it against the record the receiver read for
itself and returns the disagreements one at a time.

Three dispositions, and the middle one is the point. ACCEPTED means the packet agreed with
the record. REFUSED means it contradicted it, naming which field and both values. UNAVAILABLE
means the record could not answer - and that is neither of the other two, because a receiver
that cannot check something has not checked it, and folding that into acceptance is how an
unverifiable instruction becomes an applied one.

Nothing here sends, queues, acknowledges or judges anything. The engine for that already
exists in delivery, ack and criteria; duplicating it would give the workflow a second writer.
"""

import json

from . import cxc, envelope
from .errors import RefusalReason, RelayError
from .identity import sha256_hex

VERSION = "relay-packet/1"


class PacketRefused(RelayError):
    """A packet omitted what its own purpose cannot do without, or was malformed."""


# ------------------------------------------------------------------ the typed fields

ISSUE = "issue"
GENERATION = "generation"
CRITERIA_DIGEST = "criteriaDigest"
POLICY = "policy"
CALLBACK = "callback"
ARTIFACT = "artifact"
EVIDENCE = "evidence"
BODY = "body"
DECISION = "decision"
CORRELATION = "correlationId"

# What each occasion cannot do without, and nothing more. The restraint matters as much as
# the requirement: demanding a generation from an assignment would refuse every legitimate
# first dispatch, because a newly created child's registration needs a task id that creation
# has not returned yet and the assignment is what gets sent before it exists.
REQUIRED_BY_PURPOSE = {
    (envelope.PARENT_TO_CHILD, "assignment"): (ISSUE, CRITERIA_DIGEST, POLICY, CALLBACK, BODY),
    # The generation here is the one the verdict OPENS, not the one being superseded, and a
    # correction that names the current one names the generation the child has just stopped
    # working in. The artifact is what is being corrected, so it is named too.
    (envelope.PARENT_TO_CHILD, "revision_request"): (
        ISSUE, GENERATION, CRITERIA_DIGEST, CALLBACK, ARTIFACT),
    # A resume restates what the coordinator holds and the task cannot reconstruct alone.
    # POLICY is required because a transport carries model and effort as settings and has no
    # field for the workflow at all, so a resume that omits it has dropped it in silence.
    (envelope.PARENT_TO_CHILD, "resume"): (ISSUE, POLICY, CALLBACK, ARTIFACT),
    # An answer that owes nothing still has to say which message it answers, or it is a
    # notification the recipient cannot attach to anything.
    (envelope.PARENT_TO_CHILD, "receipt_confirmation"): (ISSUE, CORRELATION),
    (envelope.PARENT_TO_CHILD, "acceptance"): (ISSUE, CRITERIA_DIGEST, ARTIFACT),
    (envelope.PARENT_TO_CHILD, "integration_result"): (ISSUE, ARTIFACT),
    (envelope.CHILD_TO_PARENT, "completion"): (
        ISSUE, GENERATION, CRITERIA_DIGEST, ARTIFACT, EVIDENCE),
    (envelope.CHILD_TO_PARENT, "review_ready"): (
        ISSUE, GENERATION, CRITERIA_DIGEST, ARTIFACT, EVIDENCE),
    # A progress note owes nothing and is required to invent nothing. Demanding a head from
    # one is how a child with nothing to show yet learns to supply a plausible value.
    (envelope.CHILD_TO_PARENT, "progress"): (ISSUE,),
    # A block names what is in the way, and the evidence is where that is readable. Without
    # it the parent is told there is a problem and not where to look at it.
    (envelope.CHILD_TO_PARENT, "blocked"): (ISSUE, EVIDENCE),
    (envelope.CHILD_TO_PARENT, "decision_request"): (ISSUE, DECISION, EVIDENCE),
}


def required_for(direction, purpose) -> tuple:
    """What this occasion must carry. Refuses a pairing the envelope does not have."""
    envelope.kind_of(direction, purpose)
    try:
        return REQUIRED_BY_PURPOSE[(direction, purpose)]
    except KeyError:
        raise PacketRefused(
            RefusalReason.MALFORMED_RECEIPT,
            "relay-packet/1 covers the parent and child relation; "
            + direction + " is carried by the envelope alone",
        ) from None


def _present(value):
    """The value, or None when nothing was said. A stated absence is not a value."""
    if value is None or envelope.is_absent(value):
        return None
    if isinstance(value, str) and not value.strip():
        return None
    if isinstance(value, (list, tuple, dict)) and not value:
        return None
    return value


# ---------------------------------------------------------------------- the artifact

PULL_REQUEST = "pull_request"
LOCATOR = "locator"
ARTIFACT_KINDS = (PULL_REQUEST, LOCATOR)

# What each shape cannot do without. Held here rather than only inside the two constructors,
# because a packet read back from disk was never constructed: packet-check loads JSON, and
# every rule that lived only in a constructor was a rule that path did not have.
ARTIFACT_REQUIRED = {
    PULL_REQUEST: ("repository", "number", "headSha"),
    LOCATOR: ("path", "digest"),
}


def pull_request(*, repository, number, head_sha, base_sha=None, url=None) -> dict:
    """A change under review, named the way the parent will re-read it before merging.

    The repository travels beside the number because the same number on two projects is two
    different pull requests, and the head travels because a report that names a pull request
    and not a commit is silently inherited by the next push to it.
    """
    for name, value in (("repository", repository), ("number", number),
                        ("head_sha", head_sha)):
        if _present(value) is None:
            raise PacketRefused(
                RefusalReason.MALFORMED_RECEIPT,
                "a pull request artifact states its " + name)
    return {"kind": PULL_REQUEST, "repository": str(repository), "number": number,
            "headSha": str(head_sha), "baseSha": base_sha, "url": url}


def locator(*, path, digest, produced_at=None) -> dict:
    """A non-PR deliverable, identified by where it is and what its bytes hash to.

    This exists so a research, design or verification assignment has something true to put
    in the artifact field. Before it, the only shape available was a pull request, and a
    child with no repository change either invented one or left the field empty - which is
    the empty commit and the fake head the audit work is not supposed to produce.
    """
    for name, value in (("path", path), ("digest", digest)):
        if _present(value) is None:
            raise PacketRefused(
                RefusalReason.MALFORMED_RECEIPT,
                "an artifact locator states its " + name
                + "; a deliverable nobody can hash is not one this can identify")
    return {"kind": LOCATOR, "path": str(path), "digest": str(digest),
            "producedAt": produced_at}


def _check_artifact(one):
    if not isinstance(one, dict) or one.get("kind") not in ARTIFACT_KINDS:
        raise PacketRefused(
            RefusalReason.MALFORMED_RECEIPT,
            "an artifact is a pull request or a locator; a third shape cannot reach a reader"
            " as either")
    missing = [name for name in ARTIFACT_REQUIRED[one["kind"]]
               if _present(one.get(name)) is None]
    if missing:
        raise PacketRefused(
            RefusalReason.MALFORMED_RECEIPT,
            "a " + one["kind"] + " artifact states " + ", ".join(missing)
            + "; a deliverable identified by half its identity is not identified")
    return one


# ------------------------------------------------------------------------ the policy

POLICY_FIELDS = ("model", "effort", "sandbox", "approval", "workflow")


def policy(*, model, effort, workflow, sandbox=None, approval=None) -> dict:
    """The settings and the workflow, together, because only one of them has a transport field.

    Model, effort, sandbox and approval are creation arguments a receipt reads back. The
    workflow is not: no transport carries it, so a message that does not say it has dropped
    it, and the recipient's own reading cannot recover what it was told to run under. Keeping
    the five in one record is what makes that omission a refusal instead of a silence.
    """
    for name, value in (("model", model), ("effort", effort), ("workflow", workflow)):
        if _present(value) is None:
            raise PacketRefused(
                RefusalReason.MALFORMED_RECEIPT,
                "a policy states its " + name
                + "; an unstated one is read as whatever the recipient already had")
    return {"model": str(model), "effort": str(effort), "workflow": str(workflow),
            "sandbox": sandbox, "approval": approval}


# -------------------------------------------------------------------- CXC activation

# Three facts, and they are routinely read as one. The prompt naming a workflow, the child
# having actually armed it, and a host goal being active are produced by three different
# parties and none of them implies another. dispatch-verification.md holds the same
# separation as four verdicts; this is the record form of it.
INSTRUCTED = "instructed"
ACTIVATED = "activated"
NATIVE_GOAL = "nativeGoal"
ACTIVATION_FACTS = (INSTRUCTED, ACTIVATED, NATIVE_GOAL)

OBSERVED = "observed"
ABSENT = "absent"
REFUSED = "refused"
UNVERIFIED = "unverified"
INAPPLICABLE = "not_applicable"
ACTIVATION_STATES = (OBSERVED, ABSENT, REFUSED, UNVERIFIED, INAPPLICABLE)

# What a task is running, which decides which of the three facts can answer at all. A
# coordination parent holds a native goal and no implementation FSM BY DESIGN, and a non-Loop
# audit was authorised to hold neither, so demanding an activation record from either is
# reading a normal state as a defect.
LOOP = "loop"
NON_LOOP = "non_loop"
COORDINATION = "coordination"
MODES = (LOOP, NON_LOOP, COORDINATION)


def activation_fact(state, *, source=None, detail="") -> dict:
    """One of the three, with the record that answered it.

    observed, absent and refused each need a source for the same reason a reach stage does:
    without one the answer is somebody's reading of a transcript, and unverified is the honest
    word for that.
    """
    if state not in ACTIVATION_STATES:
        raise PacketRefused(
            RefusalReason.MALFORMED_RECEIPT,
            repr(state) + " is not an activation state; it is one of "
            + ", ".join(sorted(ACTIVATION_STATES)))
    if state in (OBSERVED, ABSENT, REFUSED) and not source:
        raise PacketRefused(
            RefusalReason.MALFORMED_RECEIPT,
            "an " + state + " activation fact names the record that says so; without one it"
            " is unverified")
    return {"state": state, "source": source, "detail": detail}


def unexamined(mode) -> dict:
    """The honest starting triple for a mode: inapplicable where the mode has no such thing."""
    if mode not in MODES:
        raise PacketRefused(
            RefusalReason.MALFORMED_RECEIPT,
            repr(mode) + " is not an execution mode; it is one of " + ", ".join(sorted(MODES)))
    if mode == LOOP:
        return {name: activation_fact(UNVERIFIED, detail="nothing readable answered yet")
                for name in ACTIVATION_FACTS}
    if mode == COORDINATION:
        return {
            INSTRUCTED: activation_fact(UNVERIFIED, detail="nothing readable answered yet"),
            ACTIVATED: activation_fact(
                INAPPLICABLE,
                detail="a coordination parent schedules on its own goal and persists no"
                       " implementation FSM, so there is nothing here to have armed"),
            NATIVE_GOAL: activation_fact(UNVERIFIED, detail="nothing readable answered yet"),
        }
    return {
        INSTRUCTED: activation_fact(UNVERIFIED, detail="nothing readable answered yet"),
        ACTIVATED: activation_fact(
            INAPPLICABLE,
            detail="an authorised non-Loop assignment arms no loop, so an absent one is the"
                   " agreed shape rather than a finding"),
        NATIVE_GOAL: activation_fact(
            INAPPLICABLE, detail="no goal was asked for on this assignment"),
    }


def activation_class(triple, *, mode=LOOP, earlier=None) -> dict:
    """Which of dispatch-verification's classes this triple actually is.

    The ordering is the finding. L0 is settled by the prompt alone and no later state puts an
    invocation into a message already sent, so it is read first. L2 is a refusal somebody
    recorded, which is equally unchangeable. Only then do the negative readings get looked at,
    and the last of them is L6 rather than L1, because a child still inside its first turn
    reads exactly like a child that never armed anything and the two separate only later.
    """
    if mode not in MODES:
        raise PacketRefused(
            RefusalReason.MALFORMED_RECEIPT,
            repr(mode) + " is not an execution mode; it is one of " + ", ".join(sorted(MODES)))
    for name in ACTIVATION_FACTS:
        if name not in triple:
            raise PacketRefused(
                RefusalReason.MALFORMED_RECEIPT,
                "an activation reading answers all three facts; " + name + " is missing."
                " Start from unexamined(mode) rather than from a partial dictionary")
    instructed = triple[INSTRUCTED]["state"]
    activated = triple[ACTIVATED]["state"]
    goal = triple[NATIVE_GOAL]["state"]
    if instructed == ABSENT:
        return _class("L0", "the assignment read back from this dispatch carries no"
                            " invocation and names no agreed alternative")
    if activated == REFUSED:
        return _class("L2", "binding or initialisation was attempted and refused, and the"
                            " refusal itself is the evidence")
    if activated == INAPPLICABLE:
        return _class("L5", "this mode arms no implementation FSM, so there is nothing"
                            " missing; " + triple[ACTIVATED]["detail"])
    if activated == OBSERVED:
        return _class("L5", "the task's own bound goalplan and persisted phases answer for"
                            " this assignment")
    if _was_armed(earlier) and activated == ABSENT:
        return _class("L4", "activation was observed earlier on this assignment and the"
                            " later reading runs without it, so it was lost rather than"
                            " never made")
    if activated == ABSENT and goal == OBSERVED:
        return _class("L3", "a host goal is active and no goalplan or persisted phase"
                            " answers, which is an unarmed loop wearing an active status")
    if activated == ABSENT and instructed == OBSERVED:
        return _class("L1", "the invocation was sent and nothing records it being loaded,"
                            " bound or invoked")
    return _class("L6", "nothing readable yet distinguishes the classes above; a single"
                        " negative reading of activation state is not a verdict")


def _class(name, reason) -> dict:
    return {"class": name, "reason": reason}


def _was_armed(earlier):
    """Whether an earlier reading of this same assignment held activation.

    Deliberately not a predicate anybody else calls. It answers one question for one branch
    above, and a reading that was itself unverified is not earlier evidence of anything.
    """
    if not earlier:
        return False
    return (earlier.get(ACTIVATED) or {}).get("state") == OBSERVED


# ------------------------------------------------------------------------ the packet

def compose(*, direction, purpose, relation_id, sender, recipient, subject, issue=None,
            generation=None, criteria_digest=None, policy_record=None, callback=None,
            artifact=None, evidence=(), body=None, activation=None, decision=None,
            correlation_id=None, reply_to=None, relation_revision=None, scope=None,
            basis=None, observed_at=None, reach=None) -> dict:
    """One packet: the shared envelope, and the typed data this occasion cannot do without.

    The envelope is built by envelope.region rather than restated here, so there is one
    definition of what identifies a message and this module cannot drift from it.
    """
    required = required_for(direction, purpose)
    region = envelope.region(
        direction=direction, purpose=purpose, relation_id=relation_id, sender=sender,
        recipient=recipient, subject=subject, observed_at=observed_at,
        relation_revision=relation_revision, scope=scope, basis=basis,
        evidence=[one.get("path", one) if isinstance(one, dict) else one for one in evidence],
        correlation_id=correlation_id, reply_to=reply_to, decision=decision, reach=reach)
    one = {
        "version": VERSION,
        "envelope": region,
        ISSUE: issue,
        GENERATION: generation,
        CRITERIA_DIGEST: criteria_digest,
        POLICY: policy_record,
        CALLBACK: callback,
        ARTIFACT: _check_artifact(artifact) if _present(artifact) is not None else None,
        EVIDENCE: list(evidence),
        BODY: body,
        "activation": activation,
    }
    check(one, required=required)
    return one


def check(one, *, required=None) -> None:
    """Refuse a packet that omits what its own purpose cannot do without.

    A COMPLETE validator, not a finishing touch on something compose already made safe. The
    two entry points are not the same: compose builds a packet here, where every constructor
    has already run, and packet-check reads one back from disk, where none of them has. Every
    rule that lived only in a constructor was a rule the second path did not have, so each one
    is re-run from this side - the version, the artifact's own fields and the activation
    facts - against whatever the mapping actually contains.
    """
    region = one.get("envelope") or {}
    if one.get("version") != VERSION:
        # Refused rather than read hopefully. A newer packet may mean something different by
        # a field this build already knows the name of, and reading it under these rules is
        # the silent misinterpretation the refusal exists to prevent.
        raise PacketRefused(
            RefusalReason.MALFORMED_RECEIPT,
            "this reader is " + VERSION + " and the packet says " + repr(one.get("version"))
            + "; a version nobody mapped is diagnosed rather than read under these rules")
    envelope.check(region)
    direction, purpose = region.get("direction"), region.get("purpose")
    if required is None:
        required = required_for(direction, purpose)
    for name in required:
        if name == CORRELATION:
            value = region.get("correlationId")
        elif name == DECISION:
            value = region.get("decision")
        else:
            value = one.get(name)
        if _present(value) is None:
            raise PacketRefused(
                RefusalReason.MALFORMED_RECEIPT,
                "a " + str(purpose) + " packet cannot omit " + name + ": "
                + envelope.shown(value) + ". It is one of "
                + ", ".join(required) + ", which this occasion is read against")
    if _present(one.get(POLICY)) is not None:
        missing = [name for name in ("model", "effort", "workflow")
                   if _present((one[POLICY] or {}).get(name)) is None]
        if missing:
            raise PacketRefused(
                RefusalReason.SETTINGS_INCOMPLETE,
                "the policy states " + ", ".join(missing) + " as nothing; the workflow in"
                " particular has no transport field, so an unstated one is dropped rather"
                " than defaulted")
    if _present(one.get(ARTIFACT)) is not None:
        _check_artifact(one[ARTIFACT])
    if _present(one.get(BODY)) is not None:
        missing = cxc.dispatch_problems(one[BODY])
        if missing:
            raise PacketRefused(
                RefusalReason.MALFORMED_RECEIPT,
                "the instruction body is missing " + ", ".join(missing)
                + "; DISPATCH-TASK-01 fixes these sections because an instruction that omits"
                " one is an instruction the recipient has to guess at")
    if _present(one.get("activation")) is not None:
        triple = one["activation"]
        if not isinstance(triple, dict):
            raise PacketRefused(
                RefusalReason.MALFORMED_RECEIPT,
                "an activation reading is an object of three named facts, not a "
                + type(triple).__name__)
        for name in ACTIVATION_FACTS:
            if name not in triple:
                raise PacketRefused(
                    RefusalReason.MALFORMED_RECEIPT,
                    "an activation reading answers all three facts; " + name + " is missing")
            fact = triple[name]
            if not isinstance(fact, dict):
                raise PacketRefused(
                    RefusalReason.MALFORMED_RECEIPT,
                    "the " + name + " activation fact is an object with a state, not a "
                    + type(fact).__name__)
            # The constructor IS the rule, so it is re-run rather than restated. A second
            # spelling of "observed needs a source" is how the two start disagreeing.
            activation_fact(fact.get("state"), source=fact.get("source"),
                            detail=fact.get("detail") or "")


# ---------------------------------------------------------------- agreement with the record

ACCEPTED = "accepted"
REFUSAL = "refused"
UNAVAILABLE = "unavailable"
DISPOSITIONS = (ACCEPTED, REFUSAL, UNAVAILABLE)

WRONG_RELATION = "wrong_relation"
WRONG_SENDER = "wrong_sender"
WRONG_RECIPIENT = "wrong_recipient"
SUPERSEDED_RELATION = "superseded_relation"
WRONG_ISSUE = "wrong_issue"
STALE_GENERATION = "stale_generation"
STALE_CRITERIA = "stale_criteria_digest"
STALE_HEAD = "stale_head"
REFUSED_SETTINGS = "refused_settings"
UNREADABLE = "unreadable"


def mismatch(kind, field, *, expected, found, reason) -> dict:
    """One disagreement, with both values, so a reader never has to go and get the other."""
    return {"kind": kind, "field": field, "expected": expected, "found": found,
            "reason": reason}


def reception(one, record) -> dict:
    """What a receiver may do with this packet, judged against the record it read itself.

    The record is the receiver's OWN reading - its relationship row, its current generation,
    its registered criteria, the head its forge reports. Nothing in the packet is evidence of
    itself: these transports carry opaque text and no authenticated caller, so a title, a
    self-description or a plausible task id establishes nothing. What is compared is the
    packet against the row, field by field.

    A field the record cannot answer produces an UNREADABLE entry and the whole reading comes
    back unavailable. That is deliberately not acceptance. A receiver that could not check
    the generation has not checked it, and an instruction applied on that basis was applied
    on nobody's authority.

    The packet is checked for completeness FIRST. Comparing selected fields against a record
    says nothing about the fields nobody compared, so a packet arriving from disk with its
    required data missing used to reach this function and come back accepted on the strength
    of the few values that did agree.
    """
    region = one.get("envelope") or {}
    check(one)
    problems, gaps = [], []
    _compare(problems, gaps, WRONG_RELATION, "relationId",
             region.get("relationId"), record.get("relationId"),
             "a packet naming another relationship belongs to another assignment")
    sender_role, _recipient_role = envelope.ENDPOINT_ROLES[region["direction"]]
    expected_sender = record.get("parentTaskId") if sender_role == "parent" \
        else record.get("childTaskId")
    expected_recipient = record.get("childTaskId") if sender_role == "parent" \
        else record.get("parentTaskId")
    _compare(problems, gaps, WRONG_SENDER, "sender.taskId",
             (region.get("sender") or {}).get("taskId"), expected_sender,
             "the registered pair is what says who may send this, not the message's own"
             " account of itself")
    _compare(problems, gaps, WRONG_RECIPIENT, "recipient.taskId",
             (region.get("recipient") or {}).get("taskId"), expected_recipient,
             "a packet addressed to another task is not this task's instruction")
    _compare(problems, gaps, WRONG_ISSUE, ISSUE, one.get(ISSUE), record.get(ISSUE),
             "the issue binding is what makes this the assignment it claims to be")
    if _present(record.get("relationRevision")) is not None:
        _compare(problems, gaps, SUPERSEDED_RELATION, "relationRevision",
                 region.get("relationRevision"), record.get("relationRevision"),
                 "a superseded link is preserved and not rewritten, so a message quoting the"
                 " old revision is about a relationship that has been replaced")
    required = required_for(region["direction"], region["purpose"])
    if GENERATION in required:
        _compare(problems, gaps, STALE_GENERATION, GENERATION,
                 one.get(GENERATION), record.get(GENERATION),
                 "a receipt emitted under a generation the assignment is not in is refused,"
                 " so a message naming one produces work that cannot be handed back")
    if CRITERIA_DIGEST in required:
        _compare(problems, gaps, STALE_CRITERIA, CRITERIA_DIGEST,
                 one.get(CRITERIA_DIGEST), record.get(CRITERIA_DIGEST),
                 "criteria judged against a digest nobody registered are judged against"
                 " somebody's memory of them")
    _artifact_agreement(one, record, problems, gaps)
    problems.extend(_settings_problems(one, record))
    gaps.extend(_settings_gaps(one, record))
    if problems:
        disposition = REFUSAL
    elif gaps:
        disposition = UNAVAILABLE
    else:
        disposition = ACCEPTED
    return {"version": VERSION, "disposition": disposition,
            "messageId": region.get("messageId"), "purpose": region.get("purpose"),
            "mismatches": problems, "gaps": gaps}


def _compare(problems, gaps, kind, field, found, expected, reason) -> None:
    """One field against the record, with a record that cannot answer kept separate."""
    if _present(expected) is None:
        gaps.append(mismatch(UNREADABLE, field, expected=None, found=found,
                             reason="the record the receiver read says nothing about "
                                    + field + ", so this could not be checked"))
        return
    if _present(found) is None:
        problems.append(mismatch(kind, field, expected=expected, found=None,
                                 reason="the packet states no " + field + ", and " + reason))
        return
    if str(found) != str(expected):
        problems.append(mismatch(kind, field, expected=expected, found=found, reason=reason))


def _artifact_agreement(one, record, problems, gaps) -> None:
    """The artifact against what the receiver's own reading says is current.

    Each shape is compared against its OWN currency field and only that one. A pull request is
    measured against the head the receiver's forge reading reports; a locator against the
    digest. That is what keeps a non-PR audit from being made to look stale for having no
    head: it is never compared against one and never asked for one.

    Routed through _compare rather than answering for itself, which is the correction. A
    record with no head read as agreement, so a candidate whose currency the receiver could
    not check came back accepted. It is a gap now, and the reading is unavailable. An
    unchecked head and a matching head are not the same news.
    """
    artifact = one.get(ARTIFACT)
    if not artifact:
        return
    if artifact.get("kind") == PULL_REQUEST:
        _compare(problems, gaps, STALE_HEAD, "artifact.headSha",
                 artifact.get("headSha"), record.get("headSha"),
                 "the candidate moved after this packet was written, so its checks, its"
                 " review and its readiness are about another commit")
        return
    _compare(problems, gaps, STALE_HEAD, "artifact.digest",
             artifact.get("digest"), record.get("artifactDigest"),
             "the deliverable's bytes are not the ones this packet names")


def _settings_problems(one, record) -> list:
    """A model and effort pair the record says was refused for this role.

    A stale pair coming back is not a provider fault and is not repaired by creating another
    child. It is a pair this assignment is not authorised to run under, and saying so by name
    is what stops it being reclassified into something that looks retryable.
    """
    settings = one.get(POLICY)
    if not settings:
        return []
    for pair in record.get("refusedPolicies") or ():
        if (str(pair.get("model")) == str(settings.get("model"))
                and str(pair.get("effort")) == str(settings.get("effort"))):
            return [mismatch(REFUSED_SETTINGS, "policy.model/effort",
                             expected=record.get("policy"), found=settings,
                             reason=pair.get("reason") or "this pair is recorded refused for"
                             " this role, which is a settings answer rather than a provider"
                             " failure and is not worked around with a second child")]
    return []


def _settings_gaps(one, record) -> list:
    if one.get(POLICY) and _present(record.get("refusedPolicies")) is None \
            and "refusedPolicies" not in record:
        return [mismatch(UNREADABLE, "policy", expected=None, found=one[POLICY],
                         reason="the receiver read no settings record, so whether this pair"
                                " is authorised for this role is unchecked")]
    return []


# --------------------------------------------------------------------- a message arriving twice

FIRST = "first"
REPLAY = "replay"
COLLISION = "collision"

# What makes two packets the same instruction. Identity alone is not enough: the message id
# is derived from the direction, relation, purpose and subject, so a sender can spend one in
# advance, and answering a repeat from the id alone would hand a second correction the
# disposition given to the first.
#
# So the digest covers everything that can change what the recipient does, and it is built by
# EXCLUSION rather than by a list of interesting fields. A hand-picked list is how a changed
# packet collapses into a replay: an earlier version of this hashed the generation, the head
# and the workflow, so a correction naming a different callback, a different repository or a
# different decision hashed identically to the one already answered and was handed that
# answer. What is left out is named below, one reason each.
IMMATERIAL = (
    # When the sender looked. A repeat observed a minute later is the same instruction.
    "observedAt",
    # A display field the sender may or may not have resolved, deliberately not an input to
    # the message id either.
    "scope",
    # A rendering of the generation and revision, both of which are hashed from the packet's
    # own typed fields.
    "basis",
    # How far a message got, which is the receiver's reading rather than the sender's ask.
    "reach",
    # Derived from everything else here, so including it would hash one thing twice.
    "messageId",
)


def content_digest(one) -> str:
    """What this packet actually asks for, hashed, so a repeat can be told from a collision.

    Serialised with sorted keys so two equal packets hash equal however their mappings were
    built, and with default=str so a value this module does not model cannot raise out of a
    comparison whose whole job is to be answerable.
    """
    region = dict(one.get("envelope") or {})
    for name in IMMATERIAL:
        region.pop(name, None)
    payload = {name: value for name, value in one.items()
               if name not in ("envelope",)}
    payload["envelope"] = region
    return sha256_hex(json.dumps(payload, sort_keys=True, default=str,
                                 separators=(",", ":"), ensure_ascii=False))


def repeat(one, answered) -> dict:
    """Whether this packet has been answered before, and whether it is the same one.

    Three answers, not two. A packet whose id and content both match is a REPLAY and is given
    back the disposition it already got, which is what makes an uncertain send safe to settle
    by asking rather than by sending again. A packet reusing an id while asking for something
    else is a COLLISION: it is neither a replay nor a new instruction, and answering it with
    the earlier disposition would apply a decision to a request nobody made.
    """
    region = one.get("envelope") or {}
    identifier = region.get("messageId")
    prior = (answered or {}).get(identifier)
    if not prior:
        return {"state": FIRST, "messageId": identifier, "contentDigest": content_digest(one)}
    mine = content_digest(one)
    if str(prior.get("contentDigest")) == mine:
        return {"state": REPLAY, "messageId": identifier, "contentDigest": mine,
                "disposition": prior.get("disposition"),
                "reason": "this id was answered already and asks for the same thing, so it is"
                          " given the same answer rather than acted on twice"}
    return {"state": COLLISION, "messageId": identifier, "contentDigest": mine,
            "disposition": prior.get("disposition"),
            "reason": "this id was answered already and asks for something else. It is raised"
                      " rather than given the earlier answer, which is also what stops a"
                      " predictable id being spent in advance to suppress the real request"}


# -------------------------------------------------------------- the seven states of a handover

# Seven questions, asked in the order the work travels, and every one of them has been read as
# one of the others at some point. The envelope's five stages are about ONE MESSAGE getting
# somewhere; these are about the assignment, and they run past the message into what the
# parent then did and what Linear ended up holding.
READ = "read"
TRANSPORT_ACCEPTED = "transport_accepted"
RELAY_ACK = "relay_ack"
CRITERIA_VERDICT = "criteria_verdict"
PARENT_ACCEPTANCE = "parent_acceptance"
MERGE_LANDING = "merge_landing"
LINEAR_DONE = "linear_done"
PROGRESSION = (READ, TRANSPORT_ACCEPTED, RELAY_ACK, CRITERIA_VERDICT, PARENT_ACCEPTANCE,
               MERGE_LANDING, LINEAR_DONE)

# Which record answers each one, and where none does. read is first and has no mechanism at
# all: no row in this store says a recipient read anything, and a model writing "received,
# understood" is prose. Saying so here is what stops it being the state everybody assumes.
PROGRESSION_SOURCES = {
    READ: None,
    TRANSPORT_ACCEPTED: "attempts",
    RELAY_ACK: "acks",
    CRITERIA_VERDICT: "verdicts",
    PARENT_ACCEPTANCE: "verdicts",
    MERGE_LANDING: "merge_turns",
    LINEAR_DONE: "sync_outbox",
}

NO_PROGRESSION_MECHANISM = {
    READ: "no row in this store records a recipient reading anything; an acknowledgement is"
          " the nearest fact and it is the next state, not this one",
}


def unobserved() -> dict:
    """The honest starting ladder: impossible where nothing could ever answer."""
    return {
        name: envelope.stage(envelope.UNMEASURED, detail="nothing readable answered yet")
        if PROGRESSION_SOURCES[name]
        else envelope.stage(envelope.IMPOSSIBLE, detail=NO_PROGRESSION_MECHANISM[name])
        for name in PROGRESSION
    }


def check_progression(ladder) -> None:
    """Refuse a ladder answering with a record this workflow does not read for that state."""
    for name in PROGRESSION:
        if name not in ladder:
            raise PacketRefused(
                RefusalReason.MALFORMED_RECEIPT,
                "a handover ladder answers every state; " + name + " is missing. Start from"
                " unobserved() rather than from a partial dictionary")
        entry = ladder[name] or {}
        state = entry.get("state")
        if state not in envelope.STATES:
            raise PacketRefused(
                RefusalReason.MALFORMED_RECEIPT,
                repr(state) + " is not a state; it is one of " + ", ".join(
                    sorted(envelope.STATES)))
        declared = PROGRESSION_SOURCES[name]
        if declared is None:
            if state != envelope.IMPOSSIBLE:
                raise PacketRefused(
                    RefusalReason.MALFORMED_RECEIPT,
                    "nothing answers " + name + ", so it cannot say " + repr(state) + ": "
                    + NO_PROGRESSION_MECHANISM[name])
        elif state in (envelope.YES, envelope.NO, envelope.CONDITIONAL) \
                and entry.get("source") != declared:
            raise PacketRefused(
                RefusalReason.MALFORMED_RECEIPT,
                name + " is answered by " + declared + ", not by "
                + repr(entry.get("source")))


def unsupported_promotions(ladder) -> list:
    """States standing above one that is not held, which is what a false promotion looks like.

    Returned rather than raised: an inconsistent ladder is usually somebody's reading of a real
    store, and the useful thing is to name the step that was skipped. A state with no mechanism
    is stepped over rather than treated as a missing prerequisite, for the reason the envelope
    steps over its own: read can never be held, and treating it as unheld would report every
    legitimate acknowledgement as a promotion.
    """
    check_progression(ladder)
    out, held = [], True
    for name in PROGRESSION:
        state = (ladder.get(name) or {}).get("state")
        if state == envelope.IMPOSSIBLE:
            continue
        if state == envelope.YES and not held:
            out.append(name)
        held = state == envelope.YES
    return out


def progression_lines(ladder) -> list:
    """The seven, each with the record that answered it. Never collapsed into one word."""
    out = ["  handover:"]
    for name in PROGRESSION:
        entry = ladder[name]
        source = " (" + entry["source"] + ")" if entry.get("source") else ""
        detail = " - " + entry["detail"] if entry.get("detail") else ""
        out.append("    " + name + ": " + entry["state"] + source + detail)
    return out


def packet_lines(one) -> list:
    """The whole packet on a surface with no block of its own: envelope first, then the data."""
    lines = list(envelope.region_lines(one["envelope"]))
    for name in (ISSUE, GENERATION, CRITERIA_DIGEST):
        if _present(one.get(name)) is not None:
            lines.append("  " + name + ": " + str(one[name]))
    settings = one.get(POLICY)
    if settings:
        lines.append("  workflow: " + str(settings.get("workflow"))
                     + "  model: " + str(settings.get("model"))
                     + "  effort: " + str(settings.get("effort")))
    if _present(one.get(CALLBACK)) is not None:
        lines.append("  answer to: " + str(one[CALLBACK]))
    artifact = one.get(ARTIFACT)
    if artifact and artifact.get("kind") == PULL_REQUEST:
        lines.append("  pull request: " + str(artifact.get("repository")) + " #"
                     + str(artifact.get("number")) + " at " + str(artifact.get("headSha")))
    elif artifact:
        lines.append("  artifact: " + str(artifact.get("path")) + " digest "
                     + str(artifact.get("digest")))
    triple = one.get("activation")
    if triple:
        for name in ACTIVATION_FACTS:
            entry = triple[name]
            source = " (" + entry["source"] + ")" if entry.get("source") else ""
            lines.append("  " + name + ": " + entry["state"] + source)
    return lines
