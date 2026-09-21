"""What both relations have to agree on before either message can be read.

Two relations carry messages in this workflow. A parent and its child exchange a completion
and a correction, and that pair has a queue, a receipt, an acknowledgement and a verdict. A
supervisor and a parent exchange instructions and reports, and that pair has none of those -
linkage records that an instruction EXISTS and OPS-7.4 says out loud that the relay carries no
supervisor or peer message channel.

Writing one envelope for both is therefore not a way to pretend the second pair has the first
pair's machinery. It is the opposite: one place that says which facts identify a message, what
a recipient owes because of it, and - per direction - which stages have a row behind them and
which have nothing at all. A caller that reads this module cannot accidentally credit a
supervisor with an acknowledgement, because the table below answers not_applicable where no
channel exists rather than leaving the field open for somebody to fill in.

Nothing here stores anything. Every value is derived from rows another module owns, which is
what lets the same reading survive a restart, a compaction and a service replacement without
anyone having remembered it.
"""

from .errors import RefusalReason, RelayError
from .identity import sha256_hex

VERSION = "relay-envelope/1"

# How much of the digest a message identifier keeps. 128 bits, matching linkage's own
# relay-owned ids: a collision here would silently MERGE two messages into one obligation
# rather than fail where somebody could see it.
ID_WIDTH = 32


class EnvelopeRefused(RelayError):
    """An envelope was incomplete for its kind, or contradicted the record it names."""


# --------------------------------------------------------------------------- what is owed

# A kind is not a topic. It is what the recipient owes because this message arrived, which is
# the only thing that decides whether the message may interrupt anybody.
REQUEST = "request"
NOTIFICATION = "notification"
DECISION = "decision"
STATUS_RESPONSE = "status_response"
KINDS = (REQUEST, NOTIFICATION, DECISION, STATUS_RESPONSE)

# Who owes the answer, which is not always the recipient. A decision is owed by Jun, and a
# recipient that answered one itself would be deciding something it was asked to carry.
RECIPIENT = "recipient"
USER = "user"
ANSWER_OWED_BY = {REQUEST: RECIPIENT, DECISION: USER, NOTIFICATION: None,
                  STATUS_RESPONSE: None}


# ------------------------------------------------------------------------------ directions

CHILD_TO_PARENT = "child_to_parent"
PARENT_TO_CHILD = "parent_to_child"
SUPERVISOR_TO_PARENT = "supervisor_to_parent"
PARENT_TO_SUPERVISOR = "parent_to_supervisor"
DIRECTIONS = (CHILD_TO_PARENT, PARENT_TO_CHILD, SUPERVISOR_TO_PARENT, PARENT_TO_SUPERVISOR)

# The roles the two ends hold, spelled the way the linkage records and the host role policy
# already spell them. A direction fixes both, so no caller supplies a role of its own.
ENDPOINT_ROLES = {
    CHILD_TO_PARENT: ("child", "parent"),
    PARENT_TO_CHILD: ("parent", "child"),
    SUPERVISOR_TO_PARENT: ("supervisor", "parent"),
    PARENT_TO_SUPERVISOR: ("parent", "supervisor"),
}


# ------------------------------------------------------------------------------- purposes

# Why the message was sent, and therefore what it owes. The supervisor direction's five
# occasions are the ones CRW-148 names: an initial project assignment, a midpoint check or a
# resume, a scope correction, a decision Jun made that the parent has to apply, and a stop the
# user asked for. A relayed decision is a REQUEST rather than a decision: Jun has already
# decided and the parent owes the application, not another opinion.
PURPOSES = {
    CHILD_TO_PARENT: {"completion": REQUEST},
    PARENT_TO_CHILD: {"revision_request": REQUEST},
    SUPERVISOR_TO_PARENT: {
        "project_assignment": REQUEST,
        "midpoint_check": REQUEST,
        "resume": REQUEST,
        "scope_correction": REQUEST,
        "relayed_decision": REQUEST,
        "user_stop": REQUEST,
    },
    PARENT_TO_SUPERVISOR: {
        "completion": NOTIFICATION,
        "blocked": NOTIFICATION,
        "decision_request": DECISION,
        "status_response": STATUS_RESPONSE,
    },
}


def kind_of(direction, purpose) -> str:
    """The kind a purpose carries. Never supplied by a caller, so the two cannot disagree."""
    _known(direction, DIRECTIONS, "direction")
    kinds = PURPOSES[direction]
    if purpose not in kinds:
        raise EnvelopeRefused(
            RefusalReason.MALFORMED_RECEIPT,
            f"{purpose!r} is not a purpose {direction} carries; it has "
            + ", ".join(sorted(kinds)),
        )
    return kinds[purpose]


def _known(value, allowed, what):
    if value not in allowed:
        raise EnvelopeRefused(
            RefusalReason.MALFORMED_RECEIPT,
            f"{value!r} is not a known {what}; it is one of " + ", ".join(sorted(allowed)),
        )


# -------------------------------------------------------------------------------- absences

# Three different reasons a field carries no value, and they are not interchangeable. A
# reader deciding whether to go and look something up needs to know whether nobody said,
# whether the value lives one level up, or whether this kind of message has no such thing.
INHERITED = "inherited"
UNKNOWN = "unknown"
NOT_APPLICABLE = "not_applicable"
ABSENCES = (INHERITED, UNKNOWN, NOT_APPLICABLE)


def absent(reason, detail="") -> dict:
    """A stated absence. Distinguishable from a value, and from the other two absences."""
    _known(reason, ABSENCES, "absence")
    return {"absent": reason, "detail": detail}


def is_absent(value) -> bool:
    return isinstance(value, dict) and "absent" in value


def shown(value) -> str:
    """How a field renders on a line, absence and all.

    An absent field prints its reason rather than an empty space, because a blank reads as a
    value nobody bothered to fill in and these three are answers.
    """
    if is_absent(value):
        detail = value.get("detail")
        return f"<{value['absent']}: {detail}>" if detail else f"<{value['absent']}>"
    return "" if value is None else str(value)


# --------------------------------------------------------------------- the five-stage reach

# Five separate questions, asked in the order a message travels. Collapsing any two of them
# is how a send the transport accepted becomes, three reports later, a change somebody made.
TRANSPORT_ACCEPTED = "transport_accepted"
RECEIVED = "received"
AGREED = "agreed"
APPLIED = "applied"
VERIFIED = "verified"
STAGES = (TRANSPORT_ACCEPTED, RECEIVED, AGREED, APPLIED, VERIFIED)

YES = "yes"
NO = "no"
CONDITIONAL = "conditional"
# Nothing readable answered. Never a synonym for no, and never a step toward yes.
UNMEASURED = "unmeasured"
# There is no mechanism that could answer, so waiting for one is waiting forever.
IMPOSSIBLE = "not_applicable"
STATES = (YES, NO, CONDITIONAL, UNMEASURED, IMPOSSIBLE)

# Which record answers each stage, per direction, and where nothing does.
#
# The two halves of the table differ because the directions differ. A child's completion is
# acknowledged: ack.acknowledge exists for exactly that message. A parent's revision request is
# NOT - acknowledge refuses a revision delivery, and what shows a correction was applied is the
# completion receipt of the generation it opened. The supervisor direction has no channel at
# all, so its upper stages are impossible rather than merely unmeasured, and saying so is the
# whole reason this table is data instead of a paragraph somebody has to remember.
REACH_SOURCES = {
    CHILD_TO_PARENT: {
        TRANSPORT_ACCEPTED: "attempts", RECEIVED: "acks", AGREED: "acks",
        APPLIED: "verdicts", VERIFIED: "verdicts",
    },
    PARENT_TO_CHILD: {
        TRANSPORT_ACCEPTED: "attempts", RECEIVED: None, AGREED: None,
        APPLIED: "events", VERIFIED: "verdicts",
    },
    SUPERVISOR_TO_PARENT: {
        TRANSPORT_ACCEPTED: None, RECEIVED: "scope_directives", AGREED: "scope_directives",
        APPLIED: None, VERIFIED: None,
    },
    PARENT_TO_SUPERVISOR: {
        TRANSPORT_ACCEPTED: None, RECEIVED: None, AGREED: None, APPLIED: None, VERIFIED: None,
    },
}

# Why a stage has no mechanism, said once per direction so four callers do not each invent a
# sentence for it.
NO_MECHANISM = {
    CHILD_TO_PARENT: "",
    PARENT_TO_CHILD: "a revision request carries no acknowledgement; the completion receipt of"
                     " the generation it opened is what shows it was applied",
    SUPERVISOR_TO_PARENT: "the relay carries no supervisor message channel, so nothing"
                          " transports, applies or verifies this one",
    PARENT_TO_SUPERVISOR: "the relay carries no supervisor message channel, so nothing here"
                          " records that a supervisor received, agreed, applied or verified",
}


def stage(state, *, source=None, detail="") -> dict:
    """One stage's answer, with what answered it. A state without a source is unmeasured."""
    _known(state, STATES, "reach state")
    if state in (YES, NO, CONDITIONAL) and not source:
        raise EnvelopeRefused(
            RefusalReason.MALFORMED_RECEIPT,
            f"a {state} at a reach stage needs the record that says so",
        )
    return {"state": state, "source": source, "detail": detail}


def unreached(direction) -> dict:
    """The honest starting ladder for a direction: impossible where nothing could answer.

    A caller fills in the stages it has rows for. What it does not fill in stays either
    unmeasured or impossible, and those are the two answers that must never quietly become no.
    """
    _known(direction, DIRECTIONS, "direction")
    sources = REACH_SOURCES[direction]
    return {
        name: stage(UNMEASURED, detail="nothing readable answered yet") if sources[name]
        else stage(IMPOSSIBLE, detail=NO_MECHANISM[direction])
        for name in STAGES
    }


def reached(ladder, name) -> bool:
    """Whether a stage actually holds. Only yes counts; conditional is not yes."""
    return (ladder.get(name) or {}).get("state") == YES


def promotion_refused(ladder) -> list:
    """Stages standing above an unheld one, which is what a false promotion looks like.

    Returned rather than raised, because an inconsistent ladder is usually a caller's reading
    of a real store and the useful thing is to name which step was skipped.
    """
    out = []
    held = True
    for name in STAGES:
        state = (ladder.get(name) or {}).get("state")
        if state == YES and not held:
            out.append(name)
        held = state == YES
    return out


# -------------------------------------------------------------------------------- identity

def message_id(*, direction, relation_id, purpose, subject) -> str:
    """The id one logical message keeps, however many times it is sent or re-read.

    Deliberately NOT identity.request_id, which renders del-<event>-a<attempt> and therefore
    changes on every retry. That is the right identity for a transport attempt and the wrong
    one for the thing a recipient is being told about, and conflating them is how one fact
    becomes three obligations.

    The Linear scope is deliberately not an input either. It is a display field that a reader
    may or may not be able to resolve, and feeding it in would make this id depend on whether
    the caller happened to have a scope row in hand.
    """
    _known(direction, DIRECTIONS, "direction")
    kind_of(direction, purpose)
    for name, value in (("relation_id", relation_id), ("subject", subject)):
        if not isinstance(value, str) or not value.strip():
            raise EnvelopeRefused(
                RefusalReason.MALFORMED_RECEIPT, f"{name} must be a non-empty string")
        if "|" in value:
            raise EnvelopeRefused(
                RefusalReason.MALFORMED_RECEIPT,
                f"{name} must not contain '|', which is the field separator")
    return sha256_hex(f"{direction}|{relation_id}|{purpose}|{subject}")[:ID_WIDTH]


# The fields every envelope names, and the ones a kind cannot do without. The rest may be
# absent with a reason; these may not, because a recipient without them cannot answer, cannot
# tell which execution the message is about, or cannot tell what it owes.
# observedAt is NOT in this list, and that is deliberate. A region built from a stored row
# always has one, but report.py also composes a message BEFORE its row exists, to find out
# whether a restoration block would survive the composition - and at that moment nothing has
# recorded a time. Requiring the field there would force either a refusal of a supported
# update or a fabricated timestamp, and this module exists to refuse exactly that trade. An
# unknown observation time says unknown.
REQUIRED_ALWAYS = ("version", "direction", "kind", "purpose", "messageId", "relationId",
                   "sender", "recipient")
REQUIRED_BY_KIND = {
    REQUEST: ("subject", "answerOwedBy"),
    DECISION: ("subject", "answerOwedBy", "decision"),
    NOTIFICATION: ("subject",),
    STATUS_RESPONSE: ("correlationId",),
}


def region(*, direction, purpose, relation_id, sender, recipient, subject, observed_at=None,
           relation_revision=None, scope=None, basis=None, evidence=(), correlation_id=None,
           reply_to=None, decision=None, reach=None) -> dict:
    """The identification region both relations share.

    Everything here is either a value the caller read from a row or a stated absence. There is
    no third option and no default that invents one: a region whose sender is unknown says
    unknown, and a reader can then go and find out instead of trusting a plausible task id.
    """
    kind = kind_of(direction, purpose)
    sender_role, recipient_role = ENDPOINT_ROLES[direction]
    out = {
        "version": VERSION,
        "direction": direction,
        "kind": kind,
        "purpose": purpose,
        "messageId": message_id(direction=direction, relation_id=relation_id,
                                purpose=purpose, subject=subject),
        "relationId": relation_id,
        "relationRevision": relation_revision if relation_revision is not None
        else absent(UNKNOWN, "the link revision was not read"),
        "sender": {"role": sender_role, "taskId": sender},
        "recipient": {"role": recipient_role, "taskId": recipient},
        "subject": subject,
        "scope": scope if scope is not None else absent(UNKNOWN, "no Linear scope was read"),
        "basis": basis if basis is not None
        else absent(UNKNOWN, "no generation or revision was read"),
        "observedAt": observed_at if observed_at else absent(
            UNKNOWN, "nothing has recorded when this was observed"),
        "evidence": list(evidence),
        "correlationId": correlation_id if correlation_id is not None
        else absent(NOT_APPLICABLE, "this message answers nothing earlier"),
        "replyTo": reply_to if reply_to is not None
        else absent(NOT_APPLICABLE, "no reply is directed at one message"),
        "answerOwedBy": ANSWER_OWED_BY[kind] or absent(
            NOT_APPLICABLE, "this kind owes no answer"),
        "decision": decision if decision is not None else absent(
            NOT_APPLICABLE, "no user decision is being asked for"),
        "reach": reach if reach is not None else unreached(direction),
    }
    check(out)
    return out


def check(one) -> None:
    """Refuse a region that is missing what its own kind cannot do without."""
    kind = one.get("kind")
    _known(kind, KINDS, "kind")
    for name in REQUIRED_ALWAYS + REQUIRED_BY_KIND[kind]:
        value = one.get(name)
        if value is None or is_absent(value) or (isinstance(value, str) and not value.strip()):
            raise EnvelopeRefused(
                RefusalReason.MALFORMED_RECEIPT,
                f"a {kind} envelope cannot omit {name}: " + shown(value),
            )
    for name in ("sender", "recipient"):
        endpoint = one[name]
        if is_absent(endpoint.get("taskId")):
            continue
        if not isinstance(endpoint.get("taskId"), str) or not endpoint["taskId"].strip():
            raise EnvelopeRefused(
                RefusalReason.MALFORMED_RECEIPT,
                f"the {name} is neither a task id nor a stated absence")


def announce_lines(one, *, compact=False) -> list:
    """What this message is and which one it is. Two lines, meant to be unshortenable.

    Split from the rest because a renderer that has to fit a byte budget keeps a PREFIX of a
    block, and these two are what a recipient needs even when everything under them is gone:
    what it owes, and the id to quote when it answers or when somebody asks whether this has
    already been reported. Who sent it and what scope it belongs to can be looked up from the
    record; what the message asks of you cannot be looked up from anywhere.
    """
    if compact:
        # One line, for a direction whose message already says in prose what it owes. The
        # correction direction spends eight lines telling a child there is nothing to
        # acknowledge and how to answer instead, so repeating that here would cost bytes
        # inside a floor to say something the recipient has already been told.
        return [f"  message: {one['kind']} {one['direction']}/{one['purpose']},"
                f" messageId {one['messageId']}, envelope {one['version']}"]
    return [
        f"  message: {one['kind']} - " + _owed(one),
        f"  messageId: {one['messageId']}  {one['direction']}/{one['purpose']}"
        f"  envelope: {one['version']}",
    ]


def context_lines(one) -> list:
    """Who, where and when. Shortenable: every line here is recoverable from the record."""
    lines = [
        f"  from: {one['sender']['role']} {shown(one['sender']['taskId'])}"
        f"  to: {one['recipient']['role']} {shown(one['recipient']['taskId'])}",
        f"  scope: {shown(one['scope'])}",
        f"  observedAt: {shown(one['observedAt'])}",
    ]
    if not is_absent(one["relationRevision"]):
        lines.append(f"  relationRevision: {one['relationRevision']}")
    if not is_absent(one["correlationId"]):
        lines.append(f"  answering: {one['correlationId']}")
    for pointer in one["evidence"]:
        lines.append(f"  evidence: {pointer}")
    return lines


def region_lines(one) -> list:
    """The whole region, for a surface with no legacy block of its own to merge with."""
    return (announce_lines(one)
            + [f"  relation: {one['relationId']}  basis: {shown(one['basis'])}"]
            + context_lines(one))


def _owed(one) -> str:
    owed = one["answerOwedBy"]
    if is_absent(owed):
        return "no answer is owed"
    if owed == USER:
        return "a decision is owed by the user, not by the recipient"
    return "an answer is owed by the recipient"


def reach_lines(one) -> list:
    """The five stages, each with what answered it. Never collapsed into one word."""
    ladder = one["reach"]
    out = ["  reach:"]
    for name in STAGES:
        entry = ladder[name]
        detail = f" - {entry['detail']}" if entry.get("detail") else ""
        source = f" ({entry['source']})" if entry.get("source") else ""
        out.append(f"    {name}: {entry['state']}{source}{detail}")
    return out


# ------------------------------------------------------------------- the directive pointer

# scope_directives has a nullable, format-free reference column and no message kind of its own
# - its link_kind says execution or reference, which is hierarchy authority and a different
# question entirely. So the envelope rides in the reference as a POINTER, and the row's own
# columns stay the identity. A directive written before this contract has no reference, parses
# as nothing, and is reported with an unknown kind, which is the true answer for it.
REFERENCE_PREFIX = VERSION + "|"


def directive_reference(*, purpose, link_id, digest, correlation_id=None) -> str:
    """The pointer stored on a directive row, derived from facts that row already holds."""
    identifier = message_id(direction=SUPERVISOR_TO_PARENT, relation_id=link_id,
                            purpose=purpose, subject=digest)
    return REFERENCE_PREFIX + "|".join(
        (SUPERVISOR_TO_PARENT, purpose, identifier, correlation_id or "-"))


def parse_reference(text):
    """What a stored reference says, or None when it is not one of ours.

    None is not a refusal. The column is free-form and predates this contract, so an operator's
    note in it is an ordinary thing to find and is left alone.
    """
    if not isinstance(text, str) or not text.startswith(REFERENCE_PREFIX):
        return None
    parts = text[len(REFERENCE_PREFIX):].split("|")
    if len(parts) != 4:
        return None
    direction, purpose, identifier, correlation = parts
    if direction not in DIRECTIONS or purpose not in PURPOSES.get(direction, {}):
        return None
    return {"direction": direction, "purpose": purpose, "messageId": identifier,
            "correlationId": None if correlation == "-" else correlation}


def contradiction(text, *, link_id, digest):
    """Whether a reference disagrees with the row it is being stored on.

    The check is a re-derivation rather than a comparison of copies. The pointer carries no
    duplicate of the row's scope, origin or link, so there is nothing to drift; what it carries
    is an id computed FROM those facts, and recomputing it from the row is what catches a
    pointer that belongs to another instruction. A reference that is not ours contradicts
    nothing, and neither does a missing one.
    """
    parsed = parse_reference(text)
    if parsed is None:
        return None
    if parsed["direction"] != SUPERVISOR_TO_PARENT:
        return (f"a directive reference names {parsed['direction']}, but a directive is always "
                + SUPERVISOR_TO_PARENT)
    expected = message_id(direction=SUPERVISOR_TO_PARENT, relation_id=link_id,
                          purpose=parsed["purpose"], subject=digest)
    if expected != parsed["messageId"]:
        return (f"the reference carries messageId {parsed['messageId']}, but this link and "
                f"digest derive {expected}; the pointer belongs to another instruction")
    return None
