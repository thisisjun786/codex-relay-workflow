"""What a service launches its daemon with, and where that answer came from.

The reported failure is exact. `service restart` relaunched the daemon from the caller's own
environment, and a daemon reads its role policy from its own environment once at startup. So the
same command, typed in two terminals, produced a service that enforced roles and a service that
withheld every role-bound delivery with role_policy_unconfigured - while the policy file, the
installation and the owner's intent were identical in both. The policy a service ran on was a
property of whoever last typed the command.

These cases hold the whole boundary rather than one end of it: what the launcher puts in a
child's environment, that a real child resolves it, that two files named at once are refused
before anything is stopped, and that a service with nothing declared anywhere still refuses
honestly instead of passing quietly.
"""

import json
import os
import select
import subprocess
import sys
from contextlib import contextmanager
from pathlib import Path
from unittest import mock

from codex_session_relay import rolepolicy
from codex_session_relay.errors import RefusalReason
from codex_session_relay.service import (
    LAUNCH_POLICY, canonical_policy_path, owned_service, start_ticks,
)

from .test_rolepolicy import POLICY, write_policy
from .test_service import REPO, ServiceTestCase

# A second, valid policy declaring a different parent pair, so the two files have different
# digests and a test can tell which one a process actually read.
OTHER_POLICY = {
    "roles": {
        "supervisor": {"expectation": "record"},
        "parent": {"model": "devin/swe-2", "reasoningEffort": "high"},
        "child": {"model": "anthropic/claude-opus-5", "reasoningEffort": "xhigh"},
    }
}


@contextmanager
def caller_environment(value=None):
    """This process as a shell that does, or does not, declare an execution policy.

    The runner's own variable is removed first whether or not a value is being set: a suite
    that inherited one would prove the opposite of what these cases are about.
    """
    with mock.patch.dict(os.environ, {}, clear=False):
        os.environ.pop(rolepolicy.ENVIRONMENT_VARIABLE, None)
        if value is not None:
            os.environ[rolepolicy.ENVIRONMENT_VARIABLE] = value
        yield


class Unstarted:
    """A child that was never started. The environment it would have been given is the subject."""

    returncode = None

    def __init__(self):
        self.pid = os.getpid()

    def poll(self):
        return None

    def terminate(self):
        return None

    def kill(self):
        return None

    def wait(self, timeout=None):
        return 0


class LaunchPolicyCase(ServiceTestCase):
    def setUp(self):
        super().setUp()
        # write_policy drops the per-process snapshot, and these cases stage more than one
        # file; reset again afterwards so nothing a case read leaks into the next suite.
        self.addCleanup(rolepolicy.reset)
        self.policy_path = write_policy(Path(self.tmp))
        second = Path(self.tmp) / "second"
        second.mkdir()
        self.other_policy = write_policy(second, OTHER_POLICY)
        self.digest = rolepolicy.declared(
            {rolepolicy.ENVIRONMENT_VARIABLE: self.policy_path}
        ).digest
        self.other_digest = rolepolicy.declared(
            {rolepolicy.ENVIRONMENT_VARIABLE: self.other_policy}
        ).digest
        self.assertNotEqual(self.digest, self.other_digest)

    def enabled_service(self, name="a"):
        service = self.service(name)
        self.assertTrue(service.enable(actor="test")["ok"])
        return service

    def launcher_environment(self, service, **kwargs):
        """The environment default_launcher would hand a daemon, without starting one."""
        captured = {}

        def popen(argv, **call):
            captured["argv"] = argv
            captured["env"] = call["env"]
            return Unstarted()

        with mock.patch.object(subprocess, "Popen", popen):
            service.default_launcher(service, allow_isolated=True, **kwargs)
        return captured["env"]

    def launch(self, service, *, action="start", timeout=0.4, **call):
        """Run a real start or restart, and capture what its launcher was about to exec."""
        captured = {}

        def popen(argv, **kwargs):
            captured["env"] = kwargs["env"]
            return Unstarted()

        with mock.patch.object(subprocess, "Popen", popen):
            run = service.start if action == "start" else service.restart
            payload = run(allow_isolated=True, actor="test", timeout=timeout, poll=0.02,
                          **call)
        return payload, captured.get("env")

    def resolved_by_a_child(self, environment):
        """What a process launched with this environment resolves, read from that process.

        The defect is about a process boundary, so one case crosses one: a mapping that looks
        right to the parent is not evidence that a child reads a policy out of it.
        """
        code = (
            "import json, sys\n"
            f"sys.path.insert(0, {os.path.join(REPO, 'src')!r})\n"
            "from codex_session_relay import rolepolicy\n"
            "print(json.dumps(rolepolicy.declared().summary()), flush=True)\n"
        )
        finished = subprocess.run([sys.executable, "-c", code], env=environment,
                                  capture_output=True, text=True, timeout=60)
        self.assertEqual(finished.returncode, 0, finished.stderr)
        return json.loads(finished.stdout)


class WhatALaunchCarries(LaunchPolicyCase):
    def test_a_restart_from_a_shell_without_the_variable_keeps_the_declared_policy(self):
        """The reported defect, end to end: a running service, restarted, nothing exported."""
        service = self.enabled_service()
        self.assertTrue(service.declare_launch_policy(self.policy_path, actor="test")["ok"])
        child, _pid = self.holder(service)

        with caller_environment(None):
            payload, environment = self.launch(service, action="restart", timeout=2.0)

        self.assertIsNotNone(child.poll(), "the running service was not actually restarted")
        self.assertEqual(environment[rolepolicy.ENVIRONMENT_VARIABLE], self.policy_path)
        self.assertEqual(payload["start"]["launchPolicy"]["source"], "record")
        self.assertEqual(payload["start"]["launchPolicy"]["digest"], self.digest)

    def test_the_child_can_actually_read_the_policy_it_is_handed(self):
        service = self.enabled_service()
        service.declare_launch_policy(self.policy_path, actor="test")

        with caller_environment(None):
            environment = self.launcher_environment(service)

        summary = self.resolved_by_a_child(environment)
        self.assertEqual(summary["state"], "declared")
        self.assertEqual(summary["digest"], self.digest)
        self.assertEqual(set(summary["roles"]), set(POLICY["roles"]))

    def test_an_environment_declaration_still_launches_and_says_nothing_recorded_it(self):
        """The behaviour before this record existed, kept, and no longer silent about itself."""
        service = self.enabled_service()

        with caller_environment(self.policy_path):
            payload, environment = self.launch(service)

        self.assertEqual(environment[rolepolicy.ENVIRONMENT_VARIABLE], self.policy_path)
        self.assertEqual(payload["launchPolicy"]["source"], "environment")
        self.assertIn("service declare", payload["launchPolicy"]["hint"])
        # Nothing was recorded. A start that quietly wrote down whatever shell it was typed
        # from would pin one export as this service's policy for every restart after it.
        self.assertFalse((service.selection.path / LAUNCH_POLICY).exists())

    def test_with_nothing_declared_anywhere_the_daemon_is_left_to_refuse_honestly(self):
        service = self.enabled_service()

        with caller_environment(None):
            environment = self.launcher_environment(service)

        # Nothing is set rather than something empty: the daemon's own refusal is the honest
        # answer here, and this launch must not manufacture a policy to avoid reaching it.
        self.assertNotIn(rolepolicy.ENVIRONMENT_VARIABLE, environment)
        self.assertEqual(self.resolved_by_a_child(environment)["state"], "unresolved")
        refusal = rolepolicy.refuse_unresolved(
            rolepolicy.declared({}), "child", "task-under-test",
        )
        self.assertEqual(refusal.reason, RefusalReason.ROLE_POLICY_UNCONFIGURED)

    def test_the_launch_uses_the_reading_it_was_frozen_with(self):
        """A declaration written mid-launch does not change the launch already decided.

        Without this the resolution taken before a restart stopped the service and the one
        taken when its replacement was built could differ, and the difference would land after
        the service was already down.
        """
        service = self.enabled_service()
        service.declare_launch_policy(self.policy_path, actor="test")
        with caller_environment(None):
            service.launch_environment = service.resolve_launch_policy()
            service.declare_launch_policy(self.other_policy, actor="test")
            environment = self.launcher_environment(service)

        self.assertEqual(environment[rolepolicy.ENVIRONMENT_VARIABLE], self.policy_path)


class TwoAnswersAreRefused(LaunchPolicyCase):
    def test_a_restart_naming_another_file_is_refused_before_anything_is_stopped(self):
        service = self.enabled_service()
        service.declare_launch_policy(self.policy_path, actor="test")
        child, _pid = self.holder(service)

        with caller_environment(self.other_policy):
            payload = service.restart(allow_isolated=True, actor="test",
                                      launcher=self._never_launch)

        self.assertFalse(payload["ok"])
        self.assertEqual(payload["reason"], "launch_policy_conflict")
        self.assertEqual(payload["stop"]["supervisor"], "untouched")
        self.assertIsNone(child.poll(), "a question about two files stopped the service")
        self.assertIn(self.policy_path, payload["detail"])
        self.assertIn(self.other_policy, payload["detail"])

    def test_a_start_naming_another_file_is_refused_too(self):
        service = self.enabled_service()
        service.declare_launch_policy(self.policy_path, actor="test")

        with caller_environment(self.other_policy):
            payload = service.start(allow_isolated=True, actor="test",
                                    launcher=self._never_launch)

        self.assertEqual(payload["reason"], "launch_policy_conflict")

    def test_one_file_spelled_two_ways_is_one_declaration(self):
        service = self.enabled_service()
        service.declare_launch_policy(self.policy_path, actor="test")
        alias = Path(self.tmp) / "alias.json"
        alias.symlink_to(self.policy_path)
        spellings = (
            str(alias),
            os.path.join(os.path.dirname(self.policy_path), ".", "execution-policy.json"),
            os.path.relpath(self.policy_path, os.getcwd()),
        )

        for spelling in spellings:
            with self.subTest(spelling=spelling), caller_environment(spelling):
                payload, environment = self.launch(service)
                self.assertNotEqual(payload.get("reason"), "launch_policy_conflict")
                self.assertEqual(environment[rolepolicy.ENVIRONMENT_VARIABLE],
                                 self.policy_path)

    def test_an_unreadable_record_refuses_rather_than_falling_back_to_the_shell(self):
        """Absence is a decision; corruption is not one, and must not be read as absence."""
        service = self.enabled_service()
        service.declare_launch_policy(self.policy_path, actor="test")
        (service.selection.path / LAUNCH_POLICY).write_text("{not json", encoding="utf-8")
        child, _pid = self.holder(service)

        with caller_environment(self.other_policy):
            payload = service.restart(allow_isolated=True, actor="test",
                                      launcher=self._never_launch)

        self.assertEqual(payload["reason"], "launch_policy_unreadable")
        self.assertIsNone(child.poll(), "an unreadable record stopped the service")
        self.assertIn("service declare", payload["launchPolicy"]["hint"])

    def _never_launch(self, *_args, **_kwargs):  # pragma: no cover - reaching it is the failure
        self.fail("a launch went ahead without being able to say which policy it would read")


class DeclaringAndForgetting(LaunchPolicyCase):
    def test_a_declaration_is_read_back_before_it_is_recorded(self):
        service = self.enabled_service()

        refused = service.declare_launch_policy(
            os.path.join(self.tmp, "not-a-file.json"), actor="test",
        )

        self.assertFalse(refused["ok"])
        self.assertEqual(refused["reason"], "execution_policy_unreadable")
        self.assertFalse((service.selection.path / LAUNCH_POLICY).exists())

    def test_a_declaration_records_the_digest_it_was_checked_under(self):
        service = self.enabled_service()

        declared = service.declare_launch_policy(self.policy_path, actor="operator")

        self.assertTrue(declared["ok"])
        self.assertEqual(declared["launchPolicy"]["digest"], self.digest)
        self.assertEqual(declared["launchPolicy"]["state"], "declared")
        self.assertEqual(declared["declared"]["declaredBy"], "operator")

    def test_a_relative_declaration_is_recorded_as_the_file_it_names(self):
        """The daemon resolves it in its own process; a relative path would follow the cwd."""
        service = self.enabled_service()
        relative = os.path.relpath(self.policy_path, os.getcwd())

        service.declare_launch_policy(relative, actor="test")

        recorded = json.loads(
            (service.selection.path / LAUNCH_POLICY).read_text(encoding="utf-8")
        )
        self.assertTrue(os.path.isabs(recorded["path"]))
        self.assertEqual(canonical_policy_path(recorded["path"]),
                         canonical_policy_path(self.policy_path))

    def test_the_declaration_outlives_a_disable_and_enable_cycle(self):
        """It lives beside service.json rather than inside it, and this is why."""
        service = self.enabled_service()
        service.declare_launch_policy(self.policy_path, actor="test")

        service.disable(actor="test")
        service.enable(actor="test")

        self.assertEqual(service.resolve_launch_policy({})["path"], self.policy_path)

    def test_forgetting_leaves_the_next_launch_with_its_own_environment(self):
        service = self.enabled_service()
        service.declare_launch_policy(self.policy_path, actor="test")

        forgotten = service.forget_launch_policy(actor="test")

        self.assertTrue(forgotten["ok"])
        self.assertEqual(forgotten["forgot"], self.policy_path)
        with caller_environment(None):
            self.assertNotIn(rolepolicy.ENVIRONMENT_VARIABLE,
                             self.launcher_environment(service))

    def test_a_declared_file_that_has_since_gone_is_launched_with_and_reported(self):
        """Reported, not refused: the daemon withholds role-bound work and serves the rest."""
        service = self.enabled_service()
        service.declare_launch_policy(self.policy_path, actor="test")
        os.unlink(self.policy_path)

        with caller_environment(None):
            payload, environment = self.launch(service)

        self.assertEqual(payload["launchPolicy"]["state"], "unreadable")
        self.assertIsNone(payload["launchPolicy"]["digest"])
        self.assertEqual(environment[rolepolicy.ENVIRONMENT_VARIABLE], self.policy_path)


class WhatStatusSays(LaunchPolicyCase):
    def test_status_separates_what_is_declared_from_what_is_running(self):
        """Declaring B while the service runs on A is a pending change, not a change."""
        service = self.enabled_service()
        with owned_service(service, allow_isolated=True, require_intent=False):
            code = (
                "import sys\n"
                f"sys.path.insert(0, {os.path.join(REPO, 'src')!r})\n"
                "from pathlib import Path\n"
                "from codex_session_relay.service import RelayService, ScopeRegistry\n"
                "from codex_session_relay.store import resolve_state_dir\n"
                "from codex_session_relay import rolepolicy\n"
                f"service = RelayService(resolve_state_dir({str(service.selection.path)!r}),"
                f" socket_path={service.socket_path!r},"
                f" scope=ScopeRegistry(Path({str(service.scope.root)!r}), 'isolated'),"
                f" store_id={service.store_id!r})\n"
                "service.publish_worker_policy(rolepolicy.snapshot_record())\n"
                "print('published', flush=True)\n"
                "sys.stdin.read()\n"
            )
            worker = subprocess.Popen(
                [sys.executable, "-c", code],
                env=dict(os.environ, **{rolepolicy.ENVIRONMENT_VARIABLE: self.policy_path}),
                stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.PIPE,
                text=True,
            )
            self.children.append(worker)
            for handle in (worker.stdin, worker.stdout, worker.stderr):
                self.addCleanup(handle.close)
            readable, _, _ = select.select([worker.stdout], [], [], 20)
            # Not worker.stderr.read() as the message: it is evaluated whether or not the
            # assertion holds, and reading a live child's stderr blocks until it exits.
            self.assertTrue(readable, "the worker never published a policy receipt")
            self.assertEqual(worker.stdout.readline(), "published\n")
            service._note(workerPid=worker.pid, workerStartTicks=start_ticks(worker.pid))

            service.declare_launch_policy(self.other_policy, actor="test")
            with caller_environment(None):
                reported = service.status()["launchPolicy"]

            self.assertEqual(reported["digest"], self.other_digest)
            self.assertEqual(reported["runningDigest"], self.digest)
            self.assertEqual(reported["matchesRunning"], "different")
            worker.stdin.close()


class ThroughTheCommandLine(LaunchPolicyCase):
    def cli(self, *args, expect=0, environment=None):
        state = str(self.service("cli").selection.path)
        call = dict(os.environ, PYTHONPATH=os.path.join(REPO, "src"))
        call.pop(rolepolicy.ENVIRONMENT_VARIABLE, None)
        call.pop("CODEX_SESSION_RELAY_STATE", None)
        call.update(environment or {})
        finished = subprocess.run(
            [sys.executable, "-m", "codex_session_relay.cli", "--state", state, *args],
            capture_output=True, text=True, env=call, timeout=60,
        )
        self.assertEqual(finished.returncode, expect,
                         f"exit {finished.returncode}: {finished.stdout}{finished.stderr}")
        return json.loads(finished.stdout)

    def test_declaring_needs_no_app_server_and_is_reported_as_offline(self):
        declared = self.cli("service", "declare", "--execution-policy", self.policy_path,
                            "--actor", "operator")

        self.assertTrue(declared["ok"])
        self.assertEqual(declared["launchPolicy"]["digest"], self.digest)
        self.assertIn("service declare",
                      self.cli("doctor")["actorReachability"]["offlineCommands"])

    def test_a_policy_the_command_cannot_read_exits_refused(self):
        self.cli("service", "declare", "--execution-policy",
                 os.path.join(self.tmp, "absent.json"), expect=2)

    def test_status_reports_the_declaration_a_restart_would_use(self):
        self.cli("service", "declare", "--execution-policy", self.policy_path)

        reported = self.cli("service", "status")["launchPolicy"]

        self.assertEqual(reported["record"], self.policy_path)
        self.assertEqual(reported["source"], "record")
        self.assertEqual(reported["appliesTo"],
                         "the next daemon launched from this state directory")

    def test_forgetting_through_the_command_line_leaves_nothing_declared(self):
        self.cli("service", "declare", "--execution-policy", self.policy_path)

        self.cli("service", "declare", "--forget-execution-policy")

        self.assertIsNone(self.cli("service", "status")["launchPolicy"]["record"])
