#!/usr/bin/env python3
"""What is confirmed before a live trial starts, and what its interventions cost.

A live trial drives the installed runtime through a real completion, delivery, verdict and
correction round trip. Three recorded failures of one were all preparation: a supervisor that did
not survive the shell that launched it, participants created without a turn whose lifecycle no read
could resolve, and an assignment file read before the new relationship id was in it. This module
takes the readings that catch those, refuses a dispatch whose identity disagrees with the store, and
grades the trial's own intervention ledger. The procedure it serves is docs/live-trial.md.

Four rules are the whole of it.

It reads. It does not create a task, register a relationship, emit, deliver, record a verdict, or
start or stop anything. The only programs it starts are git and the relay entry point that the
runtime host record names through its owned pointer, and it composes every command line itself from
parameters: the start record carries no argv, so no record can nominate another program. That is
not a claim that nothing is written, because every relay command opens the store on construction;
it is the claim that nothing it runs registers, emits, delivers, records a verdict or changes
service state, that doctor runs before anything that can construct a store, and that a store which
did not exist before this process started is a refusal rather than a pass.

A cell is answered when the payload carries the field its predicate reads. Otherwise it is unknown,
whatever the exit status was, and the exit status is recorded beside the cell rather than
substituted for the reading. doctor exits non-zero while printing its whole diagnosis when it
cannot prove a shared store, so a cell reading the status would call a real answer unreadable and a
cell ignoring the payload would call a refusal a pass.

Four readings are taken here and two are graded. No read-only relay command performs a lifecycle
read and none returns what a creation receipt echoed, so those two arrive as captures, carry the
time they were taken, and are labelled as the caller's claim about a read this process did not
make.

A judgment is any field named passed or met, collected by walking the document that was assembled
rather than from a list of the kinds that produce them. The exit status comes from that walk and
from nothing else.
"""

import argparse
import datetime
import hashlib
import json
import math
import os
from pathlib import Path
import subprocess
import sys
import time

# Before the first import of anything in this repository, so an import cannot write __pycache__
# beside source belonging to a checkout this command only reads.
sys.dont_write_bytecode = True

ROOT = Path(__file__).resolve().parent.parent
sys.path.insert(0, str(ROOT / "scripts"))

from crw_runtime import check, definition, hostrecord, reading  # noqa: E402

SOURCE = "live-trial-startup"
CHECKER_VERSION = 1
RECORD_VERSION = 1

# The start record may lower these and may not raise them. A record that chose its own ceiling
# would be choosing how stale its own evidence may be, which is the question the bound exists to
# take away from it.
CAPTURE_AGE_CEILING = 900
ADVANCE_CEILING = 30
# How far ahead the window this dispatch opens may be. The preflight runs immediately before that
# dispatch, so a window opening hours later is not the one this run precedes: the completion and
# any intervention would happen before the declared interval and a later ledger would report an
# uninterrupted window it never measured.
WINDOW_ALLOWANCE = 300

RELAY_COMPONENT = "codex-session-relay"
POINTER_NAME = "current"

# Every relay subcommand this module composes. None of them registers, emits, delivers, records a
# verdict or changes service state. service is admitted with status and with nothing else, because
# the neighbouring verbs on that subparser start and stop a daemon.
RELAY_SUBCOMMANDS = ("doctor", "assignment-find", "criteria-show", "settings-show")
SERVICE_VERBS = ("status",)

# What a participant's expect must name at minimum. The predicate compares every key the record
# declares rather than this list, so a settings field added later is compared by declaring it; the
# minimum is here because a permission that went uncompared passed behind a usable true once.
REQUIRED_EXPECT = ("model", "reasoningEffort", "sandbox", "approvalPolicy")

# The rest of the relay's settings contract: the access a delivery actually runs with, beside the
# four a record declares. packages/codex-session-relay settings.py names all seven, and a store
# record can be usable and complete while these three disagree with what creation recorded.
DELIVERY_ACCESS = ("cwd", "runtimeWorkspaceRoots", "environments")


def receipt_access(payload, key):
    """Where a creation receipt actually carries each delivery setting.

    The bridge builds settings.actual from its OBSERVABLE list, and environments is not in it:
    the host reports the environment selection on the created thread, so the receipt carries it
    at creation.thread.environments instead. Reading all three out of settings.actual made every
    real receipt unreadable, and an unreadable cell refuses the start, so a checker meant to
    catch a trial starting with unrecorded access would have blocked every genuine trial instead.
    """
    if key == "environments":
        found = field(payload, "creation", "thread", "environments")
        if found is MISSING:
            # A host that does echo it under the settings is read there rather than called absent.
            found = field(payload, "settings", "actual", "environments")
        return found
    return field(payload, "settings", "actual", key)


def unreadable_access(value):
    """A null environment selection means unknown in the relay's own normalisation, and unknown
    is never flattened into empty here either. An empty list is a selection; a null is not."""
    return value is MISSING or value is None

# Every fact a later predicate compares a payload against. Declared here and required at ingress,
# because a comparison between two absent values is an agreement nobody established.
REQUIRED_FIELDS = (
    ("relay", "launcher"), ("relay", "launcherSha256"), ("relay", "stateDirectory"),
    ("relay", "socket"),
    ("store", "storeId"), ("store", "device"), ("store", "inode"), ("store", "challengeNonce"),
    ("supervisor", "pid"), ("supervisor", "witness"), ("supervisor", "launchedAt"),
    ("assignment", "relationshipId"), ("assignment", "parentTaskId"),
    ("assignment", "childTaskId"), ("assignment", "issueKey"),
    ("assignment", "executionGeneration"), ("assignment", "assignmentFile"),
    ("assignment", "dispatchMessageFile"), ("assignment", "criteria", "setDigest"),
    ("assignment", "criteria", "sourceRef"), ("assignment", "criteria", "count"),
)

# The three answers a host gives for a participant created without a turn. They are matched by name
# because each of them is a successful call reporting that there is nothing to read.
LIFECYCLE_REFUSALS = ("thread not found", "missing source rollout", "no rollout found")

VERIFIED = "verified"
NOT_VERIFIED = "not_verified"
UNKNOWN = "unknown"
NOT_APPLICABLE = "not_applicable"

EXECUTED = "executed"
READ = "read"
CAPTURED = "captured"

LEDGER_KINDS = ("segment_start", "segment_end", "window_open", "window_close", "intervention")
PREPARATION = "preparation"
WINDOW = "window"

# A reading that was never taken, kept distinct from a reading whose answer was None. Both are
# falsy and only one of them is an answer.
MISSING = object()

STARTED = time.time()
ACTOR = "trial_startup.py pid " + str(os.getpid())


class Refused(Exception):
    """The run cannot be made at all. It prints why and produces no result document."""

    def __init__(self, reason, **detail):
        super().__init__(reason)
        self.reason = reason
        self.detail = detail

    def to_record(self):
        return {"source": SOURCE, "checkerVersion": CHECKER_VERSION, "refused": self.reason,
                "detail": self.detail}


def moment(value, what):
    """One timestamp, or a refusal naming what could not be read as one."""
    if not isinstance(value, str) or not value.strip():
        raise Refused(what + " is not a timestamp", value=value)
    text = value.strip()
    if text.endswith("Z"):
        text = text[:-1] + "+00:00"
    try:
        parsed = datetime.datetime.fromisoformat(text)
    except ValueError as error:
        raise Refused(what + " is not an ISO-8601 timestamp",
                      value=value, detail=str(error)) from error
    if parsed.tzinfo is None:
        parsed = parsed.replace(tzinfo=datetime.timezone.utc)
    return parsed


def maybe_moment(value):
    """A timestamp where one is optional. None says it could not be read, never a default."""
    try:
        return moment(value, "a time")
    except Refused:
        return None


def stamp(when=None):
    at = datetime.datetime.fromtimestamp(when if when is not None else time.time(),
                                         datetime.timezone.utc)
    return at.isoformat().replace("+00:00", "Z")


def finite(value):
    """Whether this number is finite, for a JSON number of any size.

    Python keeps an integer literal at arbitrary precision and math.isfinite converts to float,
    which raises on one too large to represent. An integer is finite whatever its size.
    """
    if isinstance(value, int):
        return True
    try:
        return math.isfinite(value)
    except (OverflowError, TypeError, ValueError):
        return False


def number(value, what, *, minimum, maximum=None):
    """A real, finite number in range, or a refusal.

    NaN is the reason this exists rather than an inline comparison: every comparison with it is
    false, so it passes a range check from both sides at once and then makes every staleness
    comparison after it false as well. A bool is rejected for a duller reason: it is an int here
    and a value nobody meant as a duration.
    """
    if isinstance(value, bool) or not isinstance(value, (int, float)):
        raise Refused(what + " must be a number", value=shown(value))
    if not finite(value):
        raise Refused(what + " must be a finite number", value=str(value))
    if value < minimum or (maximum is not None and value > maximum):
        raise Refused(what + " is out of range", value=value, minimum=minimum, maximum=maximum)
    return value


def absolute(value, what):
    if not isinstance(value, str) or not value.startswith("/"):
        raise Refused(what + " must be an absolute path", value=value)
    return Path(value)


def canonical(value, what):
    """An absolute, normalised POSIX path, which is the contract the relay's manifest enforces.

    An absolute path is not enough: the relay's own normalize_declared_path refuses a path holding
    .. or . segments, a trailing slash, a doubled separator or a ~, and a gate that accepted one
    would report a dispatch as ready that emit then refuses.
    """
    path = absolute(value, what)
    text = str(value)
    if ("\x00" in text or "~" in text or text != os.path.normpath(text)
            or (text.endswith("/") and text != "/") or "//" in text):
        raise Refused(what + " is not a normalised absolute path", value=text,
                      normalised=os.path.normpath(text))
    if any(character.isspace() for character in text):
        # The relay would accept it. This procedure cannot: the dispatch message names each
        # identity as a word of its own, and a path holding whitespace is never one word.
        raise Refused(what + " holds whitespace, which the dispatch message cannot name as a"
                             " word", value=text)
    return path


def symlink_component(path):
    """The first component of this path that exists and is a symbolic link, or None.

    Lexical containment says the declared string starts beneath an authorised root; it says nothing
    about the components. The relay opens each of them refusing to follow a link, so an artifact
    under a symlinked directory is refused at emit however the string reads. Components that do not
    exist yet are not an answer either way: the artifact itself is usually written by the child
    after this runs.
    """
    here = Path("/")
    for part in Path(str(path)).parts[1:]:
        here = here / part
        try:
            if os.path.islink(str(here)):
                return str(here)
        except OSError:
            return None
    return None


def resolve(path):
    """The path after every symbolic link, because containment compares places and not spellings."""
    return Path(str(path)).expanduser().resolve()


def within(child, parent):
    # A relative child would be resolved against whatever directory this process happens to be in,
    # so containment would answer about a path nobody named. Only an absolute one is a place.
    if not str(child).startswith("/"):
        return False
    try:
        return resolve(child).is_relative_to(resolve(parent))
    except (OSError, ValueError):
        return False


def lexically_within(child, parent):
    """Containment the way the relay decides it for an artifact root: on the path, not the place.

    The relay compares normalised POSIX paths and never resolves symbolic links, and then opens
    every component with O_NOFOLLOW. So an absolute symlink outside a root whose target resolves
    inside it is inside the root by resolution and outside it by the contract that actually
    decides the manifest. A gate resolving both sides approved artifacts emit then refused.

    Private-record containment keeps using the resolved comparison, where following the link is
    exactly the escape worth catching.
    """
    here, root = str(child), str(parent)
    if not here.startswith("/") or not root.startswith("/"):
        return False
    here, root = os.path.normpath(here), os.path.normpath(root)
    return here == root or here.startswith(root.rstrip("/") + "/")


def git_worktree_of(path):
    """The nearest directory holding a .git, or None. Operational state lives in neither."""
    here = resolve(path)
    for candidate in (here, *here.parents):
        if (candidate / ".git").exists():
            return candidate
    return None


def field(payload, *path):
    """The value at this path, or MISSING. MISSING is not None: only one of them is an answer."""
    found = payload
    for key in path:
        if isinstance(found, dict) and key in found:
            found = found[key]
        elif isinstance(found, list) and isinstance(key, int) and -len(found) <= key < len(found):
            found = found[key]
        else:
            return MISSING
    return found


def carries(payload, *path):
    """Whether this path holds something truthy. MISSING is an object and objects are truthy, so
    a predicate testing field(...) directly reads every absent key as present. That is not a
    hypothetical: it read a healthy lifecycle capture with no error key as one carrying an error."""
    found = field(payload, *path)
    return found is not MISSING and bool(found)


def shown(value):
    """A value on its way into evidence or into the document. MISSING never travels there.

    The sentinel is an object, so json.dumps raises on it and str() prints its address. Both are
    ways for a reading nobody took to reach a reader as though it were one that was.
    """
    if value is MISSING:
        return None
    if isinstance(value, dict):
        return {k: shown(v) for k, v in value.items()}
    if isinstance(value, list):
        return [shown(v) for v in value]
    return value


def same(left, right):
    """Equality where neither side may be absent or null.

    Two absent values compare equal to each other, which is how a payload carrying no relationship
    id agreed with a record carrying none either. Absence is never agreement.
    """
    if left is MISSING or right is MISSING or left is None or right is None:
        return False
    return str(left) == str(right)


def structurally_same(left, right):
    """Recursive equality that does not let a bool be a number.

    Python's container equality recurses through ordinary ==, and a bool is an int there, so
    {"networkAccess": False} equalled {"networkAccess": 0}.
    """
    if isinstance(left, bool) or isinstance(right, bool):
        return left is right
    if isinstance(left, dict) and isinstance(right, dict):
        return (set(left) == set(right)
                and all(structurally_same(left[key], right[key]) for key in left))
    if isinstance(left, list) and isinstance(right, list):
        return (len(left) == len(right)
                and all(structurally_same(a, b) for a, b in zip(left, right)))
    if isinstance(left, (dict, list)) or isinstance(right, (dict, list)):
        return False
    if isinstance(left, (int, float)) and isinstance(right, (int, float)):
        # JSON has one number type and Python's decoder does not: 1 and 1.0 are the same value
        # written twice, while False is still not zero because bools were settled above.
        return left == right
    return type(left) is type(right) and left == right


def same_value(left, right):
    """Equality for a value that may be structured, where str() is not an answer.

    A settings value can be an object, and comparing two of them as text makes agreement depend on
    key order while letting a string holding a dict's repr equal the dict itself. Scalars keep the
    textual comparison, because a payload legitimately answers 1 where a record wrote "1".
    """
    if left is MISSING or right is MISSING or left is None or right is None:
        return False
    if isinstance(left, (dict, list)) or isinstance(right, (dict, list)):
        return structurally_same(left, right)
    if isinstance(left, bool) or isinstance(right, bool):
        return left is right
    return str(left) == str(right)


def digest_of(path):
    reader = hashlib.sha256()
    with open(str(path), "rb") as handle:
        for block in iter(lambda: handle.read(65536), b""):
            reader.update(block)
    return reader.hexdigest()



# ------------------------------------------------------------------ the start record


def entry_point(environment=None):
    """The relay entry point the runtime host record names, read from outside the start record.

    The host record's own location comes from the environment rather than from anything the caller
    hands this command, and the runtime is reached through the owned pointer rather than through
    PATH or a checkout. So the start record can name a path and cannot choose a program: the path
    it names is only accepted when it equals one of these.

    What this establishes is agreement with the installed-runtime record, not the provenance of the
    bytes. That record is a private file the same operator writes, and this module says so rather
    than reading the comparison as proof of what the launcher is.
    """
    console = RELAY_COMPONENT
    try:
        for component in definition.load()["components"]:
            if component["component"] == RELAY_COMPONENT:
                console = component["consoleScript"]
                break
    except (OSError, ValueError, LookupError, TypeError) as error:
        raise Refused("the component definition could not be read",
                      detail=type(error).__name__ + ": " + str(error)) from error

    path = hostrecord.record_path(environment)
    found = reading.read_json(path, "the host record")
    if not found.usable:
        raise Refused("the runtime host record could not be read", path=str(path),
                      state=found.state, detail=found.detail)
    if found.state == reading.ABSENT or not isinstance(found.value, dict):
        raise Refused("there is no runtime host record, so no installed relay is named here",
                      path=str(path))
    installs = field(found.value, "components", RELAY_COMPONENT, "installs")
    if installs is MISSING or not isinstance(installs, list) or not installs:
        raise Refused("the host record names no install for " + RELAY_COMPONENT, path=str(path))
    locations = [i.get("location") for i in installs if isinstance(i, dict) and i.get("location")]
    return {"hostRecord": str(path), "consoleScript": console, "locations": locations,
            "pointers": [str(Path(location) / POINTER_NAME / "bin" / console)
                         for location in locations]}


def anchored_launcher(record, environment=None):
    """The launcher, accepted only where the host record already names it."""
    declared = absolute(field(record, "relay", "launcher"), "relay.launcher")
    named = entry_point(environment)
    # Lexically, against the pointer as the host record spells it, and the pointer is what runs.
    # A resolved comparison accepted a symlink that resolved to the pointer, and the probes then
    # executed that symlink: replacing it after the digest check, during the witness delay for
    # instance, would have run another program under this command's guarantee.
    matched = next((pointer for pointer in named["pointers"]
                    if os.path.normpath(str(declared)) == os.path.normpath(pointer)), None)
    if matched is None:
        raise Refused("the launcher is not the entry point the host record names",
                      launcher=str(declared), named=named["pointers"],
                      hostRecord=named["hostRecord"])
    declared = Path(matched)
    if not declared.is_file():
        raise Refused("the launcher is not a regular file", launcher=str(declared))
    if within(declared, field(record, "trialRoot")):
        raise Refused("the launcher is inside the trial root", launcher=str(declared))
    expected = field(record, "relay", "launcherSha256")
    try:
        actual = digest_of(declared)
    except OSError as error:
        raise Refused("the launcher could not be read", launcher=str(declared),
                      detail=type(error).__name__ + ": " + str(error)) from error
    if actual != expected:
        raise Refused("the launcher's bytes changed since the record was written",
                      launcher=str(declared), recorded=expected, measured=actual)
    return {"launcher": str(declared), "sha256": actual, "hostRecord": named["hostRecord"],
            "namedBy": named["pointers"]}


def load_start(path, *, environment=None, mode="preflight"):
    """Read the record, or refuse. Nothing after this is reached on a record that cannot be used."""
    start = absolute(path, "--start")
    found = reading.read_json(start, "the start record")
    if not found.usable or found.state == reading.ABSENT:
        raise Refused("the start record could not be read", path=str(start), state=found.state,
                      detail=found.detail)
    record = found.value
    if not isinstance(record, dict):
        raise Refused("the start record is not a JSON object", path=str(start))
    if record.get("source") != "live-trial-start":
        raise Refused("this file does not stamp itself as a live trial start record",
                      path=str(start), source=record.get("source"))
    if record.get("recordVersion") != RECORD_VERSION:
        raise Refused("unsupported record version", path=str(start),
                      recordVersion=record.get("recordVersion"))

    trial_root = absolute(record.get("trialRoot"), "trialRoot")
    if not trial_root.is_dir():
        raise Refused("the trial root is not a directory", trialRoot=str(trial_root))
    worktree = git_worktree_of(trial_root)
    if worktree is not None:
        raise Refused("the trial root is inside a git worktree, and operational state never lives"
                      " inside a repository", trialRoot=str(trial_root), worktree=str(worktree))
    if not within(start, trial_root):
        raise Refused("the start record itself is outside the trial root", path=str(start),
                      trialRoot=str(trial_root))

    # The ledger is a private trial record like the captures, and it is read by path, so it is
    # confined the same way rather than followed wherever a link points.
    record["_ledger"] = trial_root / "ledger.jsonl"
    if not within(record["_ledger"], trial_root):
        raise Refused("the ledger resolves outside the trial root",
                      path=str(record["_ledger"]), trialRoot=str(trial_root))

    # Every private trial record, the start record and the ledger among them: the rule is that
    # operational state is in no repository, and a check that exempted the two paths this command
    # is given would leave exactly those two inside one.
    for item in (start, record["_ledger"]):
        nested = git_worktree_of(item)
        if nested is not None:
            raise Refused("a private trial record is inside a git worktree", path=str(item),
                          worktree=str(nested))

    if mode == "ledger":
        # Grading a ledger uses the trial root, the window and nothing else. Requiring the
        # installed relay and every capture here made a finished trial ungradable as soon as the
        # installation it ran against changed, which is a fact about afterwards.
        record["_start"] = str(start)
        record["_relay"] = None
        return record

    age = number(record.get("captureMaxAgeSeconds"), "captureMaxAgeSeconds",
                 minimum=1, maximum=CAPTURE_AGE_CEILING)
    number(field(record, "supervisor", "witnessAdvanceSeconds"),
           "supervisor.witnessAdvanceSeconds", minimum=0, maximum=ADVANCE_CEILING)

    # Every fact a predicate later compares against, required here rather than where it is read.
    # A record that declares nothing and a payload that carries nothing agree with each other, and
    # a reading assembled from two absences reports a precondition nobody established.
    for path in REQUIRED_FIELDS:
        found = field(record, *path)
        if found is MISSING or found is None or (isinstance(found, str) and not found.strip()):
            raise Refused("the start record does not state " + ".".join(str(p) for p in path),
                          value=shown(found))
    # Operational state never lives inside a repository (OPS-3.2), and this one would be reached by
    # every relay probe: a store placed in this checkout would be constructed by the first command
    # that opened it.
    state = absolute(field(record, "relay", "stateDirectory"), "relay.stateDirectory")
    absolute(field(record, "relay", "socket"), "relay.socket")
    state_worktree = git_worktree_of(state)
    if state_worktree is not None:
        raise Refused("the relay state directory is inside a git worktree",
                      stateDirectory=str(state), worktree=str(state_worktree))
    if not isinstance(field(record, "assignment", "artifacts"), list):
        raise Refused("assignment.artifacts must be a list")
    for artifact in field(record, "assignment", "artifacts"):
        canonical(artifact, "an assignment artifact")
    if not isinstance(field(record, "captures"), dict):
        raise Refused("captures must be an object of capture kinds",
                      captures=type(record.get("captures")).__name__)
    if not isinstance(record.get("boundaries"), list) or len(record["boundaries"]) < 2:
        raise Refused("a live trial declares at least two boundaries",
                      boundaries=len(record.get("boundaries") or []))
    for index, boundary in enumerate(record["boundaries"]):
        # Before anything reads a boundary's fields, because a non-object one reached a generic
        # failure and reported an internal fault rather than the malformed record it was.
        if not isinstance(boundary, dict):
            raise Refused("a boundary is not an object", index=index,
                          boundary=type(boundary).__name__)
    # The assignment being dispatched has to belong to a boundary this record declared. Falling
    # back to the first one read the wrong boundary's registration and let an assignment outside
    # every declared repository and project pass every reading.
    owning = [b for b in record["boundaries"]
              if b.get("issueKey") == field(record, "assignment", "issueKey")]
    if not owning:
        raise Refused("the assignment's issue belongs to no declared boundary",
                      issueKey=field(record, "assignment", "issueKey"),
                      boundaries=[b.get("issueKey") for b in record["boundaries"]])
    # And its participants have to be that boundary's own, or the readings would cover one set of
    # tasks while the dispatch went to another.
    # Every boundary, not only the one owning the dispatch: all of them are this trial's evidence,
    # and a repeated role made this check read the last participant while the boundary reading read
    # the first.
    for boundary in record["boundaries"]:
        people = [p for p in (boundary.get("participants") or []) if isinstance(p, dict)]
        for role in ("parent", "child"):
            named = [p.get("taskId") for p in people if p.get("role") == role]
            if len(named) != 1:
                raise Refused("a boundary declares exactly one " + role,
                              boundary=boundary.get("name"), found=shown(named))
    roles = {p.get("role"): p.get("taskId")
             for p in (owning[0].get("participants") or []) if isinstance(p, dict)}
    for role, key in (("parent", "parentTaskId"), ("child", "childTaskId")):
        if not same(roles.get(role), field(record, "assignment", key)):
            raise Refused("the assignment's " + role + " is not the one its boundary declares",
                          boundary=owning[0].get("name"), declared=shown(roles.get(role)),
                          assignment=shown(field(record, "assignment", key)))
    launched = moment(field(record, "supervisor", "launchedAt"), "supervisor.launchedAt")
    if launched.timestamp() > time.time():
        raise Refused("the supervisor's launchedAt is in the future",
                      launchedAt=field(record, "supervisor", "launchedAt"))
    # A preflight runs immediately before the dispatch that opens the window, so the whole window
    # is required and it has to be ahead: a record whose window has opened is a trial already
    # running, and starting from it again dispatches a second time into one measured interval.
    window = field(record, "window")
    if not isinstance(window, dict):
        raise Refused("the start record declares no window", window=shown(window))
    opens = moment(window.get("opensAt"), "window.opensAt")
    closes = moment(window.get("closesAt"), "window.closesAt")
    if closes <= opens:
        raise Refused("this record's window has no duration",
                      opensAt=window.get("opensAt"), closesAt=window.get("closesAt"))
    if opens.timestamp() < time.time():
        raise Refused("this record's trial window has already opened, so it is not a record to"
                      " start from", opensAt=window.get("opensAt"), now=stamp())
    if opens.timestamp() > time.time() + WINDOW_ALLOWANCE:
        raise Refused("this record's trial window opens too long after this preflight to be the"
                      " dispatch it precedes", opensAt=window.get("opensAt"), now=stamp(),
                      allowanceSeconds=WINDOW_ALLOWANCE)
    minimum_alive = field(record, "supervisor", "minimumAliveSeconds")
    number(minimum_alive, "supervisor.minimumAliveSeconds", minimum=0)
    if minimum_alive <= 0:
        # A bound of zero is satisfied by a process that started this instant, so the reading
        # would report persistence it never observed. The observation this exists for is a
        # supervisor that outlived the shell which launched it, so the bound has to be long
        # enough for that to have happened.
        raise Refused("supervisor.minimumAliveSeconds must be greater than zero",
                      minimumAliveSeconds=shown(minimum_alive))
    pid = field(record, "supervisor", "pid")
    if isinstance(pid, bool) or not isinstance(pid, int) or pid <= 0:
        # A float pid is signalled as its truncated self while the witness is compared against the
        # original, so a witness claiming 5775.5 answered for whatever 5775 is doing.
        raise Refused("supervisor.pid must be a positive integer", pid=shown(pid))

    # Everything the trial writes for itself is confined to the trial root. The assignment file and
    # the dispatch message belong to the child's workspace and are elsewhere by nature, so the rule
    # for those two is only that they are not inside this repository.
    private = [absolute(field(record, "supervisor", "witness"), "supervisor.witness")]
    for kind, entries in sorted((record.get("captures") or {}).items()):
        if not isinstance(entries, dict):
            raise Refused("captures." + kind + " is not an object of captures")
        for name, entry in sorted(entries.items()):
            what = "captures." + kind + "." + name
            if not isinstance(entry, dict):
                raise Refused(what + " is not a capture", entry=type(entry).__name__)
            private.append(absolute(field(entry, "path"), what + ".path"))
            moment(field(entry, "capturedAt"), what + ".capturedAt")
    for item in private:
        if not within(item, trial_root):
            raise Refused("a private trial record is outside the trial root", path=str(item),
                          trialRoot=str(trial_root))
        # And not inside a worktree nested under the trial root: the rule is that operational
        # state is in no repository, not that the root itself is in none.
        nested = git_worktree_of(item)
        if nested is not None:
            raise Refused("a private trial record is inside a git worktree", path=str(item),
                          worktree=str(nested))
    for key in ("assignmentFile", "dispatchMessageFile"):
        item = absolute(field(record, "assignment", key), "assignment." + key)
        if within(item, ROOT):
            raise Refused("an input path is inside this repository", path=str(item),
                          repository=str(ROOT))
    # And the assignment file is the one in the owning child's workspace, not a matching decoy
    # somewhere else: the file under test is the file that child reads.
    child_cwd = next((p.get("cwd") for p in (owning[0].get("participants") or [])
                      if isinstance(p, dict) and p.get("role") == "child"), None)
    assignment_file = absolute(field(record, "assignment", "assignmentFile"),
                               "assignment.assignmentFile")
    if child_cwd is None or not within(assignment_file, child_cwd):
        raise Refused("the assignment file is not in the owning child's workspace",
                      path=str(assignment_file), workspace=shown(child_cwd))

    for index, boundary in enumerate(record.get("boundaries") or []):
        if not isinstance(boundary, dict) or not isinstance(boundary.get("participants"), list):
            raise Refused("a boundary carries no participants", index=index)
        for participant in boundary.get("participants") or []:
            expect = participant.get("expect")
            if not isinstance(expect, dict) or any(k not in expect for k in REQUIRED_EXPECT):
                raise Refused("a participant's expect does not name every required setting",
                              boundary=boundary.get("name"), taskId=participant.get("taskId"),
                              required=list(REQUIRED_EXPECT))
            # The relay records a sandbox as the policy object and reads its type out of it, so a
            # mode on its own is a value the store can never hold and this record can never agree
            # with. Refused here, where the operator can still fix it, rather than at a cell.
            sandbox = expect.get("sandbox")
            if not isinstance(sandbox, dict) or not isinstance(sandbox.get("type"), str):
                raise Refused("a participant's expected sandbox must be the policy object the"
                              " relay records, carrying its mode at \"type\"",
                              boundary=boundary.get("name"), taskId=participant.get("taskId"),
                              sandbox=shown(sandbox))
            if not str(participant.get("taskId") or "").strip():
                raise Refused("a participant has no task id", boundary=boundary.get("name"))
            absolute(participant.get("cwd"), "a participant cwd")
        absolute(boundary.get("repositoryRoot"), "a boundary repositoryRoot")
        if not boundary.get("name"):
            raise Refused("a boundary has no name to key its registration receipt by",
                          index=index)

    record["_start"] = str(start)
    record["_relay"] = anchored_launcher(record, environment)
    return record



# ------------------------------------------------------------------------ probes


def run(argv, *, cwd, timeout=60, environment=None):
    """One subprocess, and every way it can fail becoming a field instead of an exception."""
    at = time.time()
    try:
        done = subprocess.run([str(a) for a in argv], cwd=str(cwd), capture_output=True,
                              text=True, timeout=timeout, env=environment)
    except (OSError, subprocess.SubprocessError) as error:
        return {"argv": [str(a) for a in argv], "exitCode": None, "payload": None,
                "stdout": "", "measuredAt": stamp(at),
                "detail": type(error).__name__ + ": " + str(error)}
    payload = None
    try:
        parsed = json.loads(done.stdout)
        payload = parsed if isinstance(parsed, dict) else None
    except ValueError:
        payload = None
    return {"argv": [str(a) for a in argv], "exitCode": done.returncode, "payload": payload,
            "stdout": done.stdout.strip()[-400:], "measuredAt": stamp(at),
            "detail": None if payload is not None else "the output was not a JSON object"}


class Relay:
    """The only two programs this module starts, and the only argv it will carry.

    The subcommand is checked against the allowlist before the process exists, so a subcommand that
    is not on it is refused rather than run and judged.
    """

    def __init__(self, record):
        self.launcher = record["_relay"]["launcher"]
        self.state = field(record, "relay", "stateDirectory")
        self.socket = field(record, "relay", "socket")
        self.cwd = field(record, "trialRoot")
        self.digests = set()
        # OPS-3.3: the flag moves the store and the environment moves the adapter's ledger, so a
        # participant sets both. Setting only the flag made a colocated ledger read as split.
        self.environment = dict(os.environ, CODEX_SESSION_RELAY_STATE=str(self.state))

    def relay(self, subcommand, *arguments):
        if subcommand == "service":
            if not arguments or arguments[0] not in SERVICE_VERBS:
                raise Refused("service is admitted with status and nothing else",
                              verb=arguments[0] if arguments else None)
        elif subcommand not in RELAY_SUBCOMMANDS:
            raise Refused("this relay subcommand is not one this module composes",
                          subcommand=subcommand)
        argv = [self.launcher, "--state", self.state, "--socket", self.socket, subcommand]
        argv.extend(arguments)
        # The launcher's bytes are read immediately before each spawn, so a pointer moved between
        # two probes is caught at the next one rather than only at the end of the run.
        self.digests.add(digest_or_none(self.launcher))
        return run(argv, cwd=self.cwd, environment=self.environment)

    @staticmethod
    def git(cwd, *arguments):
        if not arguments or arguments[0] != "rev-parse":
            raise Refused("git is composed as rev-parse and nothing else",
                          subcommand=arguments[0] if arguments else None)
        return run(["git", "-C", str(cwd), *arguments], cwd=cwd)


def cell(name, value, *, evidence, provenance, probe=None, detail=None, measured_at=None):
    """One reading in the OPS-6.2 shape, with the boolean the judgment walk collects.

    measuredAt is when the observation was actually made, or unknown. A plausible time is never
    supplied to satisfy the shape and another cell's time is never copied into this one.
    """
    command = None
    if probe is not None:
        command = " ".join(probe["argv"])
        measured_at = measured_at or probe.get("measuredAt")
    answer = check.field(value, evidence, command=command, acting_process=ACTOR,
                         measured_at=measured_at or "unknown")
    answer["cell"] = name
    answer["provenance"] = provenance
    answer["met"] = value in (VERIFIED, NOT_APPLICABLE)
    if probe is not None:
        answer["exitCode"] = probe["exitCode"]
    if detail:
        answer["detail"] = detail
    return answer


def graded(name, found, ok, *, evidence, provenance, probe=None, unreadable, detail=None,
           measured_at=None):
    """The one place a cell decides between unknown and an answer.

    MISSING means the payload does not carry the field this predicate reads, which is unknown
    whatever the exit status was. Anything else is an answer, and false is reserved for it.
    """
    if found is MISSING:
        return cell(name, UNKNOWN, evidence=unreadable, provenance=provenance, probe=probe,
                    detail=detail, measured_at=measured_at)
    return cell(name, VERIFIED if ok else NOT_VERIFIED, evidence=evidence, provenance=provenance,
                probe=probe, detail=detail, measured_at=measured_at)


def capture(record, kind, name):
    """A payload recorded elsewhere, with its own age. It is never read as a live observation."""
    entry = field(record, "captures", kind, name)
    if entry is MISSING:
        return None, "no capture is declared for " + kind + " " + name
    path = field(entry, "path")
    taken = moment(field(entry, "capturedAt"), "captures." + kind + "." + name + ".capturedAt")
    # The clock is read here rather than once before the run, because a witness delay and several
    # subprocesses sit between the first capture and the last, and a bound sampled at the start
    # would call a capture fresh for as long as the run happened to take.
    now = datetime.datetime.now(datetime.timezone.utc)
    if taken > now:
        raise Refused("a capture is dated in the future", kind=kind, name=name,
                      capturedAt=stamp(taken.timestamp()), now=stamp(now.timestamp()))
    age = (now - taken).total_seconds()
    if age > record["captureMaxAgeSeconds"]:
        return None, ("the capture is " + str(int(age)) + " seconds old, past the record's bound of "
                      + str(int(record["captureMaxAgeSeconds"])))
    # Remembered so the run can age them all again at the end: a capture fresh when it was read can
    # expire during the witness delay and the probes that follow it.
    record.setdefault("_captures", []).append({"kind": kind, "name": name, "at": taken})
    found = reading.read_json(path, "a captured payload")
    if not found.usable or found.state == reading.ABSENT or not isinstance(found.value, dict):
        return None, "the capture at " + str(path) + " could not be read (" + str(found.state) + ")"
    return {"payload": found.value, "path": str(path), "capturedAt": stamp(taken.timestamp())}, None


# ---------------------------------------------------------------------- readings


def registration_identity(record, boundary, receipt):
    """Whether this receipt is the registration this boundary is running under.

    A receipt agreeing on a scope and two task names while carrying another issue, another
    relationship, another generation or an archived status is a stale registration, and the
    authorised roots inside it belong to that other assignment.
    """
    assignment = record.get("assignment") or {}
    this_one = boundary.get("issueKey") == assignment.get("issueKey")
    for path in (("issueKey",), ("status",), ("authorizedScope", "allowedRecipients")):
        # A field the receipt does not carry is a reading nobody took, not a registration that
        # disagrees, and the cell above has to be able to tell them apart.
        if field(receipt, *path) is MISSING:
            return MISSING, "it does not carry " + ".".join(path)
    if this_one:
        for name in ("relationshipId", "executionGeneration"):
            if field(receipt, name) is MISSING:
                return MISSING, "it does not carry " + name
    if not same(field(receipt, "issueKey"), boundary.get("issueKey")):
        return False, "it names issue " + str(shown(field(receipt, "issueKey")))
    if field(receipt, "status") != "active":
        return False, "its status is " + str(shown(field(receipt, "status")))
    # Both endpoints have to be allowed recipients of this registration. The relay delivers only
    # to a recipient recorded here (OPS-7.3), so a registration authorising somebody else is one
    # under which this trial's completion and correction have nowhere to go.
    allowed = field(receipt, "authorizedScope", "allowedRecipients")
    if not isinstance(allowed, list):
        return False, "it records no allowed recipients"
    for role in ("parent", "child"):
        task = next((p.get("taskId") for p in boundary.get("participants") or []
                     if p.get("role") == role), None)
        if task is not None and not any(same(entry, task) for entry in allowed):
            return False, "its allowed recipients do not include the " + role
    if this_one:
        if not same(field(receipt, "relationshipId"), assignment.get("relationshipId")):
            return False, "it names relationship " + str(shown(field(receipt, "relationshipId")))
        if not same(field(receipt, "executionGeneration"),
                    assignment.get("executionGeneration")):
            return False, ("it names generation "
                           + str(shown(field(receipt, "executionGeneration"))))
    return True, "it is this boundary's current registration"


def read_witness(path):
    """The supervisor's last line: the pid it claims and the counter it advances."""
    found = reading.read_text(path, "the supervisor witness")
    if not found.usable or found.state == reading.ABSENT:
        return None
    lines = [line for line in (found.value or "").splitlines() if line.strip()]
    if not lines:
        return None
    try:
        last = json.loads(lines[-1])
    except ValueError:
        return None
    return last if isinstance(last, dict) else None


def alive(pid):
    try:
        os.kill(int(pid), 0)
        return True
    except ProcessLookupError:
        return False
    except (PermissionError, OverflowError, TypeError, ValueError):
        # A process this user may not signal still exists; anything unreadable is not an answer.
        return None


def session_of(pid):
    try:
        return os.getsid(int(pid))
    except (ProcessLookupError, PermissionError, OSError, TypeError, ValueError):
        return None


def process_started_at(pid):
    """When this process itself started, or None where the host cannot say.

    The record declares when a supervisor was launched, and a restarted supervisor keeps the old
    declaration: a new process alive for two seconds then satisfied any minimum. The process's own
    start time is the one that belongs to the pid being observed.
    """
    try:
        return os.stat("/proc/" + str(int(pid))).st_ctime
    except (OSError, TypeError, ValueError):
        return None


def reading_process(record, relay, sleeper=time.sleep):
    """Alive, detached from this caller, and still making progress. Three questions, not one."""
    supervisor = record["supervisor"]
    pid = supervisor.get("pid")
    witness_path = supervisor.get("witness")
    seconds = supervisor.get("witnessAdvanceSeconds")
    cells = []

    first_alive, first_witness = alive(pid), read_witness(witness_path)
    sleeper(seconds)
    second_alive, second_witness = alive(pid), read_witness(witness_path)

    launched = moment(supervisor.get("launchedAt"), "supervisor.launchedAt")
    minimum = supervisor.get("minimumAliveSeconds")
    started = process_started_at(pid)
    if started is None:
        # No /proc here, which is most hosts that are not Linux. The record's declaration is what
        # is left, and the cell says which of the two it read rather than refusing every trial on
        # such a host or passing one off as the other.
        lived = time.time() - launched.timestamp()
        cells.append(cell("uptime", VERIFIED if lived >= minimum else NOT_VERIFIED,
                          provenance=CAPTURED, measured_at=stamp(),
                          evidence=("this host does not report when a process started, so the"
                                    " record's own launchedAt is what this reads: "
                                    + str(int(lived)) + " seconds against a declared minimum of "
                                    + str(minimum) + ". A restarted supervisor keeps that"
                                    " declaration, and there is nothing here that would notice")))
    else:
        lived = time.time() - started
        declared = time.time() - launched.timestamp()
        ok = lived >= minimum
        cells.append(cell("uptime", VERIFIED if ok else NOT_VERIFIED, provenance=READ,
                          evidence=("this process has been running " + str(int(lived)) + " seconds"
                                    " against a declared minimum of " + str(minimum)
                                    + ", and the record's launchedAt is " + str(int(declared))
                                    + " seconds ago. The process's own start time is the one that"
                                    " belongs to the pid, because a restarted supervisor keeps the"
                                    " old declaration"),
                          measured_at=stamp()))

    if first_alive is None or second_alive is None:
        cells.append(cell("alive", UNKNOWN, evidence="whether pid " + str(pid) + " exists could not"
                          " be established", provenance=READ))
    else:
        caller, theirs = os.getsid(0), session_of(pid)
        detached = theirs is not None and theirs != caller
        ok = first_alive and second_alive and detached
        cells.append(cell("alive", VERIFIED if ok else NOT_VERIFIED,
                          evidence=("pid " + str(pid) + " alive at both observations, session "
                                    + str(theirs) + " against this caller's " + str(caller)),
                          provenance=READ, measured_at=stamp()))

    if first_witness is None or second_witness is None:
        cells.append(cell("witnessAdvance", UNKNOWN,
                          evidence="the witness at " + str(witness_path) + " could not be read as a"
                          " JSON line carrying a pid and a progress counter", provenance=READ))
    else:
        named = same(first_witness.get("pid"), pid) and same(second_witness.get("pid"), pid)
        before, after = first_witness.get("progress"), second_witness.get("progress")
        # A bool is an int here, and False to True is not progress anybody made.
        advanced = (isinstance(before, (int, float)) and isinstance(after, (int, float))
                    and not isinstance(before, bool) and not isinstance(after, bool)
                    and finite(before) and finite(after)
                    and after > before)
        cells.append(cell("witnessAdvance", VERIFIED if (named and advanced) else NOT_VERIFIED,
                          evidence=("the witness names pid " + str(first_witness.get("pid"))
                                    + " and its counter went " + str(before) + " to " + str(after)
                                    + " across " + str(seconds) + " seconds"),
                          provenance=READ, measured_at=stamp()))

    if not supervisor.get("service"):
        cells.append(cell("service", NOT_APPLICABLE,
                          evidence="this trial supervises a bounded run of its own rather than the"
                          " relay's service, so there is no service record to read",
                          provenance=READ))
    else:
        probe = relay.relay("service", "status")
        payload = probe["payload"] or {}
        lock = field(payload, "lock")
        ok = (lock == "held" and field(payload, "staleRecord") is False
              and same(field(payload, "pid"), pid)
              and field(payload, "ownership") == "ours"
              and same(field(payload, "storeId"), field(record, "store", "storeId")))
        # Answerable only where every field its predicate reads is there. A status carrying a
        # lock and nothing else never said whose it was, and a disagreement would say it did.
        absent = [name for name, value in (("a lock", lock),
                                           ("staleRecord", field(payload, "staleRecord")),
                                           ("a pid", field(payload, "pid")),
                                           ("ownership", field(payload, "ownership")),
                                           ("a store id", field(payload, "storeId")))
                  if value is MISSING]
        cells.append(graded("service", MISSING if absent else lock, ok, probe=probe,
                            provenance=EXECUTED,
                            unreadable="service status did not report " + ", ".join(absent),
                            evidence=("lock " + str(lock) + ", ownership "
                                      + str(shown(field(payload, "ownership"))) + ", pid "
                                      + str(shown(field(payload, "pid"))) + ", staleRecord "
                                      + str(shown(field(payload, "staleRecord"))))))
    return cells


def status_state(value):
    """The state a lifecycle status carries, or MISSING when nothing there is one.

    Two shapes reach this reading. A goal status is a word. A thread status is the object the
    host returns, and its state lives at "type": the relay reads it at exactly that key, in
    bridge_adapter.read_thread and again in the receipt's own thread check, so this reads it
    there too instead of accepting whichever object turned up. {"unexpected": true} is a nonempty
    object that says nothing about a lifecycle, and taking it for a status resolved a participant
    the host never resolved.

    JSON false, 0, a null and a list are not statuses at all, and str() turns the first two into
    the nonempty words "False" and "0". A bool is an int in Python, so nothing here may fall back
    on truthiness or on str() to tell a status from a value that is not one.
    """
    if isinstance(value, str):
        return value.strip() or MISSING
    if isinstance(value, dict):
        found = value.get("type")
        if isinstance(found, str) and found.strip():
            return found.strip()
    return MISSING


def reading_lifecycle(record):
    """Per participant, a captured host response. A creation receipt is not one of these.

    Every participant, not only the parents: a child created without a standby turn cannot be
    looked up either, and a correction is delivered to it.
    """
    cells = []
    for boundary in record.get("boundaries") or []:
        for participant in boundary.get("participants") or []:
            task = participant.get("taskId")
            found, why = capture(record, "parentLifecycle", task)
            if found is None:
                cells.append(cell("lifecycle:" + str(task), UNKNOWN, evidence=why,
                                  provenance=CAPTURED))
                continue
            payload = found["payload"]
            # Only the fields a host puts a refusal in. Searching the whole payload marked a
            # healthy participant as refused because a goal or a title said those words.
            said = " ".join(str(shown(field(payload, *path))).lower()
                            for path in (("error",), ("error", "message"), ("message",),
                                         ("detail",), ("status",), ("status", "detail"))
                            if field(payload, *path) is not MISSING)
            refusal = next((text for text in LIFECYCLE_REFUSALS if text in said), None)
            status = field(payload, "status")
            # The host's own lifecycle answer carries status as a structured object, not a word.
            # A predicate insisting on a string failed every capture a real host produced, so both
            # shapes are read, and each is read where the state actually is.
            state = status_state(status)
            names = (same(field(payload, "threadId"), task)
                     or same(field(payload, "taskId"), task))
            if status is MISSING and refusal is None:
                cells.append(cell("lifecycle:" + str(task), UNKNOWN,
                                  evidence="the capture carries no thread status to read",
                                  provenance=CAPTURED, measured_at=found["capturedAt"]))
                continue
            if state is MISSING and refusal is None:
                # Not a disagreement: a value that is not a status is a reading nobody took, and
                # the start is refused for the same reason an absent capture refuses it.
                cells.append(cell("lifecycle:" + str(task), UNKNOWN,
                                  evidence=("the capture carries " + json.dumps(shown(status))
                                            + " where the thread status belongs, and no state can"
                                            " be read from it. A word, or the host's status"
                                            ' object carrying its state at "type", is what this'
                                            " resolves"),
                                  provenance=CAPTURED, measured_at=found["capturedAt"],
                                  detail=found["path"]))
                continue
            ok = (refusal is None and names
                  and not carries(payload, "error") and not carries(payload, "isError"))
            cells.append(cell("lifecycle:" + str(task), VERIFIED if ok else NOT_VERIFIED,
                              evidence=("the host resolved this thread with status "
                                        + json.dumps(shown(status))
                                        if refusal is None else
                                        "the host answered " + refusal + ", which is a participant"
                                        " with no rollout to read"),
                              provenance=CAPTURED, measured_at=found["capturedAt"],
                              detail=found["path"]))
    return cells



def reading_capability(record, relay):
    """Two halves per participant: what the host echoed, and what the store holds.

    Returns the settings row it graded for each participant as well, because the gate has to
    compare against the row as it is when the gate runs rather than this one.
    """
    cells = []
    seen = {}
    for boundary in record.get("boundaries") or []:
        for participant in boundary.get("participants") or []:
            task = participant.get("taskId")
            expect = participant.get("expect") or {}
            found, why = capture(record, "creationReceipt", task)
            actual = MISSING
            receipt = found["payload"] if found is not None else MISSING
            if found is None:
                cells.append(cell("receiptEcho:" + str(task), UNKNOWN, evidence=why,
                                  provenance=CAPTURED))
            else:
                actual = field(found["payload"], "settings", "actual")
                findings = field(found["payload"], "settings", "findings")
                if findings is MISSING:
                    findings = field(found["payload"], "findings")
                names = same(field(found["payload"], "taskId"), task)
                disagreed = [] if actual is MISSING else [
                    key for key, value in sorted(expect.items())
                    if not same_value(field(actual, key), value)
                ]
                ok = (names and not disagreed and isinstance(findings, list) and not findings
                      and actual is not MISSING)
                # Answerable only where every field its predicate reads is there. A receipt with
                # no findings key never said whether the host reported any, and grading it a
                # disagreement says it answered none.
                absent = [name for name, value in (("the settings the host echoed", actual),
                                                   ("its findings", findings),
                                                   ("the task it is about",
                                                    field(found["payload"], "taskId")))
                          if value is MISSING]
                cells.append(graded("receiptEcho:" + str(task), MISSING if absent else actual, ok,
                                    provenance=CAPTURED,
                                    measured_at=found["capturedAt"],
                                    unreadable="the receipt carries no " + ", no ".join(absent),
                                    evidence=("the host echoed every declared setting and reported"
                                              " no findings" if ok else
                                              "this receipt names task "
                                              + str(shown(field(found["payload"], "taskId")))
                                              + ", disagrees at " + ", ".join(disagreed)
                                              + " and reports findings "
                                              + json.dumps(shown(findings))),
                                    detail=found["path"]))

            probe = relay.relay("settings-show", "--task", task)
            payload = probe["payload"] or {}
            usable = field(payload, "usable")
            settings = field(payload, "settings")
            # The workspace the record already states for this participant, compared without being
            # declared twice: a settings record naming another cwd is the one delivery would use.
            wanted = dict(expect)
            if participant.get("cwd") is not None:
                wanted["cwd"] = participant.get("cwd")
            differs = [] if settings is MISSING or settings is None else [
                key for key, value in sorted(wanted.items())
                if not same_value(field(settings, key), value)
            ]
            # settings-show always reports missing, so an absent one is a payload this predicate
            # cannot read as complete rather than an empty list it may assume.
            answered = (MISSING if (settings is MISSING or usable is MISSING
                                    or field(payload, "missing") is MISSING
                                    or field(payload, "task") is MISSING) else usable)
            ok = (usable is True and field(payload, "missing") == [] and not differs
                  and settings not in (MISSING, None)
                  and same(field(payload, "task"), task))
            cells.append(graded("recordedSettings:" + str(task), answered, ok, probe=probe,
                                provenance=EXECUTED,
                                unreadable="settings-show did not report both whether the record is"
                                           " usable and the settings it holds",
                                evidence=("the store's settings are usable, complete and carry every"
                                          " declared value" if ok else
                                          "task " + str(shown(field(payload, "task"))) + ", usable "
                                          + str(shown(usable)) + ", missing "
                                          + json.dumps(shown(field(payload, "missing")))
                                          + ", disagreeing " + json.dumps(differs))))
            seen[str(task)] = settings

            # What the trial will actually run with, against what creation recorded. The four
            # declared settings say nothing about workspace access, so a record that is usable,
            # complete and agrees on all four can still hand delivery wider roots or another
            # environment than the receipt shows, and the trial starts with access nobody read.
            # Compared payload against payload rather than against a fifth declaration, because
            # neither side of it is the operator's to invent.
            unread = [key for key in DELIVERY_ACCESS
                      if unreadable_access(receipt_access(receipt, key))
                      or unreadable_access(field(settings, key))]
            apart = [key for key in DELIVERY_ACCESS
                     if not same_value(receipt_access(receipt, key), field(settings, key))]
            cells.append(graded("deliveryAccess:" + str(task),
                                MISSING if unread else field(settings, "cwd"), not apart,
                                probe=probe, provenance=EXECUTED,
                                unreadable=("creation and delivery cannot be compared on "
                                            + ", ".join(unread) + ": one of the two payloads does"
                                            " not carry it"),
                                evidence=("the store's settings and the creation receipt agree on "
                                          + ", ".join(DELIVERY_ACCESS) if not apart else
                                          "the store's settings disagree with the creation receipt"
                                          " at " + ", ".join(apart) + ", so this trial would run"
                                          " with access the receipt never recorded")))
    return cells, seen


def settings_now(record, relay, seen):
    """Every participant's settings row, read again immediately before the gate.

    The capability probes run before the witness delay and the probes after it, and delivery
    reloads the current row when it sends. A row replaced while those seconds passed left the
    trial running with settings the preflight never approved, and the cached cells stayed
    verified. Compared whole rather than field by field, because any change to the row is a
    change to what delivery will use.
    """
    cells = []
    for name in participants_of(record):
        probe = relay.relay("settings-show", "--task", name)
        payload = probe["payload"] or {}
        current = field(payload, "settings")
        before = seen.get(name, MISSING)
        readable = current is not MISSING and before is not MISSING
        agrees = readable and structurally_same(current, before)
        cells.append(graded("settingsStillCurrent:" + name,
                            field(payload, "task") if readable else MISSING, agrees,
                            probe=probe, provenance=EXECUTED,
                            unreadable="settings-show did not report a settings record for this"
                                       " participant at the moment the gate runs",
                            evidence=("this participant's settings row is still the one the"
                                      " capability reading graded" if agrees else
                                      "this participant's settings row is not the one the"
                                      " capability reading graded, so the trial would run with a"
                                      " record this preflight never approved")))
    return cells


def reading_store(record, relay):
    """The relay's own same-store verdict, not a comparison rebuilt here."""
    cells = []
    store = record.get("store") or {}
    probe = relay.relay("doctor", "--expect-store", store.get("storeId"),
                        "--expect-inode", str(store.get("device")) + ":" + str(store.get("inode")),
                        "--expect-nonce", store.get("challengeNonce"))
    payload = probe["payload"] or {}
    verdict = field(payload, "sameStore")
    cells.append(graded("sameStore", verdict, verdict == "proven", probe=probe, provenance=EXECUTED,
                        unreadable="doctor did not report a same-store verdict",
                        evidence=("the relay grades this as " + str(shown(verdict)) + ". A matching"
                                  " store id and inode alone is unproven: proof takes the nonce"
                                  " another participant wrote, found beside an agreeing device and"
                                  " inode")))

    created = field(payload, "store", "createdAt")
    when = maybe_moment(created) if created is not MISSING else None
    if created is MISSING:
        cells.append(cell("storeAge", UNKNOWN, probe=probe, provenance=EXECUTED,
                          evidence="doctor did not report when the store was created"))
    elif when is None:
        cells.append(cell("storeAge", UNKNOWN, probe=probe, provenance=EXECUTED,
                          evidence="the store's createdAt could not be read as a time: "
                                   + str(created)))
    else:
        # Against this process's real start: a store that did not exist before this run began is
        # one this run created, and its identity would be plausible and its contents empty.
        before = when.timestamp() < STARTED
        cells.append(cell("storeAge", VERIFIED if before else NOT_VERIFIED, probe=probe,
                          provenance=EXECUTED,
                          evidence=("the store was created at " + str(created) + " and this run"
                                    " started at " + stamp(STARTED))))

    configured = field(payload, "ledger", "configured")
    reach = field(payload, "actorReachability", "socketConnect")
    cells.append(graded("socketReachable", reach, reach == "ok", probe=probe, provenance=EXECUTED,
                        unreadable="doctor did not report whether it could reach the socket",
                        evidence=("the acting process reaches the App Server socket: "
                                  + str(shown(reach)) + ". A store comparison is decided on the"
                                  " database and says nothing about the socket the delivery will"
                                  " use")))
    if not isinstance(configured, bool):
        # Whether the ledger sits beside the store is only answerable once doctor says a ledger
        # is configured at all. A payload carrying split without configured graded as placed,
        # which is a missing prerequisite reading as a pass.
        cells.append(cell("ledgerSplit", UNKNOWN, probe=probe, provenance=EXECUTED,
                          evidence=("doctor did not report whether a transport ledger is"
                                    " configured, and it answered " + str(shown(configured))
                                    + ", so where that ledger sits is not a question this"
                                    " payload settles")))
    elif configured is False:
        cells.append(cell("ledgerSplit", NOT_APPLICABLE, probe=probe, provenance=EXECUTED,
                          evidence="no socket is configured, so there is no transport ledger to"
                                   " place beside this store"))
    else:
        split = field(payload, "ledger", "split")
        cells.append(graded("ledgerSplit", split, split is False, probe=probe, provenance=EXECUTED,
                            unreadable="doctor did not report where the transport ledger lives",
                            evidence=("the transport ledger sits beside this store" if split is False
                                      else "the transport ledger is split from this store, so the"
                                           " record that suppresses duplicate delivery lives"
                                           " somewhere else")))

    # The peers are the participants, not whichever captures happened to be supplied. Enumerating
    # the captures made an empty peerDoctor object a storeIdentity that passed with no peer at all.
    for name in participants_of(record):
        found, why = capture(record, "peerDoctor", name)
        entry = field(record, "captures", "peerDoctor", name)
        twins = [other for other in participants_of(record)
                 if other != name
                 and field(record, "captures", "peerDoctor", other) is not MISSING
                 and same_file(field(record, "captures", "peerDoctor", other, "path"),
                               field(entry, "path"))]
        if twins:
            # One file cannot be several participants' own reading: graded once per name it was
            # listed under, or copied, it would report a shared store on the strength of one peer.
            cells.append(cell("peer:" + name, NOT_VERIFIED, provenance=CAPTURED,
                              evidence=("this capture is the same reading as " + ", ".join(twins)
                                        + ", so it is one participant's reading counted as"
                                          " several")))
            continue
        if found is None:
            cells.append(cell("peer:" + name, UNKNOWN, evidence=why, provenance=CAPTURED))
            continue
        peer = found["payload"]
        peer_same = field(peer, "sameStore")
        # The verdict speaks for the nonce it was given, and the payload carries which one that
        # was: a peer run against an older challenge could report proven about a store this trial
        # never wrote to.
        asked = field(peer, "nonce", "nonce")
        nonce_agrees = same(asked, store.get("challengeNonce"))
        agrees = (same(field(peer, "store", "storeId"), store.get("storeId"))
                  and same(field(peer, "store", "device"), store.get("device"))
                  and same(field(peer, "store", "inode"), store.get("inode")))
        # Every field the verdict reads, not only the verdict: a doctor payload naming a store
        # and no device never said which inode it was, and a disagreement would say it did.
        answered = MISSING if (peer_same is MISSING or asked is MISSING
                               or field(peer, "store", "storeId") is MISSING
                               or field(peer, "store", "device") is MISSING
                               or field(peer, "store", "inode") is MISSING) else peer_same
        # doctor does not name the participant that ran it, so two peers legitimately produce
        # identical payloads and this is reported rather than graded. What it costs is stated in
        # the stand-ins: the attribution of a capture to a participant is the operator's.
        alike = [other for other in participants_of(record) if other != name
                 and (lambda t: t is not None
                      and json.dumps(t["payload"], sort_keys=True)
                      == json.dumps(peer, sort_keys=True))(capture(record, "peerDoctor", other)[0])]
        cells.append(graded("peer:" + name, answered,
                            peer_same == "proven" and agrees and nonce_agrees,
                            provenance=CAPTURED, measured_at=found["capturedAt"],
                            unreadable="this peer's doctor payload carries no same-store verdict"
                                       " and the challenge it was asked about",
                            evidence=("this peer reports " + str(shown(peer_same)) + " and its own"
                                      " store identity "
                                      + ("agrees with" if agrees else "disagrees with")
                                      + " the record, for challenge " + str(shown(asked))
                                      + ". A verdict speaks only for the nonce it was given"
                                      + (", and this payload is identical to " + ", ".join(alike)
                                         + ", which doctor cannot tell apart because it does not"
                                           " name the participant that ran it" if alike else "")),
                            detail=found["path"]))
    return cells


def participants_of(record):
    """Every declared participant, in a stable order. The roster is what a per-participant reading
    is required to cover, so a missing one is unknown rather than absent from the count."""
    found = []
    for boundary in record.get("boundaries") or []:
        for participant in boundary.get("participants") or []:
            task = str(participant.get("taskId"))
            if task not in found:
                found.append(task)
    return found


def repository_identity(root):
    """Which repository a declared root is in, read the way git decides it rather than by path.

    Linked worktrees of one repository have distinct toplevels, so comparing declared roots — or
    the --show-toplevel each participant reports — reads one repository as two. That is not an
    edge case here: this project runs as linked worktrees of a single repository, so a
    declaration settled on roots passes for exactly the ordinary arrangement it exists to refuse.
    --git-common-dir resolves to one directory for every linked worktree of one repository, and
    to different ones for genuinely separate repositories, which is the identity the declaration
    claims to be comparing.

    git answers it relative to the directory it ran in, so a bare .git is that directory's own.
    Returns MISSING where git had no answer, because a repository nobody could read is unknown
    rather than one that disagreed.
    """
    probe = Relay.git(root, "rev-parse", "--git-common-dir")
    found = (probe["stdout"] or "").strip()
    if probe["exitCode"] != 0 or not found:
        return MISSING, probe
    return str(resolve(Path(str(root)) / found)), probe


def reading_boundaries(record, relay):
    """Two identities, confirmed against the registration that created them."""
    cells = []
    boundaries = record.get("boundaries") or []
    keys = [b.get("issueKey") for b in boundaries]
    scopes = [b.get("scopeRef") for b in boundaries]
    roots = [str(resolve(b.get("repositoryRoot"))) for b in boundaries]
    tasks = [p.get("taskId") for b in boundaries for p in b.get("participants") or []]
    stated = all(isinstance(value, str) and value.strip()
                 for value in keys + scopes + tasks)
    distinct = (stated and len(boundaries) >= 2 and len(set(keys)) == len(keys)
                and len(set(scopes)) == len(scopes) and len(set(roots)) == len(roots)
                and len(set(tasks)) == len(tasks))
    cells.append(cell("declaration", VERIFIED if distinct else NOT_VERIFIED, provenance=READ,
                      evidence=(str(len(boundaries)) + " boundaries declared, issue keys "
                                + json.dumps(keys) + ", scope references " + json.dumps(scopes)
                                + ", repository roots " + json.dumps(roots)
                                + ", participants " + json.dumps(tasks)
                                + ". Two boundaries with one issue key are not two boundaries,"
                                  " and two sharing a participant are not two parents. A blank"
                                  " identity is not one either. Distinct roots are not distinct"
                                  " repositories, because linked worktrees of one repository have"
                                  " distinct roots, so which repository each boundary is in is"
                                  " read by git rather than taken from these paths"),
                      measured_at=stamp()))

    # Roots are spellings; this is the identity. Read once per boundary and compared across them,
    # because the false pass this refuses is two linked worktrees of one repository reported as
    # two repositories, which is how every checkout on this host is arranged.
    identities = [repository_identity(b.get("repositoryRoot")) for b in boundaries]
    for boundary, (identity, probe) in zip(boundaries, identities):
        shared = [str(other.get("name")) for other, (twin, _) in zip(boundaries, identities)
                  if other is not boundary and identity is not MISSING and twin == identity]
        cells.append(graded("repositoryIdentity:" + str(boundary.get("name")), identity,
                            not shared, probe=probe, provenance=EXECUTED,
                            unreadable=("git reported no common directory for "
                                        + str(boundary.get("repositoryRoot"))),
                            evidence=("this boundary's repository is " + str(shown(identity))
                                      + (", which is the repository " + ", ".join(shared)
                                         + " is in as well, so these are linked worktrees of one"
                                           " repository rather than two repositories"
                                         if shared else
                                         ", which no other boundary is in"))))

    for boundary in boundaries:
        name = boundary.get("name")
        found, why = capture(record, "registration", str(name))
        if found is None:
            cells.append(cell("registration:" + str(name), UNKNOWN, evidence=why,
                              provenance=CAPTURED))
        else:
            receipt = found["payload"]
            scope = field(receipt, "authorizedScope", "scopeRef")
            child = next((p for p in boundary.get("participants") or []
                          if p.get("role") == "child"), {})
            current, why = registration_identity(record, boundary, receipt)
            # MISSING is an object and objects are truthy, so this verdict is compared rather than
            # tested: a receipt missing a field its identity is decided from is unknown, not one
            # that disagreed.
            current_ok = current is True
            parent = next((p for p in boundary.get("participants") or []
                           if p.get("role") == "parent"), {})

            def workspace(side, declared):
                """Compare one endpoint's workspace, or say the receipt does not carry it.

                An absent cwd is not a disagreement, and a relative one is not a place: resolve()
                would read it against whichever directory this process runs in, so an empty string
                or a dot agreed with the declared workspace whenever the checker ran there.
                """
                found = field(receipt, side, "cwd")
                if found is MISSING or found is None:
                    return MISSING
                if declared is None:
                    return True
                return (str(found).startswith("/")
                        and resolve(str(found)) == resolve(declared))

            workspaces = [workspace("child", child.get("cwd")),
                          workspace("parent", parent.get("cwd"))]
            ok = (same(scope, boundary.get("scopeRef"))
                  and current_ok
                  and same(field(receipt, "child", "taskId"), child.get("taskId"))
                  and same(field(receipt, "parent", "taskId"),
                           next((p.get("taskId") for p in boundary.get("participants") or []
                                 if p.get("role") == "parent"), None))
                  # Both workspaces, because the relay reads the recipient's own for lifecycle
                  # discovery and a parent registered against another one has delivery withheld.
                  and all(answer is True for answer in workspaces))
            # The cell is answerable only where every field its predicate reads is there.
            answered = (MISSING if (scope is MISSING or MISSING in workspaces
                                    or current is MISSING
                                    or field(receipt, "child", "taskId") is MISSING
                                    or field(receipt, "parent", "taskId") is MISSING) else scope)
            cells.append(graded("registration:" + str(name), answered, ok, provenance=CAPTURED,
                                measured_at=found["capturedAt"],
                                unreadable="this registration receipt does not carry both the"
                                           " authorised scope, each endpoint's workspace and the"
                                           " fields its identity is decided from",
                                evidence=("the registration names scope " + str(shown(scope))
                                          + ", issue " + str(shown(field(receipt, "issueKey")))
                                          + ", status " + str(shown(field(receipt, "status")))
                                          + ", relationship "
                                          + str(shown(field(receipt, "relationshipId")))
                                          + ", generation "
                                          + str(shown(field(receipt, "executionGeneration")))
                                          + ", child "
                                          + str(shown(field(receipt, "child", "taskId"))) + " at "
                                          + str(shown(field(receipt, "child", "cwd")))
                                          + ", parent "
                                          + str(shown(field(receipt, "parent", "taskId")))),
                                detail=found["path"]))

        for participant in boundary.get("participants") or []:
            probe = relay.git(participant.get("cwd"), "rev-parse", "--show-toplevel")
            top = (probe["stdout"] or "").strip()
            ok = bool(top) and resolve(top) == resolve(boundary.get("repositoryRoot"))
            cells.append(graded("toplevel:" + str(name) + ":" + str(participant.get("taskId")),
                                top if top else MISSING, ok, probe=probe, provenance=EXECUTED,
                                unreadable="git did not report a toplevel for this directory",
                                evidence=("this directory is in " + str(top) + ", declared "
                                          + str(boundary.get("repositoryRoot")))))
    return cells


def reading_assignment(record, relay):
    """What the store says about this issue right now, and which criteria are registered."""
    assignment = record.get("assignment") or {}
    cells = []
    probe = relay.relay("assignment-find", "--issue", assignment.get("issueKey"))
    payload = probe["payload"] or {}
    responsible = field(payload, "responsibleRelationship")
    entries = field(payload, "assignments")
    entry = MISSING
    if isinstance(entries, list):
        entry = next((e for e in entries
                      if isinstance(e, dict)
                      and e.get("relationshipId") == assignment.get("relationshipId")), MISSING)
    ok = (same(responsible, assignment.get("relationshipId")) and entry is not MISSING
          and field(entry, "relationshipStatus") == "active"
          and same(field(entry, "childTaskId"), assignment.get("childTaskId"))
          and same(field(entry, "parentTaskId"), assignment.get("parentTaskId"))
          and same(field(entry, "executionGeneration"), assignment.get("executionGeneration")))
    # Answerable only where every field its predicate reads is there. An entry naming a
    # relationship and nothing else never said whether it was active or whose generation it was.
    absent = [name for name, value in (("which relationship owns this issue", responsible),
                                       ("its status", field(entry, "relationshipStatus")),
                                       ("its child", field(entry, "childTaskId")),
                                       ("its parent", field(entry, "parentTaskId")),
                                       ("its generation", field(entry, "executionGeneration")))
              if value is MISSING]
    cells.append(graded("relationship", MISSING if absent else responsible, ok, probe=probe,
                        provenance=EXECUTED,
                        unreadable="the store did not answer " + ", ".join(absent),
                        evidence=("the responsible relationship is " + str(shown(responsible))
                                  + ", status " + str(shown(field(entry, "relationshipStatus")))
                                  + ", child " + str(shown(field(entry, "childTaskId")))
                                  + ", parent " + str(shown(field(entry, "parentTaskId")))
                                  + ", generation "
                                  + str(shown(field(entry, "executionGeneration"))))))

    wanted = assignment.get("criteria") or {}
    criteria_probe = relay.relay("criteria-show", "--relationship", assignment.get("relationshipId"))
    criteria = criteria_probe["payload"] or {}
    registered = field(criteria, "criteria")
    count = len(registered) if isinstance(registered, list) else None
    ok = (same(field(criteria, "setDigest"), wanted.get("setDigest"))
          and same(field(criteria, "sourceRef"), wanted.get("sourceRef"))
          and same(count, wanted.get("count")))
    absent = [name for name, value in (("a set digest", field(criteria, "setDigest")),
                                       ("a source reference", field(criteria, "sourceRef")),
                                       ("a registered set", registered))
              if value is MISSING]
    cells.append(graded("criteria", MISSING if absent else field(criteria, "setDigest"), ok,
                        probe=criteria_probe,
                        provenance=EXECUTED,
                        unreadable="criteria-show did not report " + ", ".join(absent),
                        evidence=("digest " + str(shown(field(criteria, "setDigest")))
                                  + ", source " + str(shown(field(criteria, "sourceRef"))) + ", "
                                  + str(count) + " criteria. A non-empty set is not the intended"
                                  " set")))
    return cells, payload, entry


def assignment_now(record, relay):
    """The store's answer about this issue, read again immediately before the gate.

    reading_assignment runs before the witness delay and before every probe that follows it, and
    the gate is the last thing between this run and a dispatch. A relationship archived or
    reassigned while those seconds passed left the gate comparing a file and a message against an
    answer taken minutes earlier, and readiness was published from it. The relay's own refusal
    stays the guard that makes the race impossible; this closes the window the checker opened by
    reading once and reusing it.
    """
    assignment = record.get("assignment") or {}
    probe = relay.relay("assignment-find", "--issue", assignment.get("issueKey"))
    payload = probe["payload"] or {}
    entries = field(payload, "assignments")
    entry = MISSING
    if isinstance(entries, list):
        entry = next((e for e in entries
                      if isinstance(e, dict)
                      and e.get("relationshipId") == assignment.get("relationshipId")), MISSING)
    responsible = field(payload, "responsibleRelationship")
    status = field(entry, "relationshipStatus")
    generation = field(entry, "executionGeneration")
    still = (same(responsible, assignment.get("relationshipId")) and status == "active"
             and same(field(entry, "childTaskId"), assignment.get("childTaskId"))
             and same(field(entry, "parentTaskId"), assignment.get("parentTaskId"))
             and same(generation, assignment.get("executionGeneration")))
    answered = MISSING if (responsible is MISSING or entry is MISSING
                           or status is MISSING or generation is MISSING
                           or field(entry, "childTaskId") is MISSING
                           or field(entry, "parentTaskId") is MISSING) else responsible
    cell_now = graded("relationshipStillCurrent", answered, still, probe=probe,
                      provenance=EXECUTED,
                      unreadable="the store did not answer which relationship owns this issue at"
                                 " the moment the gate runs",
                      evidence=("read again after every other reading rather than before them:"
                                " the responsible relationship is " + str(shown(responsible))
                                + ", status " + str(shown(status)) + ", generation "
                                + str(shown(generation)) + ". The first read is separated from"
                                " the gate by the witness delay and every probe between them,"
                                " and a relationship archived while those ran would have been"
                                " graded from an answer that was already stale"))
    return cell_now, payload, entry


def criteria_now(record, relay):
    """The registered criteria, read again immediately before the gate.

    reading_assignment reads them before the witness delay and every probe after it. A set
    replaced while those seconds passed left the cached cell verified, and the trial would then
    be judged against criteria this preflight never approved.
    """
    assignment = record.get("assignment") or {}
    wanted = assignment.get("criteria") or {}
    probe = relay.relay("criteria-show", "--relationship", assignment.get("relationshipId"))
    payload = probe["payload"] or {}
    registered = field(payload, "criteria")
    digest = field(payload, "setDigest")
    source = field(payload, "sourceRef")
    count = len(registered) if isinstance(registered, list) else None
    still = (same(digest, wanted.get("setDigest")) and same(source, wanted.get("sourceRef"))
             and same(count, wanted.get("count")))
    answered = MISSING if (digest is MISSING or source is MISSING
                           or registered is MISSING) else digest
    return graded("criteriaStillCurrent", answered, still, probe=probe, provenance=EXECUTED,
                  unreadable="criteria-show did not report a registered set at the moment the"
                             " gate runs",
                  evidence=("read again after every other reading rather than before them: digest "
                            + str(shown(digest)) + ", source " + str(shown(source)) + ", "
                            + str(count) + " criteria. A set replaced while the run was working"
                            " would judge this trial against criteria nobody approved"))



# --------------------------------------------------------------------- the gate


def names_exactly(text, value):
    """Whether the message names this exact value as a word of its own.

    A POSIX filename may hold any byte but a separator and a NUL, so no rule can tell a trailing
    bracket or full stop that belongs to prose from one that belongs to the path. Two attempts
    proved it: a substring test read /repo/artifact.py.bak as naming /repo/artifact.py, and
    stripping punctuation afterwards read /repo/file! as naming /repo/file while refusing a real
    artifact ending in a bracket.

    So the requirement moves to the message instead of to the guessing: the dispatch message names
    each identity as a whitespace-delimited word. That is a rule an operator can follow exactly,
    and this comparison has nothing left to get wrong.
    """
    return bool(value) and value in text.split()


def order_gate(record, store_payload, entry):
    """The file, the store and the message, compared before the message is sent.

    A file written for a previous relationship carries that relationship's id, which is what
    emitted against an archived one. This refuses the dispatch and names the field that disagreed;
    it does not make the race impossible, because it reads at one moment and the child reads at a
    later one. What makes it impossible is the order, and the relay's own refusal remains the guard.
    """
    assignment = record.get("assignment") or {}
    comparisons = []
    file_found = reading.read_json(assignment.get("assignmentFile"), "the child's assignment file")
    if not file_found.usable or file_found.state == reading.ABSENT or not isinstance(file_found.value, dict):
        return {"passed": False, "unreadable": True,
                "detail": "the assignment file at " + str(assignment.get("assignmentFile"))
                          + " could not be read (" + str(file_found.state) + ")",
                "comparisons": []}
    in_file = file_found.value

    def compare(name, left, right, left_source, right_source):
        agrees = same(left, right)
        comparisons.append({"field": name, "agrees": agrees, "left": shown(left),
                            "right": shown(right), "leftSource": left_source,
                            "rightSource": right_source})
        return agrees

    compare("relationshipId", in_file.get("relationshipId"),
            field(store_payload, "responsibleRelationship"),
            "the assignment file", "assignment-find")
    compare("relationshipId", in_file.get("relationshipId"), assignment.get("relationshipId"),
            "the assignment file", "the start record")
    compare("childTaskId", in_file.get("childTaskId"), field(entry, "childTaskId"),
            "the assignment file", "assignment-find")
    compare("executionGeneration", in_file.get("executionGeneration"),
            field(entry, "executionGeneration"), "the assignment file", "assignment-find")
    compare("relationshipStatus", field(entry, "relationshipStatus"), "active",
            "assignment-find", "this gate")
    if entry is MISSING:
        comparisons.append({"field": "assignmentEntry", "agrees": False, "left": None,
                            "right": shown(assignment.get("relationshipId")),
                            "leftSource": "assignment-find returned no entry for this relationship",
                            "rightSource": "the start record"})

    roots, roots_source = MISSING, None
    # Load refused a record whose assignment belongs to no declared boundary, so this find always
    # has one; it is written without a fallback so that it cannot silently read another boundary's
    # registration if that ever changes.
    owning = next((b for b in record.get("boundaries") or []
                   if b.get("issueKey") == assignment.get("issueKey")), None)
    if owning is None:
        return {"passed": False, "unreadable": True,
                "detail": "the assignment's issue belongs to no declared boundary",
                "comparisons": []}
    found, why = capture(record, "registration", str(owning.get("name")))
    if found is not None:
        current, detail = registration_identity(record, owning, found["payload"])
        if current is True:
            roots = field(found["payload"], "authorizedScope", "artifactRoots")
            roots_source = found["path"]
        else:
            comparisons.append({"field": "registrationIdentity", "agrees": False,
                                "left": shown(field(found["payload"], "relationshipId")),
                                "right": shown(assignment.get("relationshipId")),
                                "leftSource": found["path"] + ": " + detail,
                                "rightSource": "the start record"})
    # The assignment file is the input the child reads, so its own list is the one under test. A
    # fallback to the record's artifacts validated the trial's intention against a file that did
    # not carry it, and an empty list took the fallback exactly like an absent key.
    declared = assignment.get("artifacts") or []
    in_file_artifacts = in_file.get("artifacts")
    if not isinstance(in_file_artifacts, list) or not in_file_artifacts:
        comparisons.append({"field": "artifacts", "agrees": False, "left": shown(in_file_artifacts),
                            "right": shown(declared), "leftSource": "the assignment file",
                            "rightSource": "the start record"})
        artifacts = []
    else:
        comparisons.append({"field": "artifacts",
                            "agrees": sorted(str(a) for a in in_file_artifacts)
                                      == sorted(str(a) for a in declared),
                            "left": shown(in_file_artifacts), "right": shown(declared),
                            "leftSource": "the assignment file",
                            "rightSource": "the start record"})
        artifacts = in_file_artifacts
    for artifact in artifacts:
        try:
            canonical(artifact, "an artifact in the assignment file")
        except Refused as refused:
            comparisons.append({"field": "artifactIsCanonical", "agrees": False,
                                "left": shown(artifact),
                                "right": "an absolute, normalised path",
                                "leftSource": "the assignment file",
                                "rightSource": "the relay's manifest refuses a path that is"
                                               " relative, unnormalised, trailing-slashed or"
                                               " holding a tilde: " + refused.reason})
            continue
        linked = symlink_component(artifact)
        if linked is not None:
            comparisons.append({"field": "artifactFollowsNoLink", "agrees": False,
                                "left": shown(artifact), "right": linked,
                                "leftSource": "the assignment file",
                                "rightSource": "this component is a symbolic link, and the relay"
                                               " opens every component refusing to follow one"})
    if roots is MISSING or not isinstance(roots, list):
        comparisons.append({"field": "artifactRoots", "agrees": None,
                            "left": shown(artifacts), "right": None,
                            "leftSource": "the assignment file",
                            "rightSource": "no registration receipt was readable, and no read-only"
                                           " command returns the authorised roots"})
    else:
        for artifact in artifacts:
            inside = any(lexically_within(artifact, root) for root in roots)
            comparisons.append({"field": "artifact", "agrees": inside, "left": shown(artifact),
                                "right": shown(roots), "leftSource": "the assignment file",
                                "rightSource": roots_source})

    message = reading.read_text(assignment.get("dispatchMessageFile"), "the dispatch message")
    if not message.usable or message.state == reading.ABSENT:
        comparisons.append({"field": "message", "agrees": None, "left": None, "right": None,
                            "leftSource": str(assignment.get("dispatchMessageFile")),
                            "rightSource": "the message could not be read ("
                                           + str(message.state) + ")"})
    else:
        text = message.value or ""
        carried = names_exactly(text, str(assignment.get("relationshipId")))
        comparisons.append({"field": "messageCarriesRelationship", "agrees": carried,
                            "left": shown(assignment.get("relationshipId")),
                            "right": "the message text",
                            "leftSource": "the start record",
                            "rightSource": str(assignment.get("dispatchMessageFile"))})
        for artifact in artifacts:
            comparisons.append({"field": "messageCarriesArtifact",
                                "agrees": names_exactly(text, str(artifact)),
                                "left": shown(artifact),
                                "right": "the message text", "leftSource": "the assignment file",
                                "rightSource": str(assignment.get("dispatchMessageFile"))})

    unresolved = [c for c in comparisons if c["agrees"] is None]
    return {"passed": all(c["agrees"] for c in comparisons if c["agrees"] is not None)
                      and not unresolved,
            "unreadable": bool(unresolved), "comparisons": comparisons,
            "note": "the equality is the race check. The authorised roots come from the captured"
                    " registration receipt because no read-only command returns them, and what"
                    " enforces them is the manifest emit builds"}


# ------------------------------------------------------------------- the ledger


def ledger_report(record):
    """Preparation and the window, separated by timestamp rather than by what a record called itself."""
    path = record["_ledger"]
    found = reading.read_text(path, "the intervention ledger")
    if not found.usable or found.state == reading.ABSENT:
        raise Refused("the ledger could not be read", path=str(path), state=found.state)

    entries = []
    for number, line in enumerate(found.value.splitlines(), 1):
        if not line.strip():
            continue
        try:
            entry = json.loads(line)
        except ValueError as error:
            raise Refused("a ledger line is not JSON", line=number, detail=str(error)) from error
        if not isinstance(entry, dict) or entry.get("kind") not in LEDGER_KINDS:
            raise Refused("a ledger line carries no known kind", line=number,
                          kind=entry.get("kind") if isinstance(entry, dict) else None)
        entry["_at"] = moment(entry.get("at"), "a ledger line's at")
        if entry["_at"].timestamp() > time.time():
            # The ledger is appended as things happen, so a line dated after the moment it is
            # graded did not happen. Classified rather than refused, it fell outside the window
            # and was counted as preparation.
            raise Refused("a ledger line is dated after the time it is being graded",
                          line=number, at=entry.get("at"), now=stamp())
        entry["_line"] = number
        entries.append(entry)

    opens = [e for e in entries if e["kind"] == "window_open"]
    closes = [e for e in entries if e["kind"] == "window_close"]
    if len(opens) > 1 or len(closes) > 1:
        raise Refused("a trial has one window", opened=len(opens), closed=len(closes))
    if not opens or not closes:
        raise Refused("the window is not bounded in this ledger", opened=len(opens),
                      closed=len(closes))
    # One window is one named interval. Pairing an open and a close that name different segments
    # built a synthetic interval, and interventions were classified against something nobody ran.
    if not opens[0].get("segment") or opens[0].get("segment") != closes[0].get("segment"):
        raise Refused("the window's open and close name different segments",
                      opensAt=opens[0].get("at"), opensSegment=shown(opens[0].get("segment")),
                      closesAt=closes[0].get("at"), closesSegment=shown(closes[0].get("segment")))
    opened, closed = opens[0]["_at"], closes[0]["_at"]
    if closed <= opened:
        # A point interval holds none of the completion, delivery, verdict and correction the
        # trial exists to measure, and it is clean of interventions by construction.
        raise Refused("this window has no duration to measure", opensAt=opens[0].get("at"),
                      closesAt=closes[0].get("at"))
    if closed.timestamp() > time.time():
        # A window that has not closed cannot be graded: the interventions it would have to be
        # clean of have not all happened yet.
        raise Refused("this window has not closed yet, so there is nothing to grade",
                      closesAt=closes[0].get("at"), now=stamp())
    # The record declares the window and the ledger records it, and they have to be the same
    # window. Grading the ledger's own pair alone let a mistaken boundary move an intervention
    # out of the measured window and report the result as clean.
    for key, event in (("opensAt", opens[0]), ("closesAt", closes[0])):
        declared = field(record, "window", key)
        if declared is MISSING or declared is None:
            raise Refused("the start record does not declare window." + key)
        if moment(declared, "window." + key) != event["_at"]:
            raise Refused("the ledger's window does not match the one the record declares",
                          field=key, declared=declared, ledger=event.get("at"),
                          line=event["_line"])

    # Segments are intervals, built from their own timestamps and then checked for intersection.
    # Pairing them by the order their lines happen to sit in accepted two segments that overlap in
    # time, and counted one intervention inside both of them.
    segments = {}
    for entry in entries:
        if entry["kind"] not in ("segment_start", "segment_end"):
            continue
        name = entry.get("segment")
        if not name:
            raise Refused("a segment boundary names no segment", line=entry["_line"])
        found = segments.setdefault(name, {"name": name, "opensAt": None, "closesAt": None,
                                           "_from": None, "_to": None, "outcome": None,
                                           "interventions": []})
        if entry["kind"] == "segment_start":
            if found["_from"] is not None:
                raise Refused("a segment starts twice", segment=name, line=entry["_line"])
            found["_from"], found["opensAt"] = entry["_at"], entry.get("at")
        else:
            if found["_to"] is not None:
                raise Refused("a segment ends twice", segment=name, line=entry["_line"])
            found["_to"], found["closesAt"] = entry["_at"], entry.get("at")
            found["outcome"] = entry.get("outcome")
    ordered = sorted(segments.values(), key=lambda s: (s["_from"] is None, s["_from"]))
    for segment in ordered:
        if segment["_from"] is None:
            raise Refused("a segment ends without starting", segment=segment["name"])
        if segment["_to"] is None:
            raise Refused("a segment never closes, so what it attempted was never recorded",
                          segment=segment["name"], opensAt=segment["opensAt"])
        if segment["outcome"] not in ("failed", "succeeded"):
            raise Refused("a segment closes without saying whether it failed or succeeded",
                          segment=segment["name"], outcome=shown(segment["outcome"]))
        if segment["_to"] is not None and segment["_to"] < segment["_from"]:
            raise Refused("a segment closes before it opens", segment=segment["name"])
    for first, second in zip(ordered, ordered[1:]):
        if first["_to"] is None or second["_from"] <= first["_to"]:
            raise Refused("two segments overlap in time", earlier=first["name"],
                          later=second["name"], earlierClosesAt=first["closesAt"],
                          laterOpensAt=second["opensAt"])
    for segment in ordered:
        # A preparation segment running through the measured window means preparation was still
        # happening inside it, whether or not anybody wrote an intervention down.
        if segment["_from"] <= closed and opened <= segment["_to"]:
            raise Refused("a preparation segment overlaps the trial window",
                          segment=segment["name"], opensAt=segment["opensAt"],
                          closesAt=segment["closesAt"], windowOpensAt=opens[0].get("at"),
                          windowClosesAt=closes[0].get("at"))
    segments = ordered

    preparation, inside = [], []
    for entry in (e for e in entries if e["kind"] == "intervention"):
        computed = WINDOW if opened <= entry["_at"] <= closed else PREPARATION
        claimed = entry.get("claimed")
        if claimed is not None and claimed != computed:
            raise Refused("a ledger line's claimed class disagrees with its own timestamp",
                          line=entry["_line"], at=entry.get("at"), claimed=claimed,
                          computed=computed)
        item = {"at": entry.get("at"), "actor": entry.get("actor"), "target": entry.get("target"),
                "action": entry.get("action"), "class": computed, "line": entry["_line"]}
        if computed == WINDOW:
            inside.append(item)
        else:
            preparation.append(item)
            for segment in segments:
                if segment["_from"] <= entry["_at"] and (segment.get("_to") is None
                                                         or entry["_at"] <= segment["_to"]):
                    segment["interventions"].append(item)

    # Corroboration is compared, not accepted. A supplied object that was only carried into the
    # document would have made a declared boundary read as a measured one, which is the whole
    # reason this field exists.
    corroboration = field(record, "window", "corroboration")
    provenance, compared = "declared", []
    if corroboration not in (MISSING, None):
        if not isinstance(corroboration, dict):
            raise Refused("window.corroboration is not an object of times",
                          corroboration=shown(corroboration))
        for key, declared in (("opensAt", opens[0].get("at")), ("closesAt", closes[0].get("at"))):
            supplied = corroboration.get(key)
            if supplied is None:
                continue
            if moment(supplied, "window.corroboration." + key) != moment(declared, key):
                raise Refused("a corroborating time disagrees with the window it corroborates",
                              field=key, corroborating=supplied, declared=declared)
            compared.append(key)
        if not compared:
            raise Refused("window.corroboration names no time to compare")
        provenance = "corroborated"
    clean = not inside
    return {
        "source": SOURCE, "checkerVersion": CHECKER_VERSION, "ledger": str(path),
        "preparation": {
            "interventions": len(preparation), "entries": preparation,
            "segments": [{"name": s["name"], "opensAt": s["opensAt"],
                          "closesAt": s.get("closesAt"), "outcome": s["outcome"],
                          "interventions": len(s["interventions"])} for s in segments],
            "failedSegments": [s["name"] for s in segments if s["outcome"] == "failed"],
        },
        "window": {
            "opensAt": opens[0].get("at"), "closesAt": closes[0].get("at"),
            "provenance": provenance, "corroborated": compared,
            "corroboration": None if corroboration is MISSING else corroboration,
            "interventions": len(inside), "entries": inside,
            "windowIsClean": clean, "passed": clean,
        },
        "note": "classification is by timestamp and by nothing else, and a window with any"
                " intervention inside it is not an uninterrupted result. What this cannot see is an"
                " intervention nobody wrote down.",
    }



# --------------------------------------------------------- assembly and judgment


def judgments(document):
    """Every field named passed or met, found by walking what was assembled.

    Built from the document rather than from a list of the kinds of thing that produce judgments,
    because a list leaves one out and the one it leaves out is a judgment that can fail while the
    command exits zero.
    """
    found = []

    def walk(node, path):
        if isinstance(node, dict):
            for key, value in node.items():
                here = path + "." + str(key) if path else str(key)
                if key in ("passed", "met") and isinstance(value, bool):
                    found.append({"at": here, "value": value})
                else:
                    walk(value, here)
        elif isinstance(node, list):
            for index, value in enumerate(node):
                walk(value, path + "[" + str(index) + "]")

    walk(document, "")
    return found


def digest_or_none(path):
    try:
        return digest_of(path)
    except (OSError, TypeError):
        return None


def same_file(left, right):
    """Whether two spellings name one file, rather than whether they are the same spelling.

    A link and a second name reach one file under two strings, so comparing the strings let one
    capture be filed under two participants and read as two.
    """
    if left is MISSING or right is MISSING or left is None or right is None:
        return False
    if str(left) == str(right):
        return True
    try:
        return os.path.samefile(str(left), str(right))
    except OSError:
        # Two paths that cannot be statted are not thereby one file, and calling them one turned
        # an unreadable capture into a duplicate reading.
        return False


def captures_still_fresh(record):
    """Every capture that contributed to readiness, aged once more at the end of the run."""
    bound = record.get("captureMaxAgeSeconds")
    now = datetime.datetime.now(datetime.timezone.utc)
    stale = [{"kind": entry["kind"], "name": entry["name"],
              "ageSeconds": int((now - entry["at"]).total_seconds())}
             for entry in record.get("_captures", [])
             if (now - entry["at"]).total_seconds() > bound]
    return {"passed": not stale, "boundSeconds": bound, "readAt": stamp(), "stale": stale,
            "detail": "a capture fresh when it was read can expire while the readings after it are"
                      " taken, and readiness is published after all of them"}


def launcher_unchanged(record, relay=None):
    """The launcher's bytes read again, after every probe has run.

    The pointer the host record names is an atomically movable symlink, which is how an update is
    meant to work, so the digest taken before the first probe says nothing about what the last one
    ran. Reading it at both ends does not prevent a move; it reports one, which is what the off/on
    harness does with its own source identity and for the same reason.

    It is also read immediately before each spawn, which narrows the unwatched interval to one
    probe. A replacement put back before the next reading is still invisible: catching that needs
    a witness at the process boundary, which is CRW-102's and is not claimed here.
    """
    anchor = record.get("_relay") or {}
    read = relay.digests if relay is not None else set()
    seen = sorted(d for d in read if d)
    unreadable = any(d is None for d in read)
    try:
        after = digest_of(anchor.get("launcher"))
    except (OSError, TypeError) as error:
        return {"passed": False, "before": anchor.get("sha256"), "after": None,
                "beforeEachProbe": seen,
                "detail": type(error).__name__ + ": " + str(error)}
    return {"passed": (after == anchor.get("sha256")
                       and all(d == anchor.get("sha256") for d in seen)
                       and not unreadable),
            "before": anchor.get("sha256"), "after": after, "beforeEachProbe": seen,
            "unreadableBeforeAProbe": unreadable,
            "detail": "the pointer moves on update by design, so this is read at both ends of the"
                      " run and before each spawn: it reports a move rather than preventing one,"
                      " and a replacement put back between two readings is CRW-102's witness"}


def preflight(record, *, sleeper=time.sleep):
    """Every reading, then the gate, in one run immediately before the dispatch."""
    relay = Relay(record)
    # doctor first, and before anything that constructs a store: opening one creates it, so a store
    # this run made for itself would otherwise be read as the shared store with a plausible identity.
    store_cells = reading_store(record, relay)
    assignment_cells, store_payload, entry = reading_assignment(record, relay)
    capability_cells, settings_seen = reading_capability(record, relay)
    readings = {
        "storeIdentity": store_cells,
        "processPersistence": reading_process(record, relay, sleeper=sleeper),
        "parentLifecycle": reading_lifecycle(record),
        "capability": capability_cells,
        "boundaries": reading_boundaries(record, relay),
        "assignmentState": assignment_cells,
    }

    # Everything above took real time: the witness delay sits inside it, and so does every probe
    # after it. The gate is graded against a fresh read rather than the one taken before them.
    current, store_payload, entry = assignment_now(record, relay)
    assignment_cells.append(current)
    assignment_cells.append(criteria_now(record, relay))
    capability_cells.extend(settings_now(record, relay, settings_seen))

    assembled = {}
    for name, cells in readings.items():
        values = [c["value"] for c in cells]
        if not values:
            value = UNKNOWN
        elif UNKNOWN in values:
            value = UNKNOWN
        elif NOT_VERIFIED in values:
            value = NOT_VERIFIED
        else:
            value = VERIFIED
        assembled[name] = {"value": value, "met": value == VERIFIED, "cells": cells}

    gate = order_gate(record, store_payload, entry)
    launcher = launcher_unchanged(record, relay)
    captures = captures_still_fresh(record)
    # The window was ahead when the record was read; the witness delay and the probes take real
    # time, so it is read again here. A run that publishes readiness after the window has opened
    # sends the dispatch into an interval already being measured.
    opens = moment(field(record, "window", "opensAt"), "window.opensAt")
    window_ahead = {"passed": (time.time() < opens.timestamp()
                               <= time.time() + WINDOW_ALLOWANCE),
                    "opensAt": shown(field(record, "window", "opensAt")),
                    "allowanceSeconds": WINDOW_ALLOWANCE,
                    "readAt": stamp(),
                    "detail": "the dispatch this preflight precedes is what opens the window"}
    document = {
        "source": SOURCE,
        "checkerVersion": CHECKER_VERSION,
        "startRecord": record["_start"],
        "trialRoot": record.get("trialRoot"),
        "pythonVersion": sys.version.split()[0],
        "startedAt": stamp(STARTED),
        "relay": record["_relay"],
        "launcherStillTheSameBytes": launcher,
        "readings": assembled,
        "orderGate": gate,
        "windowStillAhead": window_ahead,
        "capturesStillFresh": captures,
        # Filled from the judgment walk below, so a judgment added later cannot be left out of it.
        "readyToStart": None,
        "wroteNothing": "this process creates no file of its own. It is not a claim about the"
                        " commands it runs: every relay command opens the store on construction,"
                        " and doctor measures whether the state directory is writable by writing a"
                        " temporary file in it",
        "standIns": {
            "capturedLifecycle": "a lifecycle read this process did not make; it carries its own"
                                 " time and goes stale",
            "capturedReceipt": "the host's echo, recorded elsewhere. It says the host recorded the"
                               " request, never that a provider served the model",
            "supervisorWitness": "a witness at the process boundary. The pid inside it is the"
                                 " supervisor's own claim; that witness is CRW-102's",
            "hostRecord": "a trusted inventory. The launcher agrees with the installed-runtime"
                          " record rather than being proven to be the relay",
            "peerAttribution": "a doctor payload that names the participant that ran it. It does"
                               " not, so a peer capture is the operator's attribution: what this"
                               " establishes is that the peers are distinct readings, not that"
                               " each was taken by the participant it is filed under",
        },
        "procedure": "docs/live-trial.md",
    }
    document["readyToStart"] = bool(document["readyToStart"])
    counted = judgments(document)
    document["judgmentsCounted"] = len(counted)
    document["judgmentsThatFailed"] = [j["at"] for j in counted if not j["value"]]
    # Readiness is the walk's own answer rather than a second expression beside it: the two
    # disagreed once, when readiness was computed before the launcher judgment existed.
    document["readyToStart"] = not document["judgmentsThatFailed"]
    return document


def ledger(record):
    document = ledger_report(record)
    counted = judgments(document)
    document["judgmentsCounted"] = len(counted)
    document["judgmentsThatFailed"] = [j["at"] for j in counted if not j["value"]]
    return document


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    sub = parser.add_subparsers(dest="command", required=True)
    for name in ("preflight", "ledger"):
        one = sub.add_parser(name)
        one.add_argument("--start", required=True,
                         help="the start record under the private trial root")
    args = parser.parse_args(argv)

    try:
        record = load_start(args.start, mode=args.command)
        # The clock is this process's own. A pinned one was offered for tests, and an old capture
        # replayed beside an equally old pinned time was fresh by construction, which is the one
        # thing capture freshness exists to refuse.
        record["_now"] = datetime.datetime.now(datetime.timezone.utc)
        document = preflight(record) if args.command == "preflight" else ledger(record)
    except Refused as refused:
        print(json.dumps(refused.to_record(), indent=2, sort_keys=True))
        return 2
    except Exception as error:                                       # noqa: BLE001
        # Every way this can fail ends in a document. A traceback on stderr with nothing on stdout
        # is the one outcome a caller cannot tell from a run that never happened, and the raising
        # location travels with it so a defect here stays locatable rather than becoming a data
        # problem.
        print(json.dumps(Refused("this run raised before it could report",
                                 exception=type(error).__name__, detail=str(error),
                                 raisedAt=reading.where(error)).to_record(),
                         indent=2, sort_keys=True))
        return 2
    print(json.dumps(document, indent=2, sort_keys=True))
    return 1 if document["judgmentsThatFailed"] else 0


if __name__ == "__main__":
    raise SystemExit(main())
