"""The operator surface, exercised in the process rather than through a shell.

A subprocess would put this file in the suite's real-time inventory, which is declared in a
module this work may not edit, so every case drives cli.main(argv) directly and reads the JSON
it prints. That is not a compromise: what these cases are about is argument shape, exit codes
and the registration table, none of which a second process would show more of.

--state points every invocation at a temporary directory, so no case touches a real store, and
no case passes --socket, so none reaches a host.
"""

import io
import json
import unittest
from contextlib import redirect_stdout

from codex_session_relay import cli
from codex_session_relay.linkage import Linkage, PARENT
from codex_session_relay.models import Endpoint

from .support import RelayTestCase

NEW_COMMANDS = (
    "capacity-show", "limit-declare", "merge-turn-attest", "merge-turn-check",
    "merge-turn-land", "merge-turn-ready", "merge-turn-release", "merge-turn-request",
    "merge-turn-request-return", "merge-turn-resolve", "merge-turn-show",
    "merge-turn-unknown", "merge-turn-withdraw", "region-followup",
    "region-followup-accept", "region-followup-settle", "region-propose",
    "region-reaffirm", "region-restate-revision", "region-settle", "region-show",
    "slot-release", "slot-reserve", "usage-observe",
)


class CoordinationCliTestCase(RelayTestCase):
    def setUp(self):
        super().setUp()
        self.state = str(self.store.path.parent)
        self.store.close()
        linkage = Linkage(self.store, self.clock)
        del linkage

    def run_cli(self, *arguments):
        """One invocation, its exit code and whatever JSON it printed."""
        buffer = io.StringIO()
        with redirect_stdout(buffer):
            code = cli.main(["--state", self.state, *arguments])
        printed = buffer.getvalue().strip()
        return code, (json.loads(printed) if printed else None)

    def bind(self, project, task, host="host-a"):
        code, _payload = self.run_cli(
            "linkage-bind", "--role", "parent", "--scope", project,
            "--task", task, "--host", host)
        self.assertEqual(code, cli.EXIT_OK)


class TheRegistrationTableIsComplete(CoordinationCliTestCase):
    def test_every_new_command_is_registered_with_a_handler(self):
        parser = cli.build_parser()
        choices = parser._subparsers._group_actions[0].choices
        for name in NEW_COMMANDS:
            self.assertIn(name, choices, name)
            self.assertTrue(
                callable(choices[name].get_default("handler")),
                name + " has no handler default, so it would parse and then do nothing")

    def test_every_new_command_is_classified_offline_and_none_needs_a_host(self):
        for name in NEW_COMMANDS:
            self.assertIn(
                name, cli.OFFLINE_COMMANDS,
                name + " is missing from OFFLINE_COMMANDS, so doctor under-reports what an"
                " operator can run with no App Server")
            self.assertNotIn(name, cli.HOST_REQUIRED_COMMANDS, name)

    def test_the_new_commands_are_not_marker_commands(self):
        for name in NEW_COMMANDS:
            self.assertNotIn(name, cli.MARKER_COMMANDS_BY_NAME, name)


class AFullMergeTurnThroughTheCommandSurface(CoordinationCliTestCase):
    GREEN = json.dumps({"hasNextPage": False, "pagesRead": 1, "totalCount": 1,
                        "threadsSeen": ["thread-1"], "unresolved": 0})
    CHECKS = json.dumps([{"runId": "run-1", "name": "dev-gate", "headSha": "head-a",
                          "conclusion": "success", "attempt": 1}])

    def test_request_ready_check_and_land_each_answer_with_their_state(self):
        self.bind("PRJ-A", "task-alpha")
        code, claimed = self.run_cli(
            "merge-turn-request", "--repository", "owner/repo", "--base-ref", "dev",
            "--project", "PRJ-A", "--task", "task-alpha", "--host", "host-a",
            "--head", "head-a", "--pr", "7", "--ready")
        self.assertEqual(code, cli.EXIT_OK)
        self.assertEqual(claimed["state"], "holding")

        turn = claimed["turnId"]
        code, checked = self.run_cli(
            "merge-turn-check", "--turn", turn, "--actor", "task-alpha",
            "--head-sha", "head-a", "--base-sha", "base-0",
            "--checks", self.CHECKS, "--review", self.GREEN, "--required", "dev-gate")
        self.assertEqual(code, cli.EXIT_OK)
        self.assertEqual(checked["state"], "merging")
        self.assertEqual(checked["requiredDeclared"], ["dev-gate"])

        code, landed = self.run_cli(
            "merge-turn-land", "--turn", turn, "--actor", "task-alpha",
            "--landed-sha", "merge-1", "--observed-base-sha", "base-1",
            "--evidence", "the merge commit is on the base")
        self.assertEqual(code, cli.EXIT_OK)
        self.assertEqual(landed["released"]["state"], "landed")

        code, shown = self.run_cli(
            "merge-turn-show", "--repository", "owner/repo", "--base-ref", "dev")
        self.assertEqual(code, cli.EXIT_OK)
        self.assertFalse(shown["occupied"])

    def test_a_refusal_prints_its_reason_and_exits_two(self):
        self.bind("PRJ-A", "task-alpha")
        _code, claimed = self.run_cli(
            "merge-turn-request", "--repository", "owner/repo", "--base-ref", "dev",
            "--project", "PRJ-A", "--task", "task-alpha", "--host", "host-a",
            "--head", "head-a", "--ready")
        code, payload = self.run_cli(
            "merge-turn-check", "--turn", claimed["turnId"], "--actor", "task-alpha",
            "--head-sha", "head-moved", "--base-sha", "base-0",
            "--checks", self.CHECKS, "--review", self.GREEN)
        self.assertEqual(code, cli.EXIT_REFUSED)
        self.assertEqual(payload["error"], "refused")
        self.assertEqual(payload["reason"], "merge_candidate_moved")

    def test_malformed_json_refuses_instead_of_reporting_a_host_fault(self):
        self.bind("PRJ-A", "task-alpha")
        _code, claimed = self.run_cli(
            "merge-turn-request", "--repository", "owner/repo", "--base-ref", "dev",
            "--project", "PRJ-A", "--task", "task-alpha", "--host", "host-a",
            "--head", "head-a", "--ready")
        for flag, value in (("--checks", "not json"), ("--review", "{oops}")):
            arguments = ["merge-turn-check", "--turn", claimed["turnId"],
                         "--actor", "task-alpha", "--head-sha", "head-a",
                         "--base-sha", "base-0", "--checks", self.CHECKS,
                         "--review", self.GREEN]
            arguments[arguments.index(flag) + 1] = value
            code, payload = self.run_cli(*arguments)
            self.assertEqual(code, cli.EXIT_REFUSED, flag)
            self.assertEqual(payload["reason"], "bad_invocation", flag)
            self.assertIn(flag, payload["detail"])


class TheCapacityAndRegionSurfaces(CoordinationCliTestCase):
    def test_a_slot_is_reserved_released_and_reported(self):
        self.bind("PRJ-A", "task-alpha")
        code, reserved = self.run_cli(
            "slot-reserve", "--kind", "assignment", "--subject", "REL-1",
            "--parent-task", "task-alpha", "--project", "PRJ-A", "--actor", "task-alpha")
        self.assertEqual(code, cli.EXIT_OK)
        self.assertFalse(reserved["alreadyHeld"])
        code, shown = self.run_cli("capacity-show", "--project", "PRJ-A")
        self.assertEqual((code, shown["total"]), (cli.EXIT_OK, 1))
        code, released = self.run_cli(
            "slot-release", "--kind", "assignment", "--subject", "REL-1",
            "--actor", "task-alpha", "--reason", "completed")
        self.assertEqual((code, released["state"]), (cli.EXIT_OK, "released"))

    def test_a_declared_bound_reports_how_its_number_was_reached(self):
        self.bind("PRJ-A", "task-alpha")
        self.run_cli(
            "limit-declare", "--scope-kind", "project", "--scope", "PRJ-A",
            "--dimension", "file_descriptors", "--unit", "fds", "--ceiling", "100",
            "--declared-by", "supervisor-1", "--source", "operator", "--no-enforce")
        code, shown = self.run_cli(
            "capacity-show", "--scope-kind", "project", "--scope", "PRJ-A")
        self.assertEqual(code, cli.EXIT_OK)
        entry = shown["headroom"]["dimensions"][0]
        self.assertEqual((entry["used"], entry["proof"]), (None, "unmeasured"))
        code, _observed = self.run_cli(
            "usage-observe", "--scope-kind", "project", "--scope", "PRJ-A",
            "--dimension", "file_descriptors", "--observed", "42",
            "--observed-by", "probe-1", "--method", "counted the descriptors")
        self.assertEqual(code, cli.EXIT_OK)
        _code, again = self.run_cli(
            "capacity-show", "--scope-kind", "project", "--scope", "PRJ-A")
        self.assertEqual(again["headroom"]["dimensions"][0]["proof"], "observed")

    def test_a_region_is_proposed_settled_and_shown(self):
        self.bind("PRJ-A", "task-alpha")
        self.bind("PRJ-B", "task-beta", host="host-b")
        code, link = self.run_cli(
            "linkage-peer", "--left-project", "PRJ-A", "--left-task", "task-alpha",
            "--left-host", "host-a", "--right-project", "PRJ-B",
            "--right-task", "task-beta", "--right-host", "host-b")
        self.assertEqual(code, cli.EXIT_OK)
        code, proposed = self.run_cli(
            "region-propose", "--repository", "owner/repo", "--revision", "rev-1",
            "--path", "src/a.py", "--kind", "symbol", "--key", "parse",
            "--left-project", "PRJ-A", "--right-project", "PRJ-B",
            "--peer-link", link["linkId"], "--task", "task-alpha",
            "--constraint", "keep the signature", "--next-owner", "task-beta")
        self.assertEqual(code, cli.EXIT_OK)
        self.assertEqual(proposed["state"], "proposed")
        self.assertFalse(proposed["grantsMergePermission"])
        code, agreed = self.run_cli(
            "region-settle", "--agreement", proposed["agreementId"],
            "--actor", "task-beta", "--disposition", "accepted")
        self.assertEqual((code, agreed["state"]), (cli.EXIT_OK, "agreed"))
        code, shown = self.run_cli("region-show", "--repository", "owner/repo")
        self.assertEqual((code, len(shown["exclusive"])), (cli.EXIT_OK, 1))

    def test_a_follow_up_nobody_took_cannot_be_reported_done(self):
        self.bind("PRJ-A", "task-alpha")
        self.bind("PRJ-B", "task-beta", host="host-b")
        _code, link = self.run_cli(
            "linkage-peer", "--left-project", "PRJ-A", "--left-task", "task-alpha",
            "--left-host", "host-a", "--right-project", "PRJ-B",
            "--right-task", "task-beta", "--right-host", "host-b")
        _code, proposed = self.run_cli(
            "region-propose", "--repository", "owner/repo", "--revision", "rev-1",
            "--path", "src/a.py", "--kind", "file", "--left-project", "PRJ-A",
            "--right-project", "PRJ-B", "--peer-link", link["linkId"],
            "--task", "task-alpha", "--constraint", "keep the signature")
        code, item = self.run_cli(
            "region-followup", "--agreement", proposed["agreementId"],
            "--trigger", "a temporary duplicate implementation",
            "--acceptance", "the duplicate is gone", "--recorded-by", "task-alpha",
            "--issue-ref", "CRW-200")
        self.assertEqual(code, cli.EXIT_OK)
        code, payload = self.run_cli(
            "region-followup-settle", "--followup", item["followupId"],
            "--actor", "task-alpha", "--disposition", "done")
        self.assertEqual(code, cli.EXIT_REFUSED)
        self.assertEqual(payload["reason"], "followup_unassigned")


if __name__ == "__main__":
    unittest.main()

