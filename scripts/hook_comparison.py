#!/usr/bin/env python3
"""The same scenarios at a destination that registered the completion hook and one that did not.

Adding a hook does not establish that fewer completions are missed, and two things here already
look like that establishment and are not. hook_probe.py replay checks the contract's decision table
against fixtures recorded beside it, and says of itself that it re-runs no hook and would keep
passing on a machine where hooks are switched off. The composed acceptance run establishes that one
registered hook fired once. Neither compares a destination carrying the hook against the same
destination without it.

This module runs that comparison, and four rules are the whole of it.

The arms differ by one switch. Both are built by runtime_install.py hook --adapter completion with
identical arguments; the off arm omits --apply, which is that command's own dry run. What runs at
each arm is then DERIVED from that arm's own hooks.json, never written out a second time here. At
the off arm the derivation returns no entry and nothing is executed, and that absence is the arm.
An off arm that called a helper to have something to compare would be comparing this file to itself.

A cell is filled by the reading its own question called for. CELLS declares the cell, the source
that answers it and the path the answer is read from, and read() is the only way a cell is ever
filled. A reading that could not be made answers UNREADABLE naming why; a reading whose absence is
a normal state answers ABSENT naming why it is normal there. Neither ever answers False, and
neither ever takes the value of the cell beside it.

Absence is declared in advance, per scenario and per arm, so the places where it is correct are
part of the scenario table rather than a discovery made while reading the results. Every cell that
reads ABSENT must have declared it, and every cell that declared it must read it.

An expectation is never edited by its own measurement. SCENARIOS names the observation, the
decision, the decision state, the printed answer and the reservation before the run, and a
disagreement is a failure of that row. Rewriting the expectation to match what happened would make
the thing under test its own oracle, and any wrong state could then be made green.

Nothing here is evidence about a host. Every path is under the root this run created, the relay is
the one in this checkout reached through a launcher this run writes, no daemon and no App Server is
involved, the Stop payloads are composed here rather than delivered, and STAND_INS records what
each of those means a row cannot prove. The criteria reported against are fixed in
skills/crw-run/references/hook-contract.md and are neither restated nor extended here; the ones
this arrangement cannot reach are reported as not performed. Procedure: docs/hook-comparison.md.
"""

import argparse
import hashlib
import json
import os
from pathlib import Path
import shlex
import shutil
import stat
import subprocess
import sys
import tempfile
import time

# Before the first import of anything in this repository. Importing writes __pycache__ beside the
# source unless this is set, and those are writes into the checkout by a command whose result says
# everything it writes goes under the directory it made for itself.
sys.dont_write_bytecode = True

ROOT = Path(__file__).resolve().parent.parent
sys.path.insert(0, str(ROOT / "scripts"))

from crw_runtime import completion, hooks, reading  # noqa: E402

RELAY_SOURCE = ROOT / "packages" / "codex-session-relay" / "src"
RUNTIME = ROOT / "scripts" / "runtime_install.py"

SOURCE = "hook-comparison"
HARNESS_VERSION = 1

# The relay declares this floor in its own metadata, and the guard is imported by the command this
# harness runs. Below it there is no run to report on, so the refusal is the answer and it is
# printed as one rather than degraded into rows nobody took.
RELAY_PYTHON = (3, 11)

OFF, ON = "off", "on"
ARMS = (OFF, ON)

# What each arm's install must report before anything it left behind is believed. An installer that
# fails before writing leaves exactly the empty Codex home a successful dry run leaves, so without
# this the off arm would pass on a run that never happened.
ARM_INSTALL = {
    OFF: {"installExit": 0, "installResult": "MISSING", "installSettings": "config_would_create",
          "registration": 0, "foreignRegistration": "present"},
    ON: {"installExit": 0, "installResult": "CREATED", "installSettings": "config_created",
         "registration": 1, "foreignRegistration": "present"},
}

# The one reason the off arm has nothing to read. Written once, because it is one fact about one
# arm and repeating it per cell is how two spellings of it end up disagreeing.
NO_REGISTRATION = "no entry under Stop names this adapter, because the install was a dry run"

# The answers each cell may give, in the vocabulary of the thing that answers. A host sees stdout,
# so printedBlock answers in what was printed; the marker root holds files, so its cells answer in
# whether the file is there. None of these is translated into another cell's vocabulary.
PRINTED_A_BLOCK = "printed_a_block"
PRINTED_NOTHING = "printed_nothing"
PRINTED_OTHER = "printed_something_else"
RESERVED = "reserved"
NOT_RESERVED = "not_reserved"
PUBLISHED = "published"
NOT_PUBLISHED = "not_published"
RESOLVED = "resolved"
NOT_RESOLVED = "named_but_missing"

# Sources. A payload carries the stamp of the source that produced it, and read() checks the stamp
# before walking a path: handing one source's payload to another source's row is the cheapest way
# for a reading to answer a question it was never asked.
INSTALL = "install"
HOOK_FILE = "hook-file"
JOURNAL = "journal"
STDOUT = "stdout"
MARKER_ROOT = "marker-root"
HARNESS = "harness"

# cell, source, the path the answer is read from, and what produces it.
CELLS = (
    ("installExit", INSTALL, ("exitCode",), "runtime_install.cmd_hook"),
    ("installResult", INSTALL, ("result", "outcome"), "hooks.install"),
    ("installSettings", INSTALL, ("settings", "outcome"), "completion.write_configuration"),
    ("registration", HOOK_FILE, ("entries",), "completion.adapter_entries"),
    ("foreignRegistration", HOOK_FILE, ("foreign",), "completion.adapter_entries"),
    ("firedCommand", HOOK_FILE, ("command",), "completion.command_for"),
    ("adapterOutcome", JOURNAL, ("adapterOutcome",), "completion.run"),
    ("observation", JOURNAL, ("observation",), "guard.observe_state"),
    ("guardDecision", JOURNAL, ("guardDecision",), "guard.decide"),
    ("guardState", JOURNAL, ("guardState",), "guard.decide"),
    ("printedBlock", STDOUT, ("printed",), "completion.hook_output"),
    ("recordedAs", JOURNAL, ("guardRecordedAs",), "guard.record_observation"),
    ("observationFile", MARKER_ROOT, ("observationFile",), "marker.publish"),
    ("heldFile", MARKER_ROOT, ("heldFile",), "guard.reserve_hold"),
    ("journalElapsedMs", JOURNAL, ("elapsedMs",), "completion.run"),
    ("processWallMs", HARNESS, ("wallMs",), "hook_comparison.fire"),
    ("processExit", HARNESS, ("exitCode",), "hook_comparison.fire"),
)

ARM_CELLS = ("installExit", "installResult", "installSettings", "registration",
             "foreignRegistration")
FIRING_CELLS = tuple(cell for cell, _s, _p, _q in CELLS if cell not in ARM_CELLS)

# Every answer that means there is nothing there, in the vocabulary of the source that gives it.
# An absent registration and an unpublished observation are both absences and neither is the other:
# the first is a payload that does not exist, the second is the marker root saying it looked. A
# place that declares an absence declares which of these it expects, and a check compares the two.
# None is here because the journal says "nothing was published" by carrying a null in the field
# rather than by leaving it out, and that is the guard's own way of saying it looked and there was
# nothing. Including it makes the rule stricter rather than looser: every cell that answers null
# must then be a place something declared an absence to be normal.
ABSENCE_ANSWERS = (reading.ABSENT, NOT_PUBLISHED, NOT_RESERVED, None)

# The answers that are a reading which did not happen rather than something a reading found. They
# are non-empty strings, so truthiness, uniqueness and "is anything there" all let them through,
# and every place that consumes a cell has to exclude them before asking its own question.
NOT_A_VALUE = (reading.UNREADABLE, reading.ACCESS_ERROR)

# A managed scenario is one whose workspace a marker names. Only there can the guard select an
# assignment, so only there is a published observation something to require.
MANAGED = "managed"
UNMANAGED = "unmanaged"


def scenario(name, kind, expected, absent=(), fire_twice=False, stop_hook_active=False,
             second=None):
    """One declared scenario. Every expectation is fixed here, before the run."""
    return {"name": name, "kind": kind, "expected": expected, "absent": dict(absent),
            "fireTwice": fire_twice, "stopHookActive": stop_hook_active, "second": second or {}}


# The state each scenario must reach, declared before the run. observation is what the turn WAS,
# guardDecision is block or release, and guardState is the decision state: hold_in_flight and
# unresolved_handoff are both a silent release carrying no new reservation, so a row that declared
# one and accepted the other would pass while the assignment had stopped converging.
SCENARIOS = (
    scenario("receipt_missing", MANAGED,
             {"observation": "receipt_missing", "guardDecision": "block",
              "guardState": "receipt_missing", "printedBlock": PRINTED_A_BLOCK,
              "processExit": 0, "heldFile": RESERVED, "recordedAs": PUBLISHED,
              "observationFile": RESOLVED}),
    scenario("managed_unregistered", MANAGED,
             {"observation": "managed_unregistered", "guardDecision": "block",
              "guardState": "managed_unregistered", "printedBlock": PRINTED_A_BLOCK,
              "processExit": 0, "heldFile": RESERVED, "recordedAs": PUBLISHED,
              "observationFile": RESOLVED}),
    scenario("undeclared_turn_end", MANAGED,
             {"observation": "undeclared_turn_end", "guardDecision": "block",
              "guardState": "undeclared_turn_end", "printedBlock": PRINTED_A_BLOCK,
              "processExit": 0, "heldFile": RESERVED, "recordedAs": PUBLISHED,
              "observationFile": RESOLVED}),
    # The positive control. Without it the three omission rows would assert about a cell that had
    # never been seen to move the other way, which is the shape of a suite that passes while the
    # guard sees nothing.
    scenario("declared_ready_receipted", MANAGED,
             {"observation": "declared_ready_receipted", "guardDecision": "release",
              "guardState": "declared_ready_receipted", "printedBlock": PRINTED_NOTHING,
              "processExit": 0, "heldFile": NOT_RESERVED, "recordedAs": PUBLISHED,
              "observationFile": RESOLVED},
             absent={"heldFile": "a turn that declared itself releases on its own declaration,"
                                 " and a reservation here would be the defect this scenario"
                                 " watches for"}),
    scenario("declared_in_progress", MANAGED,
             {"observation": "declared_in_progress", "guardDecision": "release",
              "guardState": "declared_in_progress", "printedBlock": PRINTED_NOTHING,
              "processExit": 0, "heldFile": NOT_RESERVED, "recordedAs": PUBLISHED,
              "observationFile": RESOLVED},
             absent={"heldFile": "a turn that declared itself releases on its own declaration,"
                                 " and a reservation here would be the defect this scenario"
                                 " watches for"}),
    scenario("declared_blocked_needs_input", MANAGED,
             {"observation": "declared_blocked_needs_input", "guardDecision": "release",
              "guardState": "declared_blocked_needs_input", "printedBlock": PRINTED_NOTHING,
              "processExit": 0, "heldFile": NOT_RESERVED, "recordedAs": PUBLISHED,
              "observationFile": RESOLVED},
             absent={"heldFile": "a turn that declared itself releases on its own declaration,"
                                 " and a reservation here would be the defect this scenario"
                                 " watches for"}),
    scenario("declared_interrupted", MANAGED,
             {"observation": "declared_interrupted", "guardDecision": "release",
              "guardState": "declared_interrupted", "printedBlock": PRINTED_NOTHING,
              "processExit": 0, "heldFile": NOT_RESERVED, "recordedAs": PUBLISHED,
              "observationFile": RESOLVED},
             absent={"heldFile": "a turn that declared itself releases on its own declaration,"
                                 " and a reservation here would be the defect this scenario"
                                 " watches for"}),
    scenario("unmanaged", UNMANAGED,
             {"observation": "unmanaged", "guardDecision": "release",
              "guardState": "unmanaged", "printedBlock": PRINTED_NOTHING,
              "processExit": 0, "heldFile": NOT_RESERVED, "recordedAs": NOT_PUBLISHED,
              "observationFile": NOT_PUBLISHED},
             absent={"recordedAs": "no marker names this workspace, so no assignment directory was"
                                   " selected and the guard had nowhere to publish",
                     "observationFile": "nothing was published, so there is no path to resolve",
                     "heldFile": "a release reserves nothing, and a reservation here would be the"
                                 " defect this scenario watches for"}),
    scenario("cxc_concurrent", MANAGED,
             {"observation": "undeclared_turn_end", "guardDecision": "release",
              "guardState": "hold_in_flight", "printedBlock": PRINTED_NOTHING,
              "processExit": 0, "heldFile": NOT_RESERVED, "recordedAs": PUBLISHED,
              "observationFile": RESOLVED},
             stop_hook_active=True,
             absent={"heldFile": "a continuation is already running for this turn, so the omission"
                                 " is recorded and nothing is held"}),
    scenario("duplicate", MANAGED,
             {"observation": "undeclared_turn_end", "guardDecision": "block",
              "guardState": "undeclared_turn_end", "printedBlock": PRINTED_A_BLOCK,
              "processExit": 0, "heldFile": RESERVED, "recordedAs": PUBLISHED,
              "observationFile": RESOLVED},
             fire_twice=True,
             second={"observation": "undeclared_turn_end", "guardDecision": "release",
                     "guardState": "hold_in_flight", "printedBlock": PRINTED_NOTHING,
                     "processExit": 0, "heldFile": RESERVED, "recordedAs": PUBLISHED,
                     "observationFile": RESOLVED}),
)

MEASURED = "measured"
NOT_PERFORMED = "not_performed"

# The rule every measure below obeys, written once because it was fixed one measure at a time and
# each fix left the next one open: a criterion is not met while a reading it rests on could not be
# taken. Dropping the unreadable sample and concluding from what is left reports a bound as kept on
# evidence nobody has, which is the substitution the whole arrangement refuses.
def unreadable_among(values):
    """Which of these readings could not be taken."""
    return [str(value) for value in values if value == reading.UNREADABLE]


def judged(met, not_taken):
    """A verdict, which cannot be true while a reading under it was not taken.

    Every met in this file is this call. Stating the rule and leaving each verdict to apply it
    separately is how one of them came not to: the standing check that was supposed to catch that
    only fires when a not-taken count is already nonzero, which never happens in a healthy run, so
    it watched the property without ever exercising it. Here the guard cannot be left out, because
    there is nowhere else to compute the answer.
    """
    count = not_taken if isinstance(not_taken, int) else len(not_taken or ())
    return bool(met) and not count

# The contract fixes these at skills/crw-run/references/hook-contract.md, "Decision criteria, fixed
# before implementation". They are neither restated nor extended: where this arrangement cannot
# reach one it says so rather than reaching for something it can measure instead.
LATENCY_MEDIAN_MS = 2000
LATENCY_P95_MS = 5000

STAND_INS = {
    "the relay launcher":
        "replaces the console script an install places under the pointer at"
        " <destination>/current/bin. A row through it cannot prove that the pointer resolves or"
        " that an installed build offers guard-evaluate at all.",
    "the temporary Codex home":
        "replaces a Codex home a host actually reads. A row through it cannot prove that the host"
        " discovers this registration, trusts it, or invokes it.",
    "the composed Stop payload":
        "replaces a payload a host delivered. A row through it establishes classification given"
        " the fields it carries and nothing about what a host sends.",
    "the supplied stop_hook_active":
        "replaces a host reporting a continuation in flight. A row through it cannot prove that a"
        " host sets the flag when it continues a turn.",
    "no daemon and no App Server":
        "replaces the running service. Delivery, acknowledgement, parent verification and recovery"
        " after a fault are outside every row here.",
    "the reported argv":
        "replaces a witness at the process boundary. The command is read back out of the hook file"
        " and compared, and the journal and the published observations are read from disk, so a"
        " row establishes that a hook process ran and reached the guard. It does not establish"
        " which executable the kernel started.",
    "the harness as sole writer":
        "replaces a sandbox grant. It is what makes hold mode legitimate here, and it means no row"
        " proves that a real child under a real grant could not forge the facts the guard read.",
}

NOT_PERFORMED_HERE = {
    "CRW-68 criterion 4":
        "the real installed path from a child's completion through a parent's verification, a"
        " correction, a re-verification and a Linear write read back on one persistent database."
        " It needs an install on a host and a real round trip between tasks.",
    "CRW-68 criterion 5":
        "installation, registration, firing, delivery acceptance and artifact verification"
        " distinguished on a real host, with recovery after a daemon, connection or hook fault."
        " This run separates registration from firing on a temporary destination and stops there.",
    "CRW-68 criterion 7":
        "two parents in different repositories and Linear projects against one installed shared"
        " relay, with concurrent handover and per-parent separation. It needs that shared service.",
}



# ------------------------------------------------------------------ the readings


def _row(cell):
    """The one declared row for this cell, looked up rather than respelled at each site."""
    for declared in CELLS:
        if declared[0] == cell:
            return declared
    raise KeyError("no reading is declared for " + repr(cell))


def _cell(cell, source, path, value, readable, detail=None):
    answer = {"cell": cell, "value": value, "answeredBy": source, "readingPath": list(path),
              "readable": readable}
    if detail:
        answer["detail"] = detail
    return answer


def _unreadable(cell, source, path, detail):
    """A cell whose reading could not be made.

    A distinct answer, not a negative one. Returning False here, or falling through to whatever
    the neighbouring cell said, is the defect this module exists to refuse.
    """
    return _cell(cell, source, path, reading.UNREADABLE, False, detail)


def _absent(cell, source, path, detail):
    """A cell whose subject is not there, where not being there is a normal state.

    Distinct from unreadable in the direction that matters: one says the answer is that there is
    nothing, the other says no answer was obtained. A run that could not look and a run that looked
    and found nothing are different facts about the hook, and collapsing them is how an off arm
    that never installed reads like an off arm that installed and correctly registered nothing.
    """
    return _cell(cell, source, path, reading.ABSENT, True, detail)


def read(cell, payloads):
    """Fill one cell from the reading its own question called for, and nothing else.

    The only accessor. It takes the payload of the declared source, confirms the payload is the one
    that source produces, walks the declared path and returns what it found. A source that declared
    an absence answers ABSENT with that source's reason; every other way of failing to reach an
    answer returns UNREADABLE naming why.
    """
    declared_cell, source, path, _producer = _row(cell)
    payload = payloads.get(source)
    if not isinstance(payload, dict):
        return _unreadable(declared_cell, source, path, "no payload was produced by " + source)
    if payload.get("source") != source:
        return _unreadable(declared_cell, source, path,
                           "the payload does not carry the " + repr(source) + " stamp")
    if payload.get("absent"):
        return _absent(declared_cell, source, path, payload["absent"])
    found = payload
    for key in path:
        if not isinstance(found, dict) or key not in found:
            # A source may declare, per path, that this key is absent for a reason it can name.
            # Without that the off arm's missing command reads as a failed reading, which says the
            # harness could not look when in fact it looked and there was correctly nothing there.
            why = (payload.get("absentPaths") or {}).get(key)
            if why:
                return _absent(declared_cell, source, path, why)
            return _unreadable(declared_cell, source, path, "the reading has no " + repr(key))
        found = found[key]
    if found == reading.UNREADABLE:
        # A producer that already said it could not read is not made readable by the fact that its
        # answer was where this table expected it. The path being reachable answers "was there an
        # answer here", which is a different question from "was the reading made".
        return _unreadable(declared_cell, source, path,
                           str(payload.get("detail") or "the reading reported itself unreadable"))
    return _cell(declared_cell, source, path, found, True)


# ------------------------------------------------------------------ the destination


def launcher_for(root):
    """The in-repository relay, executable, without installing anything.

    The command under test shells out to a relay executable named in its settings, so there has to
    be one. This one runs the guard from THIS checkout: what it stands in for is the console script
    an install would place under the pointer, which is why no row here may claim that an installed
    build offers the subcommand at all.
    """
    path = Path(root) / "codex-session-relay"
    path.write_text(
        "#!" + sys.executable + "\n"
        "import sys\n"
        "sys.path.insert(0, " + repr(str(RELAY_SOURCE)) + ")\n"
        "from codex_session_relay.cli import main\n"
        "raise SystemExit(main())\n", encoding="utf-8")
    path.chmod(path.stat().st_mode | stat.S_IEXEC | stat.S_IRWXU)
    return path


# A Stop registration belonging to somebody else, seeded into both arms before either install. The
# installed CXC hook holds independently and this contract governs only its own holds, so a
# registration that displaced another owner's entry would have broken that before any decision was
# reached. Read back after the install rather than assumed.
FOREIGN_COMMAND = "/opt/cxc/stop"
FOREIGN_HOOKS = {
    "version": 1,
    "hooks": {completion.EVENT: [{"hooks": [{"type": "command", "command": FOREIGN_COMMAND,
                                             "timeout": 9}]}]},
}


class Arm(object):
    """One destination: its Codex home, its marker root, its store, and its install reading."""

    def __init__(self, root, name, launcher):
        self.name = name
        self.root = Path(root) / name
        self.codex_home = self.root / "codex"
        self.markers = self.root / "markers"
        self.state = self.root / "state"
        self.journal = self.root / "journal"
        self.work = self.root / "work"
        for directory in (self.codex_home, self.markers, self.state, self.journal, self.work):
            directory.mkdir(parents=True, exist_ok=True)
        self.db = self.state / "relay.sqlite3"
        self.launcher = launcher
        # Built before the install, because the install is the first subprocess and it is the one
        # an inherited settings pointer would send outside this root.
        self.environment = environment(self)
        hooks_file(self.codex_home).write_text(json.dumps(FOREIGN_HOOKS, indent=2),
                                               encoding="utf-8")
        self.places = [self.codex_home, self.markers, self.state, self.journal, self.work,
                       hooks_file(self.codex_home)]
        self.argv = self._install_argv()
        self.install = self._install()
        self.hook_file = self._hook_file()

    def _install_argv(self):
        argv = [sys.executable, str(RUNTIME), "hook", "--adapter", "completion",
                "--codex-home", str(self.codex_home), "--relay-command", str(self.launcher),
                "--marker-root", str(self.markers), "--db-path", str(self.db),
                "--journal-root", str(self.journal),
                # Hold mode, because observe downgrades every block to a release and prints
                # nothing, and wrong blocking would then be a question no run could answer either
                # way. What makes holding legitimate here is not this flag but that no child exists
                # and this process is the only writer of every fact the guard reads.
                "--mode", completion.HOLD, "--isolation-asserted-by", SOURCE,
                "--issue", "CRW-68"]
        if self.name == ON:
            argv.append("--apply")
        return argv

    def _install(self):
        """Run the installer and keep its own report, which is what the arm is judged on."""
        try:
            done = subprocess.run(self.argv, capture_output=True, text=True, timeout=300,
                                  env=self.environment)
        except subprocess.TimeoutExpired:
            return {"source": INSTALL, "exitCode": reading.UNREADABLE,
                    "detail": "the install did not finish within its timeout"}
        except OSError as error:
            return {"source": INSTALL, "exitCode": reading.UNREADABLE,
                    "detail": "the install could not be started: " + str(error)}
        try:
            payload = json.loads(done.stdout) if done.stdout.strip() else {}
            if not isinstance(payload, dict):
                raise ValueError("the install printed JSON that is not an object")
        except ValueError:
            return {"source": INSTALL, "exitCode": done.returncode,
                    "detail": "the install printed something that is not JSON"}
        payload = dict(payload)
        payload["source"] = INSTALL
        payload["exitCode"] = done.returncode
        return payload

    def _hook_file(self):
        """What is registered under Stop in this arm's own hook file, read back out of it."""
        document = hooks.read(hooks_file(self.codex_home))
        if not document.usable:
            return {"source": HOOK_FILE, "detail": "the hook file could not be read"}
        found = completion.adapter_entries(document.value or {}, completion.EVENT)
        payload = {"source": HOOK_FILE, "entries": len(found),
                   "foreign": _foreign(document.value or {})}
        if found:
            payload["command"] = found[0]["command"]
        else:
            # Not a failure and not a zero: the off arm is DEFINED by there being no entry to run,
            # and the cells that would have read a firing say so with this reason.
            payload["absentPaths"] = {"command": NO_REGISTRATION}
        return payload


def environment(arm):
    """Exactly what the subprocesses are told, built rather than inherited.

    The relay resolves its marker root and its store from the environment when no flag names them,
    and the adapter falls back to CODEX_HOME for its settings. An inherited CODEX_SESSION_RELAY_*
    or state variable would therefore point some part of this run at a root nobody here built, and
    the rows would still fill: a workspace the guard never matched reads unmanaged, releases and
    holds nothing, which looks calm and measures nothing. So the child gets a home of its own, a
    PATH that reaches nothing installed, and no inherited pointer of any kind.
    """
    # Named rather than filtered out of os.environ: an allow list cannot be read to see what it
    # excludes, and what matters here is exactly which pointers a child is given.
    built = {"PATH": "/usr/bin:/bin", "HOME": str(arm.root / "home"),
             "XDG_STATE_HOME": str(arm.root / "xdg-state"),
             "CODEX_HOME": str(arm.codex_home),
             "PYTHONPATH": "", "PYTHONNOUSERSITE": "1",
             # The subprocesses import the relay and the runtime modules out of this checkout, and
             # would leave __pycache__ beside that source. Those are writes, and this run says it
             # makes none outside its own directory.
             "PYTHONDONTWRITEBYTECODE": "1",
             "LANG": os.environ.get("LANG", "C.UTF-8")}
    for directory in (built["HOME"], built["XDG_STATE_HOME"]):
        Path(directory).mkdir(parents=True, exist_ok=True)
    return built


def hooks_file(codex_home):
    return Path(codex_home) / "hooks.json"


def _foreign(document):
    """Whether the entry that was in the file before the install is still in it."""
    groups = ((document.get("hooks") or {}).get(completion.EVENT) or [])
    for group in groups:
        for entry in (group or {}).get("hooks") or []:
            if isinstance(entry, dict) and entry.get("command") == FOREIGN_COMMAND:
                return "present"
    return "displaced"



# ------------------------------------------------------------------ building a scenario


class RelayError(RuntimeError):
    pass


def relay(arm, *args):
    """One relay command, run as the operator would run it, against this arm's own state.

    Every way this can fail becomes a RelayError, which main() prints as a refusal. A timeout, an
    executable that could not be started and output that is not JSON are all ordinary things at a
    process boundary, and letting one escape ends the command with a traceback and no document at
    all - which is the one output this command promises never to produce.
    """
    try:
        done = subprocess.run([str(arm.launcher), "--state", str(arm.state), *args],
                              capture_output=True, text=True, timeout=300,
                              env=arm.environment)
    except subprocess.TimeoutExpired:
        raise RelayError(" ".join(args[:2]) + " did not finish within its timeout")
    except OSError as error:
        raise RelayError(" ".join(args[:2]) + " could not be started: " + str(error))
    if done.returncode != 0:
        raise RelayError(" ".join(args[:2]) + " exited " + str(done.returncode) + ": "
                         + (done.stdout or done.stderr)[-400:])
    if not done.stdout.strip():
        return {}
    try:
        answered = json.loads(done.stdout)
    except ValueError:
        raise RelayError(" ".join(args[:2]) + " printed something that is not JSON")
    if not isinstance(answered, dict):
        raise RelayError(" ".join(args[:2]) + " printed valid JSON that is not an object, so"
                         " nothing can be read out of it")
    return answered


def build(arm, declared):
    """Put this scenario's state on disk through the relay's own commands.

    Every scenario gets its own workspace, session, assignment and issue key. Not tidiness: hold
    budgets are one per turn, two per generation and three per rolling session window, and the
    relay refuses a second relationship under one issue key, so two scenarios sharing either would
    decide each other's outcomes.

    The session identity is the child task id. That is what a dispatch receipt returns and what the
    receipt lookup matches a receipt's turn thread against, so a scenario that invented a separate
    session id would reach receipt_missing with a perfectly good receipt sitting in the store.
    """
    name = declared["name"]
    workspace = arm.work / name
    workspace.mkdir(parents=True, exist_ok=True)
    workspace = str(workspace.resolve())
    session = "01child-" + name
    turn = "turn-" + name
    dispatch = "dispatch-" + name
    issue = "CRW-68-" + name
    built = {"workspace": workspace, "session": session, "turn": turn}
    if declared["kind"] == UNMANAGED:
        # Nothing is declared for this workspace at all. The absence is the scenario.
        return built

    declared = relay(arm, "intent-declare", "--marker-root", str(arm.markers),
                     "--workspace", workspace, "--dispatch-request-id", dispatch,
                     "--issue", issue)
    assignment, declared_directory = declared["assignmentId"], declared["assignmentDir"]
    relay(arm, "intent-bind", "--marker-root", str(arm.markers), "--workspace", workspace,
          "--assignment", assignment, "--session", session, "--task-id", session)
    relay(arm, "intent-claim", "--marker-root", str(arm.markers), "--workspace", workspace,
          "--assignment", assignment, "--session", session, "--dispatch-request-id", dispatch,
          "--first-turn", turn)
    built["assignment"] = assignment
    built["assignmentDir"] = declared_directory

    if name != "managed_unregistered":
        relationship = relay(
            arm, "register", "--parent-task", "01parent-" + name, "--parent-host", "harness",
            "--parent-cwd", str(arm.root), "--child-task", session, "--child-host", "harness",
            "--child-cwd", workspace, "--issue", issue, "--artifact-root", workspace,
            "--allowed-recipient", "01parent-" + name, "--dispatch-request-id", dispatch,
            "--dispatch-turn-id", turn)["relationshipId"]
        relay(arm, "intent-register", "--marker-root", str(arm.markers), "--workspace", workspace,
              "--assignment", assignment, "--relationship", relationship,
              "--dispatch-request-id", dispatch)
        built["relationship"] = relationship

        if name == "declared_ready_receipted":
            # A real receipt over real bytes, emitted by the relay's own command. Without this the
            # three omission rows would never have seen their cell move the other way.
            artifact = Path(workspace) / "out.txt"
            artifact.write_text("the deliverable this scenario names", encoding="utf-8")
            relay(arm, "emit", "--relationship", relationship, "--generation", "1",
                  "--outcome", "ready_for_review", "--turn-thread", session, "--turn-id", turn,
                  "--turn-status", "inProgress", "--artifact", str(artifact))

    outcome = DISPOSITIONS.get(name)
    if outcome:
        relay(arm, "intent-disposition", "--marker-root", str(arm.markers),
              "--workspace", workspace, "--assignment", assignment, "--session", session,
              "--turn", turn, "--outcome", outcome)
    return built


# What each scenario records for its turn, where it records anything. Declared as a table so a
# scenario named after a disposition cannot quietly stop writing one.
DISPOSITIONS = {
    "receipt_missing": "ready_for_review",
    "declared_ready_receipted": "ready_for_review",
    "declared_in_progress": "in_progress",
    "declared_blocked_needs_input": "blocked_needs_input",
    "declared_interrupted": "interrupted",
}


# ------------------------------------------------------------------ firing


def stop_payload(built, stop_hook_active):
    """The Stop object as the host's schema names it, composed here rather than delivered.

    The field names are the nine the host was observed to deliver. last_assistant_message is
    carried because the host carries it, and the guard deliberately never reads it.
    """
    return {"cwd": built["workspace"], "hook_event_name": completion.EVENT,
            "last_assistant_message": "I finished the task.", "model": "a-model",
            "permission_mode": "default", "session_id": built["session"],
            "stop_hook_active": stop_hook_active, "transcript_path": "/dev/null",
            "turn_id": built["turn"]}


def fire(arm, built, stop_hook_active):
    """Run the command the registration names, with the Stop payload on its stdin.

    Calling completion.run() here would have been a reading of this checkout. The row says a
    registered hook fired, and the registered hook is a COMMAND LINE in that arm's hooks.json: an
    interpreter, the entry point, and the settings path the install chose. Run a helper instead and
    the entry point, the settings argument and the stdin contract are all untested while the row
    still reaches its answer.
    """
    command = (arm.hook_file or {}).get("command")
    if not command:
        return None
    started = time.monotonic()
    try:
        done = subprocess.run(
            shlex.split(command),
            input=json.dumps(stop_payload(built, stop_hook_active)).encode("utf-8"),
            capture_output=True, timeout=300, env=arm.environment)
    except subprocess.TimeoutExpired:
        return {"faulted": "the registered command did not finish within its timeout",
                "argv": shlex.split(command)}
    except OSError as error:
        return {"faulted": "the registered command could not be started: " + str(error),
                "argv": shlex.split(command)}
    wall = int((time.monotonic() - started) * 1000)
    return {"stdout": done.stdout.decode("utf-8", "replace"),
            "stderr": done.stderr.decode("utf-8", "replace"),
            "exitCode": done.returncode, "wallMs": wall,
            "argv": shlex.split(command)}


def journal_records(arm, session, turn):
    """Every journal record naming this session AND this turn, by path.

    Correlation rather than counting. A cumulative count moves when any other invocation writes,
    and a before-and-after difference over a count moves when anything else runs inside the window,
    so neither can say that THIS turn's hook fired.

    Returned by path because the records are not orderable among themselves: the name is random
    hex under a day directory, and two firings of one turn can carry the same recorded moment. A
    run that sorted them and took the second got the first back whenever the sort tied, and the
    row then read the earlier firing's decision while claiming to describe the later one.
    """
    found = {}
    for day in sorted(arm.journal.glob("*")):
        if not day.is_dir():
            continue
        for record in sorted(day.glob("*.json")):
            try:
                payload = json.loads(record.read_text(encoding="utf-8"))
            except (OSError, ValueError):
                continue
            if not isinstance(payload, dict):
                # A record that is valid JSON and not an object is not this turn's record and is
                # not readable as one either; skipping it here would silently lose a firing, so it
                # is left out of the correlation and the arriving-record count reports the gap.
                continue
            if payload.get("sessionId") == session and payload.get("turnId") == turn:
                found[str(record)] = payload
    return found


def record_of(before, after):
    """The one record this firing wrote, identified by not having been there before it.

    Exactly one is expected. None means the command left no record of the invocation, and more
    than one means something else wrote for this session and turn while the firing ran; both are
    reported as an unreadable journal rather than resolved by picking one.
    """
    arrived = [path for path in after if path not in before]
    if len(arrived) == 1:
        return after[arrived[0]], None
    if not arrived:
        return None, "the firing wrote no journal record for this session and turn"
    return None, ("the firing is not identifiable: " + str(len(arrived)) + " records arrived for"
                  " this session and turn")



# ------------------------------------------------------------------ payloads


# How an expectation is compared with what was read. Equality unless the answer is about a value
# whose exact spelling is not the question: a published observation is named by a path this run
# cannot predict, so what is asserted there is that one was named at all.
PREDICATES = {
    ("recordedAs", PUBLISHED): lambda value: isinstance(value, str) and bool(value),
    ("recordedAs", NOT_PUBLISHED): lambda value: value is None,
}


def journal_payload(record, unidentifiable=None):
    """This firing's own record, or a named reason there is none to read.

    An absent registration and a firing whose record could not be identified are different facts
    and are answered differently: the first is the off arm and is normal, the second is a reading
    that failed and must never pass as one that was taken.
    """
    if unidentifiable:
        return {"source": JOURNAL, "detail": unidentifiable, "adapterOutcome": reading.UNREADABLE}
    if record is None:
        return {"source": JOURNAL, "absent": NO_REGISTRATION}
    payload = dict(record)
    payload["source"] = JOURNAL
    return payload


def _faulted(source, fired, keys):
    """A firing that never completed. Its readings could not be taken, and say so."""
    payload = {"source": source, "detail": fired["faulted"]}
    for key in keys:
        payload[key] = reading.UNREADABLE
    return payload


def stdout_payload(fired):
    """What the host would have seen on stdout, in the host's own vocabulary.

    A release prints nothing, and so do several faults, so silence is reported as silence rather
    than translated into a decision. A cell that turned an empty stdout into "release" would be
    asserting the one thing it had failed to read.
    """
    if fired is None:
        return {"source": STDOUT, "absent": NO_REGISTRATION}
    if fired.get("faulted"):
        return _faulted(STDOUT, fired, ("printed",))
    raw = (fired.get("stdout") or "").strip()
    if not raw:
        return {"source": STDOUT, "printed": PRINTED_NOTHING, "raw": ""}
    try:
        answer = json.loads(raw)
    except ValueError:
        return {"source": STDOUT, "printed": PRINTED_OTHER, "raw": raw[:400]}
    if not isinstance(answer, dict):
        # Valid JSON and not an object: the parse succeeded and there is still nothing to read.
        return {"source": STDOUT, "printed": PRINTED_OTHER, "raw": raw[:400]}
    # Asked of the adapter's own validator rather than decided here. completion.verdict_complaints
    # is what refuses output the host reports as a failed run, and a second copy of that rule in
    # this file would agree with it only until one of them changed. It already had: the copy here
    # accepted a block that never asked for a continuation, which the host discards.
    complaints = completion.verdict_complaints(
        {"decision": answer.get("decision"), "hook_output": answer})
    printed = PRINTED_A_BLOCK if not complaints else PRINTED_OTHER
    payload = {"source": STDOUT, "printed": printed, "raw": raw[:400]}
    if complaints:
        payload["hostWouldRefuse"] = complaints
    return payload


def marker_payload(arm, built, fired, recorded_as):
    """What is on disk under the marker root after the firing.

    Two questions, kept apart. What the guard SAID it wrote is a field in the journal; whether
    anything is there is a different claim and it is the one that survives the process. The
    reservation is read as the create-once file itself rather than counted from decisions, because
    the reservation is the critical section: exactly one caller wins that file, and a count
    assembled from what each evaluation returned is a count taken outside the lock that decides it.
    """
    if fired is None:
        return {"source": MARKER_ROOT, "absent": NO_REGISTRATION}
    if fired.get("faulted"):
        return _faulted(MARKER_ROOT, fired, ("observationFile", "heldFile"))
    directory = built.get("assignmentDir")
    if not directory:
        # No assignment was ever declared for this workspace, so there is nowhere for the guard to
        # publish and nowhere for a hold to be reserved. That is the answer, not a failed reading.
        return {"source": MARKER_ROOT, "observationFile": NOT_PUBLISHED, "heldFile": NOT_RESERVED}
    directory = Path(directory)
    if not recorded_as:
        published = NOT_PUBLISHED
    else:
        published = _there(directory / (recorded_as + ".json"), RESOLVED, NOT_RESOLVED)
    held = _there(directory / "hook" / built["session"] / built["turn"] / "hold.json",
                  RESERVED, NOT_RESERVED)
    return {"source": MARKER_ROOT, "observationFile": published, "heldFile": held}


def _there(path, present, missing):
    """Whether a file is there, with a stat that failed answering neither.

    stat() rather than is_file(), and the difference is the whole point: is_file() catches its own
    OSError and answers False, so a permission error, a symlink loop or a filesystem fault would
    have been reported as "nothing was published" or "nothing was held". A reading that could not
    be taken must not be presented as the answer that there is nothing there, and catching around
    is_file() does not do it because the error never escapes. Only a missing entry is an absence;
    every other failure is unreadable.
    """
    try:
        found = path.stat()
    except FileNotFoundError:
        return missing
    except OSError:
        return reading.UNREADABLE
    return present if stat.S_ISREG(found.st_mode) else missing


def harness_payload(fired):
    if fired is None:
        return {"source": HARNESS, "absent": NO_REGISTRATION}
    if fired.get("faulted"):
        return _faulted(HARNESS, fired, ("wallMs", "exitCode"))
    return {"source": HARNESS, "wallMs": fired["wallMs"], "exitCode": fired["exitCode"]}


# ------------------------------------------------------------------ one row


def row(arm, declared, built, fired, record, expected, unidentifiable=None):
    """Every firing cell for one arm, one scenario and one firing, each by its own reading."""
    payloads = {
        HOOK_FILE: arm.hook_file,
        JOURNAL: journal_payload(record, unidentifiable),
        STDOUT: stdout_payload(fired),
        MARKER_ROOT: marker_payload(arm, built, fired, (record or {}).get("guardRecordedAs")),
        HARNESS: harness_payload(fired),
    }
    cells = {cell: read(cell, payloads) for cell in FIRING_CELLS}
    answer = {"cells": cells, "expected": dict(expected),
              "provenance": {"codexHome": str(arm.codex_home),
                             "argv": (fired or {}).get("argv"),
                             "stopPayload": "composed by this harness, not delivered by a host",
                             "stopHookActive": declared["stopHookActive"]}}
    answer["verdict"] = judge(arm, declared, cells, expected)
    return answer


def judge(arm, declared, cells, expected):
    """Whether this row passes, and when it does not, which cell disagreed with what.

    The off arm passes by reading its declared absence everywhere, with the one reason. The on arm
    passes only by reaching every value the scenario declared BEFORE the run, and a managed
    scenario whose observation was never published is reported unmeasured rather than passing: the
    likeliest silent failure of an arrangement like this is a workspace the guard never matched, in
    which every scenario reads unmanaged, releases, holds nothing and looks calm.
    """
    disagreed = []
    if arm.name == OFF:
        for cell in FIRING_CELLS:
            found = cells[cell]
            if found["value"] != reading.ABSENT:
                disagreed.append({"cell": cell, "wanted": reading.ABSENT,
                                  "found": found["value"]})
            elif found.get("detail") != NO_REGISTRATION:
                disagreed.append({"cell": cell, "wanted": NO_REGISTRATION,
                                  "found": found.get("detail")})
        return {"passed": not disagreed, "disagreed": disagreed,
                "because": "the off arm registered nothing, so nothing ran"}

    if cells["adapterOutcome"]["value"] != "guard_answered":
        return {"passed": False, "unmeasured": True, "disagreed": [
            {"cell": "adapterOutcome", "wanted": "guard_answered",
             "found": cells["adapterOutcome"]["value"]}],
            "because": "the adapter did not receive a verdict, so this scenario was not measured;"
                       " it does not mean no omission was present"}
    for cell, wanted in expected.items():
        found = cells[cell]["value"]
        if found in NOT_A_VALUE and wanted not in NOT_A_VALUE:
            # Before any predicate sees it. recordedAs expects "a path was named", and the
            # sentinel is a non-empty string, so a reading that failed would have satisfied the
            # one question the row asks there.
            disagreed.append({"cell": cell, "wanted": wanted, "found": found,
                              "because": "a reading that could not be taken is not a value"})
            continue
        predicate = PREDICATES.get((cell, wanted))
        agreed = predicate(found) if predicate else found == wanted
        if not agreed:
            disagreed.append({"cell": cell, "wanted": wanted, "found": found})
    published = cells["recordedAs"]["value"]
    unmeasured = (declared["kind"] == MANAGED
                  and expected.get("recordedAs", PUBLISHED) == PUBLISHED
                  and (published in NOT_A_VALUE or not published))
    verdict = {"passed": not disagreed and not unmeasured, "disagreed": disagreed}
    if unmeasured:
        verdict["unmeasured"] = True
        verdict["because"] = ("a managed scenario published no observation, so the guard did not"
                              " select this workspace and nothing here was measured")
    return verdict



# ------------------------------------------------------------------ the measures


def _firings(scenarios, arm, names=None):
    """Every firing at one arm, optionally only for the named scenarios."""
    found = []
    for declared in SCENARIOS:
        if names is not None and declared["name"] not in names:
            continue
        for index, fired in enumerate(scenarios[declared["name"]][arm]["firings"]):
            found.append((declared, index, fired))
    return found


def _reported(fired, observation):
    """Whether this firing reported that observation, on a reading that was actually taken."""
    cells = fired["cells"]
    return (cells["adapterOutcome"]["value"] == "guard_answered"
            and cells["observation"]["value"] == observation)


def _detection(scenarios, observation, injected_names):
    injected = _firings(scenarios, ON, injected_names)
    reported = [one for one in injected if _reported(one[2], observation)]
    # "It did not report this" and "nobody could read whether it reported this" are different
    # answers, and counting them together would let an unreadable journal read as a detector that
    # stayed silent. Neither concludes the criterion met; only one of them is about the hook.
    unreadable = [declared["name"] + "#" + str(index) for declared, index, fired in injected
                  if fired["cells"]["observation"]["value"] == reading.UNREADABLE
                  or fired["cells"]["adapterOutcome"]["value"] == reading.UNREADABLE]
    return {"answer": MEASURED, "injected": len(injected), "reported": len(reported),
            "unreadable": unreadable,
            "met": judged(len(injected) > 0 and len(injected) == len(reported), unreadable),
            "rows": [declared["name"] + "#" + str(index) for declared, index, _f in injected]}


def _percentile(values, fraction):
    if not values:
        return None
    ordered = sorted(values)
    index = int(round((len(ordered) - 1) * fraction))
    return ordered[index]


def measures(scenarios):
    """The contract's six, plus the one CRW-68 adds, each answered or declared not performed.

    Nothing here is computed from the difference between the arms. The off arm having no records
    and the on arm having them is settled by one flag before any Stop is delivered, so a criterion
    made of that difference would be measuring the flag.
    """
    answers = {}
    answers["missedDetectionStateAndReceiptAbsent"] = _detection(
        scenarios, "undeclared_turn_end", ("undeclared_turn_end", "cxc_concurrent", "duplicate"))
    answers["missedDetectionStateAndReceiptAbsent"]["criterion"] = (
        "every injected undeclared_turn_end is reported, counted on its own and never merged with"
        " the row below")
    answers["missedDetectionReceiptAbsentOnly"] = _detection(
        scenarios, "receipt_missing", ("receipt_missing",))
    answers["missedDetectionReceiptAbsentOnly"]["criterion"] = (
        "every injected receipt_missing is reported, with the declared state present")
    # CRW-68 names this case and the contract's table does not carry it as a measure of its own, so
    # it is reported beside them under its own name and never folded into either.
    answers["missedDetectionMarkerWithoutRegistration"] = _detection(
        scenarios, "managed_unregistered", ("managed_unregistered",))
    answers["missedDetectionMarkerWithoutRegistration"]["criterion"] = (
        "every injected managed_unregistered is reported. Required by CRW-68 rather than by the"
        " contract's table, and reported separately for that reason")

    answers["handoffSuccess"] = {
        "answer": NOT_PERFORMED,
        "criterion": "a reported omission is followed by a real receipt and one parent"
                     " verification, with no new user message",
        "because": "no parent task and no verification round trip exist in this arrangement, so"
                   " the second half of that criterion has nothing to observe. Not approximated.",
    }

    # The contract names unmarked sessions, blocked_needs_input and interrupted. Read from the
    # reservation and from what was printed, because a hold is both of those and neither alone.
    watched = ("unmanaged", "declared_blocked_needs_input", "declared_interrupted")
    guarded = _firings(scenarios, ON, watched)
    reserved = [declared["name"] for declared, _i, fired in guarded
                if fired["cells"]["heldFile"]["value"] == RESERVED]
    printed = [declared["name"] for declared, _i, fired in guarded
               if fired["cells"]["printedBlock"]["value"] == PRINTED_A_BLOCK]
    # A reading that could not be taken is not evidence that nothing was held. Dropping it from
    # the list made the criterion read as met on the strength of a reservation nobody could look
    # at, which is the one substitution this whole arrangement refuses.
    unreadable = [declared["name"] + "/" + cell for declared, _i, fired in guarded
                  for cell in ("heldFile", "printedBlock")
                  if fired["cells"][cell]["value"] not in
                  (RESERVED, NOT_RESERVED, PRINTED_A_BLOCK, PRINTED_NOTHING)]
    answers["wrongBlock"] = {
        "answer": MEASURED,
        "criterion": "zero holds on unmarked sessions, on blocked_needs_input and on interrupted",
        "watched": list(watched), "reserved": reserved, "printedABlock": printed,
        "unreadable": unreadable,
        "met": judged(not reserved and not printed, unreadable),
        "narrowing": "the holds counted are this hook's own, which is what the contract makes it"
                     " responsible for. Whether a host would honour a printed block is not"
                     " observed here, and a default installation is observe mode and prints none.",
    }

    answers["duplicateExecution"] = {
        "answer": NOT_PERFORMED,
        "criterion": "no verification or correction runs twice for one event id across a hold, a"
                     " daemon restart, or a recovery",
        "because": "nothing verifies and nothing corrects in this arrangement, and no daemon runs,"
                   " so none of the three legs is exercised. The reservation observation below is"
                   " reported under its own name and is not this measure.",
    }

    timings = [fired["cells"]["processWallMs"]["value"]
               for _d, _i, fired in _firings(scenarios, ON)]
    latencies = [value for value in timings if isinstance(value, int)]
    # A firing whose timing could not be taken is not a fast firing. Dropping it and reporting the
    # median of what is left says the budget was kept over samples that exclude the slow one.
    missing = len(timings) - len(latencies)
    median, p95 = _percentile(latencies, 0.5), _percentile(latencies, 0.95)
    answers["addedLatency"] = {
        "answer": MEASURED,
        "criterion": "within the budget, reported as a distribution rather than a mean",
        "budgetMs": {"median": LATENCY_MEDIAN_MS, "p95": LATENCY_P95_MS},
        "distributionMs": {"count": len(latencies), "minimum": min(latencies) if latencies else None,
                           "median": median, "p95": p95,
                           "maximum": max(latencies) if latencies else None},
        "firings": len(timings), "timingsNotTaken": missing,
        "met": judged(bool(latencies) and median <= LATENCY_MEDIAN_MS
                      and p95 <= LATENCY_P95_MS, missing),
        "narrowing": "the interval measured is the hook process alone, started by this harness."
                     " The contract's budget is per Stop as the host sees it, so meeting it here"
                     " is necessary and not sufficient.",
    }
    return answers


def supplemental(scenarios):
    """Observations worth recording that are not one of the contract's measures.

    Named apart so nothing here can be read as an answer to a criterion it does not answer.
    """
    firings = scenarios["duplicate"][ON]["firings"]
    reservations = [fired["cells"]["heldFile"]["value"] for fired in firings]
    published = [fired["cells"]["recordedAs"]["value"] for fired in firings]
    not_taken = unreadable_among(reservations + published)
    foreign_not_taken = unreadable_among(
        [scenarios["_arms"][arm]["foreignRegistration"]["value"] for arm in ARMS])
    return {
        "oneReservationPerTurn": {
            "what": "one turn, the registered command fired twice",
            "reservations": reservations,
            "observationsPublished": published,
            "notTaken": len(not_taken),
            # The claim is about the reservation, so the reservation is what is checked: the
            # create-once file is there after both firings, and the two firings published two
            # distinct observations rather than one record read twice. Counting only the
            # observations would have reported success for a turn that reserved nothing at all.
            "met": judged(len(firings) == 2 and reservations == [RESERVED, RESERVED]
                          and len([one for one in published if one]) == 2
                          and len(set(one for one in published if one)) == 2, not_taken),
            "isNot": "the duplicate execution measure. It is the hold leg of it and none of the"
                     " rest, because nothing here verifies, corrects, or restarts a daemon.",
        },
        "foreignRegistrationSurvives": {
            "what": "a Stop entry belonging to another owner was in both hook files first",
            "off": scenarios["_arms"][OFF]["foreignRegistration"]["value"],
            "on": scenarios["_arms"][ON]["foreignRegistration"]["value"],
            "notTaken": len(foreign_not_taken),
            "met": judged(scenarios["_arms"][ON]["foreignRegistration"]["value"] == "present",
                          foreign_not_taken),
            "isNot": "evidence that the two hooks interact at run time. The foreign command is"
                     " never executed here; this reads the file, not a decision.",
        },
    }



# ------------------------------------------------------------------ the run


# The two names a judgment is written under anywhere in this document. Collected by walking what
# was assembled rather than by listing the places that produce them: the list was wrong three times
# in a row, and each time the thing left out was a judgment that could fail while the command
# exited 0. A walk cannot leave one out, and a judgment added later joins it without being noticed.
JUDGMENT_KEYS = ("passed", "met")


def judgments(payload, path=()):
    """Every judgment in the document, by where it sits and what it said."""
    found = []
    if isinstance(payload, dict):
        for key, value in sorted(payload.items()):
            here = path + (key,)
            if key in JUDGMENT_KEYS:
                found.append(("/".join(str(one) for one in here), value))
            found.extend(judgments(value, here))
    elif isinstance(payload, list):
        for index, value in enumerate(payload):
            found.extend(judgments(value, path + (index,)))
    return found


def source_identity():
    """What actually ran, which is not always what the commit names.

    A commit identifies bytes only when the checkout is clean. Run from a working tree with edits
    in it - which is how anyone developing this runs it - the commit names something else, and a
    result attributing itself to that commit attributes the run to source it did not execute. So
    the dirty state is recorded beside it, and the files whose contents decide a run are digested,
    which identifies them whether or not anything is committed.
    """
    identity = {"repositoryCommit": repository_commit(), "workingTree": reading.UNREADABLE}
    try:
        done = subprocess.run(["git", "status", "--porcelain"], cwd=str(ROOT),
                              capture_output=True, text=True, timeout=60)
        if done.returncode == 0:
            identity["workingTree"] = "dirty" if done.stdout.strip() else "clean"
    except (OSError, subprocess.TimeoutExpired):
        # Left unreadable rather than guessed at: a tree nobody could look at is not a clean one.
        pass
    digests = {}
    for name, target in (("harness", Path(__file__).resolve()),
                         ("installer", RUNTIME),
                         ("entryPoint", ROOT / "scripts" / completion.ENTRY_POINT_NAME),
                         ("runtimeModules", ROOT / "scripts" / "crw_runtime"),
                         ("relay", RELAY_SOURCE)):
        digests[name] = _digest(target)
    identity["sourceDigests"] = digests
    return identity


def _digest(target):
    """One digest over a file, or over a directory's files by path and content."""
    summed = hashlib.sha256()
    try:
        # Every file, not every module: the installer reads components.json, and a digest that
        # only saw Python would report the same identity across a change that decides an install.
        paths = [target] if target.is_file() else sorted(
            path for path in target.rglob("*") if path.is_file()
            and "__pycache__" not in path.parts)
        if not paths:
            return reading.UNREADABLE
        for path in paths:
            summed.update(str(path.relative_to(ROOT)).encode("utf-8"))
            summed.update(path.read_bytes())
    except OSError:
        return reading.UNREADABLE
    return summed.hexdigest()


def repository_commit():
    try:
        done = subprocess.run(["git", "rev-parse", "HEAD"], cwd=str(ROOT), capture_output=True,
                              text=True, timeout=60)
    except (OSError, subprocess.TimeoutExpired):
        # A timeout is as ordinary here as a missing git, and letting it escape would end the
        # command with no document at all over a question about provenance.
        return reading.UNREADABLE
    return done.stdout.strip() if done.returncode == 0 else reading.UNREADABLE


def own_directory(where):
    """A directory of this run's own, created under the place the caller named.

    The caller names a place to work in; the run does not work in it. It writes a launcher and two
    Codex homes at fixed names, so using the named directory itself means replacing whatever was
    already using those names - and a directory an operator points at is exactly where something
    else already lives. mkdtemp creates a fresh child, atomically and uniquely, so nothing that
    was there is touched and --root /tmp stays a reasonable thing to type.

    Returned resolved, because every later containment question is asked against it and a root
    reached through a symlink would make those answers about somewhere else.
    """
    where = Path(where).expanduser()
    where.mkdir(parents=True, exist_ok=True)
    return Path(tempfile.mkdtemp(prefix="hook-comparison-", dir=str(where))).resolve()


def owned(path, root):
    """Whether this path is inside this run's own directory: inside, outside, or unreadable.

    Resolved on both sides rather than compared as text. A path that starts with the root as a
    string can still lead outside it through a symlink, and "everything is written inside the
    root" is a claim about where the bytes land rather than about how the path is spelled.

    Three answers rather than two, because a path that could not be resolved is not a path outside
    the root: reporting it as outside names the wrong repair and claims to know where it led.
    """
    try:
        resolved = Path(path).resolve()
    except OSError:
        return reading.UNREADABLE
    try:
        resolved.relative_to(Path(root).resolve())
    except (ValueError, OSError):
        return False
    return True


def containment(root, places):
    """Every place this run writes, checked to actually be inside its own directory.

    A judgment rather than an assumption, so it is collected with the others and a run that wrote
    somewhere else cannot exit 0 while saying it did not.
    """
    answers = dict((str(place), owned(place, root)) for place in places)
    outside = sorted(place for place, answer in answers.items() if answer is False)
    not_taken = sorted(place for place, answer in answers.items()
                       if answer == reading.UNREADABLE)
    return {"checked": len(places), "outside": outside, "notTaken": not_taken,
            "met": judged(not outside, not_taken),
            "what": "every place this run creates resolves inside the directory it made for"
                    " itself, by where the path leads rather than by how it is spelled",
            "doesNotCover": "a write a subprocess made somewhere this run never named. Seeing"
                            " those needs a witness at the process boundary, which is not built"
                            " here; what is built is that the constructed environment carries no"
                            " pointer out of this directory and no bytecode is left beside the"
                            " source the subprocesses import."}


def refusal(detail):
    """An answer for a run that cannot be made, printed instead of rows nobody took."""
    return {"source": SOURCE, "harnessVersion": HARNESS_VERSION, "refused": detail,
            "pythonVersion": ".".join(str(part) for part in sys.version_info[:3])}


def absence_places():
    """Every place an absence is the correct answer, derived rather than listed.

    Two sources: the off arm, where the reason is one fact about the arm and holds for every firing
    cell, and the scenarios that declared an absence for a particular cell with the reason it is
    normal there. A cell that reads absent without appearing here, or appears here without reading
    absent, is a disagreement the checks report.
    """
    places = {}
    for declared in SCENARIOS:
        for cell in FIRING_CELLS:
            places[(OFF, declared["name"], cell)] = {"because": NO_REGISTRATION,
                                                     "answer": reading.ABSENT}
        for cell, why in declared["absent"].items():
            wanted = declared["expected"][cell]
            # recordedAs declares its absence through the predicate that reads a null, so the
            # answer recorded here is the null the journal actually carries.
            places[(ON, declared["name"], cell)] = {
                "because": why, "answer": None if wanted == NOT_PUBLISHED and cell == "recordedAs"
                else wanted}
    return places


def stability(earlier, later):
    """Whether the source stood still for the length of the run.

    Taken before the first subprocess and again after the last. A file edited while the run was
    going means earlier scenarios executed different bytes from later ones, and an identity
    recorded at either end would describe a source no single scenario ran.
    """
    digests = list((earlier.get("sourceDigests") or {}).values())
    digests += list((later.get("sourceDigests") or {}).values())
    # Equality is not enough: a digest that could not be taken is the same sentinel at both ends,
    # so an unreadable source would compare equal to itself and report the bytes as identified.
    unread = unreadable_among(digests)
    return {"before": earlier, "after": later, "digestsNotTaken": len(unread),
            "met": judged(earlier.get("sourceDigests") == later.get("sourceDigests")
                          and bool(earlier.get("sourceDigests")), unread),
            "what": "every source whose contents decide a run was readable and had the same digest"
                    " before the first subprocess and after the last"}


def compare(root):
    """Build both arms, drive every scenario at both, and read what each one has afterwards."""
    launcher = launcher_for(root)
    arms = {name: Arm(root, name, launcher) for name in ARMS}
    scenarios = {"_arms": {}, "_wrote": [launcher] + [place for arm in arms.values()
                                                      for place in arm.places]}
    for name, arm in arms.items():
        cells = {cell: read(cell, {INSTALL: arm.install, HOOK_FILE: arm.hook_file})
                 for cell in ARM_CELLS}
        wanted = ARM_INSTALL[name]
        disagreed = [{"cell": cell, "wanted": value, "found": cells[cell]["value"]}
                     for cell, value in wanted.items() if cells[cell]["value"] != value]
        scenarios["_arms"][name] = dict(
            cells, codexHome=str(arm.codex_home), argv=arm.argv,
            installed={"passed": not disagreed, "disagreed": disagreed, "wanted": wanted})

    for declared in SCENARIOS:
        scenarios[declared["name"]] = {}
        for name, arm in arms.items():
            built = build(arm, declared)
            firings = []
            expectations = [declared["expected"]]
            if declared["fireTwice"]:
                expectations.append(declared["second"])
            for expected in expectations:
                before = journal_records(arm, built["session"], built["turn"])
                fired = fire(arm, built, declared["stopHookActive"])
                record, unidentifiable = None, None
                if fired is not None:
                    record, unidentifiable = record_of(
                        before, journal_records(arm, built["session"], built["turn"]))
                firings.append(row(arm, declared, built, fired, record, expected,
                                   unidentifiable))
            scenarios[declared["name"]][name] = {
                "built": dict((key, built[key]) for key in ("workspace", "session", "turn")),
                "firings": firings}
    return scenarios


def document(scenarios, root, earlier):
    places = absence_places()
    rows = [(declared["name"], arm, index, fired)
            for declared in SCENARIOS
            for arm in ARMS
            for index, fired in enumerate(scenarios[declared["name"]][arm]["firings"])]
    failed = [name + "/" + arm + "#" + str(index)
              for name, arm, index, fired in rows if not fired["verdict"]["passed"]]
    armed = [name for name in ARMS if not scenarios["_arms"][name]["installed"]["passed"]]
    answers = measures(scenarios)
    missed = sorted(name for name, measure in answers.items()
                    if measure.get("answer") == MEASURED and not measure.get("met"))
    body = {
        "source": SOURCE,
        "harnessVersion": HARNESS_VERSION,
        "sourceIdentity": stability(earlier, source_identity()),
        "pythonVersion": ".".join(str(part) for part in sys.version_info[:3]),
        "root": str(root),
        "mode": completion.HOLD,
        "isolation": "no child task runs here and this process is the only writer of every marker"
                     " fact and store row under the root, which is what makes holding legitimate."
                     " Every block and every reservation below is synthetic hold-mode output, and"
                     " a default installation is observe mode and holds nothing.",
        "arms": scenarios["_arms"],
        "scenarios": dict((declared["name"], scenarios[declared["name"]])
                          for declared in SCENARIOS),
        "wroteOnlyInsideItsRoot": containment(root, scenarios["_wrote"]),
        "absenceIsNormalAt": [dict(place, arm=arm, scenario=name, cell=cell)
                              for (arm, name, cell), place in sorted(places.items())],
        "measures": answers,
        "supplemental": supplemental(scenarios),
        "standIns": STAND_INS,
        "notPerformed": NOT_PERFORMED_HERE,
        "armsThatDisagreed": armed,
        "rowsThatDisagreed": failed,
        "measuresThatMissedTheirBound": missed,
        "everyRowPassed": not failed and not armed,
    }
    # The answer, derived from every judgment this document carries rather than from the three
    # kinds someone remembered to collect. Rows, arms and measures were collected and the
    # supplemental observations were not, so one of them could fail while the command exited 0.
    counted = judgments(body)
    body["judgmentsCounted"] = len(counted)
    body["judgmentsThatFailed"] = [where for where, value in counted if value is False]
    body["passed"] = not body["judgmentsThatFailed"]
    return body


def main(argv=None):
    parser = argparse.ArgumentParser(description="Compare a destination that registered the"
                                                 " completion hook with one that did not.")
    parser.add_argument("--root", help="where to build both arms; a temporary directory otherwise")
    args = parser.parse_args(argv)

    if sys.version_info < RELAY_PYTHON:
        # The relay declares this floor and the command under test imports it. Below it there is no
        # run to report on, and rows that were never taken must not be printed as though they were.
        json.dump(refusal(
            "the relay requires Python " + ".".join(str(p) for p in RELAY_PYTHON) + " or newer,"
            " so no arm can be driven on this interpreter and no row was taken"),
            sys.stdout, indent=2, sort_keys=True)
        sys.stdout.write("\n")
        return 2

    keep = args.root is not None
    try:
        root = own_directory(args.root) if keep else Path(
            tempfile.mkdtemp(prefix="hook-comparison-")).resolve()
    except OSError as error:
        json.dump(refusal("a directory for this run could not be created: " + str(error)),
                  sys.stdout, indent=2, sort_keys=True)
        sys.stdout.write("\n")
        return 2
    answer = None
    try:
        earlier = source_identity()
        answer = document(compare(root), root, earlier)
    except RelayError as error:
        answer = refusal("a relay command a scenario needed refused: " + str(error))
    finally:
        if not keep:
            shutil.rmtree(root, ignore_errors=True)
    json.dump(answer, sys.stdout, indent=2, sort_keys=True)
    sys.stdout.write("\n")
    if answer.get("refused"):
        return 2
    return 0 if answer["passed"] else 1


if __name__ == "__main__":
    raise SystemExit(main())
