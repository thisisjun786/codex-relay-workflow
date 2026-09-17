"""The class property: every guard evaluation yields a recorded, classified result.

Seven findings across six review rounds were one defect wearing different clothes - a failure path
that loses its result instead of reporting it. Patching them one at a time produced another round of
the same shape each time, so this suite establishes the property instead.

> Every guard evaluation produces a recorded and classified result. It does not end in an uncaught
> exception, does not read a failure as a normal state, and does not silently skip enforcement.

The inventory is the evaluation's outside-world STAGES, not its failure sites. "Every place that can
fail" cannot be enumerated; the stages of one evaluation can, they are few, and they are stable.
They are declared as guard.EVALUATION_STAGES and this suite fails if one of them has no case, so a
stage added later enters the denominator whether or not anyone remembers.

What this establishes: a failure does not vanish without a record. What it does NOT establish: that
every failure is classified correctly. A read that could have succeeded may still be reported as
unreadable. That residual is in the conservative direction and the classification carries its
reason, so it is reportable rather than silent.
"""

import json
import os
import shutil
import tempfile
import unittest
from pathlib import Path
from unittest import mock

from codex_session_relay import guard, intent, marker

from .support import CHILD, DISPATCH_TURN
from .test_guard import DISPATCH, LATER, NOW, GuardTestCase

# One injection per declared stage, naming the function that stage reaches the outside world
# through. Each raises something the stage's own guards do not anticipate, because the point is the
# boundary rather than the inner handling.
# "declared" marks a stage that is only reached once the turn has declared a readiness, because the
# receipt is deliberately not read for a turn that declared itself waiting or interrupted.
STAGE_INJECTIONS = {
    "workspace_listing": ("codex_session_relay.intent._list_assignments", OSError, False),
    # Patched where intent BINDS the name, not where marker defines it: intent imported it directly,
    # so patching the definition site would leave the caller holding the original.
    "assignment_facts": ("codex_session_relay.intent.read_assignment", RuntimeError, False),
    "disposition": ("codex_session_relay.guard.read_disposition", RuntimeError, False),
    "receipt_store": ("codex_session_relay.guard.lookup_receipt", TypeError, True),
    "hold_history": ("codex_session_relay.guard.hold_counters", RuntimeError, False),
    "observation_record": ("codex_session_relay.guard.record_observation", OSError, False),
}

CLASSIFICATIONS = {
    "unmanaged", "dispatch_uncorrelated", "correlated_unbound", "bound_identity_unnamed",
    "marker_claimed_by_other_session", "marker_unclaimed", "marker_malformed", "state_unreadable",
    "managed_unregistered", "receipt_missing", "undeclared_turn_end", "declared_ready_receipted",
    "declared_in_progress", "declared_blocked_needs_input", "declared_interrupted",
    "declared_failed", guard.FAULTED,
}


class Inventory(unittest.TestCase):
    def test_every_injection_names_a_real_target(self):
        for stage, (target, error, _declared) in STAGE_INJECTIONS.items():
            module, _, attribute = target.rpartition(".")
            with self.subTest(stage=stage):
                imported = __import__(module, fromlist=[attribute])
                self.assertTrue(hasattr(imported, attribute), target)
                self.assertTrue(issubclass(error, Exception))

    def test_every_declared_stage_has_an_injection(self):
        """The denominator is the declared inventory, so a new stage cannot go untested quietly."""
        self.assertEqual(set(guard.EVALUATION_STAGES), set(STAGE_INJECTIONS))

    def test_the_inventory_is_not_empty(self):
        self.assertTrue(guard.EVALUATION_STAGES)


class NothingEscapes(GuardTestCase):
    """An unanticipated exception at any stage becomes a named result, never a traceback."""

    def test_each_stage_yields_a_classified_release_instead_of_raising(self):
        for stage in guard.EVALUATION_STAGES:
            target, error, declared = STAGE_INJECTIONS[stage]
            with self.subTest(stage=stage):
                relationship = self.managed()
                if declared:
                    self.emit_ready(relationship)
                    self.dispose("ready_for_review")
                with mock.patch(target, side_effect=error("injected at " + stage)):
                    verdict = guard.evaluate(
                        self.markers, self.stop(), now=LATER, mode=guard.HOLD
                    )
                self.assertEqual(verdict["observation"], guard.FAULTED, stage)
                self.assertEqual(verdict["decision"], guard.RELEASE, stage)
                self.assertEqual(verdict["hook_output"], {}, stage)
                # The fault names what went wrong, so a defect here cannot be mistaken for a data
                # problem in somebody's marker.
                self.assertIn(error.__name__, verdict["fault"], stage)
                self.assertIn("injected at " + stage, verdict["fault"], stage)
                self.assertFalse(verdict["record"]["held"], stage)

    def test_a_fault_is_recorded_whenever_the_assignment_was_reachable(self):
        """Detection does not depend on the evaluation succeeding."""
        relationship = self.managed()
        self.emit_ready(relationship)
        self.dispose("ready_for_review")
        with mock.patch(
            "codex_session_relay.guard.lookup_receipt", side_effect=TypeError("injected")
        ):
            verdict = guard.evaluate(self.markers, self.stop(), now=LATER, mode=guard.HOLD)
        self.assertEqual(verdict["observation"], guard.FAULTED)
        self.assertTrue(verdict["recordedAs"].startswith("hook/"))
        directory = marker.assignment_dir(self.markers, self.workspace, self.assignment)
        published = sorted((directory / "hook" / CHILD / DISPATCH_TURN).glob("*.json"))
        self.assertEqual(len(published), 1)
        self.assertEqual(
            json.loads(published[0].read_text(encoding="utf-8"))["observation"], guard.FAULTED
        )

    def test_a_fault_while_recording_still_returns_a_classified_result(self):
        """Recording is the last thing that can fail, and failing it must not re-raise."""
        self.managed()
        with mock.patch(
            "codex_session_relay.guard.record_observation", side_effect=OSError("injected")
        ):
            verdict = guard.evaluate(self.markers, self.stop(), now=LATER, mode=guard.HOLD)
        self.assertEqual(verdict["observation"], guard.FAULTED)
        self.assertIsNone(verdict["recordedAs"])

    def test_every_evaluation_returns_a_known_classification(self):
        """Whatever happens, the caller gets a word from the agreed vocabulary."""
        cases = []
        self.managed()
        cases.append(guard.evaluate(self.markers, self.stop(), now=LATER, mode=guard.HOLD))
        cases.append(guard.evaluate(self.markers, self.stop(session_id="stranger"), now=LATER))
        cases.append(guard.evaluate(self.markers, {}, now=LATER))
        cases.append(guard.evaluate(self.markers, {"cwd": str(self.workspace)}, now=LATER))
        for verdict in cases:
            self.assertIn(verdict["observation"], CLASSIFICATIONS)
            self.assertIn(verdict["decision"], (guard.BLOCK, guard.RELEASE))


class FailureIsNotANormalState(GuardTestCase):
    """The second half of the property: a failure is never read as an ordinary condition."""

    def test_an_unlistable_workspace_is_unreadable_not_unmanaged(self):
        blocked = self.markers / marker.workspace_key(self.workspace)
        blocked.parent.mkdir(parents=True, exist_ok=True)
        blocked.write_text("not a directory", encoding="utf-8")
        verdict = self.evaluate()
        self.assertEqual(verdict["observation"], "state_unreadable")
        self.assertIn("workspace", verdict["reason"])

    def test_an_unlistable_fact_directory_is_unreadable_not_absent(self):
        self.managed()
        directory = marker.assignment_dir(self.markers, self.workspace, self.assignment)
        with mock.patch.object(
            Path, "iterdir", side_effect=PermissionError("injected"), autospec=True
        ):
            facts, unreadable = marker.read_assignment(directory)
        self.assertIn("claims", unreadable)

    def test_an_unreadable_single_fact_is_not_an_absent_one(self):
        self.managed()
        directory = marker.assignment_dir(self.markers, self.workspace, self.assignment)
        (directory / "bound.json").unlink()
        (directory / "bound.json").mkdir()
        facts, unreadable = marker.read_assignment(directory)
        self.assertIn("bound", unreadable)
        self.assertNotIn("bound", facts)

    def test_a_corrupt_recorded_database_path_is_malformed_not_a_traceback(self):
        """An intent recording a non-string dbPath used to reach Path() and end the evaluation."""
        relationship = self.register()
        self.declare()
        self.claim()
        self.bind()
        self.register_marker(relationship)
        directory = marker.assignment_dir(self.markers, self.workspace, self.assignment)
        published = json.loads((directory / "intent.json").read_text(encoding="utf-8"))
        published["dbPath"] = ["not", "a", "path"]
        (directory / "intent.json").unlink()
        (directory / "intent.json").write_text(json.dumps(published), encoding="utf-8")
        self.dispose("ready_for_review")
        verdict = self.evaluate()
        self.assertEqual(verdict["observation"], "marker_malformed")
        self.assertEqual(verdict["decision"], guard.RELEASE)
        self.assertTrue(verdict["recordedAs"].startswith("hook/"))


class EnforcementIsNotSkipped(GuardTestCase):
    """The third half: a bound is never lost quietly, and never lost to somebody else's data."""

    def test_a_foreign_sessions_corrupt_record_cannot_disable_this_session(self):
        """The rolling window is explicitly per session, so only this session's records reach it."""
        self.managed()
        # In a RETAINED sibling assignment, which only the per-session window pass walks. A corrupt
        # record inside the selected assignment is a different question: the generation bound is
        # per assignment, so the contract has that one read as marker_malformed.
        sibling = intent.declare_intent(
            self.markers, workspace=self.workspace, dispatch_request_id="a-retained-dispatch",
            issue_key="REL-3", declared_at="2026-01-01T00:00:00+00:00",
        )
        bad = (
            marker.assignment_dir(self.markers, self.workspace, sibling["assignmentId"])
            / "hook" / "some-other-session" / "turn-x"
        )
        bad.mkdir(parents=True)
        (bad / "0.json").write_text('"not a record"', encoding="utf-8")
        verdict = guard.evaluate(
            self.markers, self.stop(), now=LATER, mode=guard.HOLD, record=False
        )
        self.assertEqual(verdict["observation"], "undeclared_turn_end")
        self.assertEqual(verdict["decision"], guard.BLOCK)

    def test_an_uncountable_budget_releases_rather_than_renewing_itself(self):
        """Falling back to one assignment made sibling holds vanish and renewed the window."""
        self.managed()
        other = intent.declare_intent(
            self.markers, workspace=self.workspace, dispatch_request_id="a-sibling-dispatch",
            issue_key="REL-2", declared_at="2026-01-01T00:00:00+00:00",
        )
        other_dir = marker.assignment_dir(self.markers, self.workspace, other["assignmentId"])
        for index in range(guard.MAX_HOLDS_PER_SESSION_WINDOW):
            marker.publish(
                other_dir / "hook" / CHILD / ("spent-" + str(index)) / "0.json",
                {"held": True, "sessionId": CHILD, "turnId": "spent-" + str(index), "at": NOW},
            )
        selected = marker.assignment_dir(self.markers, self.workspace, self.assignment)
        real = guard.listing

        def refuse(path, pattern=None):
            # The workspace listing fails; the selected assignment's own hook tree still reads.
            if Path(path) == Path(self.markers) / marker.workspace_key(self.workspace):
                return [], False
            return real(path, pattern)

        with mock.patch("codex_session_relay.guard.listing", side_effect=refuse):
            counters, corrupt, unreadable = guard.hold_counters(
                selected, session_id=CHILD, turn_id="turn-fresh", now=LATER,
                workspace_root=Path(self.markers) / marker.workspace_key(self.workspace),
            )
        self.assertEqual(unreadable, "workspace")
        self.assertEqual(counters["holdsThisSessionWindow"], 0)
        # And the evaluation that reads it releases instead of spending a budget it never counted.
        with mock.patch(
            "codex_session_relay.guard.hold_counters",
            return_value=({"holdsThisTurn": 0, "holdsThisGeneration": 0,
                           "holdsThisSessionWindow": 0}, None, "workspace"),
        ):
            verdict = guard.evaluate(
                self.markers, self.stop(turn_id="turn-fresh"), now=LATER, mode=guard.HOLD,
            )
        self.assertEqual(verdict["observation"], "state_unreadable")
        self.assertEqual(verdict["decision"], guard.RELEASE)


class Atomicity(unittest.TestCase):
    """A fact is published whole or not at all, which is what create-once is worth."""

    def setUp(self):
        self.tmp = tempfile.mkdtemp(prefix="relay-atomic-")
        self.addCleanup(shutil.rmtree, self.tmp, ignore_errors=True)

    def test_a_short_write_still_publishes_the_whole_fact(self):
        target = Path(self.tmp) / "a" / "intent.json"
        payload = {"issueKey": "REL-1", "filler": "x" * 4096}
        real_write = os.write

        def dribble(fd, data):
            # One byte at a time, which os.write is permitted to do.
            return real_write(fd, data[:1])

        with mock.patch("codex_session_relay.marker.os.write", side_effect=dribble):
            self.assertEqual(marker.publish(target, payload), marker.PUBLISHED)
        self.assertEqual(json.loads(target.read_text(encoding="utf-8")), payload)

    def test_a_write_that_cannot_finish_publishes_nothing(self):
        target = Path(self.tmp) / "a" / "intent.json"
        with mock.patch("codex_session_relay.marker.os.write", return_value=0):
            with self.assertRaises(OSError):
                marker.publish(target, {"issueKey": "REL-1"})
        # No target, so readers see the fact as absent rather than as permanently unreadable.
        self.assertFalse(target.exists())


class CommandSurface(unittest.TestCase):
    def test_the_marker_command_lists_agree(self):
        """Two lists of the same nine commands, one by name and one by handler."""
        from codex_session_relay import cli

        self.assertEqual(len(cli.MARKER_COMMANDS), len(cli.MARKER_COMMANDS_BY_NAME))
        for name in cli.MARKER_COMMANDS_BY_NAME:
            self.assertIn(name, cli.OFFLINE_COMMANDS, name)

    def test_marker_commands_are_exempt_from_the_store_selection_refusal(self):
        """The marker exists so a hook can answer without asking the relay anything."""
        from argparse import Namespace
        from codex_session_relay import cli

        for handler in cli.MARKER_COMMANDS:
            args = Namespace(handler=handler, no_db_path=False)
            expected = handler is not cli.cmd_intent_declare
            self.assertIs(cli._reads_no_selected_store(args), expected, handler.__name__)
        # intent-declare records the resolved path, so it stays guarded unless it records none.
        self.assertTrue(
            cli._reads_no_selected_store(
                Namespace(handler=cli.cmd_intent_declare, no_db_path=True)
            )
        )
        self.assertFalse(cli._reads_no_selected_store(Namespace(handler=cli.cmd_emit)))


if __name__ == "__main__":
    unittest.main()

