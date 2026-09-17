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
    return cell.get("answer") != 0


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


def inflight_cell(envelope):
    """How many attempts are still open, from the relay's own doctor.

    doctor constructs no Store, so asking does not create the database the question is about.
    Its 'contents' reports availability separately from the counts for the same reason this
    module does: a store it could not read yields no counts rather than zero.
    """
    command = (envelope or {}).get("command")
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
    if store_answer.get("present") is False:
        return _cell(NO_STORE, readable=True, command=store_answer.get("command"),
                     evidence={"dbPath": store_answer.get("dbPath")},
                     detail=("no store exists at the resolved selection, so there is nothing"
                             " whose schema could disagree. That is absence and not agreement"))
    held = _schema(store_answer.get("tables"))
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
    """Table name -> its CREATE statement, from either side's reading.

    A reading that reports only names still compares, on names alone; the missing statements
    simply cannot disagree. That keeps an older probe readable instead of unreadable, without
    letting it claim more than it saw.
    """
    if isinstance(tables, dict):
        return {str(name): value for name, value in tables.items()}
    return {str(name): None for name in (tables or [])}


def _normalised(statement):
    """A CREATE statement compared on its tokens, so whitespace is not a schema change."""
    if statement is None:
        return None
    return " ".join(str(statement).split()).lower()


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

