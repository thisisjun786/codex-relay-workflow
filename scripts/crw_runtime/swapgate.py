"""Whether it is safe to replace a runtime, read rather than assumed.

OPS-4.4 sequences an update around a daemon that is not running and open attempts that have been
reconciled, and OPS-4.5 forbids an install step from touching the store at all. Three readings
answer that, and each fills only its own cell: a daemon is never reported stopped because nobody
could ask it, an inventory nobody could read is not an empty inventory, and a store nobody could
open is not an absent store.

The verdict has three values and two of them keep the existing installation. ALLOWED needs every
cell established and none of them blocking. BLOCKED means a cell answered and its answer was no.
UNESTABLISHED means a cell could not answer, which keeps the installation for the same reason: a
check that could not be made is not a check that passed.

Nothing here starts or stops anything. OPS-4.1 gives the service to the scope operator, so a
running daemon is a refusal here rather than something to resolve.
"""

from . import reading, scope

ALLOWED = "ALLOWED"
BLOCKED = "BLOCKED"
UNESTABLISHED = "UNESTABLISHED"
VERDICTS = (ALLOWED, BLOCKED, UNESTABLISHED)

# What comparing the store's tables with the candidate's can say.
#
# Comparing recorded schema VERSIONS would say nothing at all: the relay declares version one,
# has never raised it, writes it once with INSERT OR IGNORE when the database is created, and
# grows its schema through separate CREATE TABLE IF NOT EXISTS statements. Every store therefore
# agrees with every candidate at version one, so a version comparison detects neither a
# downgrade nor an upgrade while looking exactly like a check. The tables are what differ.
AGREES = "AGREES"
EXTENDS = "EXTENDS"
NARROWS = "NARROWS"
DIFFERS = "DIFFERS"
NO_STORE = "NO_STORE"
TABLE_ANSWERS = (AGREES, EXTENDS, NARROWS, DIFFERS, NO_STORE)

# The in-flight cell's established-absent answer, given a name. No store means no attempt can
# be open, and that is a count this command READ rather than one nobody could take. Named
# because the answer went missing while it had no name: the cell could say "unreadable" and
# could not say "nothing is there", so a clean host could never promote.
NO_ATTEMPTS = 0

# Only an identical schema, or no store at all, lets a replacement through.
#
# NARROWS loses data outright: the store holds a table the candidate does not declare, so the
# runtime being installed cannot preserve what is in it. That is the implicit downgrade the
# issue forbids.
#
# EXTENDS and DIFFERS refuse for the contract's reason rather than for that one. The relay
# opens its store read-write and runs its whole DDL script on every open, so a candidate whose
# schema is not the store's schema APPLIES the difference the first time the new daemon starts.
# OPS-4.5 says a change that needs a different schema is its own decision, in its own issue,
# with a copied backup of the whole state directory taken first. Letting an update wave it
# through is precisely the implicit migration that clause forbids, and an update is not the
# place either direction is decided.
TABLES_BLOCKING = (NARROWS, EXTENDS, DIFFERS)

def _daemon_blocks(cell):
    """A supervisor is running, so the runtime under it is not replaced (OPS-4.4)."""
    return cell.get("answer") == scope.RUNNING


def _in_flight_blocks(cell):
    """Any attempt still open is a handover in flight (OPS-4.4)."""
    return cell.get("answer") != NO_ATTEMPTS


def _tables_block(cell):
    """Only the direction that loses data refuses."""
    return cell.get("answer") in TABLES_BLOCKING


# Each cell, the reading that answers it, and the predicate that decides whether its answer
# refuses. A member carries its predicate as well as its provenance, because a gate that wrote
# its own test per cell is a gate whose rule and whose declaration are two facts kept equal by
# hand. The verdict below applies what this map names and nothing else.
GATE_CELLS = {
    "daemon": (("scope", "service_state"), _daemon_blocks),
    "inFlight": (("swapgate", "inflight_cell"), _in_flight_blocks),
    "storeTables": (("swapgate", "tables_cell"), _tables_block),
}

# The in-flight cell answers from TWO readings of its own, in this order: whether a store is
# there at all, and then what it says. That is not a neighbour's answer borrowed -- it is the
# same ordered observation the record reader makes, where absence is settled by looking at the
# path before anything is opened.
#
# Without the first reading the cell had no way to say "established absent". The relay reports
# contents unavailable for a store that is missing and for one it cannot read, so an absent
# store read as unreadable, the gate returned UNESTABLISHED, and a first install on a clean
# host could never promote -- while the schema cell, which does look at the path, answered
# NO_STORE about the very same store.
INFLIGHT_READINGS = (("runtime_install", "store_presence"), ("scope", "relay"))


def _cell(answer, *, readable, detail, command=None, evidence=None):
    """One gate cell. 'readable' is whether the question was answered at all, and it is kept
    apart from the answer so a refusal and a negative never collapse into one value."""
    return {"answer": answer, "readable": readable, "detail": detail,
            "command": command, "evidence": evidence}


def daemon_cell(envelope):
    """Is a supervisor running? Decided by the relay's own service reading.

    scope.service_state already keeps the four answers apart and puts the invocation first: a
    command that did not run says nothing about the daemon. This only records which of them
    blocks.
    """
    state = scope.service_state(envelope)
    readable = state["state"] in (scope.RUNNING, scope.STOPPED)
    return _cell(state["state"], readable=readable, detail=state["detail"],
                 command=(envelope or {}).get("command"), evidence=state.get("running"))


def inflight_cell(envelope, presence=None):
    """How many attempts are still open, from this cell's own two readings.

    'presence' settles whether a store exists by looking at the path, before anything is
    opened. It has to come first, because the relay reports contents unavailable both for a
    store that is missing and for one it cannot read, and those are opposite answers here: an
    absent store has no open attempt, and an unreadable one has an unknown number.

    'envelope' is the relay's own doctor, which constructs no Store, so asking does not create
    the database the question is about.

    A caller that supplies no presence reading gets the old behaviour and says so by passing
    nothing: absence is then indistinguishable from unreadability and the cell refuses, which
    is the safe direction for a caller that did not look.
    """
    command = (envelope or {}).get("command")
    if presence is not None:
        if not presence.get("readable"):
            return _cell(reading.ACCESS_ERROR, readable=False,
                         command=presence.get("command"),
                         detail="whether a store exists at the resolved selection could not be"
                                " established: " + str(presence.get("detail")))
        if presence.get("present") is False:
            available = (((envelope or {}).get("payload") or {}).get("contents") or {})
            if available.get("available"):
                # Two readings of this cell's own question disagreeing is not an answer.
                return _cell(reading.UNREADABLE, readable=False, command=command,
                             detail="no store exists at " + str(presence.get("dbPath"))
                                    + " and the relay reports readable contents for it")
            return _cell(NO_ATTEMPTS, readable=True, command=presence.get("command"),
                         evidence=NO_ATTEMPTS,
                         detail="no store exists at " + str(presence.get("dbPath"))
                                + ", so no attempt can be open. That is established absence"
                                  " rather than a count nobody could read")
    if not isinstance(envelope, dict) or not envelope.get("ok"):
        detail = (envelope or {}).get("unreadable") or (envelope or {}).get("stderr")
        return _cell(reading.ACCESS_ERROR, readable=False, command=command,
                     detail="the relay could not be asked for its contents: "
                            + str(detail or "the command failed"))
    payload = envelope.get("payload")
    if not isinstance(payload, dict):
        return _cell(reading.UNREADABLE, readable=False, command=command,
                     detail="the relay answered with no readable payload")
    contents = payload.get("contents")
    if not isinstance(contents, dict) or not contents.get("available"):
        return _cell(reading.UNREADABLE, readable=False, command=command,
                     detail="the store's contents could not be read: "
                            + str((contents or {}).get("detail") or "no contents were reported"))
    open_attempts = contents.get("openAttempts")
    if not isinstance(open_attempts, int) or isinstance(open_attempts, bool):
        return _cell(reading.UNREADABLE, readable=False, command=command,
                     detail="the contents carry no integer openAttempts, found "
                            + type(open_attempts).__name__)
    return _cell(open_attempts, readable=True, command=command, evidence=open_attempts,
                 detail=("no attempt is open" if open_attempts == 0
                         else str(open_attempts) + " attempts are still open, so a handover is"
                              " in flight and the runtime under it is not replaced"))


def tables_cell(store_answer, candidate_answer):
    """Compare the schema the store holds with the schema the candidate declares.

    The comparison is over each table's CREATE statement and not merely its name. Names alone
    agree while a column, a constraint or a default differs, which is a schema difference the
    new runtime would apply on its first write-open, and it would have passed as agreement.

    Both sides are readings and either can fail. An absent store is established by looking at
    the path, never inferred from a failed open, because a permission failure and a locked
    database also fail to open and neither of them means nothing is there.
    """
    if not isinstance(store_answer, dict) or not isinstance(candidate_answer, dict):
        return _cell(reading.UNREADABLE, readable=False,
                     detail="a schema reading did not return an answer")
    if not candidate_answer.get("readable"):
        return _cell(reading.UNREADABLE, readable=False,
                     command=candidate_answer.get("command"),
                     detail="the candidate's declared schema could not be read: "
                            + str(candidate_answer.get("detail")))
    if not store_answer.get("readable"):
        return _cell(reading.UNREADABLE, readable=False, command=store_answer.get("command"),
                     detail="the store's schema could not be read: "
                            + str(store_answer.get("detail")))

    candidate = _schema(candidate_answer.get("tables"))
    if candidate is None:
        return _cell(reading.UNREADABLE, readable=False,
                     command=candidate_answer.get("command"),
                     detail="the candidate reported table names without their definitions, so"
                            " the schemas could not be compared on anything but names")
    if store_answer.get("present") is False:
        return _cell(NO_STORE, readable=True, command=store_answer.get("command"),
                     evidence={"dbPath": store_answer.get("dbPath")},
                     detail=("no store exists at the resolved selection, so there is nothing"
                             " whose schema could disagree. That is absence and not agreement"))
    held = _schema(store_answer.get("tables"))
    if held is None:
        return _cell(reading.UNREADABLE, readable=False, command=store_answer.get("command"),
                     detail="the store reported table names without their definitions, so the"
                            " schemas could not be compared on anything but names")
    lost = sorted(set(held) - set(candidate))
    added = sorted(set(candidate) - set(held))
    changed = sorted(name for name in set(held) & set(candidate)
                     if _normalised(held[name]) != _normalised(candidate[name]))
    evidence = {"dbPath": store_answer.get("dbPath"), "onlyInStore": lost,
                "onlyInCandidate": added, "definedDifferently": changed}
    backup = (" OPS-4.5 makes a schema change its own decision, in its own issue, with a copied"
              " backup of the whole state directory taken first, so this update refuses rather"
              " than letting the new runtime apply it on its first write-open.")
    if lost:
        return _cell(NARROWS, readable=True, command=store_answer.get("command"),
                     evidence=evidence,
                     detail=("the store holds tables this candidate does not declare, so"
                             " installing it would leave data no runtime can read: "
                             + ", ".join(lost)))
    if changed:
        return _cell(DIFFERS, readable=True, command=store_answer.get("command"),
                     evidence=evidence,
                     detail=("the store and the candidate define the same tables differently: "
                             + ", ".join(changed) + "." + backup))
    if added:
        return _cell(EXTENDS, readable=True, command=store_answer.get("command"),
                     evidence=evidence,
                     detail=("the candidate declares tables the store does not hold: "
                             + ", ".join(added) + ". Nothing in the store would be lost, and"
                             " that is why this is reported as its own answer rather than as a"
                             " downgrade." + backup))
    return _cell(AGREES, readable=True, command=store_answer.get("command"), evidence=evidence,
                 detail="the store and the candidate declare the same tables identically")


def _schema(tables):
    """Table name -> its CREATE statement, or None when the reading cannot answer this cell.

    A reading that carries only names is not a weaker version of this comparison, it is a
    different one: two name-only readings agree while a column differs, and reporting that as
    agreement is the defect the statement comparison exists to remove. So a reading without
    statements leaves the cell unanswered rather than answering it on less.
    """
    if not isinstance(tables, dict):
        return None
    return {str(name): value for name, value in tables.items()}


def _normalised(statement):
    """A CREATE statement compared with whitespace collapsed OUTSIDE quoted text.

    Not lowercased, and not touched inside quotes. SQLite stores the original CREATE text
    verbatim, so formatting differs between a store written long ago and a candidate's current
    DDL, and collapsing runs of whitespace is what makes that not a schema change. Going
    further is not free: lowercasing made DEFAULT 'A' and DEFAULT 'a' compare equal, and
    collapsing inside quotes made 'a  b' and 'a b' compare equal. Both are real schema
    differences reported as agreement, which is the one direction this cell must never fail in.

    What remains is stated rather than implied: two statements that mean the same thing and are
    written differently -- a reordered constraint, a changed identifier quoting style -- are
    reported as a difference. That refuses an update, which keeps the previous installation, and
    the refusal names the table so it can be settled deliberately.
    """
    if statement is None:
        return None
    text = str(statement)
    out, quote, space = [], None, False
    for char in text:
        if quote is not None:
            out.append(char)
            if char == quote:
                quote = None
            continue
        if char in "\'\"" + chr(96) + "[":
            quote = "]" if char == "[" else char
            out.append(char)
            space = False
            continue
        if char.isspace():
            space = True
            continue
        if space and out:
            out.append(" ")
        space = False
        out.append(char)
    return "".join(out)


def blocking(name, cell):
    """Whether this cell's established answer refuses the swap. None when it did not answer.

    The predicate comes off the declaration rather than being written again here, so a cell
    cannot be declared with one rule and judged by another.
    """
    if not cell.get("readable"):
        return None
    return bool(GATE_CELLS[name][1](cell))


def decide(cells):
    """The verdict, and which cells produced it.

    An established refusal is reported as a refusal even when another cell could not answer, so
    a caller sees the actionable blocker rather than only that something was unreadable. Both
    outcomes keep the existing installation.
    """
    blockers, unread = [], []
    for name in GATE_CELLS:
        cell = cells.get(name) or _cell(reading.ACCESS_ERROR, readable=False,
                                        detail="this cell was not read at all")
        refuses = blocking(name, cell)
        if refuses is None:
            unread.append(name + ": " + str(cell.get("detail")))
        elif refuses:
            blockers.append(name + ": " + str(cell.get("detail")))
    if blockers:
        verdict = BLOCKED
    elif unread:
        verdict = UNESTABLISHED
    else:
        verdict = ALLOWED
    return {
        "verdict": verdict,
        "cells": {name: cells.get(name) for name in GATE_CELLS},
        "blockedBy": blockers,
        "unreadable": unread,
        "note": (
            "OPS-4.4 replaces a runtime only with the daemon stopped and open attempts"
            " reconciled, and this command never starts or stops one: the service belongs to"
            " the scope operator (OPS-4.1). A cell that could not be read keeps the existing"
            " installation exactly as a refusal does."
        ),
    }
