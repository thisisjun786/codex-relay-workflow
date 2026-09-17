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

import ast
import json
import os
import shutil
import sqlite3
import tempfile
import threading
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
                # Its own turn per stage: the marker is create-once, so a disposition published for
                # one stage would otherwise still be standing for the next and change which stage is
                # reached. The declaring stage keeps the anchor turn, because a receipt on any other
                # turn needs an explicit continuation admission that is not what this is testing.
                turn = DISPATCH_TURN if declared else "stage-" + stage
                if declared:
                    self.accept(
                        self.ready_payload(
                            relationship,
                            [self.artifact("out-" + stage + ".txt", stage)],
                            turn=self.assigned_turn("inProgress"),
                        )
                    )
                    self.dispose("ready_for_review")
                with mock.patch(target, side_effect=error("injected at " + stage)):
                    verdict = guard.evaluate(
                        self.markers, self.stop(turn_id=turn), now=LATER, mode=guard.HOLD
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
        with mock.patch(
            "codex_session_relay.marker.os.scandir", side_effect=PermissionError("injected")
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
        (bad / "hold.json").write_text('"not a record"', encoding="utf-8")
        verdict = guard.evaluate(
            self.markers, self.stop(), now=LATER, mode=guard.HOLD, record=True
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
                other_dir / "hook" / CHILD / ("spent-" + str(index)) / "hold.json",
                {"sessionId": CHILD, "turnId": "spent-" + str(index), "at": NOW},
            )
        selected = marker.assignment_dir(self.markers, self.workspace, self.assignment)
        real = guard.listing

        def refuse(path, pattern=None, **kw):
            # The workspace listing fails; the selected assignment's own hook tree still reads.
            if Path(path) == Path(self.markers) / marker.workspace_key(self.workspace):
                return [], False
            return real(path, pattern, **kw)

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
            args = Namespace(handler=handler, no_db_path=False, db_path=None)
            # Two commands reach a store: declare records its path, register confirms against it.
            expected = handler not in (cli.cmd_intent_declare, cli.cmd_intent_register)
            self.assertIs(cli._reads_no_selected_store(args), expected, handler.__name__)
        self.assertTrue(
            cli._reads_no_selected_store(
                Namespace(handler=cli.cmd_intent_register, db_path="/named/relay.sqlite3")
            )
        )
        # intent-declare records the resolved path, so it stays guarded unless it records none.
        self.assertTrue(
            cli._reads_no_selected_store(
                Namespace(handler=cli.cmd_intent_declare, no_db_path=True)
            )
        )
        self.assertFalse(cli._reads_no_selected_store(Namespace(handler=cli.cmd_emit)))


# Inventory A. The set of stdlib predicates that answer a filesystem question by SWALLOWING an
# access error and returning an ordinary value. Enumerable because it is a fixed set of NAMES,
# unlike "every place that can fail" - which is why the property is stated over these.
SWALLOWING_PREDICATES = {
    "exists", "is_dir", "is_file", "is_symlink", "is_mount", "samefile",
    "access", "isdir", "isfile", "islink", "lexists",
    "glob", "rglob", "iterdir", "scandir", "listdir", "walk",
}

# Every call site allowed to use one, with the reason. Anything else is a sibling of the defect
# this inventory exists to catch, and the test below names it.
SWALLOWING_ALLOWED = {
    ("marker.py", "listing"): "the one guarded implementation: it catches the OSError the"
                              " predicates swallow and reports readability to its callers",
    ("cli.py", "cmd_doctor"): "a diagnostic on the constant /proc/self/fd, where False is the right"
                              " answer whether it is absent or unreadable, and no marker path is"
                              " involved",
    ("cli.py", "_refuse_ambiguous_state"): "pre-existing relay state selection, not a marker path;"
                                           " owned by the store-selection surface",
}

OWNED_MODULES = ("marker.py", "intent.py", "guard.py", "cli.py")

# Inventory C: sentinel returns whose PROVENANCE is a failure. Extension A's inventory is a set of
# stdlib predicate NAMES, and a name set structurally cannot see this shape - a function that turns
# its own failure into an ordinary return calls nothing suspicious. The enumerable thing here is the
# return path: a sentinel return reached from an except handler, from "if not <readable flag>", or
# from a loop that ran out of attempts.
#
# Each one is either a classified answer the caller branches on, or it is named here with the
# reason it may lose its failure. A new one that is neither fails this test.
SENTINEL_MODULES = ("marker.py", "intent.py", "guard.py")

FAILURE_SENTINEL_ALLOWED = {
    ("marker.py", "_fsync_directory"): "best effort by design: the link already decided the winner,"
                                       " and failing a publication because a filesystem refuses to"
                                       " open a directory for fsync trades durability for"
                                       " availability",
    ("intent.py", "_moment"): "the failure IS the answer: a value that does not parse as a"
                              " timestamp is not a moment, and every caller treats the absence as"
                              " the malformed field it is",
    ("intent.py", "read_only_connection"): "None is the only thing a failed connection can be, and"
                                           " its callers convert it into their own reported"
                                           " readable=False rather than into an empty result",
}

# Named so the gap is not mistaken for a clean result. The scan classifies by PROVENANCE, so the
# remaining bare sentinel in guard.record_observation does not appear: it answers an identity that
# cannot be a directory name, which is a classification rather than a failure, and its two real
# failure paths now raise. What this scan cannot see is a failure a function converts into a
# classification before returning it.

FAILISH = ("readable", "ok", "success", "published", "written", "reserved")


def _is_sentinel(node):
    """A return that carries nothing beyond 'nothing'."""
    value = node.value
    if value is None:
        return True
    if isinstance(value, ast.Constant) and value.value in (None, False):
        return True
    if isinstance(value, ast.Tuple):
        return all(
            isinstance(element, ast.Constant) and element.value in (None, False, 0)
            for element in value.elts
        )
    return False


def _failure_provenance(function, node):
    """Why this sentinel return is reached: from a caught error, a reported flag, or exhaustion."""
    chain = []

    def walk(parent, path):
        for child in ast.iter_child_nodes(parent):
            if child is node:
                chain.extend(path + [parent])
                return True
            if walk(child, path + [parent]):
                return True
        return False

    walk(function, [])
    kinds = set()
    for ancestor in chain:
        if isinstance(ancestor, ast.ExceptHandler):
            kinds.add("except")
        if isinstance(ancestor, ast.If) and isinstance(ancestor.test, ast.UnaryOp):
            if isinstance(ancestor.test.op, ast.Not):
                rendered = ast.unparse(ancestor.test)
                if any(word in rendered for word in FAILISH):
                    kinds.add("reported-flag")
    body = function.body
    for index, statement in enumerate(body):
        # Only a RETRY loop exhausts. A loop over a collection is a search, and falling out of it
        # is a real answer - "no field is malformed" is not a failure to look at the fields.
        retry = isinstance(statement, ast.While) or (
            isinstance(statement, ast.For)
            and isinstance(statement.iter, ast.Call)
            and isinstance(statement.iter.func, ast.Name)
            and statement.iter.func.id == "range"
        )
        if retry and index + 1 < len(body) and body[index + 1] is node:
            kinds.add("exhausted")
    return sorted(kinds)


def _enclosing_functions(tree):
    """Map every node to the function that contains it, so a call site can be attributed."""
    owner = {}
    for node in ast.walk(tree):
        if isinstance(node, (ast.FunctionDef, ast.AsyncFunctionDef)):
            for child in ast.walk(node):
                owner.setdefault(child, node.name)
    return owner


class FailureProvenanceInventory(unittest.TestCase):
    """Property extension C: a failure path reports its result instead of losing it.

    Two shapes can lose one. A CALLER can ignore a sentinel, and a CALLEE can turn its own failure
    into a sentinel the caller cannot tell apart from "there was nothing to do". Both are counted
    here, because fixing one site and leaving its siblings is what produced three rounds of the
    same finding.
    """

    def modules(self):
        base = Path(__file__).resolve().parent.parent / "src" / "codex_session_relay"
        return {
            name: ast.parse((base / name).read_text(encoding="utf-8"))
            for name in SENTINEL_MODULES
        }

    def helpers(self, trees):
        """Functions that answer with a failure sentinel as well as with something substantive."""
        found = {}
        for name, tree in trees.items():
            for function in ast.walk(tree):
                if not isinstance(function, (ast.FunctionDef, ast.AsyncFunctionDef)):
                    continue
                returns = [n for n in ast.walk(function) if isinstance(n, ast.Return)]
                if any(_is_sentinel(n) for n in returns) and any(
                    not _is_sentinel(n) for n in returns
                ):
                    found[function.name] = name
        return found

    def test_no_caller_discards_a_sentinel_answer(self):
        """A bare expression statement throws the answer away, failure included."""
        trees = self.modules()
        helpers = self.helpers(trees)
        discarded = []
        for name, tree in trees.items():
            for node in ast.walk(tree):
                if not isinstance(node, ast.Expr) or not isinstance(node.value, ast.Call):
                    continue
                func = node.value.func
                called = (
                    func.attr if isinstance(func, ast.Attribute)
                    else func.id if isinstance(func, ast.Name) else None
                )
                if called in helpers:
                    discarded.append((name, node.lineno, called))
        self.assertEqual(discarded, [], "these call sites drop a sentinel answer on the floor")

    def test_no_caller_unpacks_a_readability_flag_and_then_ignores_it(self):
        """(value, readable) only reports anything if the second half is read."""
        trees = self.modules()
        helpers = self.helpers(trees)
        ignored = []
        for name, tree in trees.items():
            for function in ast.walk(tree):
                if not isinstance(function, (ast.FunctionDef, ast.AsyncFunctionDef)):
                    continue
                loaded = {
                    n.id for n in ast.walk(function)
                    if isinstance(n, ast.Name) and isinstance(n.ctx, ast.Load)
                }
                for assignment in ast.walk(function):
                    if not isinstance(assignment, ast.Assign) or len(assignment.targets) != 1:
                        continue
                    target = assignment.targets[0]
                    if not isinstance(target, ast.Tuple) or len(target.elts) != 2:
                        continue
                    if not isinstance(assignment.value, ast.Call):
                        continue
                    func = assignment.value.func
                    called = (
                        func.attr if isinstance(func, ast.Attribute)
                        else func.id if isinstance(func, ast.Name) else None
                    )
                    flag = target.elts[1]
                    if called in helpers and isinstance(flag, ast.Name):
                        if flag.id == "_" or flag.id not in loaded:
                            ignored.append((name, assignment.lineno, called, flag.id))
        self.assertEqual(ignored, [], "these unpack a readability answer and never read it")

    def sentinels(self):
        rows = []
        for name, tree in self.modules().items():
            for function in ast.walk(tree):
                if not isinstance(function, (ast.FunctionDef, ast.AsyncFunctionDef)):
                    continue
                for node in ast.walk(function):
                    if not isinstance(node, ast.Return) or not _is_sentinel(node):
                        continue
                    kinds = _failure_provenance(function, node)
                    if kinds:
                        rows.append((name, function.name, node.lineno, "+".join(kinds)))
        return rows

    def test_every_failure_sentinel_is_a_reported_pair_or_is_justified(self):
        """A sentinel reached from a failure must still tell the caller a failure happened.

        It does that by carrying a readability flag the caller branches on - proved by the two
        tests above - or it is named in FAILURE_SENTINEL_ALLOWED with the reason it may not.
        """
        trees = self.modules()
        unreported = []
        for module, function, line, kinds in self.sentinels():
            if (module, function) in FAILURE_SENTINEL_ALLOWED:
                continue
            node = next(
                f for f in ast.walk(trees[module])
                if isinstance(f, (ast.FunctionDef, ast.AsyncFunctionDef)) and f.name == function
            )
            pairs = [
                n for n in ast.walk(node)
                if isinstance(n, ast.Return) and isinstance(n.value, ast.Tuple)
            ]
            if not pairs:
                unreported.append((module, function, line, kinds))
        self.assertEqual(
            unreported, [],
            "these lose a failure behind a bare sentinel; return it as a reported pair, raise it,"
            " or name it in FAILURE_SENTINEL_ALLOWED with a reason",
        )

    def test_the_inventory_still_finds_the_shape_it_was_built_for(self):
        """A scan that finds nothing proves nothing."""
        found = {(row[0], row[1]) for row in self.sentinels()}
        self.assertIn(("intent.py", "read_only_connection"), found)
        self.assertIn(("guard.py", "lookup_receipt"), found)
        self.assertIn(("guard.py", "_next_hook_seq"), found)

    def test_the_scan_catches_the_shape_this_round_removed(self):
        """The strongest form of "it works": run it against the code as it was before the fix.

        record_observation used to answer an unreadable directory and an exhausted retry loop with
        a bare None, which the caller could not tell apart from "there was nothing to record". The
        scan is fed that exact shape and must flag both.
        """
        before = ast.parse(
            "def record_observation(directory, record):\n"
            "    for _ in range(64):\n"
            "        index, readable = _next_hook_seq(directory)\n"
            "        if not readable:\n"
            "            return None\n"
            "        if publish(index) == PUBLISHED:\n"
            "            return 'hook/0'\n"
            "    return None\n"
        )
        function = before.body[0]
        flagged = [
            _failure_provenance(function, node)
            for node in ast.walk(function)
            if isinstance(node, ast.Return) and _is_sentinel(node)
        ]
        self.assertEqual(sorted(flagged), [["exhausted"], ["reported-flag"]])

    def test_the_twin_that_always_raised_still_does(self):
        """intent._publish_numbered is the same loop; guard.record_observation was the drifted copy.

        Kept as a test because the two must not drift apart again.
        """
        tree = self.modules()["intent.py"]
        publish_numbered = next(
            f for f in ast.walk(tree)
            if isinstance(f, ast.FunctionDef) and f.name == "_publish_numbered"
        )
        self.assertEqual(
            len([n for n in ast.walk(publish_numbered) if isinstance(n, ast.Raise)]), 2
        )


class SwallowingPredicateInventory(unittest.TestCase):
    """Property extension A, as the counting method rather than a list of fixed line numbers.

    Three rounds running ended in one site fixed and its siblings left, because the sibling set was
    never counted. This counts it on every run: a new call to a predicate that hides an access
    error has to be either routed through the guarded primitive or named here with a reason.
    """

    def sites(self):
        base = Path(__file__).resolve().parent.parent / "src" / "codex_session_relay"
        found = []
        for name in OWNED_MODULES:
            source = (base / name).read_text(encoding="utf-8")
            tree = ast.parse(source)
            owner = _enclosing_functions(tree)
            for node in ast.walk(tree):
                if not isinstance(node, ast.Call):
                    continue
                func = node.func
                attribute = (
                    func.attr if isinstance(func, ast.Attribute)
                    else func.id if isinstance(func, ast.Name) else None
                )
                if attribute in SWALLOWING_PREDICATES:
                    found.append((name, owner.get(node, "<module>"), node.lineno, attribute))
        return found

    def test_every_swallowing_predicate_is_routed_or_justified(self):
        unjustified = [
            site for site in self.sites() if (site[0], site[1]) not in SWALLOWING_ALLOWED
        ]
        self.assertEqual(
            unjustified, [],
            "these call sites hide an access error behind an ordinary value; route them through"
            " marker.listing or add them to SWALLOWING_ALLOWED with a reason",
        )

    def test_the_inventory_actually_finds_the_guarded_primitive(self):
        """A scan that finds nothing proves nothing, so assert it still sees the known site."""
        self.assertIn(
            ("marker.py", "listing"), {(site[0], site[1]) for site in self.sites()}
        )


class RecordedPathsAreConfined(unittest.TestCase):
    """Property extension B: a path that came from a record is resolved and confined before use."""

    def setUp(self):
        self.tmp = Path(tempfile.mkdtemp(prefix="relay-confine-"))
        self.addCleanup(shutil.rmtree, self.tmp, ignore_errors=True)

    def test_a_symlinked_parent_cannot_carry_a_write_out_of_the_root(self):
        root = self.tmp / "markers"
        outside = self.tmp / "outside"
        outside.mkdir(parents=True)
        (root / "assignment").mkdir(parents=True)
        os.symlink(outside, root / "assignment" / "claims")
        with self.assertRaises(ValueError):
            marker.publish(
                root / "assignment" / "claims" / "sess" / "claim.json",
                {"sessionId": "sess"},
                root=root,
            )
        self.assertFalse((outside / "sess" / "claim.json").exists())

    def test_confinement_refuses_before_it_creates_anything(self):
        """Checking after mkdir left the write refused and the mutation done.

        A symlinked claims/ pointing outside meant publishing claims/sess/claim.json created the
        external sess/ directory first and only then raised, so a writer with broader filesystem
        access could still mutate paths outside the root despite the refusal.
        """
        root = self.tmp / "markers"
        outside = self.tmp / "outside"
        outside.mkdir(parents=True)
        (root / "assignment").mkdir(parents=True)
        os.symlink(outside, root / "assignment" / "claims")
        with self.assertRaises(ValueError):
            marker.publish(
                root / "assignment" / "claims" / "sess" / "claim.json",
                {"sessionId": "sess"},
                root=root,
            )
        self.assertEqual(list(outside.iterdir()), [])

    def test_confinement_accepts_a_path_that_stays_inside(self):
        root = self.tmp / "markers"
        target = root / "assignment" / "intent.json"
        self.assertEqual(marker.publish(target, {"issueKey": "REL-1"}, root=root), marker.PUBLISHED)

    def test_a_relative_database_path_is_absolutised_rather_than_failing_open(self):
        """Path.as_uri() raises on a relative path, and that was caught as an unreadable store."""
        store = self.tmp / "state" / "relay.sqlite3"
        store.parent.mkdir(parents=True)
        sqlite3.connect(store).close()
        cwd = os.getcwd()
        os.chdir(self.tmp)
        try:
            connection = intent.read_only_connection("state/relay.sqlite3")
        finally:
            os.chdir(cwd)
        self.assertIsNotNone(connection)
        connection.close()

    def test_query_characters_in_a_path_are_not_read_as_uri_parameters(self):
        """A dbPath containing ? or # must name a file, never configure the connection."""
        odd = self.tmp / "relay?mode=rw#x.sqlite3"
        sqlite3.connect(odd).close()
        connection = intent.read_only_connection(str(odd))
        self.assertIsNotNone(connection)
        try:
            # Read-only really is read-only: the URI parameter in the NAME did not become one.
            with self.assertRaises(sqlite3.OperationalError):
                connection.execute("CREATE TABLE t (x)")
        finally:
            connection.close()

    def test_a_missing_store_is_unopenable_rather_than_created(self):
        absent = self.tmp / "nested" / "relay.sqlite3"
        self.assertIsNone(intent.read_only_connection(str(absent)))
        self.assertFalse(absent.parent.exists())


class HoldReservation(GuardTestCase):
    """The per-turn bound is decided by a create-once file, not by a count read beforehand."""

    def test_two_evaluations_that_both_see_an_unspent_budget_produce_one_hold(self):
        """Deterministic: the counters are forced to zero for both, so only the reservation can
        separate them. No sleeping and no thread scheduling is involved."""
        self.managed()
        empty = {"holdsThisTurn": 0, "holdsThisGeneration": 0, "holdsThisSessionWindow": 0}
        with mock.patch(
            "codex_session_relay.guard.hold_counters", return_value=(empty, None, None)
        ):
            first = self.evaluate()
            second = self.evaluate()
        self.assertEqual(first["decision"], guard.BLOCK)
        self.assertEqual(second["decision"], guard.RELEASE)
        self.assertEqual(second["state"], "hold_in_flight")
        # And the omission is still recorded both times: detection never depended on the hold.
        self.assertEqual(second["observation"], "undeclared_turn_end")

    def test_a_real_race_on_one_stop_produces_exactly_one_winner(self):
        self.managed()
        empty = {"holdsThisTurn": 0, "holdsThisGeneration": 0, "holdsThisSessionWindow": 0}
        barrier = threading.Barrier(6)
        decisions = []
        lock = threading.Lock()

        def race():
            barrier.wait()
            verdict = guard.evaluate(
                self.markers, self.stop(), now=LATER, mode=guard.HOLD, record=True
            )
            with lock:
                decisions.append(verdict["decision"])

        with mock.patch(
            "codex_session_relay.guard.hold_counters", return_value=(empty, None, None)
        ):
            threads = [threading.Thread(target=race) for _ in range(6)]
            for thread in threads:
                thread.start()
            for thread in threads:
                thread.join()
        self.assertEqual(decisions.count(guard.BLOCK), 1, decisions)
        self.assertEqual(decisions.count(guard.RELEASE), 5, decisions)

    def test_a_reservation_is_what_the_bounds_count(self):
        self.managed()
        self.evaluate()
        directory = marker.assignment_dir(self.markers, self.workspace, self.assignment)
        reserved = sorted((directory / "hook" / CHILD).glob("*/hold.json"))
        self.assertEqual(len(reserved), 1)
        counters, corrupt, unreadable = guard.hold_counters(
            directory, session_id=CHILD, turn_id=DISPATCH_TURN, now=LATER,
            workspace_root=directory.parent,
        )
        self.assertEqual(counters["holdsThisTurn"], 1)
        self.assertEqual(counters["holdsThisGeneration"], 1)
        self.assertIsNone(corrupt)
        self.assertIsNone(unreadable)

    def test_a_releasing_declaration_is_not_masked_by_unreadable_hold_history(self):
        """User interruption always wins, and it must not be hidden by unrelated accounting.

        Reading the hold history for every turn let an unreadable holds/ tree report
        state_unreadable over a perfectly good declaration and send the coordinator to repair
        something the child never depended on.
        """
        self.managed()
        self.dispose("interrupted")
        with mock.patch(
            "codex_session_relay.guard.hold_counters",
            return_value=({"holdsThisTurn": 0}, None, "holds"),
        ) as counted:
            verdict = self.evaluate()
        self.assertEqual(verdict["observation"], "declared_interrupted")
        self.assertEqual(verdict["decision"], guard.RELEASE)
        # And the budget was never read at all, because no omission was being weighed.
        counted.assert_not_called()

    def test_an_omission_still_reports_an_uncountable_budget(self):
        self.managed()
        with mock.patch(
            "codex_session_relay.guard.hold_counters",
            return_value=({"holdsThisTurn": 0}, None, "holds"),
        ):
            verdict = self.evaluate()
        self.assertEqual(verdict["observation"], "state_unreadable")
        self.assertIn("holds", verdict["reason"])
        self.assertEqual(verdict["decision"], guard.RELEASE)

    def test_a_stop_identity_that_cannot_be_a_path_is_malformed_not_hold_in_flight(self):
        """reserve_hold answers False for an unusable identity too, and treating that as a lost
        race rewrote a holdable omission into a hold that was never in flight."""
        self.managed()
        verdict = self.evaluate(turn_id="../escape")
        self.assertEqual(verdict["observation"], "marker_malformed")
        self.assertNotEqual(verdict["state"], "hold_in_flight")
        self.assertEqual(verdict["decision"], guard.RELEASE)

    def test_a_holding_evaluation_writes_only_inside_the_granted_subtree(self):
        """The contract grants the hook process hook/<own session>/ and nothing else.

        A reservation anywhere outside it is refused by the very sandbox that makes holding
        permissible, and the refusal surfaces as guard_faulted, releasing every omission in exactly
        the configuration hold mode exists for.
        """
        self.managed()
        granted = (
            marker.assignment_dir(self.markers, self.workspace, self.assignment) / "hook" / CHILD
        )
        written = []
        real = marker.publish

        def watched(target, payload, **kw):
            written.append(Path(target))
            return real(target, payload, **kw)

        with mock.patch("codex_session_relay.guard.publish", side_effect=watched):
            verdict = self.evaluate()
        self.assertEqual(verdict["decision"], guard.BLOCK)
        self.assertTrue(written)
        for path in written:
            self.assertTrue(
                path.is_relative_to(granted),
                str(path) + " is outside the granted " + str(granted),
            )

    def test_the_reservation_shares_the_hook_directory_without_colliding(self):
        self.managed()
        self.evaluate()
        self.evaluate()
        directory = marker.assignment_dir(self.markers, self.workspace, self.assignment)
        turn = directory / "hook" / CHILD / DISPATCH_TURN
        self.assertTrue((turn / "hold.json").exists())
        numbered = sorted(p.name for p in turn.glob("*.json") if p.stem.isdigit())
        self.assertEqual(numbered, ["0.json", "1.json"])

    def test_a_failed_recording_gives_the_reservation_back(self):
        """Reserving before recording meant a recording failure spent the turn's only hold with
        nothing blocked, and every later evaluation then read hold_in_flight."""
        self.managed()
        with mock.patch(
            "codex_session_relay.guard.record_observation", side_effect=OSError("injected")
        ):
            faulted = guard.evaluate(self.markers, self.stop(), now=LATER, mode=guard.HOLD)
        self.assertEqual(faulted["observation"], guard.FAULTED)
        directory = marker.assignment_dir(self.markers, self.workspace, self.assignment)
        self.assertFalse((directory / "hook" / CHILD / DISPATCH_TURN / "hold.json").exists())
        # The turn still has its hold, which is the point.
        again = self.evaluate()
        self.assertEqual(again["decision"], guard.BLOCK)

    def test_the_receipt_reads_one_snapshot(self):
        """Three separate SELECTs with no transaction could see the head move underneath them."""
        relationship = self.managed()
        self.emit_ready(relationship)
        self.dispose("ready_for_review")
        opened = []
        real = intent.read_only_connection

        def watched(db_path):
            connection = real(db_path)
            if connection is not None:
                opened.append(connection)
            return connection

        with mock.patch("codex_session_relay.intent.read_only_connection", side_effect=watched):
            verdict = self.evaluate()
        self.assertEqual(verdict["observation"], "declared_ready_receipted")
        # The connection is closed, and closing it after an explicit BEGIN would have raised had
        # the transaction been left open.
        self.assertEqual(len(opened), 1)

    def test_observe_only_never_reserves(self):
        self.managed()
        self.evaluate(mode=guard.OBSERVE)
        directory = marker.assignment_dir(self.markers, self.workspace, self.assignment)
        self.assertEqual(list((directory / "hook" / CHILD).glob("*/hold.json")), [])


class FailedPublicationLeavesNoLitter(unittest.TestCase):
    def setUp(self):
        self.tmp = Path(tempfile.mkdtemp(prefix="relay-litter-"))
        self.addCleanup(shutil.rmtree, self.tmp, ignore_errors=True)

    def test_a_failed_write_removes_its_own_temporary_file(self):
        target = self.tmp / "a" / "intent.json"
        for _ in range(5):
            with mock.patch("codex_session_relay.marker.os.write", return_value=0):
                with self.assertRaises(OSError):
                    marker.publish(target, {"issueKey": "REL-1"})
        self.assertFalse(target.exists())
        # Repeated failures used to pile up one orphan each, in a directory readers have to walk.
        self.assertEqual(list(target.parent.iterdir()), [])

    def test_a_failed_fsync_removes_its_own_temporary_file(self):
        target = self.tmp / "a" / "intent.json"
        with mock.patch("codex_session_relay.marker.os.fsync", side_effect=OSError("injected")):
            with self.assertRaises(OSError):
                marker.publish(target, {"issueKey": "REL-1"})
        self.assertEqual(list(target.parent.iterdir()), [])


if __name__ == "__main__":
    unittest.main()
