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

from . import cxc, envelope, settings
from .settings import normalise_policy as _normalise_policy
from .errors import RefusalReason, RelayError
from .identity import sha256_hex
from .registry import LIVE as LIVE_RELATION

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

# What each typed field IS, so a value of the wrong shape is refused rather than compared.
# Non-empty was the whole test before, and an object-valued generation compared equal to an
# equally malformed record value, so two wrong answers agreed and the reading came back
# accepted. A generation is a number and a callback is an object; the rest are text.
FIELD_TYPES = {
    ISSUE: str,
    GENERATION: int,
    CRITERIA_DIGEST: str,
    CALLBACK: dict,
    BODY: str,
}

# The region fields reception compares, and their shapes.
REGION_TYPES = {
    "relationId": str,
    "messageId": str,
    "subject": str,
    "correlationId": str,
    "relationRevision": int,
    "decision": str,
}

# What each occasion cannot do without, and nothing more. The restraint matters as much as
# the requirement: demanding a generation from an assignment would refuse every legitimate
# first dispatch, because a newly created child's registration needs a task id that creation
# has not returned yet and the assignment is what gets sent before it exists.
REQUIRED_BY_PURPOSE = {
    (envelope.PARENT_TO_CHILD, "assignment"): (ISSUE, CRITERIA_DIGEST, POLICY, CALLBACK, BODY),
    # The generation here is the one the verdict OPENS, not the one being superseded, and a
    # correction that names the current one names the generation the child has just stopped
    # working in. The artifact is what is being corrected, so it is named too.
    # And it says what it corrects: the body in the correction form (the violated criterion,
    # what changed, the fix scope, what to preserve, what to re-verify and when to return)
    # and the evidence it rests on. Without them a correction is answered from memory.
    (envelope.PARENT_TO_CHILD, "revision_request"): (
        ISSUE, GENERATION, CRITERIA_DIGEST, CALLBACK, ARTIFACT, BODY, EVIDENCE),
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
    # And what a parent owes the level above, on the same rules rather than a second
    # vocabulary. The restraint here is the restraint above. A completion upward is NOT
    # required to carry an artifact: a noop completion has none and a research assignment may
    # have only a locator, and demanding one is how a plausible pull request gets invented.
    # What it cannot do without is the issue that finished, the generation it finished in, and
    # where the result is readable.
    (envelope.PARENT_TO_SUPERVISOR, "completion"): (ISSUE, GENERATION, EVIDENCE),
    # A block and a decision carry evidence for the reason a child's do: a level above told
    # there is a problem and not where to look at it has been told half of it.
    (envelope.PARENT_TO_SUPERVISOR, "blocked"): (ISSUE, EVIDENCE),
    (envelope.PARENT_TO_SUPERVISOR, "decision_request"): (ISSUE, DECISION, EVIDENCE),
    # status_response is deliberately absent, and the absence is the honest answer rather
    # than an oversight. BODY here is a dispatch instruction, checked against DISPATCH-TASK-01
    # by cxc.dispatch_problems, so requiring it of an answer would refuse every real answer;
    # and supervision.status_answer returns a structured reading rather than prose. An
    # occasion with no field it cannot do without would be a row that admits anything, which
    # is what this table exists not to be. So an answer to a midpoint check is carried by the
    # envelope alone until somebody decides what an answer cannot do without.
}

# Which name in the receiver's own reading holds each role's task id. A map rather than two
# conditionals on the sender's role: those answered the parent-child pair correctly and would
# have compared a report upward against the child of the same relationship, which is a real
# task id belonging to somebody else and therefore the worst kind of wrong answer.
RECORD_TASK_KEY = {"parent": "parentTaskId", "child": "childTaskId",
                   "supervisor": "supervisorTaskId"}

# Which section form a body is read against. A correction has its own form; every other body
# is an instruction under DISPATCH-TASK-01, which is what assignment bodies always were.
BODY_SECTIONS = {
    (envelope.PARENT_TO_CHILD, "revision_request"): (cxc.correction_problems,
                                                      "the correction form"),
}
DEFAULT_BODY_SECTIONS = (cxc.dispatch_problems, "DISPATCH-TASK-01")


def required_for(direction, purpose) -> tuple:
    """What this occasion must carry. Refuses a pairing the envelope does not have."""
    envelope.kind_of(direction, purpose)
    try:
        return REQUIRED_BY_PURPOSE[(direction, purpose)]
    except KeyError:
        raise PacketRefused(
            RefusalReason.MALFORMED_RECEIPT,
            "relay-packet/1 covers the parent-child relation and what a parent owes upward; "
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
    if one["kind"] == PULL_REQUEST:
        number = one.get("number")
        if isinstance(number, bool) or not isinstance(number, int) or number < 1:
            raise PacketRefused(
                RefusalReason.MALFORMED_RECEIPT,
                "a pull request number is a positive integer, not a "
                + type(number).__name__)
    # Every other identity field is text, and optional ones text when present. A head of 123
    # agreed with a record holding 123 and was only a gap against "123": the packet's own
    # shape has to be refused before its value is compared with anything.
    texts, optional = ARTIFACT_TEXT[one["kind"]]
    wrong = [name for name in texts if not isinstance(one.get(name), str)]
    wrong += [name for name in optional
              if one.get(name) is not None and not isinstance(one[name], str)]
    if wrong:
        raise PacketRefused(
            RefusalReason.MALFORMED_RECEIPT,
            "a " + one["kind"] + " artifact states " + ", ".join(wrong) + " as text, not as "
            + ", ".join(type(one.get(name)).__name__ for name in wrong))
    return one


# The text fields of each artifact shape: (required, optional).
ARTIFACT_TEXT = {
    PULL_REQUEST: (("repository", "headSha"), ("baseSha", "url")),
    LOCATOR: (("path", "digest"), ("producedAt",)),
}


# ------------------------------------------------------------------------ the policy

POLICY_FIELDS = ("model", "effort", "sandbox", "approval", "workflow", "mode")


def policy(*, model, effort, workflow, mode, sandbox=None, approval=None) -> dict:
    """The settings and the workflow, together, because only one of them has a transport field.

    Model, effort, sandbox and approval are creation arguments a receipt reads back. The
    workflow is not: no transport carries it, so a message that does not say it has dropped
    it, and the recipient's own reading cannot recover what it was told to run under. Keeping
    the five in one record is what makes that omission a refusal instead of a silence.

    The mode is the machine-readable half of the workflow: loop, non_loop or coordination,
    which decides what an activation reading can answer. It is stated rather than inferred
    from the workflow's wording, because prose is exactly what this module refuses to parse
    into a fact; the wording is read only to refuse a contradiction (see _mode_problem).
    """
    for name, value in (("model", model), ("effort", effort), ("workflow", workflow),
                        ("mode", mode)):
        if _present(value) is None:
            raise PacketRefused(
                RefusalReason.MALFORMED_RECEIPT,
                "a policy states its " + name
                + "; an unstated one is read as whatever the recipient already had")
    return {"model": str(model), "effort": str(effort), "workflow": str(workflow),
            "mode": str(mode), "sandbox": sandbox, "approval": approval}


# ---------------------------------------------------------------------- the callback

CALLBACK_FIELDS = ("taskId", "model", "effort")


def callback(*, task_id, model, effort) -> dict:
    """Where to answer, and the pair the task being answered is authorised to run now.

    An object rather than a sentence, because the pair is the part that goes stale. A parent
    whose model the user changed runs another pair from then on, and a packet that still names
    the old one has to be refusable by name; "parent task X" could not be compared at all.
    """
    for name, value in (("task_id", task_id), ("model", model), ("effort", effort)):
        if _present(value) is None or not isinstance(value, str):
            raise PacketRefused(
                RefusalReason.MALFORMED_RECEIPT,
                "a callback states its " + name + " as text; one without it names nowhere to"
                " answer or no pair to answer under")
    return {"taskId": task_id, "model": model, "effort": effort}


def _callback_problems(value) -> list:
    """What is wrong with a callback read back from disk, where no constructor ran."""
    wrong = [name for name in CALLBACK_FIELDS
             if not isinstance(value.get(name), str) or not value.get(name).strip()]
    extra = sorted(set(value) - set(CALLBACK_FIELDS))
    return wrong + ["unexpected " + name for name in extra]


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
# The key an activation reading and a policy both use for the mode they were stated under.
MODE = "mode"


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
    """The honest starting triple for a mode: inapplicable where the mode has no such thing.

    The reading carries the mode it was read under. Without it, not_applicable could be
    written by a loop child as easily as by an audit, and nothing downstream could tell which
    mode the answer belonged to.
    """
    if mode not in MODES:
        raise PacketRefused(
            RefusalReason.MALFORMED_RECEIPT,
            repr(mode) + " is not an execution mode; it is one of " + ", ".join(sorted(MODES)))
    triple = _unexamined_facts(mode)
    triple[MODE] = mode
    return triple


def _unexamined_facts(mode) -> dict:
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
    if activated == INAPPLICABLE and mode == LOOP:
        # The answer that says this mode has no such thing, given by the one mode that does.
        # Accepting it read an unarmed loop as a working one, which is the L3 misreading in
        # the direction that hides a defect rather than inventing one.
        raise PacketRefused(
            RefusalReason.MALFORMED_RECEIPT,
            "a loop reading cannot answer not_applicable for activation: this mode arms a"
            " goalplan and persists phases, so an absent one is absent rather than"
            " inapplicable. That answer belongs to a mode that arms nothing")
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
    """Refuse a packet that omits what its own purpose cannot do without (see _check).

    Total over whatever JSON can hold. _check validates shape before it reads meaning, and
    every shape found so far has its own refusal there; this is the guarantee for the rest.
    A part of the packet this validator cannot read ends as a refusal naming what failed,
    never as an exception out of packet-check, because a host failure tells the producer
    nothing about what it sent and a receiver nothing about what to do with it.
    """
    try:
        _check(one, required=required)
    except RelayError:
        raise
    except (AttributeError, TypeError, KeyError, IndexError, RecursionError,
            UnicodeError) as fault:
        raise PacketRefused(
            RefusalReason.MALFORMED_RECEIPT,
            "a part of this packet is not the shape relay-packet/1 declares, so it could not"
            " be read: " + type(fault).__name__ + ": " + str(fault)) from fault


def _check(one, *, required=None) -> None:
    """Refuse a packet that omits what its own purpose cannot do without.

    A COMPLETE validator, not a finishing touch on something compose already made safe. The
    two entry points are not the same: compose builds a packet here, where every constructor
    has already run, and packet-check reads one back from disk, where none of them has. Every
    rule that lived only in a constructor was a rule the second path did not have, so each one
    is re-run from this side - the version, the artifact's own fields and the activation
    facts - against whatever the mapping actually contains.

    Shape before meaning, and derivation before comparison. A packet is not always a mapping:
    a JSON array reaching one.get() raised AttributeError, which is a host failure rather than
    a producer being told what it sent, and a validator that crashes on malformed input is not
    validating it. And three of the region's fields are DERIVED from the others, so they are
    recomputed here rather than read: a caller that can write its own kind, endpoint roles or
    messageId can relabel a completion as an instruction, or spend an id that a later real
    request then collides with.
    """
    if not isinstance(one, dict):
        raise PacketRefused(
            RefusalReason.MALFORMED_RECEIPT,
            "a packet is an object with an envelope and its typed data, not a "
            + type(one).__name__)
    try:
        # Every string JSON can carry is not text: a lone surrogate decodes from JSON and
        # cannot be encoded again, so no reader could hash the packet or render it back.
        json.dumps(one, ensure_ascii=False, default=str).encode("utf-8")
    except UnicodeEncodeError as fault:
        raise PacketRefused(
            RefusalReason.MALFORMED_RECEIPT,
            "the packet holds text that is not valid Unicode (" + str(fault) + "), which no"
            " reader can hash or render") from fault
    region = one.get("envelope")
    if not isinstance(region, dict):
        raise PacketRefused(
            RefusalReason.MALFORMED_RECEIPT,
            "a packet carries a relay-envelope/1 region under envelope, not a "
            + type(region).__name__)
    if one.get("version") != VERSION:
        # Refused rather than read hopefully. A newer packet may mean something different by
        # a field this build already knows the name of, and reading it under these rules is
        # the silent misinterpretation the refusal exists to prevent.
        raise PacketRefused(
            RefusalReason.MALFORMED_RECEIPT,
            "this reader is " + VERSION + " and the packet says " + repr(one.get("version"))
            + "; a version nobody mapped is diagnosed rather than read under these rules")
    envelope.check(region)
    if region.get("version") != envelope.VERSION:
        # The packet's own version says how to read the typed data; the region's says how to
        # read the identification. Checking one and not the other let a relay-packet/1 carry
        # an envelope nobody here has mapped and be compared field by field anyway.
        raise PacketRefused(
            RefusalReason.MALFORMED_RECEIPT,
            "this reader is " + envelope.VERSION + " and the region says "
            + repr(region.get("version")) + "; the identification region is read under the"
            " version that wrote it or not at all")
    _rederive(region)
    # The region's compared fields, typed before anything is compared with them: each is
    # either its value's shape or a stated absence. An untyped one agreed with an equally
    # wrong record value, or became a gap where the packet itself was malformed.
    for name, wanted in REGION_TYPES.items():
        value = region.get(name)
        if value is None or envelope.is_absent(value):
            continue
        if isinstance(value, bool) or not isinstance(value, wanted):
            raise PacketRefused(
                RefusalReason.MALFORMED_RECEIPT,
                "the region's " + name + " is " + wanted.__name__ + " or a stated absence,"
                " not a " + type(value).__name__)
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
    for name, wanted in FIELD_TYPES.items():
        value = one.get(name)
        if value is None:
            continue
        # bool is an int to Python and is not a generation, so it is excluded by name.
        if isinstance(value, bool) or not isinstance(value, wanted):
            raise PacketRefused(
                RefusalReason.MALFORMED_RECEIPT,
                name + " is " + wanted.__name__ + ", not a " + type(value).__name__
                + "; a value of the wrong shape compares equal to an equally wrong record"
                " value and comes back agreed")
    if one.get(EVIDENCE) is not None:
        if not isinstance(one[EVIDENCE], (list, tuple)):
            raise PacketRefused(
                RefusalReason.MALFORMED_RECEIPT,
                "evidence is a list of pointers, not a " + type(one[EVIDENCE]).__name__)
        for item in one[EVIDENCE]:
            if not isinstance(item, str) or not item.strip():
                raise PacketRefused(
                    RefusalReason.MALFORMED_RECEIPT,
                    "each evidence entry is a pointer somebody can follow, not "
                    + repr(item))
    # Presence rather than _present, for the policy and the artifact alike: an empty list or
    # object where one belongs is not an absent field but one of the wrong shape, and letting
    # it pass as nothing said is how a malformed one reaches a reader unexamined.
    if one.get(POLICY) is not None:
        if not isinstance(one[POLICY], dict):
            raise PacketRefused(
                RefusalReason.MALFORMED_RECEIPT,
                "a policy is an object of named settings, not a "
                + type(one[POLICY]).__name__)
        missing = [name for name in ("model", "effort", "workflow", MODE)
                   if _present((one[POLICY] or {}).get(name)) is None]
        if missing:
            raise PacketRefused(
                RefusalReason.SETTINGS_INCOMPLETE,
                "the policy states " + ", ".join(missing) + " as nothing; the workflow in"
                " particular has no transport field, so an unstated one is dropped rather"
                " than defaulted")
        # A packet read back from disk never went through policy(), which writes text. A
        # number would compare equal to its own spelling in a record, and an accepted one
        # would be written into the receiver's ledger as an assignment no later reading can
        # use, so it is refused here, before anything is compared or recorded.
        mistyped = [name for name in ("model", "effort", "workflow")
                    if not isinstance(one[POLICY][name], str)]
        if one[POLICY].get("approval") is not None \
                and not isinstance(one[POLICY]["approval"], str):
            mistyped.append("approval")
        if one[POLICY].get("sandbox") is not None \
                and not isinstance(one[POLICY]["sandbox"], str) \
                and _normalise_policy(one[POLICY]["sandbox"]) is None:
            mistyped.append("sandbox")
        if mistyped:
            raise PacketRefused(
                RefusalReason.MALFORMED_RECEIPT,
                "the policy states " + ", ".join(mistyped) + " as something other than"
                " text (a sandbox may also be a policy object naming its type); a setting is"
                " a name, and another shape would agree with its own spelling in the record")
        if one[POLICY][MODE] not in MODES:
            raise PacketRefused(
                RefusalReason.MALFORMED_RECEIPT,
                repr(one[POLICY][MODE]) + " is not an execution mode; it is one of "
                + ", ".join(sorted(MODES)))
        contradiction = _mode_problem(one[POLICY])
        if contradiction:
            raise PacketRefused(RefusalReason.MALFORMED_RECEIPT, contradiction)
    if one.get(CALLBACK) is not None:
        wrong = _callback_problems(one[CALLBACK])
        if wrong:
            raise PacketRefused(
                RefusalReason.MALFORMED_RECEIPT,
                "the callback states " + ", ".join(wrong) + "; it is the task to answer and"
                " the model and effort that task runs now, each as text, and nothing else")
    if one.get(ARTIFACT) is not None:
        _check_artifact(one[ARTIFACT])
    if _present(one.get(BODY)) is not None:
        problems_of, form = BODY_SECTIONS.get((direction, purpose), DEFAULT_BODY_SECTIONS)
        missing = problems_of(one[BODY])
        if missing:
            raise PacketRefused(
                RefusalReason.MALFORMED_RECEIPT,
                "the instruction body is missing " + ", ".join(missing)
                + "; " + form + " fixes these sections because an instruction that omits"
                " one is an instruction the recipient has to guess at")
    if one.get("activation") is not None:
        # Presence rather than _present: an empty list is not an absent reading, it is a
        # reading of the wrong shape, and letting it pass as nothing said is how a malformed
        # activation goes unexamined.
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
        if triple.get(MODE) not in MODES:
            raise PacketRefused(
                RefusalReason.MALFORMED_RECEIPT,
                "an activation reading states the mode it was read under, one of "
                + ", ".join(sorted(MODES)) + ", not " + repr(triple.get(MODE))
                + "; not_applicable means something only under a mode that arms nothing")
        # Read under its own mode, so a loop reading cannot answer not_applicable; the class
        # function already refuses that, and it is the rule rather than a restatement of it.
        activation_class(triple, mode=triple[MODE])
        settings = one.get(POLICY)
        if _present(settings) is not None and settings.get(MODE) != triple[MODE]:
            raise PacketRefused(
                RefusalReason.MALFORMED_RECEIPT,
                "the activation reading was taken under mode " + repr(triple[MODE])
                + " and the policy this packet states runs under " + repr(settings.get(MODE))
                + "; one packet cannot say both, and an audit's not_applicable is not a loop"
                " child's")


def _mode_problem(settings):
    """The one contradiction the workflow's wording can show, or None.

    The mode is never inferred from the workflow: prose is what this module refuses to turn
    into a fact. But a workflow that names CXC Loop and states another mode says two things at
    once, and that is refused rather than resolved in favour of either.
    """
    words = "".join(ch if ch.isalnum() else " " for ch in str(settings.get("workflow"))
                    ).casefold().split()
    names_loop = any(words[i:i + 2] == ["cxc", "loop"] for i in range(len(words) - 1))
    if names_loop and settings.get(MODE) != LOOP:
        return ("the workflow names CXC Loop and the policy says " + repr(settings.get(MODE))
                + "; the Loop arms a goalplan, so its mode is loop")
    return None


# ---------------------------------------------------------------- agreement with the record

def _rederive(region) -> None:
    """The region's derived fields, recomputed from the facts they come from.

    A comparison of copies would have nothing to catch, because the packet carries no
    duplicate of anything; what it carries are values COMPUTED from the direction, the
    relation, the purpose and the subject. Recomputing them is what catches a region belonging
    to another message, exactly as the directive pointer is checked by re-derivation rather
    than by comparing stored halves.

    The messageId matters most. It keys the replay and collision reading, so a caller able to
    write its own could hand a second correction the disposition given to the first, or spend
    an id in advance that a later real request then collides with.
    """
    direction, purpose = region.get("direction"), region.get("purpose")
    kind = envelope.kind_of(direction, purpose)
    if region.get("kind") != kind:
        raise PacketRefused(
            RefusalReason.MALFORMED_RECEIPT,
            "this region says it is a " + repr(region.get("kind")) + ", but "
            + direction + "/" + purpose + " is a " + kind
            + "; the kind is what the recipient owes, and it is derived rather than declared")
    sender_role, recipient_role = envelope.ENDPOINT_ROLES[direction]
    for name, expected in (("sender", sender_role), ("recipient", recipient_role)):
        endpoint = region.get(name)
        if not isinstance(endpoint, dict):
            raise PacketRefused(
                RefusalReason.MALFORMED_RECEIPT,
                "the " + name + " is an object with a role and a task id, not a "
                + type(endpoint).__name__)
        if endpoint.get("role") != expected:
            raise PacketRefused(
                RefusalReason.MALFORMED_RECEIPT,
                "the " + name + " claims the role " + repr(endpoint.get("role")) + ", but on "
                + direction + " it is the " + expected
                + "; a direction fixes both roles and a caller supplies neither")
    derived = envelope.message_id(direction=direction, relation_id=region.get("relationId"),
                                  purpose=purpose, subject=region.get("subject"))
    if region.get("messageId") != derived:
        raise PacketRefused(
            RefusalReason.MALFORMED_RECEIPT,
            "this region carries messageId " + repr(region.get("messageId")) + ", but its own"
            " direction, relation, purpose and subject derive " + derived
            + "; the identifier belongs to another message")


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
WRONG_CALLBACK = "wrong_callback"
STALE_CALLBACK = "stale_callback"
STALE_POLICY = "stale_policy"
WRONG_MODE = "wrong_mode"
WRONG_WORKFLOW = "wrong_workflow"
UNREADABLE = "unreadable"

# The record keys this module reads beyond the task ids. relationStatus is the relationship
# row's own status; dispatchRequestId the dispatch that opened the current generation, which
# is what a first assignment names.
RELATION_STATUS = "relationStatus"
DISPATCH_REQUEST = "dispatchRequestId"
# The generation the registration that began the current tenure opened. A tenure is one
# registration of the child on the relationship; a returning registration reuses the id.
TENURE_GENERATION = "tenureGeneration"
TENURE_DISPATCH = "tenureDispatchRequestId"

# The one record key whose PRESENT nothing is an answer rather than a gap. A relationship
# registered without a project has no link and therefore no revision, and the store says so
# as a fact ("unscoped", not "missing"). Everywhere else nothing read is nothing read, which
# keeps "absence withholds" for settings and criteria.
DEFINITE_ABSENCE = ("relationRevision",)


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
    # Checked before anything is read off it, including the region: reaching into a shape
    # nobody has validated is how a malformed packet becomes a host failure rather than a
    # producer being told what it sent.
    check(one)
    region = one["envelope"]
    problems, gaps = [], []
    if not isinstance(record, dict):
        raise PacketRefused(
            RefusalReason.MALFORMED_RECEIPT,
            "the receiver's own reading is an object of named values, not a "
            + type(record).__name__)
    try:
        return _reception(one, region, record, problems, gaps)
    except (AttributeError, TypeError, KeyError, IndexError, RecursionError,
            UnicodeError) as fault:
        # Total over the reading as check() is over the packet. A supplied reading can hold
        # anything JSON can, and a part of it this cannot read is refused by name rather
        # than ending packet-check as a host failure.
        raise PacketRefused(
            RefusalReason.MALFORMED_RECEIPT,
            "a part of the receiver's reading is not a shape this reader can compare with:"
            " " + type(fault).__name__ + ": " + str(fault)) from fault


def _reception(one, region, record, problems, gaps) -> dict:
    first = _first_assignment(region)
    if first:
        # Written before the child existed, so the relation it names is the dispatch request
        # registration bound to the relationship, and the relationship's own id - derived
        # from the child's task id - is not something it could have known.
        _compare(problems, gaps, WRONG_RELATION, "relationId",
                 region.get("relationId"), record.get(DISPATCH_REQUEST),
                 "a first assignment names the dispatch it was sent under, and this is not"
                 " the dispatch that opened the receiver's current generation")
    else:
        _compare(problems, gaps, WRONG_RELATION, "relationId",
                 region.get("relationId"), record.get("relationId"),
                 "a packet naming another relationship belongs to another assignment")
    sender_role, recipient_role = envelope.ENDPOINT_ROLES[region["direction"]]
    expected_sender = record.get(RECORD_TASK_KEY[sender_role])
    expected_recipient = record.get(RECORD_TASK_KEY[recipient_role])
    _compare(problems, gaps, WRONG_SENDER, "sender.taskId",
             (region.get("sender") or {}).get("taskId"), expected_sender,
             "the registered pair is what says who may send this, not the message's own"
             " account of itself")
    if first:
        # Its recipient is a stated absence, legal because registration is what binds it; so
        # the receiver's reading has to answer who was registered, and it is not compared.
        if _present(expected_recipient) is None:
            gaps.append(mismatch(UNREADABLE, "recipient.taskId", expected=None,
                                 found=(region.get("recipient") or {}).get("taskId"),
                                 reason="no registration answers who this first assignment"
                                        " created, so it cannot be taken up yet"))
    else:
        _compare(problems, gaps, WRONG_RECIPIENT, "recipient.taskId",
                 (region.get("recipient") or {}).get("taskId"), expected_recipient,
                 "a packet addressed to another task is not this task's instruction")
    _compare(problems, gaps, WRONG_ISSUE, ISSUE, one.get(ISSUE), record.get(ISSUE),
             "the issue binding is what makes this the assignment it claims to be")
    _liveness(problems, gaps, record)
    if not first and region["direction"] in (envelope.PARENT_TO_CHILD,
                                              envelope.CHILD_TO_PARENT) \
            and not (isinstance(record.get(DISPATCH_REQUEST), str)
                     and record[DISPATCH_REQUEST].strip()):
        # Which dispatch opened the current generation is which tenure this packet belongs
        # to, and so which accepted assignment's mode and workflow apply to it. Unread, the
        # tenure is unchecked, and so is a reading of it that is not a dispatch id at all.
        # (A first assignment compares it as its relation, above.)
        gaps.append(mismatch(UNREADABLE, DISPATCH_REQUEST, expected=None, found=None,
                             reason="the receiver could not read which dispatch opened the"
                                    " current generation, so which tenure this packet"
                                    " belongs to is unchecked"))
    if not first:
        # Always compared, and a record that cannot answer is a gap: skipping the comparison
        # when the revision was unread is what let an unchecked link come back accepted. The
        # first assignment is the exception because it was written before any link existed;
        # the dispatch binding and the relationship's liveness answer for its currency.
        _revision(problems, gaps, region, record)
    required = required_for(region["direction"], region["purpose"])
    if GENERATION in required:
        _compare(problems, gaps, STALE_GENERATION, GENERATION,
                 one.get(GENERATION), record.get(GENERATION),
                 "a receipt emitted under a generation the assignment is not in is refused,"
                 " so a message naming one produces work that cannot be handed back")
    elif one.get(GENERATION) is not None:
        # Not required here, but stated: a packet that says which generation it belongs to
        # is held to it, which is how a sender binds a resume or a note to its tenure.
        _compare(problems, gaps, STALE_GENERATION, GENERATION,
                 one.get(GENERATION), record.get(GENERATION),
                 "this packet says it belongs to another generation than the current one")
    if _present(one.get(POLICY)) is not None \
            and region["direction"] in (envelope.PARENT_TO_CHILD, envelope.CHILD_TO_PARENT):
        # A policy is read against - or, on an assignment, defines - the mode and workflow of
        # the current tenure, so the tenure has to be read: the generation its registration
        # opened and that generation's dispatch. Unread, an assignment accepted here could
        # not be held for its tenure and the next packet could not be read against it.
        tenure_generation = record.get(TENURE_GENERATION)
        if isinstance(tenure_generation, bool) or not isinstance(tenure_generation, int) \
                or tenure_generation < 1:
            gaps.append(mismatch(UNREADABLE, TENURE_GENERATION, expected=None, found=None,
                                 reason="the receiver could not read which registration began"
                                        " the current tenure, so the mode and workflow this"
                                        " policy is held to are unchecked"))
        tenure_dispatch = record.get(TENURE_DISPATCH)
        if not isinstance(tenure_dispatch, str) or not tenure_dispatch.strip():
            gaps.append(mismatch(UNREADABLE, TENURE_DISPATCH, expected=None, found=None,
                                 reason="the receiver could not read the dispatch that began"
                                        " the current tenure, so an assignment could not be"
                                        " held for it"))
    if not first and "relationRevision" in record and record["relationRevision"] is None \
            and one.get(GENERATION) is None and record.get(TENURE_GENERATION) != 1 \
            and region["direction"] in (envelope.PARENT_TO_CHILD, envelope.CHILD_TO_PARENT):
        # An unscoped relationship has no link revision to tie a packet to its tenure, and
        # this packet states no generation. Once the relationship has returned to its child
        # (or the reading cannot say it has not), a delayed packet from the earlier tenure
        # agrees with every field it carries; it is unchecked rather than accepted. A scoped
        # relationship's link revision moves with every returning registration instead.
        gaps.append(mismatch(UNREADABLE, GENERATION, expected=None, found=None,
                             reason="this unscoped relationship has had more than one tenure"
                                    " (or the reading cannot say), and a packet stating"
                                    " neither a revision nor a generation cannot be told from"
                                    " one sent in an earlier tenure"))
    if CRITERIA_DIGEST in required:
        _compare(problems, gaps, STALE_CRITERIA, CRITERIA_DIGEST,
                 one.get(CRITERIA_DIGEST), record.get(CRITERIA_DIGEST),
                 "criteria judged against a digest nobody registered are judged against"
                 " somebody's memory of them")
    elif _present(one.get(CRITERIA_DIGEST)) is not None:
        # Not required here, but stated, and held to it as a stated generation is. A block or
        # a note naming the digest its sender judged against was accepted after the criteria
        # were registered again, and accepted when the store could read no digest at all:
        # the purpose table says what an occasion cannot omit, not what it may say unread.
        _compare(problems, gaps, STALE_CRITERIA, CRITERIA_DIGEST,
                 one.get(CRITERIA_DIGEST), record.get(CRITERIA_DIGEST),
                 "this packet was written against criteria that are no longer the"
                 " registered ones")
    _artifact_agreement(one, record, problems, gaps)
    _callback_agreement(one, record, problems, gaps)
    _policy_agreement(one, record, problems, gaps)
    instructed = _mode_agreement(one, record, problems, gaps)
    instructed += _workflow_agreement(one, record, problems, gaps)
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
            "mismatches": problems, "gaps": gaps, "instructed": instructed}


def _first_assignment(region) -> bool:
    """An assignment sent before its child existed: the recipient is a stated absence."""
    return (region.get("direction") == envelope.PARENT_TO_CHILD
            and region.get("purpose") == "assignment"
            and envelope.is_absent((region.get("recipient") or {}).get("taskId")))


def _liveness(problems, gaps, record) -> None:
    """The relationship this packet belongs to has to still be one.

    A packet about an archived or cancelled assignment is about work that has ended or been
    replaced, however well every other field agrees. A paused one is current; acting on it
    still waits for the relationship to be resumed, which is the relationship's business.
    """
    status = record.get(RELATION_STATUS)
    if _present(status) is None:
        gaps.append(mismatch(UNREADABLE, RELATION_STATUS, expected=None, found=None,
                             reason="the record the receiver read does not say whether the"
                                    " relationship is still live, so that is unchecked"))
    elif status not in LIVE_RELATION:
        problems.append(mismatch(
            SUPERSEDED_RELATION, RELATION_STATUS, expected=" or ".join(LIVE_RELATION),
            found=status,
            reason="the relationship this packet belongs to is " + str(status) + "; a packet"
                   " for it is about an assignment that has ended or been replaced"))


def _revision(problems, gaps, region, record) -> None:
    found = region.get("relationRevision")
    if "relationRevision" not in record or (
            record["relationRevision"] is not None
            and _present(record["relationRevision"]) is None):
        gaps.append(mismatch(UNREADABLE, "relationRevision", expected=None, found=found,
                             reason="the record the receiver read says nothing about the"
                                    " link revision, so whether this relationship has been"
                                    " replaced is unchecked"))
        return
    if record["relationRevision"] is None:
        # Definite: unscoped, so there is no link and no revision to agree with.
        if _present(found) is not None and not envelope.is_absent(found):
            problems.append(mismatch(
                SUPERSEDED_RELATION, "relationRevision", expected=None, found=found,
                reason="the receiver's store holds no link for this relationship, so a"
                       " packet quoting a revision is about some other linkage"))
        return
    _compare(problems, gaps, SUPERSEDED_RELATION, "relationRevision",
             found, record["relationRevision"],
             "a superseded link is preserved and not rewritten, so a message quoting the"
             " old revision is about a relationship that has been replaced")


def _callback_agreement(one, record, problems, gaps) -> None:
    """Where to answer, and under which pair, against what the receiver holds for them."""
    stated = one.get(CALLBACK)
    if _present(stated) is None:
        return
    held = record.get(CALLBACK)
    if not isinstance(held, dict) or not held:
        gaps.append(mismatch(UNREADABLE, CALLBACK, expected=None, found=stated,
                             reason="the receiver read no record of the task it answers or"
                                    " the pair that task runs now, so the callback is"
                                    " unchecked"))
        return
    _compare(problems, gaps, WRONG_CALLBACK, "callback.taskId",
             stated.get("taskId"), held.get("taskId"),
             "the answer would go to a task that is not the one this assignment reports to")
    for part in ("model", "effort"):
        _compare(problems, gaps, STALE_CALLBACK, "callback." + part,
                 stated.get(part), held.get(part),
                 "the task being answered is authorised to run another pair now; a callback"
                 " naming the old one is refused for its settings, not retried as a provider"
                 " failure or worked around with another child")


def _policy_agreement(one, record, problems, gaps) -> None:
    """The settings a packet states against the ones the task was actually created with."""
    stated = one.get(POLICY)
    if _present(stated) is None:
        return
    held = record.get(POLICY)
    if not isinstance(held, dict) or not held:
        gaps.append(mismatch(UNREADABLE, POLICY, expected=None, found=stated,
                             reason="the receiver read no recorded settings for this task,"
                                    " so the stated pair is unchecked"))
        return
    for part in ("model", "effort"):
        _compare(problems, gaps, STALE_POLICY, "policy." + part,
                 stated.get(part), held.get(part),
                 "the task was created with another pair; a packet stating this one is"
                 " about settings the receiver does not hold")
    # The permissions, where the packet states them. A packet that keeps the pair and changes
    # the sandbox or the approval asks the receiver to act under permissions its record never
    # authorised, which is the same staleness as another pair.
    _sandbox_agreement(stated.get("sandbox"), held, problems, gaps)
    if _present(stated.get("approval")) is not None:
        _compare(problems, gaps, STALE_POLICY, "policy.approval", stated["approval"],
                 held.get("approval"),
                 "the task was created under another approval policy; acting on this packet"
                 " would run under one its record never authorised")


# A sandbox is stated two ways: as the mode a resume carries ("danger-full-access") or as the
# policy object a creation receipt records ({"type": "dangerFullAccess", ...}).
SANDBOX_TYPE_BY_MODE = {mode: kind for kind, mode in settings.RESUME_SANDBOX_MODE.items()}


def _sandbox_reading(value):
    """A sandbox as (type, normalised policy or None), or None where it cannot be read.

    A mode string says only the type, so it is compared as a type. A policy object is filled
    with its declared defaults (settings.normalise_policy) so an omitted default and an
    explicit one agree, and is compared whole against a recorded object.
    """
    if isinstance(value, str):
        name = value.strip()
        return (SANDBOX_TYPE_BY_MODE.get(name, name), None) if name else None
    policy = settings.normalise_policy(value)
    return None if policy is None else (policy["type"], policy)


def _sandbox_agreement(stated, held, problems, gaps) -> None:
    if _present(stated) is None:
        return
    field = "policy.sandbox"
    recorded = held.get("sandbox")
    theirs = _sandbox_reading(recorded) if _present(recorded) is not None else None
    if theirs is None:
        gaps.append(mismatch(UNREADABLE, field, expected=None, found=stated,
                             reason="the receiver read no sandbox recorded for this task, so"
                                    " the stated one is unchecked"))
        return
    mine = _sandbox_reading(stated)
    if mine is None:
        problems.append(mismatch(STALE_POLICY, field, expected=recorded, found=stated,
                                 reason="the stated sandbox is neither a mode nor a policy"
                                        " object, so it names no sandbox the task holds"))
        return
    if mine[1] is not None and theirs[1] is not None:
        same = mine[1] == theirs[1]
    else:
        same = mine[0] == theirs[0]
    if not same:
        problems.append(mismatch(STALE_POLICY, field, expected=recorded, found=stated,
                                 reason="the task was created under another sandbox; acting"
                                        " on this packet would run under permissions its"
                                        " record never authorised"))


def _mode_agreement(one, record, problems, gaps) -> list:
    """The execution mode a packet states against the receiver's own reading of it.

    No store holds the workflow or its mode: only the message carries it. So the reading is
    the receiver's own - the mode of the assignment it already accepted - and an assignment
    DEFINES the mode only where the receiver holds no reading yet. Everything else is
    compared, so no packet can redefine a mode the receiver already holds, and a packet
    carrying a mode to a receiver with no reading of it is unchecked rather than believed.
    Returns what was taken as instructed rather than checked.
    """
    stated = []
    if _present(one.get(POLICY)) is not None:
        stated.append(("policy.mode", one[POLICY].get(MODE)))
    if one.get("activation") is not None:
        stated.append(("activation.mode", one["activation"].get(MODE)))
    if not stated:
        return []
    held = record.get(MODE)
    region = one["envelope"]
    if _present(held) is None:
        if (region.get("direction") == envelope.PARENT_TO_CHILD
                and region.get("purpose") == "assignment"):
            return [{"field": MODE, "value": stated[0][1],
                     "source": "packet: the assignment defines the mode where none is held"}]
        gaps.append(mismatch(UNREADABLE, MODE, expected=None, found=stated[0][1],
                             reason="the receiver holds no reading of the mode its assignment"
                                    " gave, so a mode this packet states is unchecked"))
        return []
    for field, value in stated:
        _compare(problems, gaps, WRONG_MODE, field, value, held,
                 "the assignment this receiver accepted runs under another mode, and no"
                 " later packet redefines it")
    return []


def _workflow_agreement(one, record, problems, gaps) -> list:
    """The workflow a packet's policy states against the one the receiver's assignment gave.

    The same rule as the mode, for the same reason: no transport or store carries the
    workflow, so the receiver's reading is the workflow of the assignment it accepted. An
    assignment defines it only where none is held; every other packet stating a policy is
    compared (wrong_workflow), or left a gap where the receiver holds none. A resume that
    keeps the mode and names another workflow would otherwise hand the receiver a procedure
    its accepted assignment never established.
    """
    if _present(one.get(POLICY)) is None:
        return []
    stated = one[POLICY].get("workflow")
    held = record.get("workflow")
    region = one["envelope"]
    if _present(held) is None:
        if (region.get("direction") == envelope.PARENT_TO_CHILD
                and region.get("purpose") == "assignment"):
            return [{"field": "workflow", "value": stated,
                     "source": "packet: the assignment defines the workflow where none is"
                               " held"}]
        gaps.append(mismatch(UNREADABLE, "workflow", expected=None, found=stated,
                             reason="the receiver holds no reading of the workflow its"
                                    " assignment gave, so a workflow this packet states is"
                                    " unchecked"))
        return []
    _compare(problems, gaps, WRONG_WORKFLOW, "policy.workflow", stated, held,
             "the assignment this receiver accepted runs under another workflow, and no later"
             " packet replaces it")
    return []


def _compare(problems, gaps, kind, field, found, expected, reason) -> None:
    """One field against the record, with a record that cannot answer kept separate.

    Values of different types are not compared. Both sides were once turned into strings, so
    a recorded model of 123 agreed with a packet naming "123", and a generation recorded as
    "2" with a packet's 2: a value of another shape was taken as a reading of the field. The
    packet's side is shape-checked before this runs, so a type disagreement means the record
    holds something that is not a reading of this field, and that is a gap, not agreement.
    """
    if _present(expected) is None:
        gaps.append(mismatch(UNREADABLE, field, expected=None, found=found,
                             reason="the record the receiver read says nothing about "
                                    + field + ", so this could not be checked"))
        return
    if _present(found) is None:
        problems.append(mismatch(kind, field, expected=expected, found=None,
                                 reason="the packet states no " + field + ", and " + reason))
        return
    if type(found) is not type(expected):
        gaps.append(mismatch(UNREADABLE, field, expected=expected, found=found,
                             reason="the record holds a " + type(expected).__name__ + " for "
                                    + field + " and the packet a " + type(found).__name__
                                    + "; values of different shapes are not a reading of"
                                      " each other, so this could not be checked"))
        return
    if found != expected:
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
        # Identity before currency. Comparing only the head accepted a packet naming another
        # repository or another pull request that happened to sit on the same commit, which is
        # the second writer this whole reading exists to keep out.
        _compare(problems, gaps, STALE_HEAD, "artifact.repository",
                 artifact.get("repository"), record.get("repository"),
                 "the same number on two projects is two different pull requests")
        _compare(problems, gaps, STALE_HEAD, "artifact.number",
                 artifact.get("number"), record.get("prNumber"),
                 "this assignment is bound to one pull request, and it is not that one")
        _compare(problems, gaps, STALE_HEAD, "artifact.headSha",
                 artifact.get("headSha"), record.get("headSha"),
                 "the candidate moved after this packet was written, so its checks, its"
                 " review and its readiness are about another commit")
        return
    _compare(problems, gaps, STALE_HEAD, "artifact.path",
             artifact.get("path"), record.get("artifactPath"),
             "a digest identifies bytes and not which deliverable they were supposed to be")
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
    if _refusals_unreadable(record):
        return []  # a gap, from _settings_gaps
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
    if one.get(POLICY) and _refusals_unreadable(record):
        return [mismatch(UNREADABLE, "policy", expected=None, found=one[POLICY],
                         reason="the recorded refusals are not a list of model and effort"
                                " pairs, so whether this pair is authorised for this role is"
                                " unchecked")]
    if one.get(POLICY) and _present(record.get("refusedPolicies")) is None \
            and "refusedPolicies" not in record:
        return [mismatch(UNREADABLE, "policy", expected=None, found=one[POLICY],
                         reason="the receiver read no settings record, so whether this pair"
                                " is authorised for this role is unchecked")]
    return []


def _refusals_unreadable(record) -> bool:
    """A refusal list the reading holds and cannot read: not read as "none refused".

    Present as null, not a list, or holding an entry that is not a model and effort pair of
    text. An empty list is a reading - nothing refused - and an absent key is handled as an
    absence by _settings_gaps.
    """
    if "refusedPolicies" not in record:
        return False
    held = record["refusedPolicies"]
    return not isinstance(held, list) or any(
        not isinstance(pair, dict)
        or not all(isinstance(pair.get(name), str) and pair[name].strip()
                   for name in ("model", "effort"))
        for pair in held)


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

    Three answers, not two. A packet whose id and content both match is a REPLAY, and the
    disposition it already got is reported beside today's reading (settle_repeat), which is
    what makes an uncertain send safe to settle by asking rather than by sending again. A
    packet reusing an id while asking for something else is a COLLISION: it is neither a
    replay nor a new instruction, and answering it with the earlier disposition would apply a
    decision to a request nobody made.
    """
    region = one.get("envelope") or {}
    identifier = region.get("messageId")
    prior = (answered or {}).get(identifier)
    if not prior:
        return {"state": FIRST, "messageId": identifier, "contentDigest": content_digest(one)}
    mine = content_digest(one)
    if str(prior.get("contentDigest")) == mine:
        return {"state": REPLAY, "messageId": identifier, "contentDigest": mine,
                "disposition": prior.get("disposition"), "applied": prior.get("applied") is True,
                "answeredDigest": mine,
                "reason": "this id was answered already and asks for the same thing, so it is"
                          " not acted on twice"}
    return {"state": COLLISION, "messageId": identifier, "contentDigest": mine,
            "disposition": prior.get("disposition"),
            "answeredDigest": prior.get("contentDigest"),
            "reason": "this id was answered already and asks for something else. It is raised"
                      " rather than given the earlier answer, which is also what stops a"
                      " predictable id being spent in advance to suppress the real request"}


COLLISION_MISMATCH = "message_collision"
UNCHECKED = "unchecked"
PAUSED_RELATION = "paused"


def settle_repeat(answer, repeated) -> dict:
    """Today's reading stands; a repeat only decides whether it may be acted on again.

    An earlier disposition never replaces the current one. Handing a replay the answer it got
    before let an accepted correction come back accepted after its generation had moved on or
    after its record had become unreadable, which is exactly the reading this check exists to
    refuse. So the current disposition is kept, the earlier one is reported beside it, and act
    says whether the instruction still has to be applied: it is accepted today and the receiver
    has not recorded applying it. An accepted answer is not an applied one. A receiver that
    checked and then stopped before acting gets the instruction back only as a replay, and
    answering that replay "already accepted, do not act" lost it; so act stays true until the
    receiver records the application (receiver.record_applied). With no record of earlier
    answers (repeated is None) nothing can be told apart from a first arrival, so nothing is
    acted on.

    And nothing is acted on while the relationship is paused. Paused is current, so the packet
    is accepted - it is about this assignment - but no work proceeds on a paused relationship
    until relationship-resume (the relay refuses claims and generation-open meanwhile), so act
    is held and the answer says why. Nothing is recorded applied, so the same packet checked
    after the resume comes back to be acted on.
    """
    settled = _settle(answer, repeated)
    status = (settled.get("record") or {}).get(RELATION_STATUS)
    if settled["act"] and status == PAUSED_RELATION:
        settled["act"] = False
        settled["actHeld"] = ("the relationship is paused: this packet is current and accepted,"
                              " and it is acted on only after relationship-resume; check it"
                              " again then")
    return settled


def _settle(answer, repeated) -> dict:
    settled = dict(answer)
    accepted = settled["disposition"] == ACCEPTED
    if repeated is None:
        settled["repeat"] = {"state": UNCHECKED,
                             "reason": "no reception ledger was named, so a repeat cannot be"
                                       " told from a first arrival"}
        settled["act"] = False
        return settled
    state = repeated["state"]
    if state == FIRST:
        settled["repeat"] = {"state": FIRST, "contentDigest": repeated["contentDigest"],
                             "applied": False}
        settled["act"] = accepted
        return settled
    previous = repeated.get("disposition")
    if state == REPLAY:
        applied = repeated.get("applied") is True
        settled["repeat"] = {"state": REPLAY, "contentDigest": repeated["contentDigest"],
                             "previousDisposition": previous, "applied": applied,
                             "reason": repeated["reason"]}
        settled["act"] = accepted and not applied
        return settled
    settled["mismatches"] = list(settled["mismatches"]) + [mismatch(
        COLLISION_MISMATCH, "messageId", expected=repeated.get("answeredDigest"),
        found=repeated["contentDigest"], reason=repeated["reason"])]
    settled["disposition"] = REFUSAL
    settled["repeat"] = {"state": COLLISION, "contentDigest": repeated["contentDigest"],
                         "previousDisposition": previous, "reason": repeated["reason"]}
    settled["act"] = False
    return settled


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
# linear_done is the issue's own status in Linear, which a readback of that issue answers and
# this store never holds. The outbox confirms that a coordination summary block was written
# and read back, which says nothing about the issue's status, so it does not answer this.
PROGRESSION_SOURCES = {
    READ: None,
    TRANSPORT_ACCEPTED: "attempts",
    RELAY_ACK: "acks",
    CRITERIA_VERDICT: "verdicts",
    PARENT_ACCEPTANCE: "verdicts",
    MERGE_LANDING: "merge_turns",
    LINEAR_DONE: "linear_issue_status",
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
    if not isinstance(ladder, dict):
        raise PacketRefused(
            RefusalReason.MALFORMED_RECEIPT,
            "a handover ladder is an object of named states, not a "
            + type(ladder).__name__)
    for name in PROGRESSION:
        if name not in ladder:
            raise PacketRefused(
                RefusalReason.MALFORMED_RECEIPT,
                "a handover ladder answers every state; " + name + " is missing. Start from"
                " unobserved() rather than from a partial dictionary")
        entry = ladder[name]
        if not isinstance(entry, dict):
            # Shape before meaning, as everywhere else here: reading .get() off an int turned a
            # producer's malformed ladder into a host failure instead of a refusal it can act on.
            raise PacketRefused(
                RefusalReason.MALFORMED_RECEIPT,
                "each handover state is an object with a state and the record that answered"
                " it; " + name + " is a " + type(entry).__name__)
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
    if not isinstance(ladder, dict):
        raise PacketRefused(
            RefusalReason.MALFORMED_RECEIPT,
            "a handover ladder is an object of named states, not a "
            + type(ladder).__name__)
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


def claims(claimed, held) -> dict:
    """The states a packet's own ladder claims that the receiver's reading does not hold.

    Two lists, because they are two different answers. unbacked: the reading answered and said
    no (or only conditionally). unmeasurable: the reading could not answer at all - read never
    can, Linear Done is not in this store, a landing needs an observed head. A packet writing
    yes promotes nothing either way; this says which of the two it ran into.
    """
    out = {"unbacked": [], "unmeasurable": []}
    if claimed is None:
        return out
    check_progression(claimed)
    for name in PROGRESSION:
        if claimed[name].get("state") != envelope.YES:
            continue
        state = (held.get(name) or {}).get("state")
        if state == envelope.YES:
            continue
        if state in (envelope.NO, envelope.CONDITIONAL):
            out["unbacked"].append(name)
        else:
            out["unmeasurable"].append(name)
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
                     + "  mode: " + str(settings.get(MODE))
                     + "  model: " + str(settings.get("model"))
                     + "  effort: " + str(settings.get("effort")))
    answer_to = one.get(CALLBACK)
    if _present(answer_to) is not None:
        lines.append("  answer to: " + str(answer_to.get("taskId")) + " ("
                     + str(answer_to.get("model")) + ", " + str(answer_to.get("effort")) + ")")
    artifact = one.get(ARTIFACT)
    if artifact and artifact.get("kind") == PULL_REQUEST:
        lines.append("  pull request: " + str(artifact.get("repository")) + " #"
                     + str(artifact.get("number")) + " at " + str(artifact.get("headSha")))
    elif artifact:
        lines.append("  artifact: " + str(artifact.get("path")) + " digest "
                     + str(artifact.get("digest")))
    triple = one.get("activation")
    if triple:
        lines.append("  activation read under: " + str(triple.get(MODE)))
        for name in ACTIVATION_FACTS:
            entry = triple[name]
            source = " (" + entry["source"] + ")" if entry.get("source") else ""
            lines.append("  " + name + ": " + entry["state"] + source)
    return lines
