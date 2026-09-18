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

ARMS = ("off", "on")

SCENARIOS = ("receipt_missing", "managed_unregistered", "undeclared_turn_end",
             "declared_ready_receipted", "declared_in_progress", "declared_blocked_needs_input",
             "declared_interrupted", "unmanaged", "cxc_concurrent", "duplicate")

ARM_CELLS = ("installExit", "installResult", "installSettings", "registration",
             "foreignRegistration")

FIRING_CELLS = ("firedCommand", "adapterOutcome", "observation", "guardDecision", "guardState",
                "printedBlock", "recordedAs", "observationFile", "heldFile", "journalElapsedMs",
                "processWallMs")

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
        self.assertTrue(answer.get("everyRowPassed"),
                        "rows disagreed: " + json.dumps(answer.get("rowsThatDisagreed")))

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
        for arm in ARMS:
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
        home = _RUN["root"] / "on" / "codex"
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
            hooks.read(_RUN["root"] / "on" / "codex" / "hooks.json").value or {},
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
            "harness": {"source": "harness", "wallMs": 11},
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
                    self.assertEqual(found["value"], harness.reading.UNREADABLE,
                                     cell + " answered although " + withheld + " did not")
                    self.assertIn(withheld, found["detail"])
                else:
                    self.assertNotEqual(found["value"], harness.reading.UNREADABLE,
                                        cell + " went unreadable because " + withheld
                                        + " was withheld, which is a question it was not asked")

    def test_a_missing_key_answers_unreadable_and_says_which_key(self):
        every = self.payloads()
        del every["journal"]["observation"]
        found = harness.read("observation", every)
        self.assertEqual(found["value"], harness.reading.UNREADABLE)
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
        self.assertEqual(harness.read("firedCommand", undeclared)["value"],
                         harness.reading.UNREADABLE,
                         "a key missing without a declared reason is a failed reading, not an"
                         " absence")

    def test_a_payload_from_another_source_cannot_fill_this_cell(self):
        every = self.payloads()
        every["journal"] = dict(every["journal"], source="stdout")
        found = harness.read("observation", every)
        self.assertEqual(found["value"], harness.reading.UNREADABLE)
        self.assertIn("stamp", found["detail"])

    def test_a_producer_that_reported_itself_unreadable_stays_unreadable(self):
        every = self.payloads()
        every["journal"]["observation"] = harness.reading.UNREADABLE
        every["journal"]["detail"] = "the record could not be parsed"
        found = harness.read("observation", every)
        self.assertEqual(found["value"], harness.reading.UNREADABLE)
        self.assertIn("could not be parsed", found["detail"])

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
        self.assertEqual(harness._there(loop, "present", "missing"), harness.reading.UNREADABLE)
        self.assertEqual(harness._there(root / "nope", "present", "missing"), "missing")
        self.assertEqual(harness._there(real, "present", "missing"), "present")


class VerdictTests(unittest.TestCase):
    """A row's assertion has to be able to fail, and to fail for the right reason."""

    def cells(self, **overrides):
        values = {"adapterOutcome": "guard_answered", "observation": "receipt_missing",
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

    def records(self, arm):
        found = []
        for path in sorted((_RUN["root"] / arm / "journal").rglob("*.json")):
            found.append(json.loads(path.read_text(encoding="utf-8")))
        return found

    def test_the_arm_that_registered_nothing_left_no_record_of_a_firing(self):
        run_once()
        self.assertEqual(self.records("off"), [],
                         "the arm whose install was a dry run produced hook journal records, so"
                         " something ran there")
        self.assertEqual(sorted((_RUN["root"] / "off" / "markers").rglob("hook")), [],
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
                           (_RUN["root"] / "on" / "markers").rglob("hook/*/*/*.json")
                           if path.name != "hold.json")
        # Every firing but the unmanaged one, which selects no assignment and so publishes nothing.
        self.assertEqual(len(published), sum(FIRINGS.values()) - 1,
                         "the relay published a different number of observations from the number"
                         " of managed firings")
        reserved = sorted((_RUN["root"] / "on" / "markers").rglob("hook/*/*/hold.json"))
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
        constructed = [value for value in built
                       if isinstance(value, (ast.DictComp, ast.Dict, ast.Call))]
        self.assertTrue(constructed,
                        "nothing constructs a mapping of cells, so this check is watching a file"
                        " that no longer does what it describes")
        for value in constructed:
            self.assertIsInstance(value, ast.DictComp,
                                  "a mapping of cells is built by a literal, a dict() or a helper"
                                  " call rather than by a comprehension over the declared"
                                  " readings, and a helper could stamp the right source onto a"
                                  " value it took from somewhere else")
            self.assertTrue(isinstance(value.value, ast.Call)
                            and isinstance(value.value.func, ast.Name)
                            and value.value.func.id == "read",
                            "a mapping of cells is filled by something other than read()")
        self.assertIn("test_every_cell_written_into_a_row_went_through_the_declared_reading",
                      TEXT_EVIDENCE, "the one text reading has to stay declared")


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


if __name__ == "__main__":
    unittest.main()

