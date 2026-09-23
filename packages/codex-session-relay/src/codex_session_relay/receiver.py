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
from .linkage import Linkage
from .mergeturn import LANDED
from .registry import LIVE, load_settings
from .sync import CONFIRMED

LEDGER_VERSION = 1

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
    found, answered = {}, {}
    try:
        with _snapshot(connection):
            _read_store(connection, found, answered, notes, receiver=receiver, role=role,
                        sender_role=sender_role, region=region, ledger=ledger)
    except sqlite3.Error as fault:
        # A partial reading is not a reading: nothing the store answered before it failed is
        # kept, so no field can agree on the strength of half a snapshot.
        notes.append("the store could not be read: " + type(fault).__name__ + ": " + str(fault))
        return {"record": record, "provenance": provenance, "notes": notes}
    record.update(found)
    provenance.update(answered)
    return {"record": record, "provenance": provenance, "notes": notes}


def _read_store(connection, found, answered, notes, *, receiver, role, sender_role, region,
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
    if row is None:
        notes.append("the receiver holds %d relationship(s), %d live, and the packet names none"
                     " of them, so no relationship was read" % (len(held), len(live)))
        return
    rid = row["relationship_id"]

    def answer(key, value, source):
        found[key] = value
        answered[key] = source

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
    entry = ((ledger or {}).get("assignments") or {}).get(rid)
    if entry and entry.get(packets.MODE):
        answer(packets.MODE, entry[packets.MODE],
               "ledger: assignment " + str(entry.get("messageId")))
    elif ledger is not None:
        notes.append("the reception ledger holds no accepted assignment for " + rid
                     + ", so the mode is unread")


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
    answer("relationRevision", link["revision"], "scope_links " + link["linkId"])
    if link["lower"]["taskId"] != row["child_task_id"] or \
            link["upper"]["taskId"] != row["parent_task_id"]:
        notes.append("the link now joins other tasks than this relationship's, so it has been"
                     " handed over")


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


def _settings(rows, task_id, notes):
    try:
        return load_settings(rows, task_id)
    except (ValueError, TypeError) as fault:
        notes.append("the recorded settings of " + task_id + " are unreadable: " + str(fault))
        return None


def _read_settings(rows, row, answer, notes):
    child, parent = row["child_task_id"], row["parent_task_id"]
    child_settings = _settings(rows, child, notes)
    if child_settings is not None:
        answer(packets.POLICY, {"model": child_settings.data.get("model"),
                                "effort": child_settings.data.get("reasoningEffort")},
               "authorized_settings[" + child + "]")
    else:
        notes.append("no settings are recorded for " + child)
    parent_settings = _settings(rows, parent, notes)
    if parent_settings is not None:
        answer(packets.CALLBACK, {"taskId": parent,
                                  "model": parent_settings.data.get("model"),
                                  "effort": parent_settings.data.get("reasoningEffort")},
               "relationships.parent_task_id + authorized_settings[" + parent + "]")
    else:
        notes.append("no settings are recorded for " + parent + ", the task a callback answers")
    bound = rolepolicy.bound_role(rows, child)
    if bound is None:
        answer("refusedPolicies", [], "rolepolicy: " + child + " holds no bound role, so no"
                                      " role pair applies")
        return
    if isinstance(bound, rolepolicy.Contested):
        notes.append(child + " is bound to more than one role, so its pair is unchecked")
        return
    if child_settings is None:
        return
    policy = rolepolicy.declared()
    if not policy:
        notes.append("no role policy resolved in this process, so whether the recorded pair is"
                     " authorised for " + bound + " is unchecked")
        return
    finding = rolepolicy.check_record(child_settings, bound, policy)
    model, effort = rolepolicy.recorded_pair(child_settings)
    refused = [] if finding is None else [{
        "model": model, "effort": effort, "code": finding.get("code"),
        "reason": rolepolicy.describe(finding)}]
    answer("refusedPolicies", refused, "rolepolicy " + str(policy.digest) + " for " + bound)


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
    except sqlite3.Error as fault:
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
    except (OSError, ValueError) as fault:
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
    return ledger


def record_answer(ledger, packet, answer) -> bool:
    """Write what this answer settles into the ledger. Returns whether anything changed.

    A first answer is recorded; a replay only upgrades a non-accepted answer to accepted and
    never downgrades one; a collision writes nothing. An accepted assignment records the mode
    and workflow it gave, once, for the relationship the receiver read - never overwritten, so
    no later packet can redefine it.
    """
    changed = False
    identifier = answer.get("messageId")
    state = (answer.get("repeat") or {}).get("state")
    accepted = answer.get("disposition") == packets.ACCEPTED
    if state == packets.FIRST:
        ledger["answered"][identifier] = {"contentDigest": answer["repeat"]["contentDigest"],
                                          "disposition": answer["disposition"]}
        changed = True
    elif state == packets.REPLAY and accepted and \
            ledger["answered"][identifier].get("disposition") != packets.ACCEPTED:
        ledger["answered"][identifier]["disposition"] = packets.ACCEPTED
        changed = True
    region = packet["envelope"]
    relationship = (answer.get("record") or {}).get("relationId")
    if accepted and state in (packets.FIRST, packets.REPLAY) \
            and region.get("direction") == envelope.PARENT_TO_CHILD \
            and region.get("purpose") == "assignment" and relationship \
            and relationship not in ledger["assignments"]:
        settings = packet.get(packets.POLICY) or {}
        ledger["assignments"][relationship] = {
            "mode": settings.get(packets.MODE), "workflow": settings.get("workflow"),
            "messageId": identifier,
            "dispatchRequestId": answer["record"].get(packets.DISPATCH_REQUEST)}
        changed = True
    return changed


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
    except BaseException:
        if os.path.exists(temporary):
            os.unlink(temporary)
        raise
