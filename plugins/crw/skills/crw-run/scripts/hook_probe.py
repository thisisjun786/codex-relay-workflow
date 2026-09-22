#!/usr/bin/env python3
"""Read-only probe for the Codex hook contract in ../references/hook-contract.md.

observe  reads the installed Codex binary and reports the hook input/output schemas it
         embeds, plus which events this host registers. It opens nothing for writing and
         starts no session.
decide   applies the contract's Stop decision to one sanitized observation and prints the
         resulting hook output.
replay   runs every fixture through decide and checks the recorded expectation.

Replay proves this parser and this decision table. It is not evidence that the host invoked
a hook or honored its output; that evidence comes from a real run and is recorded separately.
Replay also cross-checks the recorded host observations under fixtures/host against the
capability record each one names. That compares two recordings of the same host; it re-runs
nothing and starts no session.
"""

import argparse
import ast
from datetime import datetime, timedelta, timezone
import hashlib
import json
import mmap
import os
from pathlib import Path
import shutil
import sys

SCHEMA_NEEDLE = b'{\n  "$schema": "http://json-schema.org/draft-07/schema#"'
SCHEMA_WINDOW = 1 << 16
FIXTURES = Path(__file__).resolve().parent / "fixtures" / "decisions"
HOST_FIXTURES = Path(__file__).resolve().parent / "fixtures" / "host"
CONTRACT = Path(__file__).resolve().parent.parent / "references" / "hook-contract.md"

# Dispositions that carry no completion obligation and never justify holding a turn.
RELEASING = ("in_progress", "blocked_needs_input", "interrupted", "failed")
# The three omissions this contract detects. Everything else releases.
OMISSIONS = ("managed_unregistered", "receipt_missing", "undeclared_turn_end")
TRACED_FUNCTIONS = ("observe_state", "decide", "derive_assignment_state",
                    "identity_contested", "classify_declaration", "_correlated",
                    "_correlation_problem", "_covered", "_ambiguity_resolved",
                    "resolve_assignment",
                    "selected_marker", "_claimant")
BINDING_WINDOW_MINUTES = 30
MAX_HOLDS_PER_TURN = 1
MAX_HOLDS_PER_GENERATION = 2
MAX_HOLDS_PER_SESSION_WINDOW = 3
# The names a recorded Stop field type may use. The vocabulary is checkable; which field carries
# which type is the observation itself, and a reader that asserted that would be stating the
# answer rather than checking the record.
JSON_TYPE_NAMES = ("NoneType", "bool", "dict", "float", "int", "list", "str")


def find_codex_binary(explicit):
    return _find_codex_binary(explicit)


def _find_codex_binary(explicit):
    if explicit:
        return Path(explicit)
    found = shutil.which("codex")
    return Path(os.path.realpath(found)) if found else None


FACT_LISTS = ("attempts", "claims", "conflicts", "resolutions")
FACT_OBJECTS = ("intent", "bound", "relationship", "resolution")
# Lists a fact may carry inside itself. Named here so the next nested field is a table entry rather
# than a discovery: validating only the outer record left a field read to land on a string one level
# down, which is the same silent detector-disabling failure the outer check exists to prevent.
NESTED_FACT_LISTS = {"resolutions": ("adjudicated",), "resolution": ("adjudicated",)}
# Every identity slot a fact may carry, held to the same standard the relay's own reader holds it
# to (intent.IDENTITY_FIELDS). Kept in step deliberately: a field the relay reports as malformed
# and this reader reads through answers a different state for the same bytes, and a replay that
# agrees on every fixture would still be reporting parity it does not have.
FACT_IDENTITIES = {"intent": ("dispatchRequestIdHash", "dbPath"),
                   "bound": ("sessionId", "taskId"),
                   "relationship": ("relationshipId",),
                   "attempts": ("taskId", "outcome"),
                   "claims": ("sessionId", "dispatchRequestId"),
                   "conflicts": ("attemptedSessionId", "attemptedTaskId"),
                   "resolutions": ("chosenTaskId", "chosenSessionId"),
                   "resolution": ("chosenTaskId", "chosenSessionId")}


def _malformed_nested(key, record):
    """A nested list inside a fact, held to the standard the fact itself is held to."""
    for field in FACT_IDENTITIES.get(key, ()):
        value = record.get(field)
        if field in record and not isinstance(value, str):
            return key + "." + field
    for field in NESTED_FACT_LISTS.get(key, ()):
        value = record.get(field)
        if field in record and not isinstance(value, list):
            return key + "." + field
        for item in value or []:
            if not isinstance(item, dict):
                return key + "." + field + " entry"
    return None


def _mapping(value):
    """A record whose fields can be read, or an empty one.

    Shape is answered once by `_malformed`, so no reader downstream has to ask again.
    """
    return value if isinstance(value, dict) else {}


def _malformed(observation):
    """The first published record whose shape stops it being readable as a fact, or None.

    Readable and wrongly shaped is its own answer, and it needs one. Letting a `.get` land on a
    string ends the hook in a traceback, and a traceback records nothing at all: no observation, no
    state, no row a coordinator can read, and a turn that then looks exactly like an ordinary turn
    end. Anyone able to write a single fact could otherwise switch detection off for a workspace by
    writing a value of the wrong type.
    """
    for key in ("stop_input", "disposition", "receipt", "marker", "workspace"):
        value = observation.get(key)
        if value is not None and not isinstance(value, dict):
            return key
    markers = [observation.get("marker")]
    workspace = observation.get("workspace")
    if isinstance(workspace, dict):
        assignments = workspace.get("assignments")
        if assignments is not None and not isinstance(assignments, list):
            return "workspace.assignments"
        for entry in assignments or []:
            if not isinstance(entry, dict):
                return "workspace.assignments entry"
            markers.append(entry)
    for marker in markers:
        if not isinstance(marker, dict):
            continue
        for key in FACT_LISTS:
            value = marker.get(key)
            if value is not None and not isinstance(value, list):
                return key
            for item in value or []:
                if not isinstance(item, dict):
                    return key + " entry"
                nested = _malformed_nested(key, item)
                if nested:
                    return nested
        for key in FACT_OBJECTS:
            value = marker.get(key)
            if value is not None and not isinstance(value, dict):
                return key
            nested = _malformed_nested(key, value or {})
            if nested:
                return nested
    return None


def _malformed_counters(observation):
    """Validate persisted budgets only when this observation could request a hold."""
    if "counters" not in observation:
        return None
    counters = observation["counters"]
    if not isinstance(counters, dict):
        return "counters"
    for key, value in counters.items():
        if not isinstance(value, int) or isinstance(value, bool) or value < 0:
            return "counters." + str(key)
    return None


def _moment(value):
    try:
        parsed = datetime.fromisoformat(str(value).replace("Z", "+00:00"))
    except (TypeError, ValueError):
        return None
    # A naive timestamp is read as UTC rather than compared against an aware one, because
    # mixing the two raises rather than answering.
    return parsed if parsed.tzinfo else parsed.replace(tzinfo=timezone.utc)


def _resolutions(marker):
    """Every adjudication on record. They accumulate; none replaces another."""
    items = list(marker.get("resolutions") or [])
    single = marker.get("resolution")
    if single:
        items.append(single)
    return items


def _fact_digest(fact):
    payload = {k: v for k, v in fact.items() if k != "factId"}
    return hashlib.sha256(
        json.dumps(payload, sort_keys=True, separators=(",", ":")).encode("utf-8")).hexdigest()


def _covered(fact, resolutions):
    """Is this exact fact adjudicated by one of these resolutions?

    Coverage is by identity and digest together: the id selects the fact, the digest confirms the
    content matches what was recorded. Nothing is inferred from timestamps, because publication
    order is not timestamp order and a backdated fact would otherwise be covered unreviewed. A
    fact carrying no factId can never be covered, deliberately: unidentified evidence must not be
    able to disappear.
    """
    fact_id = fact.get("factId")
    if not fact_id:
        return False
    digest = _fact_digest(fact)
    for resolution in resolutions:
        for entry in resolution.get("adjudicated") or []:
            if entry.get("factId") == fact_id and entry.get("digest") == digest:
                return True
    return False


def _competing_facts(marker):
    bound = marker.get("bound") or {}
    facts = [a for a in marker.get("attempts") or []
             if a.get("outcome") == "accepted" and a.get("taskId")
             and a.get("taskId") != bound.get("taskId")]
    # A claim competes unless it verifiably belongs to the bound session, and the owner comes from
    # the path rather than the body. Reading the body let a competitor delete itself from this list
    # by omitting its session or by naming the bound one, leaving identityContested false and
    # clearing the way for a verdict over evidence nobody adjudicated. A claim naming nothing
    # verifiable competes too: voiding it as an identity must not also hide it as a fact.
    facts += [c for c in marker.get("claims") or []
              if _claimant(c) != bound.get("sessionId")]
    return facts + list(marker.get("conflicts") or [])


def _ambiguity_resolved(marker):
    """Pre-bind, an adjudication clears ambiguity only by agreeing and by covering everything.

    Two resolutions can each cover every ambiguous fact while naming different identities. That is
    a coordinator contradiction rather than a decision, so ambiguity survives it. Identity means the
    whole task and session pair; agreeing on the task alone is not agreement.
    """
    resolutions = [r for r in _resolutions(marker)
                   if r.get("chosenTaskId") and r.get("chosenSessionId")]
    if not resolutions:
        return False
    pairs = {(r.get("chosenTaskId"), r.get("chosenSessionId")) for r in resolutions}
    if len(pairs) != 1:
        return False
    chosen_task, chosen_session = next(iter(pairs))
    # A taskId on a failed or unknown attempt does not establish accepted creation. Counting it
    # would let a resolution select an unconfirmed task and incorrectly clear ambiguity.
    tasks = {a.get("taskId") for a in marker.get("attempts") or []
             if a.get("outcome") == "accepted" and a.get("taskId")}
    # Read from the path, so a resolution cannot be cleared by naming a session that only ever
    # "claimed" by writing that name into a body it controlled.
    sessions = {_claimant(c) for c in marker.get("claims") or []}
    sessions.discard(None)
    # Both halves must name something the record contains. Accepting a pair with one half absent
    # would let a half-invented identity clear an ambiguity nobody adjudicated.
    if chosen_task not in tasks or chosen_session not in sessions:
        return False
    facts = [a for a in marker.get("attempts") or []
             if a.get("outcome") == "accepted" and a.get("taskId")]
    facts += list(marker.get("claims") or [])
    return all(_covered(f, resolutions) for f in facts)


def derive_assignment_state(marker, now=None):
    """The assignment state, computed from which facts exist rather than stored and transitioned.

    A stored state would need every writer to agree on transition rules, and every delayed writer
    would then be a regression risk. Presence is monotonic, so a late fact can never move this
    backwards and no interleaving needs a special case.
    """
    intent = marker.get("intent") or {}
    attempts = marker.get("attempts") or []
    claims = marker.get("claims") or []
    if marker.get("bound"):
        return "relationship_registered" if marker.get("relationship") else "identity_bound"
    resolved = _ambiguity_resolved(marker)
    accepted = [a for a in attempts if a.get("outcome") == "accepted"]
    task_ids = {a.get("taskId") for a in accepted if a.get("taskId")}
    if not resolved and (len(task_ids) > 1 or len(claims) > 1):
        return "ambiguous_identity"
    # Expiry is anchored on declaration and nothing else. Any acceptance-derived anchor can be
    # moved later by a fact that arrives later, which would let an expired intent revive; an
    # anchor that never moves is the only one that makes expiry monotonic without storing state.
    anchor = _moment(intent.get("declaredAt"))
    seen = _moment(now)
    if anchor and seen and seen > anchor + timedelta(minutes=BINDING_WINDOW_MINUTES):
        return "intent_expired"
    if not resolved and not accepted and any(a.get("outcome") == "unknown" for a in attempts):
        return "creation_unknown"
    return "creation_accepted" if accepted else "intent_declared"


def identity_contested(marker):
    """A competing fact after a bind that no resolution has adjudicated.

    A resolution is scoped to the evidence it names, so one that does not name a fact cannot
    suppress it. It can still name a fact published later whose content happens to match the digest
    it recorded; that gap is closed by the writer reading before adjudicating, not by this check.
    Post-bind only a resolution naming the bound identity applies: one choosing a competitor must
    not authorise a verdict merely because it covers that competitor's evidence.
    """
    bound = marker.get("bound") or {}
    if not bound:
        return False
    applicable = [r for r in _resolutions(marker)
                  if r.get("chosenSessionId") == bound.get("sessionId")
                  and r.get("chosenTaskId") == bound.get("taskId")]
    return any(not _covered(fact, applicable) for fact in _competing_facts(marker))


# Why a session's claim does not correlate with the assignment it sits in. Separate conditions
# because they are separate repairs; they release identically, so they are labels and not states.
CLAIM_ABSENT = "claim_absent"
CLAIM_DISPATCH_UNNAMED = "claim_dispatch_unnamed"
CLAIM_DISPATCH_MISMATCH = "claim_dispatch_mismatch"
INTENT_DISPATCH_UNNAMED = "intent_dispatch_unnamed"
INTENT_ASSIGNMENT_MISMATCH = "intent_assignment_mismatch"


def _correlation_problem(marker, session_id, assignment=None):
    """Which correlation condition this session's claim fails, or None when it correlates.

    The chain is preimage -> intent hash -> assignment, and all three links are required. The
    intent stores only the hash, because storing the id in the clear would make correlation empty:
    any session able to read the directory could then present it. But the hash and the preimage can
    be made to agree with each other by anything that can write the marker, so the assignment is
    the third link, and no writer of the facts inside the assignment chooses it: it is the
    directory name, and the directory name IS the hash. Narrowly that and no more - whether the
    enumerated directory is the one the coordinator created is a property of the enumeration.

    Every condition is answered apart from the others: reporting a mismatching claim for an intent
    that published no hash, or one published under another assignment, sends an operator to settle
    a claim that is correct.
    """
    claim = next((c for c in (marker.get("claims") or [])
                  if _same_identity(_claimant(c), session_id)), None)
    if not claim:
        return CLAIM_ABSENT
    presented = claim.get("dispatchRequestId")
    if not _named(presented):
        return CLAIM_DISPATCH_UNNAMED
    declared = (marker.get("intent") or {}).get("dispatchRequestIdHash")
    if not _named(declared):
        return INTENT_DISPATCH_UNNAMED
    if _named(assignment) and not _same_identity(declared, assignment):
        return INTENT_ASSIGNMENT_MISMATCH
    digest = hashlib.sha256(presented.encode("utf-8")).hexdigest()
    return None if _same_identity(digest, declared) else CLAIM_DISPATCH_MISMATCH


def _correlated(marker, session_id, assignment=None):
    """Whether this session presented the dispatch request id the intent was declared with.

    The rule itself lives in _correlation_problem; this is that answer read as a yes or no. Written
    as a delegation rather than as its own copy because the two windows must not be able to
    disagree, and because a looser test here than in _correlation_problem is exactly the drift this
    reader exists to detect: a blank preimage passed a truthiness test and failed _named, so one
    window correlated a claim the other refused.
    """
    problem = _correlation_problem(marker, session_id, assignment)
    if problem is not None:
        return False
    return True


def _selecting_claim(marker, session_id, assignment):
    """This session's claim that independently names this assignment, or None.

    Which assignment a turn is about, answered without reading the intent. An assignment id is the
    hash of a dispatch request id, so the claim carries the whole answer: the path authorises the
    owner, the body confirms the writer meant it, and hashing the preimage says which assignment
    the claim belongs to.

    Independent of the intent on purpose. Selecting on the intent's content drops a candidate whose
    intent cannot be read, and an unreadable store must never be reported as an absent one: the
    reader would skip the current assignment, select an older one and hold against stale state.
    A claim that hashes elsewhere still selects nothing, which is what stops an uncorrelated claim
    shadowing an older assignment that owes a hold.
    """
    for claim in marker.get("claims") or []:
        if not isinstance(claim, dict):
            continue
        if not _same_identity(_claimant(claim), session_id):
            continue
        presented = claim.get("dispatchRequestId")
        if not _named(presented):
            continue
        digest = hashlib.sha256(presented.encode("utf-8")).hexdigest()
        if _same_identity(digest, assignment):
            return claim
    return None


def _selected_assignment(observation):
    """The assignment id the selected marker was read under, or None when none was supplied.

    A workspace listing names each assignment, so the resolved one carries its own id. A
    pre-resolved marker has no directory to read, so a fixture states the assignment or leaves the
    third correlation link unasked; the relay's own reader always supplies it, because it walked to
    the directory to get there.

    A workspace entry written without an assignmentId therefore leaves that link unasked rather
    than failing, and tests less than it looks like it does.
    """
    workspace = observation.get("workspace")
    if workspace is None:
        return observation.get("assignment")
    return (selected_marker(observation) or {}).get("assignmentId")


def _named(value):
    """Whether a record actually names an identity.

    Missing, empty, blank and non-string values all name nothing, and collapsing them to one
    answer is the whole point: two records that name nothing must never compare equal, which is
    exactly what `None == None` quietly does.
    """
    return isinstance(value, str) and bool(value.strip())


def _same_identity(left, right):
    """Do two records name the same identity? Unnamed on either side is never a match."""
    return _named(left) and _named(right) and left == right


def _claimant(claim):
    """The session a claim belongs to, taken from the path that authorised the write.

    A child may write inside `claims/<own session>/`, so the directory carries an identity the
    filesystem enforced, while the body carries one anybody holding that directory can type.
    Selecting on the body lets a later assignment's child name an earlier session and capture its
    Stop. The body must still agree: a fact that contradicts its own location is not one to act on.

    The body is required, not merely required to agree. The path proves who could have written the
    record; only the body says the writer meant to claim this assignment. Inferring the assertion
    from a directory that happens to exist let an old path-only claim capture a session whose own
    assignment had not published its claim yet. A claim naming nobody owns nothing, and it still
    competes, so refusing to read an identity out of it never deletes it.

    This only means anything because `factId` is the path the reader walked to, assigned by the
    reader, never copied out of the body. A fact addressed by its owning directory and one
    addressed by the file inside it name the same owner, so both are read the same way.
    """
    parts = str(claim.get("factId") or "").split("/")
    if (len(parts) not in (2, 3) or parts[0] != "claims"
            or (len(parts) == 3 and parts[2] != "claim.json")):
        return None
    owner = parts[1]
    # A segment that cannot be a session directory is not an identity. Unreachable from a real
    # directory walk, and refused here anyway: this function decides who owns a Stop.
    if owner in (".", ".."):
        return None
    body = claim.get("sessionId")
    if not _named(body) or body != owner:
        return None
    return owner


def _receipt_matches(receipt, stop, marker):
    """Does this receipt belong to the declaration being judged?

    Pointing at the current head is not enough. A receipt from an earlier turn, or from another
    session, can still point at the head while the turn in front of us produced nothing, and
    accepting it would let one producer's record stand in for another's. A receipt that names
    neither session nor turn is unmatched for the same reason unidentified marker evidence is
    uncoverable: identity has to be present, not assumed.

    Session and turn stopped being enough once assignments could coexist under one workspace. The
    same session can be the child of more than one at a time, so the receipt must also name the
    assignment being judged, through the relationship that assignment published. An assignment that
    has not published one cannot have a receipt attributed to it at all; that turn is already a
    detected omission and is answered as one rather than released on evidence nothing can place.
    """
    if not receipt or not receipt.get("atCurrentHead"):
        return False
    if not (_same_identity(receipt.get("sessionId"), stop.get("session_id"))
            and _same_identity(receipt.get("turnId"), stop.get("turn_id"))):
        return False
    registered = (marker or {}).get("relationship") or {}
    return _same_identity(receipt.get("relationshipId"), registered.get("relationshipId"))


def classify_declaration(observation):
    """What this turn declared, independent of who is bound. Used for the bound session's
    decision and for the record kept during the pre-bind window."""
    stop = observation.get("stop_input") or {}
    disposition = observation.get("disposition")
    receipt = observation.get("receipt")
    outcome = None
    if (disposition is not None
            and _same_identity(disposition.get("turnId"), stop.get("turn_id"))
            and _same_identity(disposition.get("sessionId"), stop.get("session_id"))):
        outcome = disposition.get("outcome")
    if outcome in RELEASING:
        return "declared_" + str(outcome)
    if outcome == "ready_for_review":
        if _receipt_matches(receipt, stop, selected_marker(observation)):
            return "declared_ready_receipted"
        return "receipt_missing"
    return "undeclared_turn_end"


def extract_schemas(binary):
    """Yield every draft-07 schema the binary embeds, decoded from a bounded window."""
    decoder = json.JSONDecoder()
    schemas = {}
    with open(binary, "rb") as handle:
        with mmap.mmap(handle.fileno(), 0, access=mmap.ACCESS_READ) as data:
            start = data.find(SCHEMA_NEEDLE)
            while start != -1:
                chunk = data[start:start + SCHEMA_WINDOW]
                try:
                    value, _ = decoder.raw_decode(chunk.decode("utf-8", "replace"))
                except ValueError:
                    value = None
                if isinstance(value, dict):
                    title = value.get("title")
                    if isinstance(title, str) and ".command." in title:
                        schemas[title] = value
                start = data.find(SCHEMA_NEEDLE, start + 1)
    return schemas


def capability_matrix(schemas):
    """Reduce the raw schemas to the facts the contract depends on."""
    events = {}
    for title, schema in schemas.items():
        name, _, kind = title.partition(".command.")
        entry = events.setdefault(name, {"event": name, "input": None, "output": None})
        properties = schema.get("properties") or {}
        if kind == "input":
            entry["input"] = {
                "required": sorted(schema.get("required") or []),
                "properties": sorted(properties),
            }
        elif kind == "output":
            specific = properties.get("hookSpecificOutput")
            definitions = schema.get("definitions") or {}
            decision = definitions.get("BlockDecisionWire") or definitions.get("PreToolUseDecisionWire")
            additional = False
            for definition in definitions.values():
                if "additionalContext" in (definition.get("properties") or {}):
                    additional = True
            entry["output"] = {
                "properties": sorted(properties),
                "topLevelDecisionValues": sorted((decision or {}).get("enum") or []),
                "hasHookSpecificOutput": specific is not None,
                "canEmitAdditionalContext": additional,
            }
    for entry in events.values():
        output = entry["output"]
        # PermissionRequest carries its decision inside hookSpecificOutput instead, so a false
        # here means "no top-level decision", not "cannot influence the action".
        entry["blocksViaTopLevelDecision"] = bool(output and output["topLevelDecisionValues"])
        entry["canInfluence"] = output is not None
    return dict(sorted(events.items()))


def registered_events(codex_home, known_events=()):
    """Report what declares hooks on this host. Nothing is written.

    Two different things are reported because they prove different things. Declaration files
    say what asked to be registered; the host's own trust state says what it recorded. Neither
    proves the host loaded a handler for the current session, and a glob can miss a layout it
    does not know, so counts from here are a floor and never an assertion of active wiring.
    """
    root = Path(codex_home)
    declared = []
    patterns = ["hooks.json", "plugins/cache/*/*/*/hooks/*.json", "plugins/*/hooks/*.json"]
    seen = set()
    candidates = [root / "hooks.json"]
    for pattern in patterns[1:]:
        candidates.extend(sorted(root.glob(pattern)))
    for path in candidates:
        if path in seen:
            continue
        seen.add(path)
        try:
            payload = json.loads(path.read_text(encoding="utf-8"))
        except (OSError, ValueError):
            continue
        hooks = payload.get("hooks")
        if isinstance(hooks, dict):
            declared.append({"source": str(path), "events": sorted(hooks)})
    # The host's trust keys are "<source>:<event>:<n>:<n>", but the source segment itself can
    # contain colons, so the event is identified by matching the names the binary declares
    # rather than by position.
    wanted = {name.replace("-", "_") for name in known_events}
    host_state = []
    try:
        for line in (root / "config.toml").read_text(encoding="utf-8").splitlines():
            stripped = line.strip()
            if stripped.startswith("[hooks.state.") and stripped.endswith("]"):
                for part in stripped[len("[hooks.state."):-1].strip('"').split(":"):
                    if part in wanted:
                        host_state.append(part)
    except OSError:
        host_state = []
    return {
        "declaredBy": declared,
        "declaredEvents": sorted({e for entry in declared for e in entry["events"]}),
        "hostRecordedEvents": sorted(set(host_state)),
        "note": ("Declaration files and the host's recorded hook state are separate signals. "
                 "Neither proves a handler ran for this session; only an observed invocation does."),
    }


def resolve_assignment(workspace, session_id):
    """Which assignment in this workspace is this session's, from one directory listing.

    A workspace path outlives the assignment that used it. Naming the marker after the path alone
    makes a second assignment collide with the first: every fact inside is create-once, so the new
    coordinator cannot publish its own intent, bind or relationship, and the new child reads the
    previous session's bind and releases. The assignment therefore owns a directory beneath the
    workspace hash, named by the dispatch request id hash its intent already carries.

    A session stays with the assignment it claimed. Taking the newest intent unconditionally would
    release a still-running earlier child the moment a later assignment is declared for the path.

    The claim has to name THIS assignment, not merely this session. Read on the claimant alone, an
    uncorrelated claim written into a newer assignment shadows an older one the session is
    legitimately bound to: selection prefers the newer directory, the decision path refuses the
    uncorrelated claim and releases, and the older assignment's undeclared turn never gets looked
    at. One file would switch holding off for a session correlated and bound somewhere else. The
    test is the claim against the directory and never against the intent, so a candidate whose
    intent cannot be read is still selected and still reported rather than skipped.
    """
    published = []
    for assignment in workspace.get("assignments") or []:
        declared = _moment((assignment.get("intent") or {}).get("declaredAt"))
        # A directory whose intent has not landed is nobody's assignment yet. Skipping it leaves
        # the reader on a valid earlier state, which is what the create-once layout already gives.
        if declared is not None:
            published.append((declared, str(assignment.get("assignmentId") or ""), assignment))
    claimed = [row for row in published
               if _selecting_claim(row[2], session_id, row[1]) is not None]
    pool = claimed or published
    if not pool:
        return None
    # Ties break on the assignment id, so every reader of the same listing picks the same one.
    return max(pool, key=lambda row: (row[0], row[1]))[2]


def selected_marker(observation):
    """The assignment this observation is about.

    An observation that already carries a resolved marker keeps its meaning. A workspace listing
    is resolved here, once, so the decision and the record it writes cannot name different
    assignments.
    """
    workspace = observation.get("workspace")
    if workspace is None:
        return observation.get("marker")
    return resolve_assignment(workspace, (observation.get("stop_input") or {}).get("session_id"))


def observe_state(observation):
    """Classify this turn. Observation only: no bounds, no holding, no side effects.

    last_assistant_message is deliberately never read. Prose is not evidence, and a Stop
    event alone is not completion, so neither can reach this classification.
    """
    stop = _mapping(observation.get("stop_input"))
    unreadable = observation.get("store_unreadable") or []

    # An unreadable store outranks an absent marker: "I could not look" must never be reported
    # as "there is nothing there".
    if unreadable:
        return "state_unreadable", "Cannot read " + ", ".join(sorted(unreadable)) + "."
    # Looked, and what is there is not a fact. Same rule, one step further along: a record that
    # cannot be read as a fact must not be reported as an absent one either.
    malformed = _malformed(observation)
    if malformed:
        return "marker_malformed", (
            f"The published {malformed} is not the shape a fact must be. Repair the marker; "
            "a record that cannot be read is reported, never guessed at.")
    marker = selected_marker(observation)
    if marker is None:
        return "unmanaged", "No assignment directory for this workspace."

    session = stop.get("session_id")
    bound = marker.get("bound") or None
    if not bound:
        # Binding is coordinator-only, so an unbound session is never the managed child yet.
        # Occupying the workspace is not identity.
        if not _correlated(marker, session, _selected_assignment(observation)):
            return "dispatch_uncorrelated", "This session presented no matching dispatch request id."
        return "correlated_unbound", (
            "Correlated to the intent but not yet bound by the coordinator. Released; the turn's "
            "observation is recorded for the coordinator to fold once the bind lands.")
    if not _named(bound.get("sessionId")):
        return "bound_identity_unnamed", (
            "The bind record names no session, so nothing can be shown to be the bound child. "
            "Repair the marker; a turn is never held against an identity nobody published.")
    if not _same_identity(bound.get("sessionId"), session):
        return "marker_claimed_by_other_session", "This session is not the bound child."

    declaration = classify_declaration(observation)
    # Nothing is missing. These win over every other condition, including a missing
    # registration, because holding a turn that was declared waiting, interrupted, or failed
    # is the one trade this policy refuses to make.
    if declaration.startswith("declared_"):
        return declaration, "The child declared this turn."
    # Nothing is held against a session that has not asserted it is the child. The coordinator holds
    # the creation receipt, so it can bind before the child publishes its claim, and that race must
    # not prompt a session which never claimed this assignment. Checked after the declared outcomes
    # so a turn that did declare keeps the more useful record.
    #
    # Asked through the correlation rule rather than through the claimant alone, which is the check
    # the pre-bind path above has always made and this one did not. An assignment id IS the hash of
    # a dispatch request id, so a claim naming a different dispatch is evidence about a different
    # assignment, and counting it satisfied the hold precondition with a fact nobody correlated.
    problem = _correlation_problem(marker, session, _selected_assignment(observation))
    if problem == CLAIM_ABSENT:
        return "marker_unclaimed", (
            "The coordinator bound this session, but it has not claimed this assignment. Released "
            "and recorded; a hold needs the child's own claim, not only the coordinator's bind.")
    if problem:
        # Apart from marker_unclaimed because the two clear differently. An unclaimed marker is the
        # bind-before-claim race and ends when the child publishes; this never ends by itself,
        # because claims/<session>/claim.json is create-once and a differing dispatch request id is
        # a conflict, so the correct claim can no longer be published at that path.
        return "claim_uncorrelated", (
            "This session is bound but its claim does not correlate with this assignment ("
            + problem + "). Released and recorded; every fact this reads is create-once, so it "
            "does not clear itself and no resolution consumed here will: correlation reads the "
            "claim and the intent, never the adjudications. Recovery is a new assignment, "
            "declared for a fresh dispatch request id.")
    if not marker.get("relationship"):
        return "managed_unregistered", (
            "This workspace is managed but its relationship is not registered. Register it, or "
            "record a disposition explaining why it cannot be.")
    if declaration == "receipt_missing":
        return "receipt_missing", (
            "Readiness is declared but no receipt exists at the current head revision. "
            "Emit the receipt over the actual artifacts.")
    return "undeclared_turn_end", (
        "No usable turn disposition was recorded for this turn. Record in_progress, "
        "blocked_needs_input, interrupted, failed, or ready_for_review with a receipt.")


def decide(observation):
    """Apply the hold bounds to an observation. The observation is recorded either way."""
    stop = _mapping(observation.get("stop_input"))
    state, reason = observe_state(observation)
    if state in OMISSIONS:
        malformed_count = _malformed_counters(observation)
        if malformed_count:
            state = "marker_malformed"
            reason = "Invalid persisted " + malformed_count + "; repair the hold budget record."

    def out(decision, final_state, final_reason):
        result = {
            "decision": decision,
            "state": final_state,
            "observation": state,
            "reason": final_reason,
            "record": {
                "observation": state,
                "turnId": stop.get("turn_id"),
                "sessionId": stop.get("session_id"),
                "decisionState": final_state,
                "held": decision == "block",
                "at": observation.get("now"),
            },
        }
        # Derivation follows the marker being readable, never one state's name. Asking whether the
        # state is `marker_malformed` described a symptom, and it missed an unreadable store that
        # also carried a malformed fact: that answers `state_unreadable` first and then derived a
        # summary from records that were not records, ending the hook with nothing recorded at all.
        marker = {} if _malformed(observation) else (selected_marker(observation) or {})
        if state == "correlated_unbound":
            # The pre-bind window is not blind: keep what the turn would have been judged as,
            # so the coordinator can fold it once the bind lands.
            result["record"]["pendingObservation"] = classify_declaration(observation)
        if state == "claim_uncorrelated" and marker:
            # Which artifact is wrong, kept separate from the decision. The conditions release the
            # same way and are settled differently, so the class survives in the record rather
            # than only in the reason text, and what the turn WOULD have been judged as is kept
            # too - this answer replaces a classification the coordinator still needs.
            result["record"]["claimEvidence"] = _correlation_problem(
                marker, stop.get("session_id"), _selected_assignment(observation))
            result["record"]["pendingObservation"] = classify_declaration(observation)
        if marker:
            result["record"]["assignmentState"] = derive_assignment_state(
                marker, observation.get("now"))
            # Always recorded, both ways: the coordinator needs to distinguish "checked and not
            # contested" from "nobody looked".
            result["record"]["identityContested"] = identity_contested(marker)
        result["hook_output"] = (
            {"decision": "block", "reason": final_reason, "continue": True}
            if decision == "block" else {}
        )
        return result

    if state not in OMISSIONS:
        return out("release", state, reason)

    # An omission is recorded whether or not it is held. Detection does not depend on the
    # hold succeeding, on this hook's position in the sequence, or on why a continuation
    # is already running.
    #
    # Exhaustion is evaluated first and deliberately. It is a terminal classification about
    # the assignment, while a per-turn or in-flight guard only says "not right now"; letting
    # the transient guard answer first would hide an assignment that has stopped converging.
    counters = observation.get("counters", {})
    if (int(counters.get("holdsThisGeneration") or 0) >= MAX_HOLDS_PER_GENERATION
            or int(counters.get("holdsThisSessionWindow") or 0) >= MAX_HOLDS_PER_SESSION_WINDOW):
        return out("release", "unresolved_handoff",
                   "Hold bound reached; recording an unresolved handoff instead of holding again.")
    if int(counters.get("holdsThisTurn") or 0) >= MAX_HOLDS_PER_TURN:
        return out("release", "hold_in_flight", "This turn already took its one hold.")
    if stop.get("stop_hook_active"):
        return out("release", "hold_in_flight",
                   "A continuation is already running for this turn; the omission is recorded.")
    return out("block", state, reason)


def load_fixtures(directory):
    return sorted(Path(directory).glob("*.json"))


def documented_traces(contract_path=None):
    """Trace ids the contract advertises, read from its own traces table.

    The contract names traces and claims each has a fixture. Twice already a trace was
    documented with nothing backing it and nothing noticed, so the claim is checked here
    rather than trusted.
    """
    path = Path(contract_path or CONTRACT)
    # An unreadable contract is a failed check, never an empty one. Returning [] here would make
    # replay print 0/0 traces and exit 0, which is the silently-passing verifier this check exists
    # to prevent.
    lines = path.read_text(encoding="utf-8").splitlines()
    found = []
    for line in lines:
        stripped = line.strip()
        if not stripped.startswith("| T"):
            continue
        label = stripped.split("|")[1].strip()
        if len(label) > 1 and label[0] == "T" and label[1:].isdigit() and label not in found:
            found.append(label)
    return found


def unfixtured_traces(fixture_dir, contract_path=None):
    names = [p.name.lower() for p in load_fixtures(fixture_dir)]
    missing = []
    for trace in documented_traces(contract_path):
        prefix = trace.lower() + "-"
        alt = trace.lower() + "b-"
        if not any(n.startswith(prefix) or n.startswith(alt) for n in names):
            missing.append(trace)
    return missing


def _owned_returns(function_node):
    """Return statements belonging to this function, excluding any nested definition."""
    found = []

    def walk(node):
        for child in ast.iter_child_nodes(node):
            if isinstance(child, (ast.FunctionDef, ast.AsyncFunctionDef, ast.Lambda)):
                continue
            if isinstance(child, ast.Return):
                found.append(child)
            walk(child)

    walk(function_node)
    return found


def return_sites(source_path=None):
    """Every return site the traced decision functions own, read from this module's own AST.

    Derived rather than declared on purpose. A hand-kept list stays green when someone adds a
    return and forgets to update it, which is the single failure a coverage gate exists to catch.
    """
    path = Path(source_path or __file__).resolve()
    tree = ast.parse(path.read_text(encoding="utf-8"))
    sites = {}
    for node in tree.body:
        if isinstance(node, ast.FunctionDef) and node.name in TRACED_FUNCTIONS:
            for statement in _owned_returns(node):
                key = (node.name, statement.lineno)
                if key in sites:
                    raise ValueError(
                        f"two return statements share {node.name}:{statement.lineno}; line "
                        "identity is ambiguous, so split them before trusting coverage")
                sites[key] = ast.get_source_segment(path.read_text(encoding="utf-8"), statement) or ""
    return sites


def _trace_returns(reached):
    # Resolved from the same names the denominator is built from, so the two can never drift.
    targets = {globals()[name].__code__ for name in TRACED_FUNCTIONS if name in globals()}

    def local(frame, event, arg):
        if event == "return":
            reached.add((frame.f_code.co_name, frame.f_lineno))
        return local

    def dispatch(frame, event, arg):
        return local if event == "call" and frame.f_code in targets else None

    return dispatch


def command_observe(args):
    binary = find_codex_binary(args.binary)
    if binary is None or not binary.exists():
        print("Codex binary not found. Pass --binary to point at it.", file=sys.stderr)
        return 3
    schemas = extract_schemas(binary)
    if not schemas:
        print(f"No embedded hook schemas found in {binary}.", file=sys.stderr)
        return 3
    events = capability_matrix(schemas)
    report = {
        "binary": "<codex-binary>" if args.sanitize else str(binary),
        "events": events,
        "registration": None if args.sanitize else registered_events(args.codex_home, events),
    }
    print(json.dumps(report, indent=2, sort_keys=True))
    return 0


def command_decide(args):
    payload = json.loads(Path(args.observation).read_text(encoding="utf-8"))
    print(json.dumps(decide(payload.get("observation", payload)), indent=2, sort_keys=True))
    return 0


def _check_one(label, observation, expected, report):
    actual = decide(observation or {})
    mismatch = {
        key: (expected[key], actual.get(key))
        for key in ("decision", "state", "observation")
        if key in expected and expected[key] != actual.get(key)
    }
    for key, want in (expected.get("record") or {}).items():
        got = (actual.get("record") or {}).get(key)
        if want != got:
            mismatch["record." + key] = (want, got)
    if mismatch:
        report.append(f"FAIL {label}: {json.dumps(mismatch, sort_keys=True)}")
        return False
    report.append(f"ok   {label}: {actual['decision']} {actual['state']} "
                  f"(observed {actual['observation']})")
    return True


def packet_questions(contract_path=None):
    """Row ids the host-verification packet asks about, each with the status it claims.

    Derived rather than declared for the same reason the trace list is: a row added to the
    packet later has to enter this denominator whether or not anyone remembers, and a row
    deleted from the observation record has to leave a hole somebody sees.

    The status travels with the id because the two artifacts have to agree. A record that
    downgrades a row to unresolved while the contract still prints Resolved for it is evidence
    quietly leaving through a door the contract says is shut. Reading the status here rather
    than forbidding unresolved outright keeps the other direction open: a row that genuinely
    cannot be watched on some later host is recorded as unresolved in both places, which is
    what the packet is for.
    """
    path = Path(contract_path or CONTRACT)
    found = {}
    for line in path.read_text(encoding="utf-8").splitlines():
        stripped = line.strip()
        if not stripped.startswith("| H"):
            continue
        cells = [cell.strip() for cell in stripped.strip("|").split("|")]
        label = cells[0]
        if len(label) < 2 or label[0] != "H" or not label[1:].isdigit():
            continue
        # A second row carrying an id the packet already used is reported rather than dropped.
        # Silently keeping the first would let a later table be added whose rows never take
        # effect, which is the quietest way for a packet to stop meaning what it prints.
        found[label] = "duplicated" if label in found else (cells[-1].lower() if len(cells) > 1
                                                            else "")
    return found


def _stated(value):
    """Text a reader can actually read, rather than any truthy value."""
    return isinstance(value, str) and bool(value.strip())


def _record_tag(name):
    """The host and version a host fixture's filename declares, as a pair, or None.

    The grammar is fixed here rather than inferred: host-<kind>-<host>-<version>.json, where the
    host is the first hyphen-delimited segment and the version is the whole remainder. Reading
    the version as the last segment instead would split 0.155.0-rc1 and call the release rc1.

    The tag is what makes two records the same reading of the same thing, and taking it from the
    filename keeps it out of reach of the record that would benefit from claiming it.
    """
    stem = Path(name).stem
    for prefix in ("host-observation-", "host-capability-"):
        if stem.startswith(prefix):
            host, _, version = stem[len(prefix):].partition("-")
            if host and version:
                return host, version
    return None


def check_host_observations(directory=None, contract_path=None):
    """Hold each recorded host observation to the capability record it names.

    The two are separate readings of one host: the capability record is what the binary
    declares it accepts, and the observation record is what a real invocation delivered.
    Checking them against each other is the part of the host packet that can be rechecked
    offline, so a record that drifts from its own paired schema is reported here rather than
    discovered by whoever relies on it next. Nothing here re-runs a hook, and an observation
    record proves nothing on its own about the host running now.

    Every record answers the whole packet by itself, checked against the contract's own table.
    Rows are never pooled across records: a host or version is watched on its own, so counting
    them together would let a record for a new one inherit rows an older one happened to have,
    which is the drift the packet exists to prevent. A record carrying six of seven rows is a
    packet with a hole in it, and the load-bearing row is the cheapest one to lose.

    For the same reason a record is paired only with the capability record for its own host and
    version. A 9.999.0 observation allowed to name the 0.154.0 schema would have its delivered
    fields checked against a binary nobody ran it against, which is inheritance wearing the
    shape of a check.

    What this cannot catch, stated because the check would otherwise look stronger than it is:
    a capability record carries no version inside it, so a copy of one version's record saved
    under another version's name reads as that version here. The filename is the only version
    identity these files have, and observe --sanitize is what produces a real one. The same goes
    for a recorded field type: the vocabulary is checked, but whether stop_hook_active was really
    delivered as a boolean is the observation, and a reader asserting it would be answering the
    question instead of checking the answer. That one is settled by reading the diff.
    """
    directory = Path(directory or HOST_FIXTURES)
    asked = packet_questions(contract_path)
    problems = []
    checked = 0
    for path in sorted(directory.glob("host-observation-*.json")):
        try:
            record = json.loads(path.read_text(encoding="utf-8"))
        except ValueError as exc:
            problems.append(f"{path.name}: unreadable ({exc})")
            continue
        checked += 1
        paired_name = Path(str(record.get("capabilityRecord"))).name
        paired = directory / paired_name
        own = _record_tag(path.name)
        version = record.get("version")
        # The pairing is settled before the fields are compared, because comparing delivered
        # fields against another version's schema reads as agreement while proving nothing. A
        # record that cannot be paired is still held to the packet below: a wrong schema and a
        # missing row are separate faults, and reporting only the first hides the second until
        # the first is fixed.
        if own is None:
            problems.append(f"{path.name}: its name carries no host and version tag")
        elif _record_tag(paired_name) != own:
            problems.append(f"{path.name}: names {paired_name}, which is not the capability "
                            "record for the same host and version")
        elif not isinstance(version, str) or own[1] not in version.split():
            # Whole-token equality, never a substring: 0.154.0 sits inside 10.154.0, so a
            # substring test would read one release as another.
            problems.append(f"{path.name}: the version it records, {version!r}, does not state "
                            f"{own[1]}, the version its own name carries")
        elif not paired.is_file():
            problems.append(f"{path.name}: names a capability record that is not beside it")
        else:
            capability = json.loads(paired.read_text(encoding="utf-8"))
            stop = _mapping(_mapping(capability.get("events")).get("stop"))
            declared = sorted(_mapping(stop.get("input")).get("required") or [])
            stop_input = _mapping(record.get("stopInput"))
            delivered = sorted(stop_input.get("fields") or [])
            if not declared:
                problems.append(f"{path.name}: {paired_name} declares no required Stop input")
            elif delivered != declared:
                missing = sorted(set(declared) - set(delivered))
                unexpected = sorted(set(delivered) - set(declared))
                problems.append(f"{path.name}: delivered Stop fields disagree with {paired_name} "
                                f"(missing {missing}, unexpected {unexpected})")
            else:
                # The recorded types are the structured half of what H1 concluded, so they are
                # held to the same field list rather than left as decoration that can be deleted
                # while the row still reads as evidence.
                types = record.get("stopInput", {}).get("types")
                if not isinstance(types, dict):
                    problems.append(f"{path.name}: records no Stop field types")
                elif sorted(types) != delivered:
                    absent = sorted(set(delivered) - set(types))
                    extra = sorted(set(types) - set(delivered))
                    problems.append(f"{path.name}: the recorded Stop field types do not cover the "
                                    f"fields it delivered (missing {absent}, unexpected {extra})")
                else:
                    # The vocabulary is held; the mapping is not. Requiring a particular field to
                    # be a particular type would freeze one host's answer inside the reader, and
                    # the next host has to be free to deliver something else and say so.
                    unnamed = [name for name, kind in sorted(types.items())
                               if kind not in JSON_TYPE_NAMES]
                    if unnamed:
                        problems.append(f"{path.name}: the recorded Stop field types name "
                                        "something that is not a JSON type for "
                                        + ", ".join(unnamed))
        rows = _mapping(record.get("observations"))
        unanswered = [row for row in asked if row not in rows]
        if unanswered:
            problems.append(f"{path.name}: the packet asks " + ", ".join(unanswered)
                            + " and this record carries no such row")
        unasked = sorted(set(rows) - set(asked))
        if unasked:
            problems.append(f"{path.name}: rows the packet does not ask about: "
                            + ", ".join(unasked))
        for row_id, row in sorted(rows.items()):
            row = _mapping(row)
            status = row.get("status")
            # A row has to carry its own question, its conclusion and its support, each as text
            # somebody can read. Truthiness is not enough: a number or a bare true would let a
            # row claim evidence it never states, and a resolved row without its conclusion keeps
            # the coverage count while losing the answer the count is for.
            claimed = asked.get(row_id)
            if not _stated(row.get("question")):
                problems.append(f"{path.name}: {row_id} states no question")
            elif status not in ("resolved", "unresolved"):
                problems.append(f"{path.name}: {row_id} carries no readable status")
            elif claimed in ("resolved", "unresolved") and status != claimed:
                problems.append(f"{path.name}: {row_id} records {status} where the packet's own "
                                f"table says {claimed}")
            elif status == "resolved" and not _stated(row.get("observed")):
                problems.append(f"{path.name}: {row_id} is resolved and states nothing observed")
            elif status == "resolved" and not _stated(row.get("evidence")):
                problems.append(f"{path.name}: {row_id} is resolved and states no evidence")
            elif status == "unresolved" and not _stated(row.get("whyUnresolved")):
                problems.append(f"{path.name}: {row_id} is unresolved and says nothing about why")
    duplicated = [row for row, claimed in asked.items() if claimed == "duplicated"]
    if duplicated:
        problems.append("the packet's table states " + ", ".join(duplicated) + " more than once, "
                        "so one row id would carry two statuses")
    unreadable = [row for row, claimed in asked.items()
                  if claimed not in ("resolved", "unresolved", "duplicated")]
    if unreadable:
        problems.append("the packet's table states no readable status for " + ", ".join(unreadable))
    if not asked:
        problems.append("the contract's host-verification packet asks nothing; its table is "
                        "unreadable or gone")
    elif not checked:
        problems.append("the contract asks " + ", ".join(asked) + " and no observation record "
                        "answers any of them")
    return checked, problems


def command_replay(args):
    failures = 0
    checked = 0
    skipped = []
    sites = return_sites()
    reached = set()
    previous = sys.gettrace()
    sys.settrace(_trace_returns(reached))
    try:
        for path in load_fixtures(args.fixtures):
            fixture = json.loads(path.read_text(encoding="utf-8"))
            # A sequence fixture asserts a progression: the same assignment read at successive
            # points, so a trace is checked as a transition rather than one snapshot.
            steps = fixture.get("steps")
            if steps:
                report = []
                good = True
                for index, step in enumerate(steps, 1):
                    label = f"{path.name} step {index}"
                    if not _check_one(label, step.get("observation"),
                                      step.get("expected") or {}, report):
                        good = False
                checked += 1
                if not good:
                    failures += 1
                for line in report:
                    print(line)
                continue
            expected = fixture.get("expected") or {}
            if not any(key in expected for key in ("decision", "state", "observation", "record")):
                skipped.append(path.name)
                continue
            checked += 1
            report = []
            if not _check_one(path.name, fixture.get("observation"), expected, report):
                failures += 1
            for line in report:
                print(line)
    finally:
        sys.settrace(previous)
    if skipped:
        print("SKIPPED without a decision, state, or observation expectation: " + ", ".join(skipped))
        return 1
    if not checked:
        print("No fixtures carried an expectation; nothing was checked.")
        return 1
    print(f"{checked - failures}/{checked} fixtures matched")

    orphans = unfixtured_traces(args.fixtures, args.contract)
    if orphans:
        print("DOCUMENTED WITHOUT A FIXTURE: " + ", ".join(orphans))
        if args.allow_unreached:
            # A focused subset cannot contain a fixture for every documented trace, so failing
            # here would make the advertised subset run impossible to pass. The waiver is printed
            # rather than silent, and the default run still fails.
            print("WAIVED: --allow-unreached was passed, so incomplete documented-trace coverage "
                  "did not fail this run.")
        else:
            print("The contract advertises these traces and nothing exercises them.")
            failures += 1
    else:
        documented = documented_traces(args.contract)
        print(f"documented traces backed by a fixture: {len(documented)}/{len(documented)}")

    missing = sorted(set(sites) - reached)
    print(f"return-site coverage: {len(sites) - len(missing)}/{len(sites)} sites reached")
    if missing:
        for name, line in missing:
            print(f"  UNREACHED {name}:{line}  {sites[(name, line)].splitlines()[0].strip()}")
        if args.allow_unreached:
            print("WAIVED: --allow-unreached was passed, so unreached return sites did not fail "
                  "this run. Fixture mismatches are never waived.")
        else:
            print("A return site no fixture executes is an untested decision path. Add a fixture "
                  "for it, or pass --allow-unreached for a deliberate subset run.")
    host_checked, host_problems = check_host_observations(args.host_fixtures, args.contract)
    if host_problems:
        for problem in host_problems:
            print("HOST OBSERVATION: " + problem)
        failures += 1
    else:
        asked = packet_questions(args.contract)
        print(f"host observations: {host_checked} record(s), each covering all {len(asked)} packet "
              "rows on its own and agreeing with the capability record it names. A recording, not "
              "a live host run.")

    if failures:
        return 1
    return 1 if (missing and not args.allow_unreached) else 0


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__,
                                     formatter_class=argparse.RawDescriptionHelpFormatter)
    sub = parser.add_subparsers(dest="command", required=True)

    observe = sub.add_parser("observe", help="Report the host's hook schemas and registrations")
    observe.add_argument("--binary", help="Path to the Codex binary; defaults to the one on PATH")
    observe.add_argument("--codex-home", default=os.environ.get("CODEX_HOME") or Path.home() / ".codex")
    observe.add_argument("--sanitize", action="store_true",
                         help="Omit host paths and registrations so the output is shareable")
    observe.set_defaults(func=command_observe)

    decide_parser = sub.add_parser("decide", help="Apply the Stop decision to one observation")
    decide_parser.add_argument("observation")
    decide_parser.set_defaults(func=command_decide)

    replay = sub.add_parser("replay", help="Check every fixture against its recorded expectation")
    replay.add_argument("--fixtures", default=str(FIXTURES))
    replay.add_argument("--contract", default=str(CONTRACT),
                        help="Contract whose documented traces must each have a fixture")
    replay.add_argument("--host-fixtures", default=str(HOST_FIXTURES),
                        help="Recorded host observations to hold to their capability record")
    replay.add_argument("--allow-unreached", action="store_true",
                        help="Report unreached return sites and incomplete documented-trace "
                             "coverage without failing; for deliberate subset runs only. Fixture "
                             "mismatches are never waived.")
    replay.set_defaults(func=command_replay)

    args = parser.parse_args()
    return args.func(args)


if __name__ == "__main__":
    try:
        raise SystemExit(main())
    except OSError as exc:
        print(f"Probe failed: {exc}. Nothing was written.", file=sys.stderr)
        raise SystemExit(3)
