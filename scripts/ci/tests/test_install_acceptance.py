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
"""

import ast
import copy
import hashlib
import json
import os
import shutil
import stat
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path
from unittest import mock

ROOT = Path(__file__).resolve().parents[3]
sys.path.insert(0, str(ROOT / "scripts"))
# Discovery puts this directory on the path; running this file on its own does not, and the
# composition is worth being able to run by itself while working on it.
sys.path.insert(0, str(Path(__file__).resolve().parent))

from crw_runtime import (check, codexconfig, completion, definition, hostrecord, ownership,
                         pointer, reading, scope, staging, swapgate)

import runtime_install
import test_runtime_install as base

RUNTIME = ROOT / "scripts" / "runtime_install.py"
COMPLETION_SOURCE = ROOT / "scripts" / "crw_runtime" / "completion.py"
CHECK_SOURCE = ROOT / "scripts" / "crw_runtime" / "check.py"
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

# Where a declared producer is resolved. The acceptance module is in this map on the same terms
# as the runtime modules: a producer this suite owns is checked exactly like one it does not,
# because a test-side reading exempt from the check is a reading nothing holds to anything.
PRODUCER_FILES = {
    "runtime_install": RUNTIME,
    "completion": COMPLETION_SOURCE,
    "check": CHECK_SOURCE,
    "test_install_acceptance": HERE,
}

# The sources that assemble each payload, and therefore the ones that have to still be writing
# the key this table reads out of it. A diagnosis is assembled by the command together with the
# result module it delegates the result shape to, so both are named rather than one standing in
# for the other.
EMITTING_FILES = {
    "diagnose": (RUNTIME, CHECK_SOURCE),
    "hook-status": (COMPLETION_SOURCE,),
    ACCEPTANCE: (HERE,),
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


def hook_settings(directory, **overrides):
    """The completion hook own settings, written where the hook will look for them."""
    document = completion.configuration(
        relay=str(Path(directory) / "codex-session-relay"),
        marker_root=str(Path(directory) / "marker"),
        journal_root=str(Path(directory) / "journal"),
        codex_home=str(directory), issue="CRW-69")
    document.update(overrides)
    completion.configuration_path(Path(directory)).write_text(
        json.dumps(document), encoding="utf-8")
    return document


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
    """Actually invoke the Stop hook once, and return what hook-status read before and after.

    The point of the row this feeds. A journal count read from a directory somebody pre-populated
    says nothing about a callback, because it counts files. So the hook is run, and the assertion
    is the transition -- established absence before, exactly one invocation after -- which only
    an invocation that really happened can produce.
    """
    standin_relay(directory, RELEASED)
    hook_settings(directory)
    before = completion.status(codex_home=str(directory), environ={})
    completion.run(json.dumps(STOP).encode("utf-8"), codex_home=str(directory), environ={})
    after = completion.status(codex_home=str(directory), environ={})
    return before, after


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
                    new, new_payload = install(host, clean_store=True)
                    installed = host.candidate
                    settled = host.snapshot()
                    reached = pointer.read(host.pointer_path).get("target")
                    record = hostrecord.load(host.record_path,
                                             host.data["definitionVersion"]).value or {}
                    selected = record.get("selected") or {}
                    claimed = (staging.read_claim(installed).value or {}).get("state")
                    after_new = sorted(p.name for p in host.destination.iterdir())

                    # Stage two: the same run again, which must build and write nothing.
                    repeat, repeat_payload = install(host, clean_store=True)
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
                    usable, usable_payload = install(host, clean_store=True)
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
            install(host, clean_store=True)
            arriving_source(host)
            loaded, verified = updating(host)
            with loaded, verified:
                install(host, breaking="install packages")
            closing = host.snapshot()

        self.assertEqual(closing["storeRows"], opening["storeRows"])
        self.assertEqual(closing["storeInode"], opening["storeInode"],
                         "the store was not moved or recreated")
        self.assertEqual(closing["storeBytes"], opening["storeBytes"])

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
            install(host, clean_store=True)
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
            install(host, clean_store=True)
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

def diagnose_for(root, host, *extra):
    """Ask this host for a diagnosis, with whatever extra input the case is varying."""
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
        env=dict(os.environ, **isolated(root, host)))
    return json.loads(done.stdout)


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
    install(host, clean_store=True)
    later = host.config.read_text(encoding="utf-8")

    stand_in_runtime(host)

    hook_directory = root / "hook"
    hook_directory.mkdir(exist_ok=True)
    _before, after = fire_the_hook(hook_directory)

    return {
        "diagnose": diagnose_for(root, host),
        "hook-status": after,
        ACCEPTANCE: _model_permission_delta(earlier, later),
    }, host


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
        declared = [row[0] for row in READINGS]
        self.assertEqual(sorted(declared), sorted(SEVEN),
                         "a question with no row cannot be answered, and a row nobody asked for"
                         " is an answer to nothing")
        self.assertEqual(len(set(declared)), len(declared), "each question is answered once")

    def test_no_two_cells_read_the_same_answer(self):
        places = [(row[1], row[2]) for row in DECLARED]
        self.assertEqual(len(set(places)), len(places),
                         "two cells reading one place is one reading answering two questions")

    def test_every_declared_producer_exists_where_it_is_declared(self):
        """Including the one this module owns, which is checked on the same terms."""
        for cell, _source, _path, producer in DECLARED:
            module, _, name = producer.rpartition(".")
            with self.subTest(cell):
                self.assertIn(module, PRODUCER_FILES, cell + ": no file resolves " + module)
                tree = ast.parse(PRODUCER_FILES[module].read_text(encoding="utf-8"))
                defined = {node.name for node in ast.walk(tree)
                           if isinstance(node, (ast.FunctionDef, ast.AsyncFunctionDef))}
                self.assertIn(name, defined, cell + ": " + producer + " does not exist")

    def test_the_key_each_row_reads_is_still_written_where_it_is_read_from(self):
        """A rename in the source fails here instead of leaving a row reading a dead key."""
        for cell, source, path, _producer in DECLARED:
            with self.subTest(cell):
                wanted = repr(path[-1]).replace("'", '"')
                written = any(wanted in where.read_text(encoding="utf-8").replace("'", '"')
                              for where in EMITTING_FILES[source])
                self.assertTrue(written,
                                cell + ": nothing that assembles " + source + " writes "
                                + repr(path[-1]))

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
        self.assertTrue("exitCode" in links or "unreadable" in links,
                        "a listing either ran or says it could not")

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
        """
        with tempfile.TemporaryDirectory() as temporary:
            before, after = fire_the_hook(Path(temporary))

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
            tried = diagnose_for(root, host, "--trial")
            reached = diagnose_for(root, host, "--relay-command",
                                   str(answering_relay(root)))

        # The exact transition, not merely a different value: not_applicable becoming verified,
        # or a verdict falling to unknown, would satisfy "it moved" while meaning the opposite.
        #
        # Three of the four move, so each of them is checked against neighbours that are moving
        # in some other direction. A cell that never moves anywhere cannot catch a neighbour
        # copying into it, which is the remaining way one reading stands in for another.
        directions = [
            ("deliveryAcceptance", tried, "not_applicable", "not_verified"),
            ("appServerConnection", reached, "unknown", "verified"),
        ]
        if HAS_READER:
            directions.append(("mcpToolExposure", exposed, "not_verified", "verified"))
        else:
            without = read("mcpToolExposure", {"diagnose": exposed})["value"]
            self.assertEqual(without["value"], "not_verified",
                             "with no reader for the configuration the registered command cannot"
                             " be compared, so exposure stays unverified -- and it must stay so"
                             " for that reason rather than rise on a tool list alone")

        for moved_cell, varied, was_expected, now_expected in directions:
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
                        self.assertEqual((now["value"], now["evidence"]),
                                         (was["value"], was["evidence"]),
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
            install(host, clean_store=True)
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
                    "measure_candidate", "module_location", "names", "ops12_digest", "place",
                    "relay", "run", "store_presence", "store_tables")


def _stand_ins():
    """The names the update fixture replaces, read out of its source."""
    tree = ast.parse((ROOT / "scripts" / "ci" / "tests" / "test_runtime_install.py")
                     .read_text(encoding="utf-8"))
    found = set()
    for node in ast.walk(tree):
        if not (isinstance(node, ast.ClassDef) and node.name == "UpdateRecoveryTests"):
            continue
        for item in node.body:
            if not (isinstance(item, ast.FunctionDef) and item.name == "_run"):
                continue
            for call in ast.walk(item):
                if (isinstance(call, ast.Call) and isinstance(call.func, ast.Attribute)
                        and call.func.attr == "object" and len(call.args) >= 2
                        and isinstance(call.args[1], ast.Constant)):
                    found.add(call.args[1].value)
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
