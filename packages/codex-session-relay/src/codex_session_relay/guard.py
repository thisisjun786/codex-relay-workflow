"""The Stop decision: whether a managed turn may end, judged against records rather than prose.

Three producers have to agree before a completion authorises parent verification, and none of them
is a sentence and none is a turn boundary: the child declares a disposition for THIS turn, the relay
holds a receipt naming this session, this turn and this assignment's relationship, under the
generation that assignment registered, and the host reports that the turn ended normally.
last_assistant_message is deliberately never read.

This module is the only one here that touches the relay database, and it touches it read-only,
through a mode=ro URI. That is not a stylistic choice. Store.__init__ opens the file O_RDWR, runs the
whole schema script and writes schema_meta on every open, so a guard built on it would CREATE a
store at a misresolved path; a fresh empty store answers "no receipt", and "no receipt" after a
declared readiness is a hold. One configuration slip would then wrongly hold a child on every Stop.

Nothing here writes a receipt, records a verdict, or marks anything verified. When the evidence is
missing, the answer is to say so and hold within bounds, never to manufacture the missing evidence.

Rules, states and bounds are fixed by skills/crw-run/references/hook-contract.md.
"""

import json
import sqlite3
from datetime import timedelta
from pathlib import Path

from . import intent as intents
from .currency import AMBIGUOUS, head_revision
from .errors import ScopeError
from .manifest import (
    Entry,
    revision_hash,
    verify_against_disk_detailed,
    verify_frozen_detailed,
)
from .marker import (
    DIRECTORIES,
    PUBLISHED,
    listing,
    named,
    publish,
    read_assignment,
    read_disposition,
    same_identity,
    valid_segment,
)

MAX_HOLDS_PER_TURN = 1
MAX_HOLDS_PER_GENERATION = 2
MAX_HOLDS_PER_SESSION_WINDOW = 3
SESSION_WINDOW_MINUTES = 60

# The only three classifications a hold may ever be issued for.
OMISSIONS = ("managed_unregistered", "receipt_missing", "undeclared_turn_end")

# The receipt evidence meaning the registration and the store disagree about which generation this
# assignment is for. Named once because observe_state reads it back to answer with the recovery
# instead of repeating an instruction the child cannot act on.
GENERATION_MISMATCH = "registration_generation_mismatch"
# The live generation exists but a DIFFERENT dispatch opened it. An ordinal is a counter, not an
# identity: a store rebuilt or restored under a surviving marker can stand on generation 1 again
# under another dispatch entirely, and the ordinal comparison alone reads that as agreement.
GENERATION_DISPATCH_MISMATCH = "generation_dispatch_mismatch"
# The relationship reports a current generation the generations table has no row for. Every
# legitimate advance writes both in one transaction - registry.register for the first generation
# and open_generation_in for every later one - so this is a store that cannot say which dispatch
# it is executing, which is missing evidence rather than an answer.
GENERATION_ABSENT = "generation_absent"
# The three answers that mean the live generation is not this assignment's. They hold the same
# way and are told apart because they are repaired in different places.
GENERATION_EVIDENCE = (GENERATION_MISMATCH, GENERATION_DISPATCH_MISMATCH, GENERATION_ABSENT)

BLOCK = "block"
RELEASE = "release"

OBSERVE, HOLD = "observe", "hold"

# The create-once name that claims a turn's single hold, alongside the numbered observations in the
# same hook-owned directory. Named rather than numbered so the two never collide.
HOLD_FILE = "hold.json"

# Named so a defect in this code cannot masquerade as a data problem in somebody's marker.
FAULTED = "guard_faulted"

# The outside-world stages of one evaluation. "Every place that can fail" is not enumerable; these
# are, and they are what the inventory tests iterate. A stage added later belongs in this tuple.
EVALUATION_STAGES = (
    "workspace_listing",
    "assignment_facts",
    "disposition",
    "receipt_store",
    "hold_history",
    "observation_record",
)

READY = "ready_for_review"


# What the deliverable a receipt was computed over looks like NOW. A receipt names a revision, and
# the revision is the bytes: existence is not validity.
DELIVERABLE_CURRENT = "current"
DELIVERABLE_CHANGED = "changed"
DELIVERABLE_UNVERIFIABLE = "unverifiable"


class StoreNotSelected(Exception):
    """The receipt store was never selected, and the caller will not stand behind the fallback.

    Raised by the resolver a caller may pass as default_db_path, never by this module's own logic.
    evaluate() re-raises it ahead of the envelope that turns everything else into guard_faulted,
    because the two are repaired in different places: guard_faulted says the defect is in this
    code, and this says the operator has to settle which store holds the receipts. It carries
    whatever the caller needs to explain itself, and nothing here reads that.
    """

    def __init__(self, detail=None):
        super().__init__("the receipt store was never selected")
        self.detail = detail


# ---------------------------------------------------------------- the receipt


def deliverable_state(payload, manifest_ref, roots):
    """Do the artifacts still hash to the revision this receipt claims? (state, binding, detail).

    The rule is not invented here. ReceiptIntake._verify_bytes already decides what verified means
    at intake, and this re-applies it with the roots the relationship authorised, so a receipt is
    judged at Stop time by the same standard it was admitted under. A frozen copy is consulted only
    when the live bytes disagree, exactly as intake does, because once the working files move on the
    frozen copy is what the receipt was about.

    A read that could not happen is never reported as a revision that changed. The two have
    different repairs - a permission or a vanished mount on one side, a child that must re-emit on
    the other - and collapsing them would hold a child over a directory we could not open.

    No lease is requested. A lease costs a concurrent writer real time and this runs inside a
    five-second hook budget, so the guard verifies at the best-effort tier: it confirms the bytes
    match when it looks, and it does not pretend to prevent them changing afterwards.
    """
    try:
        records = (payload or {}).get("manifest")
        if not isinstance(records, list) or not records:
            # Intake refuses a reviewable receipt that carries no manifest, so a head event without
            # one did not come through it. Reported rather than waved through: nothing here can say
            # which bytes it stood for.
            return DELIVERABLE_CHANGED, None, "the stored receipt carries no manifest to verify"
        entries = [Entry.from_record(record) for record in records]
        claimed = (payload or {}).get("revisionHash")
        recomputed = revision_hash(entries)
        if not claimed:
            # A receipt that names no revision cannot be verified against one. Skipping the
            # comparison when the digest is absent let matching live files release a receipt that
            # stands for nothing, which is the same mistake as reading a blank identity as one.
            return DELIVERABLE_CHANGED, None, "the stored receipt names no revision"
        if recomputed != claimed:
            return (
                DELIVERABLE_CHANGED,
                None,
                "the stored manifest hashes to " + recomputed + " but the receipt claims "
                + str(claimed),
            )
        problems, _bindings, unreachable_live = verify_against_disk_detailed(entries, roots)
        if not problems:
            return DELIVERABLE_CURRENT, "live", None
        if manifest_ref:
            _digest, frozen, unreachable = verify_frozen_detailed(manifest_ref, entries)
            if not frozen:
                return DELIVERABLE_CURRENT, "frozen", None
            if unreachable:
                # The frozen copy is this receipt's own fallback and we could not read it, so
                # whether the deliverable still stands is not something this evaluation knows.
                # verify_frozen catches its access errors and returns them as problem strings, so
                # they never become exceptions here - which is why the boundary that converts
                # exceptions cannot see them and the distinction has to be asked for by name.
                return (
                    DELIVERABLE_UNVERIFIABLE,
                    None,
                    "; ".join(unreachable[:3]),
                )
            problems = problems + frozen
        if unreachable_live:
            # Checked after the frozen fallback, because a frozen copy that verifies answers for a
            # receipt whose live files we could not read. Reached only when nothing rescued it, and
            # then the honest answer is that the comparison did not happen. The same shape as the
            # frozen case one line up: scope turns an unreadable component into a refusal, and
            # verify_against_disk catches it and returns it as text, so no exception ever arrives.
            return DELIVERABLE_UNVERIFIABLE, None, "; ".join(unreachable_live[:3])
        return DELIVERABLE_CHANGED, None, "; ".join(problems[:3])
    except (OSError, ScopeError) as error:
        # Could not look. Never folded into "changed".
        return DELIVERABLE_UNVERIFIABLE, None, type(error).__name__ + ": " + str(error)
    except (ValueError, KeyError, TypeError, AttributeError) as error:
        # Looked, and the stored record is not the shape a receipt is. That is a readable answer
        # about the record rather than a failure to read it.
        return DELIVERABLE_CHANGED, None, type(error).__name__ + ": " + str(error)


def lookup_receipt(db_path, *, relationship_id, session_id, turn_id, execution_generation=None,
                   dispatch_request_id=None):
    """The reviewable receipt this turn produced, and whether it stands at the current head.

    The head is computed FIRST and the matching event is then fetched by its id. Selecting a row by
    session and turn and comparing it to the head afterwards picks an arbitrary row when one turn
    emitted more than one revision, so a child that superseded its own work could have the older
    event chosen and its perfectly current receipt reported as superseded, which is a hold.

    Three identities make a receipt this declaration's rather than some other one's, and all three
    must be NAMED: the relationship the assignment published, the session, and the turn. A receipt
    from an earlier turn or another session can still stand at the head while the turn in front of
    us produced nothing.

    execution_generation is the generation the assignment REGISTERED, read off the marker's
    create-once relationship fact and never derived here. The head below is computed over the
    relationship's CURRENT generation, so without it a registration that was legitimately current
    when it landed is answered by whatever the live generation produced afterwards - and the three
    identities do not catch that, because a correction generation opened against the same dispatch
    turn admits that turn as its own anchor, so the later generation's receipt names this session
    and this turn. Weighed inside the snapshot below, so the generation compared IS the one the
    head was computed over. None means the fact carries no stamp, having been published before the
    field existed, and an absent stamp is compared against nothing.

    dispatch_request_id is the preimage THIS session claimed, and it answers what the ordinal
    cannot. A generation number is a counter scoped to one relationship, so a store rebuilt or
    restored under a surviving marker can stand on the same ordinal the fact records while a
    different dispatch opened it - the stamp then agrees with a generation this assignment never
    registered. The generations table is the only place that knows which dispatch opened which
    generation, and it is read on this same connection, so the mapping weighed is the mapping the
    head is computed under. None means this session claimed no dispatch, which observe_state
    answers as an unclaimed or uncorrelated marker before any receipt is weighed.

    stage deliberately admits staged. A child emits from inside its own turn, so the host reports
    that turn as inProgress and the event is stored staged; it is finalized only after the daemon
    observes the turn ending, which is AFTER this hook has run. Requiring final here would read
    every honest readiness as a missing receipt and hold it. What staged must never do is survive a
    turn that failed or was interrupted, and head_revision already excludes a suppressed event.

    Returns (receipt, readable). readable False means we could not look, which is never the same
    answer as there is nothing there.

    db_path may be a resolver called with no arguments, and it is called HERE rather than by the
    caller, after the identity gate below. That gate answers without a store at all, so a caller
    that resolved first would pay for - and, if its resolver refuses an unselected store, refuse
    over - a database this lookup was never going to open. One rule, in the one place that knows
    whether the read happens.
    """
    if not (named(relationship_id) and named(session_id) and named(turn_id)):
        return None, True
    if callable(db_path):
        db_path = db_path()
    if not db_path:
        # We were never told where the store is, so we cannot look. Reported as unreadable rather
        # than as an absent receipt, because the difference between those two answers is a hold.
        return None, False
    # The one place this module opens the database, and every read below - including the ones
    # currency.head_revision issues on the way - runs on this connection. The lock wait it may
    # spend is decided inside read_only_connection by intent.SQLITE_TIMEOUT, which is where the
    # relation to the hook contract's five-second budget is written down. Nothing here declares a
    # bound of its own: a declaration beside the caller is one that nothing enforces.
    connection = intents.read_only_connection(db_path)
    if connection is None:
        return None, False
    base = {
        "relationshipId": relationship_id,
        "sessionId": session_id,
        "turnId": turn_id,
        "atCurrentHead": False,
    }
    try:
        # One snapshot for all three reads. Python's sqlite3 starts no transaction for SELECTs, so
        # a concurrent writer could replace the head between computing it and fetching the event it
        # named, and the guard would then judge against a revision that is no longer current.
        connection.execute("BEGIN DEFERRED")
        relationship = connection.execute(
            "SELECT status, superseded_by, execution_generation, artifact_roots FROM relationships"
            " WHERE relationship_id = ?",
            (relationship_id,),
        ).fetchone()
        if relationship is None:
            # A readable answer: the assignment named a relationship this store does not have.
            return dict(base, evidence="relationship_absent"), True
        if relationship["status"] != "active" or relationship["superseded_by"]:
            return dict(base, evidence="relationship_not_active"), True
        if (
            execution_generation is not None
            and execution_generation != relationship["execution_generation"]
        ):
            # A registration is a statement about ONE generation, and this is not the generation
            # the head below would be computed over. Answered BEFORE the head is computed, so the
            # cause reported is the registration rather than whatever the live generation happens
            # to hold: an advance carrying no reviewable revision of its own would otherwise read
            # as no_reviewable_revision and send the child to emit a receipt that could not
            # satisfy this assignment however many times it emitted one.
            #
            # A mismatch and not "superseded": a store restored or rebuilt under a surviving
            # marker can name a generation EARLIER than the fact does, and calling that
            # supersession would tell an operator the relationship moved on when it moved back.
            #
            # This compares the ORDINAL only. An ordinal is a counter, so agreement here is not
            # yet identity; the dispatch mapping below is what establishes that the generation
            # the ordinal names is this assignment's. Kept as two answers rather than one,
            # because they are reached through different rows and repaired differently.
            return dict(
                base,
                evidence=GENERATION_MISMATCH,
                detail=(
                    "the assignment registered generation " + str(execution_generation)
                    + " and the relationship now stands on generation "
                    + str(relationship["execution_generation"])
                ),
            ), True
        if dispatch_request_id is not None:
            # Asked of the CURRENT generation, which is the one the head below is computed over.
            # Asking about the generation the fact names would answer whether this dispatch ever
            # opened something, which is the registration's question and not this one.
            opened = connection.execute(
                "SELECT dispatch_request_id FROM generations"
                " WHERE relationship_id = ? AND execution_generation = ?",
                (relationship_id, relationship["execution_generation"]),
            ).fetchone()
            if opened is None:
                return dict(
                    base,
                    evidence=GENERATION_ABSENT,
                    detail=(
                        "the relationship reports generation "
                        + str(relationship["execution_generation"])
                        + " and the store holds no record of which dispatch opened it"
                    ),
                ), True
            if not same_identity(opened["dispatch_request_id"], dispatch_request_id):
                # The ids themselves stay out of the detail. The claim preimage is the child's to
                # present, and what an operator needs is which generation is foreign, not a value
                # they can copy out of a hold record.
                return dict(
                    base,
                    evidence=GENERATION_DISPATCH_MISMATCH,
                    detail=(
                        "the relationship stands on generation "
                        + str(relationship["execution_generation"])
                        + ", which a different dispatch request opened"
                    ),
                ), True
        head = head_revision(connection, relationship_id, relationship["execution_generation"])
        if head.get("evidence") in AMBIGUOUS:
            # Not "no receipt". The lineage this generation declares does not identify a single
            # tip, so nothing is at the head and the coordinator has a lineage problem to settle.
            # The hold decision is the same; the cause is worth reporting as its own.
            return dict(base, evidence="head_" + str(head.get("evidence"))), True
        if not head.get("eventId"):
            return dict(base, evidence="no_reviewable_revision"), True
        row = connection.execute(
            "SELECT event_id, stage, revision_hash, turn_thread_id, turn_id, producer,"
            " receipt, manifest_ref"
            "  FROM events WHERE event_id = ?",
            (head["eventId"],),
        ).fetchone()
    except sqlite3.Error:
        return None, False
    finally:
        try:
            connection.execute("ROLLBACK")
        except sqlite3.Error:
            pass
        connection.close()

    if row is None:
        return dict(base, evidence="no_reviewable_revision"), True
    if row["producer"] != "child" or row["stage"] not in ("staged", "final"):
        return dict(base, evidence="head_is_not_a_child_receipt"), True
    if not (
        same_identity(row["turn_thread_id"], session_id)
        and same_identity(row["turn_id"], turn_id)
    ):
        return dict(base, evidence="head_belongs_to_another_turn"), True

    # Standing at the head is a statement about the LINEAGE. It says nothing about whether the
    # artifacts the receipt was computed over are still those artifacts, and a receipt that outlived
    # its deliverable would release a turn whose work no longer matches anything anyone reviewed.
    # Hashed after the snapshot is closed: artifact I/O must never hold BEGIN DEFERRED open.
    try:
        roots = json.loads(relationship["artifact_roots"] or "[]")
        payload = json.loads(row["receipt"])
    except (ValueError, TypeError) as error:
        return dict(
            base,
            evidence="stored_receipt_unreadable",
            detail=type(error).__name__ + ": " + str(error),
            eventId=row["event_id"],
        ), True
    state, binding, detail = deliverable_state(payload, row["manifest_ref"], roots)
    if state == DELIVERABLE_UNVERIFIABLE:
        return dict(base, evidence="deliverable_unverifiable", detail=detail), False
    if state == DELIVERABLE_CHANGED:
        return dict(
            base,
            evidence="artifacts_changed_since_receipt",
            detail=detail,
            eventId=row["event_id"],
            revisionHash=row["revision_hash"],
        ), True
    return dict(
        base,
        atCurrentHead=True,
        evidence="at_head",
        eventId=row["event_id"],
        revisionHash=row["revision_hash"],
        stage=row["stage"],
        deliverableBinding=binding,
    ), True


def receipt_matches(receipt, stop, marker) -> bool:
    """Does this receipt belong to the declaration being judged?

    Pointing at the current head is not enough, and neither is naming the session and the turn. The
    same session can be the child of more than one assignment at a time, so the receipt must also
    name the relationship THIS assignment published. An assignment that has published none cannot
    have a receipt attributed to it at all.
    """
    if not receipt or not receipt.get("atCurrentHead"):
        return False
    if not (
        same_identity(receipt.get("sessionId"), stop.get("session_id"))
        and same_identity(receipt.get("turnId"), stop.get("turn_id"))
    ):
        return False
    registered = (marker or {}).get("relationship") or {}
    return same_identity(receipt.get("relationshipId"), registered.get("relationshipId"))


# ---------------------------------------------------------------- hold budget


def _held_records(hook_root, session_id=None):
    """Hold reservations under one hook tree, as (session, turn, at). Returns (records, bad, readable).

    Every file here IS a hold: reserve_hold creates one exactly when a hold is issued, so counting
    them needs no flag to interpret and no writer has to keep a flag honest.

    session_id narrows the walk BEFORE anything is parsed. That ordering is the point: the rolling
    window is a bound on one session, so another session's malformed record must not be able to
    reach it. Reporting corruption first let a foreign bad record turn this session's counters into
    corruption and release an otherwise holdable omission.
    """
    records = []
    root = Path(hook_root)
    sessions, readable = listing(root, only=DIRECTORIES)
    if not readable:
        return records, None, False
    for session_dir in sessions:
        if session_id is not None and session_dir.name != session_id:
            continue
        turns, readable = listing(session_dir, only=DIRECTORIES)
        if not readable:
            return records, None, False
        for turn_dir in turns:
            files, readable = listing(turn_dir, HOLD_FILE)
            if not readable:
                return records, None, False
            for path in files:
                try:
                    record = json.loads(path.read_text(encoding="utf-8"))
                except (OSError, ValueError):
                    return records, "hook/" + session_dir.name, True
                if not isinstance(record, dict):
                    return records, "hook/" + session_dir.name, True
                records.append((session_dir.name, turn_dir.name, record.get("at")))
    return records, None, True


def hold_counters(directory, *, session_id, turn_id, now, workspace_root=None):
    """Count this hook's own holds. Returns (counters, malformed, unreadable).

    Counted rather than stored, so nothing has to be kept consistent across writers.

    The two scopes are different on purpose. holdsThisGeneration is counted over one assignment,
    because one assignment directory is one dispatch request id and the relay keys the same dispatch
    id to the same generation. holdsThisSessionWindow is counted over every assignment under the
    workspace, because it bounds the SESSION over a rolling hour.

    A listing failure is REPORTED rather than absorbed by narrowing the scope. Falling back to the
    selected assignment made sibling holds disappear and silently renewed the window budget, which
    is the one direction this accounting must never fail in: every other failure here loses a
    result, and that one loses a bound.
    """
    counters = {"holdsThisTurn": 0, "holdsThisGeneration": 0, "holdsThisSessionWindow": 0}
    scoped, problem, readable = _held_records(Path(directory) / "hook")
    if not readable:
        return counters, None, "hook"
    if problem:
        return counters, problem, None
    for record_session, record_turn, _at in scoped:
        counters["holdsThisGeneration"] += 1
        if same_identity(record_session, session_id) and same_identity(record_turn, turn_id):
            counters["holdsThisTurn"] += 1

    if workspace_root is None:
        assignments, readable = [Path(directory)], True
    else:
        # Directories only. A stray regular file beside the assignment directories is not an
        # assignment, and handing one to _held_records made scanning it raise NotADirectoryError,
        # which reported the budget as uncountable and released a holdable omission. An ordinary
        # file must not be able to switch holding off for the workspace.
        assignments, readable = listing(Path(workspace_root), only=DIRECTORIES)
    if not readable:
        return counters, None, "workspace"
    horizon = intents.moment(now)
    for assignment in assignments:
        found, problem, readable = _held_records(assignment / "hook", session_id=session_id)
        if not readable:
            return counters, None, "hook"
        if problem:
            return counters, problem, None
        for _record_session, _record_turn, at in found:
            when = intents.moment(at)
            if horizon is None or when is None or when > horizon - timedelta(
                minutes=SESSION_WINDOW_MINUTES
            ):
                # An unreadable or absent timestamp counts INSIDE the window. Failing the other way
                # would let an undated record renew the budget, and the budget exists to stop a loop.
                counters["holdsThisSessionWindow"] += 1
    return counters, None, None


def reserve_hold(directory, *, session_id, turn_id, at, mode, root=None) -> bool:
    """Claim this turn's single hold, atomically. True when this evaluation owns it.

    A create-once file, so two evaluations racing on one Stop produce exactly one winner. Reading
    the counters and then deciding cannot do this: both readers see an unspent budget and both
    block, which exceeds the very bound they were checking.

    Deliberately NOT the observation record. hook/<session>/<turn>/<seq>.json is sequence-numbered
    precisely so a second observation of one turn is never lost, and turning it into a mutual
    exclusion token would reintroduce the loss it exists to prevent. This is a separate file whose
    only job is to be unwinnable twice.

    It is also what the bounds count, because a reservation exists exactly when a hold was issued,
    which an observation carrying held true only mirrors afterwards.
    """
    if not (valid_segment(session_id) and valid_segment(turn_id)):
        return False
    # Inside hook/<session>/<turn>/, which is the subtree the contract grants this process. A
    # separate top-level tree would be refused by the sandbox that makes holding permissible at
    # all, and the refusal would surface as guard_faulted - releasing every omission in exactly
    # the configuration hold mode exists for. The name is not a sequence number, so the numbered
    # observation records and this reservation cannot collide.
    target = Path(directory) / "hook" / session_id / turn_id / HOLD_FILE
    return publish(
        target, {"sessionId": session_id, "turnId": turn_id, "at": at, "mode": mode}, root=root
    ) == PUBLISHED


def release_hold(directory, session_id, turn_id) -> None:
    """Give back a reservation this evaluation made but could not use.

    The reservation is taken before the observation is published, so a recording failure would
    otherwise leave the turn's only hold spent with nothing blocked and nothing recorded. It is
    this call's own file, so removing it is the same principle as discarding an unpublished temp.
    Losing it to a crash instead costs one hold, which the generation and window bounds still catch.
    """
    if not (valid_segment(session_id) and valid_segment(turn_id)):
        return
    try:
        (Path(directory) / "hook" / session_id / turn_id / HOLD_FILE).unlink()
    except OSError:
        pass


def _next_hook_seq(directory, session_id, turn_id):
    """The next observation slot for this turn, as (index, readable).

    An unreadable directory must not answer 0: the publication would lose to EEXIST on every retry
    while this caller believed it was allocating a fresh sequence number.
    """
    entries, readable = listing(Path(directory) / "hook" / session_id / turn_id, "*.json")
    if not readable:
        return 0, False
    used = [
        int(path.stem)
        for path in entries
        if not path.name.startswith(".") and path.stem.isdigit()
    ]
    return (max(used) + 1 if used else 0), True


def record_observation(directory, record, *, root=None) -> str | None:
    """Publish this observation. One turn can be observed more than once, so the sequence is part
    of the identity: a single create-once file per turn would let the first observation consume the
    only name available and silently lose every later one.

    Failing to publish RAISES. Returning None for it would hand the caller the same answer it gets
    when there was nothing to name, and the caller's release path - which gives back the hold this
    unrecorded decision reserved - would never run, leaving a hold in flight that no record
    explains. intent._publish_numbered is the same loop over a (index, readable) helper and already
    raises on both of these paths; this is the one that had drifted.
    """
    session_id, turn_id = record.get("sessionId"), record.get("turnId")
    if not (valid_segment(session_id) and valid_segment(turn_id)):
        # The observation is built from the delivered Stop payload, so these reach a path from
        # outside. An identity that cannot be a directory name records nothing here rather than
        # publishing into a directory the assignment does not own.
        #
        # Returns rather than raises, unlike the two failures below. This one is already classified
        # upstream as a malformed stop identity, and raising it would relabel a data problem as a
        # defect in the guard - the exact conflation guard_faulted exists to prevent.
        return None
    for _ in range(64):
        index, readable = _next_hook_seq(directory, session_id, turn_id)
        if not readable:
            raise OSError(
                "the hook observation directory for " + session_id + "/" + turn_id
                + " cannot be read, so no slot can be allocated in it"
            )
        target = Path(directory) / "hook" / session_id / turn_id / (str(index) + ".json")
        if publish(target, record, root=root) == PUBLISHED:
            return "hook/" + session_id + "/" + turn_id + "/" + str(index)
    raise OSError(
        "could not publish an observation for " + session_id + "/" + turn_id
        + " after 64 attempts; every allocated slot was taken by another writer first"
    )


# ---------------------------------------------------------------- classification


def classify_declaration(observation) -> str:
    """What this turn declared, independent of who is bound.

    Used both for the bound session's decision and for the record kept during the pre-bind window,
    so a coordinator that binds later can fold an undeclared first turn it would otherwise miss.
    """
    stop = observation.get("stop_input") or {}
    disposition = observation.get("disposition")
    outcome = None
    if (
        isinstance(disposition, dict)
        and same_identity(disposition.get("turnId"), stop.get("turn_id"))
        and same_identity(disposition.get("sessionId"), stop.get("session_id"))
    ):
        outcome = disposition.get("outcome")
    if outcome in intents.RELEASING_OUTCOMES:
        return "declared_" + outcome
    if outcome == READY:
        if receipt_matches(observation.get("receipt"), stop, observation.get("marker")):
            return "declared_ready_receipted"
        return "receipt_missing"
    return "undeclared_turn_end"


def observe_state(observation):
    """Classify this turn. Observation only: no bounds, no holding, no side effects."""
    stop = observation.get("stop_input") or {}
    unreadable = observation.get("store_unreadable") or []

    # An unreadable store outranks an absent marker: "I could not look" must never be reported as
    # "there is nothing there". This is also where a database error lands, and it releases, because
    # the alternative is holding a child over a store WE could not read.
    if unreadable:
        return "state_unreadable", "Cannot read " + ", ".join(sorted(set(unreadable))) + "."
    marker = observation.get("marker")
    if marker is not None or observation.get("malformed"):
        # The disposition is read at the path this Stop identity derives rather than through the
        # assignment walk, so the marker shape check never sees it. Checked here with the same
        # precedence, because a published record that is not a record must release and be recorded,
        # and treating it as no declaration at all would HOLD the turn instead.
        malformed = observation.get("malformed") or intents.malformed(marker or {})
        if malformed:
            # Looked, and what is there is not a fact. Same rule one step further along: a record
            # that cannot be read as a fact must not be reported as an absent one either.
            return "marker_malformed", (
                "The published " + malformed + " is not the shape a fact must be. Repair the "
                "marker; a record that cannot be read is reported, never guessed at."
            )
    if not marker:
        return "unmanaged", "No assignment directory for this workspace."

    session = stop.get("session_id")
    bound = marker.get("bound") or None
    if not bound:
        # Binding is coordinator-only, so an unbound session is never the managed child yet.
        # Occupying the workspace is not identity.
        if not intents.correlated(marker, session, observation.get("assignment")):
            return "dispatch_uncorrelated", "This session presented no matching dispatch request id."
        return "correlated_unbound", (
            "Correlated to the intent but not yet bound by the coordinator. Released; the turn's "
            "observation is recorded for the coordinator to fold once the bind lands."
        )
    if not named(bound.get("sessionId")):
        return "bound_identity_unnamed", (
            "The bind record names no session, so nothing can be shown to be the bound child. "
            "Repair the marker; a turn is never held against an identity nobody published."
        )
    if not same_identity(bound.get("sessionId"), session):
        return "marker_claimed_by_other_session", "This session is not the bound child."

    declaration = classify_declaration(observation)
    # Nothing is missing. These win over every other condition, including a missing registration,
    # because holding a turn that was declared waiting, interrupted or failed is the one trade this
    # policy refuses to make.
    if declaration.startswith("declared_"):
        return declaration, "The child declared this turn."
    # Nothing is held against a session that has not asserted it is the child. The coordinator holds
    # the creation receipt, so it can bind before the child publishes its claim, and that race must
    # not prompt a session which never claimed this assignment.
    #
    # Asked through the correlation rule rather than through the claimant alone, which is the check
    # the pre-bind path above has always made and this one did not. A claim is what authorises a
    # hold, and an assignment id IS the hash of a dispatch request id, so a claim naming a different
    # dispatch is evidence about a different assignment; counting it satisfied the hold precondition
    # with a fact nobody correlated. Both windows now read the same predicate, so they cannot drift.
    problem = intents.correlation_problem(marker, session, observation.get("assignment"))
    if problem == intents.CLAIM_ABSENT:
        return "marker_unclaimed", (
            "The coordinator bound this session, but it has not claimed this assignment. Released "
            "and recorded; a hold needs the child's own claim, not only the coordinator's bind."
        )
    if problem:
        # Answered apart from marker_unclaimed because the two clear differently. An unclaimed
        # marker is the ordinary bind-before-claim race and ends the moment the child publishes;
        # this one never ends by itself, because claims/<session>/claim.json is create-once and
        # _publish_or_compare answers CONFLICT on a different dispatch, so the correct claim can
        # no longer be published at that path. Reporting it as unclaimed would tell the coordinator
        # to wait for a fact that cannot arrive. Which artifact is wrong travels in the record.
        return "claim_uncorrelated", (
            "This session is bound but its claim does not correlate with this assignment ("
            + problem + "). Released and recorded; every fact this reads is create-once, so it "
            "does not clear itself and no resolution consumed here will: correlation reads the "
            "claim and the intent, never the adjudications. Recovery is a new assignment, "
            "declared for a fresh dispatch request id."
        )
    if not named((marker.get("relationship") or {}).get("relationshipId")):
        # The identity, not the object. A relationship fact that exists but names nothing has
        # registered nothing, and truthiness on the record read it as registered: lookup_receipt
        # then refuses the unnamed id immediately, the turn lands on receipt_missing, and the child
        # is told to emit a receipt that no receipt could satisfy. This is the same check the bind
        # record gets a few lines up, which was made and this one was not.
        return "managed_unregistered", (
            "This workspace is managed but its relationship is not registered. Register it, or "
            "record a disposition explaining why it cannot be."
        )
    evidence = (observation.get("receipt") or {}).get("evidence")
    if declaration == "receipt_missing" and evidence in GENERATION_EVIDENCE:
        detail = str((observation.get("receipt") or {}).get("detail"))
        if evidence == GENERATION_ABSENT:
            # A different repair from the other two, so a different sentence. Nothing the
            # coordinator declares fixes a store that cannot say what it is executing.
            return "receipt_missing", (
                "The relay's store holds no record of the generation it reports as current for "
                "this relationship: " + detail + ". Nothing can be attributed to this assignment "
                "while the store cannot say which dispatch opened the generation it is on, and "
                "no receipt this session emits changes that. The relay's store is what needs "
                "repair."
            )
        # The same omission, holding the same way; not the same instruction. The ordinary
        # receipt_missing reason tells the child to emit the receipt, and here no receipt it
        # emits can ever satisfy this assignment: relationship.json is create-once, so the
        # assignment cannot be re-registered onto the live generation. A hold a child cannot act
        # on is a hold it cannot clear, which is why this names the recovery instead.
        return "receipt_missing", (
            "This assignment's registration does not name the generation the relay is on: "
            + detail
            + ". No receipt this session emits can satisfy it, because the registration fact is "
            "create-once and cannot be republished onto the live generation, and a receipt "
            "earned there belongs to work this assignment never registered. Recovery is a new "
            "assignment, declared for a fresh dispatch request id."
        )
    if declaration == "receipt_missing":
        return "receipt_missing", (
            "Readiness is declared but no receipt stands at the current head revision for this "
            "session and turn. Emit the receipt over the actual artifacts."
        )
    return "undeclared_turn_end", (
        "No usable turn disposition was recorded for this turn. Record in_progress, "
        "blocked_needs_input, interrupted, failed, or ready_for_review with a receipt."
    )


def decide(observation, *, counters=None, mode=OBSERVE):
    """Apply the hold bounds. The observation is recorded either way.

    Detection never depends on the hold succeeding, on this hook's position in the sequence, or on
    why a continuation is already running.
    """
    stop = observation.get("stop_input") or {}
    state, reason = observe_state(observation)

    if state in OMISSIONS:
        malformed = intents.malformed_counters(counters)
        if malformed:
            state = "marker_malformed"
            reason = "Invalid persisted " + malformed + "; repair the hold budget record."

    def out(decision, final_state, final_reason):
        if decision == BLOCK and mode != HOLD:
            # Observe-only. Holding depends on a child being unable to forge the facts this decision
            # reads, and that premise comes from the sandbox grant, not from the decision logic. An
            # implementation that cannot establish the grant and holds anyway is outside the
            # contract, so the default classifies and records and never holds.
            decision = RELEASE
            final_reason = (
                final_reason + " Observe-only: recorded without holding, because per-session write "
                "isolation was not asserted for this run."
            )
        record = {
            "observation": state,
            "turnId": stop.get("turn_id"),
            "sessionId": stop.get("session_id"),
            "decisionState": final_state,
            "held": decision == BLOCK,
            "mode": mode,
            "at": observation.get("now"),
        }
        result = {
            "decision": decision,
            "state": final_state,
            "observation": state,
            "reason": final_reason,
            "record": record,
        }
        # Derivation follows the marker being READABLE, never one state's name. Asking whether the
        # state is marker_malformed describes a symptom and misses an unreadable store that also
        # carries a malformed fact, which would derive a summary from records that are not records.
        marker = observation.get("marker") or {}
        if (
            observation.get("store_unreadable")
            or observation.get("malformed")
            or (marker and intents.malformed(marker))
        ):
            marker = {}
        if state == "correlated_unbound":
            # The pre-bind window is not blind: keep what the turn would have been judged as, so the
            # coordinator can fold it once the bind lands.
            record["pendingObservation"] = classify_declaration(observation)
        if state == "claim_uncorrelated" and marker:
            # Which artifact is wrong, kept separate from the decision, exactly as receipt evidence
            # is below. A withheld preimage, a preimage belonging to another assignment, an intent
            # that published no hash and an intent published under another assignment all release
            # the same way and are all settled differently, so the class has to survive in the
            # record rather than in prose.
            record["claimEvidence"] = intents.correlation_problem(
                marker, stop.get("session_id"), observation.get("assignment")
            )
            result["claimEvidence"] = record["claimEvidence"]
            # What the turn WOULD have been judged as, kept for the same reason the pre-bind window
            # keeps it: this answer replaces a classification the coordinator still needs. Without
            # it an uncorrelated readiness that no receipt answers records only that the claim was
            # wrong, and the missing receipt - a different problem, on the same turn - is gone.
            record["pendingObservation"] = classify_declaration(observation)
        if marker:
            record["assignmentState"] = intents.derive_assignment_state(
                marker, observation.get("now")
            )
            # Recorded both ways: the coordinator needs to distinguish "checked and not contested"
            # from "nobody looked".
            record["identityContested"] = intents.identity_contested(marker)
        receipt = observation.get("receipt")
        if isinstance(receipt, dict) and receipt.get("evidence"):
            # Why there is no usable receipt, kept separate from the decision. An ambiguous lineage,
            # a superseded revision and an inactive relationship all hold the same way and are all
            # different problems to fix.
            record["receiptEvidence"] = receipt["evidence"]
            result["receiptEvidence"] = receipt["evidence"]
            if receipt.get("detail"):
                # The label says which kind of problem; this says which artifact and how. A hold a
                # child cannot act on is a hold it cannot clear.
                record["receiptDetail"] = receipt["detail"]
                result["receiptDetail"] = receipt["detail"]
        result["hook_output"] = (
            {"decision": "block", "reason": final_reason, "continue": True}
            if decision == BLOCK
            else {}
        )
        return result

    if state not in OMISSIONS:
        return out(RELEASE, state, reason)

    # Exhaustion is evaluated first, and deliberately. Reaching the generation or window bound is a
    # terminal statement about the assignment, while "this turn already held" and "a continuation is
    # running" only say not right now. If the transient guard answered first, an assignment that had
    # stopped converging would keep reporting itself as merely busy.
    counts = counters or {}
    if (
        int(counts.get("holdsThisGeneration") or 0) >= MAX_HOLDS_PER_GENERATION
        or int(counts.get("holdsThisSessionWindow") or 0) >= MAX_HOLDS_PER_SESSION_WINDOW
    ):
        return out(
            RELEASE,
            "unresolved_handoff",
            "Hold bound reached; recording an unresolved handoff instead of holding again.",
        )
    if int(counts.get("holdsThisTurn") or 0) >= MAX_HOLDS_PER_TURN:
        return out(RELEASE, "hold_in_flight", "This turn already took its one hold.")
    if stop.get("stop_hook_active"):
        return out(
            RELEASE,
            "hold_in_flight",
            "A continuation is already running for this turn; the omission is recorded.",
        )
    return out(BLOCK, state, reason)


# ---------------------------------------------------------------- the whole answer


def evaluate(root, stop_input, *, now, mode=OBSERVE, db_path=None, default_db_path=None,
             record=True) -> dict:
    """Decide one Stop, and never end without saying something.

    This is the classification boundary. Everything below it answers with a label rather than an
    exception, and anything that still escapes is turned into a named guard_faulted result carrying
    the exception type and message. It is deliberately NOT folded into state_unreadable: that would
    let a real defect in this code masquerade as a data problem in somebody's marker, and the two
    need different repairs. A detector that dies detects nothing and leaves no trace it ran, which
    is the one outcome worse than a wrong answer.

    StoreNotSelected is the one thing that travels out of here unclassified, because it is not a
    failure of this evaluation: the caller's resolver declined to name a receipt store, and the
    caller is the only party that can settle which one it should be.
    """
    stop = stop_input or {}
    downgraded = None
    if mode == HOLD and not record:
        # A hold IS a side effect: it is reserved as a create-once file, counted against three
        # bounds, and released by the record that explains it. An evaluation asked not to record
        # cannot do any of that, so reserving one here would spend a turn's budget on a decision
        # nothing durable accounts for. Answered as the observation it actually is, and the
        # downgrade is named in the verdict rather than applied quietly.
        mode, downgraded = OBSERVE, "hold_requires_a_recorded_observation"
    reached = {"directory": None}
    try:
        verdict = _evaluate(
            root, stop, now=now, mode=mode, db_path=db_path,
            default_db_path=default_db_path, record=record, reached=reached,
        )
    except StoreNotSelected:
        raise
    except Exception as error:
        verdict = _faulted(stop, now, mode, error)
        if record and reached["directory"] is not None:
            try:
                verdict["recordedAs"] = record_observation(
                    reached["directory"], verdict["record"], root=root
                )
            except Exception:
                # Recording is the last thing that can fail, and failing it must not re-raise: the
                # caller still gets a classified release rather than a traceback.
                verdict["recordedAs"] = None
    if downgraded:
        verdict["modeDowngraded"] = downgraded
        verdict["record"]["modeDowngraded"] = downgraded
        verdict["reason"] = verdict["reason"] + (
            " Hold mode was not applied: this evaluation was asked not to record, and a hold that"
            " publishes no observation cannot be released, counted against the bounds, or audited."
        )
    # Always present, so a consumer never has to tell an absent key from a recorded one. Null says
    # this evaluation had nowhere to write: no assignment directory was selected, the Stop identity
    # could not be a directory name, or recording was not asked for. Absence would make the reader
    # infer that from a missing field, which is the same guessing this module exists to remove.
    verdict.setdefault("recordedAs", None)
    return verdict


def _faulted(stop, now, mode, error) -> dict:
    detail = type(error).__name__ + ": " + str(error)
    record = {
        "observation": FAULTED,
        "turnId": stop.get("turn_id"),
        "sessionId": stop.get("session_id"),
        "decisionState": FAULTED,
        "held": False,
        "mode": mode,
        "fault": detail,
        "at": now,
    }
    return {
        "decision": RELEASE,
        "state": FAULTED,
        "observation": FAULTED,
        "reason": "The guard could not finish this evaluation: " + detail
                  + ". Released and recorded; this is a defect in the guard rather than in the"
                    " marker, and it is reported as one.",
        "record": record,
        "fault": detail,
        "assignmentId": None,
        "counters": {},
        "hook_output": {},
    }


def _evaluate(root, stop, *, now, mode, db_path, default_db_path, record, reached) -> dict:
    """Gather the records this Stop is judged against, decide, and publish the observation.

    The receipt is read only when a readiness was actually declared. A turn that declared itself
    waiting or interrupted is released on its own declaration, and going to the database anyway
    would let an unreadable store overwrite a perfectly good answer the child already gave.

    Which store to read has three sources in a deliberate order: db_path, when the caller named one
    explicitly; then the dbPath the COORDINATOR recorded in intent.json, because it is the party
    that registered the relationship and knows where its store lives; then default_db_path, the
    caller's own resolution, which is only a guess about somebody else's choice. That third source
    may be a path or a resolver called with no arguments; lookup_receipt calls it, after the
    identity gate that can answer without any store. That is how a caller gets to refuse a guess at
    the moment it would actually be read, rather than before this Stop was read.
    """
    workspace = stop.get("cwd")
    session_id, turn_id = stop.get("session_id"), stop.get("turn_id")

    directory = marker_facts = None
    unreadable = []
    if workspace:
        directory, marker_facts, unreadable = intents.select_assignment(root, workspace, session_id)
        unreadable = list(unreadable or [])
        reached["directory"] = directory

    disposition = receipt = malformed_label = None
    counters = {}
    if directory is not None:
        if not (valid_segment(session_id) and valid_segment(turn_id)):
            # The delivered identity cannot be a directory name, so nothing can be recorded for it
            # and no hold can be reserved against it. Reported as the malformed record it is,
            # rather than being folded into a hold that is not in flight.
            malformed_label = "stop_identity"
        disposition, readable = read_disposition(directory, session_id, turn_id)
        if not readable:
            unreadable.append("disposition")
        malformed_label = malformed_label or intents.malformed_disposition(disposition)
        declared = (
            disposition.get("outcome")
            if not malformed_label
            and isinstance(disposition, dict)
            and same_identity(disposition.get("turnId"), turn_id)
            and same_identity(disposition.get("sessionId"), session_id)
            else None
        )
        registered = (marker_facts or {}).get("relationship")
        # A marker whose published facts are not facts does not get to send us to a database. The
        # read would fail on its corrupt value and report the STORE as unreadable, which points the
        # repair at the wrong thing: the marker is what needs fixing, and it is already knowable.
        shape = intents.malformed(marker_facts or {})
        if declared == READY and isinstance(registered, dict) and not shape:
            recorded = (
                marker_facts.get("intent", {}).get("dbPath")
                if isinstance(marker_facts.get("intent"), dict)
                else None
            )
            receipt, readable = lookup_receipt(
                db_path or recorded or default_db_path,
                relationship_id=registered.get("relationshipId"),
                session_id=session_id,
                turn_id=turn_id,
                execution_generation=registered.get("executionGeneration"),
                dispatch_request_id=intents.claimed_dispatch(
                    marker_facts or {}, session_id,
                    directory.name if directory is not None else None,
                ),
            )
            if not readable:
                # Named apart from the store. A database we could not open and a deliverable we
                # could not hash are both "could not look", and they are repaired in different
                # places, so the recorded reason points at the right one.
                unreadable.append(
                    "the receipt's artifacts"
                    if (receipt or {}).get("evidence") == "deliverable_unverifiable"
                    else "receipts"
                )
    observation = {
        "stop_input": stop,
        "marker": marker_facts,
        # The assignment this turn was judged under, which is the directory name and therefore the
        # hash itself. Correlation needs it: the preimage and the intent hash can be made to agree
        # with each other by anything that can write the marker, and only the directory says which
        # dispatch the coordinator actually opened.
        "assignment": directory.name if directory is not None else None,
        "disposition": disposition,
        "receipt": receipt,
        "store_unreadable": unreadable,
        "malformed": malformed_label,
        "now": now,
    }
    # Classified once with no budget, so a declaration that releases on its own never pays for the
    # hold history at all. Only an omission is weighed against the bounds, and only then is the
    # budget read - an unreadable holds/ tree must not be able to hide a declared interrupted turn.
    verdict = decide(observation, counters={}, mode=mode)
    if verdict["observation"] in OMISSIONS and directory is not None:
        counters, corrupt, unreadable_history = hold_counters(
            directory,
            session_id=session_id,
            turn_id=turn_id,
            now=now,
            workspace_root=directory.parent,
        )
        if unreadable_history:
            # A budget that could not be counted is not an empty budget. Reported rather than
            # absorbed, because the alternative is holding on a bound nobody actually checked.
            unreadable.append(unreadable_history)
            observation["store_unreadable"] = unreadable
        if corrupt:
            counters = {"holdsThisTurn": None}
        verdict = decide(observation, counters=counters, mode=mode)
    if verdict["decision"] == BLOCK and directory is not None:
        # The counters were read before the decision, so two evaluations racing on one Stop can
        # both see an unspent budget. The reservation is the atomic part: exactly one of them wins
        # the create-once file, and the loser re-decides with this turn's budget already spent,
        # which is the same hold_in_flight it would have reached had it read the counters later.
        if not reserve_hold(
            directory, session_id=session_id, turn_id=turn_id, at=now, mode=mode, root=root
        ):
            verdict = decide(
                observation,
                counters={**counters, "holdsThisTurn": MAX_HOLDS_PER_TURN},
                mode=mode,
            )
    verdict["assignmentId"] = directory.name if directory is not None else None
    verdict["counters"] = counters
    if record and directory is not None:
        try:
            verdict["recordedAs"] = record_observation(directory, verdict["record"], root=root)
        except BaseException:
            if verdict["decision"] == BLOCK:
                release_hold(directory, session_id, turn_id)
            raise
    return verdict
