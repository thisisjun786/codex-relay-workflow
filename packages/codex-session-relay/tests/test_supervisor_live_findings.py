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
from codex_session_relay.errors import DeliveryRefused, RefusalReason
from codex_session_relay.fakehost import FakeHostAdapter
from codex_session_relay.settings import TaskSettings
from codex_session_relay.transport import (
    DISPATCHED,
    SETTINGS_REFUSALS,
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


class F2EveryShapeTheComparisonReads(_Seam):
    """The post-load comparison reads lists on both sides; any other shape is a named refusal.

    Found by the fresh-context review of ac6cd1f9. A record whose roots were the TEXT "/a/bc"
    passed recording, and the narrower-only allowance then iterated its characters, so a host
    root "/" read as within the record and the send started. And a host that answered roots 123
    raised inside the comparison, which the transport records as an unknown outcome rather than
    the refusal before sending that it was. One rule closes both: every value the comparison
    reads is the shape it assumes, on the record when it is recorded and on the answer when it
    arrives, or nothing starts and the refusal says which.
    """

    MALFORMED_RECORDS = {
        "roots as text": {"runtimeWorkspaceRoots": "/a/bc"},
        "roots as a number": {"runtimeWorkspaceRoots": 7},
        "a root that is not text": {"runtimeWorkspaceRoots": [seam.WORKTREE, 7]},
        "environments as text": {"environments": "local"},
        "an environment that is not an object": {"environments": ["local"]},
        "an environment id that is not text": {
            "environments": [{"environmentId": 7, "cwd": seam.WORKTREE}]},
        "an environment without a cwd": {"environments": [{"environmentId": "local"}]},
        "environment roots as text": {"environments": [
            {"environmentId": "local", "cwd": seam.WORKTREE, "runtimeWorkspaceRoots": "/a/bc"}]},
        # Null is not absent: an absent list defaults to the cwd, a null one says nothing.
        "environment roots null": {"environments": [
            {"environmentId": "local", "cwd": seam.WORKTREE, "runtimeWorkspaceRoots": None}]},
    }

    @staticmethod
    def _answer(**overrides):
        return seam.authorized_resume_response(**overrides)

    def malformed_answers(self):
        not_an_object = self._answer()
        not_an_object["thread"] = "thread-1"
        return {
            "roots as a number": self._answer(runtimeWorkspaceRoots=123),
            "roots as text": self._answer(runtimeWorkspaceRoots=seam.WORKTREE),
            "a root that is not text": self._answer(runtimeWorkspaceRoots=[123]),
            "environments as text": self._answer(thread={"environments": "local"}),
            "an environment that is not an object": self._answer(thread={"environments": [7]}),
            "an environment without an id": self._answer(
                thread={"environments": [{"cwd": seam.WORKTREE}]}),
            "environment roots as a number": self._answer(thread={"environments": [
                {"environmentId": "local", "cwd": seam.WORKTREE, "runtimeWorkspaceRoots": 123}]}),
            "environment roots null": self._answer(thread={"environments": [
                {"environmentId": "local", "cwd": seam.WORKTREE,
                 "runtimeWorkspaceRoots": None}]}),
            "a thread that is not an object": not_an_object,
        }

    def test_a_record_the_comparison_cannot_read_is_refused_when_recorded(self):
        for label, override in self.MALFORMED_RECORDS.items():
            with self.subTest(label):
                view = TaskSettings(dict(seam.AUTHORIZED.data, **override))
                self.assertEqual(view.missing(), [], "complete, so only the shape can refuse it")
                try:
                    view.require_usable()
                except DeliveryRefused as refused:
                    self.assertEqual(refused.reason, RefusalReason.SETTINGS_MISTYPED)
                    self.assertIn(next(iter(override)), refused.detail)
                else:
                    self.fail("a record the comparison cannot read was accepted: " + label)

    def test_a_record_read_as_characters_never_admits_a_wider_root(self):
        """The review's reproducer, at the real adapter, past the recorder: "/" must not start."""
        wider = ["/"]
        cases = {
            "top-level roots": (
                {"runtimeWorkspaceRoots": "/a/bc"},
                self._answer(runtimeWorkspaceRoots=wider)),
            "environment roots": (
                {"environments": [{"environmentId": "local", "cwd": seam.WORKTREE,
                                   "runtimeWorkspaceRoots": "/a/bc"}]},
                self._answer(thread={"environments": [
                    {"environmentId": "local", "cwd": seam.WORKTREE,
                     "runtimeWorkspaceRoots": wider}]})),
        }
        for number, (label, (override, answer)) in enumerate(cases.items(), start=1):
            with self.subTest(label):
                adapter, calls = self._adapter(resume=answer, status="notLoaded")
                receipt = adapter.send_message(
                    f"sup-30000000000{number}-a1", "thread-1", "hi",
                    record_based(dict(seam.AUTHORIZED.data, **override)))
                self.assertNotIn("turn/start", self._methods(calls),
                                 "a root wider than the record was admitted")
                self.assertEqual(receipt["status"], "failed", receipt)
                self.assertIn(receipt["rpcError"]["code"], SETTINGS_REFUSALS)

    def test_an_answer_the_comparison_cannot_read_is_a_named_refusal_before_any_turn(self):
        """On both routes: a pair no policy derived, and a pair policy derived."""
        for number, (label, answer) in enumerate(self.malformed_answers().items(), start=1):
            for flagged in (True, False):
                with self.subTest(label, settings_free=flagged):
                    settings = (record_based() if flagged
                                else TaskSettings(dict(seam.AUTHORIZED.data)))
                    adapter, calls = self._adapter(resume=answer, status="notLoaded")
                    try:
                        receipt = adapter.send_message(
                            f"sup-4{int(flagged)}{number:010d}-a1", "thread-1", "hi", settings)
                    except Exception as error:  # noqa: BLE001 - the defect is an exception
                        self.fail(f"the comparison raised {error!r} on {label}")
                    self.assert_withheld_before_any_turn(receipt, calls, "setting_unobservable")

    def test_the_comparison_never_raises_on_a_shape_it_cannot_read(self):
        """The fake host calls the same comparison, so this is its route as well."""
        for label, answer in self.malformed_answers().items():
            for transmitted in (True, False):
                with self.subTest(label, transmitted=transmitted):
                    try:
                        findings = TaskSettings(dict(seam.AUTHORIZED.data)).mismatches(
                            answer, transmitted=transmitted)
                    except Exception as error:  # noqa: BLE001 - the defect is an exception
                        self.fail(f"the comparison raised {error!r} on {label}")
                    self.assertTrue(findings, "an unreadable answer read as agreement")
                    self.assertEqual(findings[0]["code"], "setting_unobservable", findings)

    def test_writable_roots_that_are_not_text_are_unreadable_on_both_sides(self):
        """Fresh-context review 2 of a3c0bad1: the sandbox's own roots list was the one left out.

        A record and an answer that both held writableRoots [123] compared equal, so the send
        started on a sandbox nobody can read. Unreadable on either side is a refusal before any
        turn, as it already was for a writableRoots that is not a list at all.
        """
        bad = dict(seam.AUTHORIZED_POLICY, writableRoots=[123])
        view = TaskSettings(dict(seam.AUTHORIZED.data, sandbox=bad))
        with self.subTest("the recorder"):
            try:
                view.require_usable()
            except DeliveryRefused as refused:
                self.assertEqual(refused.reason, RefusalReason.UNSUPPORTED_SANDBOX_TYPE)
            else:
                self.fail("a sandbox whose writableRoots are not text was accepted")
        cases = {
            "record and answer alike": (dict(seam.AUTHORIZED.data, sandbox=bad),
                                        self._answer(sandbox=bad)),
            "answer only": (dict(seam.AUTHORIZED.data), self._answer(sandbox=bad)),
        }
        for number, (label, (record, answer)) in enumerate(cases.items(), start=1):
            for flagged in (True, False):
                with self.subTest(label, settings_free=flagged):
                    settings = record_based(record) if flagged else TaskSettings(record)
                    adapter, calls = self._adapter(resume=answer, status="notLoaded")
                    try:
                        receipt = adapter.send_message(
                            f"sup-6{int(flagged)}{number:010d}-a1", "thread-1", "hi", settings)
                    except Exception as error:  # noqa: BLE001 - the defect is an exception
                        self.fail(f"the comparison raised {error!r} on {label}")
                    # An unreadable sandbox is never agreement (settings.mismatches), so it is
                    # a difference: renamed on the route that transmitted nothing.
                    self.assert_withheld_before_any_turn(
                        receipt, calls,
                        "settings_differ_after_load" if flagged else "settings_not_preserved")

    def test_every_declared_sandbox_field_is_its_declared_type_on_both_sides(self):
        """Fresh-context review 3 of c4e7692e: Python equality is not a type check.

        A record holding networkAccess 0 agreed with a host answering false, because 0 == False,
        and a record and an answer that both held networkAccess [1] agreed too, so a turn started
        on a sandbox the record cannot prove. Every field the declared defaults name holds its
        default's type on either side, and the record and the answer are compared as JSON
        values, where 0 and false differ.
        """
        wrong = [("networkAccess", 0), ("networkAccess", 1), ("networkAccess", [1]),
                 ("networkAccess", "false"), ("networkAccess", None),
                 ("excludeTmpdirEnvVar", 0), ("excludeSlashTmp", 1),
                 ("writableRoots", "/tmp"), ("writableRoots", [7])]
        for field, value in wrong:
            with self.subTest("the recorder", field=field, value=value):
                view = TaskSettings(dict(seam.AUTHORIZED.data,
                                         sandbox=dict(seam.AUTHORIZED_POLICY, **{field: value})))
                try:
                    view.require_usable()
                except DeliveryRefused as refused:
                    self.assertEqual(refused.reason, RefusalReason.UNSUPPORTED_SANDBOX_TYPE)
                else:
                    self.fail(f"a sandbox holding {field} {value!r} was accepted")
        zero = dict(seam.AUTHORIZED_POLICY, networkAccess=0)
        listed = dict(seam.AUTHORIZED_POLICY, networkAccess=[1])
        cases = {
            "record 0, answer false": (dict(seam.AUTHORIZED.data, sandbox=zero),
                                       self._answer()),
            "record false, answer 0": (dict(seam.AUTHORIZED.data), self._answer(sandbox=zero)),
            "record and answer both [1]": (dict(seam.AUTHORIZED.data, sandbox=listed),
                                           self._answer(sandbox=listed)),
        }
        for number, (label, (record, answer)) in enumerate(cases.items(), start=1):
            for flagged in (True, False):
                with self.subTest(label, settings_free=flagged):
                    settings = record_based(record) if flagged else TaskSettings(record)
                    adapter, calls = self._adapter(resume=answer, status="notLoaded")
                    receipt = adapter.send_message(
                        f"sup-7{int(flagged)}{number:010d}-a1", "thread-1", "hi", settings)
                    self.assert_withheld_before_any_turn(
                        receipt, calls,
                        "settings_differ_after_load" if flagged else "settings_not_preserved")

    def test_absent_environment_roots_still_mean_the_cwd(self):
        """TurnEnvironmentParams: an omitted roots list defaults to the cwd. Only null is refused."""
        record = dict(seam.AUTHORIZED.data,
                      environments=[{"environmentId": "local", "cwd": seam.WORKTREE}])
        answer = self._answer(thread={"environments": [
            {"environmentId": "local", "cwd": seam.WORKTREE}]})
        try:
            TaskSettings(record).require_usable()
        except DeliveryRefused as refused:
            self.fail(f"an environment with no roots list was refused: {refused}")
        for number, flagged in enumerate((True, False), start=1):
            with self.subTest(settings_free=flagged):
                settings = record_based(record) if flagged else TaskSettings(record)
                adapter, calls = self._adapter(resume=answer, status="notLoaded")
                receipt = adapter.send_message(
                    f"sup-90000000000{number}-a1", "thread-1", "hi", settings)
                self.assertEqual(receipt["status"], "accepted", receipt.get("error"))

    PROFILE = {"id": "profile-1", "extends": None, "rules": []}

    def test_a_permission_profile_is_the_hosts_whole_value_and_agrees_only_as_json(self):
        """relay.md: carry activePermissionProfile WHOLE, an object, into the record.

        Devin on 16ea6c8b: typing expectedPermissionProfile as text refused the object a
        creation receipt reports, so such a task could not be registered or sent to. The record
        holds the host's value as it is; agreement is JSON equality, where 0 and false differ,
        key order does not matter, and an absent extends is not a null one.
        """
        with self.subTest("the recorder takes the object"):
            try:
                TaskSettings(dict(seam.AUTHORIZED.data,
                                  expectedPermissionProfile=self.PROFILE)).require_usable()
            except DeliveryRefused as refused:
                self.fail(f"the profile a creation receipt reports was refused: {refused}")
        reordered = {"rules": [], "extends": None, "id": "profile-1"}
        agreeing = {
            "the same object, keys in another order": (self.PROFILE, reordered),
        }
        absent = object()
        # (recorded, reported, the refusal): the last two are review 5 of d88c169e - a recorded
        # profile the answer does not report was skipped, and the turn started unverified.
        refusing = {
            "record 0, answer false": (0, False, "unverifiable_permission_profile"),
            "extends null in the record, absent in the answer": (
                self.PROFILE, {"id": "profile-1", "rules": []},
                "unverifiable_permission_profile"),
            "a recorded profile, answered null": (self.PROFILE, None, "setting_unobservable"),
            "a recorded profile, not answered at all": (
                self.PROFILE, absent, "setting_unobservable"),
        }
        number = 0
        for label, case in {**agreeing, **refusing}.items():
            recorded, reported = case[:2]
            for flagged in (True, False):
                number += 1
                with self.subTest(label, settings_free=flagged):
                    record = dict(seam.AUTHORIZED.data, expectedPermissionProfile=recorded)
                    settings = record_based(record) if flagged else TaskSettings(record)
                    answer = self._answer(activePermissionProfile=reported)
                    if reported is absent:
                        del answer["activePermissionProfile"]
                    adapter, calls = self._adapter(resume=answer, status="notLoaded")
                    receipt = adapter.send_message(
                        f"sup-8{number:011d}-a1", "thread-1", "hi", settings)
                    if label in agreeing:
                        self.assertEqual(receipt["status"], "accepted", receipt.get("error"))
                    else:
                        self.assert_withheld_before_any_turn(receipt, calls, case[2])
        for number, flagged in enumerate((True, False), start=1):
            with self.subTest("no profile recorded, none answered", settings_free=flagged):
                record = dict(seam.AUTHORIZED.data)
                settings = record_based(record) if flagged else TaskSettings(record)
                adapter, calls = self._adapter(resume=self._answer(), status="notLoaded")
                receipt = adapter.send_message(
                    f"sup-85000000000{number}-a1", "thread-1", "hi", settings)
                self.assertEqual(receipt["status"], "accepted", receipt.get("error"))

    def test_the_fake_host_refuses_an_answer_it_cannot_read(self):
        host = FakeHostAdapter(clock=None)
        host.add_thread("thread-1", status="notLoaded",
                        loaded_settings=self._answer(runtimeWorkspaceRoots=123))
        try:
            receipt = host.send_message("sup-500000000001-a1", "thread-1", "hi", record_based())
        except Exception as error:  # noqa: BLE001 - the defect is an exception
            self.fail(f"the fake host raised {error!r}")
        self.assertEqual(receipt["status"], "failed", receipt)
        self.assertEqual(receipt["rpcError"]["code"], "setting_unobservable")
        self.assertEqual(host.threads["thread-1"].status, "notLoaded", "nothing was started")


if __name__ == "__main__":
    unittest.main()
