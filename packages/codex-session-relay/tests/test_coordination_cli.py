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
import shlex
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
    "merge-turn-unknown", "merge-turn-withdraw", "merge-turn-acknowledge",
    "merge-turn-restate-base",
    "region-followup",
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

    def answer_grant(self, claimed, actor):
        """What a parent does between being given the turn and using it."""
        code, _payload = self.run_cli(
            "merge-turn-acknowledge", "--turn", claimed["turnId"], "--actor", actor,
            "--grant", claimed["grant"]["grantId"],
            "--evidence", "read the grant and re-checked the record")
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

    def test_request_and_check_answer_with_their_state_and_an_unread_target_holds(self):
        """The check reads where the base branch points (CRW-229), from the target itself.

        A path that does not exist is unreadable without starting a process, so this module
        keeps an injected clock; the successful check-and-land flow against a real repository
        is in test_merge_target.py, which is declared as spending real time.
        """
        repository = "/nonexistent-crw-229/R.git"
        self.bind("PRJ-A", "task-alpha")
        code, claimed = self.run_cli(
            "merge-turn-request", "--repository", repository, "--base-ref", "dev",
            "--project", "PRJ-A", "--task", "task-alpha", "--host", "host-a",
            "--head", "head-a", "--pr", "7", "--ready")
        self.assertEqual(code, cli.EXIT_OK)
        self.assertEqual(claimed["state"], "holding")

        turn = claimed["turnId"]
        self.answer_grant(claimed, "task-alpha")
        code, checked = self.run_cli(
            "merge-turn-check", "--turn", turn, "--actor", "task-alpha",
            "--head-sha", "head-a", "--base-sha", "base-0",
            "--checks", self.CHECKS, "--review", self.GREEN, "--required", "dev-gate")
        self.assertEqual(code, cli.EXIT_REFUSED)
        self.assertEqual(checked["reason"], "merge_target_unreadable")

        code, shown = self.run_cli(
            "merge-turn-show", "--repository", repository, "--base-ref", "dev")
        self.assertEqual(code, cli.EXIT_OK)
        self.assertTrue(shown["occupied"])
        self.assertEqual(shown["holder"]["state"], "holding")
        self.assertEqual(shown["blocked"]["cause"], "target_unreadable")

    def test_a_refusal_prints_its_reason_and_exits_two(self):
        from contract.runner import FIXTURES, run_scenario
        from pathlib import Path
        run_scenario(FIXTURES / "cli-shape" / "test_coordination_cli__test_a_refusal_prints_its_reason_and_exits_two.json", Path(self.tmp))

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

    def test_a_parent_that_came_back_asks_with_the_only_identifier_it_has(self):
        from contract.runner import FIXTURES, run_scenario
        from pathlib import Path
        run_scenario(FIXTURES / "cli-shape" / "test_coordination_cli__test_a_parent_that_came_back_asks_with_the_only_identifier_it_has.json", Path(self.tmp))

    def test_two_selectors_refuse_instead_of_answering_about_one_of_them(self):
        from contract.runner import FIXTURES, run_scenario
        from pathlib import Path
        run_scenario(FIXTURES / "cli-shape" /
                     "test_coordination_cli__test_two_selectors_refuse_instead_of_answering_about_one_of_them.json",
                     Path(self.tmp))

    def test_acknowledging_a_grant_twice_converges_on_one_record(self):
        self.bind("PRJ-A", "task-alpha")
        _code, claimed = self.run_cli(
            "merge-turn-request", "--repository", "owner/repo", "--base-ref", "dev",
            "--project", "PRJ-A", "--task", "task-alpha", "--host", "host-a",
            "--head", "head-a", "--ready")
        grant = claimed["grant"]["grantId"]
        for _ in range(2):
            code, answered = self.run_cli(
                "merge-turn-acknowledge", "--turn", claimed["turnId"],
                "--actor", "task-alpha", "--grant", grant,
                "--evidence", "read it and re-checked the head")
            self.assertEqual(code, cli.EXIT_OK)
        entries = [entry for entry in answered["ledger"]
                   if entry["evidenceKind"] == "grant_acknowledged"]
        self.assertEqual(len(entries), 1)

    def test_a_grant_that_is_not_this_tenures_is_refused_at_the_surface(self):
        from contract.runner import FIXTURES, run_scenario
        from pathlib import Path
        run_scenario(FIXTURES / "cli-shape" / "test_coordination_cli__test_a_grant_that_is_not_this_tenures_is_refused_at_the_surface.json", Path(self.tmp))

    def test_a_stated_cause_travels_with_the_readiness_it_withdrew(self):
        self.bind("PRJ-A", "task-alpha")
        _code, claimed = self.run_cli(
            "merge-turn-request", "--repository", "owner/repo", "--base-ref", "dev",
            "--project", "PRJ-A", "--task", "task-alpha", "--host", "host-a",
            "--head", "head-a", "--ready")
        code, answer = self.run_cli(
            "merge-turn-ready", "--turn", claimed["turnId"], "--actor", "task-alpha",
            "--not-ready", "--cause", "the base moved to base-7")
        self.assertEqual(code, cli.EXIT_OK)
        entry = next(e for e in answer["ledger"]
                     if e["evidenceKind"] == "readiness_withdrawn")
        self.assertEqual(entry["evidence"], "the base moved to base-7")


class TheCapacityAndRegionSurfaces(CoordinationCliTestCase):
    def test_a_slot_is_reserved_released_and_reported(self):
        from contract.runner import FIXTURES, run_scenario
        from pathlib import Path
        run_scenario(FIXTURES / "cli-shape" / "test_coordination_cli__test_a_slot_is_reserved_released_and_reported.json", Path(self.tmp))

    def test_a_declared_bound_reports_how_its_number_was_reached(self):
        from contract.runner import FIXTURES, run_scenario
        from pathlib import Path
        run_scenario(FIXTURES / "cli-shape" / "test_coordination_cli__test_a_declared_bound_reports_how_its_number_was_reached.json", Path(self.tmp))

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
        from contract.runner import FIXTURES, run_scenario
        from pathlib import Path
        run_scenario(FIXTURES / "cli-shape" / "test_coordination_cli__test_a_follow_up_nobody_took_cannot_be_reported_done.json", Path(self.tmp))

    def peers(self):
        self.bind("PRJ-A", "task-alpha")
        self.bind("PRJ-B", "task-beta", host="host-b")
        code, link = self.run_cli(
            "linkage-peer", "--left-project", "PRJ-A", "--left-task", "task-alpha",
            "--left-host", "host-a", "--right-project", "PRJ-B",
            "--right-task", "task-beta", "--right-host", "host-b")
        self.assertEqual(code, cli.EXIT_OK)
        return link["linkId"]

    def test_a_late_acceptance_is_refused_and_the_answering_side_carries_the_terms(self):
        """CRW-237, the CRW-124 G3 order through the operator surface."""
        link = self.peers()
        _code, proposed = self.run_cli(
            "region-propose", "--repository", "owner/repo", "--revision", "rev-1",
            "--path", "src/a.py", "--kind", "symbol", "--key", "parse",
            "--left-project", "PRJ-B", "--right-project", "PRJ-A", "--peer-link", link,
            "--task", "task-beta", "--constraint", "keep parse, lines 12-13 at rev-1",
            "--condition", "beta restates this before renaming parse",
            "--next-owner", "task-alpha")
        code, _moved = self.run_cli(
            "region-restate-revision", "--repository", "owner/repo",
            "--from-revision", "rev-1", "--to-revision", "rev-2", "--actor", "task-beta")
        self.assertEqual(code, cli.EXIT_OK)
        code, late = self.run_cli(
            "region-settle", "--agreement", proposed["agreementId"], "--actor", "task-alpha",
            "--disposition", "accepted")
        self.assertEqual((code, late["reason"]), (cli.EXIT_REFUSED, "agreement_revision_stale"))
        code, successor = self.run_cli(
            "region-reaffirm", "--agreement", proposed["agreementId"], "--actor", "task-alpha",
            "--revision", "rev-2", "--condition", "alpha reads parse only, at rev-2")
        self.assertEqual(code, cli.EXIT_OK)
        self.assertEqual(successor["proposerTaskId"], "task-beta")
        self.assertEqual(successor["rightCondition"], "beta restates this before renaming parse")
        awaiting = successor["reaffirmation"]["awaitingAcceptance"]
        self.assertEqual(awaiting["task"], "task-beta")
        _code, shown = self.run_cli("region-show", "--repository", "owner/repo")
        live = [r for r in shown["exclusive"] if r["agreementId"] == successor["agreementId"]]
        self.assertEqual(live[0]["reaffirmation"]["awaitingAcceptance"], awaiting)
        self.assertEqual(live[0]["leftCondition"], "alpha reads parse only, at rev-2")
        self.assertEqual(live[0]["statedOn"]["leftCondition"], "rev-2")
        # The command the answer names is the one that agrees it.
        code, agreed = self.run_cli(*shlex.split(awaiting["command"]))
        self.assertEqual((code, agreed["state"]), (cli.EXIT_OK, "agreed"))
        self.assertEqual(agreed["rightCondition"], "beta restates this before renaming parse")

    def test_the_named_acceptance_command_runs_for_a_task_id_with_a_space(self):
        """Devin review: the command was printed unquoted, so a spaced task id split in two."""
        self.bind("PRJ-A", "task-alpha")
        self.bind("PRJ-B", "task beta", host="host-b")
        code, link = self.run_cli(
            "linkage-peer", "--left-project", "PRJ-A", "--left-task", "task-alpha",
            "--left-host", "host-a", "--right-project", "PRJ-B",
            "--right-task", "task beta", "--right-host", "host-b")
        self.assertEqual(code, cli.EXIT_OK)
        _code, proposed = self.run_cli(
            "region-propose", "--repository", "owner/repo", "--revision", "rev-1",
            "--path", "src/a.py", "--kind", "file", "--left-project", "PRJ-A",
            "--right-project", "PRJ-B", "--peer-link", link["linkId"], "--task", "task beta",
            "--constraint", "keep the signature")
        self.run_cli(
            "region-restate-revision", "--repository", "owner/repo",
            "--from-revision", "rev-1", "--to-revision", "rev-2", "--actor", "task beta")
        code, successor = self.run_cli(
            "region-reaffirm", "--agreement", proposed["agreementId"], "--actor", "task-alpha",
            "--revision", "rev-2")
        self.assertEqual(code, cli.EXIT_OK)
        words = shlex.split(successor["reaffirmation"]["awaitingAcceptance"]["command"])
        self.assertEqual(words[words.index("--actor") + 1], "task beta")
        code, agreed = self.run_cli(*words)
        self.assertEqual((code, agreed["state"]), (cli.EXIT_OK, "agreed"))

    def test_the_command_a_second_successor_refusal_names_runs_as_printed(self):
        """Review of db4c85ea: the printed restatement lacked the required --actor."""
        link = self.peers()
        self.run_cli(
            "region-propose", "--repository", "owner/repo", "--revision", "rev-1",
            "--path", "src/a.py", "--kind", "file", "--left-project", "PRJ-A",
            "--right-project", "PRJ-B", "--peer-link", link, "--task", "task-alpha",
            "--constraint", "keep the signature")
        self.run_cli(
            "region-restate-revision", "--repository", "owner/repo",
            "--from-revision", "rev-1", "--to-revision", "rev-2", "--actor", "task-alpha")
        code, refused = self.run_cli(
            "region-restate-revision", "--repository", "owner/repo",
            "--from-revision", "rev-1", "--to-revision", "rev-3", "--actor", "task-alpha")
        self.assertEqual((code, refused["reason"]), (cli.EXIT_REFUSED, "agreement_revision_stale"))
        detail = refused["detail"]
        words = shlex.split(detail[detail.index("region-restate-revision"):])
        code, moved = self.run_cli(*words)
        self.assertEqual(
            (code, moved["fromRevision"], moved["currentRevision"]),
            (cli.EXIT_OK, "rev-2", "rev-3"))

    def test_an_acceptance_with_a_condition_is_a_bad_invocation(self):
        link = self.peers()
        _code, proposed = self.run_cli(
            "region-propose", "--repository", "owner/repo", "--revision", "rev-1",
            "--path", "src/a.py", "--kind", "file", "--left-project", "PRJ-A",
            "--right-project", "PRJ-B", "--peer-link", link, "--task", "task-alpha",
            "--constraint", "keep the signature")
        code, refused = self.run_cli(
            "region-settle", "--agreement", proposed["agreementId"], "--actor", "task-beta",
            "--disposition", "accepted", "--condition", "only if it forwards")
        self.assertEqual((code, refused["reason"]), (cli.EXIT_REFUSED, "bad_invocation"))
        _code, shown = self.run_cli("region-show", "--repository", "owner/repo")
        self.assertEqual(shown["exclusive"][0]["state"], "proposed")


if __name__ == "__main__":
    unittest.main()
