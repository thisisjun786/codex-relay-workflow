#!/usr/bin/env python3
"""What the off/on comparison has to be true of, checked against a run rather than against itself.

The harness reports that every row agreed with what it declared. That sentence is the thing most
worth distrusting, because the arrangement has one silent failure that produces it: point the run
at a marker root nobody built and every scenario reads unmanaged, releases, holds nothing, finishes
quickly and looks calm. So this module asserts the states themselves, and it writes them out here
rather than importing the harness's own declarations. A harness that quietly changed what it
expected would otherwise change this check with it.

The inventories below are literals for the same reason. Counting the arms, scenarios, cells and
measures from the module under test turns "ten scenarios ran" into "however many ran, ran", and a
harness that lost one would lose the assertion about it in the same edit.

One run serves every case here. It is the expensive part, it is deterministic, and running it per
case would buy nothing but minutes.
"""

import ast
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest
from unittest import mock

ROOT = Path(__file__).resolve().parents[3]
sys.path.insert(0, str(ROOT / "scripts"))

import hook_comparison as harness  # noqa: E402
from crw_runtime import completion, hooks  # noqa: E402

# The relay this harness drives requires 3.11, and CI runs this suite on 3.10 as well. The module
# imports there - nothing 3.11-only is imported, because every relay call is a subprocess - and on
# that interpreter the harness refuses and this module checks the refusal.
HAVE_RELAY = sys.version_info >= (3, 11)


def state_of(value):
    """Which of the four answers a reading gives, asked without comparing a value to a non-value.

    Written against the shape rather than against a class so the cases that use it read the same
    before and after the representation changed: where the answer was one sentinel string for
    both of them, this returns that one string and the case fails on the collapse itself.
    """
    return getattr(value, "state", value)

ARMS = ("off", "on")

SCENARIOS = ("receipt_missing", "managed_unregistered", "undeclared_turn_end",
             "declared_ready_receipted", "declared_in_progress", "declared_blocked_needs_input",
             "declared_interrupted", "unmanaged", "cxc_concurrent", "duplicate")

ARM_CELLS = ("installExit", "installResult", "installSettings", "registration",
             "foreignRegistration")

FIRING_CELLS = ("firedCommand", "adapterOutcome", "observation", "guardDecision", "guardState",
                "printedBlock", "recordedAs", "observationFile", "heldFile", "journalElapsedMs",
                "processWallMs", "processExit")

# Every answer that means there is nothing there, written here. The harness declares its own and
# this check requires the two to be the same set: importing the harness's would have made every
# absence comparison below agree with whatever the harness currently calls an absence.
ABSENCE_ANSWERS = ("ABSENT", "not_published", "not_reserved", None)

# Where an absence is the correct answer, written here as places rather than as a vocabulary. The
# harness derives its own set and the run reports it, but a declaration and a cell that moved
# together would agree with each other; this table does not move with them. The off arm is every
# firing cell of every scenario and is expanded below rather than typed out ten times.
ABSENCE_PLACES = {
    ("on", "unmanaged", "recordedAs"): None,
    ("on", "unmanaged", "observationFile"): "not_published",
    ("on", "unmanaged", "heldFile"): "not_reserved",
    ("on", "cxc_concurrent", "heldFile"): "not_reserved",
    ("on", "declared_ready_receipted", "heldFile"): "not_reserved",
    ("on", "declared_in_progress", "heldFile"): "not_reserved",
    ("on", "declared_blocked_needs_input", "heldFile"): "not_reserved",
    ("on", "declared_interrupted", "heldFile"): "not_reserved",
}

# Which source answers each cell and down which path, written here for the same reason: a row
# whose cells were assembled by something other than the declared reading would have to reproduce
# this exactly, and at that point it is a reading.
CELL_READINGS = {
    "installExit": ("install", ["exitCode"]),
    "installResult": ("install", ["result", "outcome"]),
    "installSettings": ("install", ["settings", "outcome"]),
    "registration": ("hook-file", ["entries"]),
    "foreignRegistration": ("hook-file", ["foreign"]),
    "firedCommand": ("hook-file", ["command"]),
    "adapterOutcome": ("journal", ["adapterOutcome"]),
    "observation": ("journal", ["observation"]),
    "guardDecision": ("journal", ["guardDecision"]),
    "guardState": ("journal", ["guardState"]),
    "printedBlock": ("stdout", ["printed"]),
    "recordedAs": ("journal", ["guardRecordedAs"]),
    "observationFile": ("marker-root", ["observationFile"]),
    "heldFile": ("marker-root", ["heldFile"]),
    "journalElapsedMs": ("journal", ["elapsedMs"]),
    "processWallMs": ("harness", ["wallMs"]),
    "processExit": ("harness", ["exitCode"]),
}

MEASURES = ("missedDetectionStateAndReceiptAbsent", "missedDetectionReceiptAbsentOnly",
            "missedDetectionMarkerWithoutRegistration", "handoffSuccess", "wrongBlock",
            "duplicateExecution", "addedLatency")

# The two the contract defines in terms of a parent verification and of work this arrangement does
# not do. Required to be reported not performed: a harness that started answering them would be
# answering a question it cannot reach.
NOT_PERFORMED = ("handoffSuccess", "duplicateExecution")

# Ten scenarios, eleven firings: the duplicate scenario fires twice.
FIRINGS = {name: 1 for name in SCENARIOS}
FIRINGS["duplicate"] = 2

# What each firing at the ON arm must have reached, written here and not imported. observation is
# what the turn was, decision is block or release, state is the decision state, and the last two
# are what the host would have seen and what was reserved.
DECLARED = {
    ("receipt_missing", 0): ("receipt_missing", "block", "receipt_missing",
                             "printed_a_block", "reserved"),
    ("managed_unregistered", 0): ("managed_unregistered", "block", "managed_unregistered",
                                  "printed_a_block", "reserved"),
    ("undeclared_turn_end", 0): ("undeclared_turn_end", "block", "undeclared_turn_end",
                                 "printed_a_block", "reserved"),
    ("declared_ready_receipted", 0): ("declared_ready_receipted", "release",
                                      "declared_ready_receipted", "printed_nothing",
                                      "not_reserved"),
    ("declared_in_progress", 0): ("declared_in_progress", "release", "declared_in_progress",
                                  "printed_nothing", "not_reserved"),
    ("declared_blocked_needs_input", 0): ("declared_blocked_needs_input", "release",
                                          "declared_blocked_needs_input", "printed_nothing",
                                          "not_reserved"),
    ("declared_interrupted", 0): ("declared_interrupted", "release", "declared_interrupted",
                                  "printed_nothing", "not_reserved"),
    ("unmanaged", 0): ("unmanaged", "release", "unmanaged", "printed_nothing", "not_reserved"),
    ("cxc_concurrent", 0): ("undeclared_turn_end", "release", "hold_in_flight",
                            "printed_nothing", "not_reserved"),
    ("duplicate", 0): ("undeclared_turn_end", "block", "undeclared_turn_end",
                       "printed_a_block", "reserved"),
    ("duplicate", 1): ("undeclared_turn_end", "release", "hold_in_flight",
                       "printed_nothing", "reserved"),
}

# The scenarios where the contract requires no hold: an unmarked session, a turn waiting on a
# person, and a turn the person stopped.
NO_HOLD = ("unmanaged", "declared_blocked_needs_input", "declared_interrupted")

_RUN = {}


def run_once():
    """The one comparison run these cases read, kept so the suite pays for it once."""
    if "answer" not in _RUN:
        root = Path(tempfile.mkdtemp(prefix="hook-comparison-check-"))
        # Placed before the run so the case above can show the run left it alone.
        (root / "somebody-elses-file").write_text("not the harness's to touch", encoding="utf-8")
        done = subprocess.run(
            [sys.executable, str(ROOT / "scripts" / "hook_comparison.py"), "--root", str(root)],
            capture_output=True, text=True, timeout=900)
        _RUN["root"] = root
        _RUN["done"] = done
        _RUN["answer"] = json.loads(done.stdout) if done.stdout.strip() else None
    return _RUN["answer"]


def tearDownModule():
    root = _RUN.get("root")
    if root is not None:
        import shutil
        shutil.rmtree(str(root), ignore_errors=True)


def run_root():
    """The directory the run made for itself, found rather than taken from what it reported.

    The check creates the parent and the run creates exactly one child inside it, so the child is
    a filesystem fact. Reading the path out of the document would have let a run that wrote
    somewhere else send every disk witness to the place that agrees with it.
    """
    children = sorted(path for path in _RUN["root"].iterdir() if path.is_dir())
    assert len(children) == 1, "the run made " + str(len(children)) + " directories, not one"
    return children[0]


def firing(answer, scenario, arm, index):
    return answer["scenarios"][scenario][arm]["firings"][index]


@unittest.skipUnless(HAVE_RELAY, "the relay requires Python 3.11, and the refusal case below is"
                                 " what this module checks on the older interpreter")
class ComparisonRunTests(unittest.TestCase):
    """The run happened, it produced one document, and the document is the shape claimed."""

    def test_the_run_completed_and_printed_one_document_carrying_its_own_stamp(self):
        answer = run_once()
        done = _RUN["done"]
        self.assertEqual(done.returncode, 0,
                         "the comparison did not complete: " + done.stderr[-2000:])
        self.assertIsNotNone(answer, "nothing was printed, so there is nothing to check")
        self.assertEqual(answer.get("source"), "hook-comparison",
                         "a document without the stamp is not this command's answer")
        self.assertIsNone(answer.get("refused"), "the run refused rather than comparing anything")
        self.assertTrue(answer.get("wroteOnlyInsideItsRoot", {}).get("met"),
                        "the run wrote outside the directory it created for itself: "
                        + json.dumps(answer.get("wroteOnlyInsideItsRoot")))
        self.assertTrue(answer.get("passed"),
                        "the run did not pass: rows "
                        + json.dumps(answer.get("rowsThatDisagreed")) + ", arms "
                        + json.dumps(answer.get("armsThatDisagreed")) + ", measures "
                        + json.dumps(answer.get("measuresThatMissedTheirBound")))
        self.assertEqual(answer.get("measuresThatMissedTheirBound"), [],
                         "a criterion this command exists to judge was not met")
        self.assertEqual(answer.get("judgmentsThatFailed"), [],
                         "a judgment in the document said false while the run reported passing")

    def test_the_inventories_are_the_ones_this_check_names(self):
        """Counted against literals, so a scenario that vanished takes no assertion with it."""
        answer = run_once()
        self.assertEqual(sorted(answer["arms"]), sorted(ARMS))
        self.assertEqual(sorted(answer["scenarios"]), sorted(SCENARIOS))
        self.assertEqual(sorted(answer["measures"]), sorted(MEASURES))
        for arm in ARMS:
            self.assertEqual(sorted(cell for cell in answer["arms"][arm] if cell in ARM_CELLS),
                             sorted(ARM_CELLS), "arm " + arm + " is missing a declared reading")
        for scenario in SCENARIOS:
            for arm in ARMS:
                firings = answer["scenarios"][scenario][arm]["firings"]
                self.assertEqual(len(firings), FIRINGS[scenario],
                                 scenario + "/" + arm + " fired an unexpected number of times")
                for one in firings:
                    self.assertEqual(sorted(one["cells"]), sorted(FIRING_CELLS),
                                     scenario + "/" + arm + " is missing a declared reading")


    def test_every_firing_reached_the_state_this_check_declares_for_it(self):
        """The states, asserted against this module's own table rather than the harness's.

        This is the case that catches the silent failure worth caring about. A run pointed at a
        marker root nobody built answers unmanaged everywhere, releases everything, holds nothing
        and reports itself calm, and only an assertion naming receipt_missing, managed_unregistered
        and hold_in_flight one at a time can tell that apart from a run that worked.
        """
        answer = run_once()
        for (scenario, index), declared in sorted(DECLARED.items()):
            one = firing(answer, scenario, "on", index)
            cells = one["cells"]
            where = scenario + "#" + str(index) + ": "
            self.assertEqual(cells["adapterOutcome"]["value"], "guard_answered",
                             where + "the adapter never received a verdict, so this scenario was"
                                     " not measured")
            self.assertEqual(
                (cells["observation"]["value"], cells["guardDecision"]["value"],
                 cells["guardState"]["value"], cells["printedBlock"]["value"],
                 cells["heldFile"]["value"]), declared, where + "the state disagrees")
            self.assertEqual(cells["processExit"]["value"], 0,
                             where + "the hook process exited nonzero, which the host reads as a"
                                     " failed hook run whatever it wrote")
            self.assertTrue(one["verdict"]["passed"], where + json.dumps(one["verdict"]))

    def test_a_managed_scenario_publishes_an_observation_that_is_actually_there(self):
        """What the guard said it wrote, and what is on disk, asked separately."""
        answer = run_once()
        for scenario in SCENARIOS:
            if scenario == "unmanaged":
                continue
            for index in range(FIRINGS[scenario]):
                cells = firing(answer, scenario, "on", index)["cells"]
                self.assertTrue(cells["recordedAs"]["value"],
                                scenario + " published nothing, so the guard never selected it")
                self.assertEqual(cells["observationFile"]["value"], "resolved",
                                 scenario + " named a path that is not on disk")

    def test_the_run_works_in_a_directory_of_its_own_and_leaves_the_rest_alone(self):
        """A caller names a place to work in; the run does not work in it.

        The harness writes a launcher and two Codex homes at fixed names, so using the named
        directory itself would replace whatever was already using those names. A file placed in
        the parent before the run is still there afterwards, unchanged.
        """
        run_once()
        witness = _RUN["root"] / "somebody-elses-file"
        self.assertTrue(witness.is_file(), "the file placed before the run is gone")
        self.assertEqual(witness.read_text(encoding="utf-8"), "not the harness's to touch",
                         "the run changed a file it did not create")
        self.assertNotEqual(str(run_root()), str(_RUN["root"]),
                            "the run used the directory it was given instead of one of its own")

    def test_the_off_arm_ran_nothing_and_gives_the_one_reason(self):
        """An absence with a reason, never a false, a zero, or a release."""
        answer = run_once()
        self.assertEqual(answer["arms"]["off"]["registration"]["value"], 0)
        for scenario in SCENARIOS:
            for index in range(FIRINGS[scenario]):
                one = firing(answer, scenario, "off", index)
                for cell in FIRING_CELLS:
                    found = one["cells"][cell]
                    self.assertEqual(found["value"], "ABSENT",
                                     scenario + "/" + cell + " answered something other than an"
                                                             " absence at the arm that ran nothing")
                    self.assertIn("dry run", found.get("detail") or "",
                                  scenario + "/" + cell + " is absent without saying why")
                self.assertTrue(one["verdict"]["passed"])

    def test_both_arms_installed_as_their_own_arm_declares(self):
        """The absences above mean nothing unless the install that produced them succeeded."""
        answer = run_once()
        self.assertEqual(answer["arms"]["off"]["installExit"]["value"], 0)
        self.assertEqual(answer["arms"]["off"]["installResult"]["value"], "MISSING")
        self.assertEqual(answer["arms"]["off"]["installSettings"]["value"], "config_would_create")
        self.assertEqual(answer["arms"]["on"]["installExit"]["value"], 0)
        self.assertEqual(answer["arms"]["on"]["installResult"]["value"], "CREATED")
        self.assertEqual(answer["arms"]["on"]["installSettings"]["value"], "config_created")
        self.assertEqual(answer["arms"]["off"]["registration"]["value"], 0)
        self.assertEqual(answer["arms"]["on"]["registration"]["value"], 1)
        for arm in ARMS:
            self.assertEqual(answer["arms"][arm]["foreignRegistration"]["value"], "present")
            self.assertTrue(answer["arms"][arm]["installed"]["passed"],
                            json.dumps(answer["arms"][arm]["installed"]))

    def test_what_ran_is_the_command_the_arms_own_hook_file_names(self):
        """Which command, which directory, read from the file rather than from the report.

        The run keeps its root, so the registration is on disk and this case reads it there with
        the product's own reader. Comparing the harness's firedCommand with the harness's argv
        would only have shown that it reported one fabricated string in two places.
        """
        answer = run_once()
        # Derived from the root this check made, so a run that reported a decoy home cannot
        # send this case to the file that agrees with it.
        home = run_root() / "on" / "codex"
        self.assertEqual(answer["arms"]["on"]["codexHome"], str(home),
                         "the run reports a Codex home other than the one under its own root")
        document = hooks.read(home / "hooks.json")
        self.assertTrue(document.usable, "the installer wrote a hook file that cannot be read")
        entries = completion.adapter_entries(document.value or {}, completion.EVENT)
        self.assertEqual(len(entries), 1, "the on arm does not carry exactly one registration")
        registered = entries[0]["command"]
        self.assertIn("completion_hook.py", registered)
        self.assertEqual(
            firing(answer, "receipt_missing", "on", 0)["cells"]["firedCommand"]["value"],
            registered, "the harness reported running something other than what is registered")
        for scenario in SCENARIOS:
            for index in range(FIRINGS[scenario]):
                provenance = firing(answer, scenario, "on", index)["provenance"]
                self.assertEqual(" ".join(provenance["argv"]), registered,
                                 scenario + " ran something other than the registered command")
                self.assertEqual(provenance["codexHome"], str(home),
                                 scenario + " ran against a different Codex home from the one the"
                                            " registration was written into")
                self.assertTrue(provenance["argv"][0].endswith("python3")
                                or "python" in provenance["argv"][0])

    def test_the_foreign_registration_is_still_there_and_was_never_executed(self):
        answer = run_once()
        for arm in ARMS:
            self.assertEqual(answer["arms"][arm]["foreignRegistration"]["value"], "present",
                             "the install displaced a Stop entry belonging to another owner")
        entries = completion.adapter_entries(
            hooks.read(run_root() / "on" / "codex" / "hooks.json").value or {},
            completion.EVENT)
        self.assertNotIn("/opt/cxc/stop", entries[0]["command"],
                         "the entry this adapter owns names the other owner's command")
        for scenario in SCENARIOS:
            for index in range(FIRINGS[scenario]):
                argv = firing(answer, scenario, "on", index)["provenance"]["argv"]
                self.assertNotIn("/opt/cxc/stop", " ".join(argv),
                                 scenario + " executed the other owner's command")

    def test_the_measures_are_the_contract_ones_and_the_two_it_cannot_reach_say_so(self):
        answer = run_once()
        for name in NOT_PERFORMED:
            measure = answer["measures"][name]
            self.assertEqual(measure["answer"], "not_performed", name + " claims an answer")
            self.assertTrue(measure.get("because"), name + " does not say why it was not performed")
        for name in ("missedDetectionStateAndReceiptAbsent", "missedDetectionReceiptAbsentOnly",
                     "missedDetectionMarkerWithoutRegistration"):
            measure = answer["measures"][name]
            self.assertEqual(measure["answer"], "measured")
            self.assertGreater(measure["injected"], 0, name + " injected nothing, so reporting"
                                                              " everything injected is vacuous")
            self.assertEqual(measure["injected"], measure["reported"])
            self.assertTrue(measure["met"])

    def test_no_hold_is_taken_where_the_contract_says_none_may_be(self):
        answer = run_once()
        measure = answer["measures"]["wrongBlock"]
        self.assertEqual(sorted(measure["watched"]), sorted(NO_HOLD))
        self.assertEqual(measure["reserved"], [])
        self.assertEqual(measure["printedABlock"], [])
        self.assertTrue(measure["met"])
        for scenario in NO_HOLD:
            cells = firing(answer, scenario, "on", 0)["cells"]
            self.assertEqual(cells["heldFile"]["value"], "not_reserved")
            self.assertEqual(cells["printedBlock"]["value"], "printed_nothing")

    def test_every_judgment_the_document_carries_reaches_the_answer(self):
        """Derived from the document, because the enumerated list was wrong three times.

        A judgment is any field named passed or met. Collecting only the kinds someone remembered
        left the supplemental observations out of the answer, so one of them could have failed
        while the command exited zero. This walks the document the way the harness does and
        requires the two to agree.
        """
        answer = run_once()
        counted = []

        def walk(payload, path=()):
            if isinstance(payload, dict):
                for key, value in sorted(payload.items()):
                    here = path + (key,)
                    if key in ("passed", "met"):
                        counted.append(("/".join(str(one) for one in here), value))
                    walk(value, here)
            elif isinstance(payload, list):
                for index, value in enumerate(payload):
                    walk(value, path + (index,))

        walk(dict((key, value) for key, value in answer.items() if key != "passed"))
        self.assertGreater(len(counted), 20, "the document carries almost no judgments, so this"
                                             " check is watching something that stopped judging")
        self.assertEqual(answer["judgmentsCounted"], len(counted),
                         "the run counted a different number of judgments from this walk")
        self.assertEqual(sorted(answer["judgmentsThatFailed"]),
                         sorted(where for where, value in counted if value is False),
                         "the run collected different failures from the ones in its own document")
        for kind in ("scenarios", "arms", "measures", "supplemental"):
            self.assertTrue(any(where.startswith(kind) for where, _v in counted),
                            kind + " carries no judgment, so nothing there can fail")

    def test_no_judgment_can_be_met_without_saying_what_it_could_not_read(self):
        """Derived over every judgment, because this was fixed one measure at a time.

        A criterion that drops an unreadable reading and concludes from what is left reports a
        bound as kept on evidence nobody has. Each fix closed one place and left the next open, so
        the property is now asked of every judgment carrying met: its answer set has to be able to
        say that a reading could not be taken, and it must not be met while it is saying so.
        """
        answer = run_once()
        judgments = []
        for name, measure in sorted(answer["measures"].items()):
            if measure.get("answer") == "measured":
                judgments.append(("measures/" + name, measure))
        for name, measure in sorted(answer["supplemental"].items()):
            judgments.append(("supplemental/" + name, measure))
        for name in ("wroteOnlyInsideItsRoot", "sourceIdentity"):
            judgments.append((name, answer[name]))
        self.assertGreaterEqual(len(judgments), 8,
                                "almost nothing carries a judgment, so this derivation is"
                                " watching a document that stopped judging")
        for where, measure in judgments:
            says = [key for key in measure
                    if key.lower().endswith("nottaken") or key == "unreadable"]
            self.assertTrue(says, where + " cannot say that a reading could not be taken, so an"
                                          " unreadable one is indistinguishable from a negative")
            for key in says:
                found = measure[key]
                count = found if isinstance(found, int) else len(found)
                if count:
                    self.assertFalse(measure.get("met"),
                                     where + " is met while " + key + " says a reading could not"
                                             " be taken")

    def test_a_measured_criterion_that_misses_its_bound_is_collected(self):
        """The overall answer covers the criteria, not only the rows.

        Keeping them apart let a run whose latency exceeded the contract's bound exit 0 because
        every row had agreed with its own table.
        """
        answer = run_once()
        measured = [name for name in MEASURES
                    if answer["measures"][name].get("answer") == "measured"]
        self.assertTrue(measured, "no criterion was measured, so the collection is vacuous")
        for name in measured:
            self.assertIn("met", answer["measures"][name], name + " reports no verdict to collect")
        self.assertIn("measuresThatMissedTheirBound", answer,
                      "the document does not collect the criteria that missed their bound")

    def test_the_latency_distribution_is_measured_against_the_budget(self):
        answer = run_once()
        measure = answer["measures"]["addedLatency"]
        distribution = measure["distributionMs"]
        self.assertEqual(distribution["count"], sum(FIRINGS.values()),
                         "a distribution over fewer firings than were fired")
        for key in ("minimum", "median", "p95", "maximum"):
            self.assertIsInstance(distribution[key], int, key + " was not measured")
        self.assertEqual(measure["budgetMs"], {"median": 2000, "p95": 5000})
        self.assertTrue(measure["met"], json.dumps(distribution))
        self.assertIn("necessary and not sufficient", measure["narrowing"])

    def test_the_supplemental_observation_is_about_the_reservation_it_names(self):
        answer = run_once()
        supplemental = answer["supplemental"]["oneReservationPerTurn"]
        self.assertEqual(supplemental["reservations"], ["reserved", "reserved"])
        self.assertEqual(len(set(supplemental["observationsPublished"])), 2,
                         "two firings of one turn published one observation between them")
        self.assertTrue(supplemental["met"])
        self.assertIn("duplicate execution measure", supplemental["isNot"],
                      "the supplemental observation does not say which measure it is not")



class ReadingTests(unittest.TestCase):
    """The accessor itself, driven with payloads made here. No relay and no run is needed.

    These are the mutations the run cannot perform on itself. A suite that only reads a successful
    run proves that the cells were filled, never that they could have refused to be.
    """

    def payloads(self):
        return {
            "journal": {"source": "journal", "adapterOutcome": "guard_answered",
                        "observation": "receipt_missing", "guardDecision": "block",
                        "guardState": "receipt_missing", "guardRecordedAs": "hook/s/t/0",
                        "elapsedMs": 7},
            "stdout": {"source": "stdout", "printed": "printed_a_block"},
            "marker-root": {"source": "marker-root", "observationFile": "resolved",
                            "heldFile": "reserved"},
            "harness": {"source": "harness", "wallMs": 11, "exitCode": 0},
            "hook-file": {"source": "hook-file", "entries": 1, "foreign": "present",
                          "command": "python3 completion_hook.py settings.json"},
        }

    def test_withholding_a_source_leaves_only_its_own_cells_unreadable(self):
        """One cell, one question: a source that did not answer moves nothing beside it."""
        every = self.payloads()
        for withheld in ("journal", "stdout", "marker-root", "harness"):
            kept = dict(every)
            kept.pop(withheld)
            for cell, source, _path, _producer in harness.CELLS:
                if source == "install":
                    continue
                found = harness.read(cell, kept)
                if source == withheld:
                    self.assertTrue(harness.not_read(found["value"]),
                                    cell + " answered although " + withheld + " did not")
                    self.assertIn(withheld, found["detail"])
                else:
                    self.assertFalse(harness.not_read(found["value"]),
                                     cell + " went unreadable because " + withheld
                                     + " was withheld, which is a question it was not asked")

    def test_a_missing_key_answers_unreadable_and_says_which_key(self):
        every = self.payloads()
        del every["journal"]["observation"]
        found = harness.read("observation", every)
        self.assertTrue(harness.not_read(found["value"]))
        self.assertIn("observation", found["detail"])
        self.assertFalse(found["readable"])
        # The neighbour reading the same payload is untouched.
        self.assertEqual(harness.read("guardDecision", every)["value"], "block")

    def test_a_declared_absence_answers_absent_rather_than_unreadable(self):
        """Absence and a failed reading are different facts and must not share an answer."""
        every = self.payloads()
        every["hook-file"] = {"source": "hook-file", "entries": 0, "foreign": "present",
                              "absentPaths": {"command": harness.NO_REGISTRATION}}
        found = harness.read("firedCommand", every)
        self.assertEqual(found["value"], harness.reading.ABSENT)
        self.assertTrue(found["readable"], "an absence is an answer, so it was readable")
        self.assertEqual(found["detail"], harness.NO_REGISTRATION)

        undeclared = self.payloads()
        del undeclared["hook-file"]["command"]
        self.assertTrue(harness.not_read(harness.read("firedCommand", undeclared)["value"]),
                        "a key missing without a declared reason is a failed reading, not an"
                        " absence")

    def test_stdout_that_the_host_would_discard_is_not_a_delivered_block(self):
        """A block with no reason is a failed hook run that continues nothing.

        The contract records that, so classifying it as a delivered block judges the row by a rule
        the host does not use, and every blocking scenario would pass on output the host throws
        away.
        """
        def printed(raw):
            return harness.stdout_payload({"stdout": raw, "exitCode": 0, "wallMs": 1})["printed"]

        self.assertEqual(printed(json.dumps({"decision": "block", "reason": "because",
                                             "continue": True})), "printed_a_block")
        self.assertEqual(printed(json.dumps({"decision": "block"})), "printed_something_else")
        self.assertEqual(printed(json.dumps({"decision": "block", "reason": "  "})),
                         "printed_something_else")
        self.assertEqual(printed(json.dumps({"decision": "block", "reason": None})),
                         "printed_something_else")
        self.assertEqual(printed(""), "printed_nothing")
        self.assertEqual(printed("not json at all"), "printed_something_else")

    def test_a_reading_that_failed_never_passes_as_a_value(self):
        """The other half of making absence expressible: nowhere may consume it as a value.

        The sentinel is a non-empty string, so it slips past exactly the tests that look like they
        exclude it - truthiness, uniqueness, and a predicate asking only whether something is
        there. recordedAs is the case that matters: the row asks whether a path was named, and an
        unreadable journal satisfied that question without naming one.
        """
        declared = None
        for one in harness.SCENARIOS:
            if one["name"] == "receipt_missing":
                declared = one
        values = {"processExit": 0, "adapterOutcome": "guard_answered",
                  "observation": "receipt_missing", "guardDecision": "block",
                  "guardState": "receipt_missing", "printedBlock": "printed_a_block",
                  "heldFile": "reserved", "recordedAs": unread("the firing wrote no journal"
                                                              " record for this turn"),
                  "observationFile": "resolved", "firedCommand": "python3 x",
                  "journalElapsedMs": 5, "processWallMs": 9}
        cells = dict((cell, {"cell": cell, "value": value, "readable": True})
                     for cell, value in values.items())
        arm = type("Arm", (object,), {"name": "on"})()
        verdict = harness.judge(arm, declared, cells, declared["expected"])
        self.assertFalse(verdict["passed"],
                         "an unreadable journal satisfied the question whether a path was named")
        self.assertIn("recordedAs", [one["cell"] for one in verdict["disagreed"]])
        self.assertTrue(verdict.get("unmeasured"),
                        "a managed scenario whose publication could not be read is not measured")

    def test_stdout_is_judged_by_the_adapter_own_validator(self):
        """The rule for what the host accepts comes from the code that decides it.

        A second copy of that rule here agrees with the original only until one of them changes,
        and it already had: the copy accepted a block that never asked for a continuation, which
        the host reports as a failed run.
        """
        def printed(payload):
            return harness.stdout_payload(
                {"stdout": json.dumps(payload), "exitCode": 0, "wallMs": 1})["printed"]

        self.assertEqual(printed({"decision": "block", "reason": "because", "continue": True}),
                         "printed_a_block")
        self.assertEqual(printed({"decision": "block", "reason": "because", "continue": False}),
                         "printed_something_else")
        self.assertEqual(printed({"decision": "block", "reason": "because"}),
                         "printed_something_else")
        self.assertEqual(printed({"decision": "block", "continue": True}),
                         "printed_something_else")
        # And the rule really is the adapter's: what it refuses, this refuses.
        for payload in ({"decision": "block", "reason": "because", "continue": False},
                        {"decision": "block", "continue": True}):
            self.assertTrue(
                completion.verdict_complaints({"decision": payload.get("decision"),
                                               "hook_output": payload}),
                "the adapter accepts output this check expects it to refuse")

    def test_a_payload_from_another_source_cannot_fill_this_cell(self):
        every = self.payloads()
        every["journal"] = dict(every["journal"], source="stdout")
        found = harness.read("observation", every)
        self.assertTrue(harness.not_read(found["value"]))
        self.assertIn("stamp", found["detail"])

    def test_a_producer_that_reported_itself_unreadable_stays_unreadable(self):
        every = self.payloads()
        every["journal"]["observation"] = harness.reading.UNREADABLE
        every["journal"]["detail"] = "the record could not be parsed"
        found = harness.read("observation", every)
        self.assertTrue(harness.not_read(found["value"]))
        self.assertIn("could not be parsed", found["detail"])

    def test_a_producer_that_could_not_ask_at_all_is_not_a_readable_answer_either(self):
        """The other half of the partition, which used to walk straight through this door.

        A record read off disk carries its state as text, and only one of the two spellings was
        converted here: an ACCESS_ERROR arrived as a perfectly good value and filled the cell
        with it. Both are readings nobody took and neither is something a cell answered.
        """
        every = self.payloads()
        every["journal"]["observation"] = harness.reading.ACCESS_ERROR
        every["journal"]["detail"] = "the journal could not be reached"
        found = harness.read("observation", every)
        self.assertFalse(found["readable"],
                         "a reading that could not be asked at all was recorded as readable")
        self.assertTrue(harness.not_read(found["value"]),
                        "a reading that could not be asked at all filled the cell as though it"
                        " were an answer")
        self.assertIn("could not be reached", found["detail"])
        self.assertEqual(state_of(found["value"]), harness.reading.ACCESS_ERROR,
                         "the door every cell goes through rebuilt an access error as a shape"
                         " nobody could read, which collapses two of the four answers")

    def test_a_producer_that_could_not_ask_keeps_that_answer_through_the_door(self):
        """The same partition, carried from a real producer rather than from a spelled payload.

        Review found this: read() is the one door every cell goes through, and it rebuilt the
        reading it was handed with the default state, so a question that could not be asked came
        out the other side as an answer nobody could read. The two are different facts about the
        run and reading.py keeps them apart on purpose.
        """
        def unrunnable(*_args, **_kwargs):
            raise OSError("no git on this host")

        with mock.patch.object(harness.subprocess, "run", unrunnable):
            produced = harness.repository_commit()
        self.assertEqual(state_of(produced), harness.reading.ACCESS_ERROR,
                         "a command that could not be started is answered as one that ran and"
                         " could not be read, so the two answers are the same answer")
        every = self.payloads()
        every["marker-root"] = {"source": "marker-root", "observationFile": produced,
                                "heldFile": "reserved"}
        found = harness.read("observationFile", every)
        self.assertEqual(state_of(found["value"]), harness.reading.ACCESS_ERROR,
                         "the state a producer established was replaced at the door")
        self.assertEqual(harness.render(found["value"])["notRead"],
                         harness.reading.ACCESS_ERROR,
                         "the written document reports a question that could not be asked as an"
                         " answer nobody could read")

    def test_the_marker_reading_answers_what_the_repository_partition_answers(self):
        """One partition, not a second copy of it. Review found the copy already disagreeing.

        reading.observe is where this repository decides what is at a path: a link that loops is
        a link that EXISTS whose shape cannot be read, and this file answered that the question
        could not be asked. A directory sitting where a marker file belongs was worse - it read
        as nothing having been published. Driven against the filesystem rather than a patched
        call, because the defect this replaces was one the copy made on real paths.
        """
        root = Path(tempfile.mkdtemp(prefix="hook-comparison-partition-"))
        self.addCleanup(lambda: __import__("shutil").rmtree(str(root), ignore_errors=True))
        loop = root / "loop"
        os.symlink(str(loop), str(loop))
        (root / "a-directory").mkdir()
        (root / "a-file").write_text("x", encoding="utf-8")
        for name in ("loop", "a-directory", "a-file", "nothing-here"):
            path = root / name
            with self.subTest(path=name):
                answered = harness._there(path, "resolved", "not_published")
                settled = harness.reading.observe(path, "whether a marker file is there")
                if settled is None:
                    self.assertEqual(answered, "resolved")
                elif settled.state == harness.reading.ABSENT:
                    self.assertEqual(answered, "not_published")
                else:
                    self.assertEqual(state_of(answered), settled.state,
                                     "this file answers " + str(state_of(answered)) + " where"
                                     " the partition every other reading uses answers "
                                     + settled.state)

    def test_the_environment_handed_to_a_subprocess_writes_nothing_into_the_checkout(self):
        """Asked of the function that builds it, because the promise is about what it hands over.

        The subprocesses import the relay and the runtime modules out of this checkout. Without
        this they leave __pycache__ beside that source, which is a write into the checkout by a
        command whose result says everything it writes goes under its own directory.
        """
        root = Path(tempfile.mkdtemp(prefix="hook-comparison-env-"))
        self.addCleanup(lambda: __import__("shutil").rmtree(str(root), ignore_errors=True))
        arm = type("Arm", (object,), {"root": root, "codex_home": root / "codex"})()
        built = harness.environment(arm)
        self.assertEqual(built.get("PYTHONDONTWRITEBYTECODE"), "1")
        self.assertEqual(built.get("PYTHONPATH"), "")
        self.assertEqual(built.get("CODEX_HOME"), str(root / "codex"))
        for leaked in ("CRW_COMPLETION_HOOK_CONFIG", "CODEX_SESSION_RELAY_MARKER_ROOT"):
            self.assertNotIn(leaked, built,
                             leaked + " is handed to the subprocesses, and it can send them"
                                      " outside the directory this run made for itself")

    def test_a_stat_that_failed_is_not_reported_as_an_absence(self):
        """The reading that answers whether a file is there must not answer for one it cannot see.

        Exercised against the filesystem rather than a patched call, because the defect this
        replaces was exactly that the failure never escaped the call being relied on.
        """
        root = Path(tempfile.mkdtemp(prefix="hook-comparison-stat-"))
        self.addCleanup(lambda: __import__("shutil").rmtree(str(root), ignore_errors=True))
        loop = root / "loop"
        os.symlink(str(loop), str(loop))
        real = root / "real"
        real.write_text("x", encoding="utf-8")
        self.assertTrue(harness.not_read(harness._there(loop, "present", "missing")))
        self.assertEqual(harness._there(root / "nope", "present", "missing"), "missing")
        self.assertEqual(harness._there(real, "present", "missing"), "present")


class VerdictTests(unittest.TestCase):
    """A row's assertion has to be able to fail, and to fail for the right reason."""

    def cells(self, **overrides):
        values = {"processExit": 0,
                  "adapterOutcome": "guard_answered", "observation": "receipt_missing",
                  "guardDecision": "block", "guardState": "receipt_missing",
                  "printedBlock": "printed_a_block", "heldFile": "reserved",
                  "recordedAs": "hook/s/t/0", "observationFile": "resolved",
                  "firedCommand": "python3 x", "journalElapsedMs": 5, "processWallMs": 9}
        values.update(overrides)
        return dict((cell, {"cell": cell, "value": value, "readable": True})
                    for cell, value in values.items())

    def declared(self):
        for one in harness.SCENARIOS:
            if one["name"] == "receipt_missing":
                return one
        raise AssertionError("the scenario this case is built on is gone")

    def arm(self, name):
        return type("Arm", (object,), {"name": name})()

    def test_a_row_that_missed_its_declared_state_fails_and_names_the_cell(self):
        declared = self.declared()
        verdict = harness.judge(self.arm("on"), declared,
                                self.cells(guardState="unresolved_handoff"), declared["expected"])
        self.assertFalse(verdict["passed"])
        self.assertEqual([one["cell"] for one in verdict["disagreed"]], ["guardState"])

    def test_a_row_that_reached_everything_it_declared_passes(self):
        declared = self.declared()
        verdict = harness.judge(self.arm("on"), declared, self.cells(), declared["expected"])
        self.assertTrue(verdict["passed"], json.dumps(verdict))

    def test_a_managed_scenario_that_published_nothing_is_unmeasured_rather_than_passing(self):
        """The silent failure, asserted directly: a guard that selected nothing is not a pass."""
        declared = self.declared()
        verdict = harness.judge(self.arm("on"), declared,
                                self.cells(recordedAs=None, observationFile="not_published"),
                                declared["expected"])
        self.assertFalse(verdict["passed"])
        self.assertTrue(verdict["unmeasured"])

    def test_an_adapter_that_never_answered_is_unmeasured_and_not_a_clean_bill(self):
        declared = self.declared()
        verdict = harness.judge(self.arm("on"), declared,
                                self.cells(adapterOutcome="guard_unreachable"),
                                declared["expected"])
        self.assertFalse(verdict["passed"])
        self.assertTrue(verdict["unmeasured"])
        self.assertIn("does not mean no omission", verdict["because"])

    def test_the_off_arm_fails_a_cell_that_answered_something_instead_of_an_absence(self):
        declared = self.declared()
        cells = dict((cell, {"cell": cell, "value": harness.reading.ABSENT, "readable": True,
                             "detail": harness.NO_REGISTRATION})
                     for cell in harness.FIRING_CELLS)
        self.assertTrue(harness.judge(self.arm("off"), declared, cells,
                                      declared["expected"])["passed"])
        cells["observation"] = {"cell": "observation", "value": "unmanaged", "readable": True}
        verdict = harness.judge(self.arm("off"), declared, cells, declared["expected"])
        self.assertFalse(verdict["passed"])
        self.assertEqual([one["cell"] for one in verdict["disagreed"]], ["observation"])



# The one place this module reaches a conclusion by reading source text rather than by asking the
# thing itself, with the reason it cannot ask. What it forbids is a line that was never written,
# so the source is the only witness there is. It is a lint and says so; that cells really are
# filled by read() is established behaviourally by the mutation cases above.
TEXT_EVIDENCE = {
    "test_every_cell_written_into_a_row_went_through_the_declared_reading":
        "the property is that no other line assembles a cell, and a line nobody wrote has no"
        " object to interrogate. It is a lint, and it is the weaker half: what actually carries"
        " the property is that every cell in every row names the source and the path this check"
        " writes down for it, which a helper assembling cells another way would have to"
        " reproduce exactly, and reproducing it is being a reading.",
}


@unittest.skipUnless(HAVE_RELAY, "these read what the hook process left on disk, and no run can"
                                 " be made on an interpreter the relay does not support")
class WitnessTests(unittest.TestCase):
    """What the hook process itself left behind, read from the root this check created.

    Everything else here reads a document the harness printed. These cases read files the harness
    did not write: the journal is written by the registered command, and the observations under
    the marker root by the relay that command calls. A harness that printed a well-formed document
    without running anything leaves both of them empty.
    """

    # What these cannot witness, stated where the claim is made rather than left to be assumed:
    # they establish that a hook process ran, reached the guard and left the states below, and
    # they do not establish WHICH executable ran. The argv is the harness's own report, checked
    # against the registration on disk, so a harness that deliberately ran a different entry point
    # writing byte-identical journals and markers while reporting the registered command would
    # pass. Closing that needs a witness at the process boundary, which is not built here.

    def records(self, arm):
        found = []
        for path in sorted((run_root() / arm / "journal").rglob("*.json")):
            found.append(json.loads(path.read_text(encoding="utf-8")))
        return found

    def test_the_arm_that_registered_nothing_left_no_record_of_a_firing(self):
        run_once()
        self.assertEqual(self.records("off"), [],
                         "the arm whose install was a dry run produced hook journal records, so"
                         " something ran there")
        self.assertEqual(sorted((run_root() / "off" / "markers").rglob("hook")), [],
                         "the arm that ran nothing has published observations")

    def test_the_registered_command_left_one_record_for_every_firing(self):
        """Counted on disk, so a document describing firings that did not happen fails here."""
        run_once()
        found = self.records("on")
        self.assertEqual(len(found), sum(FIRINGS.values()),
                         "the number of records the hook process wrote is not the number of"
                         " firings this check expects")
        for record in found:
            self.assertEqual(record.get("event"), "Stop")
            self.assertEqual(record.get("adapterOutcome"), "guard_answered",
                             "a firing the hook recorded never reached the guard")

    def test_the_states_on_disk_are_the_states_this_check_declares(self):
        """The same table as the row case, read from the journal instead of from the report."""
        run_once()
        found = sorted((record["observation"], record["guardDecision"], record["guardState"])
                       for record in self.records("on"))
        declared = sorted((one[0], one[1], one[2]) for one in DECLARED.values())
        self.assertEqual(found, declared,
                         "what the hook process recorded is not what this check declared, so the"
                         " document and the disk disagree")

    def test_the_guard_published_its_observations_under_the_marker_root(self):
        run_once()
        # The reservation lives in the same directory and is named rather than numbered, so it
        # is excluded here: counting it would have made this case agree with itself.
        published = sorted(path for path in
                           (run_root() / "on" / "markers").rglob("hook/*/*/*.json")
                           if path.name != "hold.json")
        # Every firing but the unmanaged one, which selects no assignment and so publishes nothing.
        self.assertEqual(len(published), sum(FIRINGS.values()) - 1,
                         "the relay published a different number of observations from the number"
                         " of managed firings")
        reserved = sorted((run_root() / "on" / "markers").rglob("hook/*/*/hold.json"))
        self.assertEqual(len(reserved), 4,
                         "the reservations on disk are not the three omissions plus the first"
                         " firing of the duplicate scenario")


class DerivationTests(unittest.TestCase):
    """Properties of the declarations, derived rather than restated."""

    def test_the_harness_absence_partition_is_the_one_this_check_names(self):
        """The set the comparisons run against, checked rather than adopted."""
        self.assertEqual(sorted(str(one) for one in harness.ABSENCE_ANSWERS),
                         sorted(str(one) for one in ABSENCE_ANSWERS),
                         "the harness recognises a different set of absences from this check")

    def test_every_declared_cell_has_exactly_one_reading(self):
        names = [cell for cell, _s, _p, _q in harness.CELLS]
        self.assertEqual(len(names), len(set(names)), "a cell is declared twice")
        self.assertEqual(sorted(names), sorted(ARM_CELLS + FIRING_CELLS),
                         "the harness declares a different set of cells from this check")
        for cell, source, path, producer in harness.CELLS:
            self.assertTrue(path, cell + " declares no path to read")
            self.assertTrue(producer, cell + " declares nothing that produces it")

    def test_every_place_an_absence_is_normal_is_derived_and_carries_a_reason(self):
        places = harness.absence_places()
        self.assertTrue(places, "no place declares an absence, so the derivation is empty")
        for (arm, scenario, cell), place in places.items():
            self.assertIn(arm, ARMS)
            self.assertIn(scenario, SCENARIOS)
            self.assertIn(cell, FIRING_CELLS)
            self.assertTrue(str(place["because"]).strip(), arm + "/" + scenario + "/" + cell
                            + " declares an absence without saying why it is normal there")
            self.assertIn(place["answer"], ABSENCE_ANSWERS,
                          arm + "/" + scenario + "/" + cell + " declares an absence whose answer"
                          " is not one the sources can give")
        for scenario in SCENARIOS:
            for cell in FIRING_CELLS:
                self.assertIn(("off", scenario, cell), places,
                              "the arm that runs nothing must declare its absence for every cell")

    @unittest.skipUnless(HAVE_RELAY, "the derived places are compared with a run, and no run can"
                                     " be made on an interpreter the relay does not support")
    def test_every_cell_that_read_absent_was_declared_absent_and_every_declared_place_read_it(self):
        """The two sets are the same set, which is what makes the declaration load-bearing."""
        answer = run_once()
        expected = dict(ABSENCE_PLACES)
        for scenario in SCENARIOS:
            for cell in FIRING_CELLS:
                expected[("off", scenario, cell)] = "ABSENT"
        declared = dict(((place["arm"], place["scenario"], place["cell"]), place["answer"])
                        for place in answer["absenceIsNormalAt"])
        self.assertEqual(sorted(str(one) for one in declared),
                         sorted(str(one) for one in expected),
                         "the run declares absences in different places from this check")
        for place, answered in sorted(expected.items()):
            self.assertEqual(declared[place], answered,
                             str(place) + " declares an absence answer this check does not")
        found = {}
        for scenario in SCENARIOS:
            for arm in ARMS:
                for one in answer["scenarios"][scenario][arm]["firings"]:
                    for cell, value in one["cells"].items():
                        if value["value"] in ABSENCE_ANSWERS:
                            found[(arm, scenario, cell)] = value["value"]
        self.assertEqual(sorted(set(found) - set(declared)), [],
                         "a cell answered an absence in a place nothing declared it normal")
        self.assertEqual(sorted(set(declared) - set(found)), [],
                         "a place declares an absence that never happened, so the declaration"
                         " describes a run other than this one")
        self.assertEqual(sorted(str(one) for one in found),
                         sorted(str(one) for one in expected),
                         "the cells that answered an absence are not the places this check names")
        for place, answered in sorted(found.items()):
            self.assertEqual(answered, expected[place],
                             str(place) + " answered a different absence from the one declared")

    @unittest.skipUnless(HAVE_RELAY, "this reads a run, and no run can be made on an interpreter"
                                     " the relay does not support")
    def test_every_cell_in_every_row_carries_the_reading_this_check_names_for_it(self):
        """The behavioural half of the rule below, and the stronger half.

        A helper that assembled cells without going through the declared reading would have to
        reproduce the source and the path for every cell exactly as they are written here, and
        something that does that is a reading.
        """
        answer = run_once()
        for arm in ARMS:
            for cell in ARM_CELLS:
                source, path = CELL_READINGS[cell]
                found = answer["arms"][arm][cell]
                self.assertEqual((found["answeredBy"], found["readingPath"]), (source, path),
                                 arm + "/" + cell + " was answered by something else")
        for scenario in SCENARIOS:
            for arm in ARMS:
                for one in answer["scenarios"][scenario][arm]["firings"]:
                    for cell, found in one["cells"].items():
                        source, path = CELL_READINGS[cell]
                        self.assertEqual((found["cell"], found["answeredBy"], found["readingPath"]),
                                         (cell, source, path),
                                         scenario + "/" + arm + "/" + cell
                                         + " was answered by something else")

    def test_every_process_and_parse_boundary_is_guarded(self):
        """Derived from source: a boundary that can raise must not be able to end the command.

        The command promises one JSON object on stdout, so a timeout, an executable that could not
        be started, or output that is not JSON has to become a refusal or an unreadable reading.
        Two of these were found by deriving the set rather than reading the code: both git calls
        caught a missing executable and not a timeout.
        """
        source = (ROOT / "scripts" / "hook_comparison.py").read_text(encoding="utf-8")
        tree = ast.parse(source)
        needed = {"run": ("TimeoutExpired", "OSError"), "loads": ("ValueError",),
                  "read_text": ("OSError",)}
        parents = {}
        for node in ast.walk(tree):
            for child in ast.iter_child_nodes(node):
                parents[child] = node

        def named(node):
            if isinstance(node, ast.Attribute):
                return node.attr
            if isinstance(node, ast.Name):
                return node.id
            return ""

        def handlers_around(node):
            seen, cur = [], parents.get(node)
            while cur is not None:
                if isinstance(cur, ast.Try):
                    for handler in cur.handlers:
                        if isinstance(handler.type, ast.Tuple):
                            seen.extend(named(one) for one in handler.type.elts)
                        elif handler.type is not None:
                            seen.append(named(handler.type))
                cur = parents.get(cur)
            return seen

        boundaries = []
        for node in ast.walk(tree):
            if not isinstance(node, ast.Call):
                continue
            call = named(node.func)
            if call not in needed:
                continue
            if call == "run" and not isinstance(node.func, ast.Attribute):
                continue
            boundaries.append((call, node.lineno, handlers_around(node)))
        self.assertGreaterEqual(len(boundaries), 8,
                                "almost no boundary was found, so this derivation is watching a"
                                " file that no longer crosses any")
        unguarded = []
        for call, line, seen in boundaries:
            for wanted in needed[call]:
                if not any(one == wanted or one in ("Exception", "BaseException") for one in seen):
                    unguarded.append(call + " at line " + str(line) + " does not handle " + wanted)
        self.assertEqual(unguarded, [],
                         "a boundary can raise past the refusal document: " + "; ".join(unguarded))

    def test_every_cell_written_into_a_row_went_through_the_declared_reading(self):
        """No other line in the harness assembles a cell. Source, because the subject is absence.

        Declared in TEXT_EVIDENCE with its reason. Everything else this module concludes it
        concludes by asking the thing itself.
        """
        source = (ROOT / "scripts" / "hook_comparison.py").read_text(encoding="utf-8")
        tree = ast.parse(source)
        # Every way the name could be written, not one spelling of it: a comprehension named
        # differently, a loop filling it key by key, a dict() call or a literal would each have
        # walked past a rule that only looked at comprehensions iterating a variable called cell.
        built, stored = [], []
        for node in ast.walk(tree):
            targets = []
            if isinstance(node, ast.Assign):
                targets = node.targets
            elif isinstance(node, (ast.AugAssign, ast.AnnAssign)):
                targets = [node.target]
            for target in targets:
                if isinstance(target, ast.Name) and target.id == "cells":
                    built.append(node.value)
                if (isinstance(target, ast.Subscript) and isinstance(target.value, ast.Name)
                        and target.value.id == "cells"):
                    stored.append(node)
        self.assertEqual(stored, [],
                         "a cell is written into the mapping one key at a time, which is a slot"
                         " this rule cannot see through")
        mutated = [node for node in ast.walk(tree)
                   if isinstance(node, ast.Call) and isinstance(node.func, ast.Attribute)
                   and isinstance(node.func.value, ast.Name) and node.func.value.id == "cells"]
        self.assertEqual(mutated, [],
                         "a mapping of cells is changed by a method call, which is another slot"
                         " this rule cannot see through")
        # Binding the name to a mapping that already exists builds nothing and is left alone;
        # what this rule is about is the places that CONSTRUCT one.
        # Only two ways of binding the name are left alone: a comprehension over the declared
        # readings, and a subscript that reads a mapping built elsewhere in this file. Binding it
        # to another name is rejected too, because the name could have been bound to a helper
        # call a line earlier and the indirection is the evasion rather than a use of it.
        constructed = [value for value in built if not isinstance(value, ast.Subscript)]
        self.assertTrue(constructed,
                        "nothing constructs a mapping of cells, so this check is watching a file"
                        " that no longer does what it describes")
        for value in constructed:
            self.assertIsInstance(value, ast.DictComp,
                                  "a mapping of cells is bound to something other than a"
                                  " comprehension over the declared readings or a subscript of a"
                                  " mapping already built that way. A helper, directly or through"
                                  " another name, could stamp the declared source onto a value it"
                                  " took from somewhere else")
            self.assertTrue(isinstance(value.value, ast.Call)
                            and isinstance(value.value.func, ast.Name)
                            and value.value.func.id == "read",
                            "a mapping of cells is filled by something other than read()")
        self.assertIn("test_every_cell_written_into_a_row_went_through_the_declared_reading",
                      TEXT_EVIDENCE, "the one text reading has to stay declared")


class VerdictGuardTests(unittest.TestCase):
    """No verdict can be computed without the guard, and the guard does what it says.

    The rule was stated and each verdict applied it separately, until one did not. The standing
    check that was meant to catch that only fired when a not-taken count was already nonzero,
    which never happens in a healthy run, so it watched the property without exercising it. Both
    halves are here now: the guard is the only way to compute a verdict, and the guard is driven
    with a nonzero count rather than observed on a run where the count is zero.
    """

    def test_the_guard_refuses_a_verdict_while_a_reading_was_not_taken(self):
        self.assertTrue(harness.judged(True, 0))
        self.assertTrue(harness.judged(True, []))
        self.assertFalse(harness.judged(True, 1))
        self.assertFalse(harness.judged(True, ["a/cell"]))
        self.assertFalse(harness.judged(False, 0))
        self.assertFalse(harness.judged(False, 2))

    def test_every_verdict_in_the_harness_is_computed_by_that_guard(self):
        """Derived over every met in the file, not over the ones a healthy run happens to show."""
        source = (ROOT / "scripts" / "hook_comparison.py").read_text(encoding="utf-8")
        tree = ast.parse(source)
        verdicts = []
        for node in ast.walk(tree):
            if not isinstance(node, ast.Dict):
                continue
            for key, value in zip(node.keys, node.values):
                if isinstance(key, ast.Constant) and key.value == "met":
                    verdicts.append((getattr(value, "lineno", None), value))
        self.assertGreaterEqual(len(verdicts), 7,
                                "almost nothing computes a verdict, so this derivation is"
                                " watching a file that stopped judging")
        for line, value in verdicts:
            self.assertTrue(isinstance(value, ast.Call) and isinstance(value.func, ast.Name)
                            and value.func.id == "judged",
                            "the verdict at line " + str(line) + " is computed without the guard,"
                            " so it can be true while a reading under it was not taken")

    def test_the_standing_check_reaches_every_verdict_the_document_carries(self):
        """The check's reach and the set of verdicts have to be the same set.

        A check whose set is narrower than the property leaks exactly where it is not looking,
        which is how one verdict passed while the rule was already written down. Compared as sets
        of places rather than as counts: the source computes seven verdicts and the document
        carries nine, because one function produces three of them, and a count would have been
        satisfied by the wrong nine.
        """
        if not HAVE_RELAY:
            self.skipTest("the document side of this needs a run")
        answer = run_once()
        carried = set()

        def walk(payload, path=()):
            if isinstance(payload, dict):
                for key, value in payload.items():
                    here = path + (key,)
                    if key == "met":
                        carried.add("/".join(str(one) for one in path))
                    walk(value, here)
            elif isinstance(payload, list):
                for index, value in enumerate(payload):
                    walk(value, path + (index,))

        walk(dict((key, value) for key, value in answer.items() if key != "passed"))
        reached = set()
        for name, measure in answer["measures"].items():
            if "met" in measure:
                reached.add("measures/" + name)
        for name in answer["supplemental"]:
            reached.add("supplemental/" + name)
        for name in ("wroteOnlyInsideItsRoot", "sourceIdentity"):
            reached.add(name)
        self.assertEqual(sorted(carried - reached), [],
                         "the document carries a verdict in a place the checks never look")
        self.assertEqual(sorted(reached - carried), [],
                         "the checks look for a verdict the document does not carry")
        self.assertGreaterEqual(len(carried), 9)


class ScopeTests(unittest.TestCase):
    """A judgment speaks for the arms it declares, and its predicate reads exactly those.

    The same judgment had two holes in a row: it folded an unreadable reading, and then it read
    one arm while the evidence it cited was the pair. Matching claim to predicate by eye is what
    produced the second, so the scope is declared and this derives the agreement.
    """

    @unittest.skipUnless(HAVE_RELAY, "this reads a run")
    def test_every_judgment_reads_exactly_the_arms_it_claims(self):
        answer = run_once()
        scoped = []
        for name, measure in sorted(answer["measures"].items()):
            if "arms" in measure:
                scoped.append(("measures/" + name, measure))
        for name, measure in sorted(answer["supplemental"].items()):
            if "arms" in measure:
                scoped.append(("supplemental/" + name, measure))
        self.assertGreaterEqual(len(scoped), 6,
                                "almost no judgment declares which arms it speaks for")
        for where, measure in scoped:
            declared = measure["arms"]
            self.assertTrue(declared, where + " declares an empty scope")
            for arm in declared:
                self.assertIn(arm, ARMS, where + " claims an arm that does not exist")
            values = measure.get("values")
            if values is not None:
                self.assertEqual(sorted(values), sorted(declared),
                                 where + " records values for a different set of arms from the"
                                         " one it claims to speak for")

    @unittest.skipUnless(HAVE_RELAY, "this reads a run")
    def test_the_judgment_about_both_arms_reads_both(self):
        answer = run_once()
        survives = answer["supplemental"]["foreignRegistrationSurvives"]
        self.assertEqual(sorted(survives["arms"]), sorted(ARMS),
                         "the claim is about the entry surviving in both hook files")
        self.assertEqual(sorted(survives["values"]), sorted(ARMS))
        for arm in ARMS:
            self.assertEqual(survives["values"][arm], "present")

    def cell(self, value):
        return {"cell": "x", "value": value, "readable": True}

    def scenarios_with(self, off, on):
        """The smallest input the supplemental judgments read, with the arms set by hand."""
        firing = {"cells": {"heldFile": self.cell("reserved"),
                            "recordedAs": self.cell("hook/s/t/0")}}
        second = {"cells": {"heldFile": self.cell("reserved"),
                            "recordedAs": self.cell("hook/s/t/1")}}
        return {"_arms": {"off": {"foreignRegistration": self.cell(off)},
                          "on": {"foreignRegistration": self.cell(on)}},
                "duplicate": {"on": {"firings": [firing, second]}}}

    def test_the_both_arms_judgment_fails_when_the_arm_it_does_not_read_fails(self):
        """Driven at the judgment, because comparing declared arms with recorded values is not it.

        A predicate that reads the on arm while the claim covers both still records both values,
        so a check comparing those two sets passes while the hole is open. The only thing that
        closes it is making the arm the predicate might be skipping the one that fails.
        """
        both_present = harness.supplemental(self.scenarios_with("present", "present"))
        self.assertTrue(both_present["foreignRegistrationSurvives"]["met"])
        off_displaced = harness.supplemental(self.scenarios_with("displaced", "present"))
        self.assertFalse(off_displaced["foreignRegistrationSurvives"]["met"],
                         "the judgment claims both hook files and passed while the off arm had"
                         " displaced the other owner's entry")
        on_displaced = harness.supplemental(self.scenarios_with("present", "displaced"))
        self.assertFalse(on_displaced["foreignRegistrationSurvives"]["met"])

    def test_an_entry_rewritten_in_place_has_not_survived(self):
        """The claim is that another owner's registration survived, not that a string is present.

        An entry whose command still reads the same while its type, timeout or matcher changed has
        been rewritten, and the owner would find a registration it did not make.
        """
        seeded = json.loads(json.dumps(harness.FOREIGN_HOOKS))
        self.assertEqual(harness._foreign(seeded), "present")
        for field, value in (("timeout", 99), ("type", "something-else")):
            altered = json.loads(json.dumps(harness.FOREIGN_HOOKS))
            altered["hooks"][harness.completion.EVENT][0]["hooks"][0][field] = value
            self.assertEqual(harness._foreign(altered), "altered",
                             "an entry rewritten in " + field + " was reported as surviving")
        with_matcher = json.loads(json.dumps(harness.FOREIGN_HOOKS))
        with_matcher["hooks"][harness.completion.EVENT][0]["matcher"] = "something"
        self.assertEqual(harness._foreign(with_matcher), "altered")
        gone = {"version": 1, "hooks": {harness.completion.EVENT: []}}
        self.assertEqual(harness._foreign(gone), "displaced")
        # Position is identity: hooks.identity derives a hook's identity from the event, the
        # matcher index and the hook index, so an entry appended ahead of this one moves it and
        # invalidates the hash its owner trusted.
        ahead = json.loads(json.dumps(harness.FOREIGN_HOOKS))
        ahead["hooks"][harness.completion.EVENT].insert(
            0, {"hooks": [{"type": "command", "command": "/opt/other", "timeout": 1}]})
        self.assertEqual(harness._foreign(ahead), "moved",
                         "an entry pushed to a new index was reported as surviving in place")
        within = json.loads(json.dumps(harness.FOREIGN_HOOKS))
        within["hooks"][harness.completion.EVENT][0]["hooks"].insert(
            0, {"type": "command", "command": "/opt/other", "timeout": 1})
        self.assertEqual(harness._foreign(within), "moved")

    def test_a_predicate_over_a_scope_fails_when_any_arm_in_it_fails(self):
        """The helper itself, driven with an arm that fails, rather than watched on a clean run."""
        passing = {"off": "present", "on": "present"}
        self.assertTrue(harness.across(ARMS, passing, lambda value: value == "present"))
        for broken in ({"off": "displaced", "on": "present"},
                       {"off": "present", "on": "displaced"}):
            self.assertFalse(harness.across(ARMS, broken, lambda value: value == "present"),
                             "a scope covering both arms passed while one of them failed")


class ResponseContractTests(unittest.TestCase):
    """Nothing is taken out of a relay response that the command did not promise to answer with.

    Three members of one family landed in a row: a parse that failed, valid JSON that is not an
    object, and an object missing the field the caller was about to read. Each time the boundary
    was a layer narrower than what could go wrong, so the contract now lives with the command and
    relay() is the only door.
    """

    def launcher(self, body):
        root = Path(tempfile.mkdtemp(prefix="hook-comparison-contract-"))
        self.addCleanup(lambda: __import__("shutil").rmtree(str(root), ignore_errors=True))
        path = root / "codex-session-relay"
        path.write_text("#!" + sys.executable + chr(10) + body, encoding="utf-8")
        path.chmod(0o755)
        return type("Arm", (object,), {
            "launcher": path, "state": root / "state", "root": root,
            "codex_home": root / "codex",
            "environment": harness.environment(
                type("A", (object,), {"root": root, "codex_home": root / "codex"})()),
        })()

    def test_a_response_missing_a_field_the_caller_reads_is_refused(self):
        for body, missing in (("pass", "assignmentId"),
                              ("print('{}')", "assignmentId"),
                              ("print('{\"assignmentId\": \"a\"}')", "assignmentDir")):
            arm = self.launcher(body + chr(10))
            with self.assertRaises(harness.RelayError) as caught:
                harness.relay(arm, "intent-declare", "--workspace", "x")
            self.assertIn(missing, str(caught.exception),
                          "the refusal does not name the field that was not answered")

    def test_a_complete_response_is_returned(self):
        arm = self.launcher(
            "print('{\"assignmentId\": \"a\", \"assignmentDir\": \"/d\"}')" + chr(10))
        self.assertEqual(harness.relay(arm, "intent-declare")["assignmentId"], "a")

    def test_every_field_taken_from_a_response_is_one_its_command_promises(self):
        """Derived over the source, so a field read without a contract cannot be added quietly."""
        source = (ROOT / "scripts" / "hook_comparison.py").read_text(encoding="utf-8")
        tree = ast.parse(source)
        taken = []
        for node in ast.walk(tree):
            if not isinstance(node, ast.Subscript):
                continue
            target = node.value
            command = None
            if isinstance(target, ast.Call) and isinstance(target.func, ast.Name) \
                    and target.func.id == "relay" and len(target.args) >= 2 \
                    and isinstance(target.args[1], ast.Constant):
                command = target.args[1].value
            if command is None:
                continue
            key = node.slice.value if isinstance(node.slice, ast.Constant) else None
            taken.append((command, key, node.lineno))
        self.assertTrue(taken, "nothing reads a field out of a response, so this derivation is"
                               " watching a file that stopped reading them")
        for command, key, line in taken:
            self.assertIn(command, harness.RESPONSE_FIELDS,
                          "line " + str(line) + " reads a field out of " + repr(command)
                          + ", which declares no response contract")
            self.assertIn(key, harness.RESPONSE_FIELDS[command],
                          "line " + str(line) + " reads " + repr(key) + " out of "
                          + repr(command) + ", which does not promise it")


class BoundaryTests(unittest.TestCase):
    """Every failure mode executed, rather than inferred from a handler being present.

    Reading the source for an except clause says a handler exists; it does not say the path
    reaches it and answers with the document this command promises. These drive each mode for
    real. The mode that was missing from the derived list is the last one: valid JSON whose shape
    is not an object, where the parse succeeds and there is still nothing to read.
    """

    MODES = ("timeout", "not startable", "nonzero exit", "unparseable output",
             "valid JSON that is not an object", "empty output")

    def launcher(self, body):
        root = Path(tempfile.mkdtemp(prefix="hook-comparison-boundary-"))
        self.addCleanup(lambda: __import__("shutil").rmtree(str(root), ignore_errors=True))
        path = root / "codex-session-relay"
        path.write_text("#!" + sys.executable + chr(10) + body, encoding="utf-8")
        path.chmod(0o755)
        arm = type("Arm", (object,), {
            "launcher": path, "state": root / "state", "root": root,
            "codex_home": root / "codex",
            "environment": harness.environment(
                type("A", (object,), {"root": root, "codex_home": root / "codex"})()),
        })()
        return arm

    def refusal_for(self, body):
        arm = self.launcher(body)
        with self.assertRaises(harness.RelayError) as caught:
            harness.relay(arm, "intent-declare", "--marker-root", str(arm.root))
        return str(caught.exception)

    def test_every_relay_failure_mode_answers_with_a_named_refusal(self):
        answered = {}
        answered["nonzero exit"] = self.refusal_for("raise SystemExit(3)" + chr(10))
        answered["unparseable output"] = self.refusal_for("print('<>')" + chr(10))
        answered["valid JSON that is not an object"] = self.refusal_for(
            "print('[1, 2]')" + chr(10))
        arm = self.launcher("pass" + chr(10))
        self.assertEqual(harness.relay(arm, "intent-bind"), {},
                         "empty output is an empty answer for a command nothing is read out of")
        with self.assertRaises(harness.RelayError):
            harness.relay(arm, "intent-declare")
        answered["empty output"] = "refused where a field is read, accepted where none is"
        missing = self.launcher("pass" + chr(10))
        missing.launcher = Path(str(missing.launcher) + "-does-not-exist")
        with self.assertRaises(harness.RelayError) as caught:
            harness.relay(missing, "intent-declare")
        answered["not startable"] = str(caught.exception)
        self.assertIn("could not be started", answered["not startable"])
        self.assertIn("exited 3", answered["nonzero exit"])
        self.assertIn("not JSON", answered["unparseable output"])
        self.assertIn("not an object", answered["valid JSON that is not an object"])
        for mode in ("nonzero exit", "unparseable output", "valid JSON that is not an object",
                     "not startable"):
            self.assertTrue(answered[mode], mode + " produced no named refusal")

    @unittest.skipUnless(HAVE_RELAY, "below the relay floor the run refuses on the interpreter"
                                     " before it reaches the build, so the injected failure is"
                                     " never the one answered; the refusal itself is checked in"
                                     " RefusalTests, which does run here")
    def test_a_failure_after_the_root_exists_still_answers_with_a_document(self):
        """The promise is one JSON object, and only the relay's own error was turned into one.

        Everything else a run can hit after the root exists - a filesystem error, an interrupted
        run - ended the command with a traceback. Driven by running the real command with a
        failure injected into its build, rather than by reading main for an except clause.

        The copy is given a repository root of its own so its imports resolve where the original's
        do; without that the process fails before reaching the injection and the case would pass
        on an error that is not the one it means to cause.
        """
        root = Path(tempfile.mkdtemp(prefix="hook-comparison-fault-"))
        self.addCleanup(lambda: __import__("shutil").rmtree(str(root), ignore_errors=True))
        scripts = root / "scripts"
        scripts.mkdir()
        for name in ("crw_runtime", "completion_hook.py", "runtime_install.py"):
            os.symlink(str(ROOT / "scripts" / name), str(scripts / name))
        os.symlink(str(ROOT / "packages"), str(root / "packages"))
        marker = "def launcher_for(root):"
        source = (ROOT / "scripts" / "hook_comparison.py").read_text(encoding="utf-8")
        self.assertIn(marker, source)
        (scripts / "hook_comparison.py").write_text(
            source.replace(marker, marker + chr(10)
                           + '    raise OSError("injected filesystem failure")'),
            encoding="utf-8")
        done = subprocess.run(
            [sys.executable, str(scripts / "hook_comparison.py"), "--root", str(root / "work")],
            capture_output=True, text=True, timeout=300)
        self.assertEqual(done.returncode, 2,
                         "a run that could not finish must not exit 0 or 1: " + done.stderr[-400:])
        answer = json.loads(done.stdout)
        self.assertIn("injected filesystem failure", answer["refused"])
        self.assertIn("OSError", answer["refused"],
                      "the refusal does not name what went wrong, so a defect here would read as"
                      " a data problem")
        self.assertNotIn("scenarios", answer, "a refusal must carry no rows")

    def test_stdout_that_is_valid_json_but_not_an_object_is_not_a_block(self):
        """The mode the derived list did not have: the parse succeeds and nothing can be read."""
        for raw in ("[1, 2]", "null", "3", '"a string"'):
            self.assertEqual(
                harness.stdout_payload({"stdout": raw, "exitCode": 0, "wallMs": 1})["printed"],
                "printed_something_else",
                raw + " was read as something other than output nobody can act on")

    def test_a_journal_record_that_is_not_an_object_is_not_correlated(self):
        root = Path(tempfile.mkdtemp(prefix="hook-comparison-journal-"))
        self.addCleanup(lambda: __import__("shutil").rmtree(str(root), ignore_errors=True))
        day = root / "journal" / "20260918"
        day.mkdir(parents=True)
        (day / ("a" * 32 + ".json")).write_text("[1, 2]", encoding="utf-8")
        (day / ("b" * 32 + ".json")).write_text("not json", encoding="utf-8")
        (day / ("c" * 32 + ".json")).write_text(
            json.dumps({"sessionId": "s", "turnId": "t", "observation": "x"}), encoding="utf-8")
        arm = type("Arm", (object,), {"journal": root / "journal"})()
        found = harness.journal_records(arm, "s", "t")
        self.assertEqual(len(found), 1, "a record that is not an object was correlated as one")


class RefusalTests(unittest.TestCase):
    """An interpreter the relay cannot run on gets a refusal, not rows nobody took."""

    @unittest.skipIf(HAVE_RELAY, "on 3.11 and later the refusal is reached by pinning the version"
                                 " below; the case above it is the real one and it runs on 3.10")
    def test_the_harness_refuses_for_real_on_this_interpreter(self):
        done = subprocess.run([sys.executable, str(ROOT / "scripts" / "hook_comparison.py")],
                              capture_output=True, text=True, timeout=300)
        self.assertEqual(done.returncode, 2, "a refusal is not a result and must not exit 0")
        answer = json.loads(done.stdout)
        self.assertIn("3.11", answer["refused"])
        self.assertNotIn("scenarios", answer, "a refusal must carry no rows")

    @unittest.skipUnless(HAVE_RELAY, "this pins the version the other case reaches for real")
    def test_the_refusal_is_the_answer_below_the_relays_floor(self):
        import io
        import contextlib
        printed = io.StringIO()
        with mock.patch.object(sys, "version_info", (3, 10, 0, "final", 0)):
            with contextlib.redirect_stdout(printed):
                code = harness.main([])
        self.assertEqual(code, 2)
        answer = json.loads(printed.getvalue())
        self.assertIn("3.11", answer["refused"])
        self.assertNotIn("scenarios", answer)
        self.assertEqual(answer["source"], "hook-comparison")


# ------------------------------------------------------------- CRW-103: a read failure is a type
#
# CRW-68 closed twenty-one consumption sites against a sentinel that was a non-empty string, and
# the child who did it wrote that a type could not have escaped. These cases are that claim, asked
# of the thing itself. They obtain the reading through the harness rather than naming a class, so
# they say the same thing against either representation: at the commit this replaces they fail on
# the property they name, not on a class that does not exist there yet.


def unread(why="the record could not be parsed"):
    """A reading that was not taken, obtained from the harness rather than spelled here.

    A FRESH one every call, which is the representation's own invariant rather than a convenience
    of this helper. CPython compares containers by identity before it calls __eq__, so one shared
    instance would answer equal to itself inside a mapping - which is exactly the comparison the
    digest stability judgment makes across the two ends of a run.
    """
    return harness.read("observation", {"journal": {"source": "journal",
                                                    "observation": "UNREADABLE",
                                                    "detail": why}})["value"]


def rendered(payload):
    """The document as JSON, asking anything that is not a value for its own written form.

    Written here rather than taken from the harness so these cases run against either
    representation. Before the change nothing needs writing out at all and the bare sentinel is
    then visible in the value slots, which is the thing being measured.
    """
    def written(value):
        own = getattr(value, "rendered", None)
        if callable(own):
            return own()
        raise TypeError(repr(value))

    return json.loads(json.dumps(payload, default=written, sort_keys=True))


def bare_sentinels(payload, path=()):
    """Every place the written document still spells a reading that was not taken as an answer."""
    found = []
    if isinstance(payload, dict):
        for key, value in sorted(payload.items()):
            if key != "notRead":
                found.extend(bare_sentinels(value, path + (key,)))
    elif isinstance(payload, list):
        for index, value in enumerate(payload):
            found.extend(bare_sentinels(value, path + (index,)))
    elif payload in ("UNREADABLE", "ACCESS_ERROR"):
        found.append("/".join(str(one) for one in path))
    return found


class AReadFailureCannotBeUsedAsAValueTests(unittest.TestCase):
    """The distinction is carried by the language, so a site that misuses it breaks at that site."""

    def test_a_reading_that_was_not_taken_is_not_a_string(self):
        """The whole defect in one line: it used to fit the slot a real answer occupies."""
        self.assertNotIsInstance(unread(), str,
                                 "a reading that was not taken is still spelled as an ordinary"
                                 " answer, so every place that consumes one has to remember to"
                                 " exclude it")

    def test_the_three_checks_it_walked_through_no_longer_let_it_through(self):
        """Truthiness, uniqueness and "is there something there" - the three CRW-68 names.

        Each is written the way a consumption site writes it. Each must either refuse to answer
        or answer that there is nothing. What none of them may do is answer that something is
        there, which is what all three of them did.
        """
        for name, ask in (("truthiness", lambda value: bool(value)),
                          ("uniqueness through a set",
                           lambda value: len({value, "resolved"}) == 2),
                          ("is there something there",
                           lambda value: isinstance(value, str) and bool(value))):
            with self.subTest(check=name):
                try:
                    answered = ask(unread())
                except Exception:
                    continue
                self.assertFalse(answered,
                                 "a reading that was not taken answered yes to " + name
                                 + ", which is how one sentinel escaped three separate checks")

    def test_the_question_the_row_actually_asks_is_answered_correctly_on_its_own(self):
        """recordedAs asks whether a path was named. The old answer said yes without naming one.

        Asked of the harness's own predicate rather than of a copy of it, because that predicate
        is the consumption site this case is about.
        """
        asks = harness.PREDICATES[("recordedAs", harness.PUBLISHED)]
        self.assertTrue(asks("hook/session/turn/0"), "a named path is a named path")
        self.assertFalse(asks(unread()),
                         "a reading that was not taken satisfied the question whether a path was"
                         " named, without naming one")

    def test_the_refusal_it_raises_cannot_be_swallowed_as_a_reading(self):
        """The refusal must not be a shape reading.region converts into a quiet unreadable one.

        reading.SHAPE_FAILURES carries TypeError and reading.region turns one into a Refused
        reporting "could not read". A refusal of that type would therefore be convertible into
        precisely the silent unreadable reading this representation exists to forbid, so the
        exception is checked against that lattice rather than against a name.
        """
        value = unread()
        raised = None
        try:
            value == "anything"
        except BaseException as error:
            raised = error
        self.assertIsNotNone(raised,
                             "using a reading that was not taken as a value raised nothing, so"
                             " nothing but attention keeps it out of a slot a value occupies")
        self.assertNotIsInstance(raised, harness.reading.SHAPE_FAILURES,
                                 "the refusal is a shape reading.region converts into a quiet"
                                 " unreadable reading, which is the swallow it exists to refuse")
        with self.assertRaises(type(raised)):
            with harness.reading.region("here", "a record"):
                raise type(raised)("a consumer used a reading that was not taken as a value")

    def test_the_refusal_names_the_site_that_consumed_it_and_not_the_one_that_raised(self):
        """A refusal without a usable location is reported as a data problem, not as a defect.

        Review found this one. reading.where() answers with the DEEPEST frame of a traceback,
        and the deepest frame here is always the single raise inside the type - so every refusal
        would name that one line and the consumption site the location exists to preserve would
        be gone. The frame wanted is the comparison, the truthiness check or the formatting that
        used the reading.
        """
        value = unread()

        def a_careless_consumer():
            return value == "resolved"

        raised = None
        try:
            a_careless_consumer()
        except BaseException as error:
            raised = error
        self.assertIsNotNone(raised,
                             "consuming a reading that was not taken raised nothing, so there"
                             " is no site to name")
        at = getattr(raised, "at", None)
        self.assertIsNotNone(at, "the refusal carries no location at all")
        consumed = str(a_careless_consumer.__code__.co_firstlineno + 1)
        self.assertTrue(at.endswith(":" + consumed),
                        "the refusal names " + str(at) + " rather than the line that consumed"
                        " the reading, which is this file at line " + consumed)
        self.assertNotEqual(at, harness.reading.where(raised),
                            "the refusal names the line it was raised on, which is the same"
                            " line for every refusal and says nothing about the site")

    def test_a_command_that_ran_out_of_time_is_not_a_command_that_could_not_be_run(self):
        """Review found this. fire() keeps the two apart and the provenance readings did not.

        A command that could not be started was never asked, and a command that started and
        exceeded its bound was asked and did not answer. Both git calls caught the two together
        and reported the second as the first, which is the same collapse read() was making at
        the door - one state standing in for two answers.
        """
        def raising(error):
            def run(*_args, **_kwargs):
                raise error
            return run

        ran_out = subprocess.TimeoutExpired(cmd="git", timeout=60)
        could_not_start = OSError("no git on this host")
        for what, ask in (("the commit", lambda: harness.repository_commit()),
                          ("the working tree",
                           lambda: harness.source_identity()["workingTree"])):
            with self.subTest(reading=what):
                with mock.patch.object(harness.subprocess, "run", raising(ran_out)):
                    self.assertEqual(state_of(ask()), harness.reading.UNREADABLE,
                                     "a git call that started and ran out of time reported "
                                     + what + " as a question that could not be asked")
                with mock.patch.object(harness.subprocess, "run", raising(could_not_start)):
                    self.assertEqual(state_of(ask()), harness.reading.ACCESS_ERROR,
                                     "a git call that could not be started reported " + what
                                     + " as a reading that was made and could not be read")

    def test_a_new_consumption_site_that_treats_it_as_a_value_is_revealed(self):
        """A new site, written here the careless way, and driven. Measured rather than claimed.

        This IS the new consumption site the issue is about: nothing below asks whether the
        reading was taken, which is how the twenty-one closed ones were written before CRW-68
        closed them one at a time. Each must either break or report an absence, and none of them
        may report that something is there.
        """
        careless = (
            ("compares it with the answer it expected", lambda v: v == "resolved"),
            ("asks whether anything is there", lambda v: bool(v)),
            ("puts it in a set beside a real answer", lambda v: {v, "resolved"}),
            ("formats it into a message", lambda v: "the file is " + str(v)),
            ("uses it as a key", lambda v: {v: 1}),
            ("tests it against a tuple of answers", lambda v: v in ("resolved", "reserved")),
            ("sorts it beside a real answer", lambda v: sorted([v, "resolved"])),
            ("writes it into a document", lambda v: json.dumps({"value": v})),
        )
        for how, site in careless:
            with self.subTest(site=how):
                try:
                    answered = site(unread())
                except Exception:
                    continue
                self.assertFalse(
                    answered,
                    "a new consumption site that " + how + " got an answer out of a reading that"
                    " was not taken, so this site has to remember to exclude it and a site that"
                    " forgets reopens CRW-68")


# Positions where a reading that was not taken changes no verdict, because nothing judges that
# cell there. judge() iterates the cells a scenario declared an expectation for, and neither of
# these appears in a scenario row or in any measure. Declared rather than skipped: a cell that
# stops being judged, or a new one nothing judges, has to be written down here. Giving them a
# verdict would be adjudication, which CRW-103 excludes - it is CRW-68's class, a reading nobody
# judges, and it is reported as a follow-up rather than closed here.
UNJUDGED_CELLS = (("on", "firedCommand"), ("on", "journalElapsedMs"))

# What the sweep below does NOT reach, emitted here rather than left for the reader to assume.
# It drives judge(), measures(), supplemental(), containment(), stability(), document() and the
# writing out. It does not drive read(), fire(), relay() or an install, so a producer that stops
# handing over a reading that was not taken is not visible from here; the cases above drive read()
# directly and the boundary cases drive the rest. It also reproduces compare()'s arm verdict
# rather than calling it, because compare() cannot be driven without a run.
SWEEP_DOES_NOT_REACH = (
    "read(), which is driven directly by the reading cases",
    "fire(), relay() and the installs, which need a run",
    "compare(), whose arm verdict is reproduced here because it cannot be driven without a run",
)


class _SyntheticArm(object):
    """The name is all a verdict reads off an arm."""

    def __init__(self, name):
        self.name = name


def _declared(name):
    for one in harness.SCENARIOS:
        if one["name"] == name:
            return one
    raise KeyError(name)


def _expectations(declared):
    found = [declared["expected"]]
    if declared["fireTwice"]:
        found.append(declared["second"])
    return found


def _disagrees(value, wanted):
    """Whether this reading failed to answer exactly this, asked without comparing a non-value.

    Written against the shape rather than against a class so it reads the same before and after
    the representation changed, which is what lets this whole sweep run at either commit.
    """
    if callable(getattr(value, "rendered", None)):
        return True
    return value != wanted


def _positions():
    """Every place a cell value sits in a document, from this module's own inventories."""
    found = [("_arms", arm, 0, cell) for arm in ARMS for cell in ARM_CELLS]
    for name in SCENARIOS:
        for arm in ARMS:
            for index in range(FIRINGS[name]):
                found.extend((name, arm, index, cell) for cell in FIRING_CELLS)
    return tuple(found)


def _answers(declared, expected, arm, index):
    """What a healthy arm reads in every firing cell, from the expectations declared in advance."""
    if arm == "off":
        return dict((cell, "ABSENT") for cell in FIRING_CELLS)
    values = {"adapterOutcome": "guard_answered",
              "firedCommand": "python3 completion_hook.py settings.json",
              "journalElapsedMs": 5, "processWallMs": 9}
    for cell, wanted in expected.items():
        if cell == "recordedAs":
            values[cell] = ("hook/" + declared["name"] + "/" + str(index)
                            if wanted == "published" else None)
        else:
            values[cell] = wanted
    return values


def _cells(values, detail=None, replaced=None):
    built = {}
    for cell, value in values.items():
        answer = {"cell": cell, "value": value, "answeredBy": "synthetic",
                  "readingPath": [], "readable": cell != replaced}
        if cell == replaced:
            answer["detail"] = "a reading this sweep replaced with one that was not taken"
        elif detail:
            answer["detail"] = detail
        built[cell] = answer
    return built


def _build(root, mutate=None):
    """Every cell position a document carries, assembled, with at most one reading replaced.

    Assembled rather than run. The run cases drive the relay, the installs and the firings; what
    this needs is every position at once, so exactly one of them can be handed a reading that was
    not taken. Everything that JUDGES is the harness's own.
    """
    where, replacement = mutate if mutate else (None, None)

    def value_of(place, cell, healthy):
        return replacement if where == place + (cell,) else healthy

    def replaced_in(place):
        return where[3] if where is not None and where[:3] == place else None

    scenarios = {"_arms": {}, "_wrote": [root / "inside"]}
    for arm in ARMS:
        place = ("_arms", arm, 0)
        wanted = harness.ARM_INSTALL[arm]
        values = dict((cell, value_of(place, cell, answer))
                      for cell, answer in wanted.items())
        cells = _cells(values, replaced=replaced_in(place))
        disagreed = [{"cell": cell, "wanted": answer, "found": cells[cell]["value"]}
                     for cell, answer in wanted.items()
                     if _disagrees(cells[cell]["value"], answer)]
        scenarios["_arms"][arm] = dict(
            cells, codexHome=str(root / arm), argv=["synthetic"],
            installed={"passed": not disagreed, "disagreed": disagreed, "wanted": dict(wanted)})

    for name in SCENARIOS:
        declared = _declared(name)
        scenarios[name] = {}
        for arm in ARMS:
            firings = []
            for index, expected in enumerate(_expectations(declared)):
                place = (name, arm, index)
                healthy = _answers(declared, expected, arm, index)
                values = dict((cell, value_of(place, cell, answer))
                              for cell, answer in healthy.items())
                cells = _cells(values,
                               detail=harness.NO_REGISTRATION if arm == "off" else None,
                               replaced=replaced_in(place))
                firings.append({
                    "cells": cells, "expected": dict(expected),
                    "provenance": {"stopPayload": "assembled by this check, never delivered"},
                    "verdict": harness.judge(_SyntheticArm(arm), declared, cells, expected)})
            scenarios[name][arm] = {"built": {"workspace": "w", "session": "s", "turn": "t"},
                                    "firings": firings}
    return scenarios


FIXED_IDENTITY = {"repositoryCommit": "0" * 40, "workingTree": "clean",
                  "sourceDigests": {"harness": "d" * 64}}


def _document(root, mutate=None):
    """One document over those readings, computed by the harness from end to end."""
    with mock.patch.object(harness, "source_identity", lambda: dict(FIXED_IDENTITY)):
        return harness.document(_build(root, mutate), root, dict(FIXED_IDENTITY))


def _failed(body):
    return set(where for where, value in harness.judgments(body) if value is False)


# Every way a judgment says a reading under it was not taken. A judgment that newly fails must
# say so through one of these, because "it answered something else" and "nobody could read what
# it answered" are different findings and only the second is what this sweep injects.
NOT_TAKEN_CHANNELS = ("unreadable", "notTaken", "timingsNotTaken", "digestsNotTaken")


def _carrier(body, where):
    node = body
    for step in where.split("/")[:-1]:
        node = node[int(step)] if isinstance(node, list) else node[step]
    return node


class TheSweepDrivesTheRefusalRatherThanWatchingForItTests(unittest.TestCase):
    """A reading that was not taken, put in every position one can sit in, and the result read.

    Nothing in this suite used to construct one inside a whole document, so the refusal the
    representation rests on would first have fired on a real run - which is the shape this file
    already names elsewhere: a guard that watches a property without ever being driven. Here it
    is driven at every position instead, and what each position does about it is measured.
    """

    @classmethod
    def setUpClass(cls):
        cls.root = Path(tempfile.mkdtemp(prefix="hook-comparison-sweep-"))
        (cls.root / "inside").mkdir(parents=True, exist_ok=True)
        cls.baseline = _document(cls.root)
        cls.baseline_failed = _failed(cls.baseline)

    @classmethod
    def tearDownClass(cls):
        import shutil
        shutil.rmtree(str(cls.root), ignore_errors=True)

    def test_the_assembled_document_without_any_injection_passes(self):
        """The control this whole sweep is measured against. A baseline that already failed
        would make every mutant below agree with it for the wrong reason."""
        self.assertTrue(self.baseline["passed"],
                        "the assembled readings do not describe a healthy run, so a mutant that"
                        " fails proves nothing: " + json.dumps(sorted(self.baseline_failed)))
        self.assertEqual(sorted(self.baseline_failed), [])
        self.assertGreaterEqual(self.baseline["judgmentsCounted"], 9)

    def detected(self, make):
        """Which positions a document notices a replaced reading in, and which it does not."""
        noticed, quiet = [], []
        for place in _positions():
            body = _document(self.root, (place, make("injected at " + "/".join(
                str(one) for one in place))))
            grew = _failed(body) - self.baseline_failed
            (noticed if grew else quiet).append((place, sorted(grew), body))
        return noticed, quiet

    def test_every_position_something_judges_notices_a_reading_that_was_not_taken(self):
        """One position at a time, over all of them, with the verdicts recomputed by the harness.

        Three things are required of every mutant and they are not the same thing. The refusal
        must not escape, or a consumption site somewhere is still treating it as a value. The
        written document must not spell it as a plain answer, or a reader of the JSON inherits
        the defect the type removed. And a judgment that newly fails must say a reading under it
        was not taken, because "it answered something else" is a different finding from "nobody
        could read what it answered" and only the second is what this injects.
        """
        noticed, quiet = self.detected(unread)
        self.assertGreaterEqual(len(noticed) + len(quiet), 200,
                                "almost no position was swept, so this is watching a document"
                                " that stopped carrying readings")
        for place, grew, body in noticed:
            where = "/".join(str(one) for one in place)
            self.assertEqual(bare_sentinels(rendered(body)), [],
                             where + " left a reading that was not taken written into the"
                             " document as an ordinary answer, so reading the JSON reopens the"
                             " defect the type closed")
            channelled = []
            for judgment in grew:
                carrier = _carrier(body, judgment)
                channels = [key for key in NOT_TAKEN_CHANNELS if key in carrier]
                if not channels:
                    continue
                channelled.append(any(carrier[key] for key in channels))
            self.assertNotIn(False, channelled,
                             where + " made a judgment fail while that judgment still reported"
                             " nothing unread, so it failed for being a different answer rather"
                             " than for being a reading nobody took")

    def test_the_positions_nothing_judges_are_the_declared_ones(self):
        """The partition, in both directions, so a new unjudged cell has to be written down."""
        _noticed, quiet = self.detected(unread)
        found = sorted((place[1], place[3]) for place, _grew, _body in quiet)
        wanted = sorted((arm, cell) for name in SCENARIOS for arm in ARMS
                        for _index in range(FIRINGS[name])
                        for cell in FIRING_CELLS if (arm, cell) in UNJUDGED_CELLS)
        self.assertEqual(found, wanted,
                         "the positions where a reading that was not taken changes no verdict"
                         " and the positions declared unjudged disagree. Say what the new one is,"
                         " or judge it.")
        self.assertTrue(SWEEP_DOES_NOT_REACH, "the sweep must declare what it does not reach")

    def test_the_sweep_stops_noticing_when_the_historical_spelling_is_injected_instead(self):
        """The sensitivity half. Without it this sweep could be passing on nothing at all.

        The control is the defect replayed rather than the enforcement disarmed. Disarming it
        would make every consumer refuse and the sweep would fail by exception, which proves the
        refusal and says nothing about the sweep. Injecting the non-empty string the sentinel
        used to be proves the sweep is sensitive to exactly the thing that changed: the string
        still disagrees with an expected answer nearly everywhere, and at recordedAs it satisfies
        the one question the row asks - a path was named - without naming one, which is how it
        escaped in CRW-68.
        """
        typed = set(place for place, _grew, _body in self.detected(unread)[0])
        historical = set(place for place, _grew, _body in self.detected(lambda why: "UNREADABLE")[0])
        self.assertTrue(historical, "the control noticed nothing anywhere, so it is not a"
                                    " control over this sweep at all")
        missed = typed - historical
        self.assertTrue(missed,
                        "the sweep notices the historical spelling in exactly the places it"
                        " notices the type, so it is not measuring the change")
        self.assertEqual(sorted(set(historical) - set(typed)), [],
                         "the historical spelling is noticed somewhere the type is not")
        recorded = sorted(place for place in missed if place[1] == "on"
                          and place[3] == "recordedAs")
        self.assertTrue(recorded,
                        "recordedAs is the case CRW-68 records, and the control did not"
                        " reproduce it: " + json.dumps(sorted(str(one) for one in missed)))

    def test_two_digests_that_could_not_be_taken_do_not_report_the_bytes_as_identified(self):
        """Driven apart from the cells, because it is the one judgment that compares two ends.

        While the answer was a sentinel string it was the SAME string at both ends, so a source
        nobody could digest compared equal to itself and the run reported its bytes as
        identified. A reading that was not taken is now a fresh object per reading, so the two
        ends cannot answer equal.
        """
        # The comparison itself first, because the judgment below is only sound while this is
        # true: two ends that answered the same sentinel string compared equal, and a mapping
        # comparison is what stability() actually makes.
        try:
            same = {"relay": unread("at the start")} == {"relay": unread("at the end")}
        except Exception:
            same = False
        self.assertFalse(same,
                         "two readings that were not taken compare equal to each other, so a"
                         " source nobody could digest reports its bytes as identified")
        both = harness.stability({"sourceDigests": {"relay": unread("at the start")}},
                                 {"sourceDigests": {"relay": unread("at the end")}})
        self.assertFalse(both["met"],
                         "two digests nobody could take reported the source as unchanged")
        self.assertEqual(both["digestsNotTaken"], 2)
        one_end = harness.stability({"sourceDigests": {"relay": "a" * 64}},
                                    {"sourceDigests": {"relay": unread("at the end")}})
        self.assertFalse(one_end["met"])
        self.assertEqual(one_end["digestsNotTaken"], 1)
        healthy = harness.stability({"sourceDigests": {"relay": "a" * 64}},
                                    {"sourceDigests": {"relay": "a" * 64}})
        self.assertTrue(healthy["met"], "an unchanged source has to still be able to pass")

    def test_a_place_that_could_not_be_resolved_is_not_reported_as_inside_the_root(self):
        """The containment judgment, driven with an answer it could not take."""
        answer = harness.containment(self.root, [self.root / "inside"])
        self.assertTrue(answer["met"])
        with mock.patch.object(harness, "owned",
                               lambda place, root: unread("this path could not be resolved")):
            refused = harness.containment(self.root, [self.root / "inside"])
        self.assertFalse(refused["met"],
                         "a run reported that it wrote only inside its own directory on a path"
                         " nobody could resolve")
        self.assertEqual(len(refused["notTaken"]), 1)
        self.assertEqual(refused["outside"], [],
                         "a path that could not be resolved was reported as leading outside,"
                         " which names the wrong repair and claims to know where it led")


# Support, not evidence for a criterion. These are drift guards: they keep the inventories above
# honest as the file changes, and none of them measures the behaviour the issue is about.

# Every function that hands over a reading that was not taken, with the state each one gives it,
# derived below and compared with this. The state is here and not just the count because the two
# states are not interchangeable: an access error means the question could not be asked at all and
# an unreadable one means it was asked and the answer could not be read, and review found this
# change collapsing the first into the second at the one door every cell goes through. A producer
# that is added, or that starts answering with the other state, fails here until somebody says so.
CARRIED = "carried from the reading it rebuilds"
UNREADABLE_PRODUCERS = {
    "_unreadable": (CARRIED,),
    "_install": ("UNREADABLE", "ACCESS_ERROR"),
    "journal_payload": ("UNREADABLE",),
    "_faulted": (CARRIED,),
    "_there": (CARRIED,),
    "working_tree": ("UNREADABLE", "ACCESS_ERROR", "UNREADABLE"),
    "_digest": ("UNREADABLE", "ACCESS_ERROR"),
    "repository_commit": ("UNREADABLE", "ACCESS_ERROR", "UNREADABLE"),
    "owned": ("ACCESS_ERROR",),
}

# Every function that consumes a cell's value, derived below and compared with this.
CELL_CONSUMERS = ("judge", "_reported", "_detection", "measures", "supplemental", "compare")

# What these two derivations cannot see, as data rather than as a claim of completeness.
DERIVATION_BLIND_SPOTS = (
    "matching is by name, so a local called Unreadable would count and a producer reached"
    " through an alias would not",
    "only scripts/hook_comparison.py is read; a consumer in another file that imports the"
    " harness is outside this reach",
    "a value bound to another name and consumed in a different function is not followed",
    "a consumer that reads the printed JSON rather than a cell is not visible at all",
)


def _harness_tree():
    return ast.parse((ROOT / "scripts" / "hook_comparison.py").read_text(encoding="utf-8"))


def _state_named(call):
    """Which of the four answers this producer gives, read off the call rather than assumed."""
    for keyword in call.keywords:
        if keyword.arg == "state":
            # Only a state named on the reading module is a state this reader can name. An
            # attribute of anything else is a value carried from elsewhere, and reporting its
            # attribute name would put a local variable's name into the declared table.
            named = keyword.value
            if (isinstance(named, ast.Attribute) and isinstance(named.value, ast.Name)
                    and named.value.id == "reading"):
                return named.attr
            return CARRIED
    return "UNREADABLE"


def _owners(tree):
    parents, owner = {}, {}
    for node in ast.walk(tree):
        for child in ast.iter_child_nodes(node):
            parents[child] = node
    for node in ast.walk(tree):
        if isinstance(node, ast.FunctionDef):
            for child in ast.walk(node):
                owner.setdefault(child, node.name)
    return parents, owner


class TheReadFailureInventoriesAreDerivedTests(unittest.TestCase):
    """Support. The two inventories the sweep rests on, produced from source rather than counted.

    A sentence naming how many producers or consumers were migrated is the thing that goes stale,
    and this file's own history is checks whose reach was narrower than the property they claimed.
    """

    def test_every_producer_of_a_reading_that_was_not_taken_is_declared(self):
        tree = _harness_tree()
        _parents, owner = _owners(tree)
        found = {}
        for node in ast.walk(tree):
            if (isinstance(node, ast.Call) and isinstance(node.func, ast.Name)
                    and node.func.id == "Unreadable"):
                where = owner.get(node, "<module>")
                found.setdefault(where, []).append((node.lineno, _state_named(node)))
        # By line, because ast.walk is breadth first and the order it yields siblings in is not
        # the order they are written in, which would make this table describe nothing.
        self.assertEqual(dict((name, tuple(state for _line, state in sorted(states)))
                              for name, states in found.items()),
                         dict(UNREADABLE_PRODUCERS),
                         "the places that hand over a reading that was not taken and the places"
                         " written down disagree, in which function or in which state. Declare"
                         " the new one, say which of the two answers it gives, and say whether"
                         " the sweep reaches it.")
        self.assertTrue(DERIVATION_BLIND_SPOTS,
                        "a derivation that declares no blind spot is claiming completeness")

    def test_every_function_that_consumes_a_cell_value_is_declared(self):
        tree = _harness_tree()
        _parents, owner = _owners(tree)
        found = set()
        for node in ast.walk(tree):
            if (isinstance(node, ast.Subscript) and isinstance(node.slice, ast.Constant)
                    and node.slice.value == "value"):
                found.add(owner.get(node, "<module>"))
        self.assertEqual(sorted(found), sorted(CELL_CONSUMERS),
                         "a function reads a cell's value that this check does not name. It is a"
                         " consumption site: show by mutation that it treats a reading nobody"
                         " took as one nobody took, then declare it.")

    def test_no_value_slot_in_the_harness_still_carries_the_state_as_text(self):
        """The class this issue closes, swept over the file rather than over the sites it knew.

        The predicate is the shape of the defect: one of the two states written into something a
        value is read out of. It is reported for this file only, and the PR names what the same
        predicate still matches elsewhere.
        """
        tree = _harness_tree()
        offenders = []
        for node in ast.walk(tree):
            if isinstance(node, ast.Return) and isinstance(node.value, ast.Attribute):
                if node.value.attr in ("UNREADABLE", "ACCESS_ERROR"):
                    offenders.append("return at line " + str(node.lineno))
            if isinstance(node, ast.Dict):
                for key, value in zip(node.keys, node.values):
                    named = isinstance(key, ast.Constant) and key.value in (
                        "value", "exitCode", "adapterOutcome", "workingTree")
                    if named and isinstance(value, ast.Attribute) and value.attr in (
                            "UNREADABLE", "ACCESS_ERROR"):
                        offenders.append("value slot at line " + str(node.lineno))
        self.assertEqual(offenders, [],
                         "a reading that was not taken is written into a value slot as text: "
                         + "; ".join(offenders))


class TheVerdictGuardRunsBeforeThePredicateTests(unittest.TestCase):
    """Support. The ordering the guard now carries, driven rather than read off the source."""

    def test_a_predicate_is_not_run_at_all_while_a_reading_under_it_was_not_taken(self):
        ran = []

        def would_raise():
            ran.append(True)
            raise AssertionError("the predicate ran while a reading under it was not taken")

        self.assertFalse(harness.judged(would_raise, ["one/cell"]))
        self.assertEqual(ran, [], "the guard ran the predicate before checking the count")
        self.assertTrue(harness.judged(lambda: True, 0))
        self.assertFalse(harness.judged(lambda: False, 0))

    def test_an_uncalled_predicate_no_longer_reads_as_a_true_verdict(self):
        """A function object is truthy, so the old shape answered true for one passed by mistake."""
        self.assertFalse(harness.judged(lambda: False, []))

    def test_the_document_is_written_out_in_one_piece(self):
        """Support. Anything the encoder cannot write must fail before a byte reaches stdout.

        json.dump streams, so a value it cannot encode leaves a truncated document behind and
        then raises - and one JSON object on stdout is the one thing this command promises.
        """
        tree = _harness_tree()
        streamed = [node.lineno for node in ast.walk(tree)
                    if isinstance(node, ast.Call) and isinstance(node.func, ast.Attribute)
                    and node.func.attr == "dump" and isinstance(node.func.value, ast.Name)
                    and node.func.value.id == "json"]
        self.assertEqual(streamed, [],
                         "the document is streamed to stdout, so a value that cannot be written"
                         " leaves a partial one behind: line " + str(streamed))
        written = harness.render(unread("nothing could be read here"))
        self.assertEqual(sorted(written), ["notRead", "why"])
        self.assertEqual(written["notRead"], harness.reading.UNREADABLE)
        self.assertNotIsInstance(written, str,
                                 "the written form is a plain answer again, so a reader of the"
                                 " JSON inherits the defect the type removed")

# Every operation that must refuse, and how a consumption site would reach it. Driven rather than
# listed as present: a method that stops refusing is invisible to a check that only looks it up.
# __bool__ is the one that matters most and the reason this table exists. Delete it and an object
# is truthy by default, so "is there something there" answers yes again - which is exactly the
# question CRW-68's sentinel escaped. __str__ is the same shape: object.__str__ falls back to
# __repr__, a non-empty string, so formatting a reading that was not taken into a message would
# quietly succeed.
REFUSING_USES = {
    "__eq__": lambda value: value == "resolved",
    "__ne__": lambda value: value != "resolved",
    "__lt__": lambda value: value < "resolved",
    "__le__": lambda value: value <= "resolved",
    "__gt__": lambda value: value > "resolved",
    "__ge__": lambda value: value >= "resolved",
    "__bool__": lambda value: bool(value),
    "__hash__": lambda value: hash(value),
    "__str__": lambda value: str(value),
    "__format__": lambda value: format(value),
    "__len__": lambda value: len(value),
    "__iter__": lambda value: list(value),
    "__contains__": lambda value: "resolved" in value,
    "__getitem__": lambda value: value[0],
}

# What the type may answer rather than refuse, each with the reason it is safe. A method that is
# neither driven above nor excused here fails the partition, which is what stops one being added
# that quietly answers a question only a value can answer.
ANSWERING_METHODS = {
    "__init__": "builds one",
    "__repr__": "a traceback and a failing assertion still have to be able to name which reading"
                " failed and why, and repr is never mistaken for a value",
    "rendered": "the written form, which is an object and therefore cannot equal a cell's answer",
    "_refuse": "the refusal itself",
}


class TheRefusalsAreAnInventoryTests(unittest.TestCase):
    """The enforcement rests on a set of methods, and nothing used to check that set.

    The producers and the functions that consume a cell are both derived and pinned. The refusals
    were not, so deleting one was silent - and two of them are silent in the worst way, because
    what replaces them is a default that answers rather than raises.
    """

    def test_every_operation_that_must_refuse_actually_refuses(self):
        """Driven against what read() answers with, not against a class this names.

        That is what keeps it honest across the change: at the commit this replaces, read()
        answers with a string, the string answers every one of these, and the case fails on the
        property rather than on a name that does not exist there yet.
        """
        for name, use in sorted(REFUSING_USES.items()):
            with self.subTest(operation=name):
                try:
                    answered = use(unread())
                except Exception:
                    continue
                self.fail(name + " answered " + repr(answered)[:60] + " instead of refusing, so a"
                          " consumption site reaching a reading that was not taken through it"
                          " gets an answer only a value should be able to give")

    def test_the_partition_covers_every_method_the_type_defines(self):
        """Support. A method added without being classified, or removed, fails here.

        Asked of the type read() hands back rather than of a name, so it moves with the
        representation instead of describing one.
        """
        defined = set(name for name, value in vars(type(unread())).items() if callable(value))
        unclassified = sorted(defined - set(REFUSING_USES) - set(ANSWERING_METHODS))
        self.assertEqual(unclassified, [],
                         "a method is defined that is neither driven as a refusal nor excused as"
                         " one that may answer. Say which it is: " + ", ".join(unclassified))
        missing = sorted(set(REFUSING_USES) - defined)
        self.assertEqual(missing, [],
                         "an operation this suite drives is no longer defined on the type, so"
                         " whatever Python does by default answers it instead: "
                         + ", ".join(missing))
        for name, why in sorted(ANSWERING_METHODS.items()):
            self.assertTrue(str(why).strip(), name + " is excused without a reason")


    def test_a_document_that_cannot_be_written_is_still_one_json_object(self):
        """Support. The promise is one JSON object on stdout, including when writing fails.

        Composing the whole string before writing any of it stops a truncated document, and it
        does not stop the other half: this runs outside the handler that turns a failure into a
        refusal, so anything the encoder rejects would end the command with a traceback and an
        empty stdout. A document that cannot be written is a result too.
        """
        import contextlib
        import io

        printed = io.StringIO()
        with contextlib.redirect_stdout(printed):
            harness._print({"source": "hook-comparison", "passed": True,
                            "cells": {"x": {"value": object()}}})
        answer = json.loads(printed.getvalue())
        self.assertIn("refused", answer, "a document that could not be written printed no reason")
        self.assertEqual(answer.get("source"), "hook-comparison")
        self.assertNotIn("cells", answer, "a refusal must carry no rows")

    def test_a_result_that_could_not_be_written_exits_as_the_refusal_it_printed(self):
        """The status has to come from the document that was written, not the one handed over.

        A refusal exits 2 everywhere else in this command. Taking the status from the original
        answer instead printed a refusal and exited zero, so anything reading the status would
        accept a run whose own output says it was refused. Driven through main() rather than
        through the writer, because the status is what is being asserted.
        """
        import contextlib
        import io

        unwritable = {"source": "hook-comparison", "passed": True, "cells": {"x": object()}}
        printed, code, raised = io.StringIO(), None, None
        with mock.patch.object(harness, "RELAY_PYTHON", (3, 0)), \
                mock.patch.object(harness, "compare", lambda root: {}), \
                mock.patch.object(harness, "source_identity", lambda: {}), \
                mock.patch.object(harness, "document",
                                  lambda scenarios, root, earlier: unwritable):
            try:
                with contextlib.redirect_stdout(printed):
                    code = harness.main([])
            except BaseException as error:
                raised = error
        self.assertIsNone(raised,
                          "the command ended with " + type(raised).__name__ + " instead of"
                          " printing one document, which is the one output it promises")
        answer = json.loads(printed.getvalue())
        self.assertIn("refused", answer)
        self.assertEqual(code, 2,
                         "a run whose result could not be written printed a refusal and exited "
                         + str(code) + ", so anything reading the status accepts it")

if __name__ == "__main__":
    unittest.main()
