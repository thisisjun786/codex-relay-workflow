#!/usr/bin/env python3
"""One destination carried from a new install to the recovery of the install it replaced.

Each landed piece already has its own tests: the integrated installer and its diagnosis, the
update that fails and restores what it found, and the completion hook the installer registers.
What none of them states is the sequence a host actually lives through -- on one destination,
with the state that has to survive it seeded before the first install and read again after the
last refusal. A suite of separately passing installer tests is not that sequence, and the
difference is where a host loses a runtime.

So this module composes rather than repeats. It drives the fixture the update tests already
drive through four stages against one temporary destination, one host record, one Codex
configuration, one hook file and one store, and it asserts on what survived rather than on what
a command reported about itself.

Three rules are the point of the composition rather than decoration.

A cell is filled by the reading its own question called for. READINGS declares the cell, the
source that answers it and the path the answer is read from, and read() is the only way a cell
is ever filled. A reading that could not be made reads UNREADABLE: it never reads False, and it
never borrows the value of the cell beside it. crw_runtime/check.py states the same rule for the
eight result fields -- "None of the eight is ever derived from another" -- and this is where the
rule is exercised across three sources instead of inside one payload.

An assertion about survival is worthless unless something happened. Every stage therefore also
asserts what a run that did nothing could not produce: an environment the command built, a
selection naming it, a pointer reaching it, and a named seam that actually fired.

Nothing here is evidence about a host. Every path is a temporary directory, the two build steps
and the relay are stand-ins inherited from the update fixture, the diagnosis runs with HOME,
XDG_STATE_HOME, CODEX_HOME and PATH pointed inside that directory, and PROVENANCE records that a
row filled here is a fixture answer. What a real combination would have to record, and the
procedure for running one, is in docs/runtime-install.md.

A fourth rule arrived as three separate findings and is one rule. A row can reach its success
answer, down the path it declared, on the host it declared, and still be answering about
something this scenario never built: a helper called instead of the registered command, a second
Codex home, a run told its store was absent while the fixture had populated one. What those share
is that something handed to the thing under test was not what the scenario built, and the
difference was nowhere in the claim. So HANDED declares, per function, whether what it hands is
the built value or a stand-in -- and a stand-in says what the scenario has instead and what a row
reading through it therefore does not prove. INJECTIONS does the same for the switches the update
fixture accepts, and names the one this module must not hand.
"""

import ast
import builtins
import copy
import hashlib
import inspect
import json
import os
import shutil
import shlex
import stat
import subprocess
import sys
import tempfile
import types
import unittest
from pathlib import Path
from unittest import mock

ROOT = Path(__file__).resolve().parents[3]
sys.path.insert(0, str(ROOT / "scripts"))
# Discovery puts this directory on the path; running this file on its own does not, and the
# composition is worth being able to run by itself while working on it.
sys.path.insert(0, str(Path(__file__).resolve().parent))

from crw_runtime import (check, codexconfig, completion, definition, hooks, hostrecord,
                         ownership, pointer, reading, scope, staging, swapgate)

import runtime_install
import test_runtime_install as base

RUNTIME = ROOT / "scripts" / "runtime_install.py"
HERE = Path(__file__).resolve()

try:
    import tomllib
    HAS_READER = True
except ImportError:  # 3.10 has no reader, and approximating TOML is refused rather than tried
    HAS_READER = False


# The seven questions the acceptance criterion asks, each answered on its own reading. Declared
# as one tuple so a row nobody asked for, or a question with no row to answer it, is a failure
# here rather than a silent gap.
SEVEN = (
    "skillLink",
    "runtimeImport",
    "mcpToolExposure",
    "appServerConnection",
    "hookCallback",
    "modelPermissionPreservation",
    "deliveryAcceptance",
)

# Sources that are commands. A payload from one of these carries its own name, and read() checks
# that name before walking a path: handing one command payload to another command row is the
# cheapest way for a reading to answer a question it was never asked.
CLI_COMMANDS = ("diagnose", "hook-status")

# The source for a reading this module performs itself. It has no command name, so its payload
# carries an explicit stamp and read() checks that stamp the same way.
ACCEPTANCE = "acceptance"

# cell, source, the path the answer is read from, the function that produces it.
READINGS = (
    ("skillLink", "diagnose", ("skillLinks",),
     "runtime_install.skill_links"),
    ("runtimeImport", "diagnose", ("checks", "results", "imported"),
     "runtime_install.cmd_diagnose"),
    ("mcpToolExposure", "diagnose", ("checks", "results", "mcpExposed"),
     "runtime_install._mcp_exposed"),
    ("appServerConnection", "diagnose", ("checks", "results", "connected"),
     "runtime_install.cmd_diagnose"),
    ("hookCallback", "hook-status", ("firingJournal",),
     "completion._journal_cell"),
    ("modelPermissionPreservation", ACCEPTANCE, ("preservation",),
     "test_install_acceptance._model_permission_delta"),
    ("deliveryAcceptance", "diagnose", ("checks", "results", "deliveryAccepted"),
     "runtime_install._trial"),
)

# Readings this module needs that are not among the seven. alwaysActive is here rather than in
# SEVEN because the criterion does not ask for it to be judged; it asks that a successful
# install is never marked as it. Declaring it keeps that claim reachable through read() like
# every other cell, instead of through a subscript the rule above would have to make an
# exception for.
BESIDE = (
    ("alwaysActive", "diagnose", ("checks", "results", "alwaysActive"),
     "runtime_install.cmd_diagnose"),
    ("destinationKind", "diagnose", ("checks", "destinationKind"),
     "check.record"),
)

DECLARED = READINGS + BESIDE

# Where a declared producer is resolved: the module OBJECT, not its file. Reading a file for a
# definition passes on a name that survives in a comment or an unreachable branch, and for the
# reading this module performs itself it would have been searching the file that declares the
# producer for the name it declares -- a check answering its own question. Asking the module
# yields the callable a caller would actually reach.
PRODUCER_MODULES = {
    "runtime_install": runtime_install,
    "completion": completion,
    "check": check,
    "test_install_acceptance": sys.modules[__name__],
}

# The places in this module that reach a conclusion by reading source text rather than by asking
# the thing itself, each with the reason it cannot ask. Everything else that once lived here now
# asks: the producers are resolved as attributes, the fixture stand-ins are observed while the
# fixture is applying them, and the declared paths are walked in payloads the sources really
# produced.
#
# The check below does not recognise a shape. source_text_reached derives every way this file can
# reach the text of a source file -- a binding that is a .py path, __file__, a called name that
# hands source back, a string naming a .py file -- accounts for every occurrence of one, and
# fails on an occurrence that is in neither this map nor TOUCHES_SOURCE_WITHOUT_CONCLUDING. What
# it still cannot reach is in SOURCE_OUT_OF_REACH, with a control for each form.
TEXT_EVIDENCE = {
    "test_no_cell_is_filled_without_going_through_the_declared_reading":
        "a style rule about this module's own code has no object to ask: the thing it forbids"
        " is a line that was never written, so the source is the only witness. It is a lint and"
        " says so; the property it approximates is proved by the mutation cases instead.",
    "test_every_text_reading_place_is_declared":
        "the derivation that polices the others has to read this file to find them.",
    "source_text_reached":
        "it IS that derivation: to find the places that conclude from source text it has to"
        " parse this file, and no object can be asked where a spelling appears in a file.",
    "refusals_reached":
        "the same, for refusals: where a refusal is named is a fact about the text, and the"
        " objects can only say what the refusals are, not where they are written.",
    "test_the_reach_of_each_derivation_is_the_declared_one":
        "it runs the derivations, and running the source-text one means reading this file.",
    "test_no_two_places_this_module_declares_can_share_a_name":
        "whether two places share a name is a property of the text: the module object has"
        " already discarded the second by the time it could be asked.",
    "test_every_place_that_settles_for_a_refusal_is_declared":
        "the places that name a refusal are in the text, so this inventory is derived from this"
        " file the same way -- and it is derived by accounting for every occurrence rather than"
        " by recognising the shape of the call, because a shape sweep is what missed two of them.",
    "test_no_call_site_hands_the_fixture_an_injection_this_module_refuses":
        "the subject IS this file's own calls: which switch a call hands the fixture is a"
        " property of the call site, and every call site is here. Observing them instead would"
        " mean running every scenario inside one case, and one run can only speak for itself."
        " The behavioural half is the agreement case, which reads what a run was actually told.",
}

# Places that reach a source file without concluding anything from its text. The negative rule
# over-collects on purpose: it is cheaper to write down why a place is not one than to discover,
# four times running, that a form nobody enumerated was never looked at.
TOUCHES_SOURCE_WITHOUT_CONCLUDING = {
    "diagnose_for":
        "it runs RUNTIME as a program and reads what the program answered. The text of that"
        " file is never opened here, and what the run answers is the thing being asked.",
    "linked_skills":
        "it names install.py to run it, not to read it.",
    "importable_runtime":
        "it names __init__.py to decide where a package is, which is a question about the"
        " filesystem rather than about what the file says.",
    "source_spellings.is_source_file":
        "it names the .py suffix to decide which bindings are source files. Which files exist"
        " is a question for the filesystem, and it asks the filesystem.",
    "_source_spelled.spelled":
        "the matcher: it names the suffix in order to look for it, which is the question"
        " rather than an answer taken from a file.",
    "asked_over":
        "it names the file it writes for a check to read. What it reads back is what the"
        " CHECK said, which is a behaviour rather than a text.",
}

# Module-level statements that name a source file, keyed by what the statement binds rather than
# by a line, because a line moves whenever anything above it does.
SOURCE_AT_MODULE_LEVEL = {
    "ROOT": "it locates the checkout from this file's path and never opens it",
    "sys.path.insert": "it puts this file's own directory on the import path",
    "RUNTIME": "it names the runtime script so a later call can run it",
    "HERE": "it is the handle itself, which is what the derivation looks for",
}

# Called names whose signature this derivation could not read, so it cannot say whether they hand
# source text back. Reported rather than assumed harmless. Which of them an interpreter can read
# differs by version -- set is readable from 3.11 and not on the 3.10 floor -- so this is the
# UNION, and each entry says where it is unreadable so a name that becomes readable everywhere
# fails its own declaration instead of sitting here forever.
EVERY_INTERPRETER = "every supported interpreter"
THE_FLOOR = "the 3.10 floor"
# Where the floor entries stop being unreadable. Written down because no object can be asked it:
# it is a fact about which interpreter gave its builtin types an introspectable signature.
READABLE_FROM = (3, 11)
SOURCE_UNDECIDED_CALLS = {
    "getattr": (EVERY_INTERPRETER, "a builtin whose signature is not introspectable"),
    "vars": (EVERY_INTERPRETER, "the same"),
    "str": (EVERY_INTERPRETER, "a builtin type; calling it constructs a string"),
    "dict": (EVERY_INTERPRETER, "a builtin type; calling it constructs a mapping"),
    "type": (EVERY_INTERPRETER, "a builtin type; calling it asks an object what it is"),
    "KeyError": (EVERY_INTERPRETER, "a builtin exception being raised"),
    "set": (THE_FLOOR, "a builtin type whose signature 3.11 made readable and 3.10 did not"),
    "frozenset": (THE_FLOOR, "the same, and the pair of them is why this record is a union"),
    "zip": (THE_FLOOR, "a builtin type the floor cannot read either"),
}

# What this derivation still cannot see, as data rather than as a sentence. Each form is planted
# by a control that requires the derivation NOT to see it, so the list cannot rot in either
# direction: widening the derivation to cover one makes its control fail.
SOURCE_OUT_OF_REACH = {
    "a handle reached by its name as a string":
        ("getattr(module, \"HERE\")",
         "the name is a string, so there is no Name for the sweep to account. This case is not"
         " hypothetical: the regression cases below reach HERE exactly this way."),
    "a source path assembled at run time":
        ("open(Path(directory) / chosen)",
         "nothing in the expression names a .py file or a handle, so which file it opens is a"
         " fact about the run rather than about the text."),
    "a callable reached through anything other than a name":
        ("readers[key](read)",
         "the derivation follows names: a handle, a called name, a name bound to one. A callable"
         " taken out of a container, off an instance or out of a registry is chosen at run time,"
         " and which one it is is not a fact about this text. This is the end of the flow the"
         " reader follows, and it is written down here rather than left to be discovered."),
    "a helper in another module that hands source text back":
        ("base.source_of(thing)",
         "the derivation follows helpers defined HERE to their callers; a name defined"
         " elsewhere would have to be resolved and read, which is a different question."),
}

# The same floor for source text: these have to keep being derived, ast.parse above all, because
# it is the one form the check this replaces spelled out by hand and it now arrives only by being
# asked for its own signature.
SOURCE_REACH_INCLUDES = {
    "handle": ("HERE", "RUNTIME", "__file__"),
    "hands source": ("ast.parse",),
}

# Rows whose reading is a check.field cell and therefore answers in check.VALUES. The others
# answer in their own producer vocabulary and are NOT translated into this one: a cell rewritten
# into a neighbour vocabulary is the same borrowed answer with better manners.
CHECK_FIELD_ROWS = ("runtimeImport", "mcpToolExposure", "appServerConnection",
                    "deliveryAcceptance")
COMPLETION_CELL_ROWS = ("hookCallback",)
OWN_SHAPE_ROWS = ("skillLink", "modelPermissionPreservation")

PRESERVED = "preserved"
CHANGED = "changed"

# What the supplied interpreter has to answer about itself. Asked of the interpreter rather
# than read out of the text that launches it: a wrapper can mention -S and -E in a comment and
# run neither, and then every path this suite reports is right while the probe had the site
# directory and the inherited import path all along. The flags are a property of the process,
# so the process is the only thing that can establish them.
HERMETIC_FLAGS = {"no_site": 1, "ignore_environment": 1}

_FLAG_PROGRAM = (
    "import json, sys\n"
    "print(json.dumps({'no_site': sys.flags.no_site,\n"
    "                  'ignore_environment': sys.flags.ignore_environment,\n"
    "                  'smuggled': [p for p in sys.path if 'crw-smuggled' in p]}))\n"
)


def hermetic_answers(interpreter, root):
    """Ask the supplied interpreter what it is, with something planted where it must not look."""
    done = subprocess.run(
        [str(interpreter), "-c", _FLAG_PROGRAM], capture_output=True, text=True, timeout=120,
        env=dict(os.environ, PYTHONPATH=str(root / "crw-smuggled")))
    if done.returncode != 0 or not done.stdout.strip():
        return None, "the supplied interpreter did not answer: " + (done.stderr or "")[-200:]
    return json.loads(done.stdout), None


def inside(where, root):
    """Whether a path is under this root, by ancestry rather than by how it is spelled.

    A prefix comparison reads /tmp/root-escape as being inside /tmp/root, which is the one
    mistake a containment check must not make.
    """
    try:
        Path(where).resolve().relative_to(Path(root).resolve())
    except ValueError:
        return False
    return True

# What this module can say about where a row was performed. "fixture" is narrower than the
# record own "temporary": a temporary destination whose build steps and relay are stand-ins.
# A committed row is never "host", and a check enforces it, because the one thing this suite
# must not be able to do is read as a host measurement nobody performed.
PROVENANCE = ("fixture", "temporary", "host")
COMMITTED_PROVENANCE = "fixture"

# The configuration keys carrying the model and the permission posture, read on both sides of an
# install. TaskSettings.REQUIRED names sandbox and approvalPolicy beside model, so a reading that
# watched only the model would miss the half deciding what a restored task may do.
MODEL_PERMISSION_KEYS = ("model", "approval_policy", "sandbox_mode")


def _row(cell):
    """The one declared row for this cell, looked up rather than respelled at each site."""
    for declared in DECLARED:
        if declared[0] == cell:
            return declared
    raise KeyError("no reading is declared for " + repr(cell))


def _unreadable(cell, source, path, detail):
    """A cell whose reading could not be made.

    A distinct answer, not a negative one. Returning False here, or falling through to whatever
    the neighbouring cell said, is the defect this module exists to refuse.
    """
    return {"cell": cell, "value": reading.UNREADABLE, "answeredBy": source,
            "readingPath": path, "readable": False, "detail": detail,
            "performedAgainst": COMMITTED_PROVENANCE}


def read(cell, payloads):
    """Fill one acceptance cell from the reading its own question called for, and nothing else.

    The only accessor. It takes the payload of the declared source, confirms the payload is the
    one that source produces, walks the declared path and returns what it found. Every way of
    failing to reach an answer returns UNREADABLE naming the reason, so a command that did not
    run and a command that answered are never the same row.
    """
    declared_cell, source, path, _producer = _row(cell)
    payload = payloads.get(source)
    if not isinstance(payload, dict):
        return _unreadable(declared_cell, source, path, "no payload was produced by " + source)
    if source in CLI_COMMANDS:
        if payload.get("command") != source:
            return _unreadable(declared_cell, source, path,
                               "the payload names " + repr(payload.get("command"))
                               + ", not " + repr(source))
    elif payload.get("source") != source:
        return _unreadable(declared_cell, source, path,
                           "the payload does not carry the " + repr(source) + " stamp")
    found = payload
    for key in path:
        if not isinstance(found, dict) or key not in found:
            return _unreadable(declared_cell, source, path, "the reading has no " + repr(key))
        found = found[key]
    if found == reading.UNREADABLE:
        # A producer that already said it could not read is not made readable by the fact that
        # its answer was where this table expected it. The path being reachable answers "was
        # there an answer here", which is a different question from "was the reading made", and
        # collapsing the two is how an unread row passes a readability check.
        return _unreadable(declared_cell, source, path,
                           str(payload.get("detail") or "the reading reported itself unreadable"))
    return {"cell": declared_cell, "value": found, "answeredBy": source, "readingPath": path,
            "readable": True, "performedAgainst": COMMITTED_PROVENANCE}


def table(payloads):
    """Every declared cell, each filled by its own reading."""
    return {cell: read(cell, payloads) for cell in SEVEN}


def _configuration_keys(raw):
    """The model and permission keys of a Codex configuration, or a refusal to approximate.

    tomllib arrives in 3.11 and the supported floor is 3.10, so on the older job there is no
    reader for this file. That is answered with a refusal rather than with a regular expression,
    for the reason the installer gives for the same refusal: a value guessed out of TOML is a
    value whose wrongness is invisible, and this row would then report a preservation nobody
    read. A second TOML parser living in a test is also the duplicate statement the runtime
    declines to make.
    """
    if not HAS_READER:
        return None, ("reading a configuration needs tomllib, which this interpreter does not"
                      " have, so preservation here is unread rather than established")
    try:
        parsed = tomllib.loads(raw)
    except (ValueError, TypeError) as error:
        return None, "the configuration could not be read: " + type(error).__name__
    return {key: parsed.get(key) for key in MODEL_PERMISSION_KEYS}, None


def _model_permission_delta(earlier, later):
    """Whether the keys carrying model and permission survived, read on both sides.

    This module own reading, and deliberately not one of the installer own.
    checks.settingsPreserved answers whether config.toml bytes were left alone by a command that
    writes nothing, and settings_usable answers whether a settings value a caller supplied is
    admissible to the relay. Neither looks at model or permission state before an install and
    again afterwards, so neither can say anything was preserved. Filling this cell from either
    would be the borrowed answer refused everywhere else here, so the absence of an installer
    cell for this question is reported as a gap instead of papered over with a nearby verdict.
    """
    was, refusal = _configuration_keys(earlier)
    if refusal is not None:
        return {"source": ACCEPTANCE, "preservation": reading.UNREADABLE, "detail": refusal}
    now, refusal = _configuration_keys(later)
    if refusal is not None:
        return {"source": ACCEPTANCE, "preservation": reading.UNREADABLE, "detail": refusal}
    return {"source": ACCEPTANCE,
            "preservation": PRESERVED if was == now else CHANGED,
            "read": {"earlier": was, "later": now}}


# The settings this composition installs over: the model and permission keys, a table belonging
# to somebody else, and the bridge table the fixture writes. The first two are what an installer
# has no business touching at all.
MODEL_PERMISSION_BLOCK = ('model = "a-model"\n'
                          'approval_policy = "never"\n'
                          'sandbox_mode = "read-only"\n')
FOREIGN_TABLE = '[mcp_servers.cxc]\ncommand = "/opt/cxc"\n\n[mcp_servers.cxc.env]\nA = "b"\n'

# A hook registration belonging to somebody else. Runtime hooks live in their own file, not in
# config.toml, and the update fixture snapshot does not read it -- so preservation of a hook
# setting has to be snapshotted here or it is not being checked at all.
FOREIGN_HOOKS = {
    "version": 1,
    "hooks": {"Stop": [{"hooks": [{"type": "command", "command": "/opt/cxc/stop", "timeout": 9}]}]},
}


def hooks_path(host):
    return host.codex_home / "hooks.json"


def seed_settings(host):
    """Put somebody else settings in front of the installer, and return what must survive."""
    raw = (MODEL_PERMISSION_BLOCK + "\n" + FOREIGN_TABLE + "\n"
           + host.config.read_text(encoding="utf-8"))
    host.config.write_text(raw, encoding="utf-8")
    hooks_path(host).write_text(json.dumps(FOREIGN_HOOKS, indent=2), encoding="utf-8")
    return settings_bytes(host)


def settings_bytes(host):
    """Every settings file an install must leave alone, read as bytes."""
    return {
        "config.toml": host.config.read_bytes(),
        "hooks.json": hooks_path(host).read_bytes() if hooks_path(host).is_file() else None,
    }


def clean(host):
    """Turn the fixture into a host that has nothing installed yet.

    The fixture exists to model an update, so it arrives with a selected runtime, a pointer and a
    recorded install. A new install is the stage before all of that. The store file stays where
    it is: snapshot() reads it to prove the rows survived, and an absent store is reported by the
    readings rather than produced by deleting the evidence the later stages assert on.
    """
    shutil.rmtree(host.previous, ignore_errors=True)
    record = hostrecord.load(host.record_path, host.data["definitionVersion"]).value or {}
    record["selected"] = {}
    record.pop("pointer", None)
    for name in list(record.get("components", {})):
        record["components"][name]["installs"] = []
    hostrecord.save(host.record_path, record)
    if host.pointer_path.is_symlink() or host.pointer_path.exists():
        host.pointer_path.unlink()


def arriving_source(host):
    """A source change: new digests, so the next install builds at a directory of its own.

    Standing in for a source change, which would give a different directory name for the same
    reason. The digests move on the fixture own copy of the definition because the update
    fixture derives its measured digests from that copy too; bumping only what load() returns
    would make the run refuse for a digest disagreement, which is a refusal about the fixture
    rather than about the boundary under test.
    """
    for component in host.data["components"]:
        component["sourceDigest"] = hashlib.sha256(
            ("arrived-" + component["sourceDigest"]).encode()).hexdigest()
    combined = hashlib.sha256(
        "".join(c["sourceDigest"] for c in host.data["components"]).encode()).hexdigest()[:12]
    host.candidate = host.destination / (
        "env-" + str(host.data["definitionVersion"]) + "-" + combined)
    return host.candidate


def updating(host):
    """The definition the arriving source would present, for the length of one run."""
    return (mock.patch.object(runtime_install.definition, "load", return_value=host.data),
            mock.patch.object(runtime_install.definition, "verify", return_value=[]))


def install(host, **injected):
    """One install run against this host, driven the way the update tests drive it."""
    return base.UpdateRecoveryTests()._run(host, **injected)


def isolated(root, host=None):
    """An environment whose homes, state roots and PATH all sit inside this directory.

    Pointing --state at a temporary path is not isolation. The survey runs its own discovery
    without the supplied state, filesystem discovery reads HOME and XDG_STATE_HOME, and an
    installed relay on PATH would be resolved and run. So the child gets homes of its own and a
    PATH that cannot reach an installed entry point, and a check afterwards confirms the paths
    it reported are under this root.
    """
    home = root / "home"
    home.mkdir(exist_ok=True)
    state_root = root / "xdg-state"
    state_root.mkdir(exist_ok=True)
    return {
        "HOME": str(home),
        "XDG_STATE_HOME": str(state_root),
        "CODEX_HOME": str(root / "codex"),
        # The supplied runtime first, then enough to run git. Never enough to reach an
        # installed entry point of this host's own.
        "PATH": ((str(host.candidate / "bin") + ":") if host is not None else "") + "/usr/bin:/bin",
        # An inherited import path is the other way in. Without these the child imports whatever
        # the host has installed, and the resolved-location reading would then be about this
        # machine rather than about the destination under test.
        "PYTHONPATH": "",
        "PYTHONNOUSERSITE": "1",
    }


def answering_relay(directory):
    """A relay whose doctor reports a socket it reached, so the connection cell can move.

    The connection question is the one with no input of its own on the command line: it is
    answered by what the relay says. Without a relay that answers, the cell sits at the same
    value in every direction, and a cell that never moves cannot catch a neighbour copying
    into it -- which is how one reading ends up standing in for another without any case
    noticing.
    """
    path = Path(directory) / "answering-relay"
    path.write_text(
        "#!/usr/bin/env python3\n"
        "import json\n"
        "print(json.dumps({'actorReachability': {'socketConnect': 'ok'}}))\n",
        encoding="utf-8")
    path.chmod(path.stat().st_mode | stat.S_IEXEC | stat.S_IRWXU)
    return path

def standin_relay(directory, answer):
    """A relay that answers the guard, so the hook has something real to call."""
    path = Path(directory) / "codex-session-relay"
    path.write_text(
        "#!/usr/bin/env python3\n"
        "import sys\n"
        "sys.stdin.buffer.read()\n"
        "sys.stdout.write(" + repr(json.dumps(answer)) + ")\n",
        encoding="utf-8")
    path.chmod(path.stat().st_mode | stat.S_IEXEC | stat.S_IRWXU)
    return path


# The Stop payload the host was observed to deliver, forwarded as it arrives.
STOP = {
    "cwd": "/tmp/workspace",
    "hook_event_name": "Stop",
    "last_assistant_message": "I finished the task.",
    "model": "test-model",
    "permission_mode": "default",
    "session_id": "01a0b109-1ea5-7fb3-9adc-87f45ed83688",
    "stop_hook_active": False,
    "transcript_path": "/tmp/transcript.jsonl",
    "turn_id": "turn-1",
}

RELEASED = {"decision": "release", "state": "unmanaged", "observation": "unmanaged",
            "reason": "No marker names this workspace.", "assignmentId": None,
            "counters": {}, "recordedAs": None, "hook_output": {}}


def fire_the_hook(directory):
    """Fire the hook the way the host does, through the command the registration names.

    Calling completion.run() here would have been a reading of this checkout. The row says an
    installed hook fired, and the installed hook is a COMMAND LINE in hooks.json: an interpreter,
    the entry point, and the settings path the install chose. Run the helper directly and that
    whole registration is untested -- a broken entry point, a settings argument pointing
    somewhere else, a stdin or stdout contract that changed, and the row still reaches "1".

    So the hook is installed by the installer, the command is DERIVED from the file it wrote
    rather than written out again here, and that command is executed as a program with the Stop
    payload on its stdin. What the row then reads is a journal entry the registered command
    produced.
    """
    directory = Path(directory)
    standin_relay(directory, RELEASED)
    arguments = base.argparse.Namespace(
        codex_home=str(directory), event=None, hook_command=None, adapter="completion",
        dest=None, relay_command=str(directory / "codex-session-relay"),
        marker_root=str(directory / "marker"), db_path=None,
        journal_root=str(directory / "journal"), python=sys.executable,
        mode=completion.OBSERVE, guard_timeout=5, timeout=10, issue="CRW-69", apply=True)
    with mock.patch.object(runtime_install, "emit"):
        runtime_install.cmd_hook(arguments)

    registered = completion.adapter_entries(
        hooks.read(directory / "hooks.json").value or {}, completion.EVENT)
    before = completion.status(codex_home=str(directory), environ={})
    done = subprocess.run(
        shlex.split(registered[0]["command"]) if registered else ["false"],
        input=json.dumps(STOP).encode("utf-8"), capture_output=True, timeout=120,
        env={**os.environ, "CODEX_HOME": str(directory)})
    after = completion.status(codex_home=str(directory), environ={})
    return before, after, registered, done


# Every boundary an update crosses, as the update tests enumerate them: two build steps, four
# gate answers and two pointer steps. The composition runs the whole sequence once per boundary,
# because "the previous install came back" is a claim about each way of failing, not about one.
BOUNDARIES = (
    ("create environment", None),
    ("install packages", None),
    (None, "running daemon"),
    (None, "handover in flight"),
    (None, "unreadable daemon"),
    (None, "store would be downgraded"),
    ("replace the owned pointer", None),
    ("read the owned pointer back", None),
)

GATE_REFUSAL = "read whether it is safe to swap"


# The answer each acceptance row reads when the thing it judges actually worked. Declared, and
# then required: a row that can never reach its success answer is a row whose assertion accepts
# its own failure, and an install acceptance suite that passes when nothing installed is worse
# than none, because the next person believes the ground is covered.
SUCCESS_ANSWERS = {
    # A listing answers in its own shape, so its success is stated in that shape: the check ran,
    # nothing is missing and nothing conflicts.
    "skillLink": {"exitCode": 0, "missing": [], "conflict": []},
    "runtimeImport": "verified",
    "mcpToolExposure": "verified",
    "appServerConnection": "verified",
    "hookCallback": "1",
    "modelPermissionPreservation": PRESERVED,
    "deliveryAcceptance": "verified",
}

# Every answer that means a question was not settled. Named so the check below can derive the
# places that REQUIRE one, rather than recognising assertions by their shape -- a sweep over
# shapes misses a refusal reached through a helper, and it was a shape sweep that missed twice.
REFUSAL_ANSWERS = ("not_verified", "unknown", "not_applicable", reading.UNREADABLE,
                   reading.ABSENT, reading.ACCESS_ERROR, CHANGED,
                   completion.NOT_READ, completion.NO_JOURNAL)

# The places that require a refusal, and what makes the refusal the right answer there. None of
# them is an acceptance row accepting its own failure: every one of the seven is separately
# required to read its success answer. A new place that settles for a refusal has to say which
# of these it is.
#
# The check derives the set from this file, and it does it by accounting rather than by
# recognising. refusal_spellings asks the objects for every way a refusal can be written -- the
# answers themselves, an attribute an imported module binds to one, a global here bound to one,
# a collection containing one -- and every occurrence of any of them has to land in this map or
# in NAMES_A_REFUSAL_WITHOUT_SETTLING. An assertNotEqual against a refusal is accounted like any
# other occurrence; which side of the question it settles is a judgement the reason below carries
# rather than something the derivation decides for itself.
ACCEPTS_A_REFUSAL = {
    "test_a_daemon_that_could_not_be_read_is_neither_running_nor_stopped":
        "the refusal IS the question: a daemon that could not be read is a third answer, and"
        " collapsing it into running or stopped is the defect.",
    "test_a_missing_registration_and_a_claimed_one_are_not_the_same_answer":
        "absence is the answer being distinguished from a conflict.",
    "test_an_install_that_succeeded_is_not_reported_as_an_activation":
        "not_verified is the correct answer and the criterion: installation is not activation,"
        " so this row succeeds by never reading verified.",
    "test_changing_one_condition_moves_only_the_reading_that_asked_about_it":
        "the refusals are the BEFORE ends of transitions that must finish at verified.",
    "test_every_declared_path_is_one_its_source_actually_writes":
        "the refusal is what proves the walk is load-bearing: the key is removed and the row"
        " has to go unreadable.",
    "test_every_row_is_readable_and_answers_in_its_own_vocabulary":
        "only on the interpreter with no configuration reader, where the row must say it could"
        " not take the reading and name why.",
    "test_the_hook_row_counts_an_invocation_that_really_happened":
        "absence is the BEFORE end: the claim is the transition to exactly one invocation.",
    "test_the_model_and_permission_row_is_not_taken_from_a_neighbouring_verdict":
        "changed is the correct answer to a posture that moved, and unreadable only where there"
        " is no reader.",
    "test_withholding_a_source_leaves_only_its_own_rows_unreadable":
        "the refusal is the property: a command that did not run must not read as an answer.",
    "test_without_a_reader_the_row_reports_itself_unread_rather_than_preserved":
        "the property is that an unread row says so instead of reading as preserved.",
}

# Places that name a refusal without settling for one. The negative rule over-collects, and this
# is where the over-collection is paid for: a producer of a refusal is not a place accepting one.
NAMES_A_REFUSAL_WITHOUT_SETTLING = {
    "_unreadable":
        "it MAKES the refusal. A cell whose reading could not be taken is what this returns,"
        " and returning it is the opposite of settling for one somebody else returned.",
    "read":
        "the accessor, which passes a producer's refusal through unchanged rather than"
        " accepting it as an answer to the question the row asked.",
    "_model_permission_delta":
        "the producer of this module's own reading: with no configuration reader it answers"
        " unreadable, and changed is its verdict about a posture that moved.",
    "refusal_spellings":
        "it derives the spellings from REFUSAL_ANSWERS. Naming the set is how it asks the"
        " question, not an answer it accepted.",
    "test_a_place_that_settles_for_a_refusal_in_a_form_the_old_sweep_missed_is_still_caught":
        "it plants a refusal in a synthetic file for the check to find; what this case reads"
        " is whether the check failed, not what the planted place asserted.",
}

# Module-level statements naming a refusal, keyed by what the statement binds.
REFUSAL_AT_MODULE_LEVEL = {
    "CHANGED": "the constant itself, which is one of the answers",
    "REFUSAL_ANSWERS": "the declaration of the answers the derivation starts from",
    "REFUSAL_REACH_INCLUDES":
        "it names the spellings that have to keep being derived. Three of those names are"
        " themselves answers, because a vocabulary that spells a constant the way it reads is"
        " the ordinary case rather than a coincidence.",
}

# What this derivation cannot see, as data. Each form is planted by a control that requires the
# derivation NOT to see it, so widening the derivation to cover one makes its control fail.
REFUSAL_OUT_OF_REACH = {
    "a refusal compared as data, never spelled":
        ("self.assertEqual(rows[cell][\"value\"], baseline[cell][\"value\"])",
         "both sides are read at run time and neither names an answer. Attributing this would"
         " mean attributing read() to every acceptance row, and an inventory that names every"
         " row has stopped distinguishing anything."),
    "a refusal reached through a callable this text does not name":
        ("self.assertEqual(row, handlers[key]())",
         "the same end of the same flow: the derivation follows names, and a helper taken out of"
         " a container or a registry is chosen while the suite runs."),
    "a refusal reached by its name as a string":
        ("getattr(completion, \"NOT_READ\")",
         "the name is a string, so there is no attribute for the sweep to account."),
    "a refusal a helper in another module asserts":
        ("base.expect_unreadable(row)",
         "no spelling appears in this file at all. A helper defined HERE is accounted, and its"
         " callers with it when it hands the refusal back rather than a container holding one."),
}

# The spellings this module actually relies on. Not the reach -- the reach is derived and may go
# further, which is not a failure. This is the floor: a kind that quietly stopped resolving, an
# import that moved or a constant that was renamed, loses one of these and fails. Occurrence
# counts alone would not catch it, because a spelling nobody writes has no occurrence to lose.
REFUSAL_REACH_INCLUDES = {
    "module attribute": ("UNREADABLE", "ABSENT", "ACCESS_ERROR", "NOT_READ", "NO_JOURNAL"),
    "own global": ("CHANGED",),
    "collection": ("REFUSAL_ANSWERS", "VALUES"),
}

# Which path each reading actually travels: the thing this run installed or registered, or a
# stand-in this suite supplied. Reaching a success answer and reaching it down the path the row
# names are two questions, and the second is the one a source-level shortcut passes silently.
# Declared per row so a reading that quietly moves onto a shortcut has to move a sentence too.
READING_PATHS = {
    "skillLink":
        "installed, on the run's own Codex home: the links this run created under it, listed"
        " by the repository check the diagnosis itself runs.",
    "runtimeImport":
        "installed, on the run's own destination: the recorded interpreter imports the packages"
        " copied into the candidate, and the success case requires the resolved locations to be"
        " inside it.",
    "mcpToolExposure":
        "installed, on the run's own Codex home: the registration register-mcp --apply wrote"
        " into it, compared with the tool names supplied. The tool list is an input; on a host"
        " it comes from a session that listed them, which the procedure says.",
    "appServerConnection":
        "stand-in, on the run's own state directory: a relay executable this suite wrote"
        " answers the doctor about that state. No App Server runs here, and the claim is"
        " narrowed to match -- the row is fixture, and the procedure states that a live socket"
        " is what a host reading needs.",
    "hookCallback":
        "registered, on the run's own Codex home: the command line in the hook file the"
        " installer wrote THERE, read back out of that file and executed as a program with the"
        " Stop payload on its stdin. Calling the adapter helper directly would have left the"
        " entry point, the settings argument and the stdin contract untested; firing into a"
        " Codex home of its own would have answered from a machine the other six never saw.",
    "modelPermissionPreservation":
        "installed, on the run's own Codex home: the configuration the install acted over,"
        " read on both sides of it.",
    "deliveryAcceptance":
        "stand-in, on the run's own state directory: a relay executable this suite wrote"
        " carries the eight steps against that state, while the preflight asks the relay"
        " predicate imported from the copy inside the candidate. No live relay completes a"
        " delivery here, and the row is fixture for that reason.",
}
# Rows whose success answer cannot be required on every supported interpreter, with the reason.
# The exception exists because absence is a real state here and the answer set can say so; it
# is not a place to file a row that simply never succeeds.
CONDITIONAL_SUCCESS = {
    "modelPermissionPreservation":
        "reading a configuration needs tomllib, which arrives in 3.11, and the supported floor",
    "mcpToolExposure":
        "exposure is verified by comparing the REGISTERED command with the tools observed, and"
        " the registration lives in the configuration -- so on the interpreter with no reader"
        " for it this row cannot reach verified either, for the same one reason",
}


def succeeding(root):
    """One run in which every one of the seven readings has something that worked to read.

    The skills are really linked, the registration really names the command the diagnosis is
    asked about, both components really import from this checkout, the relay really answers the
    doctor and every step of a trial, and the hook has really fired. Nothing here is a fixture
    standing in for an answer: the stand-ins are the things being asked, and what they answer is
    what the readings report.
    """
    host = base._Host(root)
    clean(host)
    bridge = base.component_of_for_test(host.data, runtime_install.MCP_NAME)
    registered = str(host.pointer_path / "bin" / bridge["consoleScript"])
    # The fixture arrives with the bridge already registered, which would let the exposure row
    # read verified while register-mcp wrote nothing at all. The table is taken away so the
    # registration this run performs is the one the diagnosis compares against.
    host.config.write_text("", encoding="utf-8")
    seed_settings(host)
    earlier = host.config.read_text(encoding="utf-8")
    install(host)
    later = host.config.read_text(encoding="utf-8")

    linked_skills(host)
    # The supplied runtime first: it writes the console scripts the entry points resolve to and
    # records the interpreter, and only then is that interpreter given a path it can import from.
    stand_in_runtime(host)
    sources = importable_runtime(host, root, paths=installed_components(host))
    registration = base.run("register-mcp", "--codex-home", str(host.codex_home),
                            "--bridge-command", registered, "--apply")

    # The SAME Codex home the runtime was installed into, the skills were linked into and the
    # registration was written into. Firing into a second home of its own would have answered
    # this row from a clean hook file with no relation to the host the other six readings
    # describe -- a composition of readings about two different machines is not a composition.
    _before, after, _registered, _done = fire_the_hook(host.codex_home)

    return {
        "diagnose": diagnose_for(
            root, host,
            "--bridge-command", registered, "--observed-tool", "get_capabilities",
            "--relay-command", str(delivering_relay(root)),
            "--trial", *trial_inputs(root),
            import_path=sources),
        "hook-status": after,
        ACCEPTANCE: _model_permission_delta(earlier, later),
    }, host, registration

class ComposedLifecycleTests(unittest.TestCase):
    """One destination, four stages, and the install that has to come back at the end of them.

    The update tests already inject a failure at every boundary and prove that what was there
    before is still there afterwards. What they cannot prove is that the runtime which survived
    is one this command installed: their previous installation is a directory the fixture places
    on disk and selects, never promoted by the command under test. An installer that had lost
    the ability to install would leave every one of those tests green.

    So stage one installs it, stage two proves a repeat changes nothing, stage three fails an
    update to an arriving source, and stage four asserts that the environment recovered is the
    one stage one promoted -- then installs again, because a host that is merely not broken is
    not the same as a host that still works.
    """

    def _fired(self, label, breaking, gate, failure, arriving):
        """The seam named actually refused, on the candidate it was supposed to be replacing.

        Without this the sequence could fail before it reached the seam, or fail against the
        wrong environment, and every surviving-state assertion would still pass.
        """
        self.assertEqual(failure.get("environment"), str(arriving),
                         label + ": the run must be acting on the arriving source")
        if breaking:
            self.assertEqual(failure.get("failedStep"), breaking,
                             label + ": a reader must not have to infer where it stopped")
        else:
            self.assertEqual(failure.get("failedStep"), GATE_REFUSAL, label)
            verdict = (failure.get("swapGate") or {}).get("verdict")
            self.assertIn(verdict, (swapgate.BLOCKED, swapgate.UNESTABLISHED), label)

    def test_a_new_install_a_rerun_a_failed_update_and_the_recovery_of_what_it_replaced(self):
        for breaking, gate in BOUNDARIES:
            label = breaking or gate
            with self.subTest(label):
                with tempfile.TemporaryDirectory() as temporary:
                    host = base._Host(temporary)
                    clean(host)
                    settings = seed_settings(host)

                    # Stage one: a new install, onto a destination that has nothing in it.
                    empty = sorted(p.name for p in host.destination.iterdir())
                    new, new_payload = install(host)
                    installed = host.candidate
                    settled = host.snapshot()
                    reached = pointer.read(host.pointer_path).get("target")
                    record = hostrecord.load(host.record_path,
                                             host.data["definitionVersion"]).value or {}
                    selected = record.get("selected") or {}
                    claimed = (staging.read_claim(installed).value or {}).get("state")
                    after_new = sorted(p.name for p in host.destination.iterdir())

                    # Stage two: the same run again, which must build and write nothing.
                    repeat, repeat_payload = install(host)
                    after_repeat = host.snapshot()
                    claim = (staging.read_claim(installed).value or {}).get("state")
                    after_again = sorted(p.name for p in host.destination.iterdir())

                    # Stage three: a source arrives and the update to it fails at this seam.
                    arriving = arriving_source(host)
                    loaded, verified = updating(host)
                    with loaded, verified:
                        failed, failure = install(host, breaking=breaking, gate=gate)
                    recovered = host.snapshot()
                    still_reaches = pointer.read(host.pointer_path).get("target")
                    left_behind = sorted(p.name for p in host.destination.iterdir())

                    # Stage four: the host is asked to install the source it already has.
                    host.data = definition.load()
                    host.candidate = installed
                    usable, usable_payload = install(host)
                    settings_now = settings_bytes(host)

                self.assertEqual(empty, [], label + ": the fixture was cleaned first")
                self.assertEqual(new, 0, json.dumps(new_payload)[:1200])
                self.assertTrue(new_payload["promoted"], label + ": stage one must install")
                self.assertEqual(reached, str(installed),
                                 label + ": the pointer reaches what stage one promoted")
                self.assertTrue(selected, label + ": a new install records what it selected")
                self.assertTrue(all(str(installed) in str(where) for where in selected.values()),
                                label + ": and what it selected is what it built")
                self.assertEqual(claimed, staging.COMPLETE,
                                 label + ": the claim is settled by the install that made it,"
                                 " not first read after a later run")

                self.assertEqual(repeat, 0, json.dumps(repeat_payload)[:1200])
                self.assertTrue(repeat_payload["alreadyInstalled"],
                                label + ": a repeat of a settled install installs nothing")
                self.assertEqual(repeat_payload["stagingDecision"], staging.SETTLED, label)
                self.assertEqual(claim, staging.COMPLETE, label)
                self.assertEqual(after_repeat, settled,
                                 label + ": and it changes nothing that was there")
                self.assertEqual(after_again, after_new,
                                 label + ": a repeat leaves no directory behind either, which"
                                 " the state snapshot does not inventory")

                self.assertNotEqual(arriving, installed,
                                    label + ": the arriving source must build somewhere else,"
                                    " or stage three is not an update at all")
                self.assertEqual(failed, 1, label + ": a failed update must not report success")
                self._fired(label, breaking, gate, failure, arriving)

                self.assertEqual(recovered, settled,
                                 label + ": everything the update found is still as it found it")
                self.assertEqual(still_reaches, str(installed),
                                 label + ": the runtime recovered is the one stage one installed,"
                                 " not a directory the fixture placed")
                self.assertNotEqual(still_reaches, str(host.previous), label)
                self.assertNotIn(arriving.name, left_behind,
                                 label + ": the candidate it could not promote is not left in"
                                 " the destination")
                self.assertTrue(failure.get("retriable"),
                                label + ": a host that kept everything can try again")
                residual = failure.get("residualPaths") or []
                self.assertNotIn(str(arriving), residual,
                                 label + ": the candidate it could not promote is not left for"
                                 " an operator to clear")
                self.assertNotIn(str(installed), residual,
                                 label + ": and neither is the install that has to keep working")
                self.assertEqual(settings_now, settings,
                                 label + ": the settings it was handed are byte-identical")

                self.assertEqual(usable, 0, json.dumps(usable_payload)[:1200])
                self.assertTrue(usable_payload["alreadyInstalled"],
                                label + ": after the recovery the host still works")

    def test_the_rows_seeded_before_the_first_install_survive_every_stage(self):
        """The store is the half an update can lose, so it is read at the end, not asserted at."""
        with tempfile.TemporaryDirectory() as temporary:
            host = base._Host(temporary)
            clean(host)
            opening = host.snapshot()
            install(host)
            arriving_source(host)
            loaded, verified = updating(host)
            with loaded, verified:
                install(host, breaking="install packages")
            closing = host.snapshot()

        self.assertEqual(closing["storeRows"], opening["storeRows"])
        self.assertEqual(closing["storeInode"], opening["storeInode"],
                         "the store was not moved or recreated")
        self.assertEqual(closing["storeBytes"], opening["storeBytes"])

    def test_the_store_the_run_is_told_about_is_the_store_the_fixture_built(self):
        """What the run was told, read back and compared with what this scenario put on disk.

        Every install above used to hand the fixture a switch reporting the store absent and its
        tables unknown, while the fixture had built a populated one at that same path. Nothing
        failed: a run told there is no store settles the store cell as established absence and
        moves on, so a regression that detected a store and then lost it stayed green underneath
        -- the run under it had been told there was nothing to lose.

        The switch is refused now. This is the reading that keeps it refused rather than merely
        deleted: the cells the run took are read back out of its own payload and compared with
        the store this scenario built, and the two of them answer from different readings -- the
        schema comparison, and the doctor the in-flight count came from.
        """
        with tempfile.TemporaryDirectory() as temporary:
            host = base._Host(temporary)
            clean(host)
            code, payload = install(host)
            built = host.snapshot()
            where = str(host.store)
            cells = (payload.get("swapGate") or {}).get("cells") or {}

        self.assertEqual(code, 0, json.dumps(payload)[:800])
        self.assertTrue(built["storeRows"],
                        "the fixture built a store with nothing in it, so there is nothing here"
                        " a run could have been told the wrong thing about")

        tables = cells["storeTables"]
        self.assertTrue(tables["readable"], str(tables.get("detail")))
        self.assertEqual(tables["answer"], swapgate.AGREES,
                         "the run read " + repr(tables["answer"]) + " about a store holding "
                         + repr(built["storeRows"]) + ", so what it was handed and what this"
                         " scenario built are two different stores")
        self.assertEqual((tables["evidence"] or {}).get("dbPath"), where,
                         "and the reading it took names a store somewhere else")

        self.assertEqual(cells["inFlight"]["command"], ["doctor"],
                         "the in-flight count came from the presence probe rather than from the"
                         " doctor, which is what it answers with when it was told no store"
                         " exists to have an open attempt in")

    def test_a_failed_update_that_never_reached_its_seam_is_not_a_recovery(self):
        """The guard on the guard.

        Every recovery assertion is an equality between two readings of the same state, and a run
        that refused before it touched anything satisfies all of them. So the sequence also
        requires the seam to have named itself and the arriving environment, and this case states
        that requirement directly: a refusal that never reached the seam names a different step.
        """
        with tempfile.TemporaryDirectory() as temporary:
            host = base._Host(temporary)
            clean(host)
            install(host)
            arriving = arriving_source(host)
            loaded, verified = updating(host)
            with loaded, verified:
                code, payload = install(host, breaking="create environment")

        self.assertEqual(code, 1)
        self.assertEqual(payload.get("failedStep"), "create environment")
        self.assertEqual(payload.get("environment"), str(arriving),
                         "the refusal names the environment it was building, so a refusal about"
                         " something else cannot be read as this one")

    def test_a_restoration_it_could_not_read_back_is_reported_rather_than_claimed(self):
        """The pointer seam, where the disk and the answer are allowed to disagree.

        The injected failure lands the replacement and then raises, which is the case the
        rollback exists for. The restoration puts the previous target back the same way and is
        stopped the same way, so the link on disk reaches the stage one install while the command
        could not read that back. It reports a residual pointer instead of claiming a
        restoration, and that is the honest direction: a host reading this result is told to look
        at the link rather than told it is fine.
        """
        with tempfile.TemporaryDirectory() as temporary:
            host = base._Host(temporary)
            clean(host)
            install(host)
            installed = host.candidate
            arriving_source(host)
            loaded, verified = updating(host)
            with loaded, verified:
                code, payload = install(host, breaking="replace the owned pointer")
            reached = pointer.read(host.pointer_path).get("target")

        self.assertEqual(code, 1)
        restored = (payload.get("pointer") or {}).get("pointerRestored") or {}
        self.assertFalse(restored.get("verified"),
                         "the restoration could not be read back, so it is not claimed")
        self.assertEqual(restored.get("residualPointer"), str(host.pointer_path),
                         "and the path a reader has to look at is named")
        self.assertEqual(reached, str(installed),
                         "while on disk the link does reach the install that has to keep working")
        self.assertTrue(payload.get("retriable"),
                        "nothing was left in the destination, so the destination can be retried")


def stand_in_runtime(host):
    """An interpreter with nothing of this machine in it, and the record pointing at it.

    The probes resolve where a component lives by importing it, and the bridge exercise starts
    a server, both under whatever interpreter the record names. Left to fall back, that is the
    interpreter running this suite, and on a host with these packages installed the probe would
    import and run them. Clearing PYTHONPATH and the user site does not reach a system site
    directory, and detecting the host location afterwards is too late: the import already
    happened.

    So the runtime is supplied rather than discovered, the same way the relay cases hand the
    preflight a runtime that HAS the relay in it. This one is the inverse: -S -E, so no site
    directory and no inherited import path, and the record names it for every component. What
    the probes can reach is then a decision this suite made rather than a property of the
    machine it happened to run on.
    """
    binaries = host.candidate / "bin"
    binaries.mkdir(parents=True, exist_ok=True)
    interpreter = binaries / "python"
    interpreter.write_text(
        "#!/bin/sh\nexec \"" + sys.executable + "\" -S -E \"$@\"\n", encoding="utf-8")
    interpreter.chmod(interpreter.stat().st_mode | stat.S_IEXEC | stat.S_IRWXU)

    record = hostrecord.load(host.record_path, host.data["definitionVersion"]).value or {}
    for component in host.data["components"]:
        script = binaries / component["consoleScript"]
        script.write_text("#!" + str(interpreter) + "\n", encoding="utf-8")
        script.chmod(script.stat().st_mode | stat.S_IEXEC | stat.S_IRWXU)
        for install in hostrecord.component(record, component["component"])["installs"]:
            install["interpreterPath"] = str(interpreter)
    hostrecord.save(host.record_path, record)
    return binaries, interpreter

def importable_runtime(host, root, paths=None):
    """A supplied runtime that CAN import the components, from inside the temporary directory.

    The import row is the one question whose answer nothing here was moving. A cell that holds
    the same value in every direction cannot catch a neighbour answering with its value, and it
    cannot tell a real reading from a command that stopped attempting one -- both look like
    not_verified forever.

    So the runtime is rebuilt to find two packages this test wrote, and nothing else. It keeps
    -S, so no site directory of this machine is reachable, and the only import path it is given
    is a directory under the temporary root. What moves is what can be imported, not where it
    is allowed to look.
    """
    if paths is None:
        stubs = root / "importable"
        for component in host.data["components"]:
            package = stubs / component["module"]
            package.mkdir(parents=True, exist_ok=True)
            (package / "__init__.py").write_text("", encoding="utf-8")
        paths = str(stubs)
    interpreter = host.candidate / "bin" / "python"
    interpreter.parent.mkdir(parents=True, exist_ok=True)
    interpreter.write_text(
        "#!/bin/sh\nexec \"" + sys.executable + "\" -S \"$@\"\n", encoding="utf-8")
    interpreter.chmod(interpreter.stat().st_mode | stat.S_IEXEC | stat.S_IRWXU)
    return paths

def diagnose_for(root, host, *extra, import_path=None):
    """Ask this host for a diagnosis, with whatever extra input the case is varying."""
    environment = dict(os.environ, **isolated(root, host))
    if import_path is not None:
        # Replacing, never appending: what the probe may import stays a decision of this suite.
        environment["PYTHONPATH"] = str(import_path)
    done = subprocess.run(
        [sys.executable, str(RUNTIME), "diagnose",
         "--codex-home", str(host.codex_home),
         "--dest", str(host.destination),
         "--record", str(host.record_path),
         "--state", str(host.state),
         "--socket", str(root / "no.sock"),
         # The supplied relay entry point, not a path that is missing: a console script that
         # does not resolve sends the probe back to whatever interpreter is running this suite,
         # which is the fallback this arrangement exists to avoid.
         "--relay-command", str(host.candidate / "bin" / "codex-session-relay"),
         "--temporary", *extra],
        capture_output=True, text=True, timeout=180,
        env=environment)
    return json.loads(done.stdout)


# Everything a trial sends, answered. A relay that refuses a step leaves the delivery row at
# not_verified, which is an honest answer to "did a delivery complete" and no answer at all to
# "can this command complete one".
TRIAL_ANSWERS = {
    "assignment-find": {},
    "register": {"relationshipId": "rel-1"},
    "settings-record": {},
    "generation-open": {"executionGeneration": 1},
    "generation-bind": {},
    "admit-turn": {},
    "emit": {"receipt": {"eventId": "event-1"}},
    "deliver": {"attempt": {"turnId": "turn-1"}},
    "doctor": {"contents": {"available": True, "openAttempts": 0},
               "actorReachability": {"socketConnect": "ok"}},
}

# What the relay own predicate requires of a recipient settings document, supplied in full so
# the preflight passes for the reason it exists rather than by being skipped.
RECIPIENT_SETTINGS = {"sandbox": {"type": "readOnly"}, "approvalPolicy": "never",
                      "cwd": "/tmp", "runtimeWorkspaceRoots": ["/tmp"], "model": "a-model",
                      "reasoningEffort": "low", "environments": {}}


def delivering_relay(root, *, doctor=True):
    """A relay that answers every step of a trial, so a delivery can actually complete.

    Its first line names a real interpreter inside the temporary directory. A console script
    whose shebang is a wrapper cannot be executed -- the kernel does not follow one shebang to
    another -- and one naming env answers the preflight with the literal two-word string, which
    is the trampoline shape the installer documents.
    """
    interpreter = root / "relay-python"
    if not interpreter.exists():
        interpreter.symlink_to(sys.executable)
    path = root / "delivering-relay"
    answers = dict(TRIAL_ANSWERS)
    if not doctor:
        # The connection cell asks the doctor and the delivery cell does not, so a relay used to
        # move delivery answers the trial and stays silent about reachability. Otherwise one
        # input would move two questions and neither could be said to answer on its own.
        answers["doctor"] = {"contents": {"available": True, "openAttempts": 0}}
    path.write_text(
        "#!" + str(interpreter) + "\n"
        "import json, sys\n"
        "answers = json.loads(" + repr(json.dumps(answers)) + ")\n"
        "step = next((a for a in sys.argv[1:] if a in answers), \"\")\n"
        "print(json.dumps(answers.get(step, {})))\n",
        encoding="utf-8")
    path.chmod(path.stat().st_mode | stat.S_IEXEC | stat.S_IRWXU)
    return path


def trial_inputs(root):
    """The required trial inputs, every one of them, written where the command can read them."""
    artifacts = root / "artifacts"
    artifacts.mkdir(exist_ok=True)
    artifact = artifacts / "result.txt"
    artifact.write_text("a deliverable this trial names", encoding="utf-8")
    settings = root / "recipient-settings.json"
    settings.write_text(json.dumps(RECIPIENT_SETTINGS), encoding="utf-8")
    return ["--issue", "CRW-69", "--parent-task", "parent", "--child-task", "child",
            "--recipient", "parent", "--artifact-root", str(artifacts),
            "--artifact", str(artifact), "--turn-thread", "child", "--turn-id", "turn-1",
            "--dispatch-turn-id", "anchor-1", "--recipient-settings", "@" + str(settings)]


def linked_skills(host):
    """Actually install the skill links, so the link row has something to read as linked."""
    done = subprocess.run(
        [sys.executable, str(ROOT / "scripts" / "install.py"), "--apply",
         "--dest", str(host.codex_home / "skills")],
        capture_output=True, text=True, timeout=120)
    return done


def installed_components(host):
    """Put the real packages INSIDE the candidate, so an import resolves in the installation.

    Pointing the import path at this checkout made the row read verified from source that was
    never installed: the build steps are stand-ins, so the candidate held neither package while
    the reading reported locations in the working tree. Source being available is not the
    question the row asks, and answering it that way is the substitution this module refuses.

    Copied rather than linked, and copied real rather than stubbed, because the trial preflight
    asks the relay own reader and predicate and a stub has neither.
    """
    site = host.candidate / "site"
    for component in definition.load()["components"]:
        target = site / component["module"]
        # Into the directory the build stand-in already made, not past it: it creates the module
        # directory empty, so a copy that skipped an existing path left a package with no module
        # in it and the import failed for a reason that had nothing to do with the question.
        shutil.copytree(ROOT / component["packageLocation"], target, dirs_exist_ok=True)
    return str(site)

def observe_all(root):
    """Every reading the criterion asks for, each taken from the source that answers it.

    One temporary host, installed once, then asked three separate questions in three separate
    ways. The point of gathering them here rather than in one payload is that no answer can be
    produced by another answer: the diagnosis never sees the hook, the hook never sees the
    configuration comparison, and the comparison never sees either.
    """
    host = base._Host(root)
    clean(host)
    seed_settings(host)
    earlier = host.config.read_text(encoding="utf-8")
    install(host)
    later = host.config.read_text(encoding="utf-8")

    stand_in_runtime(host)
    # Linked for real, so the listing row has a success to read here too rather than a baseline
    # of "every skill missing" that an assertion would then have to accept.
    linked_skills(host)

    # The SAME Codex home the runtime was installed into, the skills were linked into and the
    # registration was written into. Firing into a second home of its own would have answered
    # this row from a clean hook file with no relation to the host the other six readings
    # describe -- a composition of readings about two different machines is not a composition.
    _before, after, _registered, _done = fire_the_hook(host.codex_home)

    return {
        "diagnose": diagnose_for(root, host),
        "hook-status": after,
        ACCEPTANCE: _model_permission_delta(earlier, later),
    }, host


# ---------------------------------------------------------------------------
# What the two derived lists below can actually see, derived rather than remembered.
#
# Both checks used to recognise FORMS: one shape of ast.parse call, and three spellings of a
# refusal inside a call whose name begins with assert. A form nobody had thought of was a silent
# pass, and one of the three spellings was a hardcoded set of constant names -- the enumerated
# literal standing in for a derivation that this repository's evidence rule refuses. Four times
# now a derivation in this project has reached less far than the sentence describing it, and each
# time the argument for the sentence was that the set was derived. A derivation is only as
# complete as its predicate.
#
# So the rule here is the negative one. Derive every spelling of the declared thing by asking the
# objects, account for EVERY occurrence of one, and fail on an occurrence nobody wrote down. A
# form nobody anticipated is still an occurrence, so it arrives as a failure rather than as
# another review round. What the derivation still cannot reach is not argued away in a sentence:
# it is returned as data, declared, and each declared form carries a control that plants it and
# requires the derivation not to see it.
#
# Where the reader cannot tell, it errs in one direction on purpose. It follows names -- a handle,
# a called name, a name bound to one, an attribute a class binds -- and it does NOT follow flow.
# So a name that ever holds the thing is treated as holding it for that scope, even where a later
# line rebinds it. That is wrong in the direction where somebody has to write a sentence, and it
# is wrong that way so that it is never wrong in the direction where a place settles for a refusal
# and nobody is ever asked about it. Being precise about which binding a particular line saw would
# mean following flow, and a precise-looking answer this reader cannot actually justify is the
# thing this whole module exists to refuse.
#
# One limit does not fit that record, because it cannot be planted as a single line: a class is
# named by its own name, so two classes written with the same name in different function scopes
# answer to one key and the first found wins. Telling them apart would mean carrying the
# enclosing chain through the method index, the held-attribute table and the base table at once.
# It is the one place where this reader takes the first answer rather than all of them, and it is
# written down here rather than left to be discovered. That is what keeps the declaration from rotting: widening
# the derivation later makes the control for the form it now covers fail, which is the prompt to
# delete the entry rather than leave a limitation standing that stopped being true.

MODULE_LEVEL = "<module>"


def _dotted(node):
    """The name an expression spells, or None when it is not a plain dotted name."""
    if isinstance(node, ast.Name):
        return node.id
    if isinstance(node, ast.Attribute):
        base = _dotted(node.value)
        return base + "." + node.attr if base else None
    return None


def _places(tree):
    """Which place each node sits in, and which class, with MODULE_LEVEL for neither.

    The INNERMOST function, named by the chain of functions around it, because a nested helper
    and a lambda are places of their own: the check this replaces walked every FunctionDef and
    would have reported one, and attributing them to whatever encloses them would let a new
    conclusion arrive inside an already declared place and be absorbed. The chain is what keeps
    the names apart -- two helpers may both be called spelled -- while a top-level function or a
    method still answers to the plain name the declarations are keyed by. Classes are not part of
    the chain, so two classes with a method of the same name collide on purpose, and the pin
    below fails rather than letting one declaration speak for both.
    """
    found = {}

    def walk(node, chain, klass):
        for child in ast.iter_child_nodes(node):
            if isinstance(child, (ast.FunctionDef, ast.AsyncFunctionDef, ast.Lambda)):
                # A decorator, a default and an annotation are evaluated where the def is
                # written and not inside it, so they stay in the scope around it.
                args = child.args
                every = (args.posonlyargs + args.args + args.kwonlyargs
                         + ([args.vararg] if args.vararg else [])
                         + ([args.kwarg] if args.kwarg else []))
                outside = (list(getattr(child, "decorator_list", []))
                           + list(args.defaults)
                           + [value for value in args.kw_defaults if value]
                           + [arg.annotation for arg in every if arg.annotation]
                           + ([child.returns] if getattr(child, "returns", None) else []))
                for beside in outside:
                    found[id(beside)] = (".".join(chain) if chain else MODULE_LEVEL, klass)
                    walk(beside, chain, klass)
            inner, owner = chain, klass
            if isinstance(child, (ast.FunctionDef, ast.AsyncFunctionDef)):
                inner = chain + [child.name]
            elif isinstance(child, ast.Lambda):
                # By line, because two lambdas in one class body are two places and a single
                # name for both would make one of them answer for the other.
                inner = chain + ["<lambda@" + str(child.lineno) + ">"]
            elif isinstance(child, (ast.ListComp, ast.SetComp, ast.DictComp,
                                    ast.GeneratorExp)):
                # A walrus inside a comprehension binds in the scope AROUND it, which is the
                # one special case Python carved out of comprehension scoping.
                def walruses(node):
                    """The walruses this comprehension owns, not the ones a lambda in it does."""
                    for inner in ast.iter_child_nodes(node):
                        if isinstance(inner, (ast.Lambda, ast.FunctionDef,
                                              ast.AsyncFunctionDef)):
                            continue
                        if isinstance(inner, ast.NamedExpr):
                            yield inner
                        yield from walruses(inner)

                for inner in walruses(child):
                    found[id(inner)] = (".".join(chain) if chain else MODULE_LEVEL, klass)
                # A comprehension has a scope of its own on Python 3: its target shadows an
                # outer name INSIDE it and not after it. Its first iterable is the exception,
                # evaluated outside before that scope exists.
                if child.generators:
                    first = child.generators[0].iter
                    found[id(first)] = (".".join(chain) if chain else MODULE_LEVEL, klass)
                    walk(first, chain, klass)
                inner = chain + ["<comprehension@" + str(child.lineno) + ">"]
            elif isinstance(child, ast.ClassDef):
                owner = child.name
            # setdefault, because a default or a decorator was already placed in the scope
            # that evaluates it and walking into the def must not take it back.
            found.setdefault(id(child), (".".join(inner) if inner else MODULE_LEVEL, owner))
            walk(child, inner, owner)

    walk(tree, [], None)
    return found


def _module_statements(tree):
    """Each top-level statement, named by what it binds or by the call it performs.

    A module-level occurrence has no function to be attributed to, and a line number is a key
    that moves whenever anything above it moves. What does not move is what the statement is
    for, so the statement answers with the names it binds.
    """
    named = {}
    statements = []
    def spread(prefix, body):
        # A class body first, so a statement in it is named by the class and what it binds
        # rather than by the class statement that happens to enclose it. Nested classes carry
        # the whole chain, since Outer.Inner is how a reader spells one.
        for statement in body:
            if isinstance(statement, ast.ClassDef):
                spread((prefix + "." if prefix else "") + statement.name, statement.body)
            statements.append((prefix, statement))

    spread("", tree.body)
    for klass, statement in statements:
        bound = []
        if isinstance(statement, ast.Assign):
            for target in statement.targets:
                bound += [n.id for n in ast.walk(target) if isinstance(n, ast.Name)]
        elif isinstance(statement, ast.AnnAssign):
            bound += [n.id for n in ast.walk(statement.target) if isinstance(n, ast.Name)]
        elif isinstance(statement, ast.Expr) and isinstance(statement.value, ast.Call):
            bound = [_dotted(statement.value.func) or "a call"]
        if not bound and isinstance(statement, (ast.FunctionDef, ast.AsyncFunctionDef,
                                                ast.ClassDef)):
            # A def evaluates its decorators and defaults out here, so the statement answers
            # with the name it declares rather than with the kind of node it is.
            bound = [statement.name]
        name = ", ".join(bound) if bound else type(statement).__name__
        if klass:
            name = klass + "." + name
        for node in ast.walk(statement):
            named.setdefault(id(node), name)
    return named


def _bindings(node):
    """Each name a statement binds paired with what it is given, unpacking included.

    alias, other = carrier, str gives each name its own value; reading the tuple as one would
    give both names both values and lose which is which.
    """
    if isinstance(node, ast.NamedExpr):
        holders, answer = [node.target], node.value
    elif isinstance(node, (ast.Assign, ast.AnnAssign)) and node.value is not None:
        holders = node.targets if isinstance(node, ast.Assign) else [node.target]
        answer = node.value
    else:
        return []
    def pair(holder, value):
        if (isinstance(holder, (ast.Tuple, ast.List))
                and isinstance(value, (ast.Tuple, ast.List))
                and len(holder.elts) == len(value.elts)):
            # Nested, because other, (alias,) = str, (carrier,) unpacks twice.
            return [entry for inner, given in zip(holder.elts, value.elts)
                    for entry in pair(inner, given)]
        return [(holder, value)]

    return [entry for holder in holders for entry in pair(holder, answer)]


def _reachable(expression, spelled, klass, bound):
    """Whether the thing is reachable in this expression without entering a container.

    The distinction decides what a helper hands on. A helper returning the thing itself -- or a
    conditional, a boolean expression or a name bound to it -- hands it to its caller, and the
    caller is then a place nothing would otherwise account for. A helper returning a dict that
    happens to carry one does not: read() answers with a row whose value may be a refusal, and
    attributing that to every caller would attribute read() to every acceptance row, which would
    leave the inventory unable to distinguish anything.

    So this descends only where the expression's own value comes from: the branches of a
    conditional, the operands of a boolean, what an await waits for and what a named expression
    binds. It does not descend into a comparison or into a
    call's arguments, because mentioning the thing while answering a different question -- as
    value.suffix == ".py" does -- is not handing it back.
    """
    if spelled(expression, klass) is not None:
        return True
    if isinstance(expression, ast.Name) and expression.id in bound:
        return True
    if isinstance(expression, ast.IfExp):
        return (_reachable(expression.body, spelled, klass, bound)
                or _reachable(expression.orelse, spelled, klass, bound))
    if isinstance(expression, ast.BoolOp):
        return any(_reachable(value, spelled, klass, bound) for value in expression.values)
    if isinstance(expression, ast.Await):
        return _reachable(expression.value, spelled, klass, bound)
    if isinstance(expression, ast.NamedExpr):
        return _reachable(expression.value, spelled, klass, bound)
    return False


def _held_by_class(tree, spelled, over=None):
    """Attribute names a class binds the thing to, so the rest of that class can read one.

    A setUp binding self.unread to a refusal and a test asserting self.unread are one place
    spread over two methods, and the method that settles is the one with no spelling in it. The
    binding is derived rather than declared, so that shape arrives accounted.
    """
    places, held = _places(tree), dict(over or {})

    # A local name that holds the thing first, so setUp doing value = reading.UNREADABLE and then
    # self.unread = value is one binding in two steps rather than two unrelated lines.
    bound, spreading = {}, True
    while spreading:
        spreading = False
        for node in ast.walk(tree):
            function, klass = places.get(id(node), (MODULE_LEVEL, None))
            local = bound.setdefault(function, set())
            for named, value in _bindings(node):
                if not _reachable(value, spelled, klass, local):
                    continue
                if isinstance(named, ast.Name) and named.id not in local:
                    local.add(named.id)
                    spreading = True

    for node in ast.walk(tree):
        function, klass = places.get(id(node), (MODULE_LEVEL, None))
        for target, value in _bindings(node):
            through_class = (isinstance(target, ast.Attribute)
                             and _dotted(target.value) not in (None, "self", "cls"))
            if (klass is None and not through_class) or not _reachable(
                    value, spelled, klass, bound.get(function, set())):
                continue
            if isinstance(target, ast.Attribute):
                through = _dotted(target.value)
                # Example.unread = ... names the class as statically as self.unread does.
                owner = klass if through in ("self", "cls") else (
                    (through or "").rpartition(".")[2] or None)
                if owner is not None:
                    held.setdefault(owner, set()).add(target.attr)
            elif isinstance(target, ast.Name) and function == MODULE_LEVEL:
                held.setdefault(klass, set()).add(target.id)
    # An attribute declared on a base is held by everything under it, the way a method is.
    parents = {node.name: [(_dotted(base) or "").rpartition(".")[2] for base in node.bases]
               for node in ast.walk(tree) if isinstance(node, ast.ClassDef)}
    growing = True
    while growing:
        growing = False
        for klass, bases in parents.items():
            for base in bases:
                gained = held.get(base, set()) - held.get(klass, set())
                if gained:
                    held.setdefault(klass, set()).update(gained)
                    growing = True
    return held


def _hands_on(tree, spelled):
    """Functions that hand the thing to their callers, and the callers that take it.

    One hop is not enough: a helper calling a helper hands it on again, and stopping at the
    first hop would leave the second silent at one remove. So the callers are taken to a
    fixpoint, and each is reported at the line of the call that reached it.

    Two kinds of name resolution, because Python has two. A bare call is resolved outwards from
    the scope that made it, since a nested helper shadows a module-level one of the same name.
    A call on self is resolved against the plain names only: a nested helper inside a method
    never answers self.helper(), and treating it as though it did would send the call to
    something the interpreter would not reach. A name bound to a function is followed too,
    because alias = helper is an ordinary refactor and not a place to lose one.
    """
    places = _places(tree)
    parents = {node.name: [(_dotted(base) or "").rpartition(".")[2]
                           for base in node.bases]
               for node in ast.walk(tree) if isinstance(node, ast.ClassDef)}
    # Directly in a class body is what makes a def a method: a helper nested inside a method is
    # not one, and a class written inside a function still has methods.
    is_method = {id(inner) for node in ast.walk(tree) if isinstance(node, ast.ClassDef)
                 for inner in node.body
                 if isinstance(inner, (ast.FunctionDef, ast.AsyncFunctionDef))}
    defined, methods, plain, receivers = set(), {}, set(), {}
    for node in ast.walk(tree):
        if not isinstance(node, (ast.FunctionDef, ast.AsyncFunctionDef, ast.Lambda)):
            continue
        where, klass = places.get(id(node), (MODULE_LEVEL, None))
        defined.add(where)
        # A method is reached through an instance or its class, never as a bare name inside
        # another method, so it is kept out of the lexical lookup.
        if id(node) not in is_method:
            plain.add(where)
        # And the instance is whatever the first parameter is called: self is a convention.
        args = node.args
        first = (args.posonlyargs + args.args)[:1]
        if id(node) in is_method and first:
            receivers[where] = first[0].arg
        # Which class a method belongs to, because self.name reaches a method of THIS class and
        # not a module-level function or another class's method that happens to share the name.
        if id(node) in is_method:
            methods.setdefault((klass, where.rpartition(".")[2]), where)
    for node in ast.walk(tree):
        # carrier = lambda self: ... in a class body binds a method named carrier, and the
        # lambda's own place is what a call through it reaches.
        if not isinstance(node, (ast.Assign, ast.AnnAssign)) or node.value is None:
            continue
        if not isinstance(node.value, ast.Lambda):
            continue
        where, klass = places.get(id(node), (MODULE_LEVEL, None))
        if klass is None or where != MODULE_LEVEL:
            continue
        for target in (node.targets if isinstance(node, ast.Assign) else [node.target]):
            if isinstance(target, ast.Name):
                methods[(klass, target.id)] = places.get(id(node.value),
                                                         (MODULE_LEVEL, None))[0]

    class_bound = {}
    for node in ast.walk(tree):
        if not isinstance(node, ast.ClassDef):
            continue
        for statement in node.body:
            for target, _value in _bindings(statement):
                if isinstance(target, ast.Name):
                    class_bound.setdefault(node.name, set()).add(target.id)

    def instance(function):
        """How this scope spells its instance: whatever the first parameter is called.

        Looked up outwards, because a closure with no parameters of its own still sees the one
        the method around it was given.
        """
        chain = [] if function == MODULE_LEVEL else function.split(".")
        while chain:
            scope = ".".join(chain)
            spelled = receivers.get(scope)
            if spelled:
                return {spelled, "cls"}
            # A scope of its own that binds the same name is where the search stops: a nested
            # parameter called self is that function's, not the method's around it.
            taken_here = taken_names.get(scope, set())
            outer = receivers.get(".".join(chain[:-1])) if len(chain) > 1 else None
            if outer and outer in taken_here:
                return {"cls"}
            chain.pop()
        return {"cls"}

    def inherited(klass, named, seen=()):
        """The method this class reaches by that name, its own or one it inherits."""
        if klass is None or klass in seen:
            return None
        if (klass, named) in methods:
            return methods[(klass, named)]
        for base in parents.get(klass, ()):
            reached = inherited(base, named, tuple(seen) + (klass,))
            if reached:
                return reached
        return None

    def scopes(caller):
        """The scope this call sits in and every one enclosing it, innermost first."""
        chain = [] if caller == MODULE_LEVEL else caller.split(".")
        while chain:
            yield ".".join(chain)
            chain.pop()
        yield MODULE_LEVEL

    taken_names = {}
    for node in ast.walk(tree):
        if not isinstance(node, (ast.FunctionDef, ast.AsyncFunctionDef, ast.Lambda)):
            continue
        where = places.get(id(node), (MODULE_LEVEL, None))[0]
        args = node.args
        taken_names[where] = {arg.arg for arg in
                              (args.posonlyargs + args.args + args.kwonlyargs
                               + ([args.vararg] if args.vararg else [])
                               + ([args.kwarg] if args.kwarg else []))}
    # A name a scope assigns is local to that scope for the whole of it, whatever it is assigned.
    # An alias is looked up first, so this only stops the search where the binding is something
    # this reader cannot follow -- carrier = str -- which is a name Python resolves locally and
    # never to the enclosing definition.
    def binds(node):
        """Every name this statement binds locally, whatever construct does the binding."""
        targets = []
        if isinstance(node, ast.Assign):
            targets = list(node.targets)
        elif isinstance(node, (ast.AnnAssign, ast.AugAssign)):
            targets = [node.target]
        elif isinstance(node, (ast.For, ast.AsyncFor, ast.comprehension)):
            # A for target binds in the function; a comprehension target binds in the
            # comprehension, which is now a scope of its own, so both are collected here and
            # each lands in the place it belongs to.
            targets = [node.target]
        elif isinstance(node, ast.withitem):
            targets = [node.optional_vars] if node.optional_vars else []
        elif isinstance(node, ast.ExceptHandler):
            return {node.name} if node.name else set()
        elif isinstance(node, (ast.Import, ast.ImportFrom)):
            return {alias.asname or alias.name.split(".")[0] for alias in node.names}
        elif isinstance(node, ast.NamedExpr):
            targets = [node.target]
        elif isinstance(node, (ast.ClassDef, ast.FunctionDef, ast.AsyncFunctionDef)):
            # A class or a function defined in a scope takes that name in it.
            return {node.name}
        elif isinstance(node, (ast.MatchAs, ast.MatchStar)):
            return {node.name} if node.name else set()
        elif isinstance(node, ast.MatchMapping):
            return {node.rest} if node.rest else set()
        found = set()
        for target in targets:
            # Only what the target BINDS. mapping[carrier] = value and carrier.attr = value read
            # the name rather than binding it, and marking those local would stop the search
            # before a definition the call really does reach.
            found.update(inner.id for inner in ast.walk(target)
                         if isinstance(inner, ast.Name) and isinstance(inner.ctx, ast.Store))
        return found

    for node in ast.walk(tree):
        where = places.get(id(node), (MODULE_LEVEL, None))[0]
        if where == MODULE_LEVEL:
            continue
        bound_here = binds(node)
        if bound_here:
            taken_names.setdefault(where, set()).update(bound_here)
    for node in ast.walk(tree):
        # global and nonlocal say the name belongs to another scope, so assigning it here does
        # not make it this one's own and the search must not stop at it.
        if not isinstance(node, (ast.Global, ast.Nonlocal)):
            continue
        where = places.get(id(node), (MODULE_LEVEL, None))[0]
        taken_names.setdefault(where, set()).difference_update(node.names)

    declared_global, declared_nonlocal = {}, {}
    for node in ast.walk(tree):
        if not isinstance(node, (ast.Global, ast.Nonlocal)):
            continue
        where = places.get(id(node), (MODULE_LEVEL, None))[0]
        which = declared_global if isinstance(node, ast.Global) else declared_nonlocal
        which.setdefault(where, set()).update(node.names)

    def outwards(caller, named, aliases=None, klass=None):
        """Every place this name may reach, innermost scope first.

        A set, not one place. A name can be assigned twice and this reader does not decide which
        assignment a given call saw, so it answers with all of them; picking one would alternate
        forever between them, and picking the first would be a guess wearing a precise face.

        A parameter is different from a rebinding. Which object a parameter holds is a fact about
        the run, but that the name belongs to the parameter and not to a module-level function of
        the same name is a fact about the text, so the search stops there.
        """
        # A class body executes with the names it has already bound, so a default or a
        # decorator written there reaches a method defined above it, and an ordinary binding
        # made there shadows the module the same way a local one does.
        if klass is not None and named in class_bound.get(klass, ()):
            reached = methods.get((klass, named))
            return {reached} if reached else set()
        if named in declared_global.get(caller, ()):
            # global says the module, not the next scope out that happens to share the name,
            # and what the module holds under it may be an alias rather than a def.
            if aliases and named in aliases.get(MODULE_LEVEL, {}):
                return set(aliases[MODULE_LEVEL][named])
            return {named} if named in plain else set()
        outer = list(scopes(caller))
        if named in declared_nonlocal.get(caller, ()):
            # nonlocal says a scope AROUND this one, which is not the module either.
            outer = outer[1:]
        for scope in outer:
            if aliases and named in aliases.get(scope, {}):
                return set(aliases[scope][named])
            candidate = named if scope == MODULE_LEVEL else scope + "." + named
            if candidate in plain:
                return {candidate}
            if named in taken_names.get(scope, ()):
                return set()
        return set()

    # A name bound to a function, kept for the whole scope that binds it and visible to the
    # scopes inside it, the way a closure sees one. Chains are followed to a fixpoint, because
    # second = first = helper is two hops and stopping at one loses the second.
    #
    # It is never cleared. A name can be rebound, and where that happens this reader cannot say
    # which binding a given call reached without following flow it does not follow. So it answers
    # that the name still holds what it once held: the cost of being wrong is a sentence somebody
    # has to write, which is the direction this whole module errs in on purpose, rather than a
    # place that settles for a refusal and is never asked about it.
    aliases, growing = {}, True
    while growing:
        growing = False
        for node in ast.walk(tree):
            # A named expression binds a name as surely as an assignment does.
            if isinstance(node, ast.NamedExpr):
                holders, answer = [node.target], node.value
            elif isinstance(node, (ast.Assign, ast.AnnAssign)) and node.value is not None:
                holders = node.targets if isinstance(node, ast.Assign) else [node.target]
                answer = node.value
            else:
                continue
            function, _klass = places.get(id(node), (MODULE_LEVEL, None))
            def names(expression):
                """Every place an expression may name, both arms of a conditional included."""
                if isinstance(expression, ast.IfExp):
                    return names(expression.body) | names(expression.orelse)
                if isinstance(expression, ast.NamedExpr):
                    return names(expression.value)
                if isinstance(expression, ast.Name):
                    _where, in_class = places.get(id(node), (MODULE_LEVEL, None))
                    return outwards(function, expression.id, aliases, in_class)
                if isinstance(expression, ast.Await):
                    return names(expression.value)
                if isinstance(expression, ast.BoolOp):
                    return set().union(*(names(value) for value in expression.values))
                if isinstance(expression, ast.Lambda):
                    return {places.get(id(expression), (MODULE_LEVEL, None))[0]}
                if isinstance(expression, ast.Attribute):
                    through = _dotted(expression.value)
                    _where, klass = places.get(id(node), (MODULE_LEVEL, None))
                    if (through is None and isinstance(expression.value, ast.Call)
                            and _dotted(expression.value.func) == "super"):
                        for base in parents.get(klass, ()):
                            reached = inherited(base, expression.attr)
                            if reached:
                                return {reached}
                        return set()
                    reached = inherited(klass if through in instance(function)
                                        else (through or "").rpartition(".")[2] or None,
                                        expression.attr)
                    return {reached} if reached else set()
                return set()

            # One traversal for every shape a right-hand side can take: a name, a bound
            # method, a lambda, a conditional, a named expression, or one wrapped in another.
            # Unpacking is paired off first, so alias, other = carrier, str gives each name
            # what it is actually given rather than the union of both.
            for named, value in _bindings(node):
                targets = names(value)
                if not targets or not isinstance(named, ast.Name):
                    continue
                # global and nonlocal say which scope the name belongs to, so the alias is
                # recorded there rather than here, where the lookup would never consult it.
                holder_scope = function
                if named.id in declared_global.get(function, ()):
                    holder_scope = MODULE_LEVEL
                elif named.id in declared_nonlocal.get(function, ()):
                    # The nearest scope outside this one that binds the name, however many
                    # scopes down the declaration sits.
                    holder_scope = MODULE_LEVEL
                    outer_chain = function.split(".")[:-1]
                    while outer_chain:
                        candidate = ".".join(outer_chain)
                        if named.id in taken_names.get(candidate, ()):
                            holder_scope = candidate
                            break
                        outer_chain.pop()
                known = aliases.setdefault(holder_scope, {}).setdefault(named.id, set())
                if not targets <= known:
                    known |= targets
                    growing = True

    def called(node, function):
        """Every place this call may reach."""
        if isinstance(node.func, ast.Name):
            _where, in_class = places.get(id(node), (MODULE_LEVEL, None))
            return outwards(function, node.func.id, aliases, in_class)
        if isinstance(node.func, ast.Attribute):
            through = _dotted(node.func.value)
            if (through is None and isinstance(node.func.value, ast.Call)
                    and _dotted(node.func.value.func) == "super"):
                # super().name in a subclass starts the same lookup one class up, and which
                # class that is, is written right here.
                _where, klass = places.get(id(node), (MODULE_LEVEL, None))
                for base in parents.get(klass, ()):
                    reached = inherited(base, node.func.attr)
                    if reached:
                        return {reached}
                return set()
            # cls.name in a classmethod names a method of this class exactly as self.name does,
            # and Example.name names one of Example's just as statically.
            _where, klass = places.get(id(node), (MODULE_LEVEL, None))
            reached = inherited(klass if through in instance(function)
                                else (through or "").rpartition(".")[2] or None,
                                node.func.attr)
            return {reached} if reached else set()
        return set()

    # A function hands the thing back when its returned expression IS the thing, and it keeps
    # handing it back when a call that hands it back flows into that expression. Calling one
    # somewhere in the body is not enough: a helper answering {"value": _hands_refusal()} builds
    # a payload, and treating that as handing it on would drag in every caller of every payload
    # builder, which is the fan-out that gets a sweep deleted.
    #
    # The local names are re-read on every round rather than once, because a name bound to the
    # RESULT of a carrier only becomes reachable once that carrier is known. An assignment
    # replaces what a name holds instead of adding to it, so a name overwritten with an ordinary
    # value stops counting rather than staying marked for the rest of the function.
    carriers, growing = set(), True
    while growing:
        growing = False
        bound = {}

        def reaching(function, klass):
            def hands(expression, inner):
                if isinstance(expression, ast.Call):
                    reached = called(expression, function) & carriers
                    if reached:
                        return "through " + sorted(reached)[0]
                return spelled(expression, inner)
            return hands

        # A name that ever holds the thing in a function holds it for that function. Which
        # binding a particular return saw is a question about flow, and following flow is what
        # this reader does not do: a conditional return before an overwrite, and two arms of an
        # if, are both cases where a precise-looking answer would be a guess. Erring towards
        # holding it costs a sentence; erring the other way costs a place nobody is asked about.
        spreading = True
        while spreading:
            spreading = False
            for node in ast.walk(tree):
                function, klass = places.get(id(node), (MODULE_LEVEL, None))
                held = bound.setdefault(function, set())
                for named, value in _bindings(node):
                    if not _reachable(value, reaching(function, klass), klass, held):
                        continue
                    if isinstance(named, ast.Name) and named.id not in held:
                        held.add(named.id)
                        spreading = True

        for node in ast.walk(tree):
            if isinstance(node, ast.Return) and node.value is not None:
                answer = node.value
            elif isinstance(node, ast.Lambda):
                # A lambda has no return statement; its body IS what it hands back.
                answer = node.body
            else:
                continue
            function, klass = places.get(id(node), (MODULE_LEVEL, None))
            if function == MODULE_LEVEL or function in carriers:
                continue
            if _reachable(answer, reaching(function, klass), klass, bound.get(function, set())):
                carriers.add(function)
                growing = True

    statements = _module_statements(tree)
    taken = []
    for node in ast.walk(tree):
        if not isinstance(node, ast.Call):
            continue
        function, _klass = places.get(id(node), (MODULE_LEVEL, None))
        # A carrier is reported here too. One that only forwards -- return await helper() --
        # names nothing itself, so skipping it would drop the very hop that made it a carrier.
        # And a call made at module level is a place the same way a spelling written there is.
        at_module = function == MODULE_LEVEL
        where = statements.get(id(node), "a statement") if at_module else function
        for place in sorted(called(node, function) & carriers):
            taken.append((at_module, where, node.lineno, "through " + place))
    return carriers, taken


def _occurrences(tree, spelled):
    """Every place a spelling appears, one entry per occurrence.

    Per occurrence rather than per distinct spelling, because deleting one of two identical
    occurrences is a change this has to see rather than a narrowing it can sleep through.
    """
    places, statements = _places(tree), _module_statements(tree)
    found = []
    for node in ast.walk(tree):
        function, klass = places.get(id(node), (MODULE_LEVEL, None))
        spelling = spelled(node, klass)
        if spelling is None:
            continue
        at_module = function == MODULE_LEVEL
        where = statements.get(id(node), "a statement") if at_module else function
        found.append((at_module, where, node.lineno, spelling))
    _carriers, taken = _hands_on(tree, spelled)
    return sorted(found + taken)


def refusal_spellings():
    """Every way a refusal can be spelled here, asked of the objects rather than listed.

    The answers are REFUSAL_ANSWERS. Each spelling of one is then derived: an attribute an
    imported module binds to one, a global of this module bound to one, and a collection --
    here or on an imported module -- that contains one. Nothing in the result is written down,
    so a constant added to REFUSAL_ANSWERS widens what the check sees without a second edit,
    which is exactly what the hardcoded set of constant names it replaces could not do.
    """
    answers = frozenset(REFUSAL_ANSWERS)

    def is_answer(value):
        return isinstance(value, str) and value in answers

    def collects(value):
        if isinstance(value, (tuple, list, set, frozenset)):
            return any(is_answer(item) for item in value)
        if isinstance(value, dict):
            # Keys as well as values: "answer in mapping" asks about the keys, and a mapping
            # from a refusal to a label is an ordinary way for one to be written down.
            return any(is_answer(item) for item in value) or any(
                is_answer(item) for item in value.values())
        return False

    attributes, mine, held = set(), set(), set()
    for name, value in list(vars(sys.modules[__name__]).items()):
        if isinstance(value, types.ModuleType):
            for attribute, inner in list(vars(value).items()):
                if attribute.startswith("_"):
                    continue
                if is_answer(inner):
                    attributes.add(attribute)
                elif collects(inner):
                    held.add(attribute)
        elif is_answer(value):
            mine.add(name)
        elif collects(value):
            held.add(name)
    return {"answer": answers, "module attribute": frozenset(attributes),
            "own global": frozenset(mine), "collection": frozenset(held)}


def source_spellings(tree):
    """Every way the text of a source file can be reached here, and what could not be read.

    Three derivations and no list. A binding of this module whose value is a path to a .py file
    that exists, or a collection of them, is a handle, and so is the __file__ every module
    carries. A name in call position is ASKED whether it hands source back: its own name says
    source, or the first parameter of the callable it resolves to is named "source" -- which is
    where ast.parse comes from, so the one form the old check spelled out by hand is now derived
    from ast.parse's own signature. A string ending in .py names a source file.

    A name bound to one of those callables answers for it as well, to a fixpoint. That binding is
    never taken back and is not kept per function: where a name is rebound, or where another
    function happens to use the same name for something else, this reader answers that it still
    reads source. Being wrong that way costs a sentence somebody has to write; being wrong the
    other way costs a place that concludes from source text and is never asked about it.

    A called name whose signature cannot be read is not assumed to be harmless. It comes back as
    undecided and has to be written down, because a name nobody could read reported as a name
    that does not read source is a plausible default standing in for an answer nobody got.
    """
    namespace = dict(vars(sys.modules[__name__]))
    # An import written inside a function never reaches the module namespace, so the name it
    # binds is read out of the import itself and resolved to what it actually imports.
    # An import inside a function binds only in that function, and this namespace is one for
    # the whole file, so two scopes using one alias for two things cannot both be represented.
    # Every candidate is collected and the one that reads source wins, because taking the other
    # would be the silent pass this module exists to refuse; taking this one only over-reports.
    imported = {}
    for node in ast.walk(tree):
        if isinstance(node, ast.ImportFrom) and node.module:
            owner = sys.modules.get(node.module)
            if owner is None:
                continue
            for alias in node.names:
                value = getattr(owner, alias.name, None)
                if value is not None:
                    imported.setdefault(alias.asname or alias.name, []).append(value)
        elif isinstance(node, ast.Import):
            for alias in node.names:
                owner = sys.modules.get(alias.name)
                if owner is not None:
                    imported.setdefault(alias.asname or alias.name.split(".")[0],
                                        []).append(owner)

    def reads_source(value):
        own = getattr(value, "__name__", "")
        if "source" in own.lower():
            return True
        try:
            return list(inspect.signature(value).parameters)[:1] == ["source"]
        except (ValueError, TypeError):
            return False

    for name, candidates in imported.items():
        reading_ones = [value for value in candidates if reads_source(value)]
        namespace[name] = (reading_ones or candidates)[0]

    def is_source_file(value):
        return isinstance(value, Path) and value.suffix == ".py" and value.exists()

    handles = {"__file__"}
    for name, value in list(namespace.items()):
        if is_source_file(value):
            handles.add(name)
        elif isinstance(value, (tuple, list, set, frozenset)) and value and all(
                is_source_file(item) for item in value):
            handles.add(name)

    def asked(spelling):
        """Does this name hand source back? A reason, an unreadable signature, or neither."""
        head, _, attribute = spelling.rpartition(".")
        if head:
            owner = namespace.get(head)
            if not isinstance(owner, types.ModuleType):
                return None, None, False
            value = getattr(owner, attribute, None)
        else:
            value = namespace.get(attribute, getattr(builtins, attribute, None))
            if isinstance(value, types.FunctionType) and value.__module__ == __name__:
                return None, None, False
        if not callable(value):
            return None, None, False
        if "source" in attribute.lower():
            return "its own name says it hands source", None, True
        own = getattr(value, "__name__", "")
        if "source" in own.lower():
            # from inspect import getsource as reader: the local spelling says nothing, and the
            # callable still says it. Asked of the object, like everything else here.
            return "the callable it resolves to is named " + own, None, True
        try:
            first = list(inspect.signature(value).parameters)[:1]
        except (ValueError, TypeError):
            return None, "its signature cannot be read here", True
        if first == ["source"]:
            return "its first parameter is named source", None, True
        return None, None, True

    hands_source, undecided, called = {}, {}, set()
    for node in ast.walk(tree):
        if not isinstance(node, ast.Call):
            continue
        spelling = _dotted(node.func)
        if spelling is None:
            continue
        why, unread, resolved = asked(spelling)
        if not resolved:
            continue
        called.add(spelling)
        if why:
            hands_source[spelling] = why
        elif unread:
            undecided[spelling] = unread

    # And a name bound to one of those answers for it, because reader = inspect.getsource is an
    # ordinary line and the call after it reaches exactly as far. Taken to a fixpoint so an alias
    # of an alias is followed too.
    growing = True
    while growing:
        growing = False
        def spelled_by(expression):
            """Every name a right-hand side may be, a conditional's arms included."""
            if isinstance(expression, ast.IfExp):
                return spelled_by(expression.body) + spelled_by(expression.orelse)
            if isinstance(expression, (ast.NamedExpr, ast.Await)):
                return spelled_by(expression.value)
            if isinstance(expression, ast.BoolOp):
                return [name for value in expression.values for name in spelled_by(value)]
            named = _dotted(expression)
            return [named] if named else []

        for node in ast.walk(tree):
            if isinstance(node, ast.NamedExpr):
                holders, answer = [node.target], node.value
            elif isinstance(node, (ast.Assign, ast.AnnAssign)) and node.value is not None:
                holders = node.targets if isinstance(node, ast.Assign) else [node.target]
                answer = node.value
            else:
                continue
            paired = []
            for holder in holders:
                if (isinstance(holder, (ast.Tuple, ast.List))
                        and isinstance(answer, (ast.Tuple, ast.List))
                        and len(holder.elts) == len(answer.elts)):
                    paired += list(zip(holder.elts, answer.elts))
                else:
                    paired.append((holder, answer))
            for holder, value in paired:
              for named in spelled_by(value):
                if named not in hands_source:
                    # The name may hand source back without ever being called here: reader =
                    # inspect.getsource puts it behind a local name and the call names only that.
                    why, unread, _resolved = asked(named)
                    if unread:
                        # A name nobody could read stays unreadable when it is put behind
                        # another name; treating the alias as harmless is the plausible default
                        # this module refuses everywhere else.
                        undecided.setdefault(named, unread)
                    if not why:
                        continue
                    hands_source[named] = why
                    growing = True
                bound = _dotted(holder)
                if bound and bound not in hands_source:
                    hands_source[bound] = "it is bound to " + named + ", which does"
                    growing = True
    return frozenset(handles), hands_source, undecided, frozenset(called)


def _refusal_spelled(spellings, held):
    """The matcher: which node is a refusal, spelled any of the derived ways."""
    answers = spellings["answer"]
    attributes = spellings["module attribute"] | spellings["collection"]
    names = spellings["own global"] | spellings["collection"]

    def spelled(node, klass):
        if isinstance(node, ast.Constant) and isinstance(node.value, str):
            return repr(node.value) if node.value in answers else None
        if isinstance(node, ast.Attribute):
            through = _dotted(node.value)
            reader = (klass if through in ("self", "cls")
                      else (through or "").rpartition(".")[2] or None)
            if reader is not None and node.attr in held.get(reader, ()):
                return (through or "") + "." + node.attr
            return "." + node.attr if node.attr in attributes else None
        if isinstance(node, ast.Name) and node.id in names:
            return node.id
        return None

    return spelled


def _handle_names(tree, handles):
    """Every name that reaches a source file, the module's own and the local ones bound to them.

    path = HERE is an ordinary line, and a read taken on path reaches the same file.
    """
    known, growing = set(handles), True
    while growing:
        growing = False
        for node in ast.walk(tree):
            for target, value in _bindings(node):
                if _dotted(value) not in known:
                    continue
                named = _dotted(target)
                if named and named not in known:
                    known.add(named)
                    growing = True
    return frozenset(known)


def _source_spelled(handles, hands_source, held):
    """The matcher: which node reaches the text of a source file, spelled any of the derived ways."""
    def spelled(node, klass):
        if isinstance(node, ast.Name):
            return node.id if node.id in handles else None
        if isinstance(node, ast.Attribute):
            through = _dotted(node.value)
            reader = (klass if through in ("self", "cls")
                      else (through or "").rpartition(".")[2] or None)
            if reader is not None and node.attr in held.get(reader, ()):
                return (through or "") + "." + node.attr
            return "." + node.attr if node.attr == "__file__" else None
        if isinstance(node, ast.Constant) and isinstance(node.value, str):
            return repr(node.value) if node.value.endswith(".py") else None
        if isinstance(node, ast.Call):
            spelling = _dotted(node.func)
            if spelling in hands_source:
                return spelling
            # A READ taken on a handle hands source text back: a helper answering
            # HERE.read_text() gives its caller the text as surely as one answering ast.parse.
            # Only a read. HERE.exists() and HERE.with_suffix() answer about the file rather
            # than with it, and counting those would put every filesystem question in the
            # inventory. Which spellings mean a read is recognised by the name beginning with
            # read, which is narrower than the class of ways to get a file's contents. The place
            # itself is accounted either way, because the handle is named there; what a narrower
            # rule costs is the CALLER of a helper that reads some other way.
            head, _, attribute = (spelling or "").rpartition(".")
            through, _, last = head.rpartition(".")
            reader = klass if through in ("self", "cls") else None
            on_a_handle = head in handles or (reader is not None
                                              and last in held.get(reader, ()))
            return spelling if on_a_handle and attribute.startswith("read") else None
        return None

    return spelled


def planted_source(declared, spelling, planted, lines, beside=()):
    """A synthetic module: one stub per declared place, and one place spelled the planted way.

    Generated FROM the declared maps rather than written out, so it stays a balanced control while
    they move. Each stub carries a spelling the check recognises, so the only thing separating the
    planted place from the declared ones is the form it is written in.

    The companion maps are looked up by name rather than referenced, because this same case is
    run against the commit BEFORE they existed in order to measure the red it produces there, and
    a reference would make that an attribute error instead of the refusal being measured. What
    this control establishes is that the complaint about the planted place is caused by the
    plant; it does not make the synthetic module a copy of this one, and the check may still say
    other things about a module that is not the file it was written for.
    """
    text = []
    names = set(declared)
    for other in beside:
        names |= set(globals().get(other) or ())
    for name in sorted(names):
        # A declared place may be a helper inside another one, and its name is the chain that
        # keeps it apart. The stub is written as that same chain so the derivation computes the
        # name the declaration is keyed by, rather than one the check would call undeclared.
        parts = name.split(".")
        for depth, part in enumerate(parts):
            text.append("    " * depth + "def " + part + "(self):")
        text += ["    " * len(parts) + spelling, ""]
    if planted is not None:
        text += ["def " + planted + "(self):"] + ["    " + line for line in lines] + [""]
    return "\n".join(text)


# The companion maps, named rather than referenced, so a control can be run against a commit
# where they do not exist yet.
BESIDE_TEXT = ("TOUCHES_SOURCE_WITHOUT_CONCLUDING",)
BESIDE_REFUSAL = ("NAMES_A_REFUSAL_WITHOUT_SETTLING",)


def asked_over(check, source):
    """Run one of the checks over a source of our own, and report what it refused to accept.

    The check reads HERE, so HERE is what is moved. Reaching the handle by its name as a string
    is the one form the derivation cannot see, which SOURCE_OUT_OF_REACH says, and this is the
    case that proves the sentence is about something real.
    """
    with tempfile.TemporaryDirectory() as temporary:
        written = Path(temporary) / "planted.py"
        written.write_text(source, encoding="utf-8")
        with mock.patch.object(sys.modules[__name__], "HERE", written):
            try:
                check()
            except AssertionError as refused:
                return str(refused)
    return None


def refusals_reached(source):
    """Every occurrence of a refusal in this source, and the spellings the derivation used."""
    tree = ast.parse(source)
    spellings = refusal_spellings()
    # To a fixpoint, because self.second = self.first holds the refusal only once the pass
    # knows that self.first does.
    held, growing = {}, True
    while growing:
        wider = _held_by_class(tree, _refusal_spelled(spellings, held), held)
        growing = wider != held
        held = wider
    return _occurrences(tree, _refusal_spelled(spellings, held)), spellings


def source_text_reached(source):
    """Every occurrence of a reach into source text, the spellings, the unread names and the calls."""
    tree = ast.parse(source)
    handles, hands_source, undecided, called = source_spellings(tree)
    handles = _handle_names(tree, handles)
    held, growing = {}, True
    while growing:
        wider = _held_by_class(tree, _source_spelled(handles, hands_source, held), held)
        growing = wider != held
        held = wider
    return (_occurrences(tree, _source_spelled(handles, hands_source, held)),
            {"handle": handles, "hands source": frozenset(hands_source)}, undecided, called)


class SevenReadingsTests(unittest.TestCase):
    """Seven questions, seven readings, and no answer standing in for another.

    The criterion this module exists for. The seven are not one payload: two are commands and
    one is a comparison this module performs, so the easy failure is a table that quietly fills a
    cell from whatever was nearest. Each check below removes one way of doing that -- a row that
    reads a path nobody writes, a row that reads another row answer, a cell filled without going
    through read() at all -- and the mutation case states the property directly rather than
    trusting the declaration.
    """

    def setUp(self):
        self.maxDiff = None

    def test_every_question_has_exactly_one_declared_reading(self):
        """Asked of the accessor, because two lists agreeing is not a reading being reachable.

        Comparing the table with the list of questions is two declarations agreeing with each
        other. What the seven actually need is for the accessor to resolve each of them, so each
        is put through read() and has to come back as itself.
        """
        for cell in SEVEN:
            with self.subTest(cell):
                self.assertEqual(read(cell, {})["cell"], cell,
                                 cell + ": the accessor does not resolve a declared reading")
        declared = [row[0] for row in READINGS]
        self.assertEqual(len(set(declared)), len(declared), "each question is answered once")
        self.assertEqual(set(declared) - set(SEVEN), set(),
                         "a row nobody asked for is an answer to nothing")

    def test_no_two_cells_read_the_same_answer(self):
        places = [(row[1], row[2]) for row in DECLARED]
        self.assertEqual(len(set(places)), len(places),
                         "two cells reading one place is one reading answering two questions")

    def test_every_declared_producer_exists_where_it_is_declared(self):
        """Asked of the module, not read out of its file.

        A definition found in source text can be a name in a comment or in a branch nothing
        reaches, and for the producer this module owns the file being searched is the file that
        declares it -- so the search would have found its own declaration. The module is asked
        for the attribute instead, which is the thing a caller reaches.
        """
        for cell, _source, _path, producer in DECLARED:
            module, _, name = producer.rpartition(".")
            with self.subTest(cell):
                self.assertIn(module, PRODUCER_MODULES,
                              cell + ": nothing resolves " + module)
                resolved = getattr(PRODUCER_MODULES[module], name, None)
                self.assertIsNotNone(resolved, cell + ": " + producer + " does not exist")
                self.assertTrue(callable(resolved),
                                cell + ": " + producer + " is not something that can produce"
                                " a reading")


    def test_every_text_reading_place_is_declared(self):
        """The inventory, derived by accounting rather than by recognising a shape.

        Three times a check in this module concluded something about behaviour by reading source
        text, and each time the repair was the instance. The instances share a shape: a claim
        about what code does, settled by how the code is spelled -- and where the file being read
        is the file making the claim, the check can be satisfied by its own declaration.
        AGENTS.md states the rule for this repository: a text-matching test is not proof of
        behaviour.

        What this check used to see was one form, a call to ast.parse inside a function, and it
        said so. Anything else -- read_text compared directly, a phrase searched for, a name
        imported from ast, a helper handing the text on -- was simply not looked at, so the
        sentence above it reached further than the check did. It now derives instead: every
        binding of this module that is a path to a .py file that exists, the __file__ every
        module carries, every called name that hands source back (asked of the callable, which
        is where ast.parse comes from, so the one form that used to be spelled out by hand is
        now read off ast.parse's own signature), and every string naming a .py file. Then it
        accounts for EVERY occurrence of one, attributed to the innermost function it sits in and
        named by the chain around it, so a nested helper or a lambda inside an already declared
        place is its own place rather than something that place absorbs. A place that reaches
        source text some way nobody anticipated is still an occurrence of a handle, so it arrives
        here as a failure.

        This claims exactly that much. A name this derivation could not read is in
        SOURCE_UNDECIDED_CALLS rather than assumed harmless, and the forms it cannot reach at all
        are in SOURCE_OUT_OF_REACH, each with a control that plants it and requires the
        derivation not to see it.
        """
        found, reach, undecided, called = source_text_reached(
            HERE.read_text(encoding="utf-8"))

        unaccounted = [row for row in found
                       if (row[1] not in SOURCE_AT_MODULE_LEVEL if row[0]
                           else row[1] not in TEXT_EVIDENCE
                           and row[1] not in TOUCHES_SOURCE_WITHOUT_CONCLUDING)]
        self.assertEqual(unaccounted, [],
                         "a place reaches the text of a source file and nobody wrote down what"
                         " it is doing there. Say whether it concludes from the text or only"
                         " names the file, or teach the derivation the form: "
                         + json.dumps(unaccounted))

        reached = {row[1] for row in found}
        stale = sorted((set(TEXT_EVIDENCE) | set(TOUCHES_SOURCE_WITHOUT_CONCLUDING)
                        | set(SOURCE_AT_MODULE_LEVEL)) - reached)
        self.assertEqual(stale, [],
                         "a declaration outlived the place it described: " + json.dumps(stale))

        arrived = sorted(set(undecided) - set(SOURCE_UNDECIDED_CALLS))
        self.assertEqual(arrived, [],
                         "a called name this derivation cannot read is not written down, so"
                         " whether it hands source back is nobody's answer: "
                         + json.dumps(arrived))
        for name, (where, why) in sorted(SOURCE_UNDECIDED_CALLS.items()):
            with self.subTest(name):
                self.assertTrue(why.strip(), name + " is undecided without a reason")
                self.assertIn(name, called,
                              name + " is written down as unreadable and nothing here calls it")
                if where == THE_FLOOR and sys.version_info < READABLE_FROM:
                    self.assertIn(name, undecided,
                                  name + " is declared unreadable on the floor and the floor"
                                  " read it, so the declaration is now wrong")
                if where == EVERY_INTERPRETER:
                    self.assertIn(name, undecided,
                                  name + " is declared unreadable on every supported"
                                  " interpreter and this one read it, so the declaration is"
                                  " now wrong and the entry has to say so")

        for place, why in sorted(TEXT_EVIDENCE.items()):
            with self.subTest(place):
                self.assertTrue(why.strip(),
                                place + " is declared without the reason it cannot ask")
        for place, why in sorted(TOUCHES_SOURCE_WITHOUT_CONCLUDING.items()):
            with self.subTest(place):
                self.assertTrue(why.strip(),
                                place + " touches a source file without saying why that is not"
                                " a conclusion drawn from its text")
        self.assertTrue(reach["handle"] and reach["hands source"],
                        "a derivation kind came back empty, so the sweep stopped matching")


    def test_every_declared_path_is_one_its_source_actually_writes(self):
        """A rename fails here, and it is the payload that says so rather than the file text.

        Searching the producing source for the key passes on a mention in a comment, in an
        unrelated branch, or -- for the reading this module performs itself -- on the entry in
        this very table, which would have left the check answering its own question. So the
        payloads are real, the declared path is walked in them, and the case then removes the
        last key to show the walk was load-bearing rather than incidental.
        """
        with tempfile.TemporaryDirectory() as temporary:
            payloads, _host = observe_all(Path(temporary).resolve())

        for cell, source, path, _producer in DECLARED:
            with self.subTest(cell):
                found = payloads.get(source)
                for key in path:
                    self.assertIsInstance(found, dict,
                                          cell + ": " + source + " does not reach " + repr(key))
                    self.assertIn(key, found,
                                  cell + ": " + source + " no longer writes " + repr(key))
                    found = found[key]

                pruned = copy.deepcopy(payloads)
                target = pruned[source]
                for key in path[:-1]:
                    target = target[key]
                del target[path[-1]]
                self.assertEqual(read(cell, pruned)["value"], reading.UNREADABLE,
                                 cell + ": the row survived its own key being taken away, so"
                                 " the path it declares is not the path it reads")

    def test_no_cell_is_filled_without_going_through_the_declared_reading(self):
        """Lint, not proof: the proof is the mutation case below.

        It catches the cheap version of the defect -- a test reaching into a payload for a key
        this table declares -- which is how a borrowed answer usually arrives.
        """
        keys = {key for row in DECLARED for key in row[2]}
        allowed = {"read", "_row", "_unreadable", "observe_all"}
        reached = []

        def walk(node):
            for child in ast.iter_child_nodes(node):
                if isinstance(child, (ast.FunctionDef, ast.AsyncFunctionDef)):
                    if child.name in allowed:
                        continue
                if isinstance(child, ast.Subscript) and isinstance(child.slice, ast.Constant):
                    if child.slice.value in keys:
                        reached.append(child.slice.value)
                if (isinstance(child, ast.Call) and isinstance(child.func, ast.Attribute)
                        and child.func.attr == "get" and child.args
                        and isinstance(child.args[0], ast.Constant)
                        and child.args[0].value in keys):
                    reached.append(child.args[0].value)
                walk(child)

        walk(ast.parse(HERE.read_text(encoding="utf-8")))
        self.assertEqual(reached, [],
                         "a declared reading key is reached outside read(): " + repr(reached))

    def test_every_row_is_readable_and_answers_in_its_own_vocabulary(self):
        """The baseline the independence cases need.

        An independence check over seven unreadable rows proves nothing: they would stay equal
        to each other through any change. So the baseline is established first, and every row
        answers in the vocabulary of the thing that produced it rather than in a shared one.
        """
        with tempfile.TemporaryDirectory() as temporary:
            payloads, _host = observe_all(Path(temporary))
        rows = table(payloads)

        for cell in SEVEN:
            with self.subTest(cell):
                if cell == "modelPermissionPreservation" and not HAS_READER:
                    # The one row this interpreter cannot take. It has to say so, and say why:
                    # an unread row that reports itself readable is the failure this whole
                    # arrangement is against, and it would be invisible on exactly one job.
                    self.assertFalse(rows[cell]["readable"])
                    self.assertEqual(rows[cell]["value"], reading.UNREADABLE)
                    self.assertIn("tomllib", rows[cell]["detail"],
                                  "the refusal names the reader it did not have")
                    continue
                self.assertTrue(rows[cell]["readable"],
                                cell + ": " + str(rows[cell].get("detail")))

        for cell in CHECK_FIELD_ROWS:
            with self.subTest(cell):
                self.assertIn(rows[cell]["value"]["value"], check.VALUES)

        for cell in COMPLETION_CELL_ROWS:
            with self.subTest(cell):
                answer = rows[cell]["value"]["value"]
                self.assertIsInstance(answer, str)
                self.assertNotIn(answer, check.VALUES,
                                 cell + ": the hook answers in its own vocabulary, and merging"
                                 " it into the result vocabulary would be a translation nobody"
                                 " performed")

        links = rows["skillLink"]["value"]
        self.assertIn("command", links)
        self.assertEqual(links.get("exitCode"), 0,
                         "the listing ran and found nothing missing; accepting a failed check"
                         " here would leave this row green with no skill linked at all")
        self.assertEqual(links.get("missing"), [])
        self.assertTrue(links.get("linked"), "and it names what it linked")

        preservation = rows["modelPermissionPreservation"]["value"]
        if HAS_READER:
            self.assertEqual(preservation, PRESERVED,
                             "an install has no business changing the model or the permission"
                             " posture it was installed over")
        else:
            self.assertEqual(preservation, reading.UNREADABLE,
                             "without a reader this is unread, and unread is not preserved")


    def test_every_question_is_declared_to_answer_in_exactly_one_vocabulary(self):
        """The three families have to cover the seven, once each.

        Not decoration: the families are what stops a row being compared against the wrong set
        of admissible answers. A row in none of them would be checked against nothing, and a row
        in two would be checked against a vocabulary it does not answer in -- which is the same
        borrowing this module refuses, arriving through the door marked housekeeping.
        """
        families = {"check.VALUES": CHECK_FIELD_ROWS,
                    "the completion cells": COMPLETION_CELL_ROWS,
                    "a shape of their own": OWN_SHAPE_ROWS}
        declared = [cell for members in families.values() for cell in members]

        self.assertEqual(sorted(declared), sorted(SEVEN),
                         "every question answers in one declared vocabulary, and only the seven"
                         " do: " + json.dumps(families))
        self.assertEqual(len(set(declared)), len(declared),
                         "a question in two vocabularies is checked against one it does not"
                         " answer in")

    def test_every_acceptance_row_reaches_its_success_answer(self):
        """The one that decides whether this suite is worth having.

        Every row here judges whether something worked. A row whose assertion accepts a refusal,
        an absence or a not_verified is a row that stays green when the thing it judges never
        happens -- and an install acceptance suite that passes while nothing installed is worse
        than no suite, because the next person reads that ground as covered.

        So one run is made in which all seven have something that worked to read, and each row
        is required to read its declared success answer. The skills are really linked, the
        registration really names the command the diagnosis is asked about, both components
        really import from this checkout, the relay really answers the doctor and every step of
        a trial, and the hook has really fired.
        """
        with tempfile.TemporaryDirectory() as temporary:
            payloads, host, registration = succeeding(Path(temporary).resolve())
            # Where the import resolved, captured before the directory goes: a verified import
            # that resolved in the checkout would be source availability answering a question
            # about an installation.
            resolved = {name: component.get("importedLocation")
                        for name, component in
                        (payloads["diagnose"].get("components") or {}).items()}
            candidate = host.candidate
        rows = table(payloads)

        self.assertEqual(sorted(SUCCESS_ANSWERS), sorted(SEVEN),
                         "every question declares the answer it reads when the thing worked")

        if HAS_READER:
            # The exposure row declares that it travels the registration this run wrote, so the
            # writing is required here. Reconstructing the command and handing it to diagnose
            # would read verified while register-mcp wrote nothing at all.
            self.assertEqual(registration.returncode, 0, registration.stderr[-300:])
            self.assertEqual(json.loads(registration.stdout)["outcome"], "CREATED",
                             "the registration the exposure row compares against was not"
                             " written by this run")

        # One host, not several. The hook row answers from the journal of the Codex home the
        # rest of this run installed into; answering from a home of its own would compose
        # readings about two different machines and still look like a composition.
        journal = rows["hookCallback"]["value"].get("journalRoot")
        self.assertIsNotNone(journal, "the hook cell names no journal it counted")
        self.assertTrue(inside(journal, candidate.parent.parent / "codex"),
                        "the callback was recorded under " + str(journal) + ", which is not the"
                        " Codex home this run installed into")

        self.assertTrue(resolved, "the diagnosis reported no component at all")
        for name, where in resolved.items():
            with self.subTest(name):
                self.assertIsNotNone(where, name + ": nothing was imported")
                self.assertTrue(inside(where, candidate),
                                name + " imported from " + str(where) + ", which is outside the"
                                " environment this run installed, so the row is answering about"
                                " available source rather than an installed runtime")

        for cell in SEVEN:
            wanted = SUCCESS_ANSWERS[cell]
            with self.subTest(cell):
                if cell in CONDITIONAL_SUCCESS and not HAS_READER:
                    # The declared condition, asserted rather than assumed: the success answer
                    # is excused only where the thing it names is genuinely absent. Where the
                    # row can still be read, what it reads is checked by the vocabulary case;
                    # what is not claimed here is that it succeeded.
                    self.assertFalse(HAS_READER,
                                     cell + ": excused from its success answer on an"
                                     " interpreter that does have the reader")
                    continue
                self.assertTrue(rows[cell]["readable"], str(rows[cell].get("detail")))
                found = rows[cell]["value"]
                if isinstance(wanted, dict):
                    self.assertEqual({key: found.get(key) for key in wanted}, wanted,
                                     cell + ": the listing does not read as a success")
                elif cell in OWN_SHAPE_ROWS:
                    self.assertEqual(found, wanted, cell + ": not its success answer")
                else:
                    self.assertEqual(found["value"], wanted,
                                     cell + ": read " + repr(found["value"]) + " where the thing it judges had worked, so this row would stay green if it never did")


    def test_every_place_that_settles_for_a_refusal_is_declared(self):
        """The inventory of accepted answers, derived by accounting for every occurrence.

        What this used to see was a refusal spelled as a literal, or as one of five constant
        names written into the check itself, inside a call whose name began with assert. Three
        forms, enumerated by hand -- and the enumerated half is the part that cannot grow: a
        constant added to REFUSAL_ANSWERS was invisible until somebody remembered to add its
        name here too, and a refusal bound to a name before the assertion, or required through
        assertIn against the answers themselves, was never looked at at all.

        So nothing in the spelling table is written down now. refusal_spellings asks the objects: the answers are
        REFUSAL_ANSWERS, and every way one can be written is derived from them -- an attribute an
        imported module binds to one, a global here bound to one, a collection here or on an
        imported module that contains one, and an attribute a class binds one to for its own
        methods to read. Then EVERY occurrence of any of those is accounted for: it belongs to a
        place that settles for a refusal, or to one that names a refusal without settling, or the
        module fails naming the place, the line and the spelling. The place is the innermost
        function the occurrence sits in, named by the chain around it, so a nested helper or a
        lambda inside an already declared method is its own place rather than something the
        declaration absorbs. A helper that hands a refusal BACK rather than returning a container
        holding one carries its callers in with it, through a bare call or through self.

        The point is not that refusals are forbidden -- this repository is built on absence being
        a real answer. It is that a place settling for one has to say why the refusal is the
        right answer to the question it asks, so that no acceptance row quietly accepts its own
        failure. What is still out of reach is in REFUSAL_OUT_OF_REACH with a control for each.
        """
        found, spellings = refusals_reached(HERE.read_text(encoding="utf-8"))

        unaccounted = [row for row in found
                       if (row[1] not in REFUSAL_AT_MODULE_LEVEL if row[0]
                           else row[1] not in ACCEPTS_A_REFUSAL
                           and row[1] not in NAMES_A_REFUSAL_WITHOUT_SETTLING)]
        self.assertEqual(unaccounted, [],
                         "a place names an answer that means the question was not settled, and"
                         " nobody wrote down what it is doing there. Say why the refusal is the"
                         " right answer here, or why this place only names one: "
                         + json.dumps(unaccounted))

        reached = {row[1] for row in found}
        stale = sorted((set(ACCEPTS_A_REFUSAL) | set(NAMES_A_REFUSAL_WITHOUT_SETTLING)
                        | set(REFUSAL_AT_MODULE_LEVEL)) - reached)
        self.assertEqual(stale, [],
                         "a declaration outlived the place it described: " + json.dumps(stale))

        for place, why in sorted(ACCEPTS_A_REFUSAL.items()):
            with self.subTest(place):
                self.assertTrue(why.strip(), place + " settles for a refusal without a reason")
        for place, why in sorted(NAMES_A_REFUSAL_WITHOUT_SETTLING.items()):
            with self.subTest(place):
                self.assertTrue(why.strip(),
                                place + " names a refusal without saying why that is not"
                                " settling for one")
        for kind, spelled in sorted(spellings.items()):
            with self.subTest(kind):
                self.assertTrue(spelled, kind + " came back empty, so the sweep stopped matching")


    def test_a_text_reading_place_arriving_in_a_form_the_old_sweep_missed_is_still_caught(self):
        """The regression: a place that concludes from source text some other way is red.

        The forms are planted rather than argued about. Each is one the sweep this replaces could
        not see -- it looked for a call to ast.parse and nothing else -- and each is measured by
        running the check over a module written here and requiring it to refuse, naming the place,
        the line and the spelling. The control is the same module without the plant: whatever the
        check says about a synthetic module, it must not already be saying it about the planted
        place, or the refusal would be evidence of nothing.
        """
        for planted, lines, spelling in (
                ("a_place_that_reads_the_source_of_an_object",
                 ['self.assertIn("a phrase", inspect.getsource(read))'], "inspect.getsource"),
                ("a_place_that_compares_this_file_text_directly",
                 ['self.assertIn("a phrase", HERE.read_text(encoding="utf-8"))'], "HERE")):
            with self.subTest(planted):
                check = self.test_every_text_reading_place_is_declared
                control = asked_over(check, planted_source(TEXT_EVIDENCE, "ast.parse(text)", None, (),
                                                           BESIDE_TEXT))
                self.assertNotIn(planted, control or "",
                                 "the synthetic module already complains about the planted place"
                                 " before it is planted, so refusing it would prove nothing")
                built = planted_source(TEXT_EVIDENCE, "ast.parse(text)", planted, lines,
                                       BESIDE_TEXT)
                at = built.splitlines().index("    " + lines[0]) + 1
                said = asked_over(check, built)
                self.assertIsNotNone(said,
                                     planted + ": the check accepted a module in which a place"
                                     " concludes from source text without being declared")
                self.assertIn(planted, said, planted + ": refused, but not for this place")
                self.assertIn(spelling, said, planted + ": refused without naming what it saw")
                self.assertIn(str(at), said,
                              planted + ": refused without naming the line it was on")

    def test_a_place_that_settles_for_a_refusal_in_a_form_the_old_sweep_missed_is_still_caught(self):
        """The regression: a place that requires a refusal some other way is red.

        Two forms the sweep this replaces could not see. It looked inside calls whose name begins
        with assert, for a literal, one of five constant names written into the check, or the
        name CHANGED -- so a refusal required through the answers themselves, and a refusal bound
        to a name before the assertion, both passed. Same measurement, same control.
        """
        spelling = "self.assertEqual(row, " + repr(sorted(REFUSAL_ANSWERS)[0]) + ")"
        for planted, lines in (
                ("a_place_that_requires_one_of_the_answers",
                 ["self.assertIn(row, REFUSAL_ANSWERS)"]),
                ("a_place_that_names_the_refusal_before_it_asserts",
                 ["settled = reading.UNREADABLE", "self.assertEqual(row, settled)"])):
            with self.subTest(planted):
                check = self.test_every_place_that_settles_for_a_refusal_is_declared
                control = asked_over(check, planted_source(ACCEPTS_A_REFUSAL, spelling, None, (),
                                                           BESIDE_REFUSAL))
                self.assertNotIn(planted, control or "",
                                 "the synthetic module already complains about the planted place"
                                 " before it is planted, so refusing it would prove nothing")
                said = asked_over(check, planted_source(ACCEPTS_A_REFUSAL, spelling, planted,
                                                        lines, BESIDE_REFUSAL))
                self.assertIsNotNone(said,
                                     planted + ": the check accepted a module in which a place"
                                     " settles for a refusal without being declared")
                self.assertIn(planted, said, planted + ": refused, but not for this place")

    def test_the_reach_of_each_derivation_is_the_declared_one(self):
        """Support, not a regression: a derivation kind that stopped resolving reads as green.

        Every kind is derived, so nothing here is the reach itself. What is written down is the
        spellings this module actually relies on, and the check is that each is still derived.
        A kind may reach further than this and that is not a failure; a kind that quietly stopped
        resolving -- an import that moved, a constant that was renamed -- loses one of these and
        fails, which the occurrence counts alone would not catch because a spelling nobody writes
        has no occurrence to lose.
        """
        spellings = refusal_spellings()
        for kind, wanted in sorted(REFUSAL_REACH_INCLUDES.items()):
            with self.subTest(kind):
                self.assertLessEqual(set(wanted), set(spellings[kind]),
                                     kind + ": " + json.dumps(sorted(set(wanted)
                                                                     - set(spellings[kind])))
                                     + " is no longer derived")
        _found, reach, _undecided, _called = source_text_reached(
            HERE.read_text(encoding="utf-8"))
        for kind, wanted in sorted(SOURCE_REACH_INCLUDES.items()):
            with self.subTest(kind):
                self.assertLessEqual(set(wanted), set(reach[kind]),
                                     kind + ": " + json.dumps(sorted(set(wanted)
                                                                     - set(reach[kind])))
                                     + " is no longer derived")

    def test_the_forms_outside_each_derivations_reach_are_the_declared_ones(self):
        """Support: the list of what cannot be reached is checked in both directions.

        A sentence saying a derivation cannot see something rots quietly. So each declared form is
        planted and the derivation has to NOT see it. A form that gets covered later makes its own
        control fail, which is the prompt to delete the entry rather than leave it claiming a
        limitation that no longer exists.
        """
        for form, (sample, why) in sorted(SOURCE_OUT_OF_REACH.items()):
            with self.subTest(form):
                self.assertTrue(why.strip(), form + " is declared without a reason")
                found, _reach, _undecided, _called = source_text_reached(
                    planted_source((), "", "a_place_outside_the_reach", [sample]))
                self.assertEqual([row for row in found if row[1] == "a_place_outside_the_reach"],
                                 [], form + " is inside the reach now, so delete the entry")
        for form, (sample, why) in sorted(REFUSAL_OUT_OF_REACH.items()):
            with self.subTest(form):
                self.assertTrue(why.strip(), form + " is declared without a reason")
                found, _spellings = refusals_reached(
                    planted_source((), "", "a_place_outside_the_reach", [sample]))
                self.assertEqual([row for row in found if row[1] == "a_place_outside_the_reach"],
                                 [], form + " is inside the reach now, so delete the entry")

    def test_no_two_places_this_module_declares_can_share_a_name(self):
        """Support: the declarations are keyed by name, so two places sharing one would merge.

        Counted from the parsed file, which keeps both, rather than from _functions_here(), whose
        set has already discarded the second by the time anyone could look. The names counted are
        the ones the derivation computes, so a nested helper is kept apart by the chain around it
        while two classes with a method of the same name collide here, which is the case a flat
        key cannot survive.
        """
        tree = ast.parse(HERE.read_text(encoding="utf-8"))
        places = _places(tree)
        named = [places[id(node)][0] for node in ast.walk(tree)
                 if isinstance(node, (ast.FunctionDef, ast.AsyncFunctionDef, ast.Lambda))]
        repeated = sorted({name for name in named if named.count(name) > 1})
        self.assertEqual(repeated, [],
                         "two places share a name, so one declaration now speaks for both: "
                         + json.dumps(repeated))

    def test_every_reading_declares_the_path_it_travels(self):
        """Reaching a success answer and reaching it down the right path are two questions.

        A reading can pass through a source-level shortcut and answer exactly as it would have
        through the installed thing -- that is what makes the substitution quiet. This issue
        delivers an acceptance test for installation and hook REGISTRATION, so a row that skips
        the registration is not a residual detail, it is the claim missing its subject.

        And the path is only half of it. A row can travel the registered command and still be
        reading a second Codex home this suite made for it, which composes readings about two
        machines and looks exactly like a composition. So each row names the host it looks at
        as well as the path it takes.

        The paths are not derivable from the source: what an executed command reaches is a fact
        about the run, not about the call graph. So each is declared, and what this check holds
        is that no row is silent about which one it travels.
        """
        self.assertEqual(sorted(READING_PATHS), sorted(SEVEN),
                         "a reading that does not say which path it travels can move onto a"
                         " shortcut without anything here changing")
        for cell, path in READING_PATHS.items():
            with self.subTest(cell):
                self.assertTrue(path.strip(), cell + " declares no path")
                self.assertTrue(path.startswith(("installed,", "registered,", "stand-in,")),
                                cell + " names a path this suite has no word for: " + path[:40])
                self.assertIn("on the run's own", path,
                              cell + " names a path but not the host it reads, and a row can"
                              " travel the right path on the wrong machine")
    def test_a_row_that_cannot_require_success_says_why_absence_is_normal(self):
        """The only exception, and it has to carry its reason.

        Absence is a real state in this repository and an answer set has to be able to express
        it. What it must not become is a place to file a row that simply never succeeds, so a
        conditional row names the condition rather than the fact that it is conditional.
        """
        self.assertEqual(set(CONDITIONAL_SUCCESS) - set(SEVEN), set(),
                         "a condition is declared for a question nobody asks")
        for cell, why in CONDITIONAL_SUCCESS.items():
            with self.subTest(cell):
                self.assertTrue(why.strip(), cell + " is excepted without a reason")
                self.assertIn(cell, SUCCESS_ANSWERS,
                              cell + " has no success answer to fall back from")
    def test_each_row_reads_its_own_path_and_no_other(self):
        """The property, stated as a change rather than as a declaration.

        One reading is moved at a time and the whole table is rebuilt. Exactly the row that
        declared that path may move with it. Any other row that moves is a row reading something
        it did not declare, which is the borrowed answer this table exists to prevent -- and no
        amount of careful declaration would have caught it.
        """
        sentinel = "a-value-no-reading-produces"
        with tempfile.TemporaryDirectory() as temporary:
            payloads, _host = observe_all(Path(temporary))
        baseline = table(payloads)

        for cell in SEVEN:
            _declared, source, path, _producer = _row(cell)
            with self.subTest(cell):
                moved = copy.deepcopy(payloads)
                target = moved[source]
                for key in path[:-1]:
                    target = target[key]
                target[path[-1]] = sentinel
                rebuilt = table(moved)
                self.assertEqual(rebuilt[cell]["value"], sentinel,
                                 cell + ": the row does not read the path it declared")
                for other in SEVEN:
                    if other == cell:
                        continue
                    self.assertEqual(rebuilt[other], baseline[other],
                                     other + " moved when only " + cell + " was changed")

    def test_withholding_a_source_leaves_only_its_own_rows_unreadable(self):
        """A command that did not run and a command that answered are never the same row."""
        with tempfile.TemporaryDirectory() as temporary:
            payloads, _host = observe_all(Path(temporary))
        baseline = table(payloads)

        for source in ("diagnose", "hook-status", ACCEPTANCE):
            with self.subTest(source):
                withheld = {k: v for k, v in payloads.items() if k != source}
                rows = table(withheld)
                for cell in SEVEN:
                    if _row(cell)[1] == source:
                        self.assertEqual(rows[cell]["value"], reading.UNREADABLE,
                                         cell + ": a reading that was not made is unreadable,"
                                         " never False and never absent")
                        self.assertFalse(rows[cell]["readable"])
                    else:
                        self.assertEqual(rows[cell], baseline[cell],
                                         cell + " lost its answer when another source did not"
                                         " run, so it was never answering on its own")

    def test_the_hook_row_counts_an_invocation_that_really_happened(self):
        """Criterion 3 asks for a real callback, and a file count is not one.

        The journal cell counts records. A directory somebody pre-populated would answer the
        same way, so the row is established by the transition: the journal is absent before, and
        names exactly one invocation after the Stop hook has actually run. Absent is an answer
        here, not a failure to look.

        And the invocation goes through the registration, not through this checkout's helper.
        The command is read out of the hook file the installer wrote and executed as a program,
        so the entry point, the settings argument it carries and the stdin contract are all on
        the path the row reports. Calling completion.run() here would have left every one of
        them untested while the row still reached "1".
        """
        with tempfile.TemporaryDirectory() as temporary:
            before, after, registered, done = fire_the_hook(Path(temporary))

        self.assertEqual(len(registered), 1,
                         "the installer registered " + str(len(registered)) + " commands for"
                         " this adapter, so what fired is not settled")
        self.assertIn(completion.ENTRY_POINT_NAME, registered[0]["command"],
                      "the registered command names the adapter entry point")
        self.assertIsNotNone(registered[0]["settings"],
                             "the registration carries the settings path the install chose,"
                             " rather than leaving it to be resolved again at every Stop")
        self.assertEqual(done.returncode, 0, done.stderr[-400:])
        self.assertEqual(done.stdout, b"", "an observing hook holds no turn")

        self.assertEqual(read("hookCallback", {"hook-status": before})["value"]["value"],
                         reading.ABSENT,
                         "before any invocation the journal is established absent")
        self.assertEqual(read("hookCallback", {"hook-status": after})["value"]["value"], "1",
                         "and afterwards it names the one invocation that happened")

    def test_the_model_and_permission_row_is_not_taken_from_a_neighbouring_verdict(self):
        """The row most likely to be filled by something that sounds close enough.

        settingsPreserved answers whether a command that writes nothing left config.toml alone.
        It is true of a diagnosis no matter what the model keys say, so it cannot answer this.
        The case changes the model and permission keys underneath and requires this row to move
        while that verdict does not.
        """
        earlier = MODEL_PERMISSION_BLOCK
        later = earlier.replace('approval_policy = "never"', 'approval_policy = "on-request"')
        # Through read() like every other cell: the claim is about the row, not about the
        # helper, and a row that only its own test can reach is not the row the table fills.
        moved = read("modelPermissionPreservation",
                     {ACCEPTANCE: _model_permission_delta(earlier, later)})
        still = read("modelPermissionPreservation",
                     {ACCEPTANCE: _model_permission_delta(earlier, earlier)})

        if not HAS_READER:
            self.assertEqual(moved["value"], reading.UNREADABLE)
            self.assertEqual(still["value"], reading.UNREADABLE)
            return
        self.assertEqual(still["value"], PRESERVED)
        self.assertEqual(moved["value"], CHANGED,
                         "a permission posture that moved is not a preserved one")



    def test_without_a_reader_the_row_reports_itself_unread_rather_than_preserved(self):
        """Checked on both jobs rather than only on the one where it bites.

        The supported floor has no TOML reader, so on that job this row cannot be taken. The
        failure worth preventing is not the missing reader: it is a row that reports itself
        readable anyway, which would be visible on exactly one interpreter and green on the
        other. Simulating the absence here states the property on both.
        """
        with mock.patch.object(sys.modules[__name__], "HAS_READER", False):
            row = read("modelPermissionPreservation",
                       {ACCEPTANCE: _model_permission_delta(MODEL_PERMISSION_BLOCK,
                                                            MODEL_PERMISSION_BLOCK)})

        self.assertFalse(row["readable"])
        self.assertEqual(row["value"], reading.UNREADABLE)
        self.assertNotEqual(row["value"], PRESERVED,
                            "two configurations that happen to be identical are still not a"
                            " preservation anybody read")
        self.assertIn("tomllib", row["detail"])
    def test_changing_one_condition_moves_only_the_reading_that_asked_about_it(self):
        """Independence at the input, which the accessor cases cannot reach.

        Moving a value inside a payload proves this table reads the path it declared. It does
        not prove the command computed those values separately: a diagnosis deriving exposure
        from the import result would still write both keys, and every accessor case would stay
        green. Varying an input is not enough either if only the EVIDENCE moves -- a verdict
        copied from a neighbour survives that too.

        So each direction moves a judgement. Registering the bridge and observing its identity
        tool takes exposure from not_verified to verified; asking for a trial takes delivery
        from not_applicable to not_verified. In each direction the answers that were not asked
        about must come back identical in value AND evidence, which a borrowed verdict cannot do.

        The exposure direction needs a configuration reader, because reaching verified means
        comparing the registered command, and the supported floor has none. That direction is
        therefore taken only where it can be taken. The delivery direction needs no reader and
        runs everywhere, so the orthogonality claim is exercised on both jobs rather than only
        on the newer one -- and where exposure cannot rise, the case still requires it to hold
        still while delivery moves.
        """
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary).resolve()
            _payloads, host = observe_all(root)
            bridge = base.component_of_for_test(host.data, runtime_install.MCP_NAME)
            registered = str(host.pointer_path / "bin" / bridge["consoleScript"])
            plain = diagnose_for(root, host)
            exposed = diagnose_for(root, host, "--bridge-command", registered,
                                   "--observed-tool", "get_capabilities")
            tried = diagnose_for(root, host, "--relay-command",
                                 str(delivering_relay(root, doctor=False)),
                                 "--trial", *trial_inputs(root),
                                 # The copy INSIDE the candidate, not the working tree. The
                                 # preflight asks the relay's own reader and predicate, so it
                                 # needs the package importable -- and handing it the checkout
                                 # would answer a question about an installation with source
                                 # that was never installed.
                                 import_path=installed_components(host))
            reached = diagnose_for(root, host, "--relay-command",
                                   str(answering_relay(root)))
            # Last, because it rebuilds the supplied interpreter: the readings above are taken
            # against the runtime that cannot import, which is what makes the move a move.
            able = diagnose_for(root, host, import_path=importable_runtime(host, root))

        # The exact transition, not merely a different value: not_applicable becoming verified,
        # or a verdict falling to unknown, would satisfy "it moved" while meaning the opposite.
        #
        # All four move, each in a direction of its own, so every one of them is checked against
        # neighbours that are moving elsewhere. A cell that never moves cannot catch a neighbour
        # copying into it, and it cannot tell a live reading from a command that quietly stopped
        # attempting one: both look like the same answer forever.
        directions = [
            # The last field says which neighbours are compared on their evidence as well as
            # their value. A completing trial needs a runtime that has the relay in it, and the
            # import row reads where that same runtime finds things -- so that one direction
            # moves the relay's import EVIDENCE by construction while its verdict stays put.
            # Naming the coupling is honest; comparing evidence there anyway would be asserting
            # that two questions with a shared input never touch, which is not true.
            ("deliveryAcceptance", tried, "not_applicable", "verified", False),
            ("appServerConnection", reached, "unknown", "verified", True),
            ("runtimeImport", able, "not_verified", "verified", True),
        ]
        if HAS_READER:
            directions.append(("mcpToolExposure", exposed, "not_verified", "verified", True))
        else:
            without = read("mcpToolExposure", {"diagnose": exposed})["value"]
            self.assertEqual(without["value"], "not_verified",
                             "with no reader for the configuration the registered command cannot"
                             " be compared, so exposure stays unverified -- and it must stay so"
                             " for that reason rather than rise on a tool list alone")

        for moved_cell, varied, was_expected, now_expected, compare_evidence in directions:
            with self.subTest(moved_cell):
                before = read(moved_cell, {"diagnose": plain})["value"]
                after = read(moved_cell, {"diagnose": varied})["value"]
                self.assertEqual((before["value"], after["value"]),
                                 (was_expected, now_expected),
                                 moved_cell + ": the transition is the claim, and a verdict that"
                                 " moved somewhere else is not the claim being made")
                for other in CHECK_FIELD_ROWS:
                    if other == moved_cell:
                        continue
                    with self.subTest(other):
                        was = read(other, {"diagnose": plain})["value"]
                        now = read(other, {"diagnose": varied})["value"]
                        compared = ("value", "evidence") if compare_evidence else ("value",)
                        self.assertEqual(tuple(now[key] for key in compared),
                                         tuple(was[key] for key in compared),
                                         other + " moved when only " + moved_cell + " was asked"
                                         " about, so it is not answering on its own reading")
    def test_the_diagnosis_reads_nothing_outside_the_directory_it_was_given(self):
        """Pointing --state somewhere temporary is not isolation, so this checks the result.

        The survey runs discovery of its own and the filesystem side reads HOME and
        XDG_STATE_HOME, so a run that only redirected --state would still report a path on the
        host that ran it. Every path this diagnosis reports has to be inside the directory it
        was handed, which is also what keeps this suite away from an installed runtime.
        """
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary).resolve()
            payloads, _host = observe_all(root)
        scope = payloads["diagnose"].get("scope") or {}
        reported = [scope.get("stateDirectory"), scope.get("databasePath"),
                    payloads["diagnose"].get("codexHome"),
                    (payloads["diagnose"].get("mcpRegistration") or {}).get("path")]

        for where in reported:
            if where is None:
                continue
            with self.subTest(where):
                self.assertTrue(inside(where, root),
                                where + " is outside the temporary directory")

    def test_the_diagnosis_does_not_import_or_run_what_the_host_has_installed(self):
        """The other way out, which redirecting paths does not close.

        Resolving where a module lives imports it, and the bridge smoke script starts a server,
        both under whatever interpreter the record names. Clearing PYTHONPATH and the user site
        does not reach a system site directory, and checking the resolved location afterwards is
        too late: by then the import has happened. So the assertion is on the interpreter, which
        is decided before any of it runs. Every component has to be asked through the runtime
        this suite supplied; if one is asked through the interpreter running these tests, the
        probe could reach this machine and that fails here whatever it happened to find.
        """
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary).resolve()
            payloads, host = observe_all(root)
            supplied = host.candidate / "bin" / "python"
            present = supplied.is_file()
            answers, refusal = hermetic_answers(supplied, root) if present else (None, "absent")
            # The console scripts are run as programs by the survey, so their first line is what
            # decides the interpreter for those invocations. The record does not govern that
            # one: a shebang changed back to this machine's interpreter would leave every
            # recorded path reading correctly while the survey ran outside the supplied runtime.
            shebangs = {c["consoleScript"]:
                        (host.candidate / "bin" / c["consoleScript"]).read_text(
                            encoding="utf-8").splitlines()[0]
                        for c in host.data["components"]}
        components = payloads["diagnose"].get("components") or {}

        self.assertTrue(components, "the diagnosis reported no component at all")
        self.assertTrue(present, "the supplied runtime is not there at all")
        self.assertIsNone(refusal, str(refusal))
        for flag, wanted in HERMETIC_FLAGS.items():
            self.assertEqual(answers.get(flag), wanted,
                             "the supplied interpreter answered " + repr(answers.get(flag))
                             + " for " + flag + ", so it is not hermetic and every path this"
                             " case reads could be right while the probe ran with this"
                             " machine's site directory and import path")
        self.assertEqual(answers.get("smuggled"), [],
                         "an import path planted in the environment reached the interpreter the"
                         " probes run, which is the thing the flags were supposed to prevent")
        for script, first in shebangs.items():
            with self.subTest(script):
                self.assertEqual(first, "#!" + str(supplied),
                                 script + " is run as a program, and its first line sends it to "
                                 + first + " rather than to the supplied runtime")
        for name, component in components.items():
            with self.subTest(name):
                asked = component.get("interpreterPath")
                self.assertIsNotNone(asked, name + ": no interpreter was named at all")
                self.assertEqual(Path(asked), supplied,
                                 name + " was probed through " + str(asked) + " rather than the"
                                 " runtime this suite supplied")
                self.assertNotEqual(Path(asked), Path(sys.executable).resolve())
                where = component.get("importedLocation")
                if where is None:
                    continue
                self.assertTrue(inside(where, root),
                                name + " was imported from " + where + ", which is installed on"
                                " this host rather than in the destination under test")


class DistinctionTests(unittest.TestCase):
    """Five conditions that must not collapse into each other.

    Each is read by the thing that answers it, and the case asserts that the five answers are
    five different values rather than five spellings of "something is wrong". A host told only
    that an install refused cannot tell whether it is looking at somebody else runtime, a
    registration nobody wrote, a version that moved, a configuration already claimed, or a store
    it could not open -- and those five need five different actions.
    """

    def signals(self, **overrides):
        declared = dict(entry_point_recorded=True, commit_matches=True, tree_matches=True,
                        working_tree_clean=True, digest_matches=True, has_point=True)
        declared.update(overrides)
        return ownership.Signals(**declared)

    def test_an_external_installation_and_a_fork_are_not_the_same_answer(self):
        """An entry point nothing recorded is somebody else. A dirty checkout is ours, changed."""
        foreign = ownership.classify(self.signals(entry_point_recorded=False))[0]
        fork = ownership.classify(self.signals(working_tree_clean=False))[0]

        self.assertEqual(foreign, ownership.FOREIGN)
        self.assertEqual(fork, ownership.FORK)
        self.assertNotEqual(foreign, fork)
        self.assertFalse(ownership.reusable(foreign))
        self.assertFalse(ownership.reusable(fork))

    @base.needs_reader
    def test_a_missing_registration_and_a_claimed_one_are_not_the_same_answer(self):
        """Not registered and registered to somebody else are two answers, not one refusal.

        What "not registered" is called depends on what was asked. Asked with a command, the
        reading answers what registering would do -- CREATED, and it says so by also answering
        that it would write. Asked without one, there is nothing to compare and the answer is
        the absence itself. Neither is the answer a configuration already naming another
        command gives, and that is the distinction a host acts on.
        """
        with tempfile.TemporaryDirectory() as temporary:
            home = Path(temporary)
            home.joinpath("config.toml").write_text(FOREIGN_TABLE, encoding="utf-8")
            missing = runtime_install.registration_state(home, "/opt/bridge", [])
            unasked = runtime_install.registration_state(home, None, [])
            home.joinpath("config.toml").write_text(
                FOREIGN_TABLE + "\n[mcp_servers." + runtime_install.MCP_NAME + "]\n"
                'command = "/somebody/elses/bridge"\n', encoding="utf-8")
            claimed = runtime_install.registration_state(home, "/opt/bridge", [])

        self.assertEqual(missing["outcome"], codexconfig.CREATED,
                         "nothing is registered, so registering would create one")
        self.assertTrue(missing["wouldWrite"],
                        "and the reading says it would write, which is how CREATED is told"
                        " apart from a registration that is already there")
        self.assertEqual(unasked["outcome"], "ABSENT",
                         "asked without a command there is nothing to compare, and the answer"
                         " is the absence rather than a plan")
        self.assertEqual(claimed["outcome"], codexconfig.CONFLICT,
                         "a registration naming another command is a conflict, not an absence")
        self.assertFalse(claimed["wouldWrite"],
                         "a conflict is not something this command writes through")
        self.assertEqual(len({missing["outcome"], unasked["outcome"], claimed["outcome"]}), 3,
                         "three readings, three answers")

    def test_a_version_that_moved_is_not_a_source_that_moved(self):
        """The two drift findings a reader would otherwise have to tell apart by guessing."""
        self.assertEqual(definition.verify(ROOT), [],
                         "the committed definition describes this checkout, or nothing below"
                         " is a controlled comparison")

        moved = definition.load()
        moved["components"][0]["version"] = "0.0.0-never-released"
        findings = definition.verify(ROOT, definition=moved)

        self.assertTrue(any("version recorded" in finding for finding in findings), findings)
        self.assertFalse(any("sourceDigest" in finding for finding in findings),
                         "only the version moved, so only the version may be reported: a digest"
                         " finding here would mean the two conditions were read as one")

    def test_a_store_that_is_not_there_is_not_a_store_nobody_could_read(self):
        """The distinction the clean-host install depends on, kept here as a distinction."""
        schema = {"relationships": "CREATE TABLE relationships (relationship_id TEXT)"}
        candidate = {"readable": True, "tables": dict(schema)}
        absent = swapgate.tables_cell(
            {"readable": True, "present": False, "dbPath": "/nowhere/relay.sqlite3"}, candidate)
        unopenable = swapgate.tables_cell(
            {"readable": False, "present": None, "detail": "permission denied"}, candidate)

        self.assertEqual(absent["answer"], swapgate.NO_STORE)
        self.assertTrue(absent["readable"], "absence is an answer that was read")
        self.assertFalse(unopenable["readable"],
                         "a store that could not be opened established nothing")
        self.assertNotEqual(absent["answer"], unopenable["answer"])

    def test_the_five_conditions_are_five_different_answers(self):
        """Stated once, over all five, so that two of them collapsing is a failure here."""
        schema = {"relationships": "CREATE TABLE relationships (relationship_id TEXT)"}
        answers = {
            "external": ownership.classify(self.signals(entry_point_recorded=False))[0],
            "fork": ownership.classify(self.signals(working_tree_clean=False))[0],
            "configuration claimed": ownership.classify(
                self.signals(registration_conflict="registered with another command"))[0],
            "store unreadable": swapgate.tables_cell(
                {"readable": False, "present": None}, {"readable": True, "tables": schema}
            )["answer"],
            "store absent": swapgate.tables_cell(
                {"readable": True, "present": False}, {"readable": True, "tables": schema}
            )["answer"],
        }
        self.assertEqual(len(set(answers.values())), len(answers),
                         "two conditions answering the same value cannot be told apart: "
                         + json.dumps(answers))

    @base.needs_reader
    def test_a_registration_that_writes_preserves_every_other_setting(self):
        """The half a classifier call cannot establish: a command that actually writes."""
        with tempfile.TemporaryDirectory() as temporary:
            home = Path(temporary) / "codex"
            home.mkdir()
            original = MODEL_PERMISSION_BLOCK + "\n" + FOREIGN_TABLE
            (home / "config.toml").write_text(original, encoding="utf-8")
            arguments = ["register-mcp", "--codex-home", str(home),
                         "--bridge-command", "/opt/bridge", "--apply"]
            first = base.run(*arguments)
            after_first = (home / "config.toml").read_text(encoding="utf-8")
            second = base.run(*arguments)
            after_second = (home / "config.toml").read_text(encoding="utf-8")

        self.assertEqual(first.returncode, 0, first.stdout + first.stderr)
        self.assertEqual(json.loads(first.stdout)["outcome"], "CREATED")
        self.assertTrue(after_first.startswith(original),
                        "the settings it was handed are still there, unchanged, first")
        self.assertEqual(json.loads(second.stdout)["outcome"], "LINKED",
                         "a second registration recognises its own rather than appending")
        self.assertEqual(after_second, after_first,
                         "and writes nothing the second time")

    def test_a_hook_installation_leaves_a_hook_that_was_already_there(self):
        """Runtime hooks live in their own file, which no other snapshot in this suite reads."""
        with tempfile.TemporaryDirectory() as temporary:
            home = Path(temporary)
            (home / "hooks.json").write_text(json.dumps(FOREIGN_HOOKS), encoding="utf-8")
            arguments = argparse_namespace = base.argparse.Namespace(
                codex_home=str(home), event=None, hook_command=None, adapter="completion",
                dest=None, relay_command=str(home / "codex-session-relay"),
                marker_root=str(home / "marker"), db_path=None,
                journal_root=str(home / "journal"), python=sys.executable,
                mode=completion.OBSERVE, guard_timeout=5, timeout=10, issue="CRW-69",
                apply=True)
            with mock.patch.object(runtime_install, "emit"):
                runtime_install.cmd_hook(arguments)
            document = json.loads((home / "hooks.json").read_text(encoding="utf-8"))

        groups = document["hooks"]["Stop"]
        commands = [entry.get("command")
                    for group in groups for entry in group.get("hooks", [])]
        self.assertIn("/opt/cxc/stop", commands,
                      "the hook that was already registered is still registered")
        self.assertGreater(len(commands), 1, "and the new one was appended beside it")


class DaemonStateTests(unittest.TestCase):
    """A daemon that was running, one that was not, and a handover that was in progress.

    The swap is refused while a daemon is running and while attempts are open, and those are two
    different readings that happen to produce the same refusal. Reading one from the other would
    be invisible, because both stop the update. So each case asserts the cell that answered, and
    the case that could not read the daemon at all asserts the third answer: a reading that
    failed is not a daemon that is running, and it is not one that is stopped either.

    Nothing in this class starts, stops, enables or registers anything. The installer does not
    either, which is why a successful install is never reported as an activation.
    """

    def _update_with(self, **injected):
        with tempfile.TemporaryDirectory() as temporary:
            host = base._Host(temporary)
            clean(host)
            install(host)
            installed = host.candidate
            settled = host.snapshot()
            arriving_source(host)
            loaded, verified = updating(host)
            with loaded, verified:
                code, payload = install(host, **injected)
            after = host.snapshot()
            reached = pointer.read(host.pointer_path).get("target")
        return code, payload, settled, after, installed, reached

    def test_a_daemon_that_was_not_running_does_not_stop_the_update(self):
        code, payload, _settled, _after, _installed, reached = self._update_with()
        cells = (payload.get("swapGate") or {}).get("cells") or {}

        self.assertEqual(code, 0, json.dumps(payload)[:800])
        self.assertTrue(payload["promoted"])
        self.assertEqual(cells["daemon"]["answer"], scope.STOPPED)
        self.assertEqual(reached, str(payload["environment"]),
                         "the update that was allowed to happen did happen")

    def test_a_daemon_that_was_running_stops_it_and_keeps_what_was_there(self):
        code, payload, settled, after, installed, reached = self._update_with(
            gate="running daemon")
        cells = (payload.get("swapGate") or {}).get("cells") or {}

        self.assertEqual(code, 1)
        self.assertEqual(payload["failedStep"], GATE_REFUSAL)
        self.assertEqual(payload["swapGate"]["verdict"], swapgate.BLOCKED)
        self.assertEqual(cells["daemon"]["answer"], scope.RUNNING)
        self.assertEqual(cells["inFlight"]["answer"], swapgate.NO_ATTEMPTS,
                         "the running daemon refused this, and the in-flight cell answered its"
                         " own question rather than agreeing with it")
        self.assertEqual(after, settled)
        self.assertEqual(reached, str(installed))

    def test_a_handover_in_flight_is_preserved_rather_than_swapped_under(self):
        code, payload, settled, after, installed, reached = self._update_with(
            gate="handover in flight")
        cells = (payload.get("swapGate") or {}).get("cells") or {}

        self.assertEqual(code, 1)
        self.assertEqual(payload["swapGate"]["verdict"], swapgate.BLOCKED)
        self.assertEqual(cells["inFlight"]["answer"], 3,
                         "the open attempts are counted, not rounded to a refusal")
        self.assertEqual(cells["daemon"]["answer"], scope.STOPPED,
                         "and the daemon cell still answers its own question")
        self.assertEqual(after["storeRows"], settled["storeRows"],
                         "the attempts that were in flight are still in the store")
        self.assertEqual(after["storeInode"], settled["storeInode"])
        self.assertEqual(reached, str(installed))

    def test_a_daemon_that_could_not_be_read_is_neither_running_nor_stopped(self):
        code, payload, settled, after, _installed, _reached = self._update_with(
            gate="unreadable daemon")
        cells = (payload.get("swapGate") or {}).get("cells") or {}

        self.assertEqual(code, 1)
        self.assertEqual(payload["swapGate"]["verdict"], swapgate.UNESTABLISHED,
                         "a question that could not be answered did not answer no")
        self.assertEqual(cells["daemon"]["answer"], reading.ACCESS_ERROR)
        self.assertNotIn(cells["daemon"]["answer"], (scope.RUNNING, scope.STOPPED))
        self.assertFalse(cells["daemon"]["readable"])
        self.assertEqual(after, settled)

    def test_an_install_that_succeeded_is_not_reported_as_an_activation(self):
        """Installed, running and always-on are three claims, and this command makes one."""
        with tempfile.TemporaryDirectory() as temporary:
            payloads, _host = observe_all(Path(temporary))
        always = read("alwaysActive", payloads)

        self.assertTrue(always["readable"])
        self.assertEqual(always["value"]["value"], "not_verified",
                         "no supervised runtime was enabled and no host restart was observed,"
                         " so a successful install still does not establish this")
        self.assertNotEqual(always["value"]["value"], "verified")


# Every seam the update fixture stands in for, derived from the fixture rather than listed from
# memory: a stand-in added there has to be acknowledged here before this suite can describe what
# it exercised. That is the whole purpose of the declaration -- a provenance record that a later
# change can silently outgrow is worse than none.
FAKED_IN_FIXTURE = ("candidate_tables", "classify_component", "emit", "interpreter_version",
                    "load", "measure_candidate", "module_location", "names", "ops12_digest",
                    "place", "relay", "run", "store_presence", "store_tables", "verify")


def _stand_ins():
    """The names the update fixture replaces, observed while it is replacing them.

    Read out of the fixture's source this was a claim about how the fixture is written. A
    replacement applied some other way, or one applied conditionally and spelled differently,
    was simply not seen -- and the provenance record would then understate what stood in for a
    real thing while still looking derived.

    So the fixture is run and asked. _run calls interpose from inside the patched measurement,
    which is after every patch in that run has been entered, so the modules can be inspected
    for the attributes that are not themselves at that moment. The two pointer replacements are
    only applied for their own injected failures, so those runs are made too and the answers
    are taken together.
    """
    watched = ("subprocess", "definition", "scope", "pointer")
    found = set()

    def sample():
        for name, value in list(vars(runtime_install).items()):
            if isinstance(value, mock.NonCallableMock):
                found.add(name)
        for module_name in watched:
            module = getattr(runtime_install, module_name, None)
            for name, value in list(vars(module).items()) if module else []:
                if isinstance(value, mock.NonCallableMock):
                    found.add(name)

    for breaking in (None, "replace the owned pointer", "read the owned pointer back"):
        with tempfile.TemporaryDirectory() as temporary:
            host = base._Host(temporary)
            clean(host)
            # Inside the same context the composed lifecycle uses for its failed update. Run
            # without it, the sampling never sees the definition replacements that stage makes,
            # and the inventory would understate what stood in for a real thing while still
            # looking derived.
            loaded, verified = updating(host)
            with loaded, verified:
                install(host, breaking=breaking, interpose=sample)
    return found


# The switches the update fixture's run accepts, each with what handing it means here. DERIVED
# from the fixture's own signature, so a switch added there arrives unclassified rather than
# silently available.
#
# REFUSED_INJECTIONS names the one this module must never hand. clean_store tells the run the
# store is absent and its tables unknown, and the fixture has built a populated one at the same
# path -- so a regression that detects a store and then loses it passed, because the run under it
# had been told there was no store to lose.
REFUSED_INJECTIONS = ("clean_store",)
INJECTIONS = {
    "breaking": "a build step is made to fail at a named boundary, which is how every recovery"
                " claim here is exercised. It stands in for a build that really failed, and"
                " install's entry below says what that costs.",
    "gate": "a swap-gate reading is made to refuse: a running daemon, an open handover, a"
            " daemon nobody could read, or a schema the candidate would narrow.",
    "interpose": "a callback the fixture runs inside the patched measurement. It reads what is"
                 " patched at that moment and changes nothing.",
    "probes": "a list the store probe appends the interpreter it was asked through to. It"
              " records and changes nothing.",
    "clean_store": "REFUSED: it reports the store absent and empty to the run while the fixture"
                   " has built a populated one at that path, so what the run is told and what"
                   " the scenario built disagree.",
}

# What a function of this module hands the thing under test, and whether it is what this scenario
# built.
#
# Three findings were one finding wearing three faces -- a row read through a helper beside the
# registered command, a row read against a second Codex home, a run told its store was absent
# while the fixture had populated one. Each repair was the instance. The class is that something
# handed to the thing under test was not what the scenario built and the difference was nowhere
# in the claim, so the difference is written down here and the inventory is DERIVED by asking the
# module for its functions.
#
# What the derivation sees: every function defined in this file, whether or not anything calls
# it. That is how the one nothing called was found.
#
# What it does not see, and a reader should not believe it does: a value written inline inside a
# function body -- the granularity is the function, so a function declared BUILT that later hands
# a stand-in from its own body keeps a sentence that no longer fits; method names are flat, so
# the same name in two classes would be one entry; and the imported fixture's own replacements
# are not here at all, because FAKED_IN_FIXTURE covers those and observes them being applied.
BUILT = "the value this scenario built"
NOTHING = "nothing of its own: what it hands is classified elsewhere in this table"

HANDED = {
    # The composed scenarios, and the cases over them. Each hands what the helpers below hand
    # and what INJECTIONS classifies, and nothing of its own.
    "succeeding": NOTHING,
    "observe_all": NOTHING,
    "_stand_ins": NOTHING,
    "_fired": NOTHING,
    "_update_with": NOTHING,
    "setUp": NOTHING,
    "_functions_here": NOTHING,

    # The derivations behind the two declared lists, and the synthetic modules that control them.
    # None of these reaches the thing under test: they read this file's own text and this
    # module's own objects.
    "_dotted": NOTHING,
    "_places": NOTHING,
    "_module_statements": NOTHING,
    "_reachable": NOTHING,
    "_bindings": NOTHING,
    "_held_by_class": NOTHING,
    "_hands_on": NOTHING,
    "_occurrences": NOTHING,
    "_refusal_spelled": NOTHING,
    "_source_spelled": NOTHING,
    "_handle_names": NOTHING,
    "refusal_spellings": NOTHING,
    "source_spellings": NOTHING,
    "refusals_reached": NOTHING,
    "source_text_reached": NOTHING,
    "planted_source": NOTHING,
    "asked_over": NOTHING,

    # The accessor, and readings taken after the fact. None of these reaches the thing under
    # test: they read a payload, a path or a file that already exists.
    "read": NOTHING,
    "table": NOTHING,
    "_row": NOTHING,
    "_unreadable": NOTHING,
    "inside": NOTHING,
    "hooks_path": NOTHING,
    "settings_bytes": NOTHING,
    "_configuration_keys": NOTHING,
    "_model_permission_delta": NOTHING,

    # State this scenario really builds, handed as itself.
    "clean": BUILT,
    "seed_settings": BUILT,
    "isolated": BUILT,
    "diagnose_for": BUILT,
    "linked_skills": BUILT,
    "installed_components": BUILT,

    # Stand-ins: what the scenario has instead, and what a row reading through it cannot say.
    "install": (
        "the update fixture's run, in which the two build steps, the relay, the measurement, the"
        " component classification and the store readings are replaced. FAKED_IN_FIXTURE names"
        " all fifteen and _stand_ins observes them while they are applied; the store readings"
        " now describe the store the fixture built, which the agreement case checks",
        "that a venv builds, that pip installs, that a live daemon or a live store answers, or"
        " that the interpreter version the record keeps is one a built environment reported"),
    "arriving_source": (
        "a source change, made by moving the digests on the fixture's own copy of the definition."
        " The fixture derives its measured digests from that copy, so moving only what load()"
        " returns makes the run refuse for a disagreement about the fixture instead of reaching"
        " the boundary under test",
        "that a real second checkout is what produces a different candidate directory, or that"
        " the committed definition detects one"),
    "updating": (
        "the arriving definition, for the length of one run: definition.load and definition.verify"
        " answer for it, because verify re-derives the component trees from git in THIS checkout"
        " and would refuse the moved digests",
        "that a real definition file and a real verification accept an arriving source"),
    "stand_in_runtime": (
        "the recorded interpreter, written here as a wrapper around this interpreter with -S -E."
        " The build step is a stand-in, so no built environment has an interpreter of its own,"
        " and a probe left to fall back would import and run whatever THIS machine has installed",
        "that an interpreter a real build produced resolves these components; what it does"
        " establish is asked of the process rather than read off the wrapper, in HERMETIC_FLAGS"),
    "importable_runtime": (
        "the same recorded interpreter rebuilt to keep -S and drop -E, so the import path this"
        " suite chooses is the only place it can look. Called with paths=None it also writes"
        " empty stub packages, which is a directory that can be imported and nothing more",
        "that a real installation is what the import resolved, unless the caller hands it the"
        " installed copy -- which succeeding() does and the stub direction does not"),
    "standin_relay": (
        "the relay the registered hook calls, written here to answer the guard. No relay is"
        " installed in a destination whose build steps were stand-ins",
        "that an installed relay answers the guard, or that its answer is this one"),
    "answering_relay": (
        "the relay whose doctor reports a socket it reached. No App Server runs here, and the"
        " connection question has no input of its own on the command line",
        "that a live App Server socket is reachable; READING_PATHS states the same narrowing"
        " for the row itself"),
    "delivering_relay": (
        "the relay that answers every step of a trial, so a delivery can complete at all",
        "that a live relay completes a delivery; READING_PATHS states the same narrowing for"
        " the row"),
    "trial_inputs": (
        "the inputs a trial requires, written here in full. On a host an operator supplies these,"
        " so there is nothing for a scenario to have built -- but they are supplied, and a row"
        " reading through them is reading an answer to input this suite chose",
        "that the values a host would supply are these, or that a delivery anybody else asked"
        " for would be accepted"),
    "fire_the_hook": (
        "the registration and the command line are the ones cmd_hook wrote into this Codex home,"
        " read back out of that file and executed as a program. What is supplied rather than"
        " built is the Stop payload on its stdin, which is a recorded one replayed because no"
        " Codex turn happens here, and the installer arguments an operator would choose",
        "that a live turn produces this payload, or that an operator's own arguments register"
        " this command line"),
    "hermetic_answers": (
        "a directory planted on PYTHONPATH that nothing ever creates, so the probe is whether an"
        " inherited path reaches the interpreter at all",
        "that a real importable package on an inherited path would be refused -- only that no"
        " inherited entry arrives"),
    "signals": (
        "the ownership signals, written here rather than gathered from a host. The real gatherer"
        " is classify_component, which the fixture replaces, so a scenario that built a foreign"
        " entry point on disk still could not reach the classifier through a run",
        "that a foreign installation on disk produces these signals; what the row establishes is"
        " that the classifier answers the five conditions differently"),

    # Cases that hand something themselves rather than through a helper.
    "test_a_store_that_is_not_there_is_not_a_store_nobody_could_read": (
        "the two presence readings, written here, because the distinction being drawn is between"
        " a store that is absent and one that could not be opened -- and a scenario cannot build"
        " the second without making a path unopenable for the whole run",
        "that a real absent store and a real unopenable one produce these readings"),
    "test_the_five_conditions_are_five_different_answers": (
        "the same written signals and presence readings as the two cases it generalises",
        "that five real host conditions produce these inputs; what it establishes is that the"
        " five answers do not collapse into each other"),
    "test_a_version_that_moved_is_not_a_source_that_moved": (
        "a version moved in a loaded definition, rather than a component actually released at a"
        " different version",
        "that a released version change is reported this way; the verification it is read"
        " through is the committed one, run against this checkout"),
    "test_the_model_and_permission_row_is_not_taken_from_a_neighbouring_verdict": (
        "two configuration texts handed straight to the producer, because the claim is that the"
        " row moves when the posture moves. The two sides of a real install are read in the"
        " success case instead",
        "that an install produces these two texts"),
    "test_without_a_reader_the_row_reports_itself_unread_rather_than_preserved": (
        "the absence of a configuration reader, simulated, so the property is stated on both"
        " supported interpreters rather than only on the one where it bites",
        "that the floor interpreter behaves this way -- which is why the vocabulary and success"
        " cases assert the same refusal unsimulated, and only on that job"),
    "test_a_hook_installation_leaves_a_hook_that_was_already_there": (
        "the foreign hook file is really written and cmd_hook really runs over it; what is"
        " supplied is the argument namespace an operator would choose",
        "that an operator's own arguments append beside a foreign hook the same way"),
    "test_a_missing_registration_and_a_claimed_one_are_not_the_same_answer": BUILT,
    "test_a_registration_that_writes_preserves_every_other_setting": BUILT,

    # Cases that hand nothing of their own: they run a scenario above, or mutate a RESULT to show
    # a reading was load-bearing, which is the opposite direction from handing a substitute in.
    "test_a_new_install_a_rerun_a_failed_update_and_the_recovery_of_what_it_replaced": NOTHING,
    "test_the_rows_seeded_before_the_first_install_survive_every_stage": NOTHING,
    "test_a_failed_update_that_never_reached_its_seam_is_not_a_recovery": NOTHING,
    "test_a_restoration_it_could_not_read_back_is_reported_rather_than_claimed": NOTHING,
    "test_the_store_the_run_is_told_about_is_the_store_the_fixture_built": NOTHING,
    "test_a_daemon_that_was_not_running_does_not_stop_the_update": NOTHING,
    "test_a_daemon_that_was_running_stops_it_and_keeps_what_was_there": NOTHING,
    "test_a_handover_in_flight_is_preserved_rather_than_swapped_under": NOTHING,
    "test_a_daemon_that_could_not_be_read_is_neither_running_nor_stopped": NOTHING,
    "test_an_install_that_succeeded_is_not_reported_as_an_activation": NOTHING,
    "test_an_external_installation_and_a_fork_are_not_the_same_answer": NOTHING,
    "test_every_question_has_exactly_one_declared_reading": NOTHING,
    "test_no_two_cells_read_the_same_answer": NOTHING,
    "test_every_declared_producer_exists_where_it_is_declared": NOTHING,
    "test_every_text_reading_place_is_declared": NOTHING,
    "test_every_declared_path_is_one_its_source_actually_writes": NOTHING,
    "test_no_cell_is_filled_without_going_through_the_declared_reading": NOTHING,
    "test_every_row_is_readable_and_answers_in_its_own_vocabulary": NOTHING,
    "test_every_question_is_declared_to_answer_in_exactly_one_vocabulary": NOTHING,
    "test_every_acceptance_row_reaches_its_success_answer": NOTHING,
    "test_every_place_that_settles_for_a_refusal_is_declared": NOTHING,
    "test_every_reading_declares_the_path_it_travels": NOTHING,
    "test_a_row_that_cannot_require_success_says_why_absence_is_normal": NOTHING,
    "test_each_row_reads_its_own_path_and_no_other": NOTHING,
    "test_withholding_a_source_leaves_only_its_own_rows_unreadable": NOTHING,
    "test_the_hook_row_counts_an_invocation_that_really_happened": NOTHING,
    "test_changing_one_condition_moves_only_the_reading_that_asked_about_it": NOTHING,
    "test_the_diagnosis_reads_nothing_outside_the_directory_it_was_given": NOTHING,
    "test_the_diagnosis_does_not_import_or_run_what_the_host_has_installed": NOTHING,
    "test_the_stand_ins_are_the_ones_the_fixture_actually_uses": NOTHING,
    "test_no_row_this_suite_fills_can_claim_a_host": NOTHING,
    "test_the_destination_this_suite_measured_is_recorded_as_temporary": NOTHING,
    "test_every_function_here_says_what_it_hands": NOTHING,
    "test_every_switch_the_fixture_accepts_is_classified": NOTHING,
    "test_no_call_site_hands_the_fixture_an_injection_this_module_refuses": NOTHING,
    "test_a_text_reading_place_arriving_in_a_form_the_old_sweep_missed_is_still_caught": NOTHING,
    "test_a_place_that_settles_for_a_refusal_in_a_form_the_old_sweep_missed_is_still_caught":
        NOTHING,
    "test_the_reach_of_each_derivation_is_the_declared_one": NOTHING,
    "test_the_forms_outside_each_derivations_reach_are_the_declared_ones": NOTHING,
    "test_no_two_places_this_module_declares_can_share_a_name": NOTHING,
}


def _functions_here():
    """Every function defined in this module, asked of the module rather than read out of it.

    Including the ones nothing calls: a helper left behind after its last caller went is still a
    thing this file offers, and the one that had been left behind was found this way.
    """
    module = sys.modules[__name__]
    found = set()

    def mine(value):
        return isinstance(value, types.FunctionType) and value.__module__ == __name__

    for name, value in vars(module).items():
        if mine(value):
            found.add(name)
        if isinstance(value, type) and value.__module__ == __name__:
            found.update(inner for inner, member in vars(value).items() if mine(member))
    return found


class ProvenanceTests(unittest.TestCase):
    """What this suite exercised, and what it is therefore not evidence about.

    A composed run that could describe itself as a host measurement would be worse than no
    composed run, because the record it produced would read like one nobody can reproduce. So
    the provenance is fixture, the set of things standing in for real ones is derived from the
    fixture instead of remembered, and no row this suite commits can claim to have been
    performed on a host.
    """

    def test_the_stand_ins_are_the_ones_the_fixture_actually_uses(self):
        self.assertEqual(_stand_ins(), set(FAKED_IN_FIXTURE),
                         "the fixture replaces something this record does not acknowledge, so"
                         " the record understates what was simulated")

    def test_no_row_this_suite_fills_can_claim_a_host(self):
        with tempfile.TemporaryDirectory() as temporary:
            payloads, _host = observe_all(Path(temporary))
        rows = table(payloads)

        self.assertIn(COMMITTED_PROVENANCE, PROVENANCE)
        self.assertNotEqual(COMMITTED_PROVENANCE, "host")
        for cell in SEVEN:
            with self.subTest(cell):
                self.assertEqual(rows[cell]["performedAgainst"], COMMITTED_PROVENANCE)

    def test_the_destination_this_suite_measured_is_recorded_as_temporary(self):
        """The record own word for it, so a reader is not relying on this docstring."""
        with tempfile.TemporaryDirectory() as temporary:
            payloads, _host = observe_all(Path(temporary))
        kind = read("destinationKind", payloads)

        self.assertTrue(kind["readable"])
        self.assertEqual(kind["value"], "temporary")
        self.assertIn("definitionVersion", payloads["diagnose"])
        self.assertIn("repositoryCommit", payloads["diagnose"],
                      "the revision a reader would need to reproduce this is in the payload")

    def test_every_function_here_says_what_it_hands(self):
        """The inventory, derived from the module rather than remembered.

        Asked of the module object, so a function nothing calls is still in it -- a stand-in
        whose last caller went is still a stand-in this file offers, and the one that had been
        left behind was found exactly this way.
        """
        self.assertEqual(_functions_here(), set(HANDED),
                         "a function here does not say what it hands the thing under test, or a"
                         " declaration outlived the function it described: "
                         + json.dumps(sorted(_functions_here() ^ set(HANDED))))

        for name, handed in sorted(HANDED.items()):
            with self.subTest(name):
                if handed in (BUILT, NOTHING):
                    continue
                self.assertIsInstance(handed, tuple,
                                      name + ": a stand-in is declared as what the scenario has"
                                      " instead and what a row through it cannot say")
                self.assertEqual(len(handed), 2, name + ": both halves are required")
                instead, cannot_say = handed
                self.assertTrue(instead.strip(),
                                name + " stands in for something without saying what the"
                                " scenario has instead")
                self.assertTrue(cannot_say.strip(),
                                name + " stands in for something without saying what a row"
                                " reading through it therefore does not prove")

    def test_every_switch_the_fixture_accepts_is_classified(self):
        """Asked of the fixture's own signature, so a switch added there arrives unclassified."""
        parameters = inspect.signature(base.UpdateRecoveryTests._run).parameters
        accepted = {name for name, parameter in parameters.items()
                    if parameter.kind is inspect.Parameter.KEYWORD_ONLY}

        self.assertEqual(accepted, set(INJECTIONS),
                         "the fixture accepts a switch nothing here classifies, or a"
                         " classification outlived the switch: "
                         + json.dumps(sorted(accepted ^ set(INJECTIONS))))
        for name in REFUSED_INJECTIONS:
            with self.subTest(name):
                self.assertIn(name, accepted, name + " is refused but nothing accepts it")
                self.assertTrue(INJECTIONS[name].startswith("REFUSED"),
                                name + " is refused without the table saying so")

    def test_no_call_site_hands_the_fixture_an_injection_this_module_refuses(self):
        """Every call in this file, because one green case cannot speak for the others.

        The agreement case below reads what a run was actually told, which is the behavioural
        half. It can only say that about its own run: the same switch handed at another call
        site would leave it green. So the call sites are taken together, and the subject here IS
        this file's own calls -- which is why reading it is the reading to take. Observing them
        all instead would mean running every scenario inside one case.
        """
        tree = ast.parse(HERE.read_text(encoding="utf-8"))
        refused = []
        to_the_fixture = set()
        for node in ast.walk(tree):
            if not isinstance(node, ast.Call):
                continue
            named = [keyword.arg for keyword in node.keywords if keyword.arg]
            refused.extend(name for name in named if name in REFUSED_INJECTIONS)
            if isinstance(node.func, ast.Name) and node.func.id == "install":
                to_the_fixture.update(named)

        self.assertEqual(refused, [],
                         "a call here hands the fixture a switch this module refuses: "
                         + repr(sorted(set(refused))))
        self.assertEqual(to_the_fixture - set(INJECTIONS), set(),
                         "a run is handed a switch nothing classifies: "
                         + repr(sorted(to_the_fixture - set(INJECTIONS))))
        self.assertTrue(to_the_fixture,
                        "no call hands the fixture anything, so this check is watching a door"
                        " nobody uses")
