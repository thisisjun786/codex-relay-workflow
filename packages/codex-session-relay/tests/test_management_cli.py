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


if __name__ == "__main__":
    unittest.main()
