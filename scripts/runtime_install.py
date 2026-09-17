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

from crw_runtime import (check, codexconfig, definition, hooks, hostrecord, ownership,
                         pointer, reading, scope, staging, swapgate)
from crw_runtime.text import text_prefix

ROOT = Path(__file__).resolve().parents[1]
EXIT_OK, EXIT_REFUSED, EXIT_USAGE = 0, 1, 2

# The two components, named once. The MCP server name spells the bridge's component name, and
# that shared spelling is stated here rather than left for a reader to infer from two literals
# that happen to match.
BRIDGE = "codex-thread-bridge"
RELAY = "codex-session-relay"
MCP_NAME = BRIDGE

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
    # An entry point that is not there really is absent; that is an answer, not a gap.
    "entry_point_recorded": (SIGNAL_NEGATIVE, (("runtime_install", "resolve_entry_point", None),)),
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


def pointer_state(destination, record, data):
    """Read the owned pointer, and compare what it names with what the record selects.

    The question is deliberately about the pointer against the RECORD and not against whatever
    a run is about to promote. Before a swap the pointer still names the predecessor, which the
    record also selects, so the two agree; a pointer somebody repointed by hand disagrees at
    every moment. Asked the other way the cell would report a conflict during every update.
    """
    path = pointer.pointer_path(destination)
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
    except (OSError, ValueError) as error:
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
        except (OSError, ValueError) as error:
            read["detail"] = ("a recorded selection could not be resolved: "
                              + type(error).__name__ + ": " + str(error))
            return read
    read["outside"] = outside
    read["agrees"] = not outside
    return read


def protected_environment(record, environment, destination, data):
    """Whether an environment is in use, so a later run must not remove it.

    Two readings and they are reported as two: the record's selection, and the pointer on disk.
    Either of them naming the environment protects it, and so does either of them failing to
    answer, because an environment nobody could establish as free is not an environment that is
    free. That direction is the safe one: the cost of keeping a directory is a named residual
    path, and the cost of removing a live one is the accident this exists to prevent.
    """
    selects = None
    if record is not None:
        selected = (record.get("selected") or {}).values()
        try:
            root = Path(environment).resolve()
            selects = any(within(Path(location).resolve(), root)
                          for location in selected if location)
        except (OSError, ValueError):
            selects = None
    names = pointer.names(pointer.pointer_path(destination), environment)
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
_STORE_TABLES_PROGRAM = """
import json, os, sys
from codex_session_relay.store import resolve_state_dir, read_only_rows

selection = resolve_state_dir(sys.argv[1] or None, sys.argv[2] or None)
database = selection.db_path
try:
    os.lstat(str(database))
except FileNotFoundError:
    print(json.dumps({"readable": True, "present": False, "dbPath": str(database),
                      "tables": None, "detail": None}))
    raise SystemExit(0)
except OSError as error:
    print(json.dumps({"readable": False, "present": None, "dbPath": str(database),
                      "tables": None,
                      "detail": type(error).__name__ + ": " + str(error)}))
    raise SystemExit(0)
answer = read_only_rows(
    selection,
    "SELECT name, sql FROM sqlite_master WHERE type = 'table'"
    " AND name NOT LIKE 'sqlite_%' ORDER BY name",
)
if not answer["readable"] or answer["detail"]:
    print(json.dumps({"readable": False, "present": True, "dbPath": str(database),
                      "tables": None,
                      "detail": answer["detail"] or "the store could not be read"}))
    raise SystemExit(0)
print(json.dumps({"readable": True, "present": True, "dbPath": str(database),
                  "tables": {row["name"]: row["sql"] for row in answer["rows"]},
                  "detail": None}))
"""

# The candidate's tables come from its own DDL applied to an in-memory database, so nothing is
# created anywhere and the answer is the schema that relay would actually install.
_CANDIDATE_TABLES_PROGRAM = """
import json, sqlite3
from codex_session_relay import store

database = sqlite3.connect(":memory:")
database.executescript(store.DDL)
rows = database.execute(
    "SELECT name, sql FROM sqlite_master WHERE type = 'table'"
    " AND name NOT LIKE 'sqlite_%' ORDER BY name",
).fetchall()
print(json.dumps({"readable": True, "tables": {row[0]: row[1] for row in rows},
                  "schemaVersion": store.SCHEMA_VERSION, "detail": None}))
"""


def _asked(argv, what, timeout=120):
    """Run one probe and return its parsed answer, or say why there is none."""
    try:
        done = subprocess.run(argv, capture_output=True, text=True, timeout=timeout)
    except (OSError, subprocess.SubprocessError) as error:
        return {"readable": False, "command": argv, "tables": None, "present": None,
                "detail": what + " could not be asked: " + type(error).__name__ + ": "
                          + error.__str__()}
    try:
        answer = json.loads(done.stdout)
    except ValueError:
        return {"readable": False, "command": argv, "tables": None, "present": None,
                "detail": what + " did not answer with JSON: "
                          + (done.stderr or done.stdout).strip()[-400:]}
    answer["command"] = argv
    return answer


def store_tables(interpreter, state=None, socket_path=None):
    """The tables the store actually holds, read under the relay that owns the rule.

    Asked of this checkout instead, the answer would describe a copy of a schema the selected
    installation owns rather than the schema it will run.
    """
    argv = [str(interpreter), "-c", _STORE_TABLES_PROGRAM, str(state or ""),
            str(socket_path or "")]
    return _asked(argv, "the store's tables")


def candidate_tables(interpreter):
    """The tables the candidate declares, read under the candidate's own interpreter."""
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
            return str(recorded), "recorded with the install"
        environment = install.get("environment")
        if environment:
            # Records written before interpreterPath existed still name the environment, and
            # the interpreter of an environment this command built is the one it created there.
            return str(Path(environment) / "bin" / "python"), "the recorded environment"
    return interpreter_of(entry_point), "the script's first line"


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
    if resolved and not entry_recorded and python:
        with reading.region("the installed entry point", "the interpreter it names",
                            field="shebang"):
            interpreter_path = Path(python).resolve()
        entry_recorded = any(within(interpreter_path, r) for r in roots)

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
    pointer_read = pointer_state(destination, record, data) if destination else None
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
# Split in two because the two halves are checked differently, and saying so here is what keeps
# the check itself free of a literal naming one member of the set it is iterating.
TRIAL_REQUIRED_INPUTS = {
    "issue": ("--issue", NON_BLANK),
    "parent_task": ("--parent-task", NON_BLANK),
    "child_task": ("--child-task", NON_BLANK),
    "recipient": ("--recipient", NON_BLANK),
    "artifact_root": ("--artifact-root", NON_BLANK),
    "turn_thread": ("--turn-thread", NON_BLANK),
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
                    "store_tables", "candidate_tables")

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


def _unusable_artifacts(artifacts, root, interpreter):
    """Why the relay would refuse each artifact, checked before anything is written.

    The path shape is the relay's own rule, asked of the relay's own function. What remains
    here is what that function deliberately does not cover: the file has to exist, be a regular
    file with no symbolic link at any component, be readable, and be inside the declared root.
    """
    paths = [str(raw) for raw in (artifacts or [])]
    if not paths:
        return []
    refusals, reason = _relay_normalizes(paths, interpreter)
    if reason:
        return [reason + "; artifacts are not checked against a second copy of the rule"]

    problems = []
    base = Path(str(root))
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
        try:
            if not (path == base or base in path.parents):
                problems.append(raw + " is not inside the artifact root " + str(base))
        except (OSError, ValueError):
            problems.append(raw + " could not be compared with the artifact root")
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
    blank = sorted(flag for name, (flag, _) in TRIAL_REQUIRED_INPUTS.items()
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
    # consumer's own code in the runtime that will act on the answer.
    for predicate in sorted({pair[1] for pair in TRIAL_REQUIRED_INPUTS.values()
                             if pair[1] != NON_BLANK}):
        governed = {flag: str(getattr(args, name))
                    for name, (flag, declared) in TRIAL_REQUIRED_INPUTS.items()
                    if declared == predicate}
        answer = values_usable(predicate, governed, relay_interpreter)
        if not answer.get("usable"):
            return check.field(
                "not_verified",
                "these inputs are not what " + predicate[-1] + " accepts: "
                + str(answer.get("detail")) + ". This is the relay's own predicate for them,"
                " asked before the first mutating step. Nothing was written.",
                acting_process=acting_process(), measured_at=now(),
            )
    if args.recipient != args.parent_task:
        return check.field(
            "not_verified",
            "the recipient " + str(args.recipient) + " is not the parent task "
            + str(args.parent_task) + ". A completion is queued to the relationship parent and"
            " that parent must be an allowed recipient, so this combination can only be"
            " refused after the store has been written to.",
            acting_process=acting_process(), measured_at=now(),
        )
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
            "these artifacts do not satisfy what the relay requires of a manifest entry: "
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

def cmd_hook(args):
    codex_home = Path(args.codex_home or os.environ.get("CODEX_HOME") or Path.home() / ".codex")
    path = codex_home / "hooks.json"
    hook = {"type": "command", "command": args.hook_command, "timeout": args.timeout}
    result = hooks.install(path, args.event, hook, issue=args.issue, apply=args.apply)
    emit({
        "command": "hook",
        "hookFile": str(path),
        "result": result,
        "note": (
            "Installed, enabled and observed to have fired are three separate claims. This"
            " command appends and reads back; it never enables a daemon and never reports"
            " activation. Removing a hook renumbers later identities, so this command refuses"
            " removal and provides no way to perform one."
        ),
    })
    # The set comes from the module that produces the outcomes, not from a list respelled here.
    return EXIT_OK if result["outcome"] in hooks.SETTLED else EXIT_REFUSED


# ------------------------------------------------------------------------- install

def cmd_install(args):
    codex_home = Path(args.codex_home or os.environ.get("CODEX_HOME") or Path.home() / ".codex")
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
    except TimeoutError as error:
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
                                         issue=args.issue)
            if decision == staging.ADOPT:
                # rmdir, never rmtree. It succeeds only on an empty directory, so the call is
                # its own proof that nothing was destroyed, and the exclusive mkdir below still
                # establishes ownership the way it always did.
                try:
                    os.rmdir(str(environment))
                except OSError as error:
                    emit(dict(standing, refused="the empty staging directory could not be"
                                                " taken over: " + type(error).__name__ + ": "
                                                + str(error),
                              residualPaths=[str(environment)]))
                    return EXIT_REFUSED
                performed.append({"step": "take over an empty staging directory", "ok": True,
                                  "detail": why})
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
        holder = staging.Held(environment).take()
        staging.write_claim(environment, staging.STAGING, issue=args.issue,
                            run=str(os.getpid()))
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

        # OPS-4.4 decides whether a runtime may be replaced AT ALL, and it is read here:
        # after the candidate is exercised, before anything owned moves. The daemon and the
        # store are the outgoing runtime's, so they are asked of the selected relay where
        # there is one; on a first install there is none and the candidate answers for a host
        # that has neither. This command never starts or stops a service (OPS-4.1).
        outgoing_relay = _selected_install(record, RELAY)
        gate_relay = (outgoing_relay or {}).get("entryPoint") or str(environment / "bin" / RELAY)
        gate_python = (outgoing_relay or {}).get("interpreterPath") or str(python)
        gate = swapgate.decide({
            "daemon": swapgate.daemon_cell(scope.relay(
                ["service", "status"], executable=gate_relay, socket=args.socket,
                state=args.state)),
            "inFlight": swapgate.inflight_cell(scope.relay(
                ["doctor"], executable=gate_relay, socket=args.socket, state=args.state)),
            "storeTables": swapgate.tables_cell(
                store_tables(gate_python, args.state, args.socket),
                candidate_tables(python)),
        })
        if gate["verdict"] != swapgate.ALLOWED:
            # The existing installation is kept exactly as it stands. A cell that could not be
            # read keeps it for the same reason a refusal does.
            performed.append({"step": "read whether it is safe to swap", "ok": False,
                              "detail": json.dumps({"verdict": gate["verdict"],
                                                    "blockedBy": gate["blockedBy"],
                                                    "unreadable": gate["unreadable"]})})
            return _install_failed(record_path, data["definitionVersion"], performed,
                                   environment, owned, pointer_path=pointer_path,
                                   failed_step="read whether it is safe to swap", gate=gate)
        performed.append({"step": "read whether it is safe to swap", "ok": True,
                          "detail": gate["verdict"]})

        # The selection moves only to something this command's own classification calls own.
        # Validated against the STAGED record, before the pointer changes, because recovery
        # deliberately keeps a selected candidate: promoting first and checking afterwards
        # would leave an invalid runtime selected and protected from cleanup.
        staged = hostrecord.load(record_path, data["definitionVersion"])
        if not staged.usable:
            return _install_failed(record_path, data["definitionVersion"], performed,
                                   environment, owned, failed_reading=staged)
        # The conflict readings this promotion is decided against. Without them the command
        # that moves the selection was blind to a conflict diagnose would have raised: an MCP
        # registration naming a different bridge, or a foreign skill path. The registration is
        # compared on the command this run is promoting and on nothing else, because install
        # knows which entry point it installed and knows nothing about the arguments a host
        # chose; an empty argument list would be an expectation, not the absence of one.
        links = skill_links(codex_home)
        # The registration names the owned POINTER, which is stable across updates, and not the
        # environment underneath it, which changes every time the sources do. Compared against
        # the environment, an installation registered by its predecessor read as a conflict and
        # every later update refused to promote. The path comes from the record where one is
        # recorded, because this comparison is string equality and a destination spelled
        # differently on a later run is a different string for the same directory.
        bridge_entry = str(pointer_path / "bin" / component_of(data, BRIDGE)["consoleScript"])
        registration = registration_state(codex_home, bridge_entry, [], compare_args=False)
        # A host installed before the pointer existed registers a CONCRETE entry point, and
        # comparing it against the pointer reads as a conflict. It is not one: it is this
        # command's own previous registration, recorded in the host record, and treating it as
        # somebody else's would refuse every upgrade of exactly the installed base the pointer
        # exists to unpin. Ownership is established positively from the record, never from the
        # shape of the path.
        inherited = _inherited_registration(registration, staged.value, data)
        if inherited:
            registration = dict(registration, outcome=codexconfig.LINKED,
                                detail=inherited["detail"], inherited=inherited)
        # And what the pointer itself names, which the registration stopped being able to say.
        pointer_read = pointer_state(destination, staged.value, data)
        verdicts = {}
        for component in data["components"]:
            name = component["component"]
            verdicts[name] = classify_component(
                component, record=staged.value,
                entry_override=installs[name]["entryPoint"],
                # The MCP registration is the bridge's, and says nothing about the relay.
                registration=registration if name == MCP_NAME else None,
                links=links,
                pointer=pointer_read,
                # Freshly observed by the measurement this promotion is about, so measurement,
                # classification and promotion all speak about the same App Server.
                app_server=measurement.get("appServer"))
        unqualified = {n: {"class": v["class"], "reasons": v["reasons"]}
                       for n, v in verdicts.items() if not ownership.reusable(v["class"])}
        if unqualified:
            # The pointer moves only to something this command would itself call reusable.
            # The reasons travel with the refusal because the commonest one is not a fault in
            # the installation at all: a checkout with uncommitted changes cannot be
            # attributed to a revision, so what was installed from it is not reusable no
            # matter how well the installation went.
            performed.append({"step": "classify the candidate", "ok": False,
                              "detail": "the candidate would not be reusable: "
                                        + json.dumps(unqualified)})
            return _install_failed(record_path, data["definitionVersion"], performed,
                                   environment, owned)

        # Promotion is ONE critical section. The record's selection and the pointer on disk
        # are two truths, and between them lies the only window in which a runtime is selected
        # and unreachable. Holding the pointer lock across both writes means no other run of
        # this command can interleave, so the only thing that can land in that window is a
        # kill -- and a kill there is exactly what the RESUME decision above repairs.
        landed = None
        try:
            with hostrecord.Locked(pointer_path):
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
                recorded_pointer = (staged.value.get("pointer") or {}).get("path")
                if before["state"] == pointer.LINK and not recorded_pointer:
                    performed.append({"step": "establish the pointer is this command's",
                                      "ok": False,
                                      "detail": "a symbolic link is already at " + str(pointer_path)
                                                + " and this host record has never recorded"
                                                  " placing one there"})
                    return _install_failed(
                        record_path, data["definitionVersion"], performed, environment, owned,
                        pointer_path=pointer_path,
                        failed_step="establish the pointer is this command's")

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
                pointer.place(pointer_path, environment)
                landed = pointer.names(pointer_path, environment)
        except OSError as error:
            # The selection landed and the pointer did not, so the selection goes back where it
            # was for the components this run moved and the record agrees with the disk again.
            performed.append({"step": "replace the owned pointer", "ok": False,
                              "detail": type(error).__name__ + ": " + error.__str__()})
            return _install_failed(
                record_path, data["definitionVersion"], performed, environment, owned,
                pointer_path=pointer_path, failed_step="replace the owned pointer",
                restored=_restore_selection(record_path, data["definitionVersion"], previous,
                                            installs, pointer_path))

        # Read back rather than trusted. A swap reported as done that did not land is the one
        # failure that would leave the record naming a runtime no host can reach.
        if landed is not True:
            performed.append({"step": "read the owned pointer back", "ok": False,
                              "detail": pointer.read(pointer_path).get("detail")})
            if before["state"] == pointer.LINK and before.get("target"):
                with hostrecord.Locked(pointer_path):
                    pointer.place(pointer_path, before["target"])
            return _install_failed(
                record_path, data["definitionVersion"], performed, environment, owned,
                pointer_path=pointer_path, failed_step="read the owned pointer back",
                restored=_restore_selection(record_path, data["definitionVersion"], previous,
                                            installs, pointer_path))
        performed.append({"step": "replace the owned pointer", "ok": True,
                          "previousTarget": before.get("target"),
                          "target": str(environment)})

        # The claim settles last. It says this staging finished, and until the selection and the
        # pointer both name it there is nothing finished to say.
        staging.write_claim(environment, staging.COMPLETE, issue=args.issue,
                            run=str(os.getpid()))

        emit({
            "command": "install", "applied": True, "environment": str(environment),
            "hostRecord": str(record_path), "steps": performed, "installs": installs,
            "measurement": measurement,
            "promoted": True,
            "swapGate": gate,
            "pointer": {"path": str(pointer_path), "target": str(environment),
                        "previousTarget": before.get("target"),
                        "meaning": "the registered command reaches a runtime through this path."
                                   " It is a way to reach one and never an identity: a console"
                                   " script keeps its absolute shebang, so a process already"
                                   " spawned goes on running the environment it started in."},
            "classification": {n: v["class"] for n, v in verdicts.items()},
            "selected": record.get("selected") or {},
            "previousSelection": previous,
            "note": (
                "the pointer moves only after a qualifying point exists for the candidate"
                " (OPS-2.4). A candidate that imports but fails its exercise stays unselected and"
                " the previous runtime remains selected. Nothing here removes, moves or recreates"
                " the store."
            ),
        })
        # Reached only when the candidate qualified AND classified own, so this is the one
        # success return after ownership; every other exit goes through the release path.
        return EXIT_OK


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


def _finish_promotion(record_path, data, environment, pointer_path, standing, *, issue):
    """Write the half a killed run did not: the pointer, for a selection already committed.

    The two truths are written one after the other inside one lock, so the only thing that can
    land between them is the process dying. That leaves a runtime that is selected and
    unreachable, and rebuilding would be the wrong repair: it is built, it is selected, and a
    process may already be running out of it. So the pointer is brought into agreement with the
    selection and the claim is settled. Nothing is rebuilt and nothing is removed.
    """
    with hostrecord.Locked(pointer_path):
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
                                        " there is no interrupted promotion here to finish"))
            return EXIT_REFUSED
        before = pointer.read(pointer_path)
        if not pointer.usable(before["state"]):
            emit(dict(standing, refused="the interrupted promotion could not be finished: "
                                        + str(before["detail"]),
                      pointer={"path": str(pointer_path), "state": before["state"]}))
            return EXIT_REFUSED
        try:
            pointer.place(pointer_path, environment)
        except OSError as error:
            emit(dict(standing, refused="the interrupted promotion could not be finished: "
                                        + type(error).__name__ + ": " + str(error)))
            return EXIT_REFUSED
        landed = pointer.names(pointer_path, environment)
    if landed is not True:
        emit(dict(standing, refused="the pointer did not land on the environment the host"
                                    " record already selects"))
        return EXIT_REFUSED
    staging.write_claim(environment, staging.COMPLETE, issue=issue, run=str(os.getpid()))
    emit(dict(standing, applied=True, resumed=True,
              pointer={"path": str(pointer_path), "previousTarget": before.get("target"),
                       "target": str(environment)},
              note="a previous run committed this environment as selected and did not live to"
                   " move the pointer. Nothing was rebuilt and nothing was removed: the missing"
                   " half of that promotion was written and the claim settled."))
    return EXIT_OK


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
    recorded = []
    for component in data["components"]:
        entry = ((record or {}).get("components", {}).get(component["component"]) or {})
        for install in entry.get("installs") or []:
            if install.get("entryPoint"):
                recorded.append(str(install["entryPoint"]))
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


def _restore_selection(record_path, definition_version, previous, installs, pointer_path):
    """Put back the selection this run just moved, for the components it moved.

    Narrow on purpose. Re-asserting a whole selection map would carry back entries read before
    the slow work and re-assert them as current, which is the staleness the single-writer helper
    exists to prevent. This re-asserts only the components this run changed, and only where
    there was a previous value. A component that was never selected cannot be unselected through
    a delta, and saying so is better than reporting a restoration that did not happen.
    """
    missing = sorted(name for name in installs if not previous.get(name))
    with hostrecord.Locked(pointer_path):
        # Held under the promotion's own lock, and only for entries that still name what THIS
        # run wrote. A blind put-back would undo a promotion another run committed in the
        # meantime, which is the staleness the single-writer helper exists to prevent -- and
        # rolling back on top of somebody else's success is a worse outcome than the failure
        # being rolled back.
        current = hostrecord.load(record_path, definition_version)
        if not current.usable:
            return {"selection": None, "restored": [], "withoutPrevious": missing,
                    "detail": "the host record could not be read, so the previous selection"
                              " could not be put back: " + str(current.detail)}
        selected = (current.value.get("selected") or {})
        back, moved_on = {}, []
        for name, install in installs.items():
            if selected.get(name) != install["location"]:
                moved_on.append(name)
                continue
            if previous.get(name):
                back[name] = previous[name]
        if not back:
            return {"selection": None, "restored": [], "withoutPrevious": missing,
                    "movedOnByAnotherRun": sorted(moved_on),
                    "detail": "there was nothing of this run's left to put back: either nothing"
                              " was selected before it, or another run has since moved the"
                              " selection on"}
        written = hostrecord.update(record_path, definition_version, select=back)
    return {"selection": back if written.usable else None,
            "restored": sorted(back) if written.usable else [],
            "withoutPrevious": missing, "movedOnByAnotherRun": sorted(moved_on),
            "detail": ("the previous selection was put back" if written.usable
                       else "the previous selection could not be put back: "
                            + str(written.detail))}


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
                    failed_step=None, restored=None, gate=None):
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
            "restored": restored,
            "meaning": ("the pointer was not moved by this run unless 'restored' says so."
                        " Whatever a host reached before this run, it still reaches"),
        },
        "swapGate": gate,
        "residualPaths": [str(owned)] if (owned is not None and not removed) else [],
        "recoveryRequires": None if retriable else (
            ("this environment is selected, so it was kept deliberately and the destination"
             " cannot be retried until the selection moves") if keeping
            else ("remove " + str(owned) + " by hand; this run created it and could not remove"
                  " it, so the same destination will keep refusing until it is gone")
        ),
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

def cmd_register_mcp(args):
    """Register the bridge through the supported Codex configuration path.

    Append-only and idempotent: an identical registration writes nothing, an absent one is
    appended at the end, and a different command or argument list is reported and refused.
    Every other table in the file is preserved, which is checked by comparing the bytes
    outside the appended block rather than asserted.

    Reading the file and scanning it are the protected boundary; rendering, replacing and
    reporting are not. That line matters: a ValueError from a render is a defect in this
    command and must keep raising, while a configuration that cannot be decoded is a refusal.
    """
    codex_home = Path(args.codex_home or os.environ.get("CODEX_HOME") or Path.home() / ".codex")
    path, before = read_config(codex_home)
    if not before.usable:
        return refused("register-mcp", before, path=str(path), applied=False, wrote=False,
                       otherTablesPreserved=True,
                       note="nothing was written: the file was not read")
    before_text = before.value
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
    if args.apply and outcome == codexconfig.CREATED:
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
        except TimeoutError as error:
            emit({"command": "register-mcp", "path": str(path), "outcome": BUSY,
                  "detail": str(error), "applied": False, "wrote": False,
                  "otherTablesPreserved": True})
            return EXIT_REFUSED
        except reading.Refused as stop:
            return refused("register-mcp", stop.reading, path=str(path), applied=False,
                           wrote=False, otherTablesPreserved=True,
                           note="nothing was written: the reread could not be scanned")

    after = reading.read_text(path, "the Codex configuration")
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
    emit({
        "command": "register-mcp",
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
    register.add_argument("--apply", action="store_true")
    register.set_defaults(handler=cmd_register_mcp)

    hook = sub.add_parser("hook")
    hook.add_argument("--codex-home")
    hook.add_argument("--event", default="SessionStart")
    hook.add_argument("--hook-command", required=True)
    hook.add_argument("--timeout", type=int, default=10)
    hook.add_argument("--issue", default="JUN-104")
    hook.add_argument("--apply", action="store_true")
    hook.set_defaults(handler=cmd_hook)
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
