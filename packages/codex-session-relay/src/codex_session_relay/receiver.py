"""What the receiver reads for itself, from the relay store, before it acts on a packet.

`packets.reception` compares a packet with a record. Its only production caller used to take
that record as a file the caller supplied, so nothing compared a received packet with what the
receiver could read for itself. This module builds the record from the relay store.

It only reads. The caller opens the store read-only (`intent.read_only_connection` never
creates or migrates one), and every query runs inside one deferred transaction so the fields
come from one snapshot. A store that cannot be opened or read gives a record holding only the
receiver's own id, so every other field is a gap and the reading comes back unavailable.

Every field says what answered it. `provenance` maps each record key to the store object, the
ledger entry, the observation or the receiver's own id that supplied it.

Nothing the store does not hold is invented. A pull request's head is a forge reading, so it
comes only from an observation the caller supplies with its own source. The workflow's mode is
held by no store at all, so it comes from the receiver's own reception ledger: the mode of the
assignment it accepted.

The ledger also keeps what the receiver did, separately from what it was told. An answer is
recorded when it is given; that it was acted on is recorded only when the receiver says so
(record_applied), so a receiver that stops between the two gets the instruction back.

The packet selects and never answers. Its relation id may choose among relationships the
receiver already holds; it is never copied into the record, and its subject is only the key
under which the handover states are read.
"""

import fcntl
import json
import os
import sqlite3
import tempfile
from contextlib import contextmanager
from pathlib import Path

from . import envelope, packets, rolepolicy, transport
from .criteria import CriteriaService
from .errors import AckRefused, RefusalReason, RelayError
from .linkage import LIVE as LINK_LIVE, Linkage
from .mergeturn import LANDED
from .registry import LIVE, load_settings
from .settings import normalise_policy
from .sync import CONFIRMED

LEDGER_VERSION = 1

# The record key naming the dispatch whose registration began the current tenure. A tenure is
# one registration of the child on the relationship: the initial one, or a returning one after
# a supersession, which reuses the relationship id. Revision generations open under their own
# dispatches inside a tenure and do not begin one, so the mode and workflow the tenure's
# accepted assignment gave hold through them.
TENURE_DISPATCH = packets.TENURE_DISPATCH
# What the registry journals when a registration begins a tenure: the initial registration,
# and a returning one with the generation it opened. A generation opened for a revision (or
# with any reason) is journalled as generation_opened and begins nothing.
REGISTERED = "relationship_registered"
REOPENED = "relationship_tenure_reopened"

# What a store that is not the shape this reader expects raises. A missing table or column
# named in SQL is an sqlite3.Error; a column read by name from a row whose table lacks it is an
# IndexError from sqlite3.Row, and a record built from such a row a KeyError. All three mean
# the same thing here - the store could not be read - and none may escape as a host failure.
# And a value of the wrong shape in a column SQLite does not type strictly - a string in an
# INTEGER column compared with the journal's integer, a number where a task id belongs - raises
# TypeError, ValueError or AttributeError from the reader's own arithmetic. Those are the same
# answer: this store could not be read, and the reading comes back unavailable.
# So is a JSON value nested deeper than the reader can descend, which raises RecursionError
# from the decoder; the columns the reading parses catch it themselves, and this is the floor.
STORE_FAULTS = (sqlite3.Error, IndexError, KeyError, TypeError, ValueError, AttributeError,
                RecursionError)

# How deeply a recorded sandbox policy may nest and still be read. A sandbox policy is a flat
# object (its type, its writable roots, a few flags), and the reading copies it whole into the
# record, where it is compared and printed. Unbounded, a sandbox the decoder could just parse
# was printed a few levels deeper still and ended the answer as a host failure on 3.13. Only
# this value is bounded, because only it is copied; the rest of the settings row is not.
SANDBOX_DEPTH = 32

# What an observation may carry: the facts a forge or a filesystem answers and the store does
# not. Anything else in one is refused, so an observation cannot smuggle a relationship field.
OBSERVATION_FIELDS = {"repository": str, "prNumber": int, "headSha": str,
                      "artifactPath": str, "artifactDigest": str}

# Which relationships column holds a role's task id. A supervisor has no row in this table, so
# a packet sent upward is not read here; the supervisor channel reads its own messages back.
ROLE_COLUMN = {"parent": "parent_task_id", "child": "child_task_id"}
KEY_COLUMN = {"parentTaskId": "parent_task_id", "childTaskId": "child_task_id"}


class ReceptionRefused(RelayError):
    """An observation the reader cannot use."""


class LedgerUnusable(Exception):
    """A reception ledger that is not this receiver's, or not one at all. Never read through."""


class NotApplicable(Exception):
    """An application the ledger cannot record: nothing accepted was answered for this packet."""


class _Rows:
    """The Store.one/all surface over any connection.

    So readers written against a Store (Linkage.attachment, registry.load_settings,
    rolepolicy.bound_role) run against a read-only connection without a second spelling of
    their queries.
    """

    def __init__(self, connection):
        self.db = connection

    def one(self, sql, params=()):
        return self.db.execute(sql, params).fetchone()

    def all(self, sql, params=()):
        return self.db.execute(sql, params).fetchall()


@contextmanager
def _snapshot(connection):
    """One deferred read transaction, unless the caller already holds one."""
    opened = not connection.in_transaction
    if opened:
        connection.execute("BEGIN")
    try:
        yield
    finally:
        if opened and connection.in_transaction:
            connection.execute("ROLLBACK")


# ------------------------------------------------------------------------ the observation

def observation(document) -> dict:
    """A forge or filesystem reading the caller supplies, refused unless it says where from."""
    if not isinstance(document, dict):
        raise ReceptionRefused(
            RefusalReason.MALFORMED_RECEIPT,
            "an observation is an object naming its source, not a " + type(document).__name__)
    source = document.get("source")
    if not isinstance(source, str) or not source.strip():
        raise ReceptionRefused(
            RefusalReason.MALFORMED_RECEIPT,
            "an observation names its source; a head with nowhere it was read is a value copied"
            " from somewhere, which is what this reading refuses to take")
    unknown = sorted(set(document) - set(OBSERVATION_FIELDS) - {"source", "observedAt"})
    if unknown:
        raise ReceptionRefused(
            RefusalReason.MALFORMED_RECEIPT,
            "an observation carries only " + ", ".join(sorted(OBSERVATION_FIELDS))
            + "; it cannot answer " + ", ".join(unknown))
    for name, wanted in OBSERVATION_FIELDS.items():
        value = document.get(name)
        if value is None:
            continue
        if isinstance(value, bool) or not isinstance(value, wanted) or (
                wanted is str and not value.strip()) or (wanted is int and value < 1):
            raise ReceptionRefused(
                RefusalReason.MALFORMED_RECEIPT,
                name + " in an observation is a " + wanted.__name__ + ", not " + repr(value))
    if document.get("observedAt") is not None and not isinstance(document["observedAt"], str):
        raise ReceptionRefused(RefusalReason.MALFORMED_RECEIPT,
                               "observedAt in an observation is a timestamp string")
    return dict(document)


# ------------------------------------------------------------------------------ the record

def read(connection, *, receiver, packet, observation=None, ledger=None) -> dict:
    """The receiver's own record for this packet, with what answered each field."""
    region = packet["envelope"]
    sender_role, role = envelope.ENDPOINT_ROLES[region["direction"]]
    own = packets.RECORD_TASK_KEY[role]
    record, provenance, notes = {own: receiver}, {own: "receiver"}, []
    for name in OBSERVATION_FIELDS:
        if observation and observation.get(name) is not None:
            record[name] = observation[name]
            provenance[name] = "observation: " + observation["source"]
    if connection is None:
        notes.append("the store could not be opened read-only, so nothing but the receiver's own"
                     " id was read")
        return {"record": record, "provenance": provenance, "notes": notes}
    fields, sources = {}, {}
    try:
        with _snapshot(connection):
            _read_store(connection, fields, sources, notes, receiver=receiver, role=role,
                        sender_role=sender_role, region=region, ledger=ledger)
    except STORE_FAULTS as fault:
        # A partial reading is not a reading: nothing the store answered before it failed is
        # kept, so no field can agree on the strength of half a snapshot.
        notes.append("the store could not be read: " + type(fault).__name__ + ": " + str(fault))
        return {"record": record, "provenance": provenance, "notes": notes}
    record.update(fields)
    provenance.update(sources)
    return {"record": record, "provenance": provenance, "notes": notes}


def _read_store(connection, fields, sources, notes, *, receiver, role, sender_role, region,
                ledger):
    rows = _Rows(connection)
    column = ROLE_COLUMN.get(role)
    if column is None:
        notes.append("the relay store keeps no relationship row for a " + role + ", so a packet"
                     " sent to one is read back through the supervisor channel, not here")
        return
    held = rows.all("SELECT * FROM relationships WHERE " + column + " = ?"
                    " ORDER BY updated_at DESC, relationship_id", (receiver,))
    live = [row for row in held if row["status"] in LIVE]
    if len(live) == 1:
        row, how = live[0], "the receiver's one live relationship"
    else:
        named = region.get("relationId")
        row = next((one for one in held if one["relationship_id"] == named), None)
        how = "the receiver's relationship the packet named"
        if row is None and packets._first_assignment(region):
            # A first assignment was sent before its relationship existed, so it names the
            # dispatch, not a relationship. A child shared by several live relationships takes
            # it on the one whose current generation that dispatch opened, and on no other.
            opened = [one for one in live if _opened_by(rows, one) == named]
            if len(opened) == 1:
                row, how = opened[0], ("the receiver's live relationship whose current"
                                       " generation the packet's dispatch opened")
    if row is None:
        notes.append("the receiver holds %d relationship(s), %d live, and the packet names none"
                     " of them, so no relationship was read" % (len(held), len(live)))
        return
    rid = row["relationship_id"]

    def answer(key, value, source):
        fields[key] = value
        sources[key] = source

    answer("relationId", rid, "relationships (" + how + ")")
    for key, key_column in KEY_COLUMN.items():
        if key != packets.RECORD_TASK_KEY[role]:
            answer(key, row[key_column], "relationships." + key_column)
    answer(packets.ISSUE, row["issue_key"], "relationships.issue_key")
    answer(packets.RELATION_STATUS, row["status"], "relationships.status")
    answer(packets.GENERATION, row["execution_generation"],
           "relationships.execution_generation")
    opened = rows.one("SELECT dispatch_request_id FROM generations"
                      " WHERE relationship_id = ? AND execution_generation = ?",
                      (rid, row["execution_generation"]))
    if opened is not None:
        answer(packets.DISPATCH_REQUEST, opened["dispatch_request_id"],
               "generations (the current generation)")
    else:
        notes.append("no generations row opened the current generation")
    _read_link(rows, rid, row, answer, notes)
    _read_criteria(rows, connection, rid, answer, notes)
    _read_settings(rows, row, answer, notes)
    start = _tenure_start(rows, rid, row["execution_generation"], notes)
    tenure = None
    if start is not None:
        answer(packets.TENURE_GENERATION, start,
               "journal (the registration that began the current tenure)")
        begun = rows.one("SELECT dispatch_request_id FROM generations"
                         " WHERE relationship_id = ? AND execution_generation = ?", (rid, start))
        if begun is None:
            notes.append("no generations row holds generation %d, which the registration that"
                         " began the current tenure opened, so its dispatch is unread" % start)
        else:
            tenure = begun["dispatch_request_id"]
    if tenure is not None:
        answer(TENURE_DISPATCH, tenure,
               "generations (the registration that began the current tenure)")
    entry = ((ledger or {}).get("assignments") or {}).get(rid)
    if entry and entry.get(packets.MODE) and tenure is not None \
            and entry.get("dispatchRequestId") == tenure:
        # Only the current tenure's. A returning registration reuses the relationship id, and
        # the mode and workflow of the earlier tenure are not this one's; a revision generation
        # inside the tenure keeps them.
        answer(packets.MODE, entry[packets.MODE],
               "ledger: assignment " + str(entry.get("messageId")))
        answer("workflow", entry["workflow"], "ledger: assignment " + str(entry.get("messageId")))
    elif entry:
        notes.append("the reception ledger's assignment for " + rid + " was accepted under"
                     " dispatch " + str(entry.get("dispatchRequestId")) + ", not the one whose"
                     " registration began the current tenure, so this tenure's mode and"
                     " workflow are unread")
    elif ledger is not None:
        notes.append("the reception ledger holds no accepted assignment for " + rid
                     + ", so the mode and the workflow are unread")


def _tenure_start(rows, rid, current, notes):
    """The generation the current tenure's registration opened, or None where unread.

    Read from the journal the registry writes in the same transaction as the registration,
    because the generations table records why a generation opened only as a free reason: a
    revision could be opened with the initial reason, and a row can be missing. The latest
    returning registration at or below the current generation began the tenure; with none,
    the initial registration did, at generation 1. A journal that answers neither, or holds a
    reopening it cannot read, leaves the tenure unread.

    And checked against the generations rows, which answer the same question independently:
    only a returning registration writes a generation with no reason (a revision has to name
    one), so the latest reasonless generation at or below the current one - or 1 - is where
    they say the tenure began. The absence of a record cannot prove there was no return: a
    damaged reopening row read as "never returned" handed the earlier tenure's mode to its
    stale packets. So the two have to agree, and where they do not the tenure is unread.
    """
    journalled = _journal_tenure_start(rows, rid, current, notes)
    reasonless = rows.one("SELECT MAX(execution_generation) AS start FROM generations"
                          " WHERE relationship_id = ? AND execution_generation <= ?"
                          " AND reason IS NULL", (rid, current))
    counted = reasonless["start"] if reasonless is not None and \
        reasonless["start"] is not None else 1
    if journalled is None:
        return None
    if journalled != counted:
        notes.append("the registration journal says the current tenure of " + rid + " began at"
                     " generation %s and the generation rows say %s, so the tenure is unread"
                     % (journalled, counted))
        return None
    return journalled


def _journal_tenure_start(rows, rid, current, notes):
    reopened = rows.all("SELECT detail FROM journal WHERE kind = ? AND subject = ?"
                        " ORDER BY seq DESC", (REOPENED, rid))
    starts = []
    for one in reopened:
        try:
            generation = json.loads(one["detail"] or "{}").get("executionGeneration")
        except (ValueError, AttributeError, RecursionError):
            generation = None
        if isinstance(generation, bool) or not isinstance(generation, int):
            notes.append("a returning registration of " + rid + " is journalled without a"
                         " readable generation, so the current tenure is unread")
            return None
        starts.append(generation)
    earlier = [generation for generation in starts if generation <= current]
    if earlier:
        return max(earlier)
    if rows.one("SELECT 1 FROM journal WHERE kind = ? AND subject = ? LIMIT 1",
                (REGISTERED, rid)) is not None:
        return 1
    notes.append("the journal holds no registration of " + rid + ", so the current tenure is"
                 " unread")
    return None


def _opened_by(rows, row):
    opened = rows.one("SELECT dispatch_request_id FROM generations"
                      " WHERE relationship_id = ? AND execution_generation = ?",
                      (row["relationship_id"], row["execution_generation"]))
    return None if opened is None else opened["dispatch_request_id"]


def _read_link(rows, rid, row, answer, notes):
    attachment = Linkage(rows, None).attachment(rid)
    if attachment is None:
        # The store's own answer rather than an absence: an unscoped relationship has no link.
        answer("relationRevision", None, "relationship_scope: unscoped, so no link and no"
                                         " revision")
        return
    link = attachment.get("link")
    if link is None:
        notes.append("the relationship is scoped to " + str(attachment.get("projectKey"))
                     + " but no execution link joins it, so its revision is unread")
        return
    # The revision answers for this relationship only while the link is live, has no successor
    # and still joins this relationship's two tasks. Otherwise it is some other tenure's
    # revision, and quoting it would let a packet agree with a link that has moved on.
    if link["status"] not in LINK_LIVE or link["supersededBy"]:
        notes.append("the execution link " + link["linkId"] + " is " + str(link["status"])
                     + (", superseded by " + str(link["supersededBy"])
                        if link["supersededBy"] else "")
                     + ", so it no longer answers for this relationship and its revision is"
                       " unread")
        return
    if link["lower"]["taskId"] != row["child_task_id"] or \
            link["upper"]["taskId"] != row["parent_task_id"]:
        notes.append("the execution link " + link["linkId"] + " now joins "
                     + str(link["upper"]["taskId"]) + " and " + str(link["lower"]["taskId"])
                     + ", not this relationship's tasks, so it has been handed over and its"
                       " revision is unread")
        return
    answer("relationRevision", link["revision"], "scope_links " + link["linkId"])


def _read_criteria(rows, connection, rid, answer, notes):
    try:
        # The exact validity rule registration uses - managed, one source, one digest, and the
        # digest recomputes - rather than a second spelling of it.
        held = CriteriaService(rows, None)._locked_set(connection, rid)
    except AckRefused as refused:
        notes.append("the registered criteria are not one valid set: " + refused.detail)
        return
    if held is None:
        notes.append("no criteria set is registered for " + rid)
        return
    answer(packets.CRITERIA_DIGEST, held["setDigest"], "canonical_criteria (managed set)")


def _settings(rows, task_id, notes, about=""):
    """The task's recorded settings, or None where none are recorded or they cannot be parsed.

    Parsed as the writer wrote them and bounded nowhere else. The writer bounds only what a
    reading answers with (see _recorded_text), so a bound here of its own turned records the
    writer accepts - a deep value under a key the reading never uses - into unread ones.
    """
    try:
        held = load_settings(rows, task_id)
    except (ValueError, TypeError, RecursionError) as fault:
        # RecursionError: nested deeper than the decoder descends on this interpreter.
        notes.append("the recorded settings of " + task_id + " are unreadable: "
                     + type(fault).__name__ + ": " + str(fault))
        return None
    if held is None:
        notes.append("no settings are recorded for " + task_id + about)
    return held


def _recorded_text(settings, name, task_id, notes):
    """One recorded value the reading answers with, where it is the text every writer records.

    The model, the effort and the approval are text in any row the writer accepts
    (TaskSettings.require_usable). Another shape is not a reading of them - and a structure
    copied from one would be compared and printed from the record - so it is unread, a gap.
    """
    value = settings.data.get(name)
    if value is None or isinstance(value, str):
        return value
    notes.append("the recorded " + name + " of " + task_id + " is a " + type(value).__name__
                 + ", not the text a writer records, so it is unread")
    return None


def _nesting(value) -> int:
    """How many objects and lists deep a parsed JSON value goes. Iterative, so it cannot recurse."""
    deepest, pending = 0, [(value, 1)]
    while pending:
        one, depth = pending.pop()
        if isinstance(one, dict):
            one = list(one.values())
        if isinstance(one, list):
            deepest = max(deepest, depth)
            pending.extend((inner, depth + 1) for inner in one)
    return deepest


def _recorded_sandbox(settings, task_id, notes):
    """The recorded sandbox policy as the reading compares it, where it is one."""
    sandbox = normalise_policy(settings.data.get("sandbox"))
    if sandbox is not None and _nesting(sandbox) > SANDBOX_DEPTH:
        notes.append("the recorded sandbox of " + task_id + " nests deeper than %d levels, which"
                     " no sandbox policy does, so it is unread" % SANDBOX_DEPTH)
        return None
    return sandbox


def _read_settings(rows, row, answer, notes):
    child, parent = row["child_task_id"], row["parent_task_id"]
    child_settings = _settings(rows, child, notes)
    if child_settings is not None:
        # The permissions travel with the pair: a packet stating another sandbox or approval
        # is compared with these, and a sandbox this record cannot read stays None, a gap.
        answer(packets.POLICY, {
            "model": _recorded_text(child_settings, "model", child, notes),
            "effort": _recorded_text(child_settings, "reasoningEffort", child, notes),
            "sandbox": _recorded_sandbox(child_settings, child, notes),
            "approval": _recorded_text(child_settings, "approvalPolicy", child, notes)},
               "authorized_settings[" + child + "]")
    parent_settings = _settings(rows, parent, notes, ", the task a callback answers")
    if parent_settings is not None:
        answer(packets.CALLBACK, {
            "taskId": parent,
            "model": _recorded_text(parent_settings, "model", parent, notes),
            "effort": _recorded_text(parent_settings, "reasoningEffort", parent, notes)},
               "relationships.parent_task_id + authorized_settings[" + parent + "]")
    # Each recorded pair judged against the current role policy for that task's own role: the
    # child's for the policy a packet states, the parent's for the callback. A record agrees
    # with a packet naming the pair it was recorded with even after the policy has moved that
    # role on, and nothing re-records a task when the policy moves.
    _role_refusals(rows, child, child_settings, "refusedPolicies", answer, notes)
    _role_refusals(rows, parent, parent_settings, packets.CALLBACK_REFUSALS, answer, notes)


def _role_refusals(rows, task_id, settings, key, answer, notes):
    bound = rolepolicy.bound_role(rows, task_id)
    if bound is None:
        answer(key, [], "rolepolicy: " + task_id + " holds no bound role, so no role pair"
                        " applies")
        return
    if isinstance(bound, rolepolicy.Contested):
        notes.append(task_id + " is bound to more than one role, so its pair is unchecked")
        return
    if settings is None:
        return
    policy = rolepolicy.declared()
    if not policy:
        notes.append("no role policy resolved in this process, so whether the recorded pair of "
                     + task_id + " is authorised for " + bound + " is unchecked")
        return
    model, effort = rolepolicy.recorded_pair(settings)
    if any(value is not None and not isinstance(value, str) for value in (model, effort)):
        # Not a pair any writer records, so not one the role policy can judge: unread, and
        # never folded into the refusal list the answer prints.
        notes.append("the recorded pair of " + task_id + " is not text, so whether it is"
                     " authorised for " + bound + " is unchecked")
        return
    finding = rolepolicy.check_record(settings, bound, policy)
    refused = [] if finding is None else [{
        "model": model, "effort": effort, "code": finding.get("code"),
        "reason": rolepolicy.describe(finding)}]
    answer(key, refused, "rolepolicy " + str(policy.digest) + " for " + bound)


# ------------------------------------------------------------------------- the seven states

def ladder(connection, *, relationship_id, subject, observation=None) -> dict:
    """The handover states for the packet's subject event, as this store answers them."""
    states = packets.unobserved()
    states[packets.LINEAR_DONE] = envelope.stage(
        envelope.UNMEASURED, detail="the store holds no reading of the issue's status; read it"
                                    " in Linear")
    sync = envelope.stage(envelope.UNMEASURED, detail="nothing was read")
    if connection is None or not relationship_id:
        return {"handover": states, "coordinationSync": sync}
    try:
        with _snapshot(connection):
            rows = _Rows(connection)
            known = rows.one("SELECT 1 FROM events WHERE event_id = ? AND relationship_id = ?",
                             (subject, relationship_id))
            if known is None:
                for name in packets.PROGRESSION:
                    if name not in (packets.READ, packets.LINEAR_DONE):
                        states[name] = envelope.stage(
                            envelope.UNMEASURED,
                            detail="the packet's subject names no event of this relationship")
                return {"handover": states, "coordinationSync": sync}
            states[packets.TRANSPORT_ACCEPTED] = _transport(rows, subject)
            states[packets.RELAY_ACK] = _acknowledgement(rows, subject)
            states[packets.CRITERIA_VERDICT], states[packets.PARENT_ACCEPTANCE] = \
                _verdict(rows, subject)
            states[packets.MERGE_LANDING] = _landing(rows, relationship_id, observation or {})
            sync = _coordination(rows, relationship_id, subject)
    except STORE_FAULTS as fault:
        detail = "the store could not be read: " + type(fault).__name__
        for name in packets.PROGRESSION:
            if name not in (packets.READ, packets.LINEAR_DONE):
                states[name] = envelope.stage(envelope.UNMEASURED, detail=detail)
        sync = envelope.stage(envelope.UNMEASURED, detail=detail)
    packets.check_progression(states)
    return {"handover": states, "coordinationSync": sync}


def _transport(rows, subject):
    attempts = rows.all("SELECT state, internal_state FROM attempts WHERE event_id = ?",
                        (subject,))
    source = packets.PROGRESSION_SOURCES[packets.TRANSPORT_ACCEPTED]
    if any(one["state"] == transport.DISPATCHED for one in attempts):
        return envelope.stage(envelope.YES, source=source, detail="an attempt was dispatched")
    if any(one["state"] == transport.HELD_UNCERTAIN or one["internal_state"] != "settled"
           for one in attempts):
        return envelope.stage(envelope.UNMEASURED,
                              detail="an attempt is held uncertain or not yet settled")
    ended = sorted({str(one["state"]) for one in attempts})
    return envelope.stage(envelope.NO, source=source,
                          detail="no attempt" if not attempts
                          else "attempts ended as " + ", ".join(ended)
                          + "; inbox_only is an approval failure with no send")


def _acknowledgement(rows, subject):
    source = packets.PROGRESSION_SOURCES[packets.RELAY_ACK]
    ack = rows.one("SELECT accepted, verified FROM acks WHERE event_id = ?", (subject,))
    if ack is None:
        return envelope.stage(envelope.NO, source=source, detail="no acknowledgement row")
    if ack["accepted"] and ack["verified"] == "verified":
        return envelope.stage(envelope.YES, source=source, detail="accepted and verified")
    if ack["accepted"]:
        return envelope.stage(envelope.CONDITIONAL, source=source,
                              detail="recorded, verification " + str(ack["verified"]))
    return envelope.stage(envelope.NO, source=source, detail="the acknowledgement rejected it")


def _verdict(rows, subject):
    source = packets.PROGRESSION_SOURCES[packets.CRITERIA_VERDICT]
    verdict = rows.one("SELECT verdict FROM verdicts WHERE event_id = ?", (subject,))
    if verdict is None:
        none = envelope.stage(envelope.NO, source=source, detail="no verdict")
        return none, none
    judged = envelope.stage(envelope.YES, source=source, detail="verdict " + verdict["verdict"])
    accepted = envelope.stage(
        envelope.YES if verdict["verdict"] == "verified" else envelope.NO,
        source=packets.PROGRESSION_SOURCES[packets.PARENT_ACCEPTANCE],
        detail="verdict " + verdict["verdict"])
    return judged, accepted


def _landing(rows, relationship_id, observed):
    if not all(observed.get(name) for name in ("repository", "prNumber", "headSha")):
        return envelope.stage(
            envelope.UNMEASURED, detail="which repository, pull request and head is a forge"
                                        " reading; supply all three in the observation")
    source = packets.PROGRESSION_SOURCES[packets.MERGE_LANDING]
    turns = rows.all("SELECT turn_id, state, candidate_head FROM merge_turns"
                     " WHERE relationship_id = ? AND pr_number = ? AND repository = ?"
                     " ORDER BY updated_at DESC",
                     (relationship_id, observed["prNumber"], observed["repository"]))
    landed = [turn for turn in turns if turn["state"] == LANDED]
    for turn in landed:
        if turn["candidate_head"] == observed["headSha"]:
            return envelope.stage(envelope.YES, source=source,
                                  detail="turn " + turn["turn_id"] + " landed the observed head")
    if landed:
        return envelope.stage(envelope.NO, source=source, detail=(
            "landed only for head " + ", ".join(sorted({t["candidate_head"] for t in landed}))
            + ", not the observed one"))
    return envelope.stage(envelope.NO, source=source,
                          detail="no landed turn" if turns else "no merge turn")


def _coordination(rows, relationship_id, subject):
    outbox = rows.all("SELECT state FROM sync_outbox WHERE relationship_id = ? AND event_id = ?",
                      (relationship_id, subject))
    detail = ("a coordination summary block written and read back; this says nothing about"
              " the issue's status")
    if any(one["state"] == CONFIRMED for one in outbox):
        return envelope.stage(envelope.YES, source="sync_outbox", detail=detail)
    return envelope.stage(envelope.NO, source="sync_outbox",
                          detail="no confirmed outbox row" if outbox else "no outbox row")


# ---------------------------------------------------------------------------- the check

def check(connection, packet, *, receiver, observation=None, ledger=None) -> dict:
    """The packet against the receiver's own reading, with its states and its repeat answer."""
    packets.check(packet)
    reading = read(connection, receiver=receiver, packet=packet, observation=observation,
                   ledger=ledger)
    answer = packets.reception(packet, reading["record"])
    answer.update(recordSource="store", receiver=receiver, record=reading["record"],
                  provenance=reading["provenance"], notes=reading["notes"])
    held = ladder(connection, relationship_id=reading["record"].get("relationId"),
                  subject=packet["envelope"].get("subject"), observation=observation)
    answer["handover"] = held["handover"]
    answer["coordinationSync"] = held["coordinationSync"]
    answer["promotions"] = packets.unsupported_promotions(held["handover"])
    claimed = packets.claims(packet.get("progression"), held["handover"])
    answer["unbackedClaims"] = claimed["unbacked"]
    answer["unmeasurableClaims"] = claimed["unmeasurable"]
    repeated = None if ledger is None else packets.repeat(packet, ledger["answered"])
    return packets.settle_repeat(answer, repeated)


# ---------------------------------------------------------------------------- the ledger

def empty_ledger(receiver) -> dict:
    return {"version": LEDGER_VERSION, "receiver": receiver, "answered": {}, "assignments": {}}


@contextmanager
def ledger_lock(path):
    """An exclusive lock on the ledger's sidecar, held across its read, decision and write."""
    # The sidecar lives beside the ledger, so the directory a first ledger will be written to
    # has to exist before the lock can be taken, not only before the save.
    Path(path).parent.mkdir(parents=True, exist_ok=True)
    descriptor = os.open(str(path) + ".lock", os.O_CREAT | os.O_RDWR, 0o600)
    try:
        fcntl.flock(descriptor, fcntl.LOCK_EX)
        yield
    finally:
        fcntl.flock(descriptor, fcntl.LOCK_UN)
        os.close(descriptor)


def load_ledger(path, receiver) -> dict:
    """This receiver's ledger, a new one where none exists, and nothing else."""
    target = Path(path)
    if not target.exists():
        return empty_ledger(receiver)
    try:
        ledger = json.loads(target.read_text(encoding="utf-8"))
    except (OSError, ValueError, RecursionError) as fault:
        raise LedgerUnusable("the reception ledger at " + str(target) + " could not be read: "
                             + type(fault).__name__ + ": " + str(fault)) from fault
    if not isinstance(ledger, dict) or ledger.get("version") != LEDGER_VERSION or not \
            isinstance(ledger.get("answered"), dict) or \
            not isinstance(ledger.get("assignments"), dict):
        raise LedgerUnusable("the file at " + str(target) + " is not a version "
                             + str(LEDGER_VERSION) + " reception ledger")
    if ledger.get("receiver") != receiver:
        raise LedgerUnusable("the reception ledger at " + str(target) + " belongs to "
                             + repr(ledger.get("receiver")) + ", not " + repr(receiver)
                             + "; another receiver's answers are not this one's")
    problem = _entry_problem(ledger)
    if problem:
        raise LedgerUnusable("the reception ledger at " + str(target) + " is damaged: " + problem
                             + "; a ledger that cannot say what was answered is not read"
                               " through")
    return ledger


def _entry_problem(ledger):
    """What makes an entry unreadable, or None. Every entry, so none can fail later mid-check.

    Every field an entry is written with is required, not defaulted. A missing "applied" read
    as false handed a correction already done back to be done again; a ledger that has lost
    part of what it recorded is damaged, and is refused rather than read as if it had said
    the harmless thing.
    """
    for identifier, entry in ledger["answered"].items():
        if not isinstance(entry, dict) or not isinstance(entry.get("contentDigest"), str) \
                or not entry["contentDigest"].strip() \
                or entry.get("disposition") not in packets.DISPOSITIONS \
                or not isinstance(entry.get("applied"), bool) \
                or not isinstance(entry.get("toldToAct"), bool):
            return ("answered entry " + repr(identifier) + " is not a content digest, a"
                    " disposition, whether a check said act and whether it was applied")
        if entry["applied"] and (not entry["toldToAct"]
                                 or entry["disposition"] != packets.ACCEPTED):
            # No writer produces this: applied is recorded only for an accepted packet some
            # check said to act on. Read, it would count work never asked for as done.
            return ("answered entry " + repr(identifier) + " says applied for a packet it"
                    " does not hold as accepted and told to act on")
    for relationship, entry in ledger["assignments"].items():
        # The mode it holds is read as the mode of an accepted assignment, so the entry has to
        # be able to name that assignment: an execution mode, a workflow and a message id,
        # and the dispatch it was accepted under.
        if not isinstance(entry, dict) or entry.get("mode") not in packets.MODES or any(
                not isinstance(entry.get(name), str) or not entry[name].strip()
                for name in ("workflow", "messageId", "dispatchRequestId")):
            return ("assignment entry " + repr(relationship) + " is not an execution mode, a"
                    " workflow, the message id of the assignment and a dispatch id")
    return None


def record_answer(ledger, packet, answer) -> bool:
    """Write what this answer settles into the ledger. Returns whether anything changed.

    A first answer is recorded; a replay only upgrades a non-accepted answer to accepted and
    never downgrades one; a collision writes nothing. An accepted assignment records the mode
    and workflow it gave, once, for the relationship the receiver read - never overwritten, so
    no later packet can redefine it. Nothing here says the answer was acted on: a first answer
    is recorded as not applied, and only record_applied changes that. What every check told
    the receiver is kept as toldToAct, true once any check said act and never taken back: an
    application may be recorded only for a packet the receiver was told to act on, and a
    later check that held act (a paused relationship) does not unsay what an earlier one said
    about work the receiver may already have done.

    The assignment is recorded per tenure. A returning registration reuses the relationship
    id under a generation a new dispatch opened, so an accepted assignment under another
    dispatch than the one recorded replaces it; under the same dispatch it never does.
    """
    changed = False
    identifier = answer.get("messageId")
    state = (answer.get("repeat") or {}).get("state")
    accepted = answer.get("disposition") == packets.ACCEPTED
    told = answer.get("act") is True
    if state == packets.FIRST:
        ledger["answered"][identifier] = {"contentDigest": answer["repeat"]["contentDigest"],
                                          "disposition": answer["disposition"],
                                          "applied": False, "toldToAct": told}
        changed = True
    elif state == packets.REPLAY:
        entry = ledger["answered"][identifier]
        if accepted and entry.get("disposition") != packets.ACCEPTED:
            entry["disposition"] = packets.ACCEPTED
            changed = True
        if told and entry.get("toldToAct") is not True:
            entry["toldToAct"] = True
            changed = True
    region = packet["envelope"]
    relationship = (answer.get("record") or {}).get("relationId")
    dispatch = (answer.get("record") or {}).get(TENURE_DISPATCH)
    recorded = ledger["assignments"].get(relationship) if relationship else None
    if accepted and state in (packets.FIRST, packets.REPLAY) \
            and region.get("direction") == envelope.PARENT_TO_CHILD \
            and region.get("purpose") == "assignment" and relationship and dispatch \
            and (recorded is None or recorded.get("dispatchRequestId") != dispatch):
        settings = packet.get(packets.POLICY) or {}
        ledger["assignments"][relationship] = {
            "mode": settings.get(packets.MODE), "workflow": settings.get("workflow"),
            "messageId": identifier,
            "dispatchRequestId": dispatch}
        changed = True
    return changed


def record_applied(ledger, packet) -> dict:
    """Record that the receiver has acted on a packet its ledger answered accepted.

    This is the receiver's own statement, made after it acted, and it reads no store: whether
    the instruction was applied is a fact about the receiver, and today's reading has already
    been given. It is refused unless this ledger answered this very packet - same id, same
    content - as accepted, so an application cannot be recorded for a packet never checked,
    for one that was refused or unavailable, or for a different packet reusing the id.
    """
    packets.check(packet)
    repeated = packets.repeat(packet, ledger["answered"])
    identifier = repeated["messageId"]
    if repeated["state"] == packets.FIRST:
        raise NotApplicable(
            "message " + str(identifier) + " was never checked against this ledger, so there is"
            " no accepted answer to record as applied; run the check first")
    if repeated["state"] == packets.COLLISION:
        raise NotApplicable("message " + str(identifier) + " cannot be recorded as applied: "
                            + repeated["reason"])
    entry = ledger["answered"][identifier]
    if entry.get("disposition") != packets.ACCEPTED:
        raise NotApplicable(
            "message " + str(identifier) + " was answered " + str(entry.get("disposition"))
            + ", so there was nothing to act on and nothing to record as applied")
    before = entry.get("applied") is True
    if not before and entry.get("toldToAct") is not True:
        raise NotApplicable(
            "no check of message " + str(identifier) + " has said act (it was held, for"
            " example while the relationship was paused), so there is nothing acted on to"
            " record; check it again and record it applied only after a check says act")
    entry["applied"] = True
    return {"messageId": identifier, "contentDigest": repeated["contentDigest"],
            "applied": True, "alreadyApplied": before}


def save_ledger(path, ledger) -> None:
    """Replace the ledger atomically, in its own directory, so a reader never sees half."""
    target = Path(path)
    target.parent.mkdir(parents=True, exist_ok=True)
    descriptor, temporary = tempfile.mkstemp(dir=str(target.parent), prefix=target.name + ".")
    try:
        with os.fdopen(descriptor, "w", encoding="utf-8") as handle:
            json.dump(ledger, handle, indent=2, sort_keys=True)
            handle.flush()
            os.fsync(handle.fileno())
        os.replace(temporary, target)
        # The rename is what records the answer, so it is made durable too: a rename lost to a
        # power failure would bring back an answer, or an application, already given.
        directory = os.open(str(target.parent), os.O_RDONLY)
        try:
            os.fsync(directory)
        finally:
            os.close(directory)
    except BaseException:
        if os.path.exists(temporary):
            os.unlink(temporary)
        raise
