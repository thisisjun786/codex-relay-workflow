"""What CRW-215's installed live round trip found, each written down red first.

The live run on the installed runtime (b19b3fd2, round 2) delivered both reports and verified both
readbacks, and it did so only after four things the source and its fake host had never shown:

F1  An idle supervisor the host had unloaded was never sent anything. The relay refuses to
    transmit a pair it cannot prove, and a supervisor's pair is always one it cannot prove, so an
    unloaded supervisor waited until somebody else loaded it.
F2  Loaded by a resume that transmits nothing, the supervisor came back with its workspace roots
    reduced to its cwd, and a record taken from its creation receipt then refused every send.
F3  The readback line named the bare program, and this host's PATH resolved it to a relay with no
    supervisor-read at all.
F4  An omission was delivered as parent_to_supervisor/blocked with an unknown basis, and the real
    supervisor read it as a block.
"""

import json
import os
import shlex
import subprocess
import unittest
from pathlib import Path

from codex_session_relay import rolepolicy, supervision
from codex_session_relay.settings import TaskSettings
from codex_session_relay.transport import (
    DISPATCHED,
    WITHHELD_PRE_SEND,
    classify_operation_receipt,
)

from . import test_bridge_adapter as seam
from .test_rolepolicy import write_policy
from .test_supervisor_autosend import DaemonChannelCase
from .test_supervisor_channel import SUPERVISOR, ChannelTestCase
from .test_supervisor_omission_store import StoreOmissionCase

LOADED_ONLY = {"threadId": "thread-1", "excludeTurns": True}
EXTRA_ROOT = "/workspace/example/shared-notes"


def record_based(data=None):
    """A record whose pair no policy derived, flagged the way the delivery gate flags it."""
    settings = TaskSettings(dict(data or seam.AUTHORIZED.data))
    settings.settings_free_resume = True
    return settings


def with_extra_root():
    """What a creation receipt records when the creator named a root beyond the cwd."""
    return dict(seam.AUTHORIZED.data,
                runtimeWorkspaceRoots=[seam.WORKTREE, EXTRA_ROOT],
                environments=[{"environmentId": "local", "cwd": seam.WORKTREE,
                               "runtimeWorkspaceRoots": [seam.WORKTREE, EXTRA_ROOT]}])


class _Seam(unittest.TestCase):
    """The real adapter and the real guarded send, against a fake RPC endpoint."""

    setUp = seam.GuardedSettingsSeam.setUp
    _adapter = seam.GuardedSettingsSeam._adapter
    _methods = staticmethod(seam.GuardedSettingsSeam._methods)

    def assert_withheld_before_any_turn(self, receipt, calls, code):
        self.assertNotIn("turn/start", self._methods(calls), "a turn was started anyway")
        self.assertEqual(receipt["status"], "failed", receipt)
        self.assertEqual(receipt["rpcError"]["code"], code)
        facts = classify_operation_receipt(receipt)
        self.assertEqual(facts.delivery_state, WITHHELD_PRE_SEND)
        self.assertEqual(facts.send_attempted, "no")
        self.assertTrue(facts.retry_safe)


class F1AnUnloadedRecordBasedRecipient(_Seam):
    """Loaded with nothing requested, compared, and only then started."""

    def test_it_is_loaded_with_nothing_transmitted_and_then_started(self):
        adapter, calls = self._adapter(status="notLoaded")
        receipt = adapter.send_message("sup-100000000000-a1", "thread-1", "hi", record_based())
        self.assertEqual(self._methods(calls), ["thread/read", "thread/resume", "turn/start"])
        self.assertEqual(calls[1][1], LOADED_ONLY,
                         "a pair no policy derived must never reach a thread the host loads")
        self.assertEqual(receipt["status"], "accepted", receipt.get("error"))

    def test_a_thread_that_loads_as_something_else_starts_nothing(self):
        adapter, calls = self._adapter(
            resume=seam.authorized_resume_response(model="someone/else-entirely"),
            status="notLoaded")
        receipt = adapter.send_message("sup-100000000000-a2", "thread-1", "hi", record_based())
        self.assertEqual(calls[1][1], LOADED_ONLY)
        self.assert_withheld_before_any_turn(receipt, calls, "settings_differ_after_load")

    def test_a_loaded_record_based_recipient_is_not_transmitted_a_pair_either(self):
        """The unload can happen between the read and the resume, so the rule cannot wait for it."""
        adapter, calls = self._adapter(status="idle")
        receipt = adapter.send_message("sup-100000000000-a3", "thread-1", "hi", record_based())
        self.assertEqual(calls[1][1], LOADED_ONLY)
        self.assertEqual(receipt["status"], "accepted", receipt.get("error"))

    def test_a_pair_derived_unloaded_recipient_still_carries_its_settings(self):
        """Parents and children are resumed exactly as before: policy derived their pair."""
        adapter, calls = self._adapter(status="notLoaded")
        receipt = adapter.send_message("del-100000000000-a4", "thread-1", "hi",
                                       TaskSettings(dict(seam.AUTHORIZED.data)))
        self.assertEqual(receipt["status"], "accepted", receipt.get("error"))
        self.assertEqual(calls[1][1]["model"], seam.AUTHORIZED.data["model"])
        self.assertEqual(calls[1][1]["runtimeWorkspaceRoots"], [seam.WORKTREE])


class F2WhatALoadKeeps(_Seam):
    """Roots a load does not keep may come back narrower; nothing may come back wider."""

    def test_roots_a_load_did_not_keep_come_back_narrower_and_are_accepted(self):
        adapter, calls = self._adapter(status="idle")
        receipt = adapter.send_message("sup-200000000000-a1", "thread-1", "hi",
                                       record_based(with_extra_root()))
        self.assertEqual(receipt["status"], "accepted", receipt.get("error"))

    def test_roots_wider_than_the_record_are_refused(self):
        adapter, calls = self._adapter(
            resume=seam.authorized_resume_response(
                runtimeWorkspaceRoots=[seam.WORKTREE, "/somewhere/else"]),
            status="notLoaded")
        receipt = adapter.send_message("sup-200000000000-a2", "thread-1", "hi", record_based())
        self.assert_withheld_before_any_turn(receipt, calls, "settings_differ_after_load")

    def test_an_environment_root_wider_than_the_record_is_refused(self):
        adapter, calls = self._adapter(
            resume=seam.authorized_resume_response(thread={"environments": [
                {"environmentId": "local", "cwd": seam.WORKTREE,
                 "runtimeWorkspaceRoots": [seam.WORKTREE, "/somewhere/else"]}]}),
            status="notLoaded")
        receipt = adapter.send_message("sup-200000000000-a3", "thread-1", "hi", record_based())
        self.assert_withheld_before_any_turn(receipt, calls, "settings_differ_after_load")

    def test_another_environment_selection_is_refused(self):
        adapter, calls = self._adapter(
            resume=seam.authorized_resume_response(thread={"environments": [
                {"environmentId": "remote", "cwd": seam.WORKTREE,
                 "runtimeWorkspaceRoots": [seam.WORKTREE]}]}),
            status="notLoaded")
        receipt = adapter.send_message("sup-200000000000-a4", "thread-1", "hi", record_based())
        self.assert_withheld_before_any_turn(receipt, calls, "settings_differ_after_load")

    def test_sandbox_approval_cwd_model_and_effort_stay_exact(self):
        cases = {
            "sandbox": {"sandbox": dict(seam.AUTHORIZED_POLICY, networkAccess=True)},
            "cwd": {"cwd": "/workspace/example/another"},
            "model": {"model": "someone/else-entirely"},
            "reasoningEffort": {"reasoningEffort": "low"},
        }
        for number, (field, override) in enumerate(cases.items(), start=5):
            with self.subTest(field=field):
                adapter, calls = self._adapter(
                    resume=seam.authorized_resume_response(**override), status="notLoaded")
                receipt = adapter.send_message(f"sup-20000000000{number}-a1", "thread-1", "hi",
                                               record_based())
                self.assert_withheld_before_any_turn(receipt, calls,
                                                     "settings_differ_after_load")
                self.assertEqual(receipt["settingsFindings"][0]["field"], field)


class _DeclaredPolicy:
    """The real authorized_settings under a policy that declares the supervisor by record."""

    def declare_policy(self):
        previous = os.environ.get(rolepolicy.ENVIRONMENT_VARIABLE)
        os.environ[rolepolicy.ENVIRONMENT_VARIABLE] = write_policy(Path(self.tmp))

        def restore():
            if previous is None:
                os.environ.pop(rolepolicy.ENVIRONMENT_VARIABLE, None)
            else:
                os.environ[rolepolicy.ENVIRONMENT_VARIABLE] = previous
            rolepolicy.reset()

        self.addCleanup(restore)


class F1AnUnloadedSupervisorIsWokenByTheAutomaticReport(_DeclaredPolicy, DaemonChannelCase):
    """The daemon's own tick, the real settings gate, and a supervisor the host has unloaded."""

    def setUp(self):
        super().setUp()
        self.declare_policy()
        self.channel = self.build_channel(settings=None)
        self.daemon = self.build_daemon(self.channel)
        self.adapter.set_status(SUPERVISOR, "notLoaded")

    def test_the_report_reaches_an_unloaded_supervisor_with_nothing_transmitted(self):
        self.completed()
        self.tick()
        self.assertEqual(len(self.upward()), 1, "the unloaded supervisor was never sent to")
        self.assertEqual(self.only_message()["state"], DISPATCHED)
        sent = [settings for request, settings in self.adapter.settings_seen
                if request.startswith("sup-")]
        self.assertTrue(sent and all(getattr(one, "settings_free_resume", False) for one in sent),
                        "a supervisor's pair is never transmitted")


class F3RenderedLinesRunAsRendered(ChannelTestCase):
    """A line written for a later reader names the relay that wrote it, not whatever PATH finds."""

    def test_a_rendered_line_names_an_absolute_executable_and_runs_without_path(self):
        state = os.path.dirname(str(self.store.path))
        channel = self.build_channel(state_directory=state)
        message_id = channel.stage(self.obligation())["messageId"]
        channel.attempt(message_id, self.adapter)
        written = self.bytes_of(message_id)
        full = [line.split("Full record: ", 1)[1] for line in written.splitlines()
                if line.startswith("Full record: ")][0]
        readback = [line.strip() for line in written.splitlines()
                    if " supervisor-read " in line][0]
        evidence = json.loads(channel.get(message_id)["packet"])["evidence"]
        for line in (full, readback, *evidence):
            program = shlex.split(line)[0]
            self.assertTrue(os.path.isabs(program), "a rendered line depends on PATH: " + line)
            self.assertTrue(os.access(program, os.X_OK), line)
        environment = {name: value for name, value in os.environ.items()
                       if name != "CODEX_SESSION_RELAY_STATE"}
        environment["PATH"] = os.path.join(self.tmp, "no-such-bin")
        try:
            done = subprocess.run(shlex.split(full), capture_output=True, text=True,
                                  env=environment, timeout=120)
        except FileNotFoundError as missing:
            self.fail(f"the rendered line did not run as rendered: {missing}")
        self.assertEqual(done.returncode, 0, done.stderr[-2000:])
        self.assertEqual(json.loads(done.stdout)["messageId"], message_id)


class F4AnOmissionSaysWhatItIs(StoreOmissionCase):
    """The delivered bytes are the only part of a report most readers see."""

    def test_the_delivered_bytes_name_the_omission_and_the_readings_reason(self):
        self.claim_through_cli()
        self.the_turn_ends()
        self.tick(advance=self.grace + 1)
        message = self.the_omission()
        self.assertEqual(message["state"], DISPATCHED)
        row = self.store.one("SELECT reading FROM supervisor_messages WHERE message_id = ?",
                             (message["message_id"],))
        reading = json.loads(row["reading"])
        written = self.store.one(
            "SELECT message FROM supervisor_attempts WHERE message_id = ?"
            " ORDER BY attempt_no DESC LIMIT 1", (message["message_id"],))["message"]
        self.assertNotIn("<unknown: no generation or revision was read>", written)
        self.assertIn(supervision.UNREPORTED, written, written)
        self.assertIn(reading["reason"], written, written)
        self.assertIn("generation " + str(reading["executionGeneration"]), written, written)
        self.assertIn(reading["selectors"]["turn"], written, written)


if __name__ == "__main__":
    unittest.main()
