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
filled. A reading that could not be made answers with an Unreadable naming why; a reading whose
absence is a normal state answers ABSENT naming why it is normal there. Neither ever answers
False, and neither ever takes the value of the cell beside it.

The first of those is a TYPE rather than a spelling, which is the whole of CRW-103. While it was
the string UNREADABLE it fitted the slot a real value occupies: truthy, unequal to everything
real, hashable, and answering yes to "is there something there". Those are three different checks
and one string walked through all of them, so twenty-one consumption sites were closed one at a
time against it. Now the language refuses, and a site that treats a reading which was not taken
as a value breaks at that site instead of passing.

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
# 2 since CRW-103: a reading that was not taken is rendered as an object naming what was not read
# and why, where it used to be printed as the bare string a real answer could also have been.
# 3 since CRW-102: every firing is started by a tracer, and three of the readings below are taken
# from what that tracer recorded rather than from what this file reported about itself.
HARNESS_VERSION = 3

# The relay declares this floor in its own metadata, and the guard is imported by the command this
# harness runs. Below it there is no run to report on, so the refusal is the answer and it is
# printed as one rather than degraded into rows nobody took.
RELAY_PYTHON = (3, 11)

OFF, ON = "off", "on"
ARMS = (OFF, ON)

# What became of another owner's registration: still the entry it was, rewritten in place, or
# gone. Three answers, because an entry whose command survived while its shape changed is neither
# of the other two.
FOREIGN_PRESENT = "present"
FOREIGN_ALTERED = "altered"
FOREIGN_MOVED = "moved"
FOREIGN_DISPLACED = "displaced"

# What each arm's install must report before anything it left behind is believed. An installer that
# fails before writing leaves exactly the empty Codex home a successful dry run leaves, so without
# this the off arm would pass on a run that never happened.
ARM_INSTALL = {
    OFF: {"installExit": 0, "installResult": "MISSING", "installSettings": "config_would_create",
          "registration": 0, "foreignRegistration": FOREIGN_PRESENT},
    ON: {"installExit": 0, "installResult": "CREATED", "installSettings": "config_created",
         "registration": 1, "foreignRegistration": FOREIGN_PRESENT},
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

# The process witness answers in its own vocabulary too. The command reading is a comparison
# between two sources rather than a path, because what it asks is whether the argv the kernel
# recorded is the argv the arm's own hook file names, and only the place holding both can say.
COMMAND_AS_REGISTERED = "command_as_registered"
COMMAND_DIFFERS = "command_differs"
ENTRY_POINT_NOT_IN_ARGV = "entry_point_not_in_argv"

# Sources. A payload carries the stamp of the source that produced it, and read() checks the stamp
# before walking a path: handing one source's payload to another source's row is the cheapest way
# for a reading to answer a question it was never asked.
INSTALL = "install"
HOOK_FILE = "hook-file"
JOURNAL = "journal"
STDOUT = "stdout"
MARKER_ROOT = "marker-root"
HARNESS = "harness"
# What the tracer recorded. A source of its own, and deliberately not HARNESS: these are the
# readings this file used to answer about itself, and the whole point of them is that something
# else now answers.
WITNESS = "process-witness"

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
    ("startedExecutable", WITNESS, ("startedExecutable",), "hook_comparison.witness_payload"),
    ("startedCommand", WITNESS, ("startedCommand",), "hook_comparison.witness_payload"),
    ("startedEntryPoint", WITNESS, ("startedEntryPoint",), "hook_comparison.witness_payload"),
    ("unexpectedExecutions", WITNESS, ("unexpectedExecutions",), "hook_comparison.witness_payload"),
    ("writesOutsideRoot", WITNESS, ("writesOutsideRoot",), "hook_comparison.witness_payload"),
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

# The states a producer OUTSIDE this file answers in when its reading did not happen. The runtime
# modules carry these as the state of a Reading, where the state is a field of its own and the
# value sits beside it, so a record read off disk can still arrive carrying one as text. read()
# converts them, so this names what has to be converted rather than what may be consumed.
NOT_A_VALUE = (reading.UNREADABLE, reading.ACCESS_ERROR)


class NotAValue(Exception):
    """Raised where a reading that was not taken is used as though it were a value.

    Deliberately NOT a TypeError. reading.SHAPE_FAILURES carries TypeError and reading.region
    turns one into a quiet Refused reporting "could not read", so a TypeError subclass could be
    converted into precisely the silent unreadable reading this arrangement exists to refuse.
    Measured rather than assumed: a TypeError subclass raised inside a region comes back out as
    state UNREADABLE, and an Exception subclass comes back out as itself.
    """


class Unreadable(object):
    """A reading that could not be taken. It is not a value and it will not act like one.

    CRW-68 closed twenty-one consumption sites by hand because this answer was the non-empty
    string UNREADABLE. A non-empty string is truthy, compares unequal to every real answer,
    hashes into a set, and satisfies "is there something there" - three separate checks, and one
    string walked through all of them. Nothing but attention at each site kept it out, so a new
    site reopened it. Here every use that treats it as a value raises NotAValue instead, which
    moves the guarantee from attention to the language.

    ONE INSTANCE PER READING, and never a module-level one. That is the invariant this class
    rests on rather than a detail of how it is built. CPython compares by identity before it
    calls __eq__ inside a container, so with a shared sentinel both x in (x,) and the mapping
    comparison {a: x} == {a: x} answer equal without ever reaching __eq__ - and the second is
    exactly the comparison stability() makes over two source digests. A shared sentinel would
    therefore report two digests nobody could take as the same bytes, which is the defect the
    comment there already warns about. A fresh instance carrying its own reason cannot.

    repr() stays readable on purpose, so a traceback and a failing assertion can still say which
    reading failed and why.
    """

    __slots__ = ("state", "why")

    def __init__(self, why, state=reading.UNREADABLE):
        self.state = state
        self.why = why

    def __repr__(self):
        return "<not read: " + str(self.state) + ": " + str(self.why) + ">"

    def rendered(self):
        """The JSON form, which is not a value there either: an object equals no cell's answer.

        It carries its own reason because the places that hold one are not all cells. A cell has
        a detail field beside it and repeats the reason; sourceDigests, workingTree and the
        containment answers have nothing beside them, so the reason travels with the value.
        """
        return {"notRead": self.state, "why": self.why}

    def _refuse(self, how):
        refusal = NotAValue("a reading that was not taken was " + how + ", which is something"
                            " only a value can be: " + str(self.why))
        refusal.at = consumed_at()
        raise refusal

    def __eq__(self, other):
        self._refuse("compared for equality")

    def __ne__(self, other):
        self._refuse("compared for inequality")

    def __lt__(self, other):
        self._refuse("ordered")

    __le__ = __gt__ = __ge__ = __lt__

    def __bool__(self):
        self._refuse("asked whether it is true")

    def __hash__(self):
        self._refuse("hashed into a set or used as a key")

    def __str__(self):
        self._refuse("formatted as text")

    def __format__(self, specification):
        self._refuse("formatted as text")

    def __len__(self):
        self._refuse("measured for length")

    def __iter__(self):
        self._refuse("iterated")

    def __contains__(self, other):
        self._refuse("asked what it contains")

    def __getitem__(self, key):
        self._refuse("indexed")


def not_read(value):
    """Whether this is a reading that was not taken, rather than something a reading found.

    The one question a consumer may ask about one. It is an isinstance rather than a comparison,
    because a comparison is the thing that cannot be trusted here: what it would have to compare
    against is the answer that is not a value.
    """
    return isinstance(value, Unreadable)


# The code objects of the type's own refusals, taken from the class rather than listed. The frame
# that matters is the one that USED the reading, and reading.where() answers with the deepest
# frame of a traceback - which here is always the single raise inside the class, so every refusal
# would name that one line and the consumption site would be gone. Derived rather than written
# down because a refusal added later has to be skipped too.
_REFUSALS = frozenset(value.__code__ for value in vars(Unreadable).values()
                      if hasattr(value, "__code__"))


def consumed_at():
    """Where a reading that was not taken was used, which is never where the refusal is raised.

    Walked outward from the refusal to the first frame that is not one of the type's own, so the
    answer is the comparison, the truthiness check or the formatting that consumed it."""
    frame = sys._getframe(1)
    while frame is not None and frame.f_code in _REFUSALS:
        frame = frame.f_back
    if frame is None:
        return None
    return os.path.basename(frame.f_code.co_filename) + ":" + str(frame.f_lineno)


def _answered(value, wanted):
    """Whether this reading answered exactly this. A reading that was not taken answers no.

    Answering no is safe only because every place that asks this collects the readings that were
    not taken beside it and cannot be met while that list is non-empty. The two are written next
    to each other for that reason: "it did not say this" and "nobody could read whether it said
    this" are different facts, and only one of them is about the hook.
    """
    return not not_read(value) and value == wanted

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
    """Which of these readings could not be taken, each naming why."""
    return [value.why for value in values if not_read(value)]


def across(scope, values, predicate):
    """Apply a predicate over exactly the arms a judgment declares it speaks for.

    A claim about both arms and a predicate that reads one is how this went wrong twice in the
    same judgment: the first time by folding an unreadable reading, the second by checking the on
    arm while the evidence it cited was the pair. The scope is declared, the values are recorded
    per arm, and a check requires the two to be the same set, so a predicate cannot quietly speak
    for fewer arms than its claim does.
    """
    return all(predicate(values[arm]) for arm in scope)


def judged(met, not_taken):
    """A verdict, which cannot be true while a reading under it was not taken.

    Every met in this file is this call. Stating the rule and leaving each verdict to apply it
    separately is how one of them came not to: the standing check that was supposed to catch that
    only fires when a not-taken count is already nonzero, which never happens in a healthy run, so
    it watched the property without ever exercising it. Here the guard cannot be left out, because
    there is nowhere else to compute the answer.

    met may be a callable, and then it is CALLED ONLY AFTER the count is checked. That makes the
    ordering structural instead of remembered: a met expression that reads a cell cannot run at
    all while one of the readings under it was not taken, where before it ran and its answer was
    thrown away afterwards. Three of them read cell values directly and would now raise rather
    than return false. It also closes a trap in the old shape, because an uncalled function is
    truthy, so a predicate passed by mistake used to produce a true verdict.
    """
    count = not_taken if isinstance(not_taken, int) else len(not_taken or ())
    if count:
        return False
    return bool(met() if callable(met) else met)

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
    "the harness as sole writer":
        "replaces a sandbox grant. It is what makes hold mode legitimate here, and it means no row"
        " proves that a real child under a real grant could not forge the facts the guard read.",
}

# Put back into the document, by name, on a run where the process boundary could not be witnessed.
# The stand-in above was removed because a tracer now answers what it stood in for; on a host that
# has no tracer nothing answers it, and a list that had quietly dropped the entry anyway would be
# describing a different run from the one that was made.
STAND_IN_WITHOUT_WITNESS = {
    "the reported argv":
        "replaces a witness at the process boundary, on this run only, because no tracer could be"
        " established here. The command is read back out of the hook file and compared, and the"
        " journal and the published observations are read from disk, so a row establishes that a"
        " hook process ran and reached the guard. It does not establish which executable the"
        " kernel started. A run on a host carrying a tracer answers that by observation.",
}

NOT_PERFORMED_HERE = {
    "CRW-68 criterion 4":
        "the real installed path from a child's completion through a parent's verification, a"
        " correction, a re-verification and a Linear write read back on one persistent database."
        " It needs an install on a host and a real round trip between tasks.",
    "CRW-68 criterion 5":
        "installation, registration, firing, delivery acceptance and artifact verification"
        " distinguished on a real host, with recovery after a daemon, connection or hook fault."
        " This run separates registration from starting the process from firing on a temporary"
        " destination, witnesses which executable the start reached, and stops there.",
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


def _unreadable(cell, source, path, detail, state=reading.UNREADABLE):
    """A cell whose reading could not be made.

    A distinct answer, not a negative one. Returning False here, or falling through to whatever
    the neighbouring cell said, is the defect this module exists to refuse.

    Distinct by TYPE since CRW-103, so a consumer that treats it as the value it sits beside
    breaks at that consumer rather than being let through by it.

    The state travels with it. An access error and a shape nobody could read are two of the four
    answers reading.py keeps apart on purpose, and rebuilding one as the other here would collapse
    that partition at the one door every cell goes through.
    """
    return _cell(cell, source, path, Unreadable(detail, state=state), False, detail)


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
    answer returns an Unreadable naming why.
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
    if not_read(found) or found in NOT_A_VALUE:
        # A producer that already said it could not read is not made readable by the fact that its
        # answer was where this table expected it. The path being reachable answers "was there an
        # answer here", which is a different question from "was the reading made".
        #
        # Both shapes are converted here because read() is the only door. A producer in this file
        # hands over the type; a record read off disk carries the state as text, and ACCESS_ERROR
        # used to walk straight through this line as a perfectly good value. A cell whose value
        # was either spelling would be exactly the defect the type removes.
        return _unreadable(declared_cell, source, path,
                           str(payload.get("detail")
                               or (found.why if not_read(found)
                                   else "the reading reported itself unreadable")),
                           state=found.state if not_read(found) else found)
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

    def __init__(self, root, name, launcher, tracer=None):
        self.name = name
        self.run_root = Path(root)
        self.root = Path(root) / name
        self.codex_home = self.root / "codex"
        self.markers = self.root / "markers"
        self.state = self.root / "state"
        self.journal = self.root / "journal"
        self.work = self.root / "work"
        self.witness = self.root / "witness"
        for directory in (self.codex_home, self.markers, self.state, self.journal, self.work,
                          self.witness):
            directory.mkdir(parents=True, exist_ok=True)
        self.db = self.state / "relay.sqlite3"
        self.launcher = launcher
        # Handed in rather than probed here, because whether this host can witness a process is
        # one fact about the host and both arms have to be told the same one.
        self.tracer = tracer
        # Built before the install, because the install is the first subprocess and it is the one
        # an inherited settings pointer would send outside this root.
        self.environment = environment(self)
        hooks_file(self.codex_home).write_text(json.dumps(FOREIGN_HOOKS, indent=2),
                                               encoding="utf-8")
        self.places = [self.codex_home, self.markers, self.state, self.journal, self.work,
                       self.witness, hooks_file(self.codex_home)]
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
            return {"source": INSTALL,
                    "exitCode": Unreadable("the install did not finish within its timeout"),
                    "detail": "the install did not finish within its timeout"}
        except OSError as error:
            return {"source": INSTALL,
                    "exitCode": Unreadable("the install could not be started: " + str(error),
                                           state=reading.ACCESS_ERROR),
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
    """Whether the entry that was in the file before the install is still the entry it was.

    The whole entry, not its command. The claim is that another owner's registration survived, and
    an entry whose command still reads /opt/cxc/stop while its type, timeout or matcher changed
    has not survived: it has been rewritten, and the owner would find a registration it did not
    make. Matching on the command alone made the predicate narrower than the claim, which is the
    same shape as a judgment reading one arm while speaking for two.
    """
    seeded = FOREIGN_HOOKS["hooks"][completion.EVENT][0]
    groups = ((document.get("hooks") or {}).get(completion.EVENT) or [])
    found = None
    for matcher_index, group in enumerate(groups):
        for hook_index, entry in enumerate((group or {}).get("hooks") or []):
            if isinstance(entry, dict) and entry.get("command") == FOREIGN_COMMAND:
                found = (matcher_index, hook_index, group, entry)
                break
        if found:
            break
    if found is None:
        return FOREIGN_DISPLACED
    matcher_index, hook_index, group, entry = found
    if entry != seeded["hooks"][0] or (group or {}).get("matcher") != seeded.get("matcher"):
        return FOREIGN_ALTERED
    if (matcher_index, hook_index) != (0, 0):
        # Position is part of identity here, not presentation: hooks.identity derives a hook's
        # identity from the event, the matcher index and the hook index, so an entry appended
        # ahead of this one moves it and invalidates the hash its owner trusted. Finding it
        # anywhere in the file is a narrower question than whether it survived.
        return FOREIGN_MOVED
    return FOREIGN_PRESENT



# ------------------------------------------------------------------ building a scenario


class RelayError(RuntimeError):
    pass


# What a relay command has to answer with before anything is taken out of its response. Declared
# per command rather than checked at each site, because this is the third member of one family: a
# parse that failed was covered, then valid JSON that is not an object, and then an object missing
# the field the caller was about to read. Each time the boundary was a layer narrower than the
# thing that could go wrong, so the contract lives with the command and relay() is the only door.
RESPONSE_FIELDS = {
    "intent-declare": ("assignmentId", "assignmentDir"),
    "register": ("relationshipId",),
}


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
        # An empty answer is fine for a command nothing is read out of, and is a refusal for one
        # this run takes fields from. Returning {} to both is how the field check was skipped for
        # exactly the case that motivated it.
        answered = {}
        if RESPONSE_FIELDS.get(args[0] if args else ""):
            raise RelayError(" ".join(args[:2]) + " answered with nothing, and this run reads "
                             + ", ".join(RESPONSE_FIELDS[args[0]]) + " out of it")
        return answered
    try:
        answered = json.loads(done.stdout)
    except ValueError:
        raise RelayError(" ".join(args[:2]) + " printed something that is not JSON")
    if not isinstance(answered, dict):
        raise RelayError(" ".join(args[:2]) + " printed valid JSON that is not an object, so"
                         " nothing can be read out of it")
    missing = [field for field in RESPONSE_FIELDS.get(args[0] if args else "", ())
               if field not in answered]
    if missing:
        # Exit zero and a readable object still is not an answer this run can use. Reported here
        # rather than as the KeyError a caller would raise, because that ends the command with a
        # traceback in place of the one document it promises.
        raise RelayError(" ".join(args[:2]) + " answered without " + ", ".join(missing))
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
    """Run the command the registration names, started by a tracer, with the Stop payload on stdin.

    Calling completion.run() here would have been a reading of this checkout. The row says a
    registered hook fired, and the registered hook is a COMMAND LINE in that arm's hooks.json: an
    interpreter, the entry point, and the settings path the install chose. Run a helper instead and
    the entry point, the settings argument and the stdin contract are all untested while the row
    still reaches its answer.

    The tracer STARTS the command rather than attaching to it, and that is what makes which
    executable ran an observation instead of a report. Attaching was measured and works, and the
    execve is over before any attach can land, so the only answer left would be this file reading
    /proc and reporting what it saw. Started under the tracer, the kernel's own execve record is
    the first line of a file the tracer wrote. Where no tracer could be established the command
    runs exactly as it did before and the witness readings say they could not be taken.
    """
    command = (arm.hook_file or {}).get("command")
    if not command:
        return None
    argv = shlex.split(command)
    tracer = arm.tracer or {}
    log, prefix = None, []
    if tracer.get("usable"):
        # Named by mkstemp rather than by the scenario, because one turn can fire twice and a
        # second firing writing over the first trace would leave the row describing the other one.
        handle, named = tempfile.mkstemp(prefix=built["session"] + "-", suffix=".strace",
                                         dir=str(arm.witness))
        os.close(handle)
        log = Path(named)
        prefix = trace_argv(tracer["tracer"], log)
    started = time.monotonic()
    try:
        done = subprocess.run(
            prefix + argv,
            input=json.dumps(stop_payload(built, stop_hook_active)).encode("utf-8"),
            capture_output=True, timeout=300, env=arm.environment)
    except subprocess.TimeoutExpired:
        return {"faulted": "the registered command did not finish within its timeout",
                # It was asked and it did not answer, which is not the same as not having been
                # able to ask at all. Those are two of the four answers reading.py keeps apart,
                # and the reading each firing cell reports has to be the right one of them.
                "faultedState": reading.UNREADABLE,
                "argv": argv}
    except OSError as error:
        return {"faulted": "the registered command could not be started: " + str(error),
                "faultedState": reading.ACCESS_ERROR,
                "argv": argv}
    wall = int((time.monotonic() - started) * 1000)
    answer = {"stdout": done.stdout.decode("utf-8", "replace"),
              "stderr": done.stderr.decode("utf-8", "replace"),
              "exitCode": done.returncode, "wallMs": wall,
              "argv": argv, "tracerArgv": prefix or None,
              "tracePath": str(log) if log is not None else None}
    if log is None:
        return answer
    # The tracer writes its own diagnostics to the stderr the hook contractually leaves empty,
    # and its own prefix is what tells them apart. A hook that forged the prefix would make this
    # run red rather than green, which is the safe direction for a guess to fail in.
    answer["tracerSaid"] = "\n".join(line for line in answer["stderr"].splitlines()
                                     if line.startswith(TRACER + ":"))
    try:
        answer["trace"] = parse_trace(log.read_text(encoding="utf-8", errors="replace"),
                                      os.getcwd())
    except (OSError, ValueError) as error:
        answer["traceDetail"] = ("the trace of this firing could not be read: "
                                 + type(error).__name__ + ": " + str(error))
    if answer["tracerSaid"] and not (answer.get("trace") or {}).get("executions"):
        # The tracer never started it. That is not a firing that answered nothing, it is a firing
        # that did not happen, and every cell under it has to say so rather than read an absence.
        return {"faulted": "the tracer could not start the registered command: "
                           + answer["tracerSaid"][:200],
                "faultedState": reading.ACCESS_ERROR, "argv": argv,
                "tracerArgv": prefix, "tracePath": str(log)}
    return answer


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


# ------------------------------------------------------------------ the process witness

# Two questions this arrangement could not answer are answered here, and it could not answer
# either one for the same reason: nothing observed the process. The command was read out of the
# arm's hook file, executed, and reported back by the same file that executed it, so a run that
# started something else while reporting the registration would have agreed with itself. And the
# places checked for containment were the places this run creates, so a write anywhere else was
# not permitted so much as invisible.
#
# The witness is a tracer that STARTS the registered command rather than one that attaches to it.
# Attaching was measured and works, and it is the weaker answer: the execve happens before any
# attach can land, so what would be left is this file reading /proc and reporting what it saw,
# which is the harness attesting to itself and is the shape of the stand-in being removed.
# Started under the tracer, the kernel's own execve record is the first line of a file ANOTHER
# program wrote, and the checks read it the way they read a journal.
#
# A tracer is not available everywhere, and the two failures are not one. A host that has none is
# an arrangement that cannot reach this criterion, reported not performed beside the two the
# contract already cannot reach, with the stand-in truthfully put back and --require-witness
# available to anyone who needs the run to insist. A tracer that ran and produced something
# unreadable is a reading that was not taken, and nothing is met while one is outstanding.
# Neither one ever becomes "nothing was written".

TRACER = "strace"

# PATH_MAX is 4096, so a path never needs more. Anything longer comes back truncated and is read
# as a reading that was not taken rather than as the shorter path it looks like.
TRACE_STRING_LIMIT = 4096

# Whether a call acts ON the name it is given or reaches THROUGH it. A symlink, a mkdir, an
# unlink and a rename all operate on the entry and never follow its last component; an open
# and a truncate follow it and land on whatever it points at. Containment has to ask about
# the one the call actually touched, and answering both at once was measured rejecting a
# contained run for creating a link inside the root that pointed out of it.
# What asking the filesystem where a path leads can fail with, in ONE place because it is one
# fact and it is not the obvious one. OSError is what everybody catches. Measured on 3.10.20,
# 3.11.15, 3.12.13, 3.13.14 and 3.14.4 rather than read out of the documentation:
#   a symlink loop, resolved whole   RuntimeError on 3.10, 3.11 and 3.12; no raise on 3.13 and
#                                    3.14, which rebuilt resolve() on realpath. Resolving only
#                                    a parent never raised on any of the five.
#   expanduser over a home nobody    RuntimeError on all five, 3.13 and 3.14 included, which is
#   can name                         why this is not a question about old interpreters only.
#   a path carrying a NUL            ValueError on all five.
# All three answer the same question, so a site that catches one and not the others ends the
# command over a path it could have reported as unread - and takes every answer in the run that
# had nothing to do with that path down with it.
#
# Allowed around the observation ITSELF and nowhere wider. RuntimeError and ValueError also
# arrive from mistakes in this file, and a handler big enough to hold ordinary logic beside the
# call would relabel those as "unreadable": the harness hiding the kind of defect it exists to
# expose, behind the word it keeps for an honest gap. That is worse than the failure being
# fixed here, so the rule is the narrow catch rather than the wide one.
RESOLUTION_FAILURES = (OSError, RuntimeError, ValueError)

ENTRY = "the entry it names"
THROUGH = "whatever the entry leads to"

WRITES = "writes"
OPENS = "opens"
STARTS = "starts"
MOVES = "moves"

# What a path argument IS, per call, rather than per line. A rename unlinks its source, so both of
# its paths are touched; a symlink writes only the link it creates and its FIRST argument is a
# target that need not exist at all; the *at forms carry a directory descriptor the relative path
# is resolved against. "The first quoted string" would report a symlink's target as written and
# miss a rename's source. Each entry is (kind, ((path position, the position holding the directory
# descriptor that path is relative to, or None), ...)).
#
# The calls that write nothing are in this table because the tracer's filter is BUILT FROM ITS
# KEYS: an execve is what names the executable, and a chdir is what makes a later relative path
# unresolvable. Filter and parser cannot drift apart while there is only one list.
#
# Ownership, timestamps and extended attributes are deliberately absent. A metadata story told
# halfway is worse than one not told, so they are declared below as not covered instead.
TRACED_CALLS = {
    "execve": (STARTS, ((0, None),), THROUGH),
    "execveat": (STARTS, ((1, 0),), THROUGH),
    # An open follows the last component unless it is told not to, and the ways of telling
    # it not to - O_NOFOLLOW, and O_CREAT with O_EXCL over an existing link - both make the
    # call FAIL on a symlink, and a failed call never reaches this table. So every open
    # here reached what its path pointed at.
    "open": (OPENS, ((0, None),), THROUGH),
    "openat": (OPENS, ((1, 0),), THROUGH),
    "openat2": (OPENS, ((1, 0),), THROUGH),
    "creat": (WRITES, ((0, None),), THROUGH),
    "truncate": (WRITES, ((0, None),), THROUGH),
    # A descriptor the kernel already resolved, which the tracer hands over resolved.
    "ftruncate": (WRITES, ((0, None),), THROUGH),
    # Everything below works on the NAME. None of them follows its last component: mkdir
    # and mknod fail if the name is taken, rmdir refuses a symlink, unlink and rename act
    # on the link itself, and a symlink writes the link and never its target - which need
    # not exist at all.
    "mkdir": (WRITES, ((0, None),), ENTRY),
    "mkdirat": (WRITES, ((1, 0),), ENTRY),
    "rmdir": (WRITES, ((0, None),), ENTRY),
    "unlink": (WRITES, ((0, None),), ENTRY),
    "unlinkat": (WRITES, ((1, 0),), ENTRY),
    "rename": (WRITES, ((0, None), (1, None)), ENTRY),
    "renameat": (WRITES, ((1, 0), (3, 2)), ENTRY),
    "renameat2": (WRITES, ((1, 0), (3, 2)), ENTRY),
    # The link, and not what it is linked FROM. A rename removes its source, so both of its
    # paths change; a hard link only adds a second name for an inode that is otherwise left
    # exactly as it was.
    "link": (WRITES, ((1, None),), ENTRY),
    "linkat": (WRITES, ((3, 2),), ENTRY),
    "symlink": (WRITES, ((1, None),), ENTRY),
    "symlinkat": (WRITES, ((2, 1),), ENTRY),
    "mknod": (WRITES, ((0, None),), ENTRY),
    "mknodat": (WRITES, ((1, 0),), ENTRY),
    "chdir": (MOVES, ((0, None),), ENTRY),
    "fchdir": (MOVES, ((0, None),), ENTRY),
}

# Which argument carries an open's flags, so that an open for reading is not counted as a write.
# openat2 carries them inside a struct and a substring is what reads them there.
OPEN_FLAGS = {"open": 1, "openat": 2, "openat2": 2}

# An open is a write when it can change the file. O_RDONLY is the absence of all of these.
#
# Deliberately the wider set, and the direction is the point. An open that merely COULD change
# the file has not changed it yet, and the call that would - write - carries a descriptor and
# no path, so it is not in this table and cannot be. Counting only the flags that change the
# file at open time would miss every write into a file that already existed outside the root,
# which is a false clean bill. Counting the capability reports a write that may not have
# happened, which is a false alarm somebody can explain. Only one of those two errors is the
# kind this file exists to refuse. What an entry actually was sits beside it as
# changedTheFile, which answers three ways and not two, so the difference is data rather
# than a silence and the undetermined case is neither of the determined ones.
WRITE_FLAGS = ("O_WRONLY", "O_RDWR", "O_CREAT", "O_TRUNC", "O_APPEND", "O_TMPFILE")

# What an open DID, which is three answers and not two. Calling it a boolean was wrong in both
# directions in turn: counting O_CREAT as a change overstates it against a file that already
# existed, and not counting it understates the one that created a file which did not. The
# trace says which flags were passed and that the call succeeded, and from that alone a plain
# O_CREAT determines NEITHER - so it answers neither, the way a reading that cannot be taken
# answers neither true nor false everywhere else in this file.
#
# O_TRUNC and O_TMPFILE change the file whatever was there. O_CREAT with O_EXCL created it,
# and only because these calls succeeded: an exclusive create over an existing file fails with
# EEXIST, and a failed call never reaches this table.
CHANGED = "changed"
MAY_HAVE_CHANGED = "may_have_changed"
ONLY_ABLE_TO_CHANGE = "only_able_to_change"
WHAT_AN_OPEN_DID = (CHANGED, MAY_HAVE_CHANGED, ONLY_ABLE_TO_CHANGE)

# The calls whose success does not establish that anything moved. See _what_a_path_call_did.
RENAMES = ("rename", "renameat", "renameat2")

CHANGING_FLAGS = ("O_TRUNC", "O_TMPFILE")
CREATED_IT = ("O_CREAT", "O_EXCL")

# What the witness must find, fixed before any subprocess starts, and compared against the path
# strings the KERNEL recorded rather than against what they resolve to afterwards. A resolution
# taken after the run answers about the filesystem as it is then: a symlink standing where the
# entry point belongs, repointed at the real file by the program it started, resolves to exactly
# the right answer and is exactly the substitution this exists to catch. The installer writes
# these two strings verbatim - runtime_install passes sys.executable when no --python is given,
# interpreter_for keeps an absolute path unresolved, and command_for emits [interpreter, script,
# settings] - so there is no legitimate alias to accommodate.
EXPECTED_WITNESS = {
    "startedExecutable": sys.executable,
    "startedCommand": COMMAND_AS_REGISTERED,
    "startedEntryPoint": str(ROOT / "scripts" / completion.ENTRY_POINT_NAME),
    "unexpectedExecutions": [],
    "writesOutsideRoot": [],
}

WITNESS_CELLS = tuple(cell for cell, source, _p, _q in CELLS if source == WITNESS)

WITNESS_CRITERION = ("every firing was started by the command its own registration names,"
                     " from the interpreter and the program this checkout declares, without"
                     " re-executing into anything else, and wrote nowhere outside the"
                     " directory the run made for itself")

# What a row through this witness still cannot say, as data rather than as a claim of
# completeness. A detector that declares no blind spot is claiming to have none.
WITNESS_DOES_NOT_COVER = (
    "identity here is by path. A file replaced at that path while the run is going and put back"
    " before the last digest is not caught: sourceIdentity compares two moments and says nothing"
    " about the interval between them, and the bytes the kernel actually read at exec are not"
    " witnessed. A row establishes which path was started, not which bytes sat at it throughout",
    "a write through a descriptor this trace never saw opened - inherited across the exec,"
    " received over a unix socket, taken with pidfd_getfd or opened by handle - and the calls that"
    " write through one rather than through a path: write, pwrite, copy_file_range, splice,"
    " sendfile, a shared mapping, and io_uring, which can open and write with no syscall here",
    "whether a path opened in a way that could change it was actually changed. The call that"
    " would do it carries a descriptor and no path, so this reading counts the open instead."
    " It errs towards reporting a write that may not have happened rather than towards"
    " missing one. changedTheFile beside each entry says which of THREE it is, because a"
    " plain O_CREAT created the file if it was absent and changed nothing if it was there,"
    " and this trace does not say which, so it says may_have_changed rather than picking."
    " A successful rename is read the same way and for the same reason: a rename between two"
    " names for one file returns success and performs no other action, and one line carrying"
    " two paths and a zero cannot tell that from a move",
    "a change to an inode that is not a change to a path: the link count a hard link moves,"
    " and ownership, timestamps and extended attributes generally. The link is recorded as"
    " written and the file it was linked from is not",
    "a filesystem socket created by bind, which makes a path without any call in this table",
    "ownership, timestamps, extended attributes and access control lists. They are not in the"
    " traced set, so they are not in the trace at all, and no row says anything about them",
    "where a path led AT THE SYSCALL. Containment resolves it after the run, so a symlink that"
    " changed in between is answered as it is afterwards",
    "the tracer itself, which is trusted rather than checked, and its own trace file, which it"
    " writes without tracing",
    "every subprocess this run starts that is NOT a firing: the two installs, the relay commands"
    " that build each scenario, and the probe. They are started before or around the firings and"
    " none of them is traced, so a write one of them made outside the root would be caught only"
    " if it landed in a place wroteOnlyInsideItsRoot already names. What this witness covers is"
    " the firings, which is what the criterion beside it says",
    "a host whose tracer rejects one of the options this witness needs. The probe reports that"
    " as not performed, carrying the tracer's own complaint, rather than tracing with a"
    " narrower option set whose output this parser was not written against",
    "anything a process does after the tracer stops, including a grandchild that outlives the"
    " firing. That firing is reported as faulted rather than as one that wrote nothing",
)

_ESCAPES = {"n": 10, "t": 9, "r": 13, "f": 12, "v": 11, "b": 8, "a": 7, "\\": 92, '"': 34}


def _what_an_open_did(flags):
    """Which of the three an open was, from the flags the tracer recorded and nothing else.

    The middle answer is the whole point. A successful O_CREAT without O_EXCL created the file
    if it was not there and changed nothing if it was, and this trace carries no reading of
    which. Answering either one would be inventing the half of the evidence that is missing.
    """
    if any(flag in flags for flag in CHANGING_FLAGS):
        return CHANGED
    if all(flag in flags for flag in CREATED_IT):
        return CHANGED
    if "O_CREAT" in flags:
        return MAY_HAVE_CHANGED
    return ONLY_ABLE_TO_CHANGE


def _what_a_path_call_did(name):
    """Whether a call that names a path established, by succeeding, that it changed one.

    One rule, running the same direction as _what_an_open_did: a call is reported as having
    CHANGED something only where it could not have succeeded without doing it. Where the traced
    line, and what it carries of the state before it, leave both readings open, the answer is
    the middle one - because answering either way supplies the half of the evidence the trace
    does not have.

    Nearly every write in the table clears that bar, and the checks hold them to it. A mkdir
    that succeeded made a directory, an unlink that succeeded removed a name, a symlink that
    succeeded created a link, a mknod that succeeded made a node: each fails outright where the
    thing it would do is already done, so succeeding IS the change.

    The rename family does not clear it, and its own specification is the reason rather than a
    guess about kernels. A rename whose two arguments name the same entry, or name two links to
    one file, returns successfully and performs no other action - measured here doing exactly
    that, with both names still in place afterwards. One traced line carries two paths and a
    zero: no inode, and nothing from before the call. That is the shape of a plain O_CREAT one
    call family over, and it gets the same middle answer.

    A truncate stays certain, and it is worth saying why because it looks like the same case:
    it performs its operation whether or not the length already matched, and truncating a file
    that was already empty was measured moving its modification time.
    """
    if name in RENAMES:
        return MAY_HAVE_CHANGED
    return CHANGED


def _arguments_of(text):
    """The top-level arguments of one traced call, split on structure rather than on commas.

    A path may hold a comma, a flag set holds pipes, and the tracer nests braces, brackets and the
    angle brackets it puts a resolved path in. Counting commas would cut a path in half and then
    resolve half of one, which is a wrong answer where an unreadable line was available.
    """
    found, depth, quoted, escaped, current = [], 0, False, False, []
    for char in text:
        if quoted:
            current.append(char)
            if escaped:
                escaped = False
            elif char == "\\":
                escaped = True
            elif char == '"':
                quoted = False
            continue
        if char == '"':
            quoted = True
            current.append(char)
            continue
        if char in "([{<":
            depth += 1
        elif char in ")]}>":
            depth -= 1
        elif char == "," and depth == 0:
            found.append("".join(current).strip())
            current = []
            continue
        current.append(char)
    found.append("".join(current).strip())
    return [one for one in found if one]


def _string_argument(token):
    """The path the tracer printed, back out of its own escaping, or None if it cannot be read.

    A truncated string answers None rather than a shorter path. The tracer marks one by putting
    its ellipsis after the closing quote, and a path cut off at its limit names a different file
    from the one that was written - often a directory above it, which would resolve inside a root
    the whole path leaves.
    """
    if not token.startswith('"'):
        return None
    closing = token.rfind('"')
    if closing <= 0:
        return None
    if token[closing + 1:].strip():
        return None
    body, out, index = token[1:closing], bytearray(), 0
    while index < len(body):
        char = body[index]
        if char != "\\":
            out.extend(char.encode("utf-8"))
            index += 1
            continue
        index += 1
        if index >= len(body):
            return None
        marker = body[index]
        if marker == "x":
            digits = body[index + 1:index + 3]
            if len(digits) < 2:
                return None
            try:
                out.append(int(digits, 16))
            except ValueError:
                return None
            index += 3
        elif marker in "01234567":
            digits = ""
            while len(digits) < 3 and index < len(body) and body[index] in "01234567":
                digits, index = digits + body[index], index + 1
            out.append(int(digits, 8))
        elif marker in _ESCAPES:
            out.append(_ESCAPES[marker])
            index += 1
        else:
            return None
    return os.fsdecode(bytes(out))


def _resolved_descriptor(token):
    """The path the tracer's -y put on a descriptor, which is the kernel's own resolution of it.

    Read rather than recomputed. Resolving the string here would answer about the filesystem as it
    is after the run, and a file renamed or removed in between would resolve somewhere the write
    never went.
    """
    if "<" not in token or not token.endswith(">"):
        return None
    inside = token[token.index("<") + 1:-1]
    if inside.endswith(" (deleted)"):
        inside = inside[:-len(" (deleted)")]
    return inside or None


def _path_argument(arguments, position, directory_position, cwd):
    """One declared path argument, resolved, or the reason it could not be read.

    A position holds either a path the caller passed or a descriptor the tracer resolved, and both
    are answers. Anything else is a line this parser cannot read, and it says so rather than
    guessing which of the two it was looking at.
    """
    if position >= len(arguments):
        return None, "the call carries no argument at position " + str(position)
    token = arguments[position]
    named = _string_argument(token)
    if named is None:
        resolved = _resolved_descriptor(token)
        if resolved is None:
            return None, ("argument " + str(position) + " is neither a path this parser can read"
                          " nor a descriptor the tracer resolved")
        return resolved, None
    if named.startswith("/"):
        return named, None
    base = None
    if directory_position is not None and directory_position < len(arguments):
        base = _resolved_descriptor(arguments[directory_position])
    base = base or cwd
    if not base:
        return None, ("a relative path with no directory to resolve it against, because the"
                      " working directory stopped being known at a chdir earlier in this trace")
    return os.path.join(base, named), None


def _argv_of(arguments, position):
    """The argument list the kernel was handed, out of the array the tracer printed.

    An array the tracer abbreviated holds a word that is not a quoted string, so it answers
    unreadable rather than short. A short argv would be a different command that happened to agree
    with the registration on its first words.
    """
    if position >= len(arguments):
        return None, "the call carries no argument list"
    token = arguments[position]
    if not (token.startswith("[") and token.endswith("]")):
        return None, "the argument list is not one this parser can read"
    words = []
    for one in _arguments_of(token[1:-1]):
        named = _string_argument(one)
        if named is None:
            return None, "a word of the argument list could not be read"
        words.append(named)
    return words, None


def _result_of(body):
    """Where the arguments end and the result begins, across the tracer's column alignment.

    Found by walking back from the end to an equals sign whose left side ends in the bracket
    that closes the arguments. Splitting on the literal ") = " looked right and read nothing:
    the tracer pads short calls out to a column, so a real line is ")        = 0" and every
    one of them would have been reported as a call carrying no result.
    """
    at = len(body)
    while True:
        at = body.rfind("=", 0, at)
        if at < 0:
            return -1, None
        before = body[:at].rstrip()
        # An equals sign inside an argument - openat2 carries flags= inside a struct - has
        # something other than the closing bracket to its left, so the walk goes on past it.
        if before.endswith(")"):
            return len(before) - 1, body[at + 1:].strip()


def parse_trace(text, cwd):
    """Every start and every write this trace recorded, and every line it could not read.

    The unreadable lines are carried rather than dropped. A line this parser cannot read could be
    the write, so a trace holding one cannot answer that nothing was written - which would be the
    substitution this whole file refuses, arriving through a parser instead of through a cell.
    """
    answer = {"executions": [], "writes": [], "unreadable": [], "failedAttempts": 0,
              "restarted": 0, "lines": 0, "writeCalls": 0, "rootPid": None}
    unfinished = {}
    for number, line in enumerate(text.splitlines(), 1):
        if not line.strip():
            continue
        answer["lines"] += 1
        head, _separator, body = line.partition(" ")
        body = body.strip()
        if not head.isdigit() or not body:
            answer["unreadable"].append({"line": number, "text": line[:200],
                                         "why": "no process identifier and call"})
            continue
        pid = int(head)
        if body.startswith("+++") or body.startswith("---"):
            continue
        if body.startswith("<... "):
            name = body[5:].split(" ", 1)[0]
            held = unfinished.pop((pid, name), None)
            if held is None:
                answer["unreadable"].append({"line": number, "text": line[:200],
                                             "why": "a resumed call whose interrupted half is not"
                                                    " in this trace"})
                continue
            body = name + "(" + held + body.partition("resumed>")[2]
        elif body.endswith("<unfinished ...>"):
            if "(" not in body:
                answer["unreadable"].append({"line": number, "text": line[:200],
                                             "why": "an interrupted call with no arguments"})
                continue
            name = body.split("(", 1)[0].strip()
            unfinished[(pid, name)] = body[body.index("(") + 1:-len("<unfinished ...>")].rstrip()
            continue
        name = body.split("(", 1)[0].strip()
        if name not in TRACED_CALLS:
            # The filter is built from this table's keys, so a call outside it is not one the
            # tracer was asked for. Unreadable rather than skipped, because the two drifting apart
            # is how a write stops being looked at without anyone noticing.
            answer["unreadable"].append({"line": number, "text": line[:200],
                                         "why": "a call this parser was not built to read"})
            continue
        opened, (closed, result) = body.find("("), _result_of(body)
        if opened < 0 or closed < opened or result is None:
            answer["unreadable"].append({"line": number, "text": line[:200],
                                         "why": "the call carries no result"})
            continue
        arguments = _arguments_of(body[opened + 1:closed])
        kind, positions, reaches = TRACED_CALLS[name]
        if answer["rootPid"] is None:
            answer["rootPid"] = pid
        if result.startswith("?"):
            # Interrupted and restarted by the kernel. It returns on a later line, and counting it
            # here would count it twice.
            answer["restarted"] += 1
            continue
        if result.startswith("-1"):
            # It did not happen. Counted, because a run full of refused writes is worth seeing,
            # and never added to what was written.
            answer["failedAttempts"] += 1
            continue
        if kind == MOVES:
            # From here on a relative path resolves against somewhere this parser was not told
            # about, so those lines become unreadable rather than resolved against a directory
            # they no longer mean. Whole-trace rather than per process: the stricter of the two.
            cwd = None
            continue
        if kind == STARTS:
            path, why = _path_argument(arguments, positions[0][0], positions[0][1], cwd)
            argv, argv_why = _argv_of(arguments, 1 if name == "execve" else 2)
            if path is None or argv is None:
                answer["unreadable"].append({"line": number, "text": line[:200],
                                             "why": why or argv_why})
                continue
            answer["executions"].append({"line": number, "pid": pid, "path": path, "argv": argv,
                                         "resolved": os.path.realpath(path)})
            continue
        if kind == OPENS:
            position, directory = positions[0]
            path, why = _path_argument(arguments, position, directory, cwd)
            flags = OPEN_FLAGS[name]
            if path is None or flags >= len(arguments):
                answer["unreadable"].append({"line": number, "text": line[:200],
                                             "why": why or "an open whose flags are not here, so"
                                                           " whether it could write is not read"})
                continue
            if "O_" not in arguments[flags]:
                # Numeric flags. Reading them as "no write flag present" would turn an unreadable
                # line into a clean one, and clean is the dangerous direction.
                answer["unreadable"].append({"line": number, "text": line[:200],
                                             "why": "an open whose flags carry no symbolic name,"
                                                    " so whether it could write is not read"})
                continue
            if not any(flag in arguments[flags] for flag in WRITE_FLAGS):
                continue
            answer["writeCalls"] += 1
            answer["writes"].append({
                "line": number, "pid": pid, "call": name,
                "path": _resolved_descriptor(result) or path,
                "reaches": reaches,
                "changedTheFile": _what_an_open_did(arguments[flags])})
            continue
        answer["writeCalls"] += 1
        for position, directory in positions:
            path, why = _path_argument(arguments, position, directory, cwd)
            if path is None:
                answer["unreadable"].append({"line": number, "text": line[:200], "why": why})
                continue
            # A call in this table changes every path position the table declares for it, which
            # is why the link source is no longer one of them.
            answer["writes"].append({"line": number, "pid": pid, "call": name, "path": path,
                                     "reaches": reaches,
                                     "changedTheFile": _what_a_path_call_did(name)})
    for pid, name in sorted(unfinished):
        answer["unreadable"].append({"line": None, "text": name,
                                     "why": "a call interrupted in process " + str(pid) + " and"
                                            " never resumed before the trace ended"})
    return answer


def trace_argv(tracer, log):
    """The tracer invocation. Its syscall filter is built from the table that parses it.

    One list rather than two, so the filter and the parser cannot drift apart: a call added to the
    table is asked for, and a call the tracer reports that the table does not know is an unreadable
    line rather than a silence.
    """
    return [str(tracer), "-f", "-y", "-qqq", "-s", str(TRACE_STRING_LIMIT), "--kill-on-exit",
            "-e", "trace=" + ",".join(sorted(TRACED_CALLS)), "-o", str(log), "--"]


def tracer_probe(root):
    """Whether this host's process boundary can be witnessed, answered by witnessing one.

    A detector that has never been seen to detect is the shape this file refuses everywhere else:
    a cell nobody has watched move the other way. So the probe starts a process the way a firing is
    started, has it write one file, and requires that write to come back out of the parser. A
    tracer that attaches and reports nothing is reported unusable rather than trusted.

    The field is usable rather than passed or met, because the judgment walk collects those two
    names wherever they sit and whether a host carries a tracer is a fact about the host, not a
    verdict about the hook.
    """
    where = Path(root) / "witness"
    where.mkdir(parents=True, exist_ok=True)
    mark, log = where / "probe-mark", where / "probe.strace"
    answer = {"tracer": None, "wrote": [str(where), str(mark), str(log)],
              "what": "one process started the way a firing is started, made to write one file,"
                      " and required to have that write read back out of the trace"}
    found = shutil.which(TRACER)
    if not found:
        return dict(answer, usable=False,
                    because="there is no " + TRACER + " on this host, so the process boundary"
                            " cannot be witnessed here")
    answer["tracer"] = found
    argv = trace_argv(found, log) + [
        sys.executable, "-c",
        "import pathlib, sys; pathlib.Path(" + repr(str(mark)) + ").write_text('the write this"
        " probe exists to be seen making'); sys.exit(3)"]
    answer["argv"] = list(argv)
    try:
        done = subprocess.run(argv, capture_output=True, text=True, timeout=300)
    except subprocess.TimeoutExpired:
        return dict(answer, usable=False,
                    because="the tracer did not finish its own probe within its timeout")
    except OSError as error:
        return dict(answer, usable=False,
                    because="the tracer could not be started: " + str(error))
    if done.returncode != 3:
        return dict(answer, usable=False,
                    because="the tracer did not relay the probe's exit status: it answered "
                            + str(done.returncode) + " where 3 was asked for, saying "
                            + (done.stderr.strip()[:200] or "nothing"))
    try:
        recorded = log.read_text(encoding="utf-8", errors="replace")
    except (OSError, ValueError) as error:
        return dict(answer, usable=False,
                    because="the trace the probe wrote could not be read: " + str(error))
    seen = parse_trace(recorded, os.getcwd())
    answer["read"] = {"lines": seen["lines"], "writeCalls": seen["writeCalls"],
                      "unreadable": len(seen["unreadable"])}
    if seen["unreadable"]:
        return dict(answer, usable=False,
                    because="the probe's own trace carries " + str(len(seen["unreadable"]))
                            + " lines this parser could not read, the first being: "
                            + str(seen["unreadable"][0]["why"]))
    started = [one for one in seen["executions"] if one["path"] == sys.executable]
    if len(started) != 1:
        return dict(answer, usable=False,
                    because="the probe's trace names " + str(len(started)) + " starts of "
                            + sys.executable + " where exactly one was made")
    if not any(os.path.realpath(one["path"]) == os.path.realpath(str(mark))
               for one in seen["writes"]):
        return dict(answer, usable=False,
                    because="the probe wrote a file this trace does not carry, so the tracer"
                            " reports starts here without reporting writes")
    return dict(answer, usable=True, because=None)

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
        return {"source": JOURNAL, "detail": unidentifiable,
                "adapterOutcome": Unreadable(unidentifiable)}
    if record is None:
        return {"source": JOURNAL, "absent": NO_REGISTRATION}
    payload = dict(record)
    payload["source"] = JOURNAL
    return payload


def _faulted(source, fired, keys):
    """A firing that never completed. Its readings could not be taken, and say so."""
    payload = {"source": source, "detail": fired["faulted"]}
    for key in keys:
        payload[key] = Unreadable(fired["faulted"], state=fired["faultedState"])
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

    WHICH kind of failure is asked of reading.observe rather than decided here, which is the same
    rule stdout_payload follows about the adapter's own validator: a second copy of a partition
    agrees with the original only until one of them changes, and this one already disagreed. A
    symlink that loops is a link that exists whose shape cannot be read, and this file called it a
    question that could not be asked. A directory sitting where a marker file belongs was worse:
    it read as nothing having been published.
    """
    settled = reading.observe(path, "whether a marker file is there")
    if settled is None:
        return present
    if settled.state == reading.ABSENT:
        return missing
    return Unreadable(settled.detail, state=settled.state)


def harness_payload(fired):
    if fired is None:
        return {"source": HARNESS, "absent": NO_REGISTRATION}
    if fired.get("faulted"):
        return _faulted(HARNESS, fired, ("wallMs", "exitCode"))
    return {"source": HARNESS, "wallMs": fired["wallMs"], "exitCode": fired["exitCode"]}



def witness_payload(arm, fired):
    """What the tracer recorded about this firing, answered in the vocabulary each cell asks in.

    Three of these are comparisons against paths fixed before the run and one is a comparison
    between two sources, and none of them is a resolution taken afterwards. A path resolved after
    the run answers about the filesystem as it is then: a symlink standing where the entry point
    belongs, repointed at the real file by the program it started, resolves to exactly the right
    answer, which is the substitution rather than the absence of one. The resolved forms are kept
    beside the readings as context and decide nothing.
    """
    if fired is None:
        return {"source": WITNESS, "absent": NO_REGISTRATION}
    if fired.get("faulted"):
        return _faulted(WITNESS, fired, WITNESS_CELLS)
    tracer = arm.tracer or {}
    if not tracer.get("usable"):
        # A host that cannot witness its process boundary. Not an absence: there is a process and
        # it started something, and nobody here could see which.
        why = tracer.get("because") or "no tracer was handed to this arm"
        payload = {"source": WITNESS, "detail": why}
        for cell in WITNESS_CELLS:
            payload[cell] = Unreadable(why, state=reading.ACCESS_ERROR)
        return payload
    seen = fired.get("trace")
    why = fired.get("traceDetail")
    if isinstance(seen, dict) and not why:
        if seen["unreadable"]:
            why = ("the trace of this firing carries " + str(len(seen["unreadable"])) + " lines"
                   " this parser could not read, the first being: "
                   + str(seen["unreadable"][0]["why"]))
        elif fired.get("tracerSaid"):
            why = "the tracer reported a problem of its own: " + str(fired["tracerSaid"])[:200]
        elif not seen["executions"]:
            why = "the trace of this firing names no start, so what ran is not established"
    if why or not isinstance(seen, dict):
        payload = {"source": WITNESS, "detail": why or "this firing left no trace to read"}
        for cell in WITNESS_CELLS:
            payload[cell] = Unreadable(payload["detail"])
        return payload
    first = seen["executions"][0]
    argv = first["argv"]
    # Every later start is unexpected unless it is a descendant running this run's own relay
    # launcher. The root process re-executing is unexpected even at the same image: a program that
    # starts the expected interpreter and then becomes something else satisfies a reading taken
    # only of the first line.
    unexpected = [one["path"] for one in seen["executions"][1:]
                  if one["pid"] == seen["rootPid"] or one["path"] != str(arm.launcher)]
    outside, unresolved, strayed = [], [], []
    for one in seen["writes"]:
        answered = owned(one["path"], arm.run_root, one["reaches"])
        if not_read(answered):
            unresolved.append(answered.why)
        elif answered is False:
            outside.append(one["path"])
            # Published entry by entry, not as a count. A reader looking at a path this run
            # reports writing needs to know which call put it there and whether that call
            # changed the file or only asked to be able to.
            strayed.append({"path": one["path"], "call": one["call"], "line": one["line"],
                            "changedTheFile": one["changedTheFile"]})
    payload = {
        "source": WITNESS,
        "startedExecutable": first["path"],
        "startedCommand": (COMMAND_AS_REGISTERED if argv == list(fired["argv"])
                           else COMMAND_DIFFERS),
        "startedEntryPoint": argv[1] if len(argv) > 1 else ENTRY_POINT_NOT_IN_ARGV,
        "unexpectedExecutions": sorted(set(unexpected)),
        "writesOutsideRoot": sorted(set(outside)),
        "witnessed": {
            "rootPid": seen["rootPid"], "traceLines": seen["lines"],
            "writeCalls": seen["writeCalls"], "writesSeen": len(seen["writes"]),
            "writesInsideRoot": len(seen["writes"]) - len(outside) - len(unresolved),
            # How many of those changed a file outright, as against how many only opened one
            # able to change it. Both count as writes here, and a reader can tell them apart.
            "changedAFile": len([one for one in seen["writes"]
                                 if one["changedTheFile"] == CHANGED]),
            "mayHaveChangedAFile": len([one for one in seen["writes"]
                                        if one["changedTheFile"] == MAY_HAVE_CHANGED]),
            "writesOutside": strayed,
            "failedAttempts": seen["failedAttempts"], "restarted": seen["restarted"],
            "tracePath": fired.get("tracePath"), "tracerArgv": fired.get("tracerArgv"),
            "tracerSaid": fired.get("tracerSaid") or "",
            "registeredArgv": list(fired["argv"]),
            "executions": [dict(one, sameProcessAsTheStart=one["pid"] == seen["rootPid"])
                           for one in seen["executions"]],
        },
    }
    if unresolved:
        # A path nobody could resolve is not a path inside the root, and it is not one outside it
        # either. The reading that rests on it is the one that cannot be taken.
        payload["writesOutsideRoot"] = Unreadable(unresolved[0])
        payload["detail"] = unresolved[0]
    return payload


# ------------------------------------------------------------------ one row


def row(arm, declared, built, fired, record, expected, unidentifiable=None):
    """Every firing cell for one arm, one scenario and one firing, each by its own reading."""
    payloads = {
        HOOK_FILE: arm.hook_file,
        JOURNAL: journal_payload(record, unidentifiable),
        STDOUT: stdout_payload(fired),
        MARKER_ROOT: marker_payload(arm, built, fired, (record or {}).get("guardRecordedAs")),
        HARNESS: harness_payload(fired),
        WITNESS: witness_payload(arm, fired),
    }
    cells = {cell: read(cell, payloads) for cell in FIRING_CELLS}
    answer = {"cells": cells, "expected": dict(expected),
              # Beside the cells rather than in them: what the tracer saw is evidence a reader
              # needs to explain a disagreement, and none of it is a reading anything judges.
              "witness": payloads[WITNESS].get("witnessed"),
              "provenance": {"codexHome": str(arm.codex_home),
                             "argv": (fired or {}).get("argv"),
                             "tracer": (fired or {}).get("tracerArgv"),
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
            if not _answered(found["value"], reading.ABSENT):
                disagreed.append({"cell": cell, "wanted": reading.ABSENT,
                                  "found": found["value"]})
            elif found.get("detail") != NO_REGISTRATION:
                disagreed.append({"cell": cell, "wanted": NO_REGISTRATION,
                                  "found": found.get("detail")})
        return {"passed": not disagreed, "disagreed": disagreed,
                "because": "the off arm registered nothing, so nothing ran"}

    if not _answered(cells["adapterOutcome"]["value"], "guard_answered"):
        return {"passed": False, "unmeasured": True, "disagreed": [
            {"cell": "adapterOutcome", "wanted": "guard_answered",
             "found": cells["adapterOutcome"]["value"]}],
            "because": "the adapter did not receive a verdict, so this scenario was not measured;"
                       " it does not mean no omission was present"}
    for cell, wanted in expected.items():
        found = cells[cell]["value"]
        if not_read(found):
            # Before any predicate sees it. recordedAs expects "a path was named", and while the
            # answer was a non-empty string a reading that failed satisfied the one question the
            # row asks there. The type answers that question correctly on its own now, and this
            # stays because a wrong answer and a reading nobody took are different findings.
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
                  and (not_read(published) or not published))
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
    return (_answered(cells["adapterOutcome"]["value"], "guard_answered")
            and _answered(cells["observation"]["value"], observation))


def _detection(scenarios, observation, injected_names):
    injected = _firings(scenarios, ON, injected_names)
    reported = [one for one in injected if _reported(one[2], observation)]
    # "It did not report this" and "nobody could read whether it reported this" are different
    # answers, and counting them together would let an unreadable journal read as a detector that
    # stayed silent. Neither concludes the criterion met; only one of them is about the hook.
    unreadable = [declared["name"] + "#" + str(index) for declared, index, fired in injected
                  if not_read(fired["cells"]["observation"]["value"])
                  or not_read(fired["cells"]["adapterOutcome"]["value"])]
    return {"answer": MEASURED, "injected": len(injected), "reported": len(reported),
            "unreadable": unreadable, "arms": [ON],
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
                if _answered(fired["cells"]["heldFile"]["value"], RESERVED)]
    printed = [declared["name"] for declared, _i, fired in guarded
               if _answered(fired["cells"]["printedBlock"]["value"], PRINTED_A_BLOCK)]
    # A reading that could not be taken is not evidence that nothing was held. Dropping it from
    # the list made the criterion read as met on the strength of a reservation nobody could look
    # at, which is the one substitution this whole arrangement refuses.
    unreadable = [declared["name"] + "/" + cell for declared, _i, fired in guarded
                  for cell in ("heldFile", "printedBlock")
                  if not_read(fired["cells"][cell]["value"])
                  or fired["cells"][cell]["value"] not in
                  (RESERVED, NOT_RESERVED, PRINTED_A_BLOCK, PRINTED_NOTHING)]
    answers["wrongBlock"] = {
        "answer": MEASURED,
        "criterion": "zero holds on unmarked sessions, on blocked_needs_input and on interrupted",
        "watched": list(watched), "reserved": reserved, "printedABlock": printed,
        "unreadable": unreadable, "arms": [ON],
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
        "firings": len(timings), "timingsNotTaken": missing, "arms": [ON],
        "met": judged(bool(latencies) and median <= LATENCY_MEDIAN_MS
                      and p95 <= LATENCY_P95_MS, missing),
        "tracedByTheWitness": bool((scenarios.get("_tracer") or {}).get("usable")),
        "narrowing": "the interval measured is the hook process alone, started by this harness"
                     " and, where a tracer could be established, started UNDER it. A tracer adds"
                     " and never subtracts, so a bound kept here is kept without one. The"
                     " contract's budget is per Stop as the host sees it, so meeting it here is"
                     " necessary and not sufficient.",
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
    foreign_values = dict((arm, scenarios["_arms"][arm]["foreignRegistration"]["value"])
                          for arm in ARMS)
    foreign_not_taken = unreadable_among(list(foreign_values.values()))
    return {
        "oneReservationPerTurn": {
            "what": "one turn, the registered command fired twice",
            "reservations": reservations,
            "observationsPublished": published,
                "notTaken": len(not_taken), "arms": [ON],
                # The claim is about the reservation, so the reservation is what is checked: the
                # create-once file is there after both firings, and the two firings published two
                # distinct observations rather than one record read twice. Counting only the
                # observations would have reported success for a turn that reserved nothing at all.
                #
                # Passed as a callable, because every comparison below reads a cell: the list
                # equality, the truthiness and the set all refuse an unread reading rather than
                # answering about it, so the guard has to run before they do.
                "met": judged(lambda: len(firings) == 2
                              and reservations == [RESERVED, RESERVED]
                              and len([one for one in published if one]) == 2
                              and len(set(one for one in published if one)) == 2, not_taken),
            "isNot": "the duplicate execution measure. It is the hold leg of it and none of the"
                     " rest, because nothing here verifies, corrects, or restarts a daemon.",
        },
        "foreignRegistrationSurvives": {
            "what": "a Stop entry belonging to another owner was in both hook files first",
            "arms": list(ARMS),
            "values": foreign_values,
            "notTaken": len(foreign_not_taken),
            "met": judged(lambda: across(ARMS, foreign_values,
                                        lambda value: value == FOREIGN_PRESENT),
                          foreign_not_taken),
            "isNot": "evidence that the two hooks interact at run time. The foreign command is"
                     " never executed here; this reads the file, not a decision.",
        },
    }


def process_witness(scenarios, tracer):
    """Whether every firing was started by the command the registration names and wrote only inside.

    One judgment over every on-arm firing rather than a verdict inside each row. What is expected
    is the same for every scenario - which interpreter, which program, no re-execution, no write
    outside - so it is a property of the run in the way foreignRegistrationSurvives is, and not
    something a scenario declares beside its observation and its decision. A row still answers
    what it always answered: whether that scenario reached the state it declared. Under a
    substitution it did, and making the row say otherwise would be the adjudication this file
    refuses everywhere else; the finding is read from judgmentsThatFailed instead.

    Where no tracer could be established this answers not performed and carries no verdict at all,
    beside the two the contract itself cannot reach. The readings still say a reading was not
    taken and notPerformed still names the reason, so nothing here ever reads as nothing having
    been written. What it costs is that such a host does not answer this question, which is what
    --require-witness exists for.
    """
    firings = _firings(scenarios, ON)
    if not (tracer or {}).get("usable"):
        return {"answer": NOT_PERFORMED, "arms": [ON], "tracer": tracer,
                "criterion": WITNESS_CRITERION,
                "because": ((tracer or {}).get("because")
                            or "no tracer was probed for this run"),
                "doesNotCover": list(WITNESS_DOES_NOT_COVER)}
    disagreed, unreadable, silent = [], [], []
    for declared, index, fired in firings:
        where = declared["name"] + "#" + str(index)
        for cell in WITNESS_CELLS:
            found = fired["cells"][cell]["value"]
            if not_read(found):
                unreadable.append(where + "/" + cell)
            elif not _answered(found, EXPECTED_WITNESS[cell]):
                disagreed.append({"scenario": declared["name"], "firing": index, "cell": cell,
                                  "wanted": EXPECTED_WITNESS[cell], "found": found})
        seen = (fired.get("witness") or {}).get("writesInsideRoot")
        if not isinstance(seen, int) or seen < 1:
            silent.append(where)
    return {
        "answer": MEASURED, "arms": [ON], "tracer": tracer,
        "criterion": WITNESS_CRITERION,
        "expected": dict(EXPECTED_WITNESS), "firings": len(firings),
        "disagreed": disagreed, "unreadable": unreadable, "witnessedNothing": silent,
        # A parser that read nothing answers "no writes outside" exactly as convincingly as a run
        # that made none, and the probe proves only that one syntax moved once. So every on-arm
        # firing must have been SEEN writing something inside the root. Each one journals its own
        # invocation, which is such a write, so a firing this witness saw write nothing is a
        # witness that stopped seeing rather than a hook that stayed quiet.
        "met": judged(not disagreed and not silent, unreadable),
        "doesNotCover": list(WITNESS_DOES_NOT_COVER),
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
    identity = {"repositoryCommit": repository_commit(),
                "workingTree": working_tree()}
    digests = {}
    for name, target in (("harness", Path(__file__).resolve()),
                         ("installer", RUNTIME),
                         ("entryPoint", ROOT / "scripts" / completion.ENTRY_POINT_NAME),
                         ("runtimeModules", ROOT / "scripts" / "crw_runtime"),
                         ("relay", RELAY_SOURCE)):
        digests[name] = _digest(target)
    identity["sourceDigests"] = digests
    return identity


def working_tree():
    """Whether the checkout had edits in it, as a reading rather than as a guess.

    Three ways not to know and they are not one answer: git could not be run at all, it ran and
    did not answer in time, and it answered with a failure. Left unread in every one of them,
    because a tree nobody could look at is not a clean one - and kept apart, because only the
    first of the three is a question that could not be asked.
    """
    try:
        done = subprocess.run(["git", "status", "--porcelain"], cwd=str(ROOT),
                              capture_output=True, text=True, timeout=60)
    except subprocess.TimeoutExpired:
        return Unreadable("git status did not answer within its timeout, so whether the working"
                          " tree was clean is not established")
    except OSError:
        return Unreadable("git status could not be run, so whether the working tree was clean"
                          " is not established", state=reading.ACCESS_ERROR)
    if done.returncode != 0:
        return Unreadable("git status exited " + str(done.returncode) + ", so whether the"
                          " working tree was clean is not established")
    return "dirty" if done.stdout.strip() else "clean"


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
            return Unreadable("nothing under " + str(target) + " could be listed to digest")
        for path in paths:
            summed.update(str(path.relative_to(ROOT)).encode("utf-8"))
            summed.update(path.read_bytes())
    except OSError as error:
        return Unreadable("the bytes of " + str(target) + " could not be read: "
                          + type(error).__name__ + ": " + str(error),
                          state=reading.ACCESS_ERROR)
    return summed.hexdigest()


def repository_commit():
    try:
        done = subprocess.run(["git", "rev-parse", "HEAD"], cwd=str(ROOT), capture_output=True,
                              text=True, timeout=60)
    except subprocess.TimeoutExpired:
        # A timeout is as ordinary here as a missing git, and letting either escape would end
        # the command with no document at all over a question about provenance. They are caught
        # separately because they are different answers: this one was asked and did not answer.
        return Unreadable("git rev-parse did not answer within its timeout, so the commit is"
                          " not established")
    except OSError:
        return Unreadable("git rev-parse could not be run, so the commit is not established",
                          state=reading.ACCESS_ERROR)
    if done.returncode != 0:
        return Unreadable("git rev-parse exited " + str(done.returncode) + ", so the commit is"
                          " not established")
    return done.stdout.strip()


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


def _leads_to(path):
    """Where a path leads, asked of the filesystem, answered rather than raised.

    The one place this file puts that question as a READING, so what a resolution can fail with
    is decided once instead of at each call site. Both sides of the catch are deliberate. A
    resolution that raises past its caller ends the comparison and takes down every reading
    beside it, though the failure was about one path; and a catch stretched over the logic
    around the call would turn this file's own mistakes into the word it keeps for a reading
    nobody could take.
    """
    try:
        return Path(path).resolve()
    except RESOLUTION_FAILURES as error:
        return Unreadable("where " + str(path) + " leads could not be established: "
                          + type(error).__name__ + ": " + str(error),
                          state=reading.ACCESS_ERROR)


def owned(path, root, reaches=ENTRY):
    """Whether the thing this call wrote is inside the run's own directory: in, out, or unread.

    Decided by the path the CALL ACTUALLY WROTE TO, which is not the same path for every call.
    A symlink, a mkdir, an unlink and a rename write the entry they name and never follow its
    last component - a symlink does not touch its target, which need not even exist. An open and
    a truncate go through that last component and land on whatever it points at.

    Both halves of this were measured, in opposite directions. Resolving always reported an
    outside write for a link created INSIDE the root that pointed out, which fails a contained
    firing for a write that stayed in. Not resolving at all reported a clean run for a link
    created OUTSIDE the root that pointed in, which is an entry made where the run never named.
    Asking both at once answers both cases the same way, so it cannot tell them apart either.
    The call says which one it wrote, and the table says which the call is.

    The PARENT is resolved in both modes, and that is load-bearing rather than tidy: a write
    into a directory reached through a symlink lands where that symlink points, so a run could
    otherwise put a file outside the root through a linked directory and read as contained.
    Only the last component is treated differently, because only it is the thing some calls
    create rather than go through.

    Three answers rather than two, because a path that could not be resolved is not a path
    outside the root: reporting it as outside names the wrong repair and claims to know where it
    led.
    """
    spelled = Path(path)
    here = _leads_to(root)
    if not_read(here):
        return here
    wrote = _leads_to(spelled if reaches == THROUGH else spelled.parent)
    if not_read(wrote):
        return wrote
    if reaches != THROUGH:
        wrote = wrote / spelled.name
    try:
        wrote.relative_to(here)
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
    not_taken = sorted(place for place, answer in answers.items() if not_read(answer))
    return {"checked": len(places), "outside": outside, "notTaken": not_taken,
            "met": judged(not outside, not_taken),
            "what": "every place this run creates resolves inside the directory it made for"
                    " itself, by where the path leads rather than by how it is spelled",
            "doesNotCover": "a write a subprocess made somewhere this run never named. This"
                            " judgment is about the places this run CREATES, and where the"
                            " processes actually wrote is a different question: processWitness"
                            " asks it OF THE FIRINGS, and of nothing else this run starts -"
                            " not the installs, not the relay commands that build a scenario,"
                            " not the probe - and it also says whether it could be asked at all"
                            " on this host. What is also built here is that the constructed"
                            " environment carries no pointer out of this directory and no"
                            " bytecode is left beside the source the subprocesses import."}


def refusal(detail, at=None, defect=None):
    """An answer for a run that cannot be made, printed instead of rows nobody took.

    A refusal is the answer for two different things and they must not read alike. The relay's
    version floor and a root that could not be created are facts about the environment; a
    consumption site that used a reading which was not taken as a value is a defect in this file.
    The second carries the frame that raised, because a defect reported without a location is
    reported as a data problem and the place it came from is gone.
    """
    answer = {"source": SOURCE, "harnessVersion": HARNESS_VERSION, "refused": detail,
              "pythonVersion": ".".join(str(part) for part in sys.version_info[:3])}
    if at:
        answer["raisedAt"] = at
    if defect:
        answer["defect"] = defect
    return answer


def render(value):
    """The JSON form of anything in this document that is not a value.

    Handed to json.dumps as default= rather than applied as a walk over the places that might
    hold one. The encoder calls it wherever the object actually sits - inside a cell, inside a
    source identity, inside a list - while a walk is a list of places and can miss one. A missed
    one would be worse than a wrong answer here, because json.dump streams: it would write a
    truncated document and then raise, and one JSON object on stdout is what this command
    promises.
    """
    if not_read(value):
        return value.rendered()
    raise TypeError("this document carries something that cannot be written as JSON: "
                    + repr(value))


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
    # Equality is not enough, and since CRW-103 it is not even available. While the answer was a
    # sentinel string, a digest that could not be taken was the SAME sentinel at both ends, so an
    # unreadable source compared equal to itself and reported the bytes as identified. A reading
    # that was not taken is now a fresh object per reading, and CPython only shortcuts a mapping
    # comparison on identity, so the two ends no longer answer equal - they refuse to compare at
    # all, and the guard below is what keeps that refusal from running.
    unread = unreadable_among(digests)
    return {"before": earlier, "after": later, "digestsNotTaken": len(unread),
            "met": judged(lambda: earlier.get("sourceDigests") == later.get("sourceDigests")
                          and bool(earlier.get("sourceDigests")), unread),
            "what": "every source whose contents decide a run was readable and had the same digest"
                    " before the first subprocess and after the last",
            "doesNotCover": "the repository commit and the working tree. They are recorded"
                            " beside the digests as context and are not what identifies the"
                            " bytes: a commit names bytes only in a clean checkout, and anyone"
                            " developing this runs it in a dirty one, which is why the digests"
                            " are the claim here. Both are readings, so one that could not be"
                            " taken is visible in the document rather than missing from it,"
                            " and it does not decide this judgment."}


def compare(root, tracer=None):
    """Build both arms, drive every scenario at both, and read what each one has afterwards.

    The tracer is probed ONCE per run and handed in. Probing again here would answer a second
    time about a host that can change between the two, and the caller that asked for the first
    answer - --require-witness - would have refused on one probe while the rows were taken
    under another.
    """
    launcher = launcher_for(root)
    # Before the arms. The install is the first subprocess an arm makes, and whether this host can
    # witness a process has to be answered before anything that reading decides is started.
    tracer = tracer_probe(root) if tracer is None else tracer
    arms = {name: Arm(root, name, launcher, tracer) for name in ARMS}
    scenarios = {"_arms": {}, "_tracer": tracer,
                 "_wrote": [launcher] + [Path(place) for place in tracer.get("wrote") or []]
                 + [place for arm in arms.values() for place in arm.places]}
    for name, arm in arms.items():
        cells = {cell: read(cell, {INSTALL: arm.install, HOOK_FILE: arm.hook_file})
                 for cell in ARM_CELLS}
        wanted = ARM_INSTALL[name]
        disagreed = [{"cell": cell, "wanted": value, "found": cells[cell]["value"]}
                     for cell, value in wanted.items()
                     if not _answered(cells[cell]["value"], value)]
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
    witness = process_witness(scenarios, scenarios.get("_tracer"))
    # A copy per run rather than a module constant edited in place: the stand-in the witness closes
    # is closed on a run that made it and open on a run that could not, and a mutated global would
    # carry one run's answer into the next document in the same interpreter.
    witnessed = witness.get("answer") == MEASURED
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
        "processWitness": witness,
        "absenceIsNormalAt": [dict(place, arm=arm, scenario=name, cell=cell)
                              for (arm, name, cell), place in sorted(places.items())],
        "measures": answers,
        "supplemental": supplemental(scenarios),
        "standIns": dict(STAND_INS) if witnessed else dict(STAND_INS, **STAND_IN_WITHOUT_WITNESS),
        "notPerformed": dict(NOT_PERFORMED_HERE) if witnessed else dict(
            NOT_PERFORMED_HERE, **{"the process witness": witness.get("because")}),
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
    parser.add_argument("--require-witness", action="store_true",
                        help="refuse rather than report the process witness as not performed")
    args = parser.parse_args(argv)

    if sys.version_info < RELAY_PYTHON:
        # The relay declares this floor and the command under test imports it. Below it there is no
        # run to report on, and rows that were never taken must not be printed as though they were.
        _print(refusal(
            "the relay requires Python " + ".".join(str(p) for p in RELAY_PYTHON) + " or newer,"
            " so no arm can be driven on this interpreter and no row was taken"))
        return 2

    keep = args.root is not None
    try:
        root = own_directory(args.root) if keep else Path(
            tempfile.mkdtemp(prefix="hook-comparison-")).resolve()
    except RESOLUTION_FAILURES as error:
        _print(refusal("a directory for this run could not be created: "
                       + type(error).__name__ + ": " + str(error)))
        return 2
    answer = None
    try:
        earlier = source_identity()
        # Probed once, here, and handed to the run. Asking twice would let the answer that
        # decides the refusal differ from the answer the rows were taken under, so a run could
        # pass --require-witness on one probe and witness nothing on another.
        witnessing = tracer_probe(root)
        if args.require_witness and not witnessing.get("usable"):
            answer = refusal("the process boundary cannot be witnessed here and --require-witness"
                             " was given: " + str(witnessing.get("because")))
        else:
            answer = document(compare(root, witnessing), root, earlier)
    except RelayError as error:
        answer = refusal("a relay command a scenario needed refused: " + str(error))
    except NotAValue as error:
        # The one failure this file is about, reported as what it is. A consumption site used a
        # reading that was not taken as though it were a value; the type refused, and the refusal
        # names the site rather than leaving the run to be read as a data problem.
        answer = refusal("a consumption site used a reading that was not taken as a value: "
                         + str(error)[:400], at=getattr(error, "at", None),
                         defect="this is a defect in this harness, not a problem with what it"
                                " read. The reading that was not taken is a distinct type, and"
                                " the site that consumed it asked it a question only a value can"
                                " answer.")
    except BaseException as error:
        # Named rather than allowed to escape, which is the rule guard.evaluate states for itself:
        # a detector that dies detects nothing and leaves no trace it ran, and that is worse than
        # a wrong answer. Catching only the relay's own error left every other way a run can fail
        # - a filesystem error after the root exists, an interrupted run - ending the command with
        # a traceback in place of the one document it promises. The type and message are carried
        # so a defect here is reported as a defect rather than as a data problem.
        answer = refusal("the comparison did not finish: " + type(error).__name__ + ": "
                         + str(error)[:400], at=reading.where(error))
    finally:
        if not keep:
            shutil.rmtree(root, ignore_errors=True)
    # The answer this reports on is the one that was actually WRITTEN, not the one handed over.
    # A result that could not be written becomes a refusal inside _print, and taking the status
    # from the original would have printed a refusal and exited zero - a run whose own output
    # says it was refused, accepted by anything reading the status.
    answer = _print(answer)
    if answer.get("refused"):
        return 2
    return 0 if answer["passed"] else 1


def _print(answer):
    """The one write to stdout, composed in full before any of it is written.

    json.dump streams to the file object, so anything it cannot encode leaves a truncated
    document behind and then raises. Composing the whole string first means an encoding failure
    happens before a single byte is written, and one JSON object on stdout stays true.

    Composing first is only half of it. This runs outside main()'s own handler, so an encoding
    failure here would still end the command with a traceback and NOTHING on stdout - which is
    the same promise broken at the other end. A document that cannot be written is itself a
    result, so it is reported as a refusal naming what could not be written and where.
    """
    try:
        written = json.dumps(answer, default=render, indent=2, sort_keys=True)
    except (TypeError, ValueError) as error:
        answer = refusal("the result document could not be written: "
                         + type(error).__name__ + ": " + str(error)[:400],
                         at=reading.where(error))
        written = json.dumps(answer, indent=2, sort_keys=True)
    sys.stdout.write(written + "\n")
    return answer


if __name__ == "__main__":
    raise SystemExit(main())
