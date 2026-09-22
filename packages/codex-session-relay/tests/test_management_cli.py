"""The managed marker driven end to end through the real command line, with no host and no socket.

The lifecycle here is the one the contract orders: the intent is declared BEFORE the task exists,
the real native task id is bound to it afterwards, the relationship is registered against the same
dispatch request, and only then can a Stop be judged against a receipt the relay actually holds.
"""

import json
import os
import shutil
import subprocess
import sys
import tempfile
import unittest

from codex_session_relay import marker

from .support import CHILD, DISPATCH_TURN, RelayTestCase
from .test_cli import REPO, CliBase

DISPATCH = "dispatch-1"


class MarkerCli(CliBase):
    def setUp(self):
        super().setUp()
        self.markers = tempfile.mkdtemp(prefix="relay-marker-cli-")
        self.addCleanup(shutil.rmtree, self.markers, ignore_errors=True)

    def marker_cli(self, *args, stdin=None, expect=0):
        environment = dict(os.environ, PYTHONPATH=os.path.join(REPO, "src"))
        completed = subprocess.run(
            [sys.executable, "-m", "codex_session_relay.cli", "--state", self.tmp, *args],
            capture_output=True, text=True, env=environment, timeout=60, input=stdin,
        )
        self.assertEqual(
            completed.returncode, expect,
            f"exit {completed.returncode}: {completed.stdout}{completed.stderr}",
        )
        return json.loads(completed.stdout)

    def stop_payload(self, **kw):
        payload = {
            "cwd": self.root,
            "session_id": CHILD,
            "turn_id": DISPATCH_TURN,
            "stop_hook_active": False,
            "hook_event_name": "Stop",
            # Delivered by the host and deliberately never read: prose is not evidence.
            "last_assistant_message": "All done, the work is complete.",
        }
        payload.update(kw)
        return json.dumps(payload)

    def declare(self):
        return self.marker_cli(
            "intent-declare", "--marker-root", self.markers, "--workspace", self.root,
            "--dispatch-request-id", DISPATCH, "--issue", "REL-1",
        )

    def evaluate(self, *extra, stdin=None):
        return self.marker_cli(
            "guard-evaluate", "--marker-root", self.markers, *extra,
            stdin=stdin or self.stop_payload(),
        )


class Lifecycle(MarkerCli):
    def test_declare_bind_register_claim_emit_and_then_a_receipted_stop(self):
        declared = self.declare()
        assignment = declared["assignmentId"]
        self.assertEqual(assignment, marker.assignment_id(DISPATCH))
        self.assertEqual(declared["outcome"], marker.PUBLISHED)

        # Before the task exists there is a marker and no relationship anywhere.
        self.assertEqual(
            self.marker_cli("intent-show", "--marker-root", self.markers,
                            "--workspace", self.root)["assignmentState"],
            "intent_declared",
        )

        self.marker_cli(
            "intent-attempt", "--marker-root", self.markers, "--workspace", self.root,
            "--assignment", assignment, "--outcome", "accepted", "--task-id", CHILD,
        )
        bound = self.marker_cli(
            "intent-bind", "--marker-root", self.markers, "--workspace", self.root,
            "--assignment", assignment, "--session", CHILD, "--task-id", CHILD,
        )
        self.assertEqual(bound["outcome"], "bound")

        relationship = self.register()
        self.marker_cli(
            "intent-register", "--marker-root", self.markers, "--workspace", self.root,
            "--assignment", assignment, "--relationship", relationship["relationshipId"],
            "--dispatch-request-id", DISPATCH,
        )
        self.marker_cli(
            "intent-claim", "--marker-root", self.markers, "--workspace", self.root,
            "--assignment", assignment, "--session", CHILD,
            "--dispatch-request-id", DISPATCH, "--first-turn", DISPATCH_TURN,
        )
        shown = self.marker_cli(
            "intent-show", "--marker-root", self.markers, "--workspace", self.root
        )
        self.assertEqual(shown["assignmentState"], "relationship_registered")
        self.assertIs(shown["identityContested"], False)

        # An undeclared turn is the detection, and it is reported before anything is emitted.
        undeclared = self.evaluate("--mode", "hold")
        self.assertEqual(undeclared["observation"], "undeclared_turn_end")
        self.assertEqual(undeclared["decision"], "block")

        path = self.artifact("out.txt", "the deliverable")
        self.marker_cli(
            "emit", "--relationship", relationship["relationshipId"], "--generation", "1",
            "--outcome", "ready_for_review", "--turn-thread", CHILD,
            "--turn-id", DISPATCH_TURN, "--turn-status", "inProgress", "--artifact", path,
        )
        self.marker_cli(
            "intent-disposition", "--marker-root", self.markers, "--workspace", self.root,
            "--assignment", assignment, "--session", CHILD, "--turn", DISPATCH_TURN,
            "--outcome", "ready_for_review",
        )
        settled = self.evaluate("--mode", "hold", "--now", "2026-01-01T00:00:00+00:00")
        self.assertEqual(settled["observation"], "declared_ready_receipted")
        self.assertEqual(settled["decision"], "release")
        self.assertEqual(settled["hook_output"], {})


class Refusals(MarkerCli):
    def test_an_unmanaged_workspace_is_released_and_nothing_is_written(self):
        verdict = self.evaluate()
        self.assertEqual(verdict["observation"], "unmanaged")
        self.assertEqual(verdict["decision"], "release")
        self.assertEqual(os.listdir(self.markers), [])

    def test_intent_show_says_unmanaged_rather_than_failing(self):
        shown = self.marker_cli(
            "intent-show", "--marker-root", self.markers, "--workspace", self.root
        )
        self.assertIs(shown["managed"], False)

    def test_an_explicit_assignment_with_no_intent_reads_unmanaged(self):
        """Selection treats a directory with no published intent as not selectable, so naming it
        explicitly has to read the same way rather than deriving a state out of nothing."""
        shown = self.marker_cli(
            "intent-show", "--marker-root", self.markers, "--workspace", self.root,
            "--assignment", marker.assignment_id("never-declared"),
        )
        self.assertIs(shown["managed"], False)
        self.assertNotIn("assignmentState", shown)

    def test_registering_a_relationship_the_relay_never_opened_is_refused(self):
        """Restating the right dispatch id is not evidence that THIS relationship carries it."""
        declared = self.declare()
        self.register()
        refused = self.marker_cli(
            "intent-register", "--marker-root", self.markers, "--workspace", self.root,
            "--assignment", declared["assignmentId"], "--relationship", "rel-ffffffffffffffff",
            "--dispatch-request-id", DISPATCH, expect=2,
        )
        self.assertEqual(refused["error"], "refused")
        self.assertEqual(refused["reason"], "relationship_conflict")

    def test_registration_is_refused_when_the_relay_cannot_be_confirmed(self):
        """Refusing to claim beats taking the caller's word, so an unreadable store is a refusal."""
        declared = self.declare()
        refused = self.marker_cli(
            "intent-register", "--marker-root", self.markers, "--workspace", self.root,
            "--assignment", declared["assignmentId"], "--relationship", "rel-0123456789abcdef",
            "--dispatch-request-id", DISPATCH,
            "--db-path", os.path.join(self.tmp, "no-such-store.sqlite3"), expect=2,
        )
        self.assertEqual(refused["reason"], "unregistered_relationship")

    def test_a_relationship_from_another_dispatch_is_refused_with_a_reason(self):
        declared = self.declare()
        refused = self.marker_cli(
            "intent-register", "--marker-root", self.markers, "--workspace", self.root,
            "--assignment", declared["assignmentId"], "--relationship", "rel-0123456789abcdef",
            "--dispatch-request-id", "some-other-dispatch", expect=2,
        )
        self.assertEqual(refused["error"], "refused")
        self.assertEqual(refused["reason"], "relationship_conflict")

    def test_a_disposition_outside_the_vocabulary_is_rejected_by_the_parser(self):
        declared = self.declare()
        environment = dict(os.environ, PYTHONPATH=os.path.join(REPO, "src"))
        completed = subprocess.run(
            [sys.executable, "-m", "codex_session_relay.cli", "--state", self.tmp,
             "intent-disposition", "--marker-root", self.markers, "--workspace", self.root,
             "--assignment", declared["assignmentId"], "--session", CHILD,
             "--turn", DISPATCH_TURN, "--outcome", "done"],
            capture_output=True, text=True, env=environment, timeout=60,
        )
        self.assertNotEqual(completed.returncode, 0)
        self.assertIn("invalid choice", completed.stderr)

    def test_a_stop_payload_that_is_not_json_is_a_usage_error(self):
        payload = self.marker_cli(
            "guard-evaluate", "--marker-root", self.markers, stdin="not json", expect=4
        )
        self.assertEqual(payload["error"], "usage")

    def test_a_malformed_intent_stays_managed_in_the_explicit_view(self):
        """The diagnostic view disagreed with the decision about the same marker.

        Testing the SHAPE of the intent answered "unmanaged" for one that parsed into something
        that is not an object, so an operator asking about a managed workspace by name was told it
        was an ordinary one. Selection keeps that fact so it can be reported as malformed, and the
        guard does report it; absence is the test, not shape.
        """
        declared = self.declare()
        assignment = declared["assignmentId"]
        directory = marker.assignment_dir(self.markers, self.root, assignment)
        (directory / "intent.json").unlink()
        (directory / "intent.json").write_text('"a string is not an intent"', encoding="utf-8")
        shown = self.marker_cli(
            "intent-show", "--marker-root", self.markers, "--workspace", self.root,
            "--assignment", assignment,
        )
        self.assertTrue(shown["managed"])
        self.assertEqual(shown["malformed"], "intent")

    def test_an_absent_intent_is_still_unmanaged_in_the_explicit_view(self):
        """The control: a directory with no intent at all is genuinely not selectable."""
        declared = self.declare()
        assignment = declared["assignmentId"]
        directory = marker.assignment_dir(self.markers, self.root, assignment)
        (directory / "intent.json").unlink()
        shown = self.marker_cli(
            "intent-show", "--marker-root", self.markers, "--workspace", self.root,
            "--assignment", assignment,
        )
        self.assertFalse(shown["managed"])

    def test_a_stop_payload_file_that_is_not_utf8_is_a_usage_error(self):
        """A sibling of the marker decode fault, on the surface an operator types at.

        UnicodeDecodeError is a ValueError, so an unreadable replay file reached the generic
        handler and was reported as a host failure rather than as the named file it is.
        """
        path = os.path.join(self.markers, "payload.json")
        with open(path, "wb") as handle:
            handle.write(b'{"cwd": "\xff\xfe"}')
        payload = self.marker_cli(
            "guard-evaluate", "--marker-root", self.markers, "--stop-input", path, expect=4
        )
        self.assertEqual(payload["error"], "usage")

    def test_a_hold_that_cannot_be_recorded_is_refused_at_the_command_line(self):
        """A hold is reserved, counted against the bounds and released by its own record.

        Asking for one with --no-record asks for a hold nothing can account for, so it is refused
        where the operator typed it rather than answered with a release they did not expect.
        """
        payload = self.marker_cli(
            "guard-evaluate", "--marker-root", self.markers, "--mode", "hold", "--no-record",
            stdin=self.stop_payload(), expect=4,
        )
        self.assertEqual(payload["error"], "usage")
        self.assertIn("--no-record", payload["detail"])

    def test_observe_is_the_default_mode_on_the_command_line(self):
        declared = self.declare()
        assignment = declared["assignmentId"]
        self.marker_cli(
            "intent-bind", "--marker-root", self.markers, "--workspace", self.root,
            "--assignment", assignment, "--session", CHILD, "--task-id", CHILD,
        )
        self.marker_cli(
            "intent-claim", "--marker-root", self.markers, "--workspace", self.root,
            "--assignment", assignment, "--session", CHILD,
            "--dispatch-request-id", DISPATCH, "--first-turn", DISPATCH_TURN,
        )
        verdict = self.evaluate()
        self.assertEqual(verdict["observation"], "managed_unregistered")
        self.assertEqual(verdict["decision"], "release")
        self.assertEqual(verdict["record"]["mode"], "observe")


class GuardStoreSelection(MarkerCli):
    """The store a Stop is judged against has to be one somebody actually chose.

    guard-evaluate is exempt from the command-line store-selection refusal so that a legacy store
    nobody is using cannot switch a Stop hook off. It reads receipts from the first of three
    sources that answers - the caller's --db-path, the dbPath the coordinator recorded in the
    intent, then its own resolution - and the exemption used to cover that third one too, which is
    not a selection at all. These cases drive each corner of the narrowed boundary through the real
    command line, with a real Stop payload on stdin and a real hostile selection underneath.
    """

    def setUp(self):
        super().setUp()
        self.home = tempfile.mkdtemp(prefix="relay-guard-home-")
        self.addCleanup(shutil.rmtree, self.home, ignore_errors=True)
        self.socket = os.path.join(self.home, "app-server.sock")
        self.another_socket = os.path.join(self.home, "another-app-server.sock")
        # What marker_cli pins with --state, which is therefore the store the coordinator's own
        # commands register against and the one intent-declare records when it records anything.
        self.coordinator_store = os.path.join(self.tmp, "relay.sqlite3")

    # ------------------------------------------------------------------- fixtures

    def guard_cli(self, *argv, extra=(), expect=0, stdin=None):
        """guard-evaluate alone, under the selection this case is about.

        Its own runner because marker_cli pins --state, and for three of these cases a pinned
        directory is exactly what there must not be: discovery is the subject.

        argv carries the global options that choose the store, which argparse takes before the
        subcommand; extra carries guard-evaluate's own, which it takes after. Passing one in the
        other's place is a usage error rather than a selection, so they are named apart.
        """
        environment = dict(os.environ, PYTHONPATH=os.path.join(REPO, "src"), HOME=self.home)
        for name in ("CODEX_SESSION_RELAY_STATE", "XDG_STATE_HOME"):
            environment.pop(name, None)
        completed = subprocess.run(
            [sys.executable, "-m", "codex_session_relay.cli", *argv,
             "guard-evaluate", "--marker-root", self.markers, *extra],
            capture_output=True, text=True, env=environment, timeout=60,
            input=self.stop_payload() if stdin is None else stdin,
        )
        self.assertEqual(
            completed.returncode, expect,
            f"exit {completed.returncode}: {completed.stdout}{completed.stderr}",
        )
        return json.loads(completed.stdout)

    def two_stores_claiming_one_socket(self):
        """The ambiguity, under the HOME the guard run resolves from and nowhere else."""
        from pathlib import Path

        from codex_session_relay.store import Store

        root = os.path.join(self.home, ".local", "state", "codex-session-relay")
        for name in ("aaaa888888888888", "bbbb888888888888"):
            os.makedirs(os.path.join(root, name))
            Store(Path(root) / name / "relay.sqlite3", socket_path=self.socket).close()
        return root

    def store_recording_another_socket(self):
        """A directory an operator could pin for one run, holding another installation's store."""
        from pathlib import Path

        from codex_session_relay.store import Store

        directory = os.path.join(self.tmp, "pinned-for-one-run")
        os.makedirs(directory)
        Store(Path(directory) / "relay.sqlite3", socket_path=self.socket).close()
        return directory

    def declared_turn(self, *, record_db_path, outcome="ready_for_review", register=True):
        """The lifecycle up to a declared turn, with or without a recorded receipt store."""
        declared = self.marker_cli(
            "intent-declare", "--marker-root", self.markers, "--workspace", self.root,
            "--dispatch-request-id", DISPATCH, "--issue", "REL-1",
            *(() if record_db_path else ("--no-db-path",)),
        )
        assignment = declared["assignmentId"]
        self.marker_cli(
            "intent-bind", "--marker-root", self.markers, "--workspace", self.root,
            "--assignment", assignment, "--session", CHILD, "--task-id", CHILD,
        )
        if register:
            relationship = self.register()
            self.marker_cli(
                "intent-register", "--marker-root", self.markers, "--workspace", self.root,
                "--assignment", assignment, "--relationship", relationship["relationshipId"],
                "--dispatch-request-id", DISPATCH,
            )
        self.marker_cli(
            "intent-claim", "--marker-root", self.markers, "--workspace", self.root,
            "--assignment", assignment, "--session", CHILD,
            "--dispatch-request-id", DISPATCH, "--first-turn", DISPATCH_TURN,
        )
        self.marker_cli(
            "intent-disposition", "--marker-root", self.markers, "--workspace", self.root,
            "--assignment", assignment, "--session", CHILD, "--turn", DISPATCH_TURN,
            "--outcome", outcome,
        )
        return assignment

    def observations(self, assignment):
        """Where record_observation publishes, which is the evidence a Stop was judged at all."""
        return marker.assignment_dir(self.markers, self.root, assignment) / "hook"

    # --------------------------------------------------------- a selection nobody made

    def test_an_implicit_selection_is_refused_rather_than_guessed_at(self):
        """Two stores claim this socket, the intent recorded none, and the caller named none.

        So the only candidate left is discovery's own, which is a guess about somebody else's
        choice. The refusal names the candidates and the commands that tell them apart, which is
        what an operator has to have; judging the Stop against one of them would be a decision
        about a store nobody selected.
        """
        assignment = self.declared_turn(record_db_path=False)
        self.two_stores_claiming_one_socket()
        refused = self.guard_cli("--socket", self.socket, expect=2)
        self.assertEqual(refused["reason"], "ambiguous_state_directory")
        self.assertEqual(len(refused["candidates"]), 2)
        self.assertTrue(refused["recover"])
        # The cost of the refusal, said in the payload rather than left to be inferred.
        self.assertIn("stopNotJudged", refused)
        self.assertFalse(
            self.observations(assignment).exists(),
            "a refused Stop published an observation, so the refusal was not the whole answer",
        )

    def test_a_one_run_state_override_holding_another_socket_s_store_is_refused(self):
        """The half of this that removes a wrong answer rather than sharpening one.

        That store EXISTS and opens. Reading it finds no relationship for this assignment, which
        is receipt_missing, which holds a child that has finished - on the rows of an installation
        this assignment has nothing to do with.
        """
        self.declared_turn(record_db_path=False)
        pinned = self.store_recording_another_socket()
        refused = self.guard_cli(
            "--state", pinned, "--socket", self.another_socket, expect=2
        )
        self.assertEqual(refused["reason"], "state_directory_serves_another_socket")
        self.assertEqual(refused["stateDirectory"], pinned)
        self.assertIn("stopNotJudged", refused)

    def test_the_mismatch_half_needs_a_socket_and_the_installed_hook_passes_none(self):
        """The limitation, pinned as a fact rather than described in prose.

        A store's provenance is a socket, so the comparison needs one to compare against.
        stopadapter.guard_argv builds --marker-root, an optional --db-path and an optional --mode,
        and no --socket; the packaged copy in scripts/crw_runtime/completion.py mirrors it. So on
        the installed hook path Services.socket_path is None, nothing can be compared, and a state
        override reaching another installation's store is classified against that store exactly as
        it was before this change.

        Reported on PR #129. Closing it means carrying the expected socket through the hook
        configuration and both adapter copies, which changes the installed configuration's own
        contract and belongs to its own issue. This case exists so the gap cannot be mistaken for
        coverage, and it is the case that issue has to change.
        """
        assignment = self.declared_turn(record_db_path=False)
        pinned = self.store_recording_another_socket()
        verdict = self.guard_cli("--state", pinned)
        self.assertEqual(verdict["observation"], "receipt_missing")
        self.assertTrue(verdict["recordedAs"])
        self.assertTrue(self.observations(assignment).exists())

    # ------------------------------------------------- selections somebody did make

    def test_an_explicit_db_path_keeps_the_exemption_under_the_same_ambiguity(self):
        """The case the exemption was authorised for: the ambiguity is unrelated to this store."""
        assignment = self.declared_turn(record_db_path=False)
        self.two_stores_claiming_one_socket()
        verdict = self.guard_cli(
            "--socket", self.socket, extra=("--db-path", self.coordinator_store)
        )
        self.assertEqual(verdict["observation"], "receipt_missing")
        self.assertEqual(verdict["decision"], "release")
        self.assertTrue(verdict["recordedAs"])
        self.assertTrue(self.observations(assignment).exists())

    def test_the_recorded_intent_db_path_keeps_the_exemption_with_no_flag_at_all(self):
        """Why the intent carries dbPath: the common hook path never reaches discovery.

        Same ambiguity as the refused case, same absent --db-path. The only difference is that the
        coordinator recorded where its store lives, which is a durable selection rather than a
        guess, so the Stop is classified and recorded.
        """
        assignment = self.declared_turn(record_db_path=True)
        self.two_stores_claiming_one_socket()
        verdict = self.guard_cli("--socket", self.socket)
        self.assertEqual(verdict["observation"], "receipt_missing")
        self.assertEqual(verdict["decision"], "release")
        self.assertTrue(verdict["recordedAs"])
        self.assertTrue(self.observations(assignment).exists())

    # ------------------------------------------- Stops that never reach a store at all

    def test_a_turn_released_on_its_own_declaration_is_never_refused(self):
        """The guarantee the whole exemption exists for, under the worst selection available.

        An interrupted turn is released on what the child declared, and the receipt store is never
        opened. Store-selection mechanics must not be able to reach a judgment that does not
        depend on them, whatever discovery is doing.
        """
        assignment = self.declared_turn(record_db_path=False, outcome="interrupted")
        self.two_stores_claiming_one_socket()
        verdict = self.guard_cli("--socket", self.socket)
        self.assertEqual(verdict["observation"], "declared_interrupted")
        self.assertEqual(verdict["decision"], "release")
        self.assertTrue(self.observations(assignment).exists())

    def test_a_readiness_with_no_registered_relationship_is_never_refused(self):
        """Declaring readiness is not what reaches the store; a registered relationship is.

        Without one there is nothing to look a receipt up by, so the guard answers on the marker
        alone. Refusing here would make the new boundary about the declaration rather than about
        the store, which is the regression this case exists to catch.
        """
        assignment = self.declared_turn(record_db_path=False, register=False)
        self.two_stores_claiming_one_socket()
        verdict = self.guard_cli("--socket", self.socket)
        self.assertEqual(verdict["observation"], "managed_unregistered")
        self.assertEqual(verdict["decision"], "release")
        self.assertTrue(self.observations(assignment).exists())


if __name__ == "__main__":
    unittest.main()
