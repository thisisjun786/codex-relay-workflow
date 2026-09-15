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
                    "_covered", "_ambiguity_resolved", "resolve_assignment",
                    "selected_marker", "_claimant")
BINDING_WINDOW_MINUTES = 30
MAX_HOLDS_PER_TURN = 1
MAX_HOLDS_PER_GENERATION = 2
MAX_HOLDS_PER_SESSION_WINDOW = 3


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
FACT_IDENTITIES = {"attempts": ("taskId",),
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


def _correlated(marker, session_id):
    """Whether this session presented the dispatch request id the intent was declared with.

    The intent stores only the hash. Storing the id in the clear would make correlation empty,
    because any session able to read the directory could then present it.
    """
    claim = next((c for c in (marker.get("claims") or [])
                  if _same_identity(_claimant(c), session_id)), None)
    if not claim:
        return False
    presented = claim.get("dispatchRequestId")
    if not presented:
        return False
    digest = hashlib.sha256(str(presented).encode("utf-8")).hexdigest()
    return digest == (marker.get("intent") or {}).get("dispatchRequestIdHash")


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
    """
    published = []
    for assignment in workspace.get("assignments") or []:
        declared = _moment((assignment.get("intent") or {}).get("declaredAt"))
        # A directory whose intent has not landed is nobody's assignment yet. Skipping it leaves
        # the reader on a valid earlier state, which is what the create-once layout already gives.
        if declared is not None:
            published.append((declared, str(assignment.get("assignmentId") or ""), assignment))
    claimed = [row for row in published
               if any(_same_identity(_claimant(claim), session_id)
                      for claim in (row[2].get("claims") or []))]
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
        if not _correlated(marker, session):
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
    if not any(_same_identity(_claimant(claim), session)
               for claim in marker.get("claims") or []):
        return "marker_unclaimed", (
            "The coordinator bound this session, but it has not claimed this assignment. Released "
            "and recorded; a hold needs the child's own claim, not only the coordinator's bind.")
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
    """Row ids the host-verification packet asks about, read from its own table.

    Derived rather than declared for the same reason the trace list is: a row added to the
    packet later has to enter this denominator whether or not anyone remembers, and a row
    deleted from the observation record has to leave a hole somebody sees.
    """
    path = Path(contract_path or CONTRACT)
    found = []
    for line in path.read_text(encoding="utf-8").splitlines():
        stripped = line.strip()
        if not stripped.startswith("| H"):
            continue
        label = stripped.split("|")[1].strip()
        if len(label) > 1 and label[0] == "H" and label[1:].isdigit() and label not in found:
            found.append(label)
    return found


def check_host_observations(directory=None, contract_path=None):
    """Hold each recorded host observation to the capability record it names.

    The two are separate readings of one host: the capability record is what the binary
    declares it accepts, and the observation record is what a real invocation delivered.
    Checking them against each other is the part of the host packet that can be rechecked
    offline, so a record that drifts from its own paired schema is reported here rather than
    discovered by whoever relies on it next. Nothing here re-runs a hook, and an observation
    record proves nothing on its own about the host running now.

    The packet is answered as a set, so the rows are counted against the contract's own table
    rather than against whatever the record happens to contain. A record carrying six of seven
    rows is a packet with a hole in it, and the load-bearing row is the cheapest one to lose.
    """
    directory = Path(directory or HOST_FIXTURES)
    asked = packet_questions(contract_path)
    answered = set()
    problems = []
    checked = 0
    for path in sorted(directory.glob("host-observation-*.json")):
        try:
            record = json.loads(path.read_text(encoding="utf-8"))
        except ValueError as exc:
            problems.append(f"{path.name}: unreadable ({exc})")
            continue
        checked += 1
        paired = directory / Path(str(record.get("capabilityRecord"))).name
        if not paired.is_file():
            problems.append(f"{path.name}: names a capability record that is not beside it")
            continue
        capability = json.loads(paired.read_text(encoding="utf-8"))
        stop = _mapping(_mapping(capability.get("events")).get("stop"))
        declared = sorted(_mapping(stop.get("input")).get("required") or [])
        delivered = sorted(_mapping(record.get("stopInput")).get("fields") or [])
        if not declared:
            problems.append(f"{path.name}: {paired.name} declares no required Stop input")
        elif delivered != declared:
            missing = sorted(set(declared) - set(delivered))
            unexpected = sorted(set(delivered) - set(declared))
            problems.append(f"{path.name}: delivered Stop fields disagree with {paired.name} "
                            f"(missing {missing}, unexpected {unexpected})")
        rows = _mapping(record.get("observations"))
        if not rows:
            problems.append(f"{path.name}: records no observation rows")
        answered.update(rows)
        for row_id, row in sorted(rows.items()):
            row = _mapping(row)
            status = row.get("status")
            if status == "resolved" and not row.get("evidence"):
                problems.append(f"{path.name}: {row_id} is resolved and names no evidence")
            elif status == "unresolved" and not row.get("whyUnresolved"):
                problems.append(f"{path.name}: {row_id} is unresolved and says nothing about why")
            elif status not in ("resolved", "unresolved"):
                problems.append(f"{path.name}: {row_id} carries no readable status")
    if not asked:
        problems.append("the contract's host-verification packet asks nothing; its table is "
                        "unreadable or gone")
    elif not checked:
        problems.append("the contract asks " + ", ".join(asked) + " and no observation record "
                        "answers any of them")
    else:
        unanswered = [row for row in asked if row not in answered]
        if unanswered:
            problems.append("the packet asks " + ", ".join(unanswered) + " and no observation "
                            "record carries that row")
        unasked = sorted(answered - set(asked))
        if unasked:
            problems.append("recorded rows the packet does not ask about: " + ", ".join(unasked))
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
        print(f"host observations: {host_checked} record(s) cover all {len(asked)} packet rows "
              "and agree with the capability record each names. A recording, not a live host run.")

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
