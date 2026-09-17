"""The Stop decision: whether a managed turn may end, judged against records rather than prose.

Three producers have to agree before a completion authorises parent verification, and none of them
is a sentence and none is a turn boundary: the child declares a disposition for THIS turn, the relay
holds a receipt naming this session, this turn and this assignment's relationship, and the host
reports that the turn ended normally. last_assistant_message is deliberately never read.

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
from .marker import (
    named,
    valid_segment,
    publish,
    read_assignment,
    read_disposition,
    same_identity,
)

MAX_HOLDS_PER_TURN = 1
MAX_HOLDS_PER_GENERATION = 2
MAX_HOLDS_PER_SESSION_WINDOW = 3
SESSION_WINDOW_MINUTES = 60

# The hook contract gives this evaluation a five-second self-imposed wall clock. SQLite's timeout
# bounds lock waiting only, and everything after it - the marker walk, the head computation, the
# hold count, publishing the observation - is additional. Kept well under the budget so a database
# a writer is holding cannot spend the whole of it before the rest of the work has started.
SQLITE_TIMEOUT = 2.0

# The only three classifications a hold may ever be issued for.
OMISSIONS = ("managed_unregistered", "receipt_missing", "undeclared_turn_end")

BLOCK = "block"
RELEASE = "release"

OBSERVE, HOLD = "observe", "hold"

READY = "ready_for_review"


# ---------------------------------------------------------------- the receipt


def _read_only(db_path):
    """Open the relay store for reading and never for creating.

    Mirrors store.read_only_rows: every failure becomes a field rather than an exception, because a
    guard that raises on a locked database records nothing, and recording nothing is how a detector
    silently switches itself off.
    """
    try:
        connection = sqlite3.connect(
            Path(db_path).as_uri() + "?mode=ro", uri=True, timeout=SQLITE_TIMEOUT
        )
    except (OSError, sqlite3.Error, ValueError):
        return None
    connection.row_factory = sqlite3.Row
    return connection


def lookup_receipt(db_path, *, relationship_id, session_id, turn_id):
    """The reviewable receipt this turn produced, and whether it stands at the current head.

    The head is computed FIRST and the matching event is then fetched by its id. Selecting a row by
    session and turn and comparing it to the head afterwards picks an arbitrary row when one turn
    emitted more than one revision, so a child that superseded its own work could have the older
    event chosen and its perfectly current receipt reported as superseded, which is a hold.

    Three identities make a receipt this declaration's rather than some other one's, and all three
    must be NAMED: the relationship the assignment published, the session, and the turn. A receipt
    from an earlier turn or another session can still stand at the head while the turn in front of
    us produced nothing.

    stage deliberately admits staged. A child emits from inside its own turn, so the host reports
    that turn as inProgress and the event is stored staged; it is finalized only after the daemon
    observes the turn ending, which is AFTER this hook has run. Requiring final here would read
    every honest readiness as a missing receipt and hold it. What staged must never do is survive a
    turn that failed or was interrupted, and head_revision already excludes a suppressed event.

    Returns (receipt, readable). readable False means we could not look, which is never the same
    answer as there is nothing there.
    """
    if not (named(relationship_id) and named(session_id) and named(turn_id)):
        return None, True
    if not db_path:
        # We were never told where the store is, so we cannot look. Reported as unreadable rather
        # than as an absent receipt, because the difference between those two answers is a hold.
        return None, False
    connection = _read_only(db_path)
    if connection is None:
        return None, False
    base = {
        "relationshipId": relationship_id,
        "sessionId": session_id,
        "turnId": turn_id,
        "atCurrentHead": False,
    }
    try:
        relationship = connection.execute(
            "SELECT status, superseded_by, execution_generation FROM relationships"
            " WHERE relationship_id = ?",
            (relationship_id,),
        ).fetchone()
        if relationship is None:
            # A readable answer: the assignment named a relationship this store does not have.
            return dict(base, evidence="relationship_absent"), True
        if relationship["status"] != "active" or relationship["superseded_by"]:
            return dict(base, evidence="relationship_not_active"), True
        head = head_revision(connection, relationship_id, relationship["execution_generation"])
        if head.get("evidence") in AMBIGUOUS:
            # Not "no receipt". The lineage this generation declares does not identify a single
            # tip, so nothing is at the head and the coordinator has a lineage problem to settle.
            # The hold decision is the same; the cause is worth reporting as its own.
            return dict(base, evidence="head_" + str(head.get("evidence"))), True
        if not head.get("eventId"):
            return dict(base, evidence="no_reviewable_revision"), True
        row = connection.execute(
            "SELECT event_id, stage, revision_hash, turn_thread_id, turn_id, producer"
            "  FROM events WHERE event_id = ?",
            (head["eventId"],),
        ).fetchone()
    except sqlite3.Error:
        return None, False
    finally:
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
    return dict(
        base,
        atCurrentHead=True,
        evidence="at_head",
        eventId=row["event_id"],
        revisionHash=row["revision_hash"],
        stage=row["stage"],
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


def _held_records(hook_root):
    """Every recorded hold under one hook tree, as (session, turn, at). Returns (records, problem).

    A record that is not a record says the store being counted is wrong, and it is reported instead
    of being read as zero, which would quietly hand back a full hold budget.
    """
    records = []
    root = Path(hook_root)
    if not root.is_dir():
        return records, None
    for path in sorted(root.glob("*/*/*.json")):
        if path.name.startswith("."):
            continue
        try:
            record = json.loads(path.read_text(encoding="utf-8"))
        except (OSError, ValueError):
            return records, "hook/" + path.parent.parent.name
        if not isinstance(record, dict):
            return records, "hook/" + path.parent.parent.name
        held = record.get("held")
        if not isinstance(held, bool):
            return records, "hook.held"
        if held:
            records.append((path.parent.parent.name, path.parent.name, record.get("at")))
    return records, None


def hold_counters(directory, *, session_id, turn_id, now, workspace_root=None):
    """Count this hook's own holds from the records it published. Returns (counters, malformed).

    Counted rather than stored, so nothing has to be kept consistent across writers.

    The two scopes are deliberately different. holdsThisGeneration is counted over one assignment,
    because one assignment directory is one dispatch request id and the relay keys the same dispatch
    id to the same generation, so a generation advance is a new dispatch and a new directory.
    holdsThisSessionWindow is counted over every assignment under the workspace: it is a bound on
    the SESSION over a rolling hour, and counting it per assignment would hand the same session a
    fresh hour every time a new assignment was declared for the same path.
    """
    counters = {"holdsThisTurn": 0, "holdsThisGeneration": 0, "holdsThisSessionWindow": 0}
    scoped, problem = _held_records(Path(directory) / "hook")
    if problem:
        return counters, problem
    for record_session, record_turn, _at in scoped:
        counters["holdsThisGeneration"] += 1
        if same_identity(record_session, session_id) and same_identity(record_turn, turn_id):
            counters["holdsThisTurn"] += 1

    roots = [Path(directory)]
    if workspace_root is not None:
        try:
            roots = sorted(p for p in Path(workspace_root).iterdir() if p.is_dir())
        except OSError:
            roots = [Path(directory)]
    horizon = intents.moment(now)
    for assignment in roots:
        found, problem = _held_records(assignment / "hook")
        if problem:
            return counters, problem
        for record_session, _record_turn, at in found:
            if not same_identity(record_session, session_id):
                continue
            when = intents.moment(at)
            if horizon is None or when is None or when > horizon - timedelta(
                minutes=SESSION_WINDOW_MINUTES
            ):
                # An unreadable or absent timestamp counts INSIDE the window. Failing the other way
                # would let an undated record renew the budget, and the budget exists to stop a loop.
                counters["holdsThisSessionWindow"] += 1
    return counters, None


def _next_hook_seq(directory, session_id, turn_id) -> int:
    sub = Path(directory) / "hook" / session_id / turn_id
    if not sub.is_dir():
        return 0
    used = [
        int(path.stem)
        for path in sub.glob("*.json")
        if not path.name.startswith(".") and path.stem.isdigit()
    ]
    return max(used) + 1 if used else 0


def record_observation(directory, record) -> str | None:
    """Publish this observation. One turn can be observed more than once, so the sequence is part
    of the identity: a single create-once file per turn would let the first observation consume the
    only name available and silently lose every later one."""
    session_id, turn_id = record.get("sessionId"), record.get("turnId")
    if not (valid_segment(session_id) and valid_segment(turn_id)):
        # The observation is built from the delivered Stop payload, so these reach a path from
        # outside. An identity that cannot be a directory name records nothing here rather than
        # publishing into a directory the assignment does not own.
        return None
    for _ in range(64):
        index = _next_hook_seq(directory, session_id, turn_id)
        target = Path(directory) / "hook" / session_id / turn_id / (str(index) + ".json")
        if publish(target, record) == "published":
            return "hook/" + session_id + "/" + turn_id + "/" + str(index)
    return None


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
        if not intents.correlated(marker, session):
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
    if not any(
        same_identity(intents.claimant(claim), session) for claim in marker.get("claims") or []
    ):
        return "marker_unclaimed", (
            "The coordinator bound this session, but it has not claimed this assignment. Released "
            "and recorded; a hold needs the child's own claim, not only the coordinator's bind."
        )
    if not marker.get("relationship"):
        return "managed_unregistered", (
            "This workspace is managed but its relationship is not registered. Register it, or "
            "record a disposition explaining why it cannot be."
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
    """Gather the records this Stop is judged against, decide, and publish the observation.

    The receipt is read only when a readiness was actually declared. A turn that declared itself
    waiting or interrupted is released on its own declaration, and going to the database anyway
    would let an unreadable store overwrite a perfectly good answer the child already gave.

    Which store to read has three sources in a deliberate order: db_path, when the caller named one
    explicitly; then the dbPath the COORDINATOR recorded in intent.json, because it is the party
    that registered the relationship and knows where its store lives; then default_db_path, the
    caller's own resolution, which is only a guess about somebody else's choice. Letting the guess
    win is the whole defect this ordering exists to prevent: a hook run without the coordinator's
    state selection would read a different store, find no relationship, and hold a child whose
    receipt is sitting at the head of the right one.
    """
    stop = stop_input or {}
    workspace = stop.get("cwd")
    session_id, turn_id = stop.get("session_id"), stop.get("turn_id")

    directory = marker = None
    unreadable = []
    if workspace:
        directory, marker, unreadable = intents.select_assignment(root, workspace, session_id)
        unreadable = list(unreadable or [])

    disposition = receipt = malformed_label = None
    if directory is not None:
        disposition, readable = read_disposition(directory, session_id, turn_id)
        if not readable:
            unreadable.append("disposition")
        malformed_label = intents.malformed_disposition(disposition)
        declared = (
            disposition.get("outcome")
            if not malformed_label
            and isinstance(disposition, dict)
            and same_identity(disposition.get("turnId"), turn_id)
            and same_identity(disposition.get("sessionId"), session_id)
            else None
        )
        registered = (marker or {}).get("relationship")
        if declared == READY and isinstance(registered, dict):
            recorded = (
                marker.get("intent", {}).get("dbPath")
                if isinstance(marker.get("intent"), dict)
                else None
            )
            resolved = db_path or recorded or default_db_path
            receipt, readable = lookup_receipt(
                resolved,
                relationship_id=registered.get("relationshipId"),
                session_id=session_id,
                turn_id=turn_id,
            )
            if not readable:
                unreadable.append("receipts")

    observation = {
        "stop_input": stop,
        "marker": marker,
        "disposition": disposition,
        "receipt": receipt,
        "store_unreadable": unreadable,
        "malformed": malformed_label,
        "now": now,
    }

    counters, corrupt = ({}, None)
    if directory is not None:
        counters, corrupt = hold_counters(
            directory,
            session_id=session_id,
            turn_id=turn_id,
            now=now,
            workspace_root=directory.parent,
        )
        if corrupt:
            # A hold budget that cannot be counted is corruption of the store this hook keeps, and
            # it is reported rather than defaulted to zero, which would hand back a full budget.
            counters = {"holdsThisTurn": None}

    verdict = decide(observation, counters=counters, mode=mode)
    verdict["assignmentId"] = directory.name if directory is not None else None
    verdict["counters"] = counters
    if record and directory is not None:
        verdict["recordedAs"] = record_observation(directory, verdict["record"])
    return verdict
