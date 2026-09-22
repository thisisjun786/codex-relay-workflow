"""Management intent: what a coordinator publishes before the task it is about exists.

The relay's own tables cannot hold this. A relationship row needs a real child task id, and
relationship_id() refuses an empty one, so a record about a task nobody has created yet has nowhere
to live there. That is precisely the gap this module fills: the intent is declared first, the real
native task id is bound to it afterwards, and the binding is a create-once publication, so it is
atomic against a concurrent coordinator and idempotent against a replay.

The assignment state is DERIVED from which facts exist, never stored. A stored state would need
every writer to agree on transition rules, and every delayed writer would then be a regression risk.
Presence is monotonic, so a late fact can never move the state backwards and no interleaving needs a
special case.

Rules and precedence are fixed by skills/crw-run/references/hook-contract.md.
"""

import hashlib
from datetime import timedelta

import json
import os
from contextlib import contextmanager
from pathlib import Path

from .errors import RefusalReason, RegistrationError
from .marker import (
    assignment_dir,
    assignment_id,
    PUBLISHED,
    fact_digest,
    named,
    publish,
    read_assignment,
    same_identity,
    valid_assignment,
    valid_segment,
)
from .marker import listing
from .marker import list_assignments as _list_assignments

BINDING_WINDOW_MINUTES = 30

RELATIONSHIP_REGISTERED = "relationship_registered"
IDENTITY_BOUND = "identity_bound"
AMBIGUOUS_IDENTITY = "ambiguous_identity"
INTENT_EXPIRED = "intent_expired"
CREATION_UNKNOWN = "creation_unknown"
CREATION_ACCEPTED = "creation_accepted"
INTENT_DECLARED = "intent_declared"

ATTEMPT_OUTCOMES = ("accepted", "unknown", "failed")

# Everything an intent MEANS, which is everything except when it was said. A replay correcting any
# of these is a different declaration, and reporting it unchanged told a coordinator its fix had
# landed while the create-once file kept the original. A corrected dbPath is exactly the case that
# then leaves the guard reading a stale store.
INTENT_FIELDS = (
    "dispatchRequestIdHash", "issueKey", "workspace", "dbPath",
    "criteriaSource", "baselineRevision", "authorizedSettings",
)

# Exhaustive, not illustrative. An outcome outside this vocabulary is not a declaration at all:
# reading anything that is not ready_for_review as a release would let a typo buy one, which is
# the single way this design could fail open.
DISPOSITION_OUTCOMES = (
    "in_progress",
    "blocked_needs_input",
    "interrupted",
    "failed",
    "ready_for_review",
)

RELEASING_OUTCOMES = ("in_progress", "blocked_needs_input", "interrupted", "failed")

BOUND = "bound"
UNCHANGED = "unchanged"
CONFLICT = "conflict"

# Every identity slot a fact may carry. A present value here must be a string: an array, an object
# or an explicit null is malformed, and it is reported rather than read through, because a set built
# out of unhashable values ends the reader in a traceback and a traceback records nothing at all.
IDENTITY_FIELDS = {
    "intent": ("dispatchRequestIdHash", "dbPath"),
    "bound": ("sessionId", "taskId"),
    "relationship": ("relationshipId",),
    "attempts": ("taskId", "outcome"),
    "claims": ("sessionId", "dispatchRequestId"),
    "conflicts": ("attemptedSessionId", "attemptedTaskId"),
    "resolutions": ("chosenTaskId", "chosenSessionId"),
}

SINGLE = ("intent", "bound", "relationship")
LISTED = ("attempts", "claims", "conflicts", "resolutions")


def _moment(value):
    from datetime import datetime, timezone

    if value is None:
        return None
    try:
        parsed = datetime.fromisoformat(str(value).replace("Z", "+00:00"))
    except (TypeError, ValueError):
        return None
    # A naive timestamp is read as UTC rather than compared against an aware one, because mixing
    # the two raises rather than answering.
    return parsed if parsed.tzinfo else parsed.replace(tzinfo=timezone.utc)


# Public name for the one timestamp reader every module in this pair shares. Duplicating it would
# be the easiest way for two readers to disagree about what an undated record means.
moment = _moment


# ---------------------------------------------------------------- shape


def malformed(marker) -> str | None:
    """The first published record that is not the shape a fact must be, or None.

    Read straight through, one wrongly typed value ends the reader in a traceback, which records
    nothing at all: a single bad byte would switch detection off for the whole workspace. So the
    shape is checked before any derivation, and a record that cannot be read as a fact is reported
    rather than guessed at.
    """
    for key in SINGLE:
        if key in marker and not isinstance(marker[key], dict):
            return key
    for key in LISTED:
        if key not in marker:
            continue
        items = marker[key]
        if not isinstance(items, list):
            return key
        for item in items:
            if not isinstance(item, dict):
                return key
    for key in SINGLE + LISTED:
        if key not in marker:
            continue
        items = marker[key] if key in LISTED else [marker[key]]
        for item in items:
            for field in IDENTITY_FIELDS.get(key, ()):
                if field in item and not isinstance(item[field], str):
                    return key + "." + field
            # The adjudication list is READ to decide coverage, so validating the resolution and
            # not its nested list leaves the same silent failure one level down.
            if key in ("resolutions",) and "adjudicated" in item:
                entries = item["adjudicated"]
                if not isinstance(entries, list):
                    return key + ".adjudicated"
                for entry in entries:
                    if not isinstance(entry, dict):
                        return key + ".adjudicated"
    return None


def malformed_counters(counters) -> str | None:
    """Validate a hold budget only where one is actually weighed.

    A count that is not a count says the store it was counted from is wrong. Reported rather than
    read as zero, which would quietly hand back a full hold budget. Only an ABSENT optional counter
    defaults to zero; a present null or a negative one is corruption.
    """
    if counters is None:
        return None
    if not isinstance(counters, dict):
        return "counters"
    for key, value in counters.items():
        if isinstance(value, bool) or not isinstance(value, int) or value < 0:
            return "counters." + key
    return None


# ---------------------------------------------------------------- coverage


def _resolutions(marker):
    """Every adjudication on record. They accumulate; none replaces another."""
    return list(marker.get("resolutions") or [])


def covered(fact, resolutions) -> bool:
    """Is this exact fact adjudicated by one of these resolutions?

    Coverage is by identity AND digest together: the id selects the fact, the digest confirms the
    content equals what was recorded. Timestamps are never read, because publication order is not
    timestamp order and a backdated fact would otherwise be covered with nobody having reviewed it.
    A fact carrying no factId can never be covered, deliberately: unidentified evidence must not be
    able to disappear.
    """
    fact_id = fact.get("factId")
    if not named(fact_id):
        return False
    digest = fact_digest(fact)
    for resolution in resolutions:
        for entry in resolution.get("adjudicated") or []:
            if not isinstance(entry, dict):
                continue
            if entry.get("factId") == fact_id and entry.get("digest") == digest:
                return True
    return False


def claimant(claim):
    """The session a claim belongs to, taken from the path that authorised the write.

    A child may write inside claims/<own session>/, so the directory carries an identity the
    filesystem enforced, while the body carries one anybody holding that directory can type.
    Selecting on the body would let a later assignment's child name an earlier session and capture
    that session's Stop.

    The body is required too, not merely required to agree: the path proves who COULD have written
    the record, and only the body says the writer meant to claim this assignment. A claim naming
    nobody owns nothing, and it still competes, so refusing to read an identity out of it never
    deletes it as evidence.
    """
    parts = str(claim.get("factId") or "").split("/")
    if len(parts) != 3 or parts[0] != "claims" or parts[2] != "claim.json":
        return None
    owner = parts[1]
    if owner in (".", "..") or not named(owner):
        return None
    body = claim.get("sessionId")
    if not named(body) or body != owner:
        return None
    return owner


def _accepted_tasks(marker):
    return {
        attempt.get("taskId")
        for attempt in marker.get("attempts") or []
        if attempt.get("outcome") == "accepted" and named(attempt.get("taskId"))
    }


def _competing_facts(marker):
    bound = marker.get("bound") or {}
    facts = [
        attempt
        for attempt in marker.get("attempts") or []
        if attempt.get("outcome") == "accepted"
        and named(attempt.get("taskId"))
        and not same_identity(attempt.get("taskId"), bound.get("taskId"))
    ]
    # A claim competes unless it VERIFIABLY belongs to the bound session, and the owner comes from
    # the path rather than the body. same_identity rather than != is what keeps a claim naming
    # nobody, against a bind naming nobody, inside this list: compared with !=, two absences match
    # and the claim drops out, leaving the contest invisible and clearing the way for a verdict over
    # evidence nobody adjudicated.
    facts += [
        claim
        for claim in marker.get("claims") or []
        if not same_identity(claimant(claim), bound.get("sessionId"))
    ]
    return facts + list(marker.get("conflicts") or [])


def _ambiguity_resolved(marker) -> bool:
    """Pre-bind, an adjudication clears ambiguity only by agreeing and by covering everything.

    Two resolutions can each cover every ambiguous fact while naming different identities. That is a
    coordinator contradiction rather than a decision, so ambiguity survives it. Identity means the
    whole task and session pair; agreeing on the task alone is not agreement.
    """
    resolutions = [
        resolution
        for resolution in _resolutions(marker)
        if named(resolution.get("chosenTaskId")) and named(resolution.get("chosenSessionId"))
    ]
    if not resolutions:
        return False
    pairs = {(r.get("chosenTaskId"), r.get("chosenSessionId")) for r in resolutions}
    if len(pairs) != 1:
        return False
    chosen_task, chosen_session = next(iter(pairs))
    # A taskId on a failed or unknown attempt does not establish an accepted creation. Counting it
    # would let a resolution select an unconfirmed task and wrongly clear the ambiguity.
    sessions = {claimant(claim) for claim in marker.get("claims") or []}
    sessions.discard(None)
    if chosen_task not in _accepted_tasks(marker) or chosen_session not in sessions:
        return False
    facts = [
        attempt
        for attempt in marker.get("attempts") or []
        if attempt.get("outcome") == "accepted" and named(attempt.get("taskId"))
    ]
    facts += list(marker.get("claims") or [])
    return all(covered(fact, resolutions) for fact in facts)


def identity_contested(marker) -> bool:
    """A competing fact after a bind that no applicable resolution has adjudicated.

    Only a resolution naming the BOUND identity applies after a bind: one choosing a competitor must
    not authorise a verdict merely because it covers that competitor's evidence. Compared through
    same_identity, so a resolution naming nobody cannot become applicable to a bind naming nobody.
    """
    bound = marker.get("bound") or {}
    if not bound:
        return False
    applicable = [
        resolution
        for resolution in _resolutions(marker)
        if same_identity(resolution.get("chosenSessionId"), bound.get("sessionId"))
        and same_identity(resolution.get("chosenTaskId"), bound.get("taskId"))
    ]
    return any(not covered(fact, applicable) for fact in _competing_facts(marker))


# ---------------------------------------------------------------- state


def derive_assignment_state(marker, now=None) -> str:
    """The contract's precedence table, in its order.

    Expiry sits between ambiguity and the creation outcomes deliberately. An unresolved identity
    needs a decision from the coordinator whether or not the window has closed, while an intent that
    never got an acceptance must still be able to expire, which an acceptance-derived anchor could
    never reach.
    """
    intent = marker.get("intent") or {}
    attempts = marker.get("attempts") or []
    claims = marker.get("claims") or []
    if marker.get("bound"):
        # The identity, not the object: the same test the guard's decision path makes. A
        # relationship fact carrying a blank or missing relationshipId has registered nothing, and
        # deriving "registered" from its mere presence reports a registration to a coordinator that
        # names no relationship.
        registered = named((marker.get("relationship") or {}).get("relationshipId"))
        return RELATIONSHIP_REGISTERED if registered else IDENTITY_BOUND
    resolved = _ambiguity_resolved(marker)
    accepted = [a for a in attempts if a.get("outcome") == "accepted"]
    if not resolved and (len(_accepted_tasks(marker)) > 1 or len(claims) > 1):
        return AMBIGUOUS_IDENTITY
    # Anchored on declaredAt and on nothing else. Any acceptance-derived anchor can be moved later
    # by a fact that arrives later, which would let an already expired intent revive; an anchor that
    # never moves is the only way to keep expiry monotonic without storing a state nobody may
    # rewrite.
    anchor, seen = _moment(intent.get("declaredAt")), _moment(now)
    if anchor and seen and seen > anchor + timedelta(minutes=BINDING_WINDOW_MINUTES):
        return INTENT_EXPIRED
    if not resolved and not accepted and any(a.get("outcome") == "unknown" for a in attempts):
        return CREATION_UNKNOWN
    return CREATION_ACCEPTED if accepted else INTENT_DECLARED


def correlated(marker, session_id) -> bool:
    """Whether this session presented the dispatch request id the intent was declared with.

    Replayable evidence, not authentication: the intent stores only the hash, but the child's own
    claim necessarily stores the preimage, so on a single-uid host another session can copy it.
    Doing so confers nothing, because binding is the coordinator's; it produces at most a second
    claim, which is exactly the contested condition the coordinator must resolve.
    """
    claim = next(
        (c for c in (marker.get("claims") or []) if same_identity(claimant(c), session_id)), None
    )
    if not claim:
        return False
    presented = claim.get("dispatchRequestId")
    if not named(presented):
        return False
    digest = hashlib.sha256(presented.encode("utf-8")).hexdigest()
    return same_identity(digest, (marker.get("intent") or {}).get("dispatchRequestIdHash"))


# ---------------------------------------------------------------- selection


def select_assignment(root, workspace, session_id):
    """Which assignment under this workspace this session's turn is about.

    A claim is consulted BEFORE recency, and that order is the protection: a child stays with the
    assignment it claimed, so declaring a later assignment for the same path can neither release a
    still-running earlier child nor make it read as somebody else's.


    An assignment with no published intent at all is not selectable. It is a directory someone is
    still building, and skipping it leaves the reader on a valid earlier state rather than on
    nothing, which is the property every other create-once fact already has. An assignment whose
    intent IS published but is not a record, or cannot be read, stays selectable: dropping it would
    turn a corrupt marker into an unmanaged workspace, and an unmanaged workspace is released with
    nothing recorded at all, which is the one outcome a corrupt marker must never produce.

    Read problems stay with the candidate they came from. Merged across the workspace, one stale
    corrupt assignment would release every omission the CURRENT assignment could otherwise detect,
    because an unreadable store outranks everything: retained state nobody is using would switch
    detection off.
    """
    listed, readable = _list_assignments(root, workspace)
    if not readable:
        # The listing itself failed. Reported as unreadable rather than as no assignments, because
        # "unmanaged" releases the turn AND records nothing, so a permission or mount fault would
        # silently switch detection off for the whole workspace.
        return None, None, ["workspace"]
    candidates = []
    for directory in listed:
        facts, problems = read_assignment(directory)
        if "intent" not in facts and "intent" not in problems:
            continue
        candidates.append((directory, facts, problems))
    if not candidates:
        return None, None, []

    claimed = [
        candidate
        for candidate in candidates
        if any(
            same_identity(claimant(claim), session_id)
            for claim in (candidate[1].get("claims") or [])
            if isinstance(claim, dict)
        )
    ]
    directory, facts, problems = max(claimed or candidates, key=_recency)
    return directory, facts, problems


def _recency(candidate):
    """Newest declaration first, ties broken on the assignment id.

    Compared as instants rather than as strings. ISO 8601 sorts chronologically only when the
    offsets match, so two declarations written in different zones order lexically by their printed
    hour: 01:00+02:00 is 23:00 the previous day and would sort AFTER 00:30+00:00, picking the older
    assignment and changing every bind, claim, disposition and hold decision that follows.

    An undeclared or unparseable timestamp sorts below every real one rather than raising, and the
    assignment id still breaks the tie, so every reader of the same listing selects the same entry.
    """
    from datetime import datetime, timezone

    directory, facts, _problems = candidate
    intent_fact = facts.get("intent")
    declared = _moment(intent_fact.get("declaredAt")) if isinstance(intent_fact, dict) else None
    return (
        declared is not None,
        declared or datetime.min.replace(tzinfo=timezone.utc),
        directory.name,
    )


# ---------------------------------------------------------------- writing


def _assignment(value) -> str:
    """Refuse a malformed assignment id as a refusal, not as an internal error.

    It reaches these functions from a command line. A mistyped id would otherwise publish facts into
    a directory no reader can select, and one carrying a parent reference would publish them outside
    the workspace it claims to be under.
    """
    if not valid_assignment(value):
        raise RegistrationError(
            RefusalReason.UNKNOWN_GENERATION,
            "an assignment id is the hex sha256 of a dispatch request id, not " + repr(value),
        )
    return str(value)


def _identity(value, what: str) -> str:
    """Refuse an identity that cannot safely be a directory name, as a refusal with a reason.

    named() asks whether a record names something; this asks whether that name may be written as a
    path component. A session id of ".." names something perfectly well and would still redirect a
    create-once write out of its assignment.
    """
    if not valid_segment(value):
        raise RegistrationError(
            RefusalReason.UNBOUND_GENERATION,
            "a " + what + " becomes a directory name, so it cannot be empty, . or .., or contain a "
            "path separator: " + repr(value),
        )
    return str(value)


# The lock wait an open of the relay store may spend, and the only place it is decided.
#
# The hook contract gives one guard evaluation a five-second self-imposed wall clock. SQLite's
# timeout bounds lock waiting only, and everything after it - the marker walk, the head
# computation, the hold count, publishing the observation - is additional. Kept well under the
# budget so a database a writer is holding cannot spend the whole of it before the rest of the
# work has started.
#
# It sits beside the connect calls rather than in guard.py, where the reasoning used to sit with
# nothing reading it, because a bound declared away from the call that enforces it is a bound
# nobody is actually setting. Neither opener below takes a timeout parameter for the same
# reason: the readers - guard.lookup_receipt and dispatch_generation_state - and the one caller
# that takes a lock, registration_hold, all receive this value, and none can quietly choose
# another one while still using these functions. The hook's five seconds is the only stated
# budget among them and it is the tightest, so a bound that fits inside it is not too generous
# for a caller that has no stated budget at all. The hook reaches read_only_connection and
# nothing else: the hold is opened by registration, which runs in the coordinator rather than
# in a Stop evaluation.
SQLITE_TIMEOUT = 2.0


def read_only_connection(db_path):
    """Open the relay store for reading and never for creating. None when it cannot be opened.

    Lives here rather than in guard.py because registration needs it too, and guard imports this
    module. Mirrors store.read_only_rows: every failure becomes a None rather than an exception,
    because a caller that raises on a locked database records nothing.

    The path is absolutised BEFORE the URI is built. Path.as_uri() raises ValueError on a relative
    path, and that exception was being caught as "the store is unreadable", so a perfectly good
    relative --db-path made every readiness check fail open. as_uri() also percent-encodes, which is
    what keeps a path containing ? or # from being re-read as SQLite URI parameters.
    """
    import sqlite3

    try:
        resolved = Path(db_path).expanduser().absolute()
        uri = resolved.as_uri() + "?mode=ro"
    except (OSError, ValueError, TypeError, AttributeError):
        return None
    try:
        # Read from the module at call time rather than captured as a default argument, so the
        # constant above owns the bound every subsequent open uses instead of a value frozen
        # when this module was first imported.
        connection = sqlite3.connect(uri, uri=True, timeout=SQLITE_TIMEOUT)
    except (OSError, sqlite3.Error, ValueError, TypeError):
        return None
    connection.row_factory = sqlite3.Row
    return connection


@contextmanager
def registration_hold(db_path):
    """The relay's write lock, held across a check and the publication that depends on it.

    Registration asks which generation a dispatch opened and then writes a marker fact saying so.
    Read first and publish afterwards, those are two operations with nothing held between them,
    and an advance committing in the interval returns success over a generation the store has
    already moved past. Every advance is a relay WRITE - registry.open_generation runs under
    store.transaction(), which is BEGIN IMMEDIATE - so the store's write lock is the one lock the
    advance and the registration already share. Taking it here is what removes the interval:
    an advance either commits before the lock is granted, and the read inside then reports stale,
    or it waits until the fact has landed under the generation it names.

    Yields (connection, None) inside BEGIN IMMEDIATE, or (None, why) when the store cannot be
    opened or the lock cannot be taken within SQLITE_TIMEOUT. A pair rather than a bare None,
    because "the hold was not taken" is three different operator problems - no such store, a
    store this process may not write, and a store somebody else is writing - and a refusal that
    does not say which sends its reader to the wrong repair. Not an exception, for the reason
    read_only_connection answers the way it does: only the caller knows what an unavailable
    store means, and for registration it means refusing rather than publishing unheld.

    mode=rw and never rwc. An absent store must stay absent and be refused; Store() cannot be
    used here because opening one CREATES the database and runs the schema, which would turn
    "the relay has no such store" into a new empty one that answers absent to everything. The
    same shape is already how store.probe decides whether a process can write.
    """
    import sqlite3

    try:
        resolved = Path(db_path).expanduser().absolute()
        uri = resolved.as_uri() + "?mode=rw"
    except (OSError, ValueError, TypeError, AttributeError):
        yield None, "the relay store path " + repr(db_path) + " could not be read as a path"
        return
    try:
        # isolation_level=None so the BEGIN IMMEDIATE below IS the transaction. Left at the
        # default, sqlite3 opens an implicit deferred one on the first statement, and the hold
        # would be a read that excludes nobody.
        connection = sqlite3.connect(
            uri, uri=True, timeout=SQLITE_TIMEOUT, isolation_level=None
        )
    except (OSError, sqlite3.Error, ValueError, TypeError) as fault:
        yield None, "the relay store could not be opened for writing: " + str(fault)
        return
    connection.row_factory = sqlite3.Row
    try:
        connection.execute("BEGIN IMMEDIATE")
    except (OSError, sqlite3.Error) as fault:
        # A store somebody else is writing, or one this process may read but not write. Both are
        # answered the same way: the caller could not take the lock, so it cannot prove anything
        # it publishes is current.
        connection.close()
        yield None, "the relay store's write lock could not be taken: " + str(fault)
        return
    try:
        yield connection, None
    finally:
        # ROLLBACK and never COMMIT. This transaction exists to exclude other writers and writes
        # nothing of its own, so the way it ends must not be able to record anything.
        try:
            connection.execute("ROLLBACK")
        except sqlite3.Error:
            pass
        connection.close()


# What the relay says about the generation a dispatch request id opened.
DISPATCH_CURRENT = "current"
DISPATCH_STALE = "stale"
DISPATCH_ABSENT = "absent"


def _generation_state(connection, relationship_id: str, dispatch_request_id: str):
    """(state, generation) for one dispatch, read on a connection the caller already owns.

    Split out so the read-only reader and the writer holding the lock ask the same question of
    the same columns. A second copy of this comparison is exactly how the two paths would start
    to disagree about what stale means. (None, None) says the read itself failed, which is not
    the same answer as a dispatch the store does not have.
    """
    import sqlite3

    try:
        row = connection.execute(
            "SELECT g.execution_generation AS opened, r.execution_generation AS current"
            "  FROM generations g"
            "  JOIN relationships r ON r.relationship_id = g.relationship_id"
            " WHERE g.relationship_id = ? AND g.dispatch_request_id = ?",
            (relationship_id, dispatch_request_id),
        ).fetchone()
    except sqlite3.Error:
        return None, None
    if row is None:
        return DISPATCH_ABSENT, None
    if row["opened"] != row["current"]:
        return DISPATCH_STALE, row["current"]
    return DISPATCH_CURRENT, row["current"]


def dispatch_generation_state(db_path, relationship_id: str, dispatch_request_id: str):
    """Which generation did this dispatch open, and is it still the current one? (state, readable).

    The marker cannot answer any of it: an assignment id is the hash of a dispatch request id, so a
    caller pairing an unrelated relationship with the RIGHT dispatch id satisfies every check the
    filesystem can make. The relay's generations table is the only place that knows which
    relationship a dispatch actually opened, and it is unique on the pair.

    Finding the row is not enough. generations keeps one row per generation, so a relationship that
    has moved on still has the older dispatch's row, and asking only whether one exists proves that
    SOME generation used this dispatch rather than the live one. lookup_receipt computes the head
    over the relationship's current execution_generation, so a stale assignment registered that way
    would release turns on the strength of work belonging to a later generation. Stale is therefore
    a separate answer from absent, which is what the contract asks for: an old generation is one of
    the states that has to be distinguished rather than folded into "not registered".
    """
    connection = read_only_connection(db_path)
    if connection is None:
        return DISPATCH_ABSENT, False
    try:
        # The generation is not part of this answer; only a caller holding the lock has a use
        # for it, and binding a name nothing here reads would drop a readability answer on the
        # floor in the one shape that looks like it was considered.
        state = _generation_state(connection, relationship_id, dispatch_request_id)[0]
    finally:
        connection.close()
    # A failed read is reported exactly as an unopenable store is, because both mean the same
    # thing to every caller: the relay did not answer.
    if state is None:
        return DISPATCH_ABSENT, False
    return state, True


def dispatch_is_registered(db_path, relationship_id: str, dispatch_request_id: str):
    """Does the relay agree that this relationship carries this dispatch request id, right now?

    Returns (registered, readable): the detailed answer with stale and absent collapsed, for a
    caller that only needs to know whether to proceed.
    """
    state, readable = dispatch_generation_state(db_path, relationship_id, dispatch_request_id)
    return state == DISPATCH_CURRENT, readable


def _read_published(target):
    try:
        return json.loads(Path(target).read_text(encoding="utf-8")), True
    except (OSError, ValueError):
        return None, False


def _publish_or_compare(target, payload, fields, *, root=None) -> str:
    """Publish a single create-once fact, and say whether losing was a replay or a contradiction.

    Returning 'exists' for both would report a coordinator that published a DIFFERENT value exactly
    as it reports one that repeated itself, which is the difference between a retry that is safe to
    ignore and a contest somebody has to settle.
    """
    if publish(target, payload, root=root) == PUBLISHED:
        return PUBLISHED
    existing, readable = _read_published(target)
    if not readable or not isinstance(existing, dict):
        return CONFLICT
    return UNCHANGED if all(existing.get(f) == payload.get(f) for f in fields) else CONFLICT


def malformed_disposition(record) -> str | None:
    """A disposition that parsed but is not a record.

    Dispositions are read at the path the Stop identity derives rather than through the assignment
    walk, so the marker shape check never sees them. Without this a file holding a bare string is
    readable, classifies as no declaration at all, and a turn that published SOMETHING is held as if
    it had published nothing.
    """
    if record is None:
        return None
    if not isinstance(record, dict):
        return "disposition"
    for field in ("sessionId", "turnId", "outcome"):
        if field in record and not isinstance(record[field], str):
            return "disposition." + field
    return None



def _next_index(directory, kind):
    """The next free slot for a numbered fact, as (index, readable).

    readable travels with it because an unreadable directory must not answer 0. Answering 0 would
    name a slot a fact may already occupy, and the publication would then lose to EEXIST on every
    retry while the caller believed it was allocating a fresh one.
    """
    entries, readable = listing(Path(directory) / kind, "*.json")
    if not readable:
        return 0, False
    used = [
        int(path.stem)
        for path in entries
        if not path.name.startswith(".") and path.stem.isdigit()
    ]
    return (max(used) + 1 if used else 0), True


def _publish_numbered(directory, kind, payload, *, root) -> str:
    """Publish the next numbered fact, re-listing on every loss.

    A sequence number is allocated from a listing, and a listing is stale the moment another writer
    publishes. Losing the link is therefore ordinary and is answered by looking again, never by
    assuming the first guess was current.
    """
    for _ in range(64):
        index, readable = _next_index(directory, kind)
        if not readable:
            raise RegistrationError(
                RefusalReason.RELATIONSHIP_CONFLICT,
                "the " + kind + " directory cannot be read, so no slot can be allocated in it",
            )
        target = Path(directory) / kind / (str(index) + ".json")
        if publish(target, payload, root=root) == PUBLISHED:
            return kind + "/" + str(index)
    raise RegistrationError(
        RefusalReason.RELATIONSHIP_CONFLICT,
        "could not allocate a free " + kind + " slot after 64 attempts",
    )


def declare_intent(
    root,
    *,
    workspace,
    dispatch_request_id: str,
    issue_key: str,
    declared_at: str,
    criteria_source=None,
    baseline_revision=None,
    authorized_settings=None,
    db_path=None,
) -> dict:
    """Publish the intent. This is the fact that exists BEFORE the task does.

    The dispatch request id is stored only as its sha256. Storing it in the clear would make
    correlation meaningless, because any session able to read the directory could then present it.
    The child holds the preimage from its own dispatch and writes it into its claim.
    """
    if not named(dispatch_request_id):
        raise RegistrationError(
            RefusalReason.UNBOUND_GENERATION, "an intent needs an exact dispatch request id"
        )
    assignment = assignment_id(dispatch_request_id)
    directory = assignment_dir(root, workspace, _assignment(assignment))
    payload = {
        "dispatchRequestIdHash": assignment,
        "issueKey": issue_key,
        "workspace": str(Path(workspace).expanduser().resolve()),
        "criteriaSource": criteria_source,
        "baselineRevision": baseline_revision,
        "authorizedSettings": authorized_settings,
        "declaredAt": declared_at,
    }
    if db_path:
        # Where the relay store this assignment was registered against actually lives. The Stop
        # payload carries only cwd, and resolving the store from the environment instead is the one
        # slip that silently creates an EMPTY store, whose answer is "no receipt" and whose
        # consequence is a wrongly held child. The contract leaves the database location to the
        # operations contract; the coordinator is the party that knows it, so it records it here.
        payload["dbPath"] = str(db_path)
    outcome = _publish_or_compare(
        directory / "intent.json", payload, INTENT_FIELDS, root=root
    )
    return {
        "assignmentId": assignment,
        "assignmentDir": str(directory),
        "outcome": outcome,
        "declaredAt": declared_at,
    }


def record_attempt(root, *, workspace, assignment, outcome: str, at: str, task_id=None) -> dict:
    """Record what the creation call returned: accepted, unknown, or failed.

    unknown is the one that matters most. A creation whose response was lost is not a failure and
    not a success, and collapsing it into either is how a real task becomes invisible or a
    non-existent one becomes authoritative.
    """
    if outcome not in ATTEMPT_OUTCOMES:
        raise RegistrationError(
            RefusalReason.UNKNOWN_GENERATION,
            "an attempt outcome is one of " + ", ".join(ATTEMPT_OUTCOMES) + ", not " + repr(outcome),
        )
    directory = assignment_dir(root, workspace, _assignment(assignment))
    payload = {"outcome": outcome, "at": at}
    if task_id is not None:
        payload["taskId"] = task_id
    fact_id = _publish_numbered(directory, "attempts", payload, root=root)
    return {"assignmentId": assignment, "factId": fact_id, "outcome": outcome}


def bind(root, *, workspace, assignment, session_id: str, task_id: str, at: str) -> dict:
    """Bind the intent to the real native task id. Atomic, and idempotent on replay.

    Atomic because the publication is a link(): two coordinator processes racing produce exactly one
    winner and one EEXIST. Idempotent because the loser reads the winner and compares: the same
    identity is unchanged, and only a DIFFERENT identity is a conflict.

    A conflict is RECORDED rather than swallowed. Writing nothing would leave the contest invisible
    to every later reader, and the assignment has to stay contested until a resolution naming the
    bound identity covers that exact record.

    Binding is coordinator-only. A child claims and never binds, because only the party holding the
    creation receipt can tell "the task I created" from "a session that read an id".
    """
    if not (named(session_id) and named(task_id)):
        raise RegistrationError(
            RefusalReason.UNBOUND_GENERATION,
            "a bind needs an exact session id and task id; a record naming nothing binds nothing",
        )
    directory = assignment_dir(root, workspace, _assignment(assignment))
    payload = {"sessionId": session_id, "taskId": task_id, "at": at}
    if publish(directory / "bound.json", payload, root=root) == PUBLISHED:
        return {"assignmentId": assignment, "outcome": BOUND, "sessionId": session_id,
                "taskId": task_id}

    marker, unreadable = read_assignment(directory)
    winner = marker.get("bound")
    if not isinstance(winner, dict):
        raise RegistrationError(
            RefusalReason.UNBOUND_GENERATION,
            "a bind already exists here and cannot be read: " + ", ".join(unreadable or ["bound"]),
        )
    if same_identity(winner.get("sessionId"), session_id) and same_identity(
        winner.get("taskId"), task_id
    ):
        return {"assignmentId": assignment, "outcome": UNCHANGED, "sessionId": session_id,
                "taskId": task_id}
    conflict = _publish_numbered(
        directory,
        "conflicts",
        {
            "attemptedSessionId": session_id,
            "attemptedTaskId": task_id,
            "loserProcess": str(os.getpid()),
            "at": at,
        },
        root=root,
    )
    return {
        "assignmentId": assignment,
        "outcome": CONFLICT,
        "factId": conflict,
        "boundSessionId": winner.get("sessionId"),
        "boundTaskId": winner.get("taskId"),
    }


def register_relationship(
    root, *, workspace, assignment, relationship_id: str, dispatch_request_id: str, at: str,
    db_path=None,
) -> dict:
    """Publish which relay relationship this assignment was registered as.

    The dispatch request id is restated and checked against the assignment directory it would be
    written into. One assignment is one dispatch request, and the relay's generations table keys the
    same dispatch id to the same generation, so a relationship whose dispatch id does not hash to
    this directory belongs to a different assignment. Refusing here is what makes a receipt earned
    under another assignment's relationship unattributable rather than merely unlikely.

    The generation check and the publication happen under ONE hold on the relay's write lock.
    Checked first and published afterwards, they were two operations with nothing held between
    them, and an advance committing in that interval returned success over a generation the store
    had already moved past, leaving the marker naming it. Nothing corrects that later: guard reads
    only the relationship id out of this fact, so the stale-versus-current distinction the
    contract requires is drawn here or nowhere. Under the hold an advance either commits before
    the read, which then reports stale and publishes nothing, or waits until the fact has landed.

    The fact records the generation it was registered under. A registration is a statement about
    one generation rather than about "now", and a reader that has both the fact and the store can
    say which.
    """
    if not named(relationship_id):
        raise RegistrationError(
            RefusalReason.UNREGISTERED_RELATIONSHIP, "a registration needs an exact relationship id"
        )
    if assignment_id(dispatch_request_id) != assignment:
        raise RegistrationError(
            RefusalReason.RELATIONSHIP_CONFLICT,
            "relationship " + relationship_id + " was dispatched under a different request id, so "
            "it does not belong to assignment " + str(assignment),
        )
    # Resolved before the lock is taken. A malformed assignment is a refusal that needs no hold,
    # and holding the relay's only write slot while validating an argument would make every
    # caller's mistake somebody else's wait.
    directory = assignment_dir(root, workspace, _assignment(assignment))
    with registration_hold(db_path) as (held, unavailable):
        if held is None:
            raise RegistrationError(
                RefusalReason.UNREGISTERED_RELATIONSHIP,
                "the relay store could not be held for this registration, so it cannot be "
                "confirmed that relationship " + relationship_id + " is still this assignment's "
                "at the moment the registration lands (" + str(unavailable) + "); it is refused "
                "rather than published on the caller's word",
            )
        # The hash check above only proves the CALLER restated the right dispatch id. Pairing an
        # unrelated relationship with that id passes it, and its receipts would then satisfy this
        # assignment's guard. Only the relay knows which relationship a dispatch actually opened.
        state, generation = _generation_state(held, relationship_id, dispatch_request_id)
        if state is None:
            raise RegistrationError(
                RefusalReason.UNREGISTERED_RELATIONSHIP,
                "the relay store could not be read, so it cannot be confirmed that relationship "
                + relationship_id + " belongs to this assignment; registration is refused rather "
                "than taken on the caller's word",
            )
        if state == DISPATCH_STALE:
            # Separated from absent on purpose. The relay HAS this dispatch, under a generation
            # the relationship has since moved past, and registering it anyway would let this
            # assignment's guard answer with receipts earned by the live generation. An old
            # generation is one of the states the contract requires to be told apart, not a
            # variant of not being registered.
            raise RegistrationError(
                RefusalReason.STALE_GENERATION,
                "this assignment's dispatch request id opened an earlier generation of "
                "relationship " + relationship_id + ", which has since advanced, so registering "
                "it would attribute the current generation's receipts to a superseded assignment",
            )
        if state == DISPATCH_ABSENT:
            raise RegistrationError(
                RefusalReason.RELATIONSHIP_CONFLICT,
                "the relay has no generation of relationship " + relationship_id + " opened under "
                "this assignment's dispatch request id, so it is not this assignment's "
                "relationship",
            )
        # Inside the hold, deliberately. This publication is the whole reason the lock is held:
        # moving it out again would restore the interval this function exists to close.
        outcome = _publish_or_compare(
            directory / "relationship.json",
            {"relationshipId": relationship_id, "executionGeneration": generation, "at": at},
            # Compared on the relationship id alone. A record published before the generation was
            # recorded carries no executionGeneration, and comparing a field it never had would
            # report an ordinary replay as a contradiction somebody has to settle.
            ("relationshipId",),
            root=root,
        )
    return {
        "assignmentId": assignment, "relationshipId": relationship_id,
        "executionGeneration": generation, "outcome": outcome,
    }


def publish_resolution(
    root, *, workspace, assignment, chosen_task_id, chosen_session_id, reason, at, adjudicated
) -> dict:
    """Adjudicate named evidence. A resolution naming nothing adjudicates nothing.

    Resolutions accumulate: a later adjudication is another file, never a rewrite of an earlier one,
    so each keeps the scope it was published with and coverage does not depend on their order.
    """
    directory = assignment_dir(root, workspace, _assignment(assignment))
    entries = [
        {"factId": entry["factId"], "digest": entry["digest"]} for entry in (adjudicated or [])
    ]
    fact_id = _publish_numbered(
        directory,
        "resolutions",
        {
            "chosenTaskId": chosen_task_id,
            "chosenSessionId": chosen_session_id,
            "reason": reason,
            "at": at,
            "adjudicated": entries,
        },
        root=root,
    )
    return {"assignmentId": assignment, "factId": fact_id, "adjudicated": len(entries)}


def publish_claim(
    root, *, workspace, assignment, session_id: str, dispatch_request_id: str, first_turn_id, at
) -> dict:
    """The child's own assertion that it is this assignment's session.

    Written into a directory the child owns, and the body must name that same session: a fact that
    contradicts its own location is not one to act on. Refused here rather than silently published,
    because a claim whose body disagrees with its path owns nothing and would only ever read as a
    competitor.

    The dispatch request id is checked against the directory for the same reason register_relationship
    checks it: an assignment id IS the hash of a dispatch request id, so the two are derivable from
    each other and a claim naming a different dispatch belongs to a different assignment. A claim is
    what the guard requires before it will hold a bound session, so a fact nobody correlated must not
    be able to satisfy it.
    """
    session_id = _identity(session_id, "session id")
    if assignment_id(dispatch_request_id) != assignment:
        raise RegistrationError(
            RefusalReason.RELATIONSHIP_CONFLICT,
            "this claim names dispatch request id " + str(dispatch_request_id) + ", which does not "
            "hash to assignment " + str(assignment) + ", so it claims a different assignment",
        )
    directory = assignment_dir(root, workspace, _assignment(assignment))
    outcome = _publish_or_compare(
        directory / "claims" / session_id / "claim.json",
        {
            "dispatchRequestId": dispatch_request_id,
            "sessionId": session_id,
            "firstTurnId": first_turn_id,
            "at": at,
        },
        ("dispatchRequestId", "sessionId"),
        root=root,
    )
    return {"assignmentId": assignment, "sessionId": session_id, "outcome": outcome}


def publish_disposition(
    root, *, workspace, assignment, session_id: str, turn_id: str, outcome: str, at: str
) -> dict:
    """What this turn declared, recorded by the session itself at the path its Stop identity derives.

    The outcome vocabulary is exhaustive rather than illustrative, and it is checked here so that a
    typo cannot buy a release later: anything outside it is not a declaration at all.
    """
    session_id = _identity(session_id, "session id")
    turn_id = _identity(turn_id, "turn id")
    if outcome not in DISPOSITION_OUTCOMES:
        raise RegistrationError(
            RefusalReason.OUTCOME_INCONSISTENT,
            "a disposition outcome is one of " + ", ".join(DISPOSITION_OUTCOMES) + ", not "
            + repr(outcome),
        )
    directory = assignment_dir(root, workspace, _assignment(assignment))
    published = _publish_or_compare(
        directory / "dispositions" / session_id / (turn_id + ".json"),
        {"sessionId": session_id, "turnId": turn_id, "outcome": outcome, "at": at},
        ("sessionId", "turnId", "outcome"),
        root=root,
    )
    return {
        "assignmentId": assignment,
        "sessionId": session_id,
        "turnId": turn_id,
        "outcome": outcome,
        "published": published,
    }
