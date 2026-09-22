#!/usr/bin/env python3
"""Install and diagnose the runtime the workflow depends on: the bridge and the relay.

This is the separate runtime entry point OPS-2.3 asks for. scripts/install.py stays what it
is - the standard-library-only, idempotent link step for skills - and runtime installation is
never folded into it. This command reads that installer rather than reimplementing it, and it
reuses its LINKED / MISSING / CONFLICT vocabulary so one word means one thing across both.

Standard library only, and importable on the Python this repository runs its own checks with.
The components it installs need 3.11 or newer; that interpreter is resolved, not assumed.
"""

import argparse
import hashlib
import json
import os
import shutil
import socket
import subprocess
import sys
from datetime import datetime, timezone
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))

from crw_runtime import (bridgerecord, check, codexconfig, completion, definition, hooks,
                         hostrecord, ownership, pointer, reading, residue, scope, staging,
                         swapgate)
from crw_runtime.text import text_prefix

ROOT = Path(__file__).resolve().parents[1]
EXIT_OK, EXIT_REFUSED, EXIT_USAGE = 0, 1, 2
# A fourth answer, because replacing a runtime and RECORDING that it was replaced are two
# results and one status cannot carry both. The staging claim is written last, after the
# selection is committed and the owned pointer is placed and read back, so by the time it can
# fail a host already reaches the new runtime through the registered command. Reporting that as
# EXIT_REFUSED says nothing was replaced, about a host that has already moved; reporting it as
# EXIT_OK says bookkeeping landed that did not. Nothing before the claim exits this way: every
# earlier failure leaves the previous runtime selected and reachable, which is a refusal.
#
# A WRAPPER MUST NOT READ THIS AS A FREE DESTINATION. Non-zero here means the opposite of what
# it means everywhere else in this command: the candidate was promoted, it is selected, the
# owned pointer names it, and a host is running out of it. A caller that treats every non-zero
# install status as "nothing happened, clean it up" would delete the runtime in service. The
# release path is reached only by EXIT_REFUSED; these two statuses keep the environment
# deliberately, which is why they are declared together and why the result says 'promoted'.
EXIT_INCOMPLETE = 3

# How long the settle failure path waits for the promotion lock to take a consistent snapshot
# of what this host selects. Short on purpose: this is already a failure path, and "could not
# be established" is a modelled answer, so waiting the promotion timeout to improve a sentence
# would be the wrong trade.
SETTLE_SNAPSHOT_TIMEOUT_SECONDS = 5.0

# The two components, named once. The MCP server name spells the bridge's component name, and
# that shared spelling is stated here rather than left for a reader to infer from two literals
# that happen to match.
BRIDGE = "codex-thread-bridge"
RELAY = "codex-session-relay"
MCP_NAME = BRIDGE

# The hooks this repository owns and can therefore derive a command for, and the event a hook
# lands on when nothing names one. A named adapter is not a second install path: it reaches the
# same append-only installation, with the command derived instead of typed.
COMPLETION = "completion"
ADAPTERS = (COMPLETION,)
SESSION_START = "SessionStart"

# Which command-line override supplies each component's entry point. Diagnosis used to classify
# the bridge against whatever was on PATH while --bridge-command pointed somewhere else, because
# the override was wired for one member of this set instead of for the set.
COMMAND_OVERRIDES = {BRIDGE: "bridge_command", RELAY: "relay_command"}

# A registration outcome is either a reading state or a config outcome. So "there is a
# registration" and "classification must stop" are unions of what those two modules answer,
# rather than a respelling of some of their members here.
# Two different answers, and only one of them carries a comparison. LINKED means the file
# registers exactly the command this run asked about. PRESENT means a registration is there and
# nothing was compared, because no expected command was supplied. Collapsed into one set, a
# field whose evidence reads "the configuration registers this exact command" could be reported
# for a host registering something else entirely.
REGISTRATION_EXISTS = (codexconfig.LINKED, reading.PRESENT)
REGISTRATION_COMPARED = (codexconfig.LINKED,)

# What happens to each cell ownership.Signals decides on when the observation behind it does not
# answer. Four outcomes, because they are genuinely different and one uniform rule would be
# wrong about most of them: a reading that returns nothing reaches classification and has to
# stop it; a reading that raises is a named refusal at the boundary; a cell can be legitimately
# negative on absence; and some cells are answered by no observation this command makes.
#
# The check derives its cases from ownership.Signals itself and requires an entry for every
# cell, so a signal added without saying which reading answers it fails the inventory instead of
# quietly going untested.
SIGNAL_UNREADABLE = "class-unreadable"
SIGNAL_REFUSED = "refused"
SIGNAL_NEGATIVE = "legitimate-negative"
SIGNAL_NOT_A_READING = "not-a-reading"

# Each observation is (module, attribute, only_for). One reader answers several questions --
# definition.git reads the component tree, the repository commit and, through
# working_tree_clean, the status -- so 'only_for' names the argument fragment that identifies
# THIS cell's call. Without it a case cannot isolate one cell, and a check that cannot isolate
# one cell cannot tell which reading the classification actually rested on.
SIGNAL_READINGS = {
    "tree_matches": (SIGNAL_UNREADABLE, (("definition", "git", "HEAD:"),)),
    "working_tree_clean": (SIGNAL_UNREADABLE, (("definition", "working_tree_clean", None),)),
    # Composite: a point is only looked up once every dimension of the combination answered.
    "has_point": (SIGNAL_UNREADABLE, (("runtime_install", "codex_cli_version", None),
                                      ("runtime_install", "interpreter_version", None),
                                      ("runtime_install", "this_host", None),
                                      ("runtime_install", "module_location", None))),
    # The digest is read inside a region, so a filesystem failure is a named refusal rather
    # than a classification. Only a returned None reaches the comparison.
    "digest_matches": (SIGNAL_REFUSED, (("definition", "ops12_digest", None),)),
    # An entry point that is not there really is absent; that is an answer, not a gap. TWO
    # readings fill this cell -- where the entry point resolves, and whether the interpreter the
    # record names for it is in a recorded path -- and both are named, because a cell is
    # answered by every reading written into it and not by the first one somebody declared.
    "entry_point_recorded": (SIGNAL_NEGATIVE,
                             (("runtime_install", "resolve_entry_point", None),
                              ("runtime_install", "interpreter_in_recorded_path", None))),
    # Read and reported beside the tree, never used as the identity test (OPS-1.5).
    "commit_matches": (SIGNAL_NOT_A_READING, ()),
    # Conflict cells: the reading answers with a state or a list, and finding none really is
    # "no conflict". A reading that could not be made goes to the collector instead.
    "registration_conflict": (SIGNAL_NEGATIVE, (("runtime_install", "registration_state", None),)),
    "link_conflict": (SIGNAL_NEGATIVE, (("runtime_install", "skill_links", None),)),
    # The owned pointer took a question off the registration. Once the configuration names a
    # stable pointer, LINKED says the configuration names the pointer and no longer says which
    # runtime that is, so this cell asks what the registration stopped asking.
    "pointer_conflict": (SIGNAL_NEGATIVE, (("runtime_install", "pointer_state", None),)),
    # The collector itself, not a cell.
    "unreadable": (SIGNAL_NOT_A_READING, ()),
}

# Naming a cell "not a reading" is a claim about this command, and a claim nobody checks is
# how link_conflict sat empty while skill_links was answering the very question. The check
# verifies the claim: for a cell that names no observation, no function of this command may
# carry that cell's subject in its name. These are the suffixes a cell name adds to its
# subject, so the subject can be recovered mechanically rather than listed.
SIGNAL_SUBJECT_SUFFIXES = ("_matches", "_conflict", "_recorded", "_clean", "_point")
SIGNAL_SUBJECT_PREFIXES = ("has_",)

# The conflict readings a caller of classify_component supplies. Declared because the inventory
# that matters for these cells is the CALLER set, not the cell set: every cell was named and
# checked, and install still promoted over a conflict because it passed neither of these. A
# caller may pass None -- the MCP registration is the bridge's and means nothing for the relay --
# but it says so by passing the keyword, and the classification reports which readings were made.
CONFLICT_READINGS = ("registration", "links", "pointer")

# Every value the promotion decides on, which must be READ INSIDE the promotion lock.
#
# This set exists because three review findings turned out to be one defect arriving three
# times: the swap gate ran against the record loaded before the build, the rollback baseline
# was captured before the build, and the classification read a pointer at the destination
# rather than the one the record names and the swap replaces. A value read before the lock is
# a value another run may have replaced in between, so a judgment made on it is a judgment
# about a state that no longer exists.
#
# Declared rather than remembered, because the previous three got in exactly where nothing was
# looking. The check reads this set, finds the promotion critical section, and fails any member
# that is read there without having been assigned there.
PROMOTION_FRESH = ("fresh", "previous_selection", "gate", "before", "owned_before",
                   "pointer_read", "pointer_path")

# Every answer this command gives about a state as it was FOUND, and the operation each one
# says "there was nothing there" with.
#
# Three review rounds in a row were one thing missing, and it was never a check nobody had
# written: it was a VALUE an answer set could not express. The in-flight cell had no
# established-absent, so a clean host could not promote while the schema cell answered NO_STORE
# about the very same store. The pointer rollback had no restore-to-absence, so a first install
# that failed left a link naming a candidate nothing selected -- and the candidate was then kept
# BECAUSE the pointer named it. The selection rollback had no remove-a-selection-that-had-none,
# so the same first install left its own candidate selected. One shape, three places, three
# rounds.
#
# So the rule is stated at the layer the instances came from. A place that is handed the state
# it found -- see PRIOR_STATE_ARGUMENTS -- is answering about something that may not have been
# there, and its answer set is incomplete until it can say so. The check DERIVES those places
# from the source rather than reading this list, so a new one arrives as a failure instead of as
# a fourth round, and it requires the declared operation both to exist and to be used where the
# answer is given: a capability nothing calls is the same silence as no capability at all.
ABSENCE_ANSWERS = {
    "runtime_install._restore_pointer": ("pointer.remove", "hostrecord.drop_pointer"),
    "runtime_install._restore_selection": ("hostrecord.deselect",),
    "swapgate.inflight_cell": ("swapgate.NO_ATTEMPTS",),
    # The hook's own settings are the fourth place, and the first one that arrived declared
    # rather than as a review round: a write handed the file it found has to be able to say that
    # there was no file, because a first install and an install over somebody else's settings
    # need opposite handling.
    "completion.config_outcome": ("completion.no_configuration",),
}

# How a function says it receives the state as it was found. These are the parameter names the
# three instances used, and they are what the derivation above keys on.
PRIOR_STATE_ARGUMENTS = ("previous", "before", "presence")
UNUSABLE_REGISTRATIONS = tuple(dict.fromkeys(reading.UNUSABLE + (codexconfig.UNREADABLE,)))

# register-mcp's own three answers, beside the ones those modules own. A partial application is
# never a success: left out of the refusal set it would exit 0, and a caller reading only the
# exit status would record a registration as verified that nobody could read back.
APPLIED_UNVERIFIED = "APPLIED_UNVERIFIED"
CHANGED = "CHANGED"
BUSY = "BUSY"
REGISTER_REFUSALS = tuple(dict.fromkeys(
    (codexconfig.CONFLICT,) + UNUSABLE_REGISTRATIONS + (APPLIED_UNVERIFIED, CHANGED, BUSY)))


def now():
    return datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")


def component_of(data, name):
    """The one component entry with this name, looked up rather than compared for at each site.

    Five call sites each carried their own literal, so the component a function meant and the
    string it matched on were two facts that had to be kept equal by hand.
    """
    return next(c for c in data["components"] if c["component"] == name)


def emit(payload):
    print(json.dumps(payload, indent=2, sort_keys=False, default=str))


def acting_process():
    return "runtime_install.py on " + sys.executable


def within(candidate, root):
    """Whether a resolved path is a root or lies under it.

    A string prefix test says yes for /opt/env-other against /opt/env, because it does not
    know where a path component ends. Every identity decision over paths here is containment
    over resolved parts, so a sibling directory sharing a prefix is a different place.
    """
    candidate, root = Path(candidate), Path(root)
    return candidate == root or root in candidate.parents


def refused(command, failed, **extra):
    """Report a reading that failed, as the reading it was.

    The state is the outcome, so an unreadable shape, an unreachable file and an absent one
    stay three answers. The exception type and the line that raised travel with it: a code
    defect reaching this boundary must stay locatable instead of being filed as bad data.
    """
    payload = {"command": command, "refused": failed.detail, "reading": failed.refusal()}
    payload.update(extra)
    emit(payload)
    return EXIT_REFUSED


# ----------------------------------------------------------------- verify-definition

def cmd_verify_definition(args):
    try:
        with reading.region(definition.DEFINITION_PATH, "the component definition"):
            findings = definition.verify(ROOT)
    except reading.Refused as stop:
        return refused("verify-definition", stop.reading)
    emit({
        "command": "verify-definition",
        "definition": str(definition.DEFINITION_PATH.relative_to(ROOT)),
        "findings": findings,
        "ok": not findings,
        "note": (
            "Re-derives every derivable field from this checkout. The upstream tree hash and"
            " the repository commit are not derivable here and are recorded or measured at run"
            " time instead; see the definition's own notes."
        ),
    })
    return EXIT_OK if not findings else EXIT_REFUSED


# ------------------------------------------------------------------------- skill links

def skill_links(codex_home):
    """Read the existing skill-link layer by running its own installer, never by copying it."""
    destination = Path(codex_home) / "skills"
    argv = [sys.executable, str(ROOT / "scripts" / "install.py"), "--check", "--dest", str(destination)]
    try:
        done = subprocess.run(argv, capture_output=True, text=True, timeout=60)
    except (OSError, subprocess.SubprocessError) as error:
        return {"unreadable": type(error).__name__ + ": " + error.__str__(), "command": argv}
    lines = [line for line in (done.stdout + done.stderr).splitlines() if line.strip()]
    return {
        "command": argv,
        "exitCode": done.returncode,
        "linked": [l.split(" ", 1)[1] for l in lines if text_prefix(l, "LINKED ")],
        "missing": [l.split(" ", 1)[1] for l in lines if text_prefix(l, "MISSING ")],
        "conflict": [l.split(" ", 1)[1] for l in lines if text_prefix(l, "CONFLICT ")],
        "legacy": [l.split(" ", 1)[1] for l in lines if text_prefix(l, "LEGACY ")],
        "note": "scripts/install.py is unchanged and was run read-only with --check",
    }


def link_conflict_of(links):
    """The skill-link conflict this command's own reading found, and what it could not read.

    Returns (conflict, unreadable). scripts/install.py --check reports CONFLICT for a path
    this command does not own, and that is an OPS-2.1 conflict signal like a differing MCP
    registration. The reading existed and its answer was thrown away: classification consulted
    a link_conflict cell that nothing ever filled.
    """
    if links is None:
        return None, None
    if links.get("unreadable"):
        return None, "the skill-link layer (" + str(links["unreadable"]) + ")"
    found = links.get("conflict") or []
    if not found:
        return None, None
    return ("scripts/install.py --check reports a foreign skill path: "
            + "; ".join(str(path) for path in found)), None


# ------------------------------------------------------------------- the owned pointer

def pointer_conflict_of(read):
    """Whether the owned pointer names something the record does not select.

    Returns (conflict, unreadable), the shape link_conflict_of uses, because the two cells
    answer the same kind of question and a caller that made no reading is distinguishable from
    one that found no conflict either way.
    """
    if read is None:
        return None, None
    state = read.get("state")
    if state == pointer.NO_POINTER:
        # No pointer has been placed here. That is a host this command has not registered a
        # stable path for, and it is an answer rather than a gap.
        return None, None
    if not pointer.usable(state):
        return None, ("the owned pointer (" + str(state) + "): " + str(read.get("detail")))
    if read.get("agrees") is None:
        return None, ("the owned pointer names " + str(read.get("target")) + " and whether the"
                      " recorded selection lies under it could not be established: "
                      + str(read.get("detail")))
    if read.get("agrees"):
        return None, None
    return ("the owned pointer names " + str(read.get("target")) + ", which does not contain"
            " the runtime this host record selects (" + ", ".join(read.get("outside") or [])
            + "), so the command a host reaches is not the one that was promoted"), None


def pointer_state(pointer_path, record, data):
    """Read the owned pointer, and compare what it names with what the record selects.

    Takes the pointer PATH rather than a destination, because those are not always the same
    place: a record can name a pointer under an earlier destination, and a run invoked with a
    different --dest then classified a link at the new destination while the promotion went on
    to replace the recorded one. A judgment about a link that is not the link being replaced is
    a judgment about the wrong thing.

    The question is deliberately about the pointer against the RECORD and not against whatever
    a run is about to promote. Before a swap the pointer still names the predecessor, which the
    record also selects, so the two agree; a pointer somebody repointed by hand disagrees at
    every moment. Asked the other way the cell would report a conflict during every update.
    """
    path = Path(pointer_path)
    read = dict(pointer.read(path))
    read["pointer"] = str(path)
    read["agrees"] = None
    read["outside"] = []
    if read["state"] != pointer.LINK:
        return read
    try:
        root = Path(read["target"])
        if not root.is_absolute():
            root = Path(path).parent / root
        root = root.resolve()
    except reading.RESOLVE_FAILURES as error:
        # Below 3.11 a symlink loop comes out of pathlib as RuntimeError rather than
        # OSError(ELOOP), so catching only OSError answered with a reading on 3.13 and let a
        # traceback out of the whole diagnosis on this repository's own floor.
        read["detail"] = ("the pointer's target could not be resolved: "
                          + type(error).__name__ + ": " + str(error))
        return read
    read["targetResolves"] = str(root)

    selected = (record or {}).get("selected") or {}
    named = [selected.get(c["component"]) for c in data["components"]]
    named = [location for location in named if location]
    if not named:
        # A pointer exists and this record selects nothing for it to agree with. That is not
        # agreement and it is not a clean host: it is a pointer this command cannot account
        # for, and repointing one of those is how somebody else's link gets hijacked.
        read["detail"] = ("a pointer is placed here and the host record selects no runtime for"
                          " it, so what it names could not be checked against anything")
        return read
    outside = []
    for location in named:
        try:
            if not within(Path(location).resolve(), root):
                outside.append(str(location))
        except reading.RESOLVE_FAILURES as error:
            read["detail"] = ("a recorded selection could not be resolved: "
                              + type(error).__name__ + ": " + str(error))
            return read
    read["outside"] = outside
    read["agrees"] = not outside
    return read


def protected_environment(record, environment, destination, data, pointer_path=None):
    """Whether an environment is in use, so a later run must not remove it.

    Two readings and they are reported as two: the record's selection, and the pointer on disk.
    Either of them naming the environment protects it, and so does either of them failing to
    answer, because an environment nobody could establish as free is not an environment that is
    free. That direction is the safe one: the cost of keeping a directory is a named residual
    path, and the cost of removing a live one is the accident this exists to prevent.

    'pointer_path' is the link to read, for a caller that KNOWS which link this host reaches a
    runtime through. A caller that knows only a destination lets the deterministic path be
    derived from it, which is what a run choosing where to PUT a link has. Derived from a
    destination alone, a recorded pointer whose basename is not the default one named a link
    nobody placed, and an environment the real pointer still reaches read as unprotected. The
    settle path is the second such caller: a promotion keeps using the path the record names,
    which survives a run invoked against a different --dest.
    """
    selects = None
    if record is not None:
        selected = (record.get("selected") or {}).values()
        try:
            root = Path(environment).resolve()
            selects = any(within(Path(location).resolve(), root)
                          for location in selected if location)
        except reading.RESOLVE_FAILURES:
            selects = None
    names = pointer.names(Path(pointer_path) if pointer_path
                          else pointer.pointer_path(destination), environment)
    protected = selects is not False or names is not False
    return protected, {
        "recordSelectsIt": selects,
        "pointerNamesIt": names,
        "detail": (
            "the host record selects it" if selects else
            "the owned pointer names it" if names else
            "neither the record nor the pointer could be read for it" if (
                selects is None or names is None) else
            "neither the record nor the pointer names it"
        ),
    }


# --------------------------------------------------------- what the store and candidate hold

# Read read-only through the relay's own reader, which opens the database with mode=ro and runs
# no schema script, so asking the question does not create the store the question is about.
# Absence is established by looking at the path FIRST: a failed open also answers for a
# permission failure and for a locked database, and neither of those means nothing is there.
#
# WHAT is asked is swapgate's and is embedded here rather than written again. The two sides of
# this comparison asking different questions would arrive as a schema difference and be refused
# as one, so the question is one value with two readers rather than two copies kept equal by hand.
_STORE_TABLES_PROGRAM = """
import json, os, sys
from codex_session_relay.store import resolve_state_dir, read_only_rows

selection = resolve_state_dir(sys.argv[1] or None, sys.argv[2] or None)
database = selection.db_path
try:
    os.lstat(str(database))
except FileNotFoundError:
    print(json.dumps({"readable": True, "present": False, "dbPath": str(database),
                      "objects": None, "detail": None}))
    raise SystemExit(0)
except OSError as error:
    print(json.dumps({"readable": False, "present": None, "dbPath": str(database),
                      "objects": None,
                      "detail": type(error).__name__ + ": " + str(error)}))
    raise SystemExit(0)
answer = read_only_rows(selection, """ + repr(swapgate.SCHEMA_OBJECTS_QUERY) + """)
if not answer["readable"] or answer["detail"]:
    print(json.dumps({"readable": False, "present": True, "dbPath": str(database),
                      "objects": None,
                      "detail": answer["detail"] or "the store could not be read"}))
    raise SystemExit(0)
print(json.dumps({"readable": True, "present": True, "dbPath": str(database),
                  "objects": {row["object"]: row["sql"] for row in answer["rows"]},
                  "detail": None}))
"""

# The candidate's schema comes from its own DDL applied to an in-memory database, so nothing is
# created anywhere and the answer is the schema that relay would actually install. It asks the
# same question the store side asks, from the same value.
#
# The script alone is NOT that schema. Store.__init__ runs it and THEN applies GUARD_INDEXES one
# at a time, so a store any runtime has opened read-write holds those as well, and a candidate
# that stopped at the script declared six objects fewer than the runtime installs. Once a host
# had opened its store even once, storeSchema read NARROWS and the gate refused every promotion
# after it -- permanently, because the refusal correctly left the store alone, so the next
# attempt met the same difference. Applied here FROM THE SAME TUPLE rather than from a copy of
# their SQL, so revising an index cannot drift the declaration and the installation apart again.
# The in-memory database is fresh, so the rows that can make one of these fail on a live store
# do not exist here and a failure would be a real defect rather than the tolerated case.
_CANDIDATE_TABLES_PROGRAM = """
import json, sqlite3
from codex_session_relay import store

database = sqlite3.connect(":memory:")
database.executescript(store.DDL)
for _name, _statement in store.GUARD_INDEXES:
    database.execute(_statement)
rows = database.execute(""" + repr(swapgate.SCHEMA_OBJECTS_QUERY) + """).fetchall()
print(json.dumps({"readable": True, "objects": {row[0]: row[1] for row in rows},
                  "schemaVersion": store.SCHEMA_VERSION, "detail": None}))
"""


def _asked(argv, what, timeout=120):
    """Run one probe and return its parsed answer, or say why there is none."""
    try:
        done = subprocess.run(argv, capture_output=True, text=True, timeout=timeout)
    except (OSError, subprocess.SubprocessError) as error:
        return {"readable": False, "command": argv, "objects": None, "present": None,
                "detail": what + " could not be asked: " + type(error).__name__ + ": "
                          + error.__str__()}
    try:
        answer = json.loads(done.stdout)
    except ValueError:
        return {"readable": False, "command": argv, "objects": None, "present": None,
                "detail": what + " did not answer with JSON: "
                          + (done.stderr or done.stdout).strip()[-400:]}
    answer["command"] = argv
    return answer


# Whether a store is THERE, settled by looking at the path and opening nothing. The relay
# reports contents unavailable both for a store that is missing and for one it cannot read, so
# the in-flight cell cannot tell those apart from doctor alone -- and they are opposite answers
# for it. Established absence means no attempt can be open; an unreadable store means the count
# is unknown.
_STORE_PRESENCE_PROGRAM = """
import json, os, sys
from codex_session_relay.store import resolve_state_dir

selection = resolve_state_dir(sys.argv[1] or None, sys.argv[2] or None)
database = selection.db_path
try:
    os.lstat(str(database))
except FileNotFoundError:
    print(json.dumps({"readable": True, "present": False, "dbPath": str(database),
                      "detail": None}))
    raise SystemExit(0)
except OSError as error:
    print(json.dumps({"readable": False, "present": None, "dbPath": str(database),
                      "detail": type(error).__name__ + ": " + str(error)}))
    raise SystemExit(0)
print(json.dumps({"readable": True, "present": True, "dbPath": str(database), "detail": None}))
"""


def store_presence(interpreter, state=None, socket_path=None):
    """Whether a store exists at the selection the relay itself resolves.

    Asked of the relay rather than of this checkout, because which path a selection resolves to
    is the relay's rule. Nothing is opened: the answer comes from the path.
    """
    argv = [str(interpreter), "-c", _STORE_PRESENCE_PROGRAM, str(state or ""),
            str(socket_path or "")]
    return _asked(argv, "whether a store exists")


def store_tables(interpreter, state=None, socket_path=None):
    """The schema objects the store actually holds, read under the relay that owns the rule.

    Asked of this checkout instead, the answer would describe a copy of a schema the selected
    installation owns rather than the schema it will run.

    Every object the catalog reports, keyed by kind and name: a store's indexes, triggers and
    views are part of its schema and an update that replaces the runtime over them has to see
    them.
    """
    argv = [str(interpreter), "-c", _STORE_TABLES_PROGRAM, str(state or ""),
            str(socket_path or "")]
    return _asked(argv, "the store's tables")


def candidate_tables(interpreter):
    """The schema objects the candidate declares, read under the candidate's own interpreter."""
    argv = [str(interpreter), "-c", _CANDIDATE_TABLES_PROGRAM]
    return _asked(argv, "the candidate's declared tables")



# ------------------------------------------------------------------------- ownership

def resolve_entry_point(console_script, override=None):
    if override:
        # A bare name is resolved the way the shell resolves it, because scope.relay hands
        # the same token to subprocess and gets PATH lookup. Treating it as a relative path
        # reports the executable as foreign while diagnosis runs it successfully, so the
        # ownership result would describe something other than what was exercised.
        if os.sep not in str(override) and not text_prefix(override, "."):
            found = shutil.which(str(override))
            return Path(found) if found else Path(override)
        return Path(override)
    found = shutil.which(console_script)
    return Path(found) if found else None


def interpreter_of(entry_point):
    """The interpreter named by a console script's first line.

    This is the fallback, not the answer. A console script has more than one written shape: pip
    emits a direct '#!<python>' shebang when the path allows it, and a '#!/bin/sh' trampoline
    that execs the real interpreter on a following line when it does not, which is what a
    destination containing a space produces. Reading the first line answers '/bin/sh' for the
    second shape, and nothing can be asked of that. interpreter_for is the caller's entry point;
    this stays for a script this command did not create, where there is no recorded environment
    to ask instead.
    """
    if entry_point is None:
        return None
    try:
        first = Path(entry_point).read_text(encoding="utf-8", errors="replace").splitlines()[0]
    except (OSError, IndexError):
        return None
    return first[2:].strip() if text_prefix(first, "#!") else None


def interpreter_version(python):
    try:
        done = subprocess.run(
            [str(python), "-c", "import platform;print(platform.python_version())"],
            capture_output=True, text=True, timeout=30,
        )
    except (OSError, subprocess.SubprocessError):
        return None
    return done.stdout.strip() if done.returncode == 0 else None


def module_location(python, module):
    """Where the interpreter actually imports this module from. Never assumed.

    An editable install leaves nothing under site-packages and a copied one does, so the
    only honest answer comes from the interpreter itself (OPS-1.1).
    """
    code = "import " + module + " as m, os; print(os.path.dirname(m.__file__))"
    argv = [str(python), "-c", code]
    try:
        done = subprocess.run(argv, capture_output=True, text=True, timeout=60)
    except (OSError, subprocess.SubprocessError) as error:
        return None, type(error).__name__ + ": " + error.__str__(), argv
    if done.returncode != 0:
        return None, (done.stderr.strip().splitlines() or ["import failed"])[-1], argv
    return done.stdout.strip(), None, argv


def recorded_roots(record, name):
    """Paths the definition or this host's record already accounts for.

    The source checkout is one of them, but an installation this command produced lives
    wherever its destination was, which is normally outside the checkout. Treating only
    the checkout as recorded would classify every runtime it installs as foreign for
    ever, and nothing would ever become reusable. Source-checkout identity and
    installed-runtime identity stay separate readings; this only decides which paths are
    accounted for.
    """
    roots = [ROOT.resolve()]
    # Interpreting a recorded string as a path is a read of the record, so it sits inside the
    # boundary: a stored value that cannot name a path is an unreadable record, not a crash
    # in the middle of classification.
    with reading.region("the host record", "the paths an install recorded",
                        field="components[].installs[].environment|location"):
        for install in ((record or {}).get("components", {}).get(name) or {}).get("installs", []):
            for key in ("environment", "location"):
                value = install.get(key)
                if value:
                    roots.append(Path(value).resolve())
    return roots


def recorded_install_for(record, name, entry_point):
    """The install this command recorded for THIS entry point, or nothing rather than a guess.

    Returns (install, ambiguity). The recorded entry point is matched first, because that is the
    exact fact and it cannot be shared. Containment is the compatibility path for a record that
    predates entryPoint, and it is not first-match: --dest is arbitrary, so one environment can
    legally sit inside another's destination and an entry point under the inner one is contained
    by both. The innermost environment is the one that owns it. Two installs recorded against
    that same environment naming DIFFERENT interpreters own it equally, and choosing by order
    would pick an interpreter that installed something else, so that is reported rather than
    resolved.

    Interpreting a recorded string as a path is a read of the record, so it sits inside the
    boundary for the same reason recorded_roots does.
    """
    if not entry_point:
        return None, None
    with reading.region("the host record", "the environment an install recorded",
                        field="components[].installs[].entryPoint|environment"):
        candidate = Path(entry_point).resolve()
        installs = ((record or {}).get("components", {}).get(name) or {}).get("installs", [])
        for install in installs:
            recorded_entry = install.get("entryPoint")
            if recorded_entry and Path(recorded_entry).resolve() == candidate:
                return install, None
        containing = []
        for install in installs:
            environment = install.get("environment")
            if environment:
                resolved = Path(environment).resolve()
                if within(candidate, resolved):
                    containing.append((len(resolved.parts), str(resolved), install))
    if not containing:
        return None, None
    deepest = max(depth for depth, _path, _install in containing)
    innermost = [install for depth, _path, install in containing if depth == deepest]
    named = {str(install.get("interpreterPath")) for install in innermost
             if install.get("interpreterPath")}
    if len(named) > 1:
        return None, ("the host record names " + str(len(named)) + " different interpreters for "
                      + str(entry_point) + ", so which one installed it cannot be established: "
                      + ", ".join(sorted(named)))
    return innermost[0], None


# Where interpreter_for's answer came from. Declared rather than spelled at each site, because
# one consumer decides an OWNERSHIP cell on the difference between them: an interpreter the
# record names was written by the run that installed it, and a first line is whatever somebody
# put in a file.
INTERPRETER_RECORDED = "recorded with the install"
INTERPRETER_FROM_ENVIRONMENT = "the recorded environment"
INTERPRETER_FROM_SHEBANG = "the script's first line"


def interpreter_for(record, name, entry_point):
    """The interpreter a console script runs under, and where that answer came from.

    An interpreter's identity is not one line of a script. Reading the shebang answered
    '/bin/sh' for the trampoline pip writes when a destination contains a space, so
    interpreter_version and module_location had nothing runnable to ask, the component
    classified 'unreadable', and install deleted the environment it had just built as though it
    belonged to somebody else.

    For a script this command created the answer comes from the install record, written by the
    run that used that interpreter, and it is confirmed by running it downstream: a value that
    cannot report its version or locate the module still produces no classification. That is
    independent of how the script happens to be written. The shebang stays the answer only
    where there is no recorded environment to ask.

    Returns (path, source). The source travels with it so the report says which evidence the
    classification rested on, and an ambiguous record yields no interpreter at all rather than
    whichever one came first.
    """
    install, ambiguous = recorded_install_for(record, name, entry_point)
    if ambiguous:
        return None, ambiguous
    if install:
        recorded = install.get("interpreterPath")
        if recorded:
            return str(recorded), INTERPRETER_RECORDED
        environment = install.get("environment")
        if environment:
            # Records written before interpreterPath existed still name the environment, and
            # the interpreter of an environment this command built is the one it created there.
            return str(Path(environment) / "bin" / "python"), INTERPRETER_FROM_ENVIRONMENT
    return interpreter_of(entry_point), INTERPRETER_FROM_SHEBANG


# The interpreter sources the RECORD stands behind. Both were written by a run of this command
# into the host record; the first line of a script was not, and a wrapper this command never
# created carries whatever first line its author wrote.
RECORDED_INTERPRETER_SOURCES = (INTERPRETER_RECORDED, INTERPRETER_FROM_ENVIRONMENT)


def interpreter_in_recorded_path(python, roots):
    """Whether the interpreter a console script runs under lives in a recorded path.

    Its own reading, and named, because it fills the same cell resolve_entry_point fills. A
    cell is answered by the readings it declares, and this one was a second write site nobody
    had to declare: an entry point outside every recorded root reached the ownership cell
    through it on the strength of a first line, which interpreter_of already says is the
    fallback rather than the answer.
    """
    with reading.region("the installed entry point", "the interpreter it names",
                        field="shebang"):
        interpreter_path = Path(python).resolve()
    return any(within(interpreter_path, r) for r in roots)


# The checkout artifacts this command EXECUTES to produce a component's exercise, named as
# definition fields rather than as paths. A claim rests on its instrument, and this one was in
# no recorded dimension: exerciseScript lives outside packageLocation, so a modified smoke
# check produced a point that named only the clean installed bytes and went on matching once
# the check was restored.
INSTRUMENT_FIELDS = ("exerciseScript",)
NO_CHECKOUT_INSTRUMENT = "none: exercised through its own installed entry point"


def instrument_digest(component):
    """The bytes of every checkout artifact this command runs for this component, or None.

    One helper, called by the writer in measure_candidate and by the reader in
    classify_component, so the value a point carries is the value a later run asks with. A
    component exercised only through its own installed entry point answers with a declared
    token: that is an answer, not a missing value, and the installed bytes are already a
    dimension of their own.

    None means the instrument could not be read, which is an unread signal and not a value.
    """
    paths = sorted(str(component[field]) for field in INSTRUMENT_FIELDS if component.get(field))
    if not paths:
        return NO_CHECKOUT_INSTRUMENT
    digest = hashlib.sha256()
    for relative in paths:
        digest.update(relative.encode("utf-8") + b"\0")
        try:
            digest.update((ROOT / relative).read_bytes())
        except OSError:
            return None
    return digest.hexdigest()


def classify_component(component, *, record, entry_override=None, registration=None,
                       record_state=None, app_server=None, links=None, pointer=None):
    """Gather the four OPS-2.1 signals and classify."""
    # Every signal below is filled by the reading its own question produced. A reading that did
    # not answer leaves its cell empty and names itself here instead of being flattened into a
    # boolean, which is how a git read nobody could perform reported a fork.
    judged = ownership.Judgement()
    unreadable = judged.unreadable
    entry = resolve_entry_point(component["consoleScript"], entry_override)
    resolved = entry.resolve() if entry and entry.exists() else None

    roots = recorded_roots(record, component["component"])
    entry_recorded = bool(resolved and any(within(resolved, r) for r in roots))
    python, interpreter_from = (
        interpreter_for(record, component["component"], resolved) if resolved else (None, None))
    if resolved and python is None and interpreter_from:
        # An ambiguous record is a signal that could not be read, not a reason to pick one.
        unreadable.append(interpreter_from)
    if (resolved and not entry_recorded and python
            and interpreter_from in RECORDED_INTERPRETER_SOURCES):
        # The second write site of this cell, and it answers only from an interpreter the
        # RECORD names. Reached from the fallback first line it let an external wrapper decide
        # ownership: a script outside every recorded root, whose author wrote a shebang naming
        # an interpreter inside a recorded environment, classified as this installation.
        entry_recorded = interpreter_in_recorded_path(python, roots)

    if python is None and resolved is None:
        python = sys.executable
    # The interpreter version is a dimension every point is compared on, so an interpreter that
    # did not answer is an unread signal. Left as None it skipped the point lookup instead,
    # which reported 'unmeasured' -- a claim that nothing covers this run, from a reading that
    # never happened.
    version = judged.answer(interpreter_version(python),
                            what="the interpreter version of " + str(python)) if python else None
    location, import_error, import_command = (None, "no interpreter to ask", None)
    if python:
        location, import_error, import_command = module_location(python, component["module"])

    current_digest = None
    digest_matches = None
    if location and Path(location).is_dir():
        with reading.region(location, "the installed bytes of " + component["component"],
                            field="importedLocation"):
            current_digest = definition.ops12_digest(location)
        digest_matches = judged.compare(
            current_digest, component["sourceDigest"],
            what="the installed bytes of " + component["component"])
    elif resolved is not None:
        unreadable.append("the installed package location for " + component["component"])

    # The component's subdirectory tree is the identity test, because packages share a
    # repository commit and a commit therefore cannot say whether this component changed
    # (OPS-1.5). The repository commit is read and reported beside it, but it is
    # informational: an unrelated commit must not turn an unchanged component into a fork.
    tree_matches = None
    if entry_recorded:
        tree_matches = judged.compare(
            definition.git(["rev-parse", "HEAD:" + component["subdirectory"]], ROOT),
            component["subdirectoryTree"],
            what="the recorded subdirectory tree of " + component["component"])
    repository_commit = definition.git(["rev-parse", "HEAD"], ROOT)
    recorded_commit = ((record or {}).get("components", {}).get(component["component"]) or {}) \
        .get("repositoryCommit")
    commit_matches = None
    clean = None
    if entry_recorded:
        clean = judged.answer(definition.working_tree_clean(ROOT),
                              what="the working tree cleanliness of " + str(ROOT))

    points = []
    # A dimension that could not be read is not a dimension that agrees. Passing None through
    # would drop it from the comparison entirely, and a point measured under another Codex CLI
    # or another App Server would then carry this component to 'own' (OPS-1.3, OPS-2.1).
    codex_cli = judged.answer(codex_cli_version(), what="the Codex CLI version")
    host_name = judged.answer(this_host(), what="this host's name")
    app_server = judged.answer(app_server, what="the App Server identity")
    # The instrument this component's exercise runs from, read here by the same helper the
    # measurement writes with. Unread, it is an unread signal like any other: passing None
    # through would drop the dimension and let a point measured with a different smoke check
    # carry the component to 'own'.
    instrument = judged.answer(instrument_digest(component),
                               what="the instrument that exercises " + component["component"])
    if record is None:
        # Which failure it was, not merely that there was one.
        unreadable.append("the host record (" + str(record_state or reading.UNREADABLE) + ")")
    elif (codex_cli is not None and app_server is not None and host_name is not None
          and instrument is not None and location and version):
        # Each unreadable signal is recorded on its own. Reporting only the first would hide
        # the others, and every one of them independently stops the classification.
        points = hostrecord.points_for(
            record, component["component"], location=location,
            interpreter=version, install_digest=current_digest,
            codex_cli=codex_cli, app_server=app_server, host=host_name,
            exercise_digest=instrument,
        )

    conflict = None
    if registration and registration.get("outcome") == codexconfig.CONFLICT:
        conflict = "the Codex configuration registers " + MCP_NAME + " differently: " + registration["detail"]
    if registration and registration.get("outcome") in UNUSABLE_REGISTRATIONS:
        # Both answers stop classification. Testing one member of the partition said yes to a
        # malformed configuration and no to one that could not be reached at all, and the second
        # of those then reached classification as though the file had been read.
        unreadable.append("the Codex configuration (" + str(registration.get("outcome")) + "): "
                          + str(registration.get("detail")))

    # This command's own reading of the skill-link layer. A CONFLICT there is an OPS-2.1
    # conflict exactly as a differing MCP registration is, and a layer that could not be read
    # is an unread signal. The cell existed and nothing filled it.
    link_conflict, link_unreadable = link_conflict_of(links)
    if link_unreadable:
        judged.note(link_unreadable)

    # The owned pointer, read by the caller and passed here the way the other conflict readings
    # are. A pointer that names something the record does not select is a user-facing command
    # resolving into a runtime this command never promoted, which is the OPS-2.1 conflict the
    # registration comparison stopped being able to see.
    pointer_conflict, pointer_unreadable = pointer_conflict_of(pointer)
    if pointer_unreadable:
        judged.note(pointer_unreadable)

    signals = ownership.Signals(
        entry_point_recorded=entry_recorded,
        commit_matches=commit_matches,
        tree_matches=tree_matches,
        working_tree_clean=clean,
        digest_matches=digest_matches,
        has_point=bool(points),
        registration_conflict=conflict,
        link_conflict=link_conflict,
        pointer_conflict=pointer_conflict,
        unreadable=unreadable,
    )
    classification, reasons = ownership.classify(signals)
    return {
        "component": component["component"],
        "class": classification,
        "reasons": reasons,
        "entryPoint": str(entry) if entry else None,
        "entryPointResolves": str(resolved) if resolved else None,
        "entryPointInRecordedPath": entry_recorded,
        "interpreter": version,
        "interpreterPath": python,
        "interpreterFrom": interpreter_from,
        "linkConflict": link_conflict,
        "pointerConflict": pointer_conflict,
        # Three separate fields, because registering a pointer split one question into three.
        # What the configuration registers, what the pointer names, and where the entry point
        # actually resolves are no longer the same fact and none of them is read from another.
        "pointerTarget": (pointer or {}).get("target"),
        "pointerState": (pointer or {}).get("state"),
        # Which conflict readings this caller made at all. "No conflict was found" and "nobody
        # looked" are different answers, and one hand-written flag for one cell did not
        # generalise: the caller that moves the selection was passing neither.
        "conflictsRead": {"registration": registration is not None, "links": links is not None,
                          "pointer": pointer is not None},
        "importedLocation": location,
        "importError": import_error,
        "importCommand": import_command,
        "digestMatches": digest_matches,
        "installedDigest": current_digest,
        "repositoryCommit": repository_commit,
        "repositoryCommitRecordedAtInstall": recorded_commit,
        "repositoryCommitDrift": (
            # Unknown when either side is missing. A repository commit nobody could read is not
            # a commit that differs, and reporting True for it is the same flattening as a tree
            # comparison against an unmade reading.
            None if not recorded_commit or repository_commit is None
            else recorded_commit != repository_commit
        ),
        "repositoryCommitMeaning": (
            "reported, not used as the identity test. The component subdirectory tree decides"
            " whether this component changed (OPS-1.5); an unrelated commit does not."
        ),
        "measuredPoints": len(points),
        "reusable": ownership.reusable(classification),
    }


# ------------------------------------------------------------------------- diagnose

def read_config(codex_home):
    """The configuration text, as a reading.

    Decoding is part of reading it: a file that is not UTF-8 raised straight through this
    function before, so a configuration nobody could read arrived as a traceback rather than
    as the refusal it is.
    """
    path = Path(codex_home) / "config.toml"
    return path, reading.read_text(path, "the Codex configuration", absent="")


def registration_state(codex_home, command, args, name=MCP_NAME, compare_args=True):
    """What the configuration registers, and only compared when a command was supplied.

    Diagnosis with no expected command must not invent one. Comparing an existing, correct
    registration against an empty string reports CONFLICT for a host that is registered
    exactly right, and that false conflict then drags the component and the installed
    result down with it.

    compare_args is for a caller that has an expectation about the command and none about the
    arguments. install knows which entry point it is promoting and knows nothing about the
    arguments a host chose, and an empty list is not "no expectation": it is the expectation
    that there are none, which reports a conflict for a registration that is correct and merely
    carries supported bridge arguments.
    """
    path, config = read_config(codex_home)
    if not config.usable:
        # The state IS the outcome, so 'the file could not be decoded' and 'the file could
        # not be reached' stay two answers here rather than collapsing into UNREADABLE.
        return {"path": str(path), "outcome": config.state, "detail": config.detail,
                "wouldWrite": False, "registered": None, "reading": config.refusal()}
    text = config.value
    view = codexconfig.scan(text)
    if not view.readable:
        return {"path": str(path), "outcome": "UNREADABLE",
                "detail": "; ".join(view.unreadable), "wouldWrite": False,
                "registered": None}
    present, registered = codexconfig.registration_of(view, name)
    if not command:
        return {
            "path": str(path),
            "outcome": "PRESENT" if present else "ABSENT",
            "detail": ("the configuration registers " + repr((registered or {}).get("command"))
                       + "; no expected command was supplied, so nothing was compared")
                      if present else "no registration for " + name,
            "wouldWrite": False,
            "registered": registered,
        }
    if not compare_args:
        # The caller expects this command and has no expectation about arguments, so the
        # arguments compare against themselves and only the command decides.
        args = list((registered or {}).get("args") or [])
    new_text, outcome, detail = codexconfig.register(text, name, command, args)
    return {"path": str(path), "outcome": outcome, "detail": detail,
            "comparedArguments": compare_args,
            "wouldWrite": new_text != text, "registered": registered}


def cmd_diagnose(args):
    codex_home = Path(args.codex_home or os.environ.get("CODEX_HOME") or Path.home() / ".codex")
    record_path = Path(args.record) if args.record else hostrecord.record_path()
    try:
        with reading.region(definition.DEFINITION_PATH, "the component definition"):
            data = definition.load()
    except reading.Refused as stop:
        return refused("diagnose", stop.reading)
    host_record = hostrecord.load(record_path, data["definitionVersion"])
    record = host_record.value if host_record.usable else None

    bridge = component_of(data, BRIDGE)
    relay_component = component_of(data, RELAY)

    registration = registration_state(codex_home, args.bridge_command or "", args.bridge_arg or [])
    # One read-only observation, shared by every component's classification. Without it the
    # App Server dimension is unread and no recorded point can be said to cover this run.
    bridge_entry = resolve_entry_point(bridge["consoleScript"], args.bridge_command)
    app_server = observe_app_server(
        interpreter_for(record, BRIDGE, bridge_entry)[0] or sys.executable, args.socket)
    # Read before classification, not after it. This command already ran scripts/install.py
    # --check and reported the result; it just did so once the classes were decided, so a
    # foreign skill path never reached the conflict signal it exists to raise.
    links = skill_links(codex_home)
    # The owned pointer, where a destination was named. Without one there is no pointer in
    # scope to read, and the caller says so by passing None rather than by omitting the
    # keyword: no conflict found and nobody looked are different answers.
    destination = getattr(args, "dest", None)
    # Settled once, here, and used by everything below that compares paths. A --dest spelled
    # relatively is a different string from the absolute path an install records, and two
    # readings of the same destination in two spellings compare unequal: the fallback pointer
    # was built from the raw spelling while the survey root was absolute, so a pointer sitting
    # inside the surveyed destination was reported as belonging to another one and never
    # inspected at all.
    # Path.expanduser() raises RuntimeError -- not OSError -- for a ~user this host cannot
    # resolve, and --dest is operator input rather than an internal fault. Losing the whole
    # diagnostic payload to it was the worst of both answers: nothing diagnosed, and nothing
    # said about the single input that failed. A spelling that cannot be expanded becomes a
    # reading here, the way every other unreadable spelling this command meets already does.
    # A LIST, because this command can fail to settle a destination twice -- an unresolvable
    # --dest beside a recorded pointer that names none -- and keeping only the last of those
    # loses why the destination the operator actually named was never scanned.
    settled_destination, unreadable_destination = None, []
    # Asked as "was the option given", not "is its string non-empty". --dest '' is a spelling
    # and not an absence: the installer settles it to the current directory, and judging it by
    # truthiness made this command read the one destination the operator did name as no
    # destination at all.
    if destination is not None:
        try:
            settled_destination = Path(destination).expanduser().absolute()
        # OSError as well as RuntimeError: absolute() reads the current working directory for a
        # relative spelling, and a working directory that has been removed answers ENOENT. Both
        # are one answer here -- this command could not settle the path it was given.
        except (OSError, RuntimeError) as error:
            unreadable_destination.append(
                "the destination named by --dest could not be settled: " + str(destination)
                + ": " + type(error).__name__ + ": " + str(error))
    # The recorded pointer first: that is the link this host actually reaches a runtime
    # through, and a --dest supplied here only names where to look when nothing is recorded.
    owned_pointer = ((record or {}).get("pointer") or {}).get("path")
    if not owned_pointer and settled_destination:
        owned_pointer = str(pointer.pointer_path(settled_destination))
    # Only an ABSOLUTE recorded pointer names a link this command can read. hostrecord.shape
    # accepts any string for it, and a relative one resolves against THIS process's working
    # directory, so every reading taken from it would be about whatever sits beside the
    # diagnosis rather than about this host's installation.
    recorded_names_a_destination = bool(owned_pointer) and Path(owned_pointer).is_absolute()
    if owned_pointer and not recorded_names_a_destination:
        unreadable_destination.append(
            "the host record's pointer path is not absolute (" + str(owned_pointer) + "), so it"
            " names no link this command can read and no destination it can survey")
    # Answered from the recorded link or not at all. A component classified against a pointer
    # read from the working directory is classified against another installation's link.
    pointer_read = (pointer_state(owned_pointer, record, data)
                    if recorded_names_a_destination else None)
    # What a run left behind on this destination. Asked here because `residualPaths` used to
    # live only on a failed install's own result, so an operator who wanted the cleanup warning
    # after a failed update had to have kept that run's stdout; the procedure said so, and this
    # is the reading it was missing. The destination comes from --dest, or from the directory
    # the recorded pointer sits in when no --dest was given, because that is the destination
    # this host actually reaches a runtime through.
    # Settled to the absolute form an install records, because the boundary check below
    # compares this with a pointer parent. A --dest spelled relatively kept that spelling here
    # while the record holds an absolute path, so an owned dangling pointer under the very
    # destination being surveyed compared unequal and was reported as another installation's.
    # A destination that was SUPPLIED and could not be read is not an omitted one. Falling
    # back here scanned the recorded pointer's own directory and could have filled
    # residualPaths from an installation the operator never named, inside the same answer
    # that reported the destination they did name as unreadable. The pointer is the fallback
    # for a missing --dest, never for an unusable one.
    residue_root = (settled_destination if destination is not None
                    else (Path(owned_pointer).parent if recorded_names_a_destination else None))
    # Which directory the protection reading asks about. The pointer the HOST reaches a runtime
    # through is the recorded one, and it does not have to sit under the destination this run
    # was invoked with.
    pointer_home = (Path(owned_pointer).parent if recorded_names_a_destination
                    else residue_root)

    def ownership_of(environment):
        """The caller's ownership reading, which residue never takes for itself.

        protected_environment is deliberately conservative -- it answers protected when either
        the record or the pointer names the environment AND when either of those readings
        failed -- and 'selected' is True only when the record was read and names it. Both are
        passed through unchanged, because the decision they feed is the installer's own.

        It is asked about the RECORDED pointer's own directory, not about this survey's root.
        protected_environment derives the pointer from the destination it is handed, and
        cmd_install deliberately reuses a previously recorded pointer across a destination
        change, so handing it --dest asked about a pointer the host does not use: an
        environment the real pointer still reaches, under a record that does not select it,
        classified as reclaimable while a live process was running out of it.
        """
        # The recorded link itself where there is one, not a link derived from its directory.
        # A recorded pointer whose basename is not the default one names a link nobody would
        # reconstruct, and an environment that link still reaches then read as unprotected --
        # which is the one reading here that authorises removal.
        protected, detail = protected_environment(
            record, environment, pointer_home, data,
            pointer_path=owned_pointer if recorded_names_a_destination else None)
        return protected, detail["recordSelectsIt"]

    residual = residue.survey(
        residue_root,
        # A pointer that names no link this command can read is not handed to the survey at
        # all, whether or not --dest was supplied: read against the working directory, the
        # boundary check accepted it as belonging to an explicit destination equal to that
        # directory, and the relative string could reach residualPaths.
        pointer_path=owned_pointer if recorded_names_a_destination else None,
        # The whole ownership entry, not the path out of it: a rollback keeps the path and
        # withdraws the evidence that a link this command placed is at it, and only the
        # record itself can tell those two states apart.
        pointer_ownership=((record or {}).get("pointer")
                           if recorded_names_a_destination else None),
        # A destination this command could not even spell out is the survey's own unreadable
        # reading, so it is carried in the same list as every other one rather than reported
        # as no destination having been named.
        unreadable=unreadable_destination,
        protection=None if residue_root is None else ownership_of)
    try:
        classes = {
            c["component"]: classify_component(
                c, record=record, record_state=host_record.state, app_server=app_server,
                links=links,
                # Every component that takes an override gets its own. Wired for the relay alone,
                # the bridge was classified against whatever PATH resolved while --bridge-command
                # named a different executable, so the class reported and the entry point being
                # diagnosed were about two different files.
                entry_override=getattr(args, COMMAND_OVERRIDES[c["component"]], None),
                registration=registration if c["component"] == MCP_NAME else None,
                # The pointer is the destination's and says nothing about one component rather
                # than another, so both components are classified against the same reading.
                pointer=pointer_read,
            )
            for c in data["components"]
        }
    except reading.Refused as stop:
        return refused("diagnose", stop.reading, hostRecord=str(record_path))

    relay_executable = args.relay_command or shutil.which(relay_component["consoleScript"])
    survey = readings = None
    summary = {"skipped": "no relay executable was found, so no scope reading was made"}
    if relay_executable:
        readings = scope.survey(executable=relay_executable, socket=args.socket, state=args.state)
        status = scope.relay(["service", "status"], executable=relay_executable,
                             socket=args.socket, state=args.state)
        summary = scope.summarise(readings, issue=args.issue, service={
            "reading": status.get("payload"),
            # The invocation envelope is kept, not discarded, and then interpreted. Keeping
            # only the payload made 'the command could not run' and 'the daemon answered and
            # is stopped' the same answer, and one of those two is a claim about activation
            # that nobody observed.
            "state": scope.service_state(status),
            "invocation": {"ok": status.get("ok"), "exitCode": status.get("exitCode"),
                           "command": status.get("command"),
                           "unreadable": status.get("unreadable")},
            "note": "who owns the service for this scope. A service is never started here, and"
                    " no parent may stop one another parent is using (OPS-4.1).",
        })

    # OPS-3.4's lookup constructs a store, and a diagnosis constructs none, so it is opt-in
    # and never part of the default path.
    assignment = {"ran": False, "reason": (
        "assignment-find constructs a writable store and this command constructs none."
        " Pass --assignment-lookup to run it, or use --trial, where it runs before anything"
        " is written."
    )}
    if args.assignment_lookup and relay_executable and args.issue:
        found = scope.relay(["assignment-find", "--issue", str(args.issue)],
                            executable=relay_executable, socket=args.socket, state=args.state)
        assignment = {"ran": True, "ok": found.get("ok"), "command": found.get("command"),
                      "payload": found.get("payload"),
                      "note": "OPS-3.4 also wants this reading from each participating"
                              " process; one command cannot produce that."}
    elif args.assignment_lookup:
        assignment = {"ran": False, "reason": "no --issue, or no relay executable was found"}
    if summary:
        summary["assignmentFind"] = assignment

    both_own = all(ownership.reusable(c["class"]) for c in classes.values())
    imported_ok = all(c["importedLocation"] for c in classes.values())

    connect = (summary or {}).get("socketConnect")
    fields = {
        "installed": check.field(
            "verified" if both_own else "not_verified",
            "component classes: " + json.dumps({k: v["class"] for k, v in classes.items()})
            + ". Only 'own' is reusable (OPS-2.2).",
            command="runtime_install.py diagnose", acting_process=acting_process(), measured_at=now(),
        ),
        "imported": check.field(
            "verified" if imported_ok else "not_verified",
            "resolved locations: " + json.dumps({k: v["importedLocation"] for k, v in classes.items()})
            + ". Read from the interpreter, so an import satisfied by another copy is visible.",
            command=json.dumps({k: v.get("importCommand") for k, v in classes.items()}),
            acting_process=acting_process(), measured_at=now(),
        ),
        "mcpExposed": _mcp_exposed(registration, args.observed_tool),
        "connected": check.field(
            "verified" if connect == "ok" else ("not_verified" if connect else "unknown"),
            "doctor actorReachability.socketConnect = " + repr(connect)
            + ". A socket file existing on disk does not establish this.",
            # The command the summary's own scope came from. Deciding the provenance a second
            # time here let this field name one reading while scopeAnsweredBy named another.
            command=json.dumps((summary or {}).get("scopeCommand")),
            acting_process=acting_process(), measured_at=now() if connect else None,
        ),
        "deliveryAccepted": (
            check.not_applicable(
                "no trial was requested. This field requires an attempt that recorded a returned"
                " turn id, which means creating work, so it is only measured under --trial."
            ) if not args.trial else _trial(
                args, relay_executable,
                # The relay's own interpreter, resolved the same way its classification resolved
                # it, so the settings preflight asks the relay's code rather than this one's.
                classes[RELAY].get("interpreterPath"))
        ),
        "verificationComplete": check.not_applicable(
            "OPS-6.4 is a property of a verdict at a head, not of an installation. This command"
            " observes no verdict and never infers one from a completed turn or a green check."
        ),
        "alwaysActive": check.field(
            "not_verified",
            "no supervised runtime was enabled and no host restart was observed. Installation is"
            " not activation; this command enables no daemon.",
            acting_process=acting_process(),
        ),
        "settingsPreserved": check.field(
            "verified" if not registration["wouldWrite"] else "not_applicable",
            "diagnose writes nothing, so every table in config.toml and every hook entry is"
            " unchanged by it. Registration outcome would be " + registration["outcome"] + ".",
            acting_process=acting_process(), measured_at=now(),
        ),
    }

    emit({
        "command": "diagnose",
        "definitionVersion": data["definitionVersion"],
        "repositoryCommit": definition.git(["rev-parse", "HEAD"], ROOT),
        "codexHome": str(codex_home),
        "hostRecord": str(record_path),
        "hostRecordState": host_record.state,
        "hostRecordStateMeaning": (
            "ABSENT is a clean host, PRESENT was read, UNREADABLE exists and its shape could"
            " not be read, ACCESS_ERROR could not be reached at all. They are four answers and"
            " none of them is inferred from another."
        ),
        "hostRecordReading": None if host_record.ok else host_record.refusal(),
        "skillLinks": links,
        "components": classes,
        "mcpRegistration": registration,
        # The same key the install failure result uses, and deliberately not the same set: that
        # one is what THAT RUN left, read from the run itself, and this is what is on the
        # destination now. Neither is a superset of the other, and an empty answer here never
        # means a failed run left nothing behind.
        "residualPaths": residual["residualPaths"],
        "residue": residual,
        "scope": summary,
        "assignment": assignment,
        "scopeReadings": readings,
        "checks": check.record(
            fields, destination=codex_home,
            destination_kind="temporary" if args.temporary else "host",
            scope=(summary or {}).get("stateDirectory"),
        ),
    })
    return EXIT_OK


def _mcp_exposed(registration, observed):
    """Registration alone is never enough, and a tool list alone is not either.

    The bridge's own smoke check launches its own server, so its tool list says nothing about
    whether the REGISTERED command works. Both halves are required.
    """
    if registration["outcome"] == codexconfig.CONFLICT:
        return check.field("not_verified", "the configuration registers a different command: "
                          + registration["detail"], acting_process=acting_process(), measured_at=now())
    exists = registration["outcome"] in REGISTRATION_EXISTS
    compared = registration["outcome"] in REGISTRATION_COMPARED
    identity_tool = _bridge_identity_tool()
    if observed and identity_tool not in observed:
        return check.field(
            "not_verified",
            "the observed tools " + ", ".join(observed) + " do not include " + identity_tool
            + ", which this bridge defines, so they do not establish that THIS server is the"
            " one exposed.",
            acting_process=acting_process(), measured_at=now(),
        )
    if not observed:
        return check.field(
            "not_verified",
            "registration outcome " + registration["outcome"] + ". No tool names were observed:"
            " only a live session can list them, and a configuration entry alone never establishes"
            " this field. Supply --observed-tool from a session that lists them.",
            acting_process=acting_process(), measured_at=now(),
        )
    if not exists:
        return check.field(
            "not_verified",
            "tools were observed (" + ", ".join(observed) + ") but the configuration does not"
            " register this exact command, so the observation does not cover the registration"
            " being diagnosed.",
            acting_process=acting_process(), measured_at=now(),
        )
    if not compared:
        # A registration exists and nothing compared it with the command being diagnosed,
        # because none was supplied. Reporting verified here would put a comparison this run
        # did not make into the evidence of a field that exists to be falsifiable.
        return check.field(
            "not_verified",
            "a registration for " + MCP_NAME + " exists and these tools were listed ("
            + ", ".join(observed) + "), but no expected command was supplied, so nothing"
            " compared what is registered with what is being diagnosed. The configuration"
            " registers " + repr((registration.get("registered") or {}).get("command"))
            + ". Pass --bridge-command to make that comparison.",
            acting_process=acting_process(), measured_at=now(),
        )
    return check.field(
        "verified",
        "the configuration registers this exact command and these tools were listed in a live"
        " session: " + ", ".join(observed),
        acting_process=acting_process(), measured_at=now(),
    )


def observe_app_server(python, socket_path=None):
    """The App Server this host is talking to, observed now.

    The bridge's own read-only check starts the MCP server, lists its tools and calls
    get_capabilities, and its connection block is the identity a point records. Diagnosis makes
    its own observation rather than reading the one out of the point it is about to compare
    against, because a comparison against a value copied from its own subject is vacuous.
    """
    bridge = component_of(definition.load(), BRIDGE)
    argv = [str(python), str(ROOT / bridge["exerciseScript"])]
    if socket_path:
        argv += ["--socket", str(socket_path)]
    try:
        done = subprocess.run(argv, capture_output=True, text=True, timeout=180)
        payload = json.loads(done.stdout) if done.stdout.strip() else {}
    except (OSError, subprocess.SubprocessError, ValueError):
        return None
    if done.returncode != 0 or not payload.get("connection"):
        return None
    return json.dumps(payload["connection"])


def _bridge_identity_tool():
    return component_of(definition.load(), BRIDGE)["identityTool"]


def this_host():
    """This host's name, as a reading rather than a call that can raise into the top.

    It is one of the dimensions a point is compared on, so a name that could not be read is an
    unread signal like the others, not an exception escaping classification.
    """
    try:
        return socket.gethostname() or None
    except OSError:
        return None


def codex_cli_version():
    try:
        done = subprocess.run(["codex", "--version"], capture_output=True, text=True, timeout=30)
    except (OSError, subprocess.SubprocessError):
        return None
    return done.stdout.strip() or None if done.returncode == 0 else None


TRIAL_RELATIONSHIP = "<relationship>"
TRIAL_GENERATION = "<generation>"
TRIAL_EVENT = "<event>"

# The values Registry.register compares when it decides replay against conflict, and where each
# one is established for this trial. Three are the identity it hashes into a relationship id and
# the read-only lookup exposes them. The other four -- the authorized scope and the two host ids
# -- are returned by no read-only relay command, so the trial cannot pre-compare them. It orders
# register FIRST instead, because register is where those four are decided and it decides them
# without writing anything else.
#
# Drawing this set as {responsibleChild} was the defect: an assignment carrying this issue and
# this child under a different parent is a different relationship id, and comparing the child
# alone read it as the same one.
REPLAY_FROM_LOOKUP = ("parentTaskId", "childTaskId", "issueKey")
REPLAY_FROM_REGISTER = ("artifactRoots", "allowedRecipients", "parentHostId", "childHostId")
REGISTER_REPLAY_FIELDS = REPLAY_FROM_LOOKUP + REPLAY_FROM_REGISTER

# The two callables settings-record actually uses: the relay's reader for a JSON object or an
# @path, and the predicate record_settings applies before it writes anything. Named here and
# derived again from the relay's source by a check, so a relay that changes either one fails
# that check rather than leaving this preflight enforcing a rule nobody applies any more.
SETTINGS_READER = ("codex_session_relay.cli", "_settings_json")
SETTINGS_PREDICATE = ("codex_session_relay.settings", "TaskSettings", "require_usable")

# The relay callable that decides whether an anchor turn id is one at all. Paired with the
# inputs it governs below and asked of the relay's own code, never restated here.
RELAY_TURN_ID = ("codex_session_relay.registry", "validated_turn_id")

# The relay's own containment test for an artifact root. Named as the second half of the pair
# AuthorizedFile asks -- normalise the declared path, then ask which root holds it -- because
# asking only the first half answers only half the question, and the half left over is the one
# this command had been answering itself.
#
# Relational, unlike every predicate above: a root is not acceptable or unacceptable on its
# own, it is acceptable FOR the artifacts declared with it. A member declaring this one names
# the member that supplies the other operand.
RELAY_WITHIN = ("codex_session_relay.scope", "assert_within")

# The relay's own test for whether a task may receive a completion. Relational for the same
# reason: a recipient is authorised FOR a relationship's allowed set, never on its own.
RELAY_RECIPIENT = ("codex_session_relay.scope", "check_recipient")

# A member whose consumer applies a rule that is reachable through no read-only callable. The
# receipt's thread check lives inside a method that needs a store, so the rule is restated in
# the gate -- and the pair records WHERE it lives in the consumer's source, so a check fails
# when the consumer stops holding it there. Every layer of this class has been made of
# restatements nobody checked; a restatement that is declared and checked is not one of them.
RESTATED_HERE = "restated-here"
RELAY_TURN_THREAD = (RESTATED_HERE, "codex_session_relay.receipts", "ReceiptIntake",
                     "_check_turn_identity")

# This command's own minimum for a flag that was supplied: a value with something in it.
NON_BLANK = "non-blank"

# Every input the trial requires before its first mutating step, as argument name ->
# (the flag that supplies it, the predicate its CONSUMER applies).
#
# The pair is the point. Carrying only names, the set left each member to whatever predicate
# the gate happened to write, and the gate wrote truthiness: a whitespace-only turn id is
# truthy here and blank in the relay's validated_turn_id, so it passed the preflight and was
# refused after register had written a relationship row. NON_BLANK closes that for the whole
# family rather than for the two members a reviewer named, and a member whose consumer applies
# a stricter rule names that rule so it can be asked of the consumer's own code.
#
# Carrying A predicate is not carrying the RIGHT one. NON_BLANK is this command's own minimum
# and nothing more, so a member left on it has every further question about its value answered
# here -- which is how the artifact root came to be judged by a second copy of the relay's
# containment rule. That copy disagreed with the relay in both directions: it refused a root
# the relay accepts end to end, and for a root the relay refuses it blamed the deliverable
# rather than the root. A member carries the STRONGEST predicate its consumer applies, and
# where that predicate is relational it names the member supplying the other operand.
#
# Split in two because the two halves are checked differently, and saying so here is what keeps
# the check itself free of a literal naming one member of the set it is iterating.
TRIAL_REQUIRED_INPUTS = {
    "issue": ("--issue", NON_BLANK),
    "parent_task": ("--parent-task", NON_BLANK),
    "child_task": ("--child-task", NON_BLANK),
    "recipient": ("--recipient", RELAY_RECIPIENT, "parent_task"),
    "artifact_root": ("--artifact-root", RELAY_WITHIN, "artifact"),
    "turn_thread": ("--turn-thread", RELAY_TURN_THREAD, "child_task"),
    "artifact": ("--artifact", NON_BLANK),
    "turn_id": ("--turn-id", RELAY_TURN_ID),
    "dispatch_turn_id": ("--dispatch-turn-id", RELAY_TURN_ID),
}
# Required too, but with an acknowledgement path and a usability question of its own.
TRIAL_ACKNOWLEDGED_INPUTS = {"recipient_settings": ("--recipient-settings", SETTINGS_PREDICATE)}
TRIAL_PREFLIGHT_INPUTS = dict(TRIAL_REQUIRED_INPUTS, **TRIAL_ACKNOWLEDGED_INPUTS)

# The read-only probes this command runs before the trial mutates anything. Each asks a
# question whose answer the SELECTED relay will act on, so each runs that relay's interpreter.
# A probe running sys.executable asks this checkout instead, and a checkout whose rule differs
# from the installed relay's accepts what the relay refuses -- after four mutating steps.
PREFLIGHT_PROBES = ("settings_usable", "values_usable", "_relay_normalizes",
                    "_relay_contains", "_relay_admits", "store_presence", "store_tables",
                    "candidate_tables")

# Every presence question this command asks, paired with the reader whose own sentinel answers
# it. Deciding presence here instead means deciding it by whatever predicate this module wrote,
# and the one it wrote read an empty server table as an absent one.
PRESENCE_READINGS = {"the MCP registration": ("codexconfig", "registration_of")}


def _supplied(value):
    """Whether a flag arrived carrying something.

    A value made only of whitespace is not supplied. It is truthy, which is how one reached the
    relay and was refused there, after the rows a refusal was supposed to prevent.
    """
    if value is None:
        return False
    if isinstance(value, (list, tuple)):
        return bool(value) and all(_supplied(item) for item in value)
    return bool(str(value).strip())


def _relational(pair):
    """Whether a member's predicate is asked about it TOGETHER with another member's value.

    Containment is the case that needs it. An artifact root cannot be judged on its own: it is
    judged for the artifacts declared with it, and a pair able to carry only a single-value
    predicate would have to leave that question here. Leaving it here is what produced a second
    copy of a rule the relay owns.

    The third slot names the member supplying the other operand, so the gate reads it from the
    declaration instead of knowing which members go together.
    """
    return len(pair) > 2


def _settings_program():
    """The read-only program that asks the relay's own code whether a settings value is usable.

    Built from SETTINGS_READER and SETTINGS_PREDICATE rather than written out, so the names this
    runs are the names those declarations carry.
    """
    reader_module, reader = SETTINGS_READER
    predicate_module, predicate, method = SETTINGS_PREDICATE
    return "\n".join([
        "import json, sys",
        "from " + reader_module + " import " + reader,
        "from " + predicate_module + " import " + predicate,
        "try:",
        "    " + predicate + "(" + reader + "(sys.argv[1]))." + method + "()",
        "except BaseException as error:",
        "    print(json.dumps({'usable': False,",
        "                      'detail': type(error).__name__ + ': ' + str(error)}))",
        "else:",
        "    print(json.dumps({'usable': True, 'detail': 'read, and complete'}))",
        "",
    ])


def settings_usable(raw, interpreter):
    """Whether settings-record would accept this value, asked of the relay's own code.

    Not a second validator. The reader that turns '@path' or a JSON string into an object and
    the predicate that decides completeness are the two settings-record itself uses, run
    read-only in the relay's interpreter. Neither opens a store and neither writes. A copy of
    those rules here would be a second statement of something that lives elsewhere, and the next
    change would move only one of them.

    Checked before the first mutating step because settings-record now runs after register: an
    unreadable value discovered there would leave a relationship row behind, which is exactly
    the property reordering register was meant to protect.
    """
    if not interpreter:
        return {"usable": None, "detail": (
            "the relay's interpreter could not be resolved, so the relay's own settings reader"
            " and predicate could not be asked here. Pass --relay-command naming an installed"
            " entry point, or record the install first.")}
    try:
        # -B because a check that claims to be read-only must not leave __pycache__ behind in
        # somebody's installed runtime. Importing two modules is enough to write it.
        done = subprocess.run([str(interpreter), "-B", "-c", _settings_program(), str(raw)],
                             capture_output=True, text=True, timeout=60)
    except (OSError, subprocess.SubprocessError) as error:
        return {"usable": None,
                "detail": "the check could not be run: " + type(error).__name__ + ": " + str(error)}
    if done.returncode != 0 or not done.stdout.strip():
        return {"usable": None, "detail": (
            "the relay's settings check did not answer (exit " + str(done.returncode) + "): "
            + ((done.stderr or done.stdout).strip()[-300:] or "no output"))}
    try:
        return json.loads(done.stdout)
    except ValueError as error:
        return {"usable": None,
                "detail": "the check answered something unreadable: " + str(error)}


def _predicate_program(predicate):
    """A read-only program that asks one declared callable about each value it is given."""
    module, callable_name = predicate
    return "\n".join([
        "import json, sys",
        "from " + module + " import " + callable_name,
        "refusals = {}",
        "for flag, value in json.loads(sys.argv[1]).items():",
        "    try:",
        "        " + callable_name + "(value)",
        "    except BaseException as error:",
        "        refusals[flag] = type(error).__name__ + ': ' + str(error)",
        "print(json.dumps(refusals))",
        "",
    ])


def values_usable(predicate, values, interpreter):
    """Whether the consumer's own predicate accepts these values, asked in its own runtime.

    The other half of the pair a declared input carries. An input's predicate belongs to
    whatever will act on the value, so the value goes there rather than to a rule restated
    here: validated_turn_id refuses a blank anchor and a truthiness test written here does not,
    and the difference is a relationship row written before the refusal arrives.

    Returns {"usable": bool or None, "detail": str}. None means the question could not be
    asked, which is a refusal of its own and never a fall back to a predicate of this
    command's own making.
    """
    if not interpreter:
        return {"usable": None, "detail": (
            "the relay's interpreter could not be resolved, so its own rule for "
            + predicate[-1] + " could not be asked here. Pass --relay-command naming an"
            " installed entry point, or record the install first.")}
    try:
        # -B for the same reason the settings probe uses it: this runs inside somebody's
        # installed runtime and read-only has to mean it.
        done = subprocess.run([str(interpreter), "-B", "-c", _predicate_program(predicate),
                               json.dumps(dict(values))],
                              capture_output=True, text=True, timeout=60)
    except (OSError, subprocess.SubprocessError) as error:
        return {"usable": None,
                "detail": "the check could not be run: " + type(error).__name__ + ": " + str(error)}
    if done.returncode != 0 or not done.stdout.strip():
        return {"usable": None, "detail": (
            "the relay's " + predicate[-1] + " check did not answer (exit "
            + str(done.returncode) + "): "
            + ((done.stderr or done.stdout).strip()[-300:] or "no output"))}
    try:
        refusals = json.loads(done.stdout)
    except ValueError as error:
        return {"usable": None,
                "detail": "the check answered something unreadable: " + str(error)}
    if refusals:
        return {"usable": False,
                "detail": "; ".join(flag + " " + why for flag, why in sorted(refusals.items()))}
    return {"usable": True, "detail": "read, and accepted by " + predicate[-1]}


def trial_request_id(issue, dispatch_turn):
    """Unique to one dispatch, and stable across retries of that same dispatch.

    Keyed on the issue alone, the second trial for an issue replays the first generation-open
    and gets back the generation already bound to the first dispatch turn; generation-bind
    then refuses the new anchor and the trial cannot reach a delivery. A trial that only works
    once is not the delivery test criterion 5 asks for. Keyed on the dispatch turn too, a
    retry of one dispatch still replays, and a genuinely new dispatch opens its own.
    """
    seed = str(issue) + "/" + str(dispatch_turn)
    return "jun104-trial-" + hashlib.sha256(seed.encode()).hexdigest()[:16]


def trial_steps(*, issue, parent_task, child_task, recipient, artifact_root,
                turn_thread, turn_id, host, artifacts=None, dispatch_turn_id=None,
                turn_status="completed", recipient_settings=None):
    """The exact relay invocations the trial makes, returned as data.

    Kept as data rather than built inline so the argv this command sends can be checked
    against the relay's own required arguments without performing a delivery. Identifiers
    that only exist once an earlier step has run appear as placeholders and are substituted
    at execution time.
    """
    request_id = trial_request_id(issue, dispatch_turn_id)
    steps = [
        # First, before anything is written. A lookup run after register could find the
        # relationship this trial just created, which says nothing about the store
        # (OPS-3.4). Run first, it describes the store as it was found.
        ["assignment-find", "--issue", str(issue)],
        # register is the FIRST mutating step, and that ordering is the guarantee. It is the
        # producer of the replay predicate: it compares seven values and either replays the
        # existing relationship or refuses the whole registration, without writing anything
        # else. Four of those seven are not exposed by any read-only relay command, so a
        # settings write placed before this one would land for a trial that register then
        # refuses. See REGISTER_REPLAY_FIELDS.
        ["register", "--parent-task", str(parent_task), "--parent-host", str(host),
         "--child-task", str(child_task), "--child-host", str(host),
         "--issue", str(issue), "--artifact-root", str(artifact_root),
         "--allowed-recipient", str(recipient), "--dispatch-request-id", request_id],
    ]
    if recipient_settings:
        # A send is withheld until the recipient's authorized settings are on record, because
        # preserving them is what the delivery has to check against. Recorded after the
        # relationship exists, so a refused registration leaves no settings behind.
        steps.append(["settings-record", "--task", str(recipient),
                      "--settings", str(recipient_settings)])
    return steps + [
        # The generation stays unbound until an exact dispatch turn id is supplied, and the
        # relay refuses to emit against an unbound generation.
        ["generation-open", "--relationship", TRIAL_RELATIONSHIP,
         "--dispatch-request-id", request_id]
        + (["--dispatch-turn-id", dispatch_turn_id] if dispatch_turn_id else []),
        # A reviewable receipt must carry a deliverable: the relay refuses one whose manifest
        # is empty, because that is the no-deliverable sentinel.
        # register opens the generation but leaves it unbound, and the relay refuses to emit
        # against a generation with no anchor. Binding is its own step, not a flag on the open.
        ["generation-bind", "--relationship", TRIAL_RELATIONSHIP,
         "--generation", TRIAL_GENERATION, "--dispatch-turn-id", dispatch_turn_id],
        # Only the anchor turn is admitted by default. A turn the child actually ran is a
        # continuation and needs an explicit admission naming the generation and an actor.
        ["admit-turn", "--relationship", TRIAL_RELATIONSHIP, "--generation", TRIAL_GENERATION,
         "--turn", str(turn_id), "--actor", str(child_task)],
        ["emit", "--relationship", TRIAL_RELATIONSHIP, "--generation", TRIAL_GENERATION,
         "--outcome", "ready_for_review", "--turn-thread", str(turn_thread),
         "--turn-id", str(turn_id), "--turn-status", str(turn_status)]
        + [token for artifact in (artifacts or []) for token in ("--artifact", str(artifact))],
        ["deliver", "--event", TRIAL_EVENT],
    ]


def _relay_normalizes(paths, interpreter):
    """Ask the relay's own normalizer, in the runtime that will act on the answer, what it refuses.

    Reimplementing this drifted: a path written with a parent segment compares equal to its own
    string, so a "is it already normalised" check written here passed something the relay
    rejects at emit, after four mutating steps. The rule belongs to the relay, so the question
    goes to the relay rather than to a second copy of it.

    And to the relay that will run, not to this checkout's copy of it. Asked with this
    interpreter and this source, a selected installation whose rule differs accepts here and
    refuses at emit, which is the same failure one layer up: the question was paired with a
    predicate but not with the runtime that applies it. The settings probe already does this.

    Returns (refusals, reason). A reason means the question could not be asked, which is a
    refusal of its own - never a fallback to an approximation.
    """
    if not interpreter:
        return {}, ("the relay's interpreter could not be resolved, so its own path rule could"
                    " not be asked here. Pass --relay-command naming an installed entry point,"
                    " or record the install first")
    probe = (
        "import json, sys\n"
        "from codex_session_relay.scope import normalize_declared_path\n"
        "out = {}\n"
        "for path in json.loads(sys.argv[1]):\n"
        "    try:\n"
        "        normalize_declared_path(path)\n"
        "    except Exception as error:\n"
        "        out[path] = type(error).__name__ + ': ' + str(error)\n"
        "print(json.dumps(out))\n"
    )
    argv = [str(interpreter), "-B", "-c", probe, json.dumps(list(paths))]
    try:
        done = subprocess.run(argv, capture_output=True, text=True, timeout=60)
    except (OSError, subprocess.SubprocessError) as error:
        return {}, "the relay's normalizer could not be run: " + type(error).__name__
    if done.returncode != 0:
        return {}, ("the relay's normalizer could not be asked: "
                    + (done.stderr or "").strip()[-200:])
    try:
        return json.loads(done.stdout), None
    except ValueError:
        return {}, "the relay's normalizer returned nothing readable"


def _relay_contains(paths, root, interpreter):
    """Whether the relay would hold each artifact inside this root, asked of the relay.

    The pair AuthorizedFile asks, in the order it asks them: the declared path is normalised,
    then the roots are asked which of them holds it. Deciding it here is what the member's
    predicate had been left free to allow, and the copy written here disagreed with the relay in
    both directions -- it refused a root the relay accepts end to end, and where the relay
    refuses a root it named the deliverable as the thing at fault.

    The root itself is never normalised, because the relay does not normalise one. is_within
    compares normalised forms without requiring the recorded root to already be in one, so
    asking more of the root here than the relay asks would refuse a registration it performs.

    Returns (refusals, reason). A reason means the question could not be asked, which is a
    refusal of its own and never a fall back to a rule of this command's making.
    """
    if not interpreter:
        return {}, ("the relay's interpreter could not be resolved, so its own containment rule"
                    " could not be asked here. Pass --relay-command naming an installed entry"
                    " point, or record the install first")
    module, containment = RELAY_WITHIN
    probe = "\n".join([
        "import json, sys",
        "from " + module + " import normalize_declared_path, " + containment,
        "root, out = sys.argv[1], {}",
        "for path in json.loads(sys.argv[2]):",
        "    try:",
        "        " + containment + "(normalize_declared_path(path), [root])",
        "    except Exception as error:",
        "        out[path] = type(error).__name__ + ': ' + str(error)",
        "print(json.dumps(out))",
        "",
    ])
    argv = [str(interpreter), "-B", "-c", probe, str(root), json.dumps(list(paths))]
    try:
        done = subprocess.run(argv, capture_output=True, text=True, timeout=60)
    except (OSError, subprocess.SubprocessError) as error:
        return {}, "the relay's containment rule could not be run: " + type(error).__name__
    if done.returncode != 0:
        return {}, ("the relay's containment rule could not be asked: "
                    + (done.stderr or "").strip()[-200:])
    try:
        return json.loads(done.stdout), None
    except ValueError:
        return {}, "the relay's containment rule returned nothing readable"


def _relay_admits(predicate, subject, allowed, interpreter):
    """Whether the relay's own rule admits this subject against this allowed value.

    The shape every relational rule here has: a value, and the set it must belong to. Asked of
    the relay, in the runtime that will act on the answer, for the same reason the single-value
    predicates are. A rule restated here is a rule that drifts, and the drift is discovered
    after the rows the refusal was meant to prevent.

    Returns (refusal, reason). Both None means admitted; a reason means the question could not
    be asked, which is a refusal of its own.
    """
    if not interpreter:
        return None, ("the relay's interpreter could not be resolved, so its own rule for "
                      + predicate[-1] + " could not be asked here. Pass --relay-command naming"
                      " an installed entry point, or record the install first")
    module, callable_name = predicate
    probe = "\n".join([
        "import json, sys",
        "from " + module + " import " + callable_name,
        "try:",
        "    " + callable_name + "(sys.argv[1], [sys.argv[2]])",
        "except BaseException as error:",
        "    print(json.dumps(type(error).__name__ + ': ' + str(error)))",
        "else:",
        "    print(json.dumps(None))",
        "",
    ])
    argv = [str(interpreter), "-B", "-c", probe, str(subject), str(allowed)]
    try:
        done = subprocess.run(argv, capture_output=True, text=True, timeout=60)
    except (OSError, subprocess.SubprocessError) as error:
        return None, "the relay's rule could not be run: " + type(error).__name__
    if done.returncode != 0:
        return None, ("the relay's rule could not be asked: "
                      + (done.stderr or "").strip()[-200:])
    try:
        return json.loads(done.stdout), None
    except ValueError:
        return None, "the relay's rule returned nothing readable"


def _unusable_artifacts(artifacts, root, interpreter):
    """Why the relay would refuse each artifact, checked before anything is written.

    Both path questions belong to the relay and both are asked of it: the shape of a declared
    path, and whether the declared root holds it. What remains here is what neither of those
    covers: the file has to exist, be a regular file with no symbolic link at any component,
    and be readable.
    """
    paths = [str(raw) for raw in (artifacts or [])]
    if not paths:
        return []
    refusals, reason = _relay_normalizes(paths, interpreter)
    if reason:
        return [reason + "; artifacts are not checked against a second copy of the rule"]
    outside, unasked = _relay_contains(paths, root, interpreter)
    if unasked:
        return [unasked + "; the artifact root is not checked against a second copy of the rule"]

    problems = []
    for raw in paths:
        if raw in refusals:
            problems.append(raw + ": " + refusals[raw])
            continue
        path = Path(raw)
        try:
            if not path.exists():
                problems.append(raw + " does not exist")
                continue
            if not path.is_file() or path.is_symlink():
                problems.append(raw + " is not a regular file")
                continue
            if any(part.is_symlink() for part in list(path.parents)):
                problems.append(raw + " has a symbolic link in its path")
                continue
            path.open("rb").close()
        except OSError as error:
            problems.append(raw + " could not be read: " + type(error).__name__)
            continue
        if raw in outside:
            problems.append("the artifact root " + str(root) + " does not hold " + raw + ": "
                            + outside[raw])
    return problems


def _trial(args, relay_executable, relay_interpreter=None):
    """Register, open the generation, emit, then a bounded deliver. Only this path creates work.

    The field's evidence is the delivery attempt's returned turn id. emit stores the receipt
    and enqueues it; the attempt itself happens in deliver, so an emitted receipt's own turn
    id never satisfies deliveryAccepted (OPS-6.1).

    The invocations come from trial_steps so that what this sends is the same data a test can
    check against the relay's own required arguments.
    """
    if not relay_executable:
        return check.field("not_verified", "trial requested but no relay executable was found",
                           acting_process=acting_process())
    # Driven from the declared pairs, so each input is checked by the predicate its consumer
    # applies rather than by whatever this gate would otherwise invent. A reviewable receipt
    # with an empty manifest is refused and a generation with no anchor cannot be emitted
    # against; both were discovered at the relay, after the trial had written rows.
    blank = sorted(pair[0] for name, pair in TRIAL_REQUIRED_INPUTS.items()
                   if not _supplied(getattr(args, name, None)))
    if blank:
        return check.field(
            "not_verified",
            "trial requested but these inputs were not supplied with a value: "
            + ", ".join(blank) + ". Everything the trial needs is checked here, before the"
            " first command, so an incomplete trial writes nothing. A flag carrying only"
            " whitespace is not supplied: it is refused by the relay after rows exist.",
            acting_process=acting_process(), measured_at=now(),
        )
    # The members whose consumer applies a stricter rule than "supplied", asked of that
    # consumer's own code in the runtime that will act on the answer. A relational member is
    # not asked here: its predicate takes the value it is judged WITH, so it is asked where
    # that other value is, and asking it with one operand would be a third rule again.
    for predicate in sorted({pair[1] for pair in TRIAL_REQUIRED_INPUTS.values()
                             if pair[1] != NON_BLANK and not _relational(pair)}):
        governed = {pair[0]: str(getattr(args, name))
                    for name, pair in TRIAL_REQUIRED_INPUTS.items()
                    if pair[1] == predicate and not _relational(pair)}
        answer = values_usable(predicate, governed, relay_interpreter)
        if not answer.get("usable"):
            return check.field(
                "not_verified",
                "these inputs are not what " + predicate[-1] + " accepts: "
                + str(answer.get("detail")) + ". This is the relay's own predicate for them,"
                " asked before the first mutating step. Nothing was written.",
                acting_process=acting_process(), measured_at=now(),
            )
    # The relational members whose consumer exposes an askable rule. Which member is judged
    # WITH which comes off the declaration, so this loop does not know that a recipient goes
    # with a parent task. A member whose operand is a list is asked by the probe that reads
    # that list, because the question needs its members and they are read there.
    for name, pair in sorted(TRIAL_REQUIRED_INPUTS.items()):
        if not _relational(pair) or pair[1][0] == RESTATED_HERE:
            continue
        operand = getattr(args, pair[2])
        if isinstance(operand, (list, tuple)):
            continue
        refusal, unasked = _relay_admits(pair[1], str(operand), str(getattr(args, name)),
                                         relay_interpreter)
        if unasked or refusal:
            return check.field(
                "not_verified",
                str(pair[0]) + " and " + str(TRIAL_REQUIRED_INPUTS[pair[2]][0]) + " are not a"
                " combination " + pair[1][-1] + " accepts: " + str(unasked or refusal)
                + ". This is the relay's own rule for them, asked before the first mutating"
                " step. Nothing was written.",
                acting_process=acting_process(), measured_at=now(),
            )
    # The one rule the consumer holds in a method that needs a store, so there is nowhere to
    # ask it. Restated here, and declared as restated by RELAY_TURN_THREAD, which names the
    # place in the consumer's source a check reads back.
    if str(args.turn_thread) != str(args.child_task):
        return check.field(
            "not_verified",
            "the turn thread " + str(args.turn_thread) + " is not the child task "
            + str(args.child_task) + ". The relay requires a receipt's thread to be the"
            " relationship's child task, so this combination can only be refused at emit,"
            " after the store has been written to.",
            acting_process=acting_process(), measured_at=now(),
        )
    unusable = _unusable_artifacts(args.artifact, args.artifact_root, relay_interpreter)
    if unusable:
        return check.field(
            "not_verified",
            "the declared root and its artifacts do not satisfy what the relay requires of a"
            " manifest entry: "
            + "; ".join(unusable) + ". They are checked here because the relay checks them"
            " while building the manifest, which happens after four mutating steps.",
            acting_process=acting_process(), measured_at=now(),
        )
    if not args.recipient_settings and not args.settings_already_recorded:
        return check.field(
            "not_verified",
            "a send is withheld until the recipient's authorized settings are on record."
            " Supply --recipient-settings, or --settings-already-recorded to proceed on the"
            " caller's own claim that they are already recorded for this recipient. There is"
            " no read-only way to check from here: every relay read constructs a store.",
            acting_process=acting_process(), measured_at=now(),
        )
    if args.recipient_settings:
        # Present is not usable. settings-record reads this value and applies its predicate, and
        # it now runs after register, so a malformed object or an unreadable @path discovered
        # there would leave a relationship row behind. Asked here with the relay's own reader
        # and its own predicate, read-only.
        settings = settings_usable(args.recipient_settings, relay_interpreter)
        if not settings.get("usable"):
            return check.field(
                "not_verified",
                "the recipient settings supplied as " + str(args.recipient_settings)
                + " are not usable: " + str(settings.get("detail"))
                + ". This is the relay's own reader and its own predicate, asked before the"
                " first mutating step. No settings and no relationship row were written.",
                acting_process=acting_process(), measured_at=now(),
            )

    steps = trial_steps(
        issue=args.issue, parent_task=args.parent_task, child_task=args.child_task,
        recipient=args.recipient, artifact_root=args.artifact_root,
        turn_thread=args.turn_thread, turn_id=args.turn_id, host=socket.gethostname(),
        artifacts=args.artifact, dispatch_turn_id=args.dispatch_turn_id,
        turn_status=args.turn_status, recipient_settings=args.recipient_settings,
    )
    performed = []
    resolved = {}

    def run_step(argv):
        concrete = [resolved.get(token, token) for token in argv]
        answer = scope.relay(concrete, executable=relay_executable, socket=args.socket,
                             state=args.state)
        performed.append({"command": answer.get("command"), "ok": answer.get("ok"),
                          "exitCode": answer.get("exitCode")})
        return answer

    def refuse(step, answer):
        return check.field(
            "not_verified",
            step + " did not succeed, so nothing later could be established: "
            + str(answer.get("stderr") or answer.get("unreadable")
                  or json.dumps(answer.get("payload"))[:300])
            + ". Steps: " + json.dumps(performed),
            command=json.dumps(performed[-1]["command"]) if performed else None,
            acting_process=acting_process(), measured_at=now(),
        )

    by_name = {argv[0]: argv for argv in steps}

    # OPS-3.4: the lookup that distinguishes the expected store from a different populated
    # one. Run before anything is written, and compared against what the caller independently
    # expects. With no expectation supplied it stays an observation, not a proof.
    found = run_step(by_name["assignment-find"])
    if not found.get("ok"):
        # The lookup is the trial's store check and it runs before any settings or relationship
        # row exists, so a lookup that did not run stops the trial here. An answer that found
        # nothing is an observation; an invocation that failed is not an observation at all, and
        # registering afterwards would put rows in a store this process could not read
        # (OPS-3.4). The lookup itself constructs a store, so what is guaranteed here is that no
        # settings and no relationship row were written, not that nothing at all was.
        return refuse("assignment-find", found)
    assignment = {
        "ran": True,
        "ok": found.get("ok"),
        "payload": found.get("payload"),
        "expected": args.expect_relationship,
        "agrees": None,
        "replayFields": {
            "comparedHere": list(REPLAY_FROM_LOOKUP),
            "decidedByRegister": list(REPLAY_FROM_REGISTER),
            "why": "Registry.register compares seven values to decide replay against conflict."
                   " The lookup exposes three of them; no read-only relay command returns the"
                   " other four, so register runs first and decides them without writing"
                   " anything else.",
        },
        "meaning": (
            "OPS-3.4 also wants this reading from each participating process; one command"
            " cannot produce that, and this is the part it can."
        ),
    }
    # The lookup is the trial's pre-mutation store check, so its ANSWER is consulted, not only
    # whether it ran. Every field of the registration identity the lookup exposes is compared,
    # not the child alone: an assignment carrying this issue under a different parent hashes to
    # a different relationship id, and comparing the child alone read it as the same one.
    payload = found.get("payload")
    responsible = payload.get("responsibleRelationship") if isinstance(payload, dict) else None
    responsible_child = payload.get("responsibleChild") if isinstance(payload, dict) else None
    assignment["responsibleRelationship"] = responsible
    assignment["responsibleChild"] = responsible_child

    # The identity this trial would register, against the identity the owning assignment has.
    intended = {"parentTaskId": str(args.parent_task), "childTaskId": str(args.child_task),
                "issueKey": str(args.issue)}
    owner = None
    for entry in ((payload or {}).get("assignments") or []) if isinstance(payload, dict) else []:
        if isinstance(entry, dict) and entry.get("relationshipId") == responsible:
            owner = entry
            break
    if owner is not None:
        compared = {field: owner.get(field) for field in REPLAY_FROM_LOOKUP}
    elif responsible:
        # The lookup names an owner but returned no record for it, so the top-level answer
        # carries one field and that is the one that can be compared. Reported as a partial
        # comparison rather than presented as a complete one.
        compared = {"childTaskId": responsible_child}
    else:
        compared = {}
    differing = [field for field, value in compared.items() if str(value) != intended[field]]
    assignment["intendedIdentity"] = intended
    assignment["owningIdentity"] = compared or None
    assignment["identityFieldsCompared"] = sorted(compared)
    assignment["identityFieldsNotExposed"] = sorted(
        set(REPLAY_FROM_LOOKUP) - set(compared)) if responsible else []
    if responsible and differing:
        return check.field(
            "not_verified",
            "issue " + str(args.issue) + " already belongs to child "
            + str(compared.get("childTaskId")) + " under " + str(responsible)
            + ", and its " + ", ".join(sorted(differing)) + " differs from the identity this"
            " trial would register (owning " + json.dumps(compared)
            + ", intended " + json.dumps(intended) + "). The relay owns one issue to one child,"
            " so proceeding would drive a trial that cannot complete. No settings and no"
            " relationship row were written.",
            command=json.dumps(performed[-1]["command"]),
            acting_process=acting_process(), measured_at=now(),
        )

    if args.expect_relationship:
        # Compared against the field that names the responsible relationship, not against the
        # serialized answer. A substring test over the payload matches an archived assignment
        # sitting anywhere in it, so the guard meant to prove this process reads the expected
        # store would pass against a store where that relationship is closed.
        assignment["comparedField"] = "responsibleRelationship"
        assignment["agrees"] = responsible is not None and responsible == args.expect_relationship
        if not assignment["agrees"]:
            seen = json.dumps(payload)
            return check.field(
                "not_verified",
                "the store does not hold the expected relationship "
                + str(args.expect_relationship) + " for issue " + str(args.issue)
                + " as its responsible relationship (it reports " + repr(responsible) + ")"
                + ", so this process is pointed at a different store than the one the"
                " assignment lives in. No settings and no relationship row were written."
                " Lookup: " + seen[:300],
                command=json.dumps(performed[-1]["command"]),
                acting_process=acting_process(), measured_at=now(),
            )

    registered = run_step(by_name["register"])
    payload = registered.get("payload") or {}
    relationship = payload.get("relationshipId") or (payload.get("relationship") or {}).get("id")
    if not registered.get("ok") or not relationship:
        return refuse("register", registered)
    resolved[TRIAL_RELATIONSHIP] = str(relationship)

    # After register, never before. register is the producer of the replay predicate and the
    # only place the four unexposed fields are compared: a settings write placed ahead of it
    # lands for a trial that register then refuses on a scope or host the lookup cannot show.
    for argv in steps:
        if argv[0] == "settings-record":
            recorded = run_step(argv)
            if not recorded.get("ok"):
                return refuse("settings-record", recorded)

    # generation-open declares --dispatch-request-id required, and replaying the SAME id the
    # registration used returns the generation it already opened rather than opening another.
    opened = run_step(by_name["generation-open"])
    payload = opened.get("payload") or {}
    generation = payload.get("executionGeneration")
    if generation is None:
        generation = (payload.get("generation") or {}).get("executionGeneration")
    if not opened.get("ok") or generation is None:
        return refuse("generation-open", opened)
    resolved[TRIAL_GENERATION] = str(generation)

    bound = run_step(by_name["generation-bind"])
    if not bound.get("ok"):
        return refuse("generation-bind", bound)

    admitted = run_step(by_name["admit-turn"])
    if not admitted.get("ok"):
        return refuse("admit-turn", admitted)

    emitted = run_step(by_name["emit"])
    payload = emitted.get("payload") or {}
    receipt = payload.get("receipt") or {}
    event = receipt.get("eventId") or payload.get("eventId") or receipt.get("id")
    if not emitted.get("ok") or not event:
        return refuse("emit", emitted)
    resolved[TRIAL_EVENT] = str(event)

    delivered = run_step(by_name["deliver"])
    payload = delivered.get("payload") or {}
    attempt = payload.get("attempt") or {}
    turn = attempt.get("turnId") or (attempt.get("turn") or {}).get("id")
    if delivered.get("ok") and turn:
        return check.field(
            "verified",
            "assignment lookup before any write: " + json.dumps(assignment)[:300]
            + ". The delivery attempt returned turn id " + str(turn) + " for event " + str(event)
            + ", relationship " + str(relationship) + ", generation " + str(generation)
            + ". Recipient " + str(args.recipient) + ". Steps: " + json.dumps(performed),
            command=json.dumps(performed[-1]["command"]),
            acting_process=acting_process(), measured_at=now(),
        )
    return check.field(
        "not_verified",
        "the delivery attempt recorded no returned turn id. A dispatch, a staged receipt or an"
        " absent error does not establish this field. Attempt: " + json.dumps(attempt)[:400]
        + ". Steps: " + json.dumps(performed),
        command=json.dumps(performed[-1]["command"]),
        acting_process=acting_process(), measured_at=now(),
    )


# ------------------------------------------------------------------------- hook

def _hook_busy(adapter, path, error, *, settings, locked):
    """Another run holds the hook file. Nothing about this hook was established.

    The answer install, register-mcp and release_candidate already give for the same event.
    Left to escape, a competing run was reported as an internalError -- a claim that this
    command has a defect, which is about the code rather than about the host and sends whoever
    reads it somewhere that has nothing wrong with it.

    Whether the settings were written is carried rather than decided: they are written before
    the hook, so a lock taken between the two leaves them on disk and saying otherwise would be
    a second false claim on top of the first.

    The file this command was installing into and the file whose lock it could not take are two
    facts, and two of the writes here take DIFFERENT locks. Reporting the locked resource as
    hookFile named the settings file as the hook being installed, which is this change's own
    subject one more time: a field filled by a value other than the reading its own question
    produced. Review found it, which is the point of review.
    """
    emit({"command": "hook", "adapter": adapter, "hookFile": str(path),
          "lockedPath": str(locked), "outcome": BUSY,
          "settings": settings, "result": None, "applied": False, "wrote": False,
          "refused": "another run holds " + str(locked) + ": " + str(error),
          "note": ("nothing about this hook was established and no hook was appended. What"
                   " happened to the settings is reported above and is not changed by this"
                   " refusal.")})
    return EXIT_REFUSED


def cmd_hook(args):
    codex_home = Path(args.codex_home or os.environ.get("CODEX_HOME") or Path.home() / ".codex")
    path = codex_home / "hooks.json"
    settings = None
    # Read once, defaulted, so a caller that names a command outright needs to know nothing
    # about the adapters this repository happens to own.
    adapter = getattr(args, "adapter", None)
    if adapter == COMPLETION:
        owner = getattr(args, "owner", completion.OWNER_USER)
        # EVERY precondition is checked before ANY write, and that ordering is the whole point
        # of this block rather than an accident of how it grew. Five rounds of review each found
        # one more condition being evaluated after a write it should have preceded, and the last
        # of them was the expensive shape: the settings were written, the duplicate registration
        # was refused afterwards, and a hook already in the file immediately began running
        # against settings this command had just reported it would not install.
        #
        # So a new precondition belongs in this list, not in a new branch further down.
        event = args.event or completion.EVENT
        refused = (completion.registration_complaints(args.event)
                   + completion.budget_complaints(args.guard_timeout, args.timeout))
        refused += completion.override_complaints(owner)
        interpreter = wanted = None
        if not refused:
            try:
                # The candidate is only executed when this command is going to write. A plan
                # that writes nothing should not run a program the caller named, and what it
                # did not check it does not claim.
                interpreter = completion.interpreter_for(args.python or sys.executable,
                                                         run=args.apply)
                wanted = completion.configuration(
                    destination=args.dest, relay=args.relay_command,
                    marker_root=args.marker_root, database=args.db_path, mode=args.mode,
                    timeout=args.guard_timeout, journal_root=args.journal_root,
                    codex_home=codex_home, issue=args.issue,
                    isolation=getattr(args, "isolation_asserted_by", None),
                    owner=owner,
                    # The socket this installation expects, when the caller named one. It is what
                    # lets the guard refuse a state directory whose store records another App
                    # Server rather than judging a Stop against it; with none there is nothing to
                    # compare and the document stays byte-identical to one written before this key.
                    #
                    # Read with getattr for the same reason isolation above it is: this command is
                    # also called with a Namespace built by a caller that never had the global
                    # option, and a missing attribute there is an absent socket, not a crash.
                    socket=getattr(args, "socket", None),
                    adapter_interpreter=interpreter,
                    adapter_entry_point=ROOT / "scripts" / completion.ENTRY_POINT_NAME,
                )
                # The document this run would write, judged HERE rather than inside
                # write_configuration. That is the same check, and it used to be the first
                # thing write_configuration did -- but write_configuration runs after the
                # fallback launcher is placed, so a budget the plugin owner may not record
                # (anything above completion.MAX_PLUGIN_GUARD_SECONDS) left a launcher on
                # disk and then refused the settings. A launcher alone is harmless, but
                # installing one for a document this command just called unusable is not
                # something to do quietly, and the preconditions are where "nothing was
                # written" is promised.
                refused += completion.complaints(wanted)
            except ValueError as error:
                refused = [str(error)]
        if refused:
            emit({"command": "hook", "adapter": adapter, "error": "; ".join(refused),
                  "hookFile": str(path), "settings": None, "result": None,
                  "note": "nothing was written: every precondition is checked first."})
            return EXIT_USAGE
        configuration = completion.configuration_path(codex_home)
        command = completion.command_for(interpreter,
                                         ROOT / "scripts" / completion.ENTRY_POINT_NAME,
                                         configuration)
        already = hooks.read(path)
        if not already.usable:
            emit({"command": "hook", "adapter": adapter, "hookFile": str(path),
                  "settings": None, "result": None, "reading": already.refusal(),
                  "note": "the hook file could not be read, so nothing was written: whether"
                          " this adapter is already registered could not be established."})
            return EXIT_REFUSED
        # Ownership before duplication, because they answer different questions and the first
        # one can make the second meaningless. Duplication asks whether appending would leave
        # two registrations in THIS file; ownership asks whether the other owner already holds
        # the event somewhere this file cannot see. A plugin package declares its Stop hook in
        # its own manifest, so the hook file stays empty and every check that reads only the
        # hook file answers "nothing here" while two hooks run on every Stop.
        registered = completion.adapter_entries(already.value, event)
        conflict = completion.ownership_complaints(configuration, owner, registered=registered)
        if conflict:
            emit({"command": "hook", "adapter": adapter, "owner": owner, "settings": None,
                  "hookFile": str(path), "result": None, "error": "; ".join(conflict),
                  "registrations": [entry["identity"] for entry in registered],
                  "note": "nothing was written. One owner registers this event; the other is"
                          " reported with its evidence rather than joined."})
            return EXIT_REFUSED
        duplicate = completion.duplicate_complaints(already.value, event, command, args.timeout)
        if duplicate and owner == completion.OWNER_USER:
            emit({"command": "hook", "adapter": adapter, "settings": None,
                  "hookFile": str(path), "result": None, "error": "; ".join(duplicate),
                  "note": "nothing was written. Writing the settings first would have handed"
                          " them to the registration already in this file, which this command"
                          " is refusing to join."})
            return EXIT_REFUSED
        # Preconditions are settled. Now the writes, settings before the hook that reads them:
        # a hook registered against settings that are not there releases on every Stop and says
        # so nowhere, while settings with no hook cost nothing at all.
        #
        # And before both, for the plugin owner, the fallback launcher. The order is chosen from
        # what each half does alone: a launcher with no settings stands down in silence, while
        # settings whose fallback was never installed look installed and are not. So the half
        # that is harmless on its own goes first, and a refusal here leaves the host untouched.
        # The user owner gets none of this: its registration runs this checkout's own script by
        # absolute path, with no version cache underneath it to be replaced.
        placement = None
        if owner == completion.OWNER_PLUGIN:
            launcher = completion.launcher_path(codex_home)
            try:
                placement = completion.place_launcher(
                    launcher, ROOT / completion.LAUNCHER_SOURCE, apply=args.apply)
            except hostrecord.Busy as error:
                return _hook_busy(adapter, path, error, settings=None, locked=launcher)
            if placement["outcome"] not in completion.LAUNCHER_SETTLED:
                emit({"command": "hook", "adapter": adapter, "owner": owner, "event": event,
                      "settings": None, "launcher": placement, "hookFile": str(path),
                      "result": None, "error": placement.get("detail"),
                      "note": ("Nothing was written. The fallback launcher is installed before"
                               " the settings, so a refusal here leaves this host as it was"
                               " rather than with settings whose fallback is missing.")})
                return EXIT_REFUSED
        try:
            settings = completion.write_configuration(
                configuration, wanted, apply=args.apply)
        except hostrecord.Busy as error:
            return _hook_busy(adapter, path, error, settings=None, locked=configuration)
        if settings["outcome"] not in completion.CONFIG_SETTLED:
            emit({"command": "hook", "adapter": adapter, "settings": settings,
                  "hookFile": str(path), "result": None, "launcher": placement,
                  "note": ("The settings were not written, so no hook was appended. A hook"
                           " registered against settings it cannot act on is installed and"
                           " inert, which is the one outcome worth refusing outright. A"
                           " launcher already placed is left where it is: alone it stands"
                           " down, so removing it would buy nothing and could take a fallback"
                           " from a registration this command did not install.")})
            return EXIT_REFUSED
        if owner == completion.OWNER_PLUGIN:
            # The registration is the plugin package's to declare, so this command writes the
            # settings that registration will read and stops. Appending here as well is the
            # duplicate this owner exists to prevent.
            #
            # Reported as what it is: settings written, nothing registered. A caller reading
            # only the exit status would otherwise record an installed hook, and on this host
            # there is none until the plugin is installed.
            #
            # Both paths are read back here, at the end, because these two files are written
            # under separate locks. A concurrent removal can retire the settings and delete the
            # launcher around this run, and the receipt would otherwise say a fallback was
            # installed when the host no longer has one. This does not prevent that race; it
            # stops it from ending quietly.
            observed = completion.launcher_state(codex_home,
                                                 ROOT / completion.LAUNCHER_SOURCE)
            # BOTH halves, read back at the end. The launcher and the settings are written under
            # separate locks, so either can be taken away between its own write and this point,
            # and a receipt that checked only one would report an installation the host does not
            # have. For the launcher: gone, or there and not what this run put there, because a
            # concurrent installer from another revision leaves a regular file at the same path.
            # For the settings: retired by a concurrent removal, which leaves a declared hook
            # that finds nothing to act on and stands down on every Stop in silence.
            settled = placement["outcome"] in (completion.LAUNCHER_PLACED,
                                               completion.LAUNCHER_UNCHANGED) \
                if placement is not None else False
            launcher_lost = args.apply and settled \
                and (observed["kind"] != "file"
                     or observed["digest"] != placement["sourceDigest"])
            back = reading.read_json(configuration, "the completion hook configuration")
            settings_now = {"configuration": str(configuration), "state": back.state,
                            "matchesWanted": bool(back.usable and back.value == wanted)}
            settings_lost = args.apply and not settings_now["matchesWanted"]
            reasons = []
            if launcher_lost:
                reasons.append("the fallback launcher at " + observed["launcher"] + " is not the"
                               " one this run installed (kind " + str(observed["kind"])
                               + ", digest " + str(observed["digest"]) + ", expected "
                               + str(placement["sourceDigest"]) + ")")
            if settings_lost:
                reasons.append("the settings at " + str(configuration) + " are no longer the ones"
                               " this run wrote (reading " + str(back.state) + "), so the"
                               " declared hook has nothing to act on and stands down on every"
                               " Stop")
            lost = bool(reasons)
            emit({"command": "hook", "adapter": adapter, "owner": owner, "event": event,
                  "settings": settings, "hookFile": str(path), "result": None,
                  "registrations": [], "launcher": placement, "launcherObserved": observed,
                  "settingsObserved": settings_now,
                  "error": ("; ".join(reasons) + ". Something changed this host while the run was"
                            " writing, so what this receipt would have claimed is not what the"
                            " host holds") if lost else None,
                  "note": ("Settings written; no registration was made and the hook file was"
                           " not touched. The " + completion.OWNER_PLUGIN + " owner registers"
                           " this event through the plugin package's own manifest, so install"
                           " that package to register it. Written, registered and observed to"
                           " have fired stay three separate claims. launcherObserved is what"
                           " the launcher path held after the writes and settingsObserved is"
                           " what the settings path held, neither of them what this run"
                           " intended.")})
            return EXIT_INCOMPLETE if lost else EXIT_OK
    else:
        command = args.hook_command
        event = args.event or SESSION_START
        if getattr(args, "owner", completion.OWNER_USER) != completion.OWNER_USER:
            emit({"command": "hook", "adapter": None, "hookFile": str(path), "result": None,
                  "error": "--owner names who registers an adapter this repository owns; an"
                           " explicit --hook-command is registered by whoever ran this command",
                  "note": "nothing was written"})
            return EXIT_USAGE
    hook = {"type": "command", "command": command, "timeout": args.timeout}
    try:
        result = hooks.install(path, event, hook, issue=args.issue, apply=args.apply)
    except hostrecord.Busy as error:
        return _hook_busy(adapter, path, error, settings=settings, locked=path)
    landed = None
    if adapter == COMPLETION and args.apply:
        # Read back after the append, because the duplicate check above and the append itself
        # are not one atomic step: hooks.install takes its own lock, so two runs can both pass
        # the check and both append. Detected and reported rather than claimed away; the append
        # cannot be undone here, because removal renumbers later identities.
        after = hooks.read(path)
        if not after.usable:
            # The promise this block makes is exactly one registration, and a read that did not
            # happen cannot establish it. Skipping the judgment left a CREATED result exiting 0
            # on a claim nobody could check, which is the same silence as claiming settings that
            # were never read back.
            emit({"command": "hook", "adapter": adapter, "settings": settings,
                  "hookFile": str(path), "result": result, "registrations": None,
                  "reading": after.refusal(),
                  "error": "the hook file could not be read back after the append, so whether"
                           " this adapter is registered exactly once could not be established"})
            return EXIT_REFUSED
        landed = completion.adapter_entries(after.value, event)
        # Exactly one, and the append confirmed. Zero means the file was replaced after the
        # append by a writer that does not take this lock; a false read-back means the append
        # itself was not confirmed. Both leave this command claiming an installation nobody
        # can find.
        if len(landed) != 1 or result.get("readBack") is False:
            emit({"command": "hook", "adapter": adapter, "settings": settings,
                  "hookFile": str(path), "result": result,
                  "registrations": [entry["identity"] for entry in landed],
                  "error": ("this adapter is registered " + str(len(landed)) + " times for "
                            + event + " after the append"
                            + ("" if result.get("readBack") is not False
                               else ", and the append was not read back")
                            + "; the hook file changed under this run or the write could not"
                              " be confirmed. Reconcile it by editing the hook file, which"
                              " this command does not do because removal renumbers later"
                              " identities.")})
            return EXIT_REFUSED
        # And it is the registration this run meant to make. Counting one without reading it
        # would accept somebody else's adapter entry as this command's own work.
        survivor = landed[0]
        if (survivor["command"] != command or survivor["timeout"] != args.timeout
                or survivor["matcher"] != hooks.INSTALLED_MATCHER):
            emit({"command": "hook", "adapter": adapter, "settings": settings,
                  "hookFile": str(path), "result": result,
                  "registrations": [survivor["identity"]],
                  "error": ("the one registration for " + event + " after the append is not the"
                            " one this run made: it reads " + repr(survivor["command"])
                            + " with timeout " + repr(survivor["timeout"])
                            + " under matcher " + repr(survivor["matcher"])
                            + ". The hook file changed under this run; reconcile it by hand,"
                              " because removal renumbers later identities.")})
            return EXIT_REFUSED
    emit({
        "command": "hook",
        "adapter": adapter,
        "event": event,
        "settings": settings,
        "hookFile": str(path),
        "result": result,
        "registrations": None if landed is None else [entry["identity"] for entry in landed],
        "note": (
            "Installed, enabled and observed to have fired are three separate claims. This"
            " command appends and reads back; it never enables a daemon and never reports"
            " activation. Removing a hook renumbers later identities, so this command refuses"
            " removal and provides no way to perform one."
        ),
    })
    # The set comes from the module that produces the outcomes, not from a list respelled here.
    return EXIT_OK if result["outcome"] in hooks.SETTLED else EXIT_REFUSED


# ------------------------------------------------------------------------- hook-status

def cmd_hook_status(args):
    """Read what is registered and what this hook recorded about itself, without merging them.

    Writes nothing. A registration says a line is in the hook file; it does not say the host ran
    it, that the runtime it names can answer the call, or that any turn was ever judged. Those
    are separate cells here for exactly that reason.
    """
    codex_home = Path(args.codex_home or os.environ.get("CODEX_HOME") or Path.home() / ".codex")
    answer = completion.status(codex_home=codex_home, event=args.event or completion.EVENT)
    # Beside the rest, never merged into it. An installed fallback says a turn whose version
    # cache was replaced still has something to run; it says nothing about registration, trust
    # or firing, and matchesCheckout is how a copy left behind by an older install becomes
    # visible instead of staying quietly stale.
    answer["launcher"] = completion.launcher_state(codex_home, ROOT / completion.LAUNCHER_SOURCE)
    emit(answer)
    return EXIT_OK


# ------------------------------------------------------------------------- install

def cmd_install(args):
    codex_home = Path(args.codex_home or os.environ.get("CODEX_HOME") or Path.home() / ".codex")
    # The issue is EVIDENCE here, not a label. It is written to the ownership entry's recordedBy,
    # and the predicate that decides whether a link may be replaced requires a value the record
    # actually states -- so a blank one records ownership this command reads back as somebody
    # else's, and the very next update refuses the pointer it placed itself. What a writer emits
    # and what the reader accepts have to be one question, so it is asked of the same helper,
    # here, before anything is read or written.
    # Only for a run that will WRITE. A plan reports what an install would do and records
    # nothing, so it has no evidence to get wrong, and refusing it would be answering a
    # question this invocation never asks.
    if args.apply and not hostrecord.stated(args.issue):
        emit({"command": "install", "applied": False,
              "refused": "--issue is written into the host record as the evidence that this"
                         " command placed the owned pointer, so it has to say something. A"
                         " blank one records an ownership entry this command would read back"
                         " as somebody else's, and the next update would refuse the pointer"
                         " this one placed.",
              "note": "nothing was read, nothing was built and nothing was written."})
        return EXIT_REFUSED
    try:
        with reading.region(definition.DEFINITION_PATH, "the component definition"):
            data = definition.load()
            findings = definition.verify(ROOT)
    except reading.Refused as stop:
        return refused("install", stop.reading)
    if findings:
        emit({"command": "install", "refused": "the definition does not describe this checkout",
              "findings": findings})
        return EXIT_REFUSED

    interpreter = args.python or _find_interpreter(data)
    if not interpreter:
        emit({"command": "install", "refused": "no interpreter satisfying requires-python was found",
              "requiresPython": sorted({c["requiresPython"] for c in data["components"]}),
              "note": "the controller runs on " + ".".join(str(p) for p in sys.version_info[:3])
                      + " and never selects itself for a runtime that needs more"})
        return EXIT_REFUSED

    destination = Path(args.dest).expanduser().absolute()
    # Every component, not the first one. Derived from only the bridge, a relay-only change
    # produced the same directory name and the existence check then refused to install it.
    combined = hashlib.sha256(
        "".join(c["sourceDigest"] for c in data["components"]).encode()
    ).hexdigest()[:12]
    environment = destination / ("env-" + str(data["definitionVersion"]) + "-" + combined)
    record_path = Path(args.record) if args.record else hostrecord.record_path()
    host_record = hostrecord.load(record_path, data["definitionVersion"])
    if not host_record.usable:
        return refused("install", host_record, hostRecord=str(record_path),
                       note="it is never replaced silently: it holds the only evidence of"
                            " what was run here")
    record = host_record.value

    plan = [
        {"step": "verify-definition", "outcome": "passed"},
        {"step": "resolve interpreter", "outcome": str(interpreter),
         "version": interpreter_version(interpreter)},
        {"step": "create environment", "target": str(environment)},
        {"step": "install packages", "from": [c["subdirectory"] for c in data["components"]]},
        {"step": "read imported locations back from the interpreter"},
        {"step": "measure the candidate", "note": "a qualifying OPS-1.3 point is required"},
        {"step": "promote the recorded pointer",
         "note": "only after the candidate is exercised; a candidate that imports but fails its"
                 " exercise stays unselected and the previous runtime remains selected"},
    ]
    if not args.apply:
        emit({"command": "install", "applied": False, "plan": plan, "environment": str(environment),
              "note": "nothing was written. Rerun with --apply to stage the installation."})
        return EXIT_OK

    previous = dict(record.get("selected") or {})
    performed = []

    def perform(name, argv, timeout=900):
        try:
            done = subprocess.run(argv, capture_output=True, text=True, timeout=timeout)
        except (OSError, subprocess.SubprocessError) as error:
            performed.append({"step": name, "command": argv, "ok": False,
                              "detail": type(error).__name__ + ": " + error.__str__()})
            return False
        performed.append({"step": name, "command": argv, "ok": done.returncode == 0,
                          "exitCode": done.returncode,
                          "detail": (done.stderr or done.stdout).strip()[-600:] or None})
        return done.returncode == 0

    destination.mkdir(parents=True, exist_ok=True)
    # OPS-2.4's first measurement: what is selected now, and whether it is still the runtime
    # that was recorded, taken before anything stages over it. It is only READ here; nothing
    # about it is written until this run has proved it owns a destination, because a run that
    # is about to be refused must not have changed the record on its way to the refusal.
    try:
        outgoing = _outgoing_runtime(record, data)
    except reading.Refused as stop:
        return refused("install", stop.reading, hostRecord=str(record_path))

    # An environment directory that already exists is READ before it is refused. The name is
    # derived from the digests, so it is deterministic, and a run killed outright used to leave
    # one behind that refused every retry of this destination for ever. Each input below is one
    # reading's answer; nothing here is inferred from a neighbour.
    pointer_path = Path((record.get("pointer") or {}).get("path")
                        or pointer.pointer_path(destination))
    # ONE lock across deciding, creating and claiming, because those three are one step.
    #
    # The exclusive mkdir proves this run owns the directory, but proving it is not the whole
    # of taking it: between the mkdir and the claim the directory is empty and carries no
    # claim, which is exactly what another run reads as adoptable. It would remove it,
    # recreate it and start building, and then one of the two runs would clean up the other's
    # live build. Reading and acting were already serialised; creating and claiming have to be
    # inside the same span or the window simply moves.
    owned = None
    holder = None
    try:
        taking = hostrecord.Locked(environment).__enter__()
    except hostrecord.Busy as error:
        # A lock another run holds establishes nothing about this directory, which is the same
        # answer release_candidate gives for a record it cannot read. Letting it out would
        # report a competing run as an internal defect in this command.
        emit({"command": "install", "applied": False, "environment": str(environment),
              "refused": "another run is deciding what to do with this directory: " + str(error),
              "note": "nothing was read, nothing was removed and nothing was written."})
        return EXIT_REFUSED
    try:
        if environment.exists():
            protected, protection = protected_environment(record, environment, destination,
                                                          data)
            decision, why = staging.decide(
                staging.read_claim(environment),
                staging.owner_liveness(environment)[0],
                occupied=staging.directory_occupied(environment)[0],
                protected=protected,
                # The narrow half of the same reading. Removing asks the conservative question;
                # reporting an installation and writing a pointer ask this one, because a
                # reading that failed must authorise neither.
                selected=protection["recordSelectsIt"])
            standing = {"command": "install", "applied": False,
                        "environment": str(environment), "stagingDecision": decision,
                        "stagingReason": why, "protection": protection, "plan": plan,
                        "outgoing": outgoing}
            if decision == staging.SETTLED:
                # The claim and the selection say this environment is installed and in use.
                # They say nothing about the path a host actually reaches it through, and
                # reporting an installation while the registered command dangles or resolves
                # somewhere else is a success claim about something nobody read.
                reaches = pointer.names(pointer_path, environment)
                if reaches is not True:
                    emit(dict(standing, alreadyInstalled=False,
                              pointer=dict(pointer.read(pointer_path),
                                           namesThisEnvironment=reaches),
                              refused="this environment is installed and selected, but the"
                                      " owned pointer does not name it, so the command a host"
                                      " reaches is not the runtime that is selected",
                              note="nothing was built and nothing was written. Run"
                                   " register-mcp against the pointer, or rerun once the"
                                   " pointer can be read."))
                    return EXIT_REFUSED
                emit(dict(standing, alreadyInstalled=True,
                          selected=record.get("selected") or {},
                          pointer={"path": str(pointer_path), "target": str(environment)},
                          note="nothing was built and nothing was written."))
                return EXIT_OK
            if decision == staging.RESUME:
                # A previous run committed this environment as selected and did not live to
                # move the pointer. Rebuilding is the wrong repair: it is built, it is already
                # selected, and a process may be running out of it.
                return _finish_promotion(record_path, data, environment, pointer_path, standing,
                                         issue=args.issue, socket_path=args.socket,
                                         state=args.state, reported={
                    "resumed": True,
                    # This run finished a REPLACEMENT somebody else committed, so it reports
                    # the promotion marker. The adoption below does not: it replaced nothing.
                    "promoted": True,
                    "note": "a previous run committed this environment as selected and did not"
                            " live to move the pointer. Nothing was rebuilt and nothing was"
                            " removed: the missing half of that promotion was written. Whether"
                            " the claim that RECORDS it settled is reported separately in"
                            " 'claimSettled', because the pointer and the record are written at"
                            " different moments and a note speaking for both would be speaking"
                            " for one it never read."})
            if decision == staging.RECORDED:
                # An installation made before this command wrote claims. It carries no claim,
                # so every earlier reading called it somebody else's directory and refused --
                # which refused the whole installed base this update exists to move forward.
                # The host record positively selects it, so it is this host's own runtime: the
                # bookkeeping it never had is written and nothing is rebuilt or removed.
                return _finish_promotion(record_path, data, environment, pointer_path, standing,
                                         issue=args.issue, socket_path=args.socket,
                                         state=args.state, reported={
                    "adopted": True,
                    # Not promoted. This run replaced no runtime -- the record already
                    # selected this installation -- and saying otherwise on a non-zero exit
                    # would describe a swap that never happened. 'inService' is what that exit
                    # needs a reader to know, and this path reports it like the other one.
                    "promoted": False,
                    "note": "this installation was made before this command wrote staging"
                            " claims, and the host record selects it. It is brought under this"
                            " command's bookkeeping -- the pointer it is reached through, and"
                            " the claim reported in 'claimSettled' -- so the NEXT update can"
                            " move it. Nothing was rebuilt,"
                            " nothing was removed, and the runtime a host reaches is the one"
                            " the record already selected. Its bytes were not re-measured"
                            " here: this run replaced nothing, and the swap gate is asked"
                            " where something is replaced."})
            if decision == staging.ADOPT:
                # rmdir, never rmtree. It succeeds only on an empty directory, so the call is
                # its own proof that nothing was destroyed, and the exclusive mkdir below still
                # establishes ownership the way it always did.
                # This command's own leftovers first. A previous run whose claim write failed
                # leaves a lock file here, and rmdir refuses a directory that still holds one --
                # which blocked the deterministic destination for ever.
                cleared = staging.clear_own(environment)
                try:
                    os.rmdir(str(environment))
                except OSError as error:
                    emit(dict(standing, refused="the empty staging directory could not be"
                                                " taken over: " + type(error).__name__ + ": "
                                                + str(error),
                              residualPaths=[str(environment)]))
                    return EXIT_REFUSED
                performed.append({"step": "take over an empty staging directory", "ok": True,
                                  "detail": why, "clearedOwnFiles": cleared})
            elif decision in staging.REMOVES:
                try:
                    shutil.rmtree(str(environment))
                except OSError as error:
                    emit(dict(standing, refused="the abandoned staging could not be removed: "
                                                + type(error).__name__ + ": " + str(error),
                              residualPaths=[str(environment)]))
                    return EXIT_REFUSED
                if environment.exists():
                    emit(dict(standing, refused="the abandoned staging is still there after"
                                                " removal", residualPaths=[str(environment)]))
                    return EXIT_REFUSED
                performed.append({"step": "reclaim abandoned staging", "ok": True,
                                  "detail": why})
            else:
                emit(dict(standing, refused=why,
                          note="an existing environment is never overwritten. Only a directory"
                               " carrying a claim this command wrote, whose owner is"
                               " established gone and which nothing is using, is removed."
                               " Nothing was written to the host record."))
                return EXIT_REFUSED
        # Exclusive: this fails if the directory exists, which is what proves the run owns it
        # and may therefore remove it on failure. An exists() test before a separate create
        # does not.
        try:
            environment.mkdir()
        except FileExistsError:
            emit({"command": "install",
                  "refused": "the environment directory already exists",
                  "environment": str(environment), "plan": plan, "outgoing": outgoing,
                  "note": "an existing environment is never overwritten, and a run only"
                          " removes a directory it created itself. Nothing was written to"
                          " the host record."})
            return EXIT_REFUSED
        owned = environment
        # Claimed while the same lock is still held, so no other run can read this directory
        # between its creation and its claim. The advisory lock is held for the RUN: the
        # operating system releases it when this process ends however it ends, which is
        # exactly the question a later run asks.
        try:
            holder = staging.Held(environment).take()
            staging.write_claim(environment, staging.STAGING, issue=args.issue,
                                run=str(os.getpid()))
        except OSError as error:
            # Past the exclusive mkdir this run owns the directory, and a claim that could not
            # be written does not change that. Left to escape, it took the directory and its
            # lock file with it, and the next run's ADOPT then met a lock file rmdir would not
            # remove -- the permanent refusal this path exists to prevent, by another door.
            performed.append({"step": "claim the staging directory", "ok": False,
                              "detail": type(error).__name__ + ": " + error.__str__()})
            return _install_failed(record_path, data["definitionVersion"], performed,
                                   environment, owned,
                                   failed_step="claim the staging directory")
        performed.append({"step": "claim the staging directory", "ok": True,
                          "claim": str(staging.claim_path(environment))})
    finally:
        # Released on every path, including the returns above. It guards deciding, creating and
        # claiming; the build that follows is guarded by the staging lock this run now holds.
        taking.__exit__()

    # Past the exclusive mkdir this run owns a directory, and owning it obliges it to release
    # it however the run ends. A returned failure and a raised one are the same obligation:
    # an escaping exception used to leave the deterministic environment name behind, and the
    # next run then refused that destination for ever.
    try:

        # Ownership is proven, so this run may record what it observed on the way in.
        staged = hostrecord.update(record_path, data["definitionVersion"], outgoing=outgoing)
        if not staged.usable:
            # Past the exclusive mkdir, so every exit releases what this run created. Returning
            # a bare refusal here would leave the deterministic directory behind and refuse every
            # retry of the same destination for ever.
            return _install_failed(record_path, data["definitionVersion"], performed, environment,
                                   owned, failed_reading=staged)

        if not perform("create environment", [str(interpreter), "-m", "venv", str(environment)]):
            return _install_failed(record_path, data["definitionVersion"], performed, environment, owned)

        python = environment / "bin" / "python"
        packages = [str(ROOT / c["subdirectory"]) for c in data["components"]]
        if not perform("install packages", [str(python), "-m", "pip", "install", "--quiet", *packages]):
            return _install_failed(record_path, data["definitionVersion"], performed, environment, owned)

        version = interpreter_version(python)
        installs = {}
        facts = {}
        for component in data["components"]:
            location, error, _argv = module_location(python, component["module"])
            if not location:
                performed.append({"step": "read imported location", "component": component["component"],
                                  "ok": False, "detail": error})
                return _install_failed(record_path, data["definitionVersion"], performed, environment, owned)
            with reading.region(location, "the bytes installed for " + component["component"]):
                digest = definition.ops12_digest(location)
            install = {
                "location": location,
                # Read back from the interpreter: an editable install leaves nothing under
                # site-packages and a copied one does, so the mode follows the location.
                # Containment over resolved parts, not a substring: /opt/env-other contains the
                # text /opt/env, and reading a neighbouring environment's install as this one's
                # copy is the same class of error as a prefix test on a recorded root.
                "installMode": "copied" if within(Path(location).resolve(), environment.resolve())
                               else "editable",
                "entryPoint": str(environment / "bin" / component["consoleScript"]),
                "environment": str(environment),
                "interpreter": version,
                # The interpreter this run actually installed with, recorded so a later
                # classification asks the record rather than reading the console script's first
                # line. Those lines have more than one shape and one of them names /bin/sh.
                "interpreterPath": str(python),
                "integrity": digest,
                "digestMatchesDefinition": digest == component["sourceDigest"],
                "reachedVia": "installed by runtime_install.py into " + str(destination),
            }
            facts[component["component"]] = {
                # Recorded at install time, from the checkout the bytes actually came from, so
                # the identity in the record is the one this run installed rather than whatever
                # the checkout says later.
                "repositoryCommit": definition.git(["rev-parse", "HEAD"], ROOT),
                "repositoryTree": definition.git(["rev-parse", "HEAD^{tree}"], ROOT),
                "subdirectoryTree": definition.git(
                    ["rev-parse", "HEAD:" + component["subdirectory"]], ROOT),
                "workingTreeClean": definition.working_tree_clean(ROOT),
            }
            hostrecord.put_install(record, component["component"], install)
            installs[component["component"]] = install
            performed.append({"step": "read imported location", "component": component["component"],
                              "ok": True, "location": location,
                              "digestMatchesDefinition": install["digestMatchesDefinition"]})

        written = hostrecord.update(
            record_path, data["definitionVersion"],
            installs=[(name, install) for name, install in installs.items()],
            component_facts=facts,
        )
        if not written.usable:
            return _install_failed(record_path, data["definitionVersion"], performed, environment,
                                   owned, failed_reading=written)
        measurement = measure_candidate(data, record, python=python, environment=environment,
                                        socket_path=args.socket, state=args.state,
                                        relay_command=str(environment / "bin" / RELAY),
                                        measured_by=args.issue)
        # A point measured during this run is a delta, applied to the record as it stands now.
        # Measuring takes minutes; anything appended in the meantime is not this run's to drop.
        if measurement.get("points"):
            appended = hostrecord.update(
                record_path, data["definitionVersion"],
                points=[(name, point) for name, point in measurement["points"]])
            if not appended.usable:
                return _install_failed(record_path, data["definitionVersion"], performed,
                                       environment, owned, failed_reading=appended)
        if not measurement["qualifyingPoint"]:
            # The candidate imports but does not work. Release the destination the same way any
            # other failure does, so a transient connection failure does not block every retry.
            performed.append({"step": "measure the candidate", "ok": False,
                              "detail": measurement.get("refused")
                              or "the candidate was not exercised successfully"})
            return _install_failed(record_path, data["definitionVersion"], performed, environment, owned)

        # PROMOTION IS ONE CRITICAL SECTION, AND EVERY JUDGMENT IN IT READS ITS OWN STATE.
        #
        # Three separate review findings were one defect wearing three hats: the swap gate ran
        # against the record loaded before the build, the rollback baseline was captured before
        # the build, and the classification read a pointer at the destination rather than the
        # one the record names and this swap actually replaces. Each is the same shape -- a
        # decision taken inside the critical section on a value read outside it, which another
        # run may have replaced in between -- and fixing them one at a time would have left the
        # fourth to arrive as another round.
        #
        # So the boundary moved rather than the instances: the lock opens first, the record is
        # read inside it, and the gate, the baseline, the pointer reading and the classification
        # are all decided on that reading. PROMOTION_FRESH declares the set and a check enforces
        # it, because a rule nobody checks is how the previous three got in.
        landed = None
        try:
            with hostrecord.Exclusive(record_path):
                fresh = hostrecord.load(record_path, data["definitionVersion"])
                # The path this swap actually replaces, re-derived from the reading taken
                # inside this lock. Derived before it -- as it is for the staging decision
                # above, where it is the right value -- it is a path another run's promotion
                # may have recorded somewhere else in the meantime, and then the link this
                # swap reads, guards and replaces is not the link a host reaches through.
                # Assigned before the refusal below reports it, so the member is read fresh
                # everywhere in this section.
                # The ownership entry as this run FOUND it, read where the path is derived from
                # it and before anything is written. Whether this promotion INTRODUCES that
                # entry or refreshes one that was already there is the whole of what its
                # rollback may take away, and the write below is a merge that erases the
                # difference -- so the difference is read here and carried to the rollback.
                owned_before = (fresh.value or {}).get("pointer")
                pointer_path = Path((owned_before or {}).get("path")
                                    or pointer.pointer_path(destination))
                if not fresh.usable:
                    return _install_failed(record_path, data["definitionVersion"], performed,
                                           environment, owned, pointer_path=pointer_path,
                                           failed_reading=fresh,
                                           failed_step="read the host record for promotion")

                # The selection this promotion replaces. It is the gate's subject AND the
                # rollback baseline, and both were previously taken from a reading made before
                # the build -- minutes earlier, and by then possibly somebody else's runtime.
                previous_selection = dict(fresh.value.get("selected") or {})

                # OPS-4.4 decides whether a runtime may be replaced at all. The daemon and the
                # store belong to the runtime that is selected NOW, so the gate is asked of the
                # relay this reading names; on a first install nothing is selected and the
                # candidate answers for a host that has neither. Nothing here starts or stops a
                # service (OPS-4.1).
                gate = _swap_gate(data, fresh.value, environment=environment, python=python,
                                  socket_path=args.socket, state=args.state)
                if gate["verdict"] != swapgate.ALLOWED:
                    # The existing installation is kept exactly as it stands. A cell that could
                    # not be read keeps it for the same reason a refusal does.
                    performed.append({"step": "read whether it is safe to swap", "ok": False,
                                      "detail": json.dumps({"verdict": gate["verdict"],
                                                            "blockedBy": gate["blockedBy"],
                                                            "unreadable": gate["unreadable"]})})
                    return _install_failed(record_path, data["definitionVersion"], performed,
                                           environment, owned, pointer_path=pointer_path,
                                           failed_step="read whether it is safe to swap",
                                           gate=gate)
                performed.append({"step": "read whether it is safe to swap", "ok": True,
                                  "detail": gate["verdict"],
                                  "selectionMovedWhileBuilding": previous_selection != previous})

                # Read first, so a pointer this command may not replace refuses while nothing
                # has moved. A real directory there belongs to somebody else, and a reading
                # that failed established nothing; neither is placed over.
                before = pointer.read(pointer_path)
                if not pointer.usable(before["state"]):
                    performed.append({"step": "read the owned pointer", "ok": False,
                                      "detail": before["detail"]})
                    return _install_failed(record_path, data["definitionVersion"], performed,
                                           environment, owned, pointer_path=pointer_path,
                                           failed_step="read the owned pointer")
                # A link is not this command's merely because it is a link. Renaming over one
                # succeeds whoever made it, so ownership is established from the record: a
                # pointer this command placed is recorded when it is placed, and a link nobody
                # recorded belongs to somebody else.
                # The question is whether a link this command PLACED is recorded at that path,
                # which is not the question of which path the record names. A rollback that
                # established the link was gone keeps the path -- the registration depends on it
                # -- and withdraws the placement, so a link that turns up there afterwards is
                # still somebody else's and is still refused here.
                if before["state"] == pointer.LINK and not hostrecord.placement_recorded(
                        owned_before):
                    performed.append({"step": "establish the pointer is this command's",
                                      "ok": False,
                                      "detail": "a symbolic link is already at "
                                                + str(pointer_path)
                                                + " and this host record has never recorded"
                                                  " placing one there"})
                    return _install_failed(
                        record_path, data["definitionVersion"], performed, environment, owned,
                        pointer_path=pointer_path,
                        failed_step="establish the pointer is this command's")

                # The selection moves only to something this command's own classification calls
                # own, and the classification is decided on this reading for the same reason
                # everything else here is.
                links = skill_links(codex_home)
                # The registration names the owned POINTER, which is stable across updates, and
                # not the environment underneath it, which changes every time the sources do.
                # The path comes from the record where one is recorded, because this comparison
                # is string equality and a destination spelled differently on a later run is a
                # different string for the same directory.
                bridge_entry = str(pointer_path / "bin"
                                   / component_of(data, BRIDGE)["consoleScript"])
                registration = registration_state(codex_home, bridge_entry, [],
                                                  compare_args=False)
                # A host installed before the pointer existed registers a CONCRETE entry point,
                # and comparing it against the pointer reads as a conflict. It is not one: it is
                # this command's own previous registration, recorded in the host record, and
                # treating it as somebody else's would refuse every upgrade of exactly the
                # installed base the pointer exists to unpin.
                inherited = _inherited_registration(registration, fresh.value, data)
                if inherited:
                    registration = dict(registration, outcome=codexconfig.LINKED,
                                        detail=inherited["detail"], inherited=inherited)
                # The link this swap will actually replace, not whichever one sits under the
                # destination this run was invoked with. A record can name a pointer under an
                # earlier destination, and classifying the wrong path reported NO_POINTER while
                # the promotion below went on to overwrite the real one.
                pointer_read = pointer_state(pointer_path, fresh.value, data)
                verdicts = {}
                for component in data["components"]:
                    name = component["component"]
                    verdicts[name] = classify_component(
                        component, record=fresh.value,
                        entry_override=installs[name]["entryPoint"],
                        # The MCP registration is the bridge's, and says nothing about the relay.
                        registration=registration if name == MCP_NAME else None,
                        links=links,
                        pointer=pointer_read,
                        # Freshly observed by the measurement this promotion is about, so
                        # measurement, classification and promotion all speak about the same
                        # App Server.
                        app_server=measurement.get("appServer"))
                unqualified = {n: {"class": v["class"], "reasons": v["reasons"]}
                               for n, v in verdicts.items() if not ownership.reusable(v["class"])}
                if unqualified:
                    # The reasons travel with the refusal because the commonest one is not a
                    # fault in the installation at all: a checkout with uncommitted changes
                    # cannot be attributed to a revision, so what was installed from it is not
                    # reusable no matter how well the installation went.
                    performed.append({"step": "classify the candidate", "ok": False,
                                      "detail": "the candidate would not be reusable: "
                                                + json.dumps(unqualified)})
                    return _install_failed(record_path, data["definitionVersion"], performed,
                                           environment, owned, pointer_path=pointer_path,
                                           failed_step="classify the candidate")

                # Only the components this run installed. A whole selection map would re-assert
                # entries read before the installation as though they were current.
                #
                # The selection is committed BEFORE the pointer moves. Reversed, a run can land
                # the symlink, fail at the record, and have recovery read a selection that does
                # not name this environment, remove it, and leave the registered command aimed
                # at a directory that no longer exists. OPS-4.4 requires every state transition
                # to be committed before its side effect.
                promoted = hostrecord.update(
                    record_path, data["definitionVersion"],
                    select={name: install["location"] for name, install in installs.items()},
                    pointer={"path": str(pointer_path), "recordedAt": now(),
                             "recordedBy": args.issue})
                if not promoted.usable:
                    return _install_failed(record_path, data["definitionVersion"], performed,
                                           environment, owned, failed_reading=promoted,
                                           pointer_path=pointer_path,
                                           failed_step="commit the selection")
                record = promoted.value
                try:
                    pointer.place(pointer_path, environment)
                    landed = pointer.names(pointer_path, environment)
                except OSError as error:
                    # The selection landed and the pointer did not. The baseline this puts back
                    # is the one read a few lines above, inside this lock, so it restores the
                    # selection this promotion actually replaced rather than whatever was there
                    # before the build.
                    performed.append({"step": "replace the owned pointer", "ok": False,
                                      "detail": type(error).__name__ + ": " + error.__str__()})
                    # place() can fail with the link already replaced, so the same restoration
                    # answers this branch: whatever is there now goes back to what was found.
                    put_back = _restore_pointer(pointer_path, before, environment, record_path,
                                                data["definitionVersion"], owned_before)
                    performed.append({"step": "put the pointer back", "ok": put_back["verified"],
                                      "detail": put_back["detail"]})
                    return _install_failed(
                        record_path, data["definitionVersion"], performed, environment, owned,
                        pointer_path=pointer_path, failed_step="replace the owned pointer",
                        pointer_restored=put_back,
                        restored=_restore_selection(record_path, data["definitionVersion"],
                                                    previous_selection, installs))

                # Read back rather than trusted. A swap reported as done that did not land is
                # the one failure that would leave the record naming a runtime no host can
                # reach.
                if landed is not True:
                    performed.append({"step": "read the owned pointer back", "ok": False,
                                      "detail": pointer.read(pointer_path).get("detail")})
                    put_back = _restore_pointer(pointer_path, before, environment, record_path,
                                                data["definitionVersion"], owned_before)
                    performed.append({"step": "put the pointer back", "ok": put_back["verified"],
                                      "detail": put_back["detail"]})
                    return _install_failed(
                        record_path, data["definitionVersion"], performed, environment, owned,
                        pointer_path=pointer_path, failed_step="read the owned pointer back",
                        pointer_restored=put_back,
                        restored=_restore_selection(record_path, data["definitionVersion"],
                                                    previous_selection, installs))
                performed.append({"step": "replace the owned pointer", "ok": True,
                                  "previousTarget": before.get("target"),
                                  "target": str(environment)})
        except reading.Refused:
            raise
        except hostrecord.Busy as error:
            # Another run holds the promotion. A lock this run could not take establishes
            # nothing, so the candidate is released and nothing owned is touched.
            performed.append({"step": "take the promotion lock", "ok": False,
                              "detail": str(error)})
            return _install_failed(record_path, data["definitionVersion"], performed,
                                   environment, owned, pointer_path=pointer_path,
                                   failed_step="take the promotion lock")
        except OSError as error:
            performed.append({"step": "promote under the promotion lock", "ok": False,
                              "detail": type(error).__name__ + ": " + error.__str__()})
            return _install_failed(record_path, data["definitionVersion"], performed,
                                   environment, owned, pointer_path=pointer_path,
                                   failed_step="promote under the promotion lock")

        # The claim settles last. It says this staging finished, and until the selection and the
        # pointer both name it there is nothing finished to say. Which makes it the one step
        # whose failure finds the replacement already done, so it answers rather than raises
        # and the result carries the two outcomes side by side.
        settled = _settle_claim(environment, staging.COMPLETE, issue=args.issue,
                                run=str(os.getpid()), record_path=record_path,
                                definition_version=data["definitionVersion"], data=data)

        emit({
            "command": "install", "applied": True, "environment": str(environment),
            "hostRecord": str(record_path), "steps": performed, "installs": installs,
            "measurement": measurement,
            "promoted": True,
            # What a consumer must not destroy, reported under the same name the resume exit
            # uses. 'promoted' says this run replaced a runtime; this says the record selects
            # this environment and the owned pointer names it, read after the promotion rather
            # than asserted -- a competing install can supersede this one between the lock
            # releasing and this emit, and a constant here would deny it.
            "inService": settled["inService"],
            # Two answers, never one. 'promoted' is the replacement -- the selection is
            # committed and the pointer resolves into this environment -- and 'claimSettled' is
            # the record of it. They are written at different moments and they can differ, and a
            # result that folded them together reported a host that had already moved as a host
            # that had not.
            "claimSettled": settled["settled"],
            "claim": settled,
            # The same key a refusal reports it under, so a reader looking for what has to be
            # done next finds the answer in one place whichever exit they are reading. None when
            # there is nothing outstanding.
            "recoveryRequires": settled["recoveryRequires"],
            "swapGate": gate,
            "pointer": {"path": str(pointer_path), "target": str(environment),
                        "previousTarget": before.get("target"),
                        "meaning": "the registered command reaches a runtime through this path."
                                   " It is a way to reach one and never an identity: a console"
                                   " script keeps its absolute shebang, so a process already"
                                   " spawned goes on running the environment it started in."},
            "classification": {n: v["class"] for n, v in verdicts.items()},
            "selected": record.get("selected") or {},
            "previousSelection": previous_selection,
            "selectionWhenThisRunStarted": previous,
            "note": (
                "the pointer moves only after a qualifying point exists for the candidate"
                " (OPS-2.4). A candidate that imports but fails its exercise stays unselected and"
                " the previous runtime remains selected. Nothing here removes, moves or recreates"
                " the store."
            ),
        })
        # Reached only when the candidate qualified, classified own and was promoted, so this is
        # the one return after ownership that KEEPS the directory: it is the runtime a host now
        # reaches, and releasing it would delete what this run just put into service. Every
        # other exit goes through the release path. Which of the two promoted statuses it is
        # turns on the record alone, so it is chosen here rather than at a second return that
        # nothing would hold to the same rule.
        return EXIT_OK if settled["settled"] else EXIT_INCOMPLETE


    except reading.Refused as stop:
        return _install_failed(record_path, data["definitionVersion"], performed, environment,
                               owned, failed_reading=stop.reading)
    except Exception as error:                                   # noqa: BLE001
        return _install_failed(record_path, data["definitionVersion"], performed, environment,
                               owned, failed_error=error)
    finally:
        # The lock's lifetime is this run's. The operating system releases it when the process
        # ends however it ends, which is what makes a killed run readable as abandoned; a run
        # that reaches an end of its own says so itself rather than leaving the answer to exit.
        if holder is not None:
            holder.__exit__()


def _settle_claim(environment, state, *, issue, run, record_path, definition_version, data):
    """Write the claim that says this staging finished, and answer whether the RECORD landed.

    The claim settles last on purpose: it says the replacement finished, and until the selection
    and the pointer both name this environment there is nothing finished to say. That ordering
    is what makes its failure unlike every other failure in this command. Everything before it
    fails with the previous runtime still selected and still reachable -- a refusal, and the
    result says so truthfully. By the time this one can fail the selection is committed, the
    pointer is placed and read back, and the registered command resolves into this environment:
    THE REPLACEMENT HAPPENED. Only the record of it did not.

    Left to raise, that went out through the handler for failures this command does not model
    and was reported as an update failure -- false in the direction that matters. The operator
    read 'applied: false', a pointer of null, and a destination that 'cannot be retried until
    the selection moves', and none of it said the host had already moved. So this ANSWERS
    instead of raising, and the caller reports the record beside the replacement rather than in
    place of it.

    What the next run should do is part of the answer rather than left to the reader, because
    only here is it known which half is missing. A promotion that could not settle leaves the
    claim its staging wrote -- STAGING -- over a selection and a pointer that both name this
    environment, and that is exactly the state staging.decide() reads as RESUME: the next run
    finishes the bookkeeping, rebuilds nothing and removes nothing. An installation that never
    wrote a first claim leaves none at all, which the same reading answers with RECORDED for
    the same repair. Either way the destination is not stuck and the runtime is not at risk,
    which is the opposite of what the update failure said.

    A LOCK THIS RUN NEVER TOOK IS NOT A LOCK IT STRANDED, and the two arrive as the same type.
    hostrecord.Busy is an OSError, so catching the broad type alone read a COMPETING writer's
    live lock file as this run's leftover and told an operator to delete it -- and because
    Locked excludes by O_EXCL on that filename, deleting it admits a second writer into a
    read-modify-write that is still running. So Busy is caught first and answered as what it
    is: nothing was written here, the file belongs to somebody else, and the next action is to
    wait rather than to remove anything. Anything else is a defect in this command rather than
    a record that would not write, and a defect reported as a settled-looking outcome is how
    one stops being found.

    AND THE RECORD HAS THE SAME SPLIT THE RESULT DOES, so it gets the same treatment rather
    than a branch. Writing the claim is two steps: the bytes are replaced under
    hostrecord.Locked, and the lock is released afterwards. A failure in the SECOND raises with
    the new bytes already on disk, and reading that as an unsettled record is this very
    substitution one layer down -- it would send an operator to repair bookkeeping that is
    already correct. So the answer carries two outcomes of its own, decided by two different
    readings:

      'settled'  -- did the RECORD land. Decided by reading the claim back, never by the
                    exception, because an exception says the call did not finish.
      'released' -- did the CALL finish. False whenever anything raised, whatever landed.

    What failed afterwards is then named as itself rather than folded into the record's
    outcome. A release that failed leaves the lock file it could not unlink, and that file is
    not cosmetic: the next claim write at this path waits on it and then refuses until it is
    gone or older than hostrecord.STALE_LOCK_SECONDS. So it is read on the filesystem, reported
    as a residual path, and carried into the recovery sentence -- the same shape _install_failed
    already reports residue in, for the same reason.

    Fail closed where the readback itself failed. A claim nobody could read establishes
    nothing, and it is not the same case as one that is merely missing: staging.decide()
    answers KEEP for an unreadable claim, so the next run refuses this directory instead of
    repairing it. The advice has to differ because the behaviour does, which is why it is
    derived from the reading rather than written once for every failure.

    AND THE SELECTION IS RE-READ BEFORE ANY OF THAT IS PROMISED. This runs OUTSIDE the
    promotion lock -- deliberately, because settling a claim is not a promotion -- so an
    install that was queued on that lock can promote its own environment between this run
    releasing it and this line. "Rerun and it will finish the bookkeeping" is then false in
    the expensive direction: with the selection moved on, staging.decide() reads this
    directory as an abandoned staging and answers RECLAIM, so following the advice would
    remove and rebuild it. What the next run will do depends on what the record says NOW, so
    that is what the advice is derived from rather than the selection this run committed.
    """
    path = staging.claim_path(environment)
    contended = False
    raised = None
    try:
        staging.write_claim(environment, state, issue=issue, run=run)
    except hostrecord.Busy as error:
        contended = True
        raised = type(error).__name__ + ": " + error.__str__()
    except OSError as error:
        raised = type(error).__name__ + ": " + error.__str__()

    # Still read back, contended or not. The run this one lost the lock to may be the run that
    # finished this very promotion, and a claim it already settled is settled.
    left = staging.read_claim(environment)
    says = (left.value or {}).get("state") if left.ok else None
    settled = says == state
    lock = Path(str(path) + hostrecord.LOCK_SUFFIX)
    # This run's leftover only where this run could have taken the lock at all. Busy says it
    # did not, so whatever is at that path is somebody else's and reporting it as residue
    # would be this command naming another run's live working file for deletion.
    residual = [] if contended else ([str(lock)] if lock.exists() else [])
    # The state as it stands NOW, read as the PAIR the next run's decision takes rather than
    # as the selection alone. 'selected' is the narrow reading that authorises finishing a
    # promotion; 'protected' is the conservative one that keeps a directory something may
    # still reach. The destination is this environment's parent, which is how the name was
    # derived in the first place. An unreadable record leaves both None, and None is never
    # folded into either of the other answers.
    # 'ok' and not 'usable', because ABSENT is usable and it is not an answer about a
    # selection. hostrecord.load() fills an absent record with an EMPTY one, and an empty
    # record selects nothing -- which reads identically to a record that selects somewhere
    # else, and would have this command report a host record that VANISHED as another run
    # having moved on. The authority for what this host selected is gone in that case, and
    # losing it is not the same event as being superseded by a successful promotion.
    # UNDER THE PROMOTION LOCK, because promotions are what move these two truths and they are
    # written one after the other. Read outside it, the record load and the pointer reading can
    # straddle another run's promotion and describe a state that never existed at any instant.
    # A short timeout rather than the promotion default: this is already a failure path, "could
    # not be established" is a modelled answer, and waiting a minute to improve a sentence
    # would be the wrong trade. A lock this call could not take leaves both answers None, which
    # is the same fail-closed answer an unreadable record gets.
    #
    # It is still a SNAPSHOT and the advice says so. Nothing can hold this lock until an
    # operator acts, so what is reported is what the record said at this moment, not a promise
    # about when they read it.
    #
    # OSError and not just Busy. Exclusive.__enter__ makes a directory, opens a file and takes
    # a lock, and any of those can fail for reasons that have nothing to do with contention.
    # Caught narrowly, such a failure escaped into cmd_install's generic handler and came back
    # as 'applied: false' with exit 1 -- this PR's own defect, reintroduced by the lock added
    # to fix a different one -- and out of _finish_promotion it became an internal error with
    # no result at all. A snapshot that could not be taken is an unknown snapshot, however it
    # failed to be taken.
    try:
        with hostrecord.Exclusive(record_path, timeout=SETTLE_SNAPSHOT_TIMEOUT_SECONDS):
            current = hostrecord.load(record_path, definition_version)
            if current.ok:
                # The link the PROMOTION uses, not whichever one sits under the destination
                # this run was invoked with. A record naming a pointer under an earlier
                # destination is the case that makes those two different, and asking about the
                # wrong one reports a runtime the owned pointer still reaches as unprotected.
                protected, protection = protected_environment(
                    current.value, environment, environment.parent, data,
                    pointer_path=(current.value.get("pointer") or {}).get("path"))
                selected = protection["recordSelectsIt"]
                # A SECOND question, and it is not this one. recordSelectsIt folds the entries
                # with any(), so one component inside this environment answers True while the
                # others point elsewhere -- the right reading for "must this directory be
                # kept", and the wrong one for "would a resume finish the promotion here".
                # _finish_promotion requires EVERY configured component through
                # _names_environment and refuses otherwise, so an arm promising a
                # bookkeeping-only rerun has to ask the question that path actually asks.
                finishable = _names_environment(current.value, environment, data)
                names = protection["pointerNamesIt"]
            else:
                protected, selected, names, finishable = None, None, None, None
    except reading.READ_FAILURES as error:
        # Not OSError alone. A host record this command ACCEPTS can carry a truthy non-path in
        # an unrelated 'selected' entry, and protected_environment hands it to Path(), which
        # raises TypeError -- escaping into cmd_install's generic handler and reporting exit 1
        # for a promotion whose selection, pointer and claim had all landed. Every shape a
        # reading can fail in belongs here, which is the set reading already declares.
        current, protected, selected, names, finishable = None, None, None, None, None
        snapshot_detail = ("a consistent snapshot of what this host selects could not be"
                           " taken: " + type(error).__name__ + ": " + error.__str__())
    else:
        snapshot_detail = None

    absent = current is not None and current.state == reading.ABSENT

    # Whether a consumer must keep this destination -- ASKED OF THE ANSWER SET THAT DECIDES IT
    # rather than reconstructed from its inputs.
    #
    # Every case this cell got wrong was the same mistake in a new place: a settled claim, an
    # unreadable claim, a snapshot nobody could take. Each time the rule was restated here and
    # each time it disagreed with staging.decide(), which is what actually removes directories
    # -- and the documented contract points a wrapper at this cell, so every disagreement was
    # a wrapper deleting something this command refuses to. Restating it once more would only
    # move the disagreement, so the guard is decide()'s own answer instead: REMOVES is the
    # declared set of decisions that remove anything, and this is its complement. The two
    # cannot drift apart again, and where they still disagree the disagreement is in decide()
    # where it can be argued about.
    #
    # DEAD, because the question is what a LATER run finds. This process is the only thing
    # holding the staging lock, and it is the only liveness that permits a removal at all.
    # An unread protection is a protection, which is protected_environment's own rule, so the
    # conservative value goes in even though the reported cell keeps saying it was not read.
    occupied, _occupied_detail = staging.directory_occupied(environment)
    decision, _why_kept = staging.decide(
        left, staging.DEAD, occupied=occupied,
        protected=True if protected is None else protected, selected=selected)
    in_service = decision not in staging.REMOVES

    # Ordered as staging.decide() orders it, because that is whose behaviour this describes:
    # an unreadable claim is answered before the state is consulted at all, and only then does
    # the selected/protected pair choose between finishing, keeping and reclaiming.
    #
    # 'contended' is no longer one of these branches. It is a fact about THIS call -- the lock
    # it did not take -- and it was sitting in front of every state-dependent answer asserting
    # a selection it had not read. What the next run will do is decided by the same pair here
    # as anywhere else, and the lock this call lost is said alongside rather than instead.
    if settled:
        record_requires = None
    elif not left.usable and selected is False:
        # An unreadable claim AND a selection that has moved on. decide() keeps this directory
        # either way -- a claim it cannot read is not one it may act on -- but the repair the
        # arm below prescribes is wrong here, because what follows from it depends on the
        # selection it never consulted. Rewriting the claim as STAGING over a selection that
        # has moved makes the next run read an abandoned staging and rebuild; removing it
        # leaves a populated directory with no claim, which reads as somebody else's for ever.
        record_requires = (
            "leave the claim at " + str(path) + " alone. It cannot be read, so this command"
            " will not act on it: the next run keeps this directory and reports it. And the"
            " host record no longer selects this environment, so there is no promotion here"
            " left to finish and nothing this claim would usefully record. Repairing it does"
            " not change that -- rewritten as STAGING over a selection that has moved on it"
            " reads as an abandoned staging and the next run would remove and rebuild the"
            " directory -- and removing it leaves a populated directory carrying no claim,"
            " which reads as somebody else's and is refused from then on.")
    elif not left.usable:
        record_requires = (
            "make the claim at " + str(path) + " readable or remove it, then run install"
            " again. The replacement itself finished and this environment is in service, so it"
            " must not be deleted -- but rerunning alone will NOT repair this one: a claim that"
            " cannot be read is not a claim this command may act on, so the next run reports"
            " the directory and leaves it exactly as it stands rather than finishing the"
            " promotion.")
    elif absent:
        record_requires = (
            "restore the host record at " + str(record_path) + " before rerunning, and do not"
            " remove this environment. The record is GONE, not merely saying something else:"
            " it is the authority for what this host selected, and the promotion this run made"
            " cannot be confirmed or finished by anything without it. The next run reads an"
            " empty record, finds nothing selecting this environment and keeps the directory"
            " rather than repairing it, so rerunning alone settles nothing. The owned pointer"
            " may still reach this environment and a process may still be running out of it.")
    elif not left.usable and selected is None:
        # Unknown is not superseded. The arm above reads a record that positively selects
        # nothing here; this one could not read one at all, and the repair that fits depends
        # on which it turns out to be.
        record_requires = (
            "read " + str(record_path) + " before touching the claim at " + str(path) + ". It"
            " cannot be read, and what this host selects could not be established either, so"
            " which repair applies is unknown: if this environment is still selected the claim"
            " has to be made readable or removed before a rerun can finish the promotion, and"
            " if it has been superseded then repairing it would have the next run rebuild the"
            " directory and removing it would leave one no install will touch again. Nothing"
            " here is at risk meanwhile -- a claim this command cannot read is one it leaves"
            " alone.")
    elif selected is None:
        record_requires = (
            "read " + str(record_path) + " before acting on this"
            + ("" if snapshot_detail is None else " (" + snapshot_detail + ")")
            + ". What this host selects could not be established here, so what the next run"
            " would do with this directory could not be either -- and that is the difference"
            " between finishing the missing bookkeeping, leaving the directory alone, and"
            " removing and rebuilding it. Do not rerun to settle the record until that reading"
            " succeeds.")
    elif finishable is True and names is not True:
        # The record still selects this environment and the LINK does not agree, or could not
        # be read. A rerun is not bookkeeping then: _finish_promotion replaces the pointer
        # before it writes the claim, and refuses outright for a link this record does not
        # account for. Saying "it only settles the claim" understates what the next run does,
        # and understating a write is how somebody authorises one they did not mean to.
        record_requires = (
            "read the owned pointer at " + str((
                (current.value.get("pointer") or {}).get("path") if current.ok else None)
                or pointer.pointer_path(environment.parent))
            + " before rerunning, and clear whatever stopped the write at " + str(path)
            + ". This host record still selects this environment, but the owned pointer does"
            " not name it or could not be read, so a rerun is NOT bookkeeping only: it"
            " replaces that link before it writes the claim, and refuses outright if the link"
            " is one this record does not account for. Nothing is at risk meanwhile -- this"
            " environment stays selected and nothing removes it.")
    elif finishable is not True and selected is True:
        # Some of this environment is selected and some is not. protected_environment answers
        # with any(), so this reads as selected and the directory is rightly kept -- but
        # _finish_promotion requires every configured component and refuses, so promising a
        # bookkeeping-only rerun here would promise something that cannot happen.
        record_requires = (
            "read " + str(record_path) + " before rerunning. It selects part of this"
            " environment and part of another, so there is no single promotion here to finish:"
            " a rerun refuses, because finishing one requires the record to select every"
            " configured component in the same place. The directory is kept and nothing is at"
            " risk; what needs deciding is which environment this host is meant to be on.")
    elif finishable is True:
        record_requires = (
            "clear whatever stopped the write at " + str(path) + " -- the error is in"
            " 'detail' -- and then run install again against the same destination. The"
            " replacement itself finished: this environment is selected and the owned pointer"
            " names it, so there is nothing to rebuild and nothing to undo, and what is"
            " missing is only the claim that records it. A rerun writes that claim: it reads a"
            " selected environment whose claim never settled as an interrupted promotion and"
            " finishes the bookkeeping. BUT ONLY ONCE THE WRITE CAN SUCCEED -- rerunning while"
            " the same thing stops it reaches the same failure and returns this same result,"
            " without rebuilding or removing anything. Until it settles, this destination"
            " carries a runtime that is in service and a claim that does not say so.")
    elif protected:
        record_requires = (
            "leave this directory alone, and do not expect a rerun to settle it. The host"
            " record no longer selects this environment, so there is no promotion here for the"
            " next run to finish -- but something still reaches it, or a reading that would"
            " say otherwise failed, so the next run keeps it and reports it rather than"
            " removing it. A promotion that died before moving the pointer leaves exactly this"
            " shape. Nothing is lost and nothing is at risk; the claim stays unsettled for a"
            " staging this record has moved past.")
    else:
        record_requires = (
            "nothing needs doing about this record, and DO NOT rerun install here to settle"
            " it. Another run moved the selection on after this one promoted, and nothing"
            " selects this environment or points at it now, so a claim recording it records"
            " nothing anybody reads. The next run reads this directory as an abandoned staging"
            " and would remove and rebuild it rather than finish any bookkeeping.")
    # The lock this call never took, said alongside the state answer rather than in place of
    # it. Suppressed when the record already settled, because then there is nothing to wait
    # for: the run that held the lock may well be the one that wrote it.
    contended_requires = None if not (contended and not settled) else (
        "wait for the run that holds " + str(lock) + " before anything else. This call never"
        " took that lock, so it wrote nothing and changed nothing, and the other run may be"
        " writing this very claim. Nothing there is this run's to remove -- that lock file is"
        " a live writer's, and taking it away would let a second writer into a"
        " read-modify-write that is still running.")
    residue_requires = None if not residual else (
        "look at " + str(lock) + " before anything else touches it. A lock file is there and"
        " whether it outlived the call that took it or belongs to a run still writing could"
        " not be established here, so it is reported rather than removed: deleting a live"
        " writer's lock admits a second writer into a read-modify-write that is still running."
        " Nothing has to be done about it by hand in any case -- the next claim write at this"
        " path waits on it and clears it once it is older than "
        + str(hostrecord.STALE_LOCK_SECONDS) + " seconds.")
    return {"path": str(path), "settled": settled, "released": raised is None, "wanted": state,
            "detail": raised,
            "readBack": {"state": left.state, "saying": says, "detail": left.detail},
            "residualPaths": residual,
            # What a consumer must not destroy, from the snapshot above and never a constant.
            "inService": in_service,
            # The snapshot the advice was derived from, reported as its own cell so a reader
            # can see WHICH reading produced it. It is a snapshot and not a guarantee: nothing
            # holds the promotion lock until somebody acts on this.
            "selection": {"state": None if current is None else current.state,
                          "selects": selected, "pointerNames": names, "protected": protected,
                          "detail": snapshot_detail},
            # Composed the way a refusal composes its own, so a reader meets one sentence
            # covering everything outstanding rather than one per thing that went wrong.
            "recoveryRequires": "; and ".join(
                part for part in (contended_requires, record_requires, residue_requires)
                if part) or None}


def _finish_promotion(record_path, data, environment, pointer_path, standing, *, issue,
                      reported, socket_path=None, state=None):
    """Write the half a killed run did not: the pointer, for a selection already committed.

    The two truths are written one after the other inside one lock, so the only thing that can
    land between them is the process dying. That leaves a runtime that is selected and
    unreachable, and rebuilding would be the wrong repair: it is built, it is selected, and a
    process may already be running out of it. So the pointer is brought into agreement with the
    selection and the claim is settled. Nothing is rebuilt and nothing is removed.

    Two callers reach it for the same state read two ways. A killed run leaves a claim and a
    committed selection; an installation older than claims leaves a committed selection and no
    claim at all. Both are a runtime this record selects that no pointer reaches, and both are
    repaired by writing the half that is missing. 'reported' is what the caller says about the
    case it found, because the state is one thing and the reason is not.
    """
    with hostrecord.Exclusive(record_path):
        # Re-read the selection under the lock rather than trusting the decision that got here.
        # The reading that chose RESUME was taken before this lock existed, and the pointer is
        # only ever aimed at an environment the record is CURRENTLY read to select.
        current = hostrecord.load(record_path, data["definitionVersion"])
        if not current.usable:
            emit(dict(standing, refused="the host record could not be read, so whether it"
                                        " selects this environment could not be established: "
                                        + str(current.detail),
                      reading=current.refusal()))
            return EXIT_REFUSED
        if not _names_environment(current.value, environment, data):
            emit(dict(standing, refused="the host record no longer selects this environment, so"
                                        " there is no promotion here to finish"))
            return EXIT_REFUSED
        # The ownership entry as this call found it, read before the write below merges over it
        # and before either question about the link is asked of it. Two things depend on it: what
        # this call's rollback may take away, and whether there is a link here it may replace.
        owned_before = (current.value or {}).get("pointer")
        before = pointer.read(pointer_path)
        if not pointer.usable(before["state"]):
            emit(dict(standing, refused="the missing half of this promotion could not be"
                                        " written: "
                                        + str(before["detail"]),
                      pointer={"path": str(pointer_path), "state": before["state"]}))
            return EXIT_REFUSED
        # Normal promotion asks whether the link is this command's before replacing it, and
        # this path did not. A resume necessarily finds the pointer disagreeing with the
        # selection -- that IS the interruption it repairs -- so the question is narrower: does
        # the link still name a runtime this record accounts for. One repointed by hand during
        # the interruption does not, and overwriting it silently is exactly what the pointer
        # conflict cell exists to stop.
        if before["state"] == pointer.LINK and not _target_is_recorded(
                current.value, before.get("target"), data):
            emit(dict(standing,
                      refused="the owned pointer names " + str(before.get("target"))
                              + ", which this host record does not account for, so it was"
                              " repointed by something other than this command and the"
                              " interrupted promotion is not this run's to finish",
                      pointer={"path": str(pointer_path), "target": before.get("target")}))
            return EXIT_REFUSED
        # There is one thing this record can say that settles the narrower question the other
        # way. An entry holding the PATH and no placement is this command's own statement that
        # no link IT placed is here -- written by a rollback that established the link was gone
        # -- so a link that has turned up there since was put there by something else, and its
        # target naming a runtime this record happens to account for does not make it this
        # command's to replace. Without this the promotion refuses that link and the resume
        # replaces it, which would be the record saying one thing and two readers answering
        # differently.
        #
        # An installation older than claims has NO entry at all, which says nothing either way,
        # and it keeps the adoption this path exists for.
        #
        # Bound to THIS path. The entry is read fresh under this lock while pointer_path was
        # derived before it, so the two can be about different places -- and an entry naming
        # somewhere else says nothing at all about the link here. Read unbound, this guard
        # answered about one path from a reading taken of another, which is the very shape the
        # rest of this change exists to remove.
        owned_here = hostrecord.pointer_entry_for(owned_before, pointer_path)
        if (before["state"] == pointer.LINK and owned_here
                and not hostrecord.placement_recorded(owned_here)):
            emit(dict(standing,
                      refused="a symbolic link is at " + str(pointer_path) + " and this host"
                              " record holds that path without recording that this command"
                              " placed a link there, so it was placed by something else and is"
                              " not this run's to replace",
                      pointer={"path": str(pointer_path), "target": before.get("target")}))
            return EXIT_REFUSED
        # OPS-4.4 AGAIN, AND NOT THE READING THE INTERRUPTED RUN TOOK.
        #
        # The state this path takes over is DURABLE: a selection on disk and a claim beside it,
        # sitting there for however long it took somebody to notice. The gate's three cells all
        # read state outside this process, and all three move while that state sits still. A
        # supervisor can be started; attempts open and close continuously; the store's schema
        # is whatever the selected runtime has since migrated it to. So none of them may be
        # carried across the interruption -- which CRW-92 settled for the daemon in the
        # strongest form, that a prior ALLOWED cannot cross a process boundary at all.
        #
        # There was in fact nothing to carry, and that is the sharper statement of the defect.
        # The interrupted run died before recording any verdict, so the durable state holds no
        # gate reading of any kind: this path was not reusing a stale ALLOWED, it was moving a
        # host's runtime having never asked. The candidate's own declared schema is the one
        # input that cannot have changed -- it is derived from bytes that are already built --
        # but it is only ever read as half of a comparison against a store that can, so even
        # that buys no reuse.
        #
        # ASKED WHERE SOMETHING IS REPLACED, which is not every caller. A resume finds a link
        # naming the predecessor and moves a host from it to this environment: that is a swap
        # and the gate decides it. An installation older than claims usually has no link at
        # all, and writing the first one changes which PATH reaches a runtime the record
        # already selects rather than which runtime is reached -- nothing is replaced, which is
        # why its own result says the gate is asked where something is. The test is therefore
        # the link itself rather than which caller this is: a link that already names this
        # environment, or no link, replaces nothing.
        #
        # Before anything is written. The ownership entry below is this call's first write, and
        # a refusal after it would leave the record claiming a placement for a link that was
        # never placed.
        replacing = before["state"] == pointer.LINK and pointer.names(
            pointer_path, environment) is not True
        if replacing:
            gate = _swap_gate(data, current.value, environment=environment,
                              python=environment / "bin" / "python",
                              socket_path=socket_path, state=state)
            if gate["verdict"] != swapgate.ALLOWED:
                # Named, and named by the cell that decided it. "The gate said no" sends an
                # operator to read a command's source; the verdict, what blocked and what could
                # not be read send them to the daemon, the attempts or the store.
                emit(dict(standing,
                          refused="the interrupted promotion was not finished: moving the"
                                  " owned pointer to this environment replaces the runtime a"
                                  " host reaches, and the swap gate answered "
                                  + str(gate["verdict"]) + " for "
                                  + (", ".join(gate["blockedBy"] or gate["unreadable"] or [])
                                     or "a condition it did not name")
                                  + ". The reading the interrupted run took cannot stand in"
                                  " for this one: it died before recording any verdict, and"
                                  " every cell here reads state that moves while an"
                                  " interrupted promotion sits on disk",
                          swapGate=gate,
                          pointer={"path": str(pointer_path),
                                   "target": before.get("target")},
                          retriable=True,
                          recoveryRequires="nothing was written and nothing was removed. This"
                                           " environment stays selected and the owned pointer"
                                           " still names what it named, so the destination can"
                                           " be retried as it stands: clear what the gate"
                                           " named and run install again."))
                return EXIT_REFUSED
        # A pointer this command owns is RECORDED when it is placed, and this path placed one
        # without recording it. An installation older than claims has no such record, so the
        # link written here was a link nobody recorded -- and the next update refuses to
        # replace one of those. Adopting a host once and then refusing it for ever is the
        # failure this command exists to remove, so the record is written with the link.
        #
        owning = hostrecord.update(record_path, data["definitionVersion"],
                                   pointer={"path": str(pointer_path), "recordedAt": now(),
                                            "recordedBy": issue})
        if not owning.usable:
            emit(dict(standing, refused="the pointer could not be recorded as this command's,"
                                        " so placing one would leave a link the next update"
                                        " refuses to replace: " + str(owning.detail),
                      reading=owning.refusal()))
            return EXIT_REFUSED
        try:
            pointer.place(pointer_path, environment)
        except OSError as error:
            put_back = _restore_pointer(pointer_path, before, environment, record_path,
                                        data["definitionVersion"], owned_before)
            emit(dict(standing, refused="the missing half of this promotion could not be"
                                        " written: "
                                        + type(error).__name__ + ": " + str(error),
                      pointerRestored=put_back, **_outstanding_ownership(put_back)))
            return EXIT_REFUSED
        landed = pointer.names(pointer_path, environment)
        if landed is not True:
            # Put back what was found, absence included, so a destination this call could not
            # repair is left the way it was rather than holding a link nothing selects.
            put_back = _restore_pointer(pointer_path, before, environment, record_path,
                                        data["definitionVersion"], owned_before)
            emit(dict(standing, refused="the pointer did not land on the environment the host"
                                        " record already selects",
                      pointerRestored=put_back, **_outstanding_ownership(put_back)))
            return EXIT_REFUSED
    # Outside the lock, and previously outside every handler too: an OSError here escaped this
    # function, escaped cmd_install through a finally with no except, and left the command with
    # no result and no exit status at all -- for a repair that had already written the pointer.
    settled = _settle_claim(environment, staging.COMPLETE, issue=issue, run=str(os.getpid()),
                            record_path=record_path,
                            definition_version=data["definitionVersion"], data=data)
    emit(dict(standing, applied=True,
              # What a consumer must not destroy, and it is not 'promoted'. This function
              # serves two decisions: RESUME finishes a promotion, and RECORDED adopts
              # bookkeeping for an installation this run replaced NOTHING to obtain -- so
              # 'promoted' is the caller's to state and travels in 'reported'. This is the
              # other question, and it is read rather than asserted: the snapshot taken after
              # the claim write says whether the record still selects this environment and the
              # owned pointer still names it.
              inService=settled["inService"],
              pointer={"path": str(pointer_path), "previousTarget": before.get("target"),
                       "target": str(environment)},
              claimSettled=settled["settled"], claim=settled,
              recoveryRequires=settled["recoveryRequires"],
              **reported))
    return EXIT_OK if settled["settled"] else EXIT_INCOMPLETE


def _inherited_registration(registration, record, data):
    """Whether a CONFLICT is really this command's own earlier registration.

    Returns a note when the registered command is an entry point the host record recorded for
    an install of ours, and nothing otherwise. That is positive proof of ownership: a path that
    merely looks like ours proves nothing, and a registration nobody recorded stays the conflict
    it is.

    Recognising it is not migrating it. The configuration still names the predecessor, which is
    preserved and still works, and moving the registration onto the pointer is a separate
    operation with its own contract; this only stops an inherited registration from refusing an
    update and destroying the candidate it built.
    """
    if not registration or registration.get("outcome") != codexconfig.CONFLICT:
        return None
    registered = (registration.get("registered") or {}).get("command")
    if not registered:
        return None
    # The bridge's entry points and nothing else. This exception exists for the bridge's
    # pre-pointer registration, so a relay entry point that happens to sit in the same record
    # is not evidence that [mcp_servers.<bridge>] naming it is this command's own registration.
    # Widened to every component, a configuration registering the relay CLI as the bridge
    # server read as inherited and the candidate promoted while Codex went on launching the
    # wrong process.
    entry = ((record or {}).get("components", {}).get(MCP_NAME) or {})
    recorded = [str(install["entryPoint"]) for install in entry.get("installs") or []
                if install.get("entryPoint")]
    if str(registered) not in recorded:
        return None
    return {
        "registeredCommand": str(registered),
        "recordedInstall": True,
        "detail": (
            "the configuration registers " + str(registered) + ", which this host record"
            " recorded as an entry point of an install this command made. It is this command's"
            " own earlier registration rather than a foreign one, so it does not refuse the"
            " update. It is NOT moved onto the pointer here: the configuration still names the"
            " predecessor, which is preserved and still works, and re-registering is a separate"
            " operation."
        ),
    }


def _target_is_recorded(record, target, data):
    """Whether a pointer target names a runtime this host record accounts for.

    True for an environment or install location the record holds, and False for anything else
    INCLUDING a target that could not be resolved, because this answer authorises replacing a
    link and an unread answer authorises nothing.

    Equality, and not containment in either direction. A target that CONTAINS a recorded path
    is not a recorded runtime: the destination root is the parent of every environment under
    it, so a link repointed at the destination read as accounted for and was replaced. The
    containment helper asks the opposite question -- is this path inside that root -- and is
    right where it is used; it was the wrong question here.
    """
    if not target:
        return False
    try:
        wanted = Path(target).resolve()
    except (OSError, ValueError):
        return False
    for component in data["components"]:
        entry = ((record or {}).get("components", {}).get(component["component"]) or {})
        for install in entry.get("installs") or []:
            for key in ("environment", "location"):
                value = install.get(key)
                if not value:
                    continue
                try:
                    known = Path(value).resolve()
                except (OSError, ValueError):
                    continue
                if wanted == known:
                    return True
    return False


def _names_environment(record, environment, data):
    """Whether the record's selection lies inside this environment, read and not assumed.

    False for a record that names something else AND for one whose paths could not be resolved,
    because this answer authorises writing a pointer and an unread answer authorises nothing.
    """
    selected = (record or {}).get("selected") or {}
    named = [selected.get(c["component"]) for c in data["components"]]
    if not all(named):
        # An update moves a whole verified combination (OPS-2.4). A record naming one component
        # here and nothing for the other is not a promotion this run may finish: moving the
        # shared pointer on it would aim every command at a combination nobody selected.
        return False
    try:
        root = Path(environment).resolve()
        return all(within(Path(location).resolve(), root) for location in named)
    except (OSError, ValueError):
        return False


def _swap_gate(data, record, *, environment, python, socket_path=None, state=None):
    """The OPS-4.4 reading, taken against the runtime THIS record selects.

    Which relay owns the daemon and the store is decided by what is selected, so the record
    handed in decides which runtime is asked. Handed a reading taken before a long build, it
    answers about a runtime that may no longer be in use by the time anything moves, which is
    why its caller reads the record inside the promotion lock and passes that one.

    On a first install nothing is selected and the candidate answers for a host that has
    neither a daemon nor a store. Nothing here starts or stops a service (OPS-4.1).
    """
    outgoing = _selected_install(record, RELAY)
    executable = (outgoing or {}).get("entryPoint") or str(environment / "bin" / RELAY)
    interpreter = (outgoing or {}).get("interpreterPath") or str(python)
    return swapgate.decide({
        "daemon": swapgate.daemon_cell(scope.relay(
            ["service", "status"], executable=executable, socket=socket_path, state=state)),
        "inFlight": swapgate.inflight_cell(
            scope.relay(["doctor"], executable=executable, socket=socket_path, state=state),
            store_presence(interpreter, state, socket_path)),
        "storeSchema": swapgate.schema_cell(
            store_tables(interpreter, state, socket_path), candidate_tables(python)),
    })


def _selected_install(record, name):
    """The install record for the runtime currently selected for this component, or None.

    The selected runtime owns the daemon and the store, so it is the one the swap gate asks.
    On a first install nothing is selected and the answer is None, which is an answer.
    """
    location = ((record or {}).get("selected") or {}).get(name)
    if not location:
        return None
    for install in ((record or {}).get("components", {}).get(name) or {}).get("installs", []):
        if install.get("location") == location:
            return install
    return None


# What a rollback did with the host record's pointer ownership entry. Five answers, because
# "it was not taken away" had three entirely different reasons and one boolean answered all
# three with False: an entry this run INHERITED is kept deliberately, an entry another run has
# since moved on is not this one's to touch, and a write that failed is a rollback that did not
# finish. None is the sixth thing and is not an answer: nothing was attempted.
OWNERSHIP_DROPPED = "dropped"
OWNERSHIP_WITHDRAWN = "withdrawn"
OWNERSHIP_RESTORED = "restored"
OWNERSHIP_MOVED_ON = "moved on"
OWNERSHIP_UNREADABLE = "unreadable"
OWNERSHIP_ANSWERS = (OWNERSHIP_DROPPED, OWNERSHIP_WITHDRAWN, OWNERSHIP_RESTORED,
                     OWNERSHIP_MOVED_ON, OWNERSHIP_UNREADABLE)


def _ownership_answer(written, wanted, wrote):
    """What the host record says about pointer ownership after a rollback wrote to it.

    READ BACK from the record the single writer loaded, never inferred from the delta having
    been sent. Every rollback delta is compare-and-act: a record another run has since moved on
    is left exactly as it stands and the write still reports usable, so "the call returned" and
    "the entry is what this rollback meant to leave" are two facts, and only the second one is
    the answer this reports.

    It is the state the record was left IN, which is not the claim that this call wrote it: a
    compare that matched what was already there reports the same answer, and that is the honest
    one, because the question a reader has is what the NEXT run will read.

    'wanted' is the entry the rollback meant to leave, and None when it meant to leave nothing.
    'wrote' is the path THIS RUN recorded, and it is what separates the two ways the record can
    disagree with 'wanted'. An entry naming somewhere else belongs to another run. An entry
    still naming this run's path is this run's own, left because the delta did not land -- and
    calling that "moved on" would hand it to a run that never touched it.
    """
    if not written.usable:
        return OWNERSHIP_UNREADABLE, ("the ownership record could not be written: "
                                      + str(written.detail))
    after = (written.value or {}).get("pointer")
    if after == wanted:
        if wanted is None:
            return OWNERSHIP_DROPPED, ""
        return (OWNERSHIP_RESTORED if hostrecord.placement_recorded(after)
                else OWNERSHIP_WITHDRAWN), ""
    if (after or {}).get("path") != str(wrote):
        return OWNERSHIP_MOVED_ON, ("the ownership record names "
                                    + str((after or {}).get("path")) + " now, so the entry this"
                                    " run wrote is not its to put back")
    return OWNERSHIP_UNREADABLE, ("the ownership record still holds what this promotion wrote"
                                  " at " + str(wrote) + ", so the rollback's delta did not land")


def _restore_pointer(pointer_path, before, environment, record_path=None,
                     definition_version=None, ownership=None):
    """Put the pointer back the way this run found it, INCLUDING finding it absent.

    The rollback could only restore a previous target, which has no answer for a first or legacy
    install where there was no pointer at all. There, place() creates one, and a failed read-back
    left it naming a candidate the selection had just been taken away from -- and _install_failed
    then kept that candidate precisely BECAUSE the pointer named it, so the staging could never
    be reclaimed. That is the permanent refusal this command exists to remove, arriving from the
    other side, on the very path that was just made to work.

    'Restore to absence' was the value missing from this answer set, the same shape as the
    established-absent answer the in-flight cell was missing.

    'ownership' is the RECORD side of the same question, and it is the second thing this had to
    be handed. A link state of NO_POINTER says the LINK was not there; it says nothing about the
    RECORD, and a host whose recorded link was deleted out from under it has the entry and no
    link. Taking that entry away is not a rollback: it removes the path the Codex registration
    names, and a retry with a different --dest then derives another path and reads a
    registration nobody changed as a conflict. So only an entry this run INTRODUCED goes away
    with the link, and only for the path this run recorded.

    An entry this run INHERITED goes back, and what goes back depends on what the link ended up
    as, because the entry answers two questions (hostrecord.POINTER_PLACEMENT). Where the link
    was put back, the whole entry goes back -- which also takes this run's refreshed stamp off
    an entry it did not introduce. Where the link is established ABSENT, the path goes back and
    the placement evidence is WITHDRAWN: the registration still needs the path, and a link that
    turns up there afterwards is still one this command never recorded placing, which is what
    the promotion refuses. Put back whole it would authorise replacing that link, which is the
    protection the absence rollback was written to keep.

    A restoration that cannot be read back is reported as residual rather than claimed: the
    caller then keeps the candidate, which is the safe direction when the disk and the record
    may disagree.

    That applies to the RECORD half too. The link going back and the record not going with it
    is half a rollback, not a completed one: the record still says this command placed a link
    at a path where there is now none, and that claim is exactly what the next promotion reads
    before it replaces whatever has turned up there. So the restoration is not claimed as
    verified and the path whose claim somebody has to settle is named.
    """
    restored_to, verified, residual = None, True, None
    detail = "this run placed no pointer, so there is nothing to put back"
    # What the record should hold once this is over, and whether it may be written yet. 'wanted'
    # of None means nothing should be there, which is the answer only for an entry this run
    # introduced.
    wanted, settled = None, False

    if before["state"] == pointer.NO_POINTER:
        removed, detail = pointer.remove(pointer_path, environment)
        restored_to = "absent" if removed else None
        verified = removed
        residual = None if removed else str(pointer_path)
        # Only after the link is verifiably gone. Writing the record first would leave a link
        # nobody recorded, which is the refusal shape from the opposite side.
        settled = removed
        wanted = hostrecord.without_placement(ownership) if ownership else None
    elif before["state"] == pointer.LINK and before.get("target"):
        try:
            pointer.place(pointer_path, before["target"])
        except OSError as error:
            verified, residual = False, str(pointer_path)
            detail = ("the previous target could not be put back: " + type(error).__name__
                      + ": " + str(error))
        else:
            verified = pointer.names(pointer_path, before["target"]) is True
            restored_to = str(before["target"]) if verified else None
            residual = None if verified else str(pointer_path)
            detail = ("the previous target was put back and read back" if verified
                      else "the previous target could not be read back after restoring it")
        # A link was here and a link is here, so nothing disproved the placement. The entry
        # goes back exactly as found, and an entry this run introduced over no entry at all --
        # a legacy adoption that failed -- goes away, which is the same "as found".
        settled = True
        wanted = dict(ownership) if ownership else None

    owned = None
    residual_claim = None
    settle_claim = None
    if settled and record_path is not None:
        try:
            if wanted is None:
                written = hostrecord.update(record_path, definition_version,
                                            drop_pointer=str(pointer_path))
            else:
                written = hostrecord.update(record_path, definition_version,
                                            restore_pointer={"wrote": str(pointer_path),
                                                             "found": wanted})
        except OSError as error:
            # Reported, not raised. This bookkeeping is the SMALLER of the two rollbacks a
            # failed promotion needs and it happens first, so letting it out costs the larger
            # one: the caller never reaches the selection rollback, the pointer is back on the
            # predecessor while the record still selects the candidate, and _install_failed
            # keeps that candidate and reports a defect in this command instead of the failure
            # that actually happened. The exception type travels in the detail rather than
            # being read as anything -- a lock this run could not take and a disk that refused
            # the write are the same answer here, which is that the record does not say what
            # this rollback meant it to.
            #
            # It is also not evidence that the write did not HAPPEN. The save lands inside the
            # lock and releasing that lock can raise afterwards, so a run can have committed
            # the delta and still come out here. Answering "unreadable" from the exception
            # alone invented an outstanding claim for a record that was already correct, so the
            # record is READ BACK and the answer comes from what it says; only a read-back that
            # also fails leaves the outcome unknown.
            detail = (detail + ", but writing the ownership record raised: "
                      + type(error).__name__ + ": " + str(error))
            after = hostrecord.load(record_path, definition_version)
            if not after.usable:
                owned = OWNERSHIP_UNREADABLE
                detail = (detail + ", and the record could not be read back to establish"
                          " whether it landed: " + str(after.detail))
            else:
                owned, note = _ownership_answer(after, wanted, pointer_path)
                detail = detail + (", but " + note if note
                                   else ", and the record reads back as " + str(owned))
        else:
            owned, note = _ownership_answer(written, wanted, pointer_path)
            if note:
                detail = detail + ", but " + note
        if owned in (OWNERSHIP_UNREADABLE, OWNERSHIP_MOVED_ON):
            # Reporting this as a completed rollback is the one outcome that would let a
            # re-armed guard pass unseen: the candidate is released, the result says retriable
            # and clean, and the record goes on claiming a placement for a link that is gone.
            # 'restoredTo' still says what the LINK was put back to, because that part is true;
            # 'verified' is about the restoration as a whole, and this one did not finish.
            verified = False
        if owned == OWNERSHIP_UNREADABLE:
            # An ownership claim THIS RUN left outstanding, which is the only thing this cell
            # means. The write did not happen, so the record still holds what this promotion put
            # there and somebody has to settle it.
            #
            # Two states reach here and they need different things said, because the sentence
            # is written where the readings that decide it were made. Composed by the caller it
            # could name only one, and telling an operator the link was taken away when it is
            # back sends them looking for something that did not happen.
            residual_claim = str(pointer_path)
            # Composed from the two readings rather than from one of them. What happened to the
            # LINK and where the ENTRY came from are separate facts, and a sentence that assumes
            # either -- that the link went back when the restoration failed, or that the entry
            # was inherited when this run introduced it over a legacy install -- tells an
            # operator something that did not happen.
            link_says = (
                "the link this run placed was taken away" if restored_to == "absent"
                else "the link there was put back to " + str(restored_to) if restored_to
                else "the link could not be put back either, so what is at that path is this"
                     " run's")
            record_says = (
                "the record holds an entry this run introduced" if not ownership
                else "the record still carries this run's stamp on an entry it did not"
                     " introduce")
            # The consequence is a THIRD reading, not a restatement of the first. Where nothing
            # went back, the link there and the record that claims it are both this run's, so
            # they do not disagree -- saying they do would name a conflict that is not the
            # problem. What is wrong there is that a failed promotion's link and entry are the
            # ones a host now reaches through.
            reaches = (
                "so the next update would read a link that appears at that path as its own"
                if restored_to == "absent"
                else "so the record and the link disagree about who placed it" if restored_to
                else "so a host reaches through the link this failed run left, and the record"
                     " agrees with it")
            settle_claim = ("settle the host record's pointer ownership for "
                            + str(pointer_path) + ": " + link_says + " and " + record_says
                            + ", " + reaches)
        elif owned == OWNERSHIP_MOVED_ON:
            # NOT a residual of this run's. Another writer owns the entry now, and the reading
            # that established that also established there is nothing here for this run to put
            # back -- so asking an operator to settle a claim at this path would send them after
            # somebody else's record. The restoration is still unverified, because it did not do
            # what it set out to, and the concurrent move is reported through the ownership cell
            # and the detail, which is what actually happened.
            pass
    return {"restoredTo": restored_to, "verified": verified, "residualPointer": residual,
            "residualOwnership": residual_claim, "settleOwnership": settle_claim,
            "ownership": owned, "detail": detail}


def _outstanding_ownership(pointer_restored):
    """The claim a rollback left behind, as the two fields a RESULT reports it with.

    One place, because two commands end on this and a reader has to find the same answer in
    both. _install_failed is the update's exit and _finish_promotion is the resume's, and the
    resume's never goes through the update's -- so a receipt following the documented procedure
    read nulls there for a claim that was outstanding, while the same failure one path over
    reported it. Written twice they would drift; asked of one helper they cannot.

    The sentence is carried rather than composed. Only the restoration knows which of its states
    this was, and a sentence written at an exit could name just one of them.
    """
    return {"residualOwnership": (pointer_restored or {}).get("residualOwnership"),
            "recoveryRequires": (pointer_restored or {}).get("settleOwnership")}


def _restore_selection(record_path, definition_version, previous, installs):
    """Put back the selection this run just moved, for the components it moved.

    Narrow on purpose. Re-asserting a whole selection map would carry back entries read before
    the slow work and re-assert them as current, which is the staleness the single-writer helper
    exists to prevent. This re-asserts only the components this run changed.

    Two answers, because putting a selection back has two shapes and this had only one. Where
    there was a previous value it goes back. Where there was NONE -- a first install, and a
    legacy install whose combination was never selected before -- the entry this run wrote is
    taken away, which is the answer the delta set could not express: the run reported a
    rollback, the pointer correctly went back to absence, and the candidate stayed selected.
    Being selected is then what keeps the candidate from being released, so the destination
    could never be retried. The permanent refusal again, and again from the answer set rather
    than from the check.
    """
    missing = sorted(name for name in installs if not previous.get(name))
    # The caller holds the promotion lock; this does not take it again. 'previous' is the
    # baseline that caller read INSIDE that lock, so what goes back is the selection this
    # promotion actually replaced rather than whatever was there before the build began.
    #
    # Only entries that still name what THIS run wrote are put back. A blind restore would undo
    # a promotion another run committed in the meantime, and rolling back on top of somebody
    # else's success is a worse outcome than the failure being rolled back.
    current = hostrecord.load(record_path, definition_version)
    if not current.usable:
        return {"selection": None, "restored": [], "withoutPrevious": missing,
                "detail": "the host record could not be read, so the previous selection"
                          " could not be put back: " + str(current.detail)}
    selected = (current.value.get("selected") or {})
    back, gone, moved_on = {}, {}, []
    for name, install in installs.items():
        if selected.get(name) != install["location"]:
            moved_on.append(name)
            continue
        if previous.get(name):
            back[name] = previous[name]
        else:
            gone[name] = install["location"]
    if not back and not gone:
        return {"selection": None, "restored": [], "withoutPrevious": missing,
                "movedOnByAnotherRun": sorted(moved_on),
                "detail": "there was nothing of this run's left to put back: either nothing"
                          " this run wrote is still selected, or another run has since moved"
                          " the selection on"}
    written = hostrecord.update(record_path, definition_version, select=back, deselect=gone)
    return {"selection": back if written.usable else None,
            "restored": sorted(back) if written.usable else [],
            "removed": sorted(gone) if written.usable else [],
            "withoutPrevious": missing, "movedOnByAnotherRun": sorted(moved_on),
            "detail": ("the selection this run moved was put back to what it was, including"
                       " back to nothing where nothing was selected before it" if written.usable
                       else "the selection could not be put back: " + str(written.detail))}


def _selected_digest(location):
    """The bytes of a selected runtime, read at the boundary.

    An unreadable file under a selected installation is a reading that failed, not a crash in
    the middle of deciding what to stage over.
    """
    with reading.region(location, "the bytes of the selected runtime"):
        return definition.ops12_digest(location)


def _outgoing_runtime(record, data):
    """What is selected right now, and whether its bytes are still what was recorded."""
    observed = {}
    selected = record.get("selected") or {}
    for component in data["components"]:
        location = selected.get(component["component"])
        if not location:
            observed[component["component"]] = {"selected": None}
            continue
        present = Path(location).is_dir()
        observed[component["component"]] = {
            "selected": location,
            "present": present,
            "digest": _selected_digest(location) if present else None,
        }
    return observed


def _install_failed(record_path, definition_version, performed, environment, owned=None,
                    failed_reading=None, failed_error=None, pointer_path=None,
                    failed_step=None, restored=None, gate=None, pointer_restored=None):
    """Release a destination this run created, and report whether it is retriable.

    Two outcomes, because saying "refused" does not delete a directory. When removal is
    VERIFIED the destination can be retried and the result says so. When removal could not
    finish the result says NOT retriable, names what is left and what recovery needs, and the
    original failure is reported alongside rather than replaced by the cleanup failure.

    Whether the candidate may be removed at all is read from the record, never remembered:
    release_candidate looks at the selection under the lock, because a run can commit its
    promotion and still raise while releasing the lock. An environment that is selected, or a
    record that cannot be read, keeps its candidate.

    The store is never removed, moved or recreated: update failure and store loss are
    different accidents.
    """
    # The second truth about whether this environment is in use. A caller with no pointer in
    # scope passes none, and that is False rather than None: there being no pointer to consult
    # is an established answer, while a pointer that could not be read is not.
    reached = False if pointer_path is None else pointer.names(pointer_path, environment)
    dropped, decision = hostrecord.release_candidate(
        record_path, definition_version, environment, pointer_names=reached)
    keeping = not text_prefix(decision, "dropped")

    removed, residue, cleanup_error = False, None, None
    if owned is not None and not keeping:
        try:
            shutil.rmtree(str(owned))
        except OSError as error:
            cleanup_error = type(error).__name__ + ": " + str(error)
        # Verified on the filesystem rather than inferred from the call returning.
        removed = not Path(owned).exists()
        if not removed:
            residue = str(owned)

    # Only verified removal makes the destination reusable. Reporting a kept candidate as
    # retriable was false in the way that matters: the deterministic directory is still there
    # and the next install refuses at the existence check.
    retriable = owned is None or removed
    # A pointer restoration that did not finish leaves a CLAIM rather than a path. The record
    # goes on saying this command placed a link where there is now none, and that claim is what
    # the next promotion reads before replacing whatever has turned up at that path -- so it is
    # an outstanding thing somebody has to settle, and a result that does not say so is how it
    # goes unseen.
    #
    # It is not folded into 'residualPaths' and it does not move 'retriable'. Nothing is on
    # disk, and 'retriable' answers whether this DESTINATION can be used again, which the
    # deterministic directory decides and a record claim does not. Answering either of those
    # with this would be a cell carrying a reading its own question did not produce, which is
    # the shape the rest of this change exists to remove.
    outstanding = _outstanding_ownership(pointer_restored)
    unsettled = outstanding["residualOwnership"]
    settle = outstanding["recoveryRequires"]
    emit({
        "command": "install", "applied": False, "steps": performed,
        "environment": str(environment),
        "selected": (dropped.value or {}).get("selected") if dropped.usable else None,
        "hostRecordState": dropped.state,
        "candidate": decision,
        "removedCandidate": str(owned) if removed else None,
        "cleanupError": cleanup_error,
        "retriable": retriable,
        # Which step failed, not merely that one did. The steps above say what ran; this names
        # the boundary the run stopped at, so a reader does not have to infer it from the tail.
        "failedStep": failed_step or next(
            (step.get("step") for step in reversed(performed) if step.get("ok") is False), None),
        "pointer": None if pointer_path is None else {
            "path": str(pointer_path), "namesThisEnvironment": reached,
            "restored": restored, "pointerRestored": pointer_restored,
            "meaning": ("the pointer was not moved by this run unless 'restored' says so."
                        " Whatever a host reached before this run, it still reaches"),
        },
        "swapGate": gate,
        "residualPaths": ([str(owned)] if (owned is not None and not removed) else [])
                         + ([(pointer_restored or {}).get("residualPointer")]
                            if (pointer_restored or {}).get("residualPointer") else []),
        # What this run left behind that is not a path. Reported beside residualPaths rather
        # than inside it, because a reader looking for a directory to delete and a reader
        # looking for a record claim to settle are answering different questions.
        "residualOwnership": unsettled,
        "recoveryRequires": "; and ".join(part for part in (
            None if retriable else (
                ("this environment is selected, so it was kept deliberately and the destination"
                 " cannot be retried until the selection moves") if keeping
                else ("remove " + str(owned) + " by hand; this run created it and could not"
                      " remove it, so the same destination will keep refusing until it is"
                      " gone")),
            settle,
        ) if part) or None,
        "refused": (
            failed_reading.detail if failed_reading is not None else
            ("this command failed in a way it does not model: " + type(failed_error).__name__
             + ": " + str(failed_error)[:300]) if failed_error is not None else
            "a step failed; whatever runtime was selected remains selected"),
        "reading": None if failed_reading is None else failed_reading.refusal(),
        "internalError": None if failed_error is None else {
            "exception": type(failed_error).__name__,
            "raisedAt": reading.where(failed_error),
        },
        "note": (
            "the selection is left as found, because another run's promotion is not this"
            " run's to undo, and a candidate that is selected or whose record cannot be read"
            " is kept rather than deleted. The store is untouched."
        ),
    })
    return EXIT_REFUSED


def _find_interpreter(data):
    minimum = (3, 11)
    for candidate in ("python3.13", "python3.12", "python3.11", "python3"):
        found = shutil.which(candidate)
        if not found:
            continue
        version = interpreter_version(found)
        if not version:
            continue
        parts = tuple(int(p) for p in version.split(".")[:2])
        if parts >= minimum:
            return found
    return None


# ------------------------------------------------------------------------- measure

def _interpreter_prefix(python):
    """The environment an interpreter reports for itself."""
    try:
        done = subprocess.run([str(python), "-c", "import sys;print(sys.prefix)"],
                              capture_output=True, text=True, timeout=60)
    except (OSError, subprocess.SubprocessError):
        return None
    return done.stdout.strip() or None if done.returncode == 0 else None


def _bind_installs(record, data, python, environment):
    """Match each module's imported location to the install recorded for this environment.

    Returns (bound, refusal). 'bound' maps a component name to its recorded install and the
    digest of the bytes as they are right now, which is what the point will record.
    """
    bound = {}
    for component in data["components"]:
        entry = (record.get("components", {}).get(component["component"]) or {})
        installs = [i for i in entry.get("installs", [])
                    if i.get("environment") == str(environment)]
        if not installs:
            return None, ("no install of " + component["component"] + " is recorded for "
                          + str(environment) + ", so a run there could not be attributed")
        install = installs[0]
        location, error, _argv = module_location(python, component["module"])
        if not location:
            return None, (component["component"] + " could not be imported by " + str(python)
                          + ": " + str(error))
        if Path(location).resolve() != Path(install["location"]).resolve():
            return None, (component["component"] + " imported from " + location
                          + " but the recorded install for this environment is "
                          + install["location"])
        with reading.region(location, "the bytes of " + component["component"]
                            + " about to be exercised"):
            digest = definition.ops12_digest(location)
        bound[component["component"]] = {"install": install, "digest": digest,
                                         "location": location}
    return bound, None


def measure_candidate(data, record, *, python, environment, socket_path, state,
                      relay_command, measured_by=None):
    """Exercise both components and record a point only if both actually ran.

    A point means the combination was exercised (OPS-1.3). Starting a process is not that:
    the bridge's entry point starts a stdio server and never contacts the App Server, so a
    startup-based recipe would record success against an unreachable host. The relay is
    exercised by a doctor whose socketConnect is a real connect, and the bridge by its own
    read-only smoke check, which starts the MCP server, lists its tools and calls
    get_capabilities. Any connection, protocol or tool-call failure records no point.
    """
    bridge = component_of(data, BRIDGE)
    relay_component = component_of(data, RELAY)
    operations = []

    # Bind the run to the environment before anything is exercised or recorded. Two checks,
    # because neither alone is enough. The interpreter reports its own prefix, which is the
    # only thing that identifies which environment is running: a virtual environment's
    # bin/python legitimately resolves to an interpreter outside it, so a path test would
    # reject valid environments. And each module's imported location must equal the location
    # recorded for that environment's install, which is what accommodates an editable
    # install whose location sits outside its environment by design (OPS-1.1). Without the
    # first, two environments sharing one editable source are indistinguishable.
    prefix = _interpreter_prefix(python)
    if prefix is None:
        return {"operations": [], "qualifyingPoint": False, "appServer": None, "toolsListed": [],
                "refused": "the interpreter did not report its prefix, so the environment it"
                           " runs cannot be identified"}
    if Path(prefix).resolve() != Path(environment).resolve():
        return {"operations": [], "qualifyingPoint": False, "appServer": None, "toolsListed": [],
                "refused": "the interpreter reports prefix " + str(prefix) + " but the selected"
                           " environment is " + str(environment) + ", so this run would exercise"
                           " one runtime and record the point against another"}

    bound, mismatch = _bind_installs(record, data, python, environment)
    if mismatch:
        return {"operations": [], "qualifyingPoint": False, "appServer": None, "toolsListed": [],
                "refused": mismatch}

    # The executable is bound the way the modules are. Without this, a doctor from whatever
    # PATH or --relay-command supplied could record an exercised point against THIS
    # environment, and the point would name a run that never happened.
    recorded_entry = bound[relay_component["component"]]["install"].get("entryPoint")
    if recorded_entry and not within(Path(relay_command).resolve(),
                                    Path(recorded_entry).resolve().parent):
        return {"operations": [], "qualifyingPoint": False, "points": [], "appServer": None,
                "toolsListed": [],
                "refused": "the relay to exercise is " + str(relay_command) + " but the install"
                           " recorded for " + str(environment) + " is " + str(recorded_entry)
                           + ", so a successful doctor would describe a different runtime"}

    doctor = scope.relay(["doctor"], executable=recorded_entry or relay_command,
                         socket=socket_path, state=state)
    connect = ((doctor.get("payload") or {}).get("actorReachability") or {}).get("socketConnect")
    operations.append({
        "component": relay_component["component"], "command": doctor.get("command"),
        "exercised": connect == "ok",
        "detail": "actorReachability.socketConnect = " + repr(connect)
                  + "; a real connect is what makes this an exercise rather than a file read",
    })

    script = ROOT / bridge["exerciseScript"]
    argv = [str(python), str(script)]
    if socket_path:
        argv += ["--socket", str(socket_path)]
    tools_listed, app_server = [], None
    try:
        done = subprocess.run(argv, capture_output=True, text=True, timeout=180)
        payload = json.loads(done.stdout) if done.stdout.strip() else {}
        tools_listed = payload.get("tools") or []
        app_server = json.dumps(payload.get("connection")) if payload.get("connection") else None
        exercised = done.returncode == 0 and bridge["identityTool"] in tools_listed
        detail = ("listed " + str(len(tools_listed)) + " tools and called "
                  + bridge["identityTool"]) if exercised else (
            (done.stderr or done.stdout).strip()[-500:] or "the smoke check did not succeed")
    except (OSError, subprocess.SubprocessError, ValueError) as error:
        exercised, detail = False, type(error).__name__ + ": " + error.__str__()
    operations.append({
        "component": bridge["component"], "command": argv, "exercised": exercised,
        "toolsListed": tools_listed, "detail": detail,
    })

    qualifying = all(op["exercised"] for op in operations)

    # Every dimension a point is compared on, observed ONCE for this measurement and reused for
    # both the gate below and the point written after it. Four are shared by the whole run and
    # two are per-component by nature, already observed when the installs were bound. Observed
    # twice, a transient failure between the gate and the write puts a null dimension into a
    # point the consumer can never match, and install then promotes on evidence the next
    # diagnosis rejects.
    shared = {"interpreter": interpreter_version(python), "codexCli": codex_cli_version(),
              "host": socket.gethostname(), "appServer": app_server}
    measured = {
        name: dict(shared, install=bound[name]["install"].get("location"),
                   installDigest=bound[name]["digest"],
                   # The instrument this component's claim rests on, read by the same helper
                   # classification reads it with.
                   exerciseDigest=instrument_digest(component_of(data, name)))
        for name in bound
    }

    # A point this run records must be a point this run would later ACCEPT. Recording one the
    # consumer can never match is worse than recording none: install promotes on it and the
    # next diagnosis rejects the very evidence that authorized the promotion. The condition is
    # read off the declared map rather than written out, because checking one dimension by name
    # left an unread interpreter to reach a qualifying point as a null mandatory value, and an
    # unread App Server to reach one that classification refuses to proceed on at all.
    refused_reason = None
    if qualifying:
        unobserved = sorted({field for facts in measured.values()
                             for field in hostrecord.DIMENSIONS if facts.get(field) is None})
        if unobserved:
            refused_reason = ("these dimensions could not be observed: " + ", ".join(unobserved)
                              + ", so any point recorded here would carry a null dimension that"
                              " can never match")
        else:
            mismatched = [name for name in bound
                          if measured[name]["installDigest"]
                          != component_of(data, name)["sourceDigest"]]
            if mismatched:
                refused_reason = ("the installed bytes of " + ", ".join(sorted(mismatched))
                                  + " disagree with the definition, so classification would"
                                  " call them a fork and no point measured against them can"
                                  " authorize reuse")
    if refused_reason:
        return {"operations": operations, "qualifyingPoint": False, "points": [],
                "appServer": app_server, "toolsListed": tools_listed,
                "refused": refused_reason}
    # Points are RETURNED, not written into the record this function was handed. Measuring
    # takes minutes, and a record mutated here and saved by the caller would carry back a
    # value read before all of it, silently dropping whatever another run appended in
    # between. The caller applies these as a delta against the record as it then stands.
    points = []
    if qualifying:
        for component in data["components"]:
            facts = measured[component["component"]]
            point = {field: facts[field] for field in hostrecord.DIMENSIONS}
            point.update({
                "date": now(),
                "measuredBy": measured_by or "JUN-104",
                "method": "; ".join(
                    " ".join(str(part) for part in (op.get("command") or [])) for op in operations
                ),
                "exercised": True,
                # installDigest is the digest of what was exercised, measured now, not the one
                # the definition expects. A point has to describe the bytes that ran.
                "definitionDigest": component["sourceDigest"],
                "digestMatchesDefinition":
                    facts["installDigest"] == component["sourceDigest"],
            })
            points.append((component["component"], point))
    return {"operations": operations, "qualifyingPoint": qualifying, "points": points,
            "appServer": app_server, "toolsListed": tools_listed}


def cmd_measure(args):
    try:
        with reading.region(definition.DEFINITION_PATH, "the component definition"):
            data = definition.load()
    except reading.Refused as stop:
        return refused("measure", stop.reading)
    record_path = Path(args.record) if args.record else hostrecord.record_path()
    host_record = hostrecord.load(record_path, data["definitionVersion"])
    if not host_record.usable:
        return refused("measure", host_record, hostRecord=str(record_path))
    record = host_record.value

    relay_component = component_of(data, RELAY)
    relay_command = args.relay_command or shutil.which(relay_component["consoleScript"])
    python = args.python or sys.executable
    # Derived from what the interpreter reports, not from its executable's parent: a virtual
    # environment's bin/python commonly resolves into the base installation, and that path
    # names the wrong environment.
    environment = args.environment or _interpreter_prefix(python)
    if not environment:
        emit({"command": "measure", "refused": "the interpreter did not report a prefix, so no"
              " environment could be selected; pass --environment"})
        return EXIT_REFUSED

    if not relay_command:
        emit({"command": "measure", "refused": "no relay executable was found"})
        return EXIT_REFUSED

    measurement = measure_candidate(data, record, python=python, environment=environment,
                                    socket_path=args.socket, state=args.state,
                                    relay_command=relay_command, measured_by=args.issue)
    if measurement.get("points"):
        appended = hostrecord.update(record_path, data["definitionVersion"],
                                     points=measurement["points"])
        if not appended.usable:
            return refused("measure", appended, hostRecord=str(record_path))
    emit({
        "command": "measure", "hostRecord": str(record_path),
        "recorded": bool(measurement["qualifyingPoint"]),
        **measurement,
        "note": (
            "A point requires the combination to be EXERCISED (OPS-1.3). A connection,"
            " protocol or tool-call failure records no point, and reading bytes never"
            " produces one."
        ),
    })
    return EXIT_OK if measurement["qualifyingPoint"] else EXIT_REFUSED


# ------------------------------------------------------------------------- register-mcp

def _starts_this_bridge(command, executable):
    """Whether a registration starts the bridge, whatever table name it was given.

    --name is the operator's to choose, so a registration made before the ownership record
    existed can sit under any name at all and carries no owner anywhere. Its table name
    therefore answers nothing, and the command it starts answers everything: the exact path
    this run was handed, or any path whose final component is the bridge's own console script.
    """
    if not isinstance(command, str) or not command.strip():
        return False
    if executable and command == str(executable):
        return True
    return Path(command).name == BRIDGE


def _other_bridge_tables(configuration, name, wanted):
    """Tables that start this bridge under some name other than the one being registered.

    None means the configuration could not be read, which is not an empty answer: it is the
    question going unanswered, and both owners refuse on it rather than defaulting.

    name is the table this run is registering, or None when it registers none. It is excluded
    because a legacy registration acquiring its ownership record is the supported migration:
    the entry is already there and this run adds no second one. Every other bridge table would
    be joined rather than replaced.
    """
    try:
        view = codexconfig.scan(configuration)
    except reading.Refused:
        return None
    if not view.readable:
        return None
    return sorted(table for table, entry in view.servers.items()
                  if table != name and _starts_this_bridge(entry.get("command"),
                                                           (wanted or {}).get("bridgeExecutable")))


def _plugin_declared_servers(codex_home):
    """The servers an installed plugin package declares here, or None when that is unreadable.

    None is not "none declared". It is the question going unanswered, and this refuses on it the
    way every other reading in this command does, because registering on an unanswered question is
    how a second bridge arrives.

    An absent cache directory is a real answer: nothing is installed, so nothing is declared, and
    the ordinary manual install this command exists for goes on working untouched.
    """
    cache = Path(codex_home) / "plugins" / "cache"
    if not cache.is_dir():
        return set()
    declared, unreadable = set(), False

    def _children(directory):
        # os.scandir rather than glob or iterdir. Those answer a directory they cannot read by
        # leaving it out, and a package silently skipped here would read as a host where nothing
        # declares this server -- the one answer that must never be guessed, because guessing it
        # is what lets the shadowing table land. scandir raises, and the caller turns that into
        # "not established" rather than into "none".
        with os.scandir(directory) as entries:
            return [Path(entry.path) for entry in entries if entry.is_dir()]

    try:
        versions = sorted(version
                          for marketplace in _children(cache)
                          for package in _children(marketplace)
                          for version in _children(package))
    except OSError:
        return None
    for version in versions:
        manifest_path = version / ".codex-plugin" / "plugin.json"
        if not manifest_path.is_file():
            continue
        try:
            manifest = json.loads(manifest_path.read_text(encoding="utf-8"))
            if not isinstance(manifest, dict):
                # Valid JSON is not a manifest. A root that is a list answers .get with an
                # AttributeError, which the handler below does not catch, so register-mcp
                # answered internal_error instead of the ownership refusal it promises. A
                # package this cannot read is unreadable, which refuses, not absent.
                unreadable = True
                continue
            named = manifest.get("mcpServers")
            if not (isinstance(named, str) and named.strip()):
                continue
            relative = named[2:] if text_prefix(named, "./", at="start") else named
            document = json.loads((version / relative).read_text(encoding="utf-8"))
            if not isinstance(document, dict):
                unreadable = True
                continue
            servers = document.get("mcpServers")
            if not isinstance(servers, dict):
                unreadable = True
                continue
            declared.update(str(server) for server in servers)
        except (OSError, ValueError):
            unreadable = True
    return None if unreadable else declared


def _mcp_ownership(record_path, owner, configuration, name, wanted, codex_home=None):
    """Why this owner may not register the bridge, given what this host already holds.

    Two registrations of one server is the failure this prevents, and it is prevented in both
    directions because either can be installed first. The configuration entry is refused by a
    record naming the plugin; the record is refused by an entry already in the configuration.

    Each artifact is read as itself. A record or a configuration that could not be read refuses
    rather than defaulting, because installing on an unanswered question is how a second bridge
    arrives.

    wanted is the record this run would write. It is decided here, before the configuration is
    touched, because deciding it afterwards is how the registration lands and the record does
    not: the file then holds a second [mcp_servers] table while the record still names the
    first, and the host starts two bridges out of a run that reported a refusal.
    """
    if owner not in bridgerecord.OWNERS:
        return "owner must be one of " + ", ".join(bridgerecord.OWNERS) + ", found " + repr(owner)
    if owner == bridgerecord.OWNER_PLUGIN and name != MCP_NAME:
        # The package declares one server name. Checking a different one would inspect an entry
        # that is not the server that will start, and record a name the launcher never reads.
        return ("the " + bridgerecord.OWNER_PLUGIN + " owner registers the server the package"
                " declares, which is " + repr(MCP_NAME) + ", not " + repr(name)
                + "; --name belongs to a " + bridgerecord.OWNER_USER + "-owned registration")
    found, outcome, detail = bridgerecord.read(record_path)
    if found is None and outcome != bridgerecord.ABSENT:
        # Every way of not reading it, not only the malformed one. read() reports an undecodable
        # file, a dangling link, a directory and a permission failure as their own states, and
        # treating those like absence lets the other owner install on an unanswered question.
        return ("the record at " + str(record_path) + " could not be acted on (" + str(detail)
                + "), so who owns this server was not established")
    if owner == bridgerecord.OWNER_USER:
        if found is not None and bridgerecord.owner_of(found) == bridgerecord.OWNER_PLUGIN:
            return ("the record at " + str(record_path) + " names the "
                    + bridgerecord.OWNER_PLUGIN + " as the owner of this server, so the plugin"
                    " package already declares it; a configuration entry beside it would run a"
                    " second bridge. Register with --owner " + bridgerecord.OWNER_PLUGIN
                    + ", or remove that record first")
        # The gap the ownership record alone does not close. That record is what says the plugin
        # owns this server, and remove --apply retires it while deliberately leaving the plugin
        # cache and its config entry alone, because codex plugin remove owns those. A run after
        # that finds no record and a package that still declares the server, and the table this
        # would append WINS over that declaration -- measured -- so it shadows a gated server
        # with an ungated one and nothing reports it.
        #
        # Refused, not carried. Copying the package's approval fields into a user table would
        # duplicate a declaration the package owns and leave two writers for one policy; refusing
        # keeps one writer, which is what the rest of this function is for.
        if found is None or bridgerecord.owner_of(found) != bridgerecord.OWNER_USER:
            declared = _plugin_declared_servers(codex_home) if codex_home else set()
            if declared is None:
                return ("an installed plugin package under "
                        + str(Path(codex_home) / "plugins" / "cache") + " could not be read, so"
                        " whether a package already declares " + repr(name) + " was not"
                        " established. A table appended beside a declaration wins over it, so"
                        " this is refused rather than decided on an unanswered question")
            if name in declared:
                return ("an installed plugin package already declares " + repr(name) + ", and a "
                        + bridgerecord.OWNER_USER + "-owned table wins over a plugin declaration,"
                        " so registering one now would shadow the declared server -- including"
                        " any per-tool approval policy the package declares for it -- and nothing"
                        " would report it. Register with --owner " + bridgerecord.OWNER_PLUGIN
                        + ", or remove that package first")
        if found is not None and not bridgerecord.same_registration(found, wanted):
            # Compared against the whole document, the same comparison the write makes, so this
            # check and that write cannot disagree about what counts as the same record. A
            # differing serverName is the case that hurts most -- the registration would append
            # a table under one name while the record kept naming another -- but a differing
            # command or argument list leaves the same split between the two artifacts.
            differing = sorted(field for field in bridgerecord.IDENTITY
                               if found.get(field) != wanted.get(field))
            return ("the record at " + str(record_path) + " is already installed and says"
                    " something else (" + ", ".join(differing) + "); this command does not"
                    " overwrite it. Registering now would append a second table to the Codex"
                    " configuration while the record went on naming the first, and the host"
                    " would start two bridges. Repair or remove that record first")
        # A host that registered before the record existed has the configuration as its only
        # evidence, and this path is the migration route: the requested table may already be
        # there and acquire its record, but a bridge sitting under any OTHER name would be
        # joined by a second table rather than replaced by one.
        legacy = _other_bridge_tables(configuration, name, wanted)
        if legacy is None:
            return ("the Codex configuration could not be read, so whether this bridge is"
                    " already registered under another name was not established")
        if legacy:
            return ("the Codex configuration already starts this bridge as "
                    + ", ".join(repr(table) for table in legacy) + ", under a name this run is"
                    " not registering; adding " + repr(name) + " beside it would leave two"
                    " tables starting the same bridge. Register under that name to give it an"
                    " ownership record, or remove the entry first")
        return None
    try:
        with reading.region(record_path, "the Codex configuration"):
            view = codexconfig.scan(configuration)
    except reading.Refused as stop:
        return ("the Codex configuration could not be scanned (" + str(stop.reading.detail)
                + "), so whether this server is already registered was not established")
    if not view.readable:
        return ("the Codex configuration could not be read, so whether this server is already"
                " registered was not established")
    aliased = _other_bridge_tables(configuration, None, wanted)
    if aliased is None:
        return ("the Codex configuration could not be read, so whether this server is already"
                " registered was not established")
    if aliased:
        # Found by what it starts rather than by what it is called. A host that registered
        # this bridge before the ownership record existed has the configuration as its only
        # evidence, and checking one name would look straight past a registration sitting
        # under any other.
        return ("the Codex configuration already starts this bridge as "
                + ", ".join(repr(table) for table in aliased) + ", which is a "
                + bridgerecord.OWNER_USER + "-owned registration carrying no ownership record;"
                " a plugin declaration beside it would run a second bridge. It is recognised by"
                " the command it starts, because --name is free to choose the table it sits"
                " under. Remove that entry, or migrate it with --owner " + bridgerecord.OWNER_USER
                + " first")
    if name in view.servers:
        return ("the Codex configuration already registers " + repr(name) + ", which is the "
                + bridgerecord.OWNER_USER + "-owned registration; a plugin declaration beside"
                " it would run a second bridge. Remove that entry first")
    return None


def cmd_register_mcp(args):
    """Decide the owner and register, with both halves under one lock.

    The two owners write different files, so their own write locks do not serialize the
    decision they share: without this, two concurrent runs both read a host with neither
    artifact present and both write, and the host then starts two bridges. The decision and
    the write it authorizes happen inside this one lock, and the configuration is read again
    inside it so the decision is made about the state that will be written.
    """
    codex_home = Path(args.codex_home or os.environ.get("CODEX_HOME") or Path.home() / ".codex")
    lock = bridgerecord.ownership_lock_path(codex_home)
    try:
        with hostrecord.Locked(lock):
            return _register_mcp_owned(args, codex_home)
    except hostrecord.Busy as error:
        emit({"command": "register-mcp", "owner": getattr(args, "owner", None),
              "outcome": BUSY, "detail": str(error), "applied": False, "wrote": False,
              "otherTablesPreserved": True,
              "note": "nothing was written: another run holds the ownership lock"})
        return EXIT_REFUSED


def _register_mcp_owned(args, codex_home):
    """Register the bridge through the supported Codex configuration path.

    Append-only and idempotent: an identical registration writes nothing, an absent one is
    appended at the end, and a different command or argument list is reported and refused.
    Every other table in the file is preserved, which is checked by comparing the bytes
    outside the appended block rather than asserted.

    Reading the file and scanning it are the protected boundary; rendering, replacing and
    reporting are not. That line matters: a ValueError from a render is a defect in this
    command and must keep raising, while a configuration that cannot be decoded is a refusal.
    """
    path, before = read_config(codex_home)
    if not before.usable:
        return refused("register-mcp", before, path=str(path), applied=False, wrote=False,
                       otherTablesPreserved=True,
                       note="nothing was written: the file was not read")
    before_text = before.value
    owner = getattr(args, "owner", bridgerecord.OWNER_USER)
    record_path = bridgerecord.record_path(codex_home)
    # Built before anything is written, for both owners, because the ownership decision needs
    # it and because a record that cannot be built is a reason to register nothing rather than
    # a result to report after the registration has already landed.
    try:
        wanted = bridgerecord.document(command=args.bridge_command,
                                       arguments=args.bridge_arg or [], name=args.name,
                                       issue=getattr(args, "issue", None), owner=owner)
    except ValueError as error:
        emit({"command": "register-mcp", "owner": owner, "path": str(path),
              "record": str(record_path), "outcome": codexconfig.CONFLICT,
              "detail": str(error), "applied": False, "wrote": False,
              "otherTablesPreserved": True,
              "note": "nothing was written: this run could not say what record would name the"
                      " owner of the registration it was about to make"})
        return EXIT_USAGE
    conflict = _mcp_ownership(record_path, owner, before_text, args.name, wanted,
                              codex_home=codex_home)
    if conflict:
        emit({"command": "register-mcp", "owner": owner, "path": str(path),
              "record": str(record_path), "outcome": codexconfig.CONFLICT,
              "detail": conflict,
              "applied": False, "wrote": False, "otherTablesPreserved": True,
              "note": "nothing was written. One owner registers this server; the other is"
                      " reported with its evidence rather than joined."})
        return EXIT_REFUSED
    if owner == bridgerecord.OWNER_PLUGIN:
        # The declaration is the package's, so this command writes the one fact the package
        # cannot carry and leaves the configuration alone. Reported as that: a record written
        # and no registration made, because on this host there is none until the plugin is
        # installed and its hooks and servers are trusted.
        try:
            written = bridgerecord.write(record_path, wanted, apply=args.apply)
        except hostrecord.Busy as error:
            emit({"command": "register-mcp", "owner": owner, "record": str(record_path),
                  "outcome": BUSY, "detail": str(error), "applied": False, "wrote": False,
                  "otherTablesPreserved": True})
            return EXIT_REFUSED
        emit({"command": "register-mcp", "owner": owner, "path": str(path),
              "record": str(record_path), "outcome": written["outcome"],
              "detail": written.get("detail"), "applied": written["applied"],
              "wrote": written["wrote"], "otherTablesPreserved": True,
              "preservedHow": "the Codex configuration was read and not written",
              "note": "The record was written and no MCP server was registered. The plugin"
                      " package declares the server, so install that package to register it."
                      " Written, registered and a tool actually called stay three claims."})
        return EXIT_OK if written["outcome"] in bridgerecord.SETTLED else EXIT_REFUSED
    try:
        with reading.region(path, "the Codex configuration"):
            new_text, outcome, detail = codexconfig.register(
                before_text, args.name, args.bridge_command, args.bridge_arg or [],
            )
    except reading.Refused as stop:
        return refused("register-mcp", stop.reading, path=str(path), applied=False, wrote=False,
                       otherTablesPreserved=True,
                       note="nothing was written: the file could not be scanned")

    wrote = False
    if not (args.apply and outcome == codexconfig.CREATED):
        # Nothing will be written on this path, so this read confirms no write: it reports the
        # file as it stands and takes no lock. Taking one here would make a run that writes
        # nothing wait on, and then refuse for, a lock it never needed. It sits ABOVE the write
        # rather than below it so that no read of this file follows the write outside the lock,
        # which is what makes the ordering checkable in source rather than only in a race.
        after = reading.read_text(path, "the Codex configuration")
    else:
        # Held under one lock for the whole read-modify-write, re-read immediately before
        # replacing, and written by temp file and replace. That coordinates runs of this
        # command with each other and removes truncation. It cannot coordinate with an editor
        # that does not take the same lock, and this is not called compare-and-swap for that
        # reason: a writer ignoring the lock can still land in the remaining window.
        try:
            with hostrecord.Locked(path):
                current = reading.read_text(path, "the Codex configuration")
                if not current.usable:
                    return refused("register-mcp", current, path=str(path), applied=False,
                                   wrote=False, otherTablesPreserved=True,
                                   note="nothing was written: the reread failed")
                if current.value != before_text:
                    emit({"command": "register-mcp", "path": str(path), "outcome": CHANGED,
                          "detail": "config.toml changed after it was read, so nothing was"
                                    " written; rerun against the file as it now stands",
                          "applied": False, "wrote": False, "otherTablesPreserved": True})
                    return EXIT_REFUSED
                with reading.region(path, "the Codex configuration"):
                    fresh, outcome, detail = codexconfig.register(
                        current.value, args.name, args.bridge_command, args.bridge_arg or [],
                    )
                if outcome == codexconfig.CREATED:
                    hostrecord.atomic_write(path, fresh)
                    new_text, wrote = fresh, True
                # Read back INSIDE the lock that guarded the write, which is what makes
                # readBack a claim about the bytes this run wrote. Taken after the lock was
                # released, a writer taking the same lock could land in between and this run
                # would report a verified round trip over somebody else's registration. The
                # sibling that appends a hook has always read back here; this is the same
                # ordering, not a stricter one.
                after = reading.read_text(path, "the Codex configuration")
        except hostrecord.Busy as error:
            emit({"command": "register-mcp", "path": str(path), "outcome": BUSY,
                  "detail": str(error), "applied": False, "wrote": False,
                  "otherTablesPreserved": True})
            return EXIT_REFUSED
        except reading.Refused as stop:
            return refused("register-mcp", stop.reading, path=str(path), applied=False,
                           wrote=False, otherTablesPreserved=True,
                           note="nothing was written: the reread could not be scanned")

    if not after.usable:
        if wrote:
            # The table landed and the file cannot be read back. Reporting this as a refusal
            # that wrote nothing would invite a retry that appends a second registration,
            # which is the exact outcome this command exists to prevent.
            emit({"command": "register-mcp", "path": str(path),
                  "outcome": APPLIED_UNVERIFIED, "applied": True, "wrote": True,
                  "readBack": False, "reading": after.refusal(),
                  "detail": "the registration was written and the file could not be read"
                            " back: " + str(after.detail),
                  "otherTablesPreserved": None,
                  "preservedHow": "not established: the file could not be read after the write"})
            return EXIT_REFUSED
        return refused("register-mcp", after, path=str(path), applied=False, wrote=False,
                       otherTablesPreserved=True,
                       note="nothing was written: the file could not be read back")
    after_text = after.value

    preserved = text_prefix(after_text, before_text) if wrote else after_text == before_text
    try:
        with reading.region(path, "the Codex configuration"):
            view = codexconfig.scan(after_text)
            servers = sorted(view.servers) if view.readable else None
            unreadable = view.unreadable or None
    except reading.Refused as stop:
        servers, unreadable = None, [stop.reading.detail]
    # The user owner records itself too. Without this, a host that registered the bridge here
    # and later installs the plugin gives the packaged launcher no record to read: it would
    # report an absent record on every session and tell the operator to write a plugin-owned
    # one, which is the opposite of what that host should do. With it, the launcher reads the
    # owner and stands down for the registration this command just made.
    #
    # Failing to write it never fails the registration, which has already landed; it is
    # reported as its own result.
    record = None
    if owner == bridgerecord.OWNER_USER and outcome not in REGISTER_REFUSALS:
        try:
            # The same document the ownership check settled on, so the check and the write
            # cannot describe two different records.
            record = bridgerecord.write(record_path, wanted, apply=args.apply)
        except hostrecord.Busy as error:
            record = {"record": str(record_path), "outcome": bridgerecord.MALFORMED,
                      "applied": False, "wrote": False, "detail": str(error)}
    emit({
        "command": "register-mcp",
        "owner": owner,
        "record": record,
        "path": str(path),
        "outcome": outcome,
        "detail": detail,
        "applied": wrote,
        "wrote": wrote,
        "readBack": True,
        "otherTablesPreserved": preserved,
        "preservedHow": (
            "the prior content is a byte-exact prefix of the new file, so nothing before the"
            " appended table was rewritten" if wrote else "nothing was written"
        ),
        "serversNow": servers,
        "unreadable": unreadable,
    })
    # Built from the two modules that own their own answers plus this command's three, rather
    # than respelled here. A partial application is never a success: left out of this set it
    # would exit 0, and a caller reading only the exit status would record a registration as
    # verified that nobody could read back.
    if outcome in REGISTER_REFUSALS:
        return EXIT_REFUSED
    if record is not None and record["outcome"] not in bridgerecord.SETTLED:
        # The registration landed and the record that names its owner did not. Reported as a
        # refusal rather than a success, because a host left in that state answers "nobody owns
        # this" to the launcher: installing the package would then start a second bridge beside
        # the registration this run just made. Nothing is undone here, since the registration is
        # already in the file; the emitted result carries both halves so the record can be
        # repaired on its own.
        return EXIT_REFUSED
    return EXIT_OK


# ------------------------------------------------------------------------- wiring

def build_parser():
    parser = argparse.ArgumentParser(prog="runtime_install.py", description=__doc__)
    sub = parser.add_subparsers(dest="command", required=True)

    sub.add_parser("verify-definition").set_defaults(handler=cmd_verify_definition)

    diagnose = sub.add_parser("diagnose")
    diagnose.add_argument("--codex-home")
    diagnose.add_argument("--dest",
                          help="the destination whose owned pointer is read; without it no"
                               " pointer is in scope and the reading is reported as not made")
    diagnose.add_argument("--record")
    diagnose.add_argument("--socket")
    diagnose.add_argument("--state")
    diagnose.add_argument("--issue")
    diagnose.add_argument("--relay-command")
    diagnose.add_argument("--bridge-command")
    diagnose.add_argument("--bridge-arg", action="append")
    diagnose.add_argument("--observed-tool", action="append",
                          help="a tool name actually listed in a live session")
    diagnose.add_argument("--trial", action="store_true",
                          help="the only mode that creates work; never implied by another flag")
    diagnose.add_argument("--parent-task", help="trial input: the registering parent task")
    diagnose.add_argument("--child-task", help="trial input: the child task")
    diagnose.add_argument("--recipient", help="trial input: the authorized recipient")
    diagnose.add_argument("--artifact-root", help="trial input: the artifact root")
    diagnose.add_argument("--turn-thread", help="trial input: the observed turn thread")
    diagnose.add_argument("--turn-id", help="trial input: the observed turn id")
    diagnose.add_argument("--artifact", action="append",
                          help="trial input: a deliverable for the reviewable receipt")
    diagnose.add_argument("--dispatch-turn-id",
                          help="trial input: the parent turn the generation binds to")
    diagnose.add_argument("--recipient-settings",
                          help="trial input: the recipient's authorized settings, JSON or @path")
    diagnose.add_argument("--settings-already-recorded", action="store_true",
                          help="trial input: the caller's own unverified claim that the"
                               " recipient's authorized settings are already recorded")
    diagnose.add_argument("--expect-relationship",
                          help="the relationship the caller expects the store to hold")
    diagnose.add_argument("--assignment-lookup", action="store_true",
                          help="run assignment-find and nothing else; it constructs a"
                               " store, which is why plain diagnose does not")
    diagnose.add_argument("--turn-status", default="completed",
                          choices=["completed", "failed", "interrupted", "inProgress"],
                          help="trial input: the status the child turn was observed in")
    diagnose.add_argument("--temporary", action="store_true",
                          help="record that this destination is temporary, not a host")
    diagnose.set_defaults(handler=cmd_diagnose)

    install = sub.add_parser("install")
    install.add_argument("--dest", required=True)
    install.add_argument("--python")
    install.add_argument("--codex-home",
                         help="the Codex home whose MCP registration and skill links are read"
                              " before promoting; defaults the way diagnose does")
    install.add_argument("--record")
    install.add_argument("--socket")
    install.add_argument("--state")
    install.add_argument("--issue", default="JUN-104")
    install.add_argument("--apply", action="store_true")
    install.set_defaults(handler=cmd_install)

    measure = sub.add_parser("measure")
    measure.add_argument("--socket")
    measure.add_argument("--state")
    measure.add_argument("--relay-command")
    measure.add_argument("--python")
    measure.add_argument("--environment")
    measure.add_argument("--record")
    measure.add_argument("--issue", default="JUN-104")
    measure.set_defaults(handler=cmd_measure)

    register = sub.add_parser("register-mcp")
    register.add_argument("--codex-home")
    register.add_argument("--name", default=MCP_NAME)
    register.add_argument("--bridge-command", required=True)
    register.add_argument("--bridge-arg", action="append")
    register.add_argument("--owner", choices=bridgerecord.OWNERS,
                          default=bridgerecord.OWNER_USER,
                          help="who registers this server. user writes the Codex configuration"
                               " entry, which is what this command has always done. plugin"
                               " writes only the record the packaged launcher reads, because"
                               " the CRW plugin declares the server itself; both together would"
                               " run a second bridge")
    register.add_argument("--issue", default="CRW-114")
    register.add_argument("--apply", action="store_true")
    register.set_defaults(handler=cmd_register_mcp)

    hook = sub.add_parser("hook")
    hook.add_argument("--codex-home")
    hook.add_argument("--event", default=None,
                      help="defaults to " + SESSION_START + ", or to " + completion.EVENT
                           + " when --adapter names the completion hook")
    named = hook.add_mutually_exclusive_group(required=True)
    named.add_argument("--hook-command", help="an explicit command to register")
    named.add_argument("--adapter", choices=ADAPTERS,
                       help="a hook this repository owns, whose command is derived rather than"
                            " typed and whose settings are written before it is registered")
    hook.add_argument("--dest", help="the install destination whose pointer names the runtime")
    hook.add_argument("--relay-command", help="an explicit relay executable, instead of --dest")
    hook.add_argument("--marker-root")
    hook.add_argument("--db-path")
    hook.add_argument("--journal-root")
    hook.add_argument("--python", help="the interpreter the registered command runs under")
    hook.add_argument("--mode", choices=completion.MODES, default=completion.OBSERVE,
                      help="observe classifies and records and never holds, which is the"
                           " default because holding depends on per-session write isolation"
                           " the caller has to have granted")
    hook.add_argument("--owner", choices=completion.OWNERS, default=completion.OWNER_USER,
                      help="who registers this adapter. user appends to the hook file, which is"
                           " what this command has always done. plugin writes the settings and"
                           " registers nothing, because the CRW plugin package declares the"
                           " registration itself; installing both would run two copies on every"
                           " " + completion.EVENT)
    hook.add_argument("--isolation-asserted-by",
                      help="who established that a held child cannot write the facts the"
                           " decision reads; required by --mode hold and recorded in the"
                           " settings, because the contract makes that grant a prerequisite")
    hook.add_argument("--guard-timeout", type=int, default=completion.DEFAULT_TIMEOUT_SECONDS,
                      help="the adapter's own budget for one guard call, kept under --timeout")
    hook.add_argument("--timeout", type=int, default=10)
    hook.add_argument("--issue", default="JUN-104")
    hook.add_argument("--apply", action="store_true")
    hook.set_defaults(handler=cmd_hook)

    hook_status = sub.add_parser("hook-status")
    hook_status.add_argument("--codex-home")
    hook_status.add_argument("--event", default=None)
    hook_status.set_defaults(handler=cmd_hook_status)
    return parser


def main(argv=None):
    """Run one command, and never let a defect leave a traceback.

    This is a bounded failure contract, not a second reading boundary, and the two are kept
    apart deliberately. A reading boundary answers a question about a record and reports a
    state from the four-state partition. This answers nothing: it says a defect in this
    command reached the top, names the exception and the line that raised it, and exits
    non-zero. 'internalError' never becomes an UNREADABLE record, because a code defect
    filed as a data problem is a defect that disappears.

    What it guarantees is narrow and worth stating: the worst case is a named refusal rather
    than a traceback. It does not guarantee that every input was anticipated.
    """
    args = build_parser().parse_args(argv)
    try:
        return args.handler(args)
    except reading.Refused as stop:
        emit({"command": args.command, "refused": stop.reading.detail,
              "reading": stop.reading.refusal(),
              "note": "a record could not be read and the command that reads it did not"
                      " report the refusal itself"})
        return EXIT_REFUSED
    except hostrecord.Busy as error:
        # A lock another run holds is a modelled outcome of every command that takes one, and
        # it says the same thing wherever it happens: nothing was established. Each site that
        # can say more answers it itself; this is the backstop, so a sibling added later cannot
        # report a competing run as a defect in this command the way the hook path did.
        #
        # The LOCK's own type, not the built-in. TimeoutError is an OSError, and a destination
        # on a network mount raises it with ETIMEDOUT for an ordinary filesystem call; catching
        # the broad type here would answer "another run holds a lock" about a failure no lock
        # took part in, which is this command's own subject in its own failure contract.
        emit({"command": args.command, "outcome": BUSY,
              "refused": "another run holds a lock this command needs: " + str(error),
              "raisedAt": reading.where(error),
              "note": "nothing was established by the step that needed the lock, and a run"
                      " that holds it is not a defect in this command."})
        return EXIT_REFUSED
    except Exception as error:                                   # noqa: BLE001 - see above
        emit({"command": args.command, "internalError": {
            "exception": type(error).__name__,
            "raisedAt": reading.where(error),
            "detail": str(error)[:500],
        }, "refused": "this command failed in a way it does not model",
            "note": "this is a defect in runtime_install.py, not a statement about any"
                    " record. It is reported rather than raised so a caller gets a result"
                    " instead of a traceback, and named so the defect stays reportable."})
        return EXIT_REFUSED


if __name__ == "__main__":
    raise SystemExit(main())
