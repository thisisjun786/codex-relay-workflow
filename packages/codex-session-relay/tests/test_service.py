"""Who owns the daemon, and who is allowed to stop it.

These tests use real child processes on purpose. A mocked flock proves that the code called
flock; only a second process trying to start proves that the first one is actually excluded.
"""

import json
import os
import shutil
import subprocess
import sys
import tempfile
import time
import unittest
from pathlib import Path
from unittest import mock

from codex_session_relay import service as service_module
from codex_session_relay.policy import RetryPolicy
from codex_session_relay.service import (
    ISOLATED, PRODUCTION, ProcessHandle, RelayService, ScopeRegistry, ServiceRefused,
    installation_id, owned_service, production_scope_root, resolve_scope_root,
)
from codex_session_relay.store import Store, resolve_state_dir

REPO = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
SOCKET = "/nonexistent-app-server.sock"

# A child that takes the claim and holds it until told to stop, so exclusion is observed
# rather than asserted against a double.
HOLDER = """
import os, sys, time
sys.path.insert(0, {src!r})
from codex_session_relay.service import RelayService, ScopeRegistry, owned_service
from codex_session_relay.store import resolve_state_dir
service = RelayService(
    resolve_state_dir({state!r}), socket_path={socket!r},
    scope=ScopeRegistry(__import__("pathlib").Path({scopes!r}), "isolated"),
    store_id={store_id!r},
)
service.launch_id = {launch!r}
with owned_service(service, allow_isolated=True, require_intent=False) as record:
    # What supervise() writes once on_start has returned: this child is serving.
    service._note(readyAt="2026-01-01T00:00:00Z")
    # The record file IS the handshake. A pipe would add buffering and lifetime questions
    # that have nothing to do with what this test is about.
    while True:
        time.sleep(0.05)
"""

# A child parked in the window supervision leaves behind on its way out: the launch is
# recorded, the pid has been cleared, and the daemon lock is not released until the context
# exits. The record is written once, already cleared, so the parent cannot observe a live
# pid first and pass for the wrong reason.
FINISHING = """
import os, sys, time
sys.path.insert(0, {src!r})
from codex_session_relay.daemon import SingleInstance
from codex_session_relay.service import RelayService, ScopeRegistry
from codex_session_relay.store import resolve_state_dir
service = RelayService(
    resolve_state_dir({state!r}), socket_path={socket!r},
    scope=ScopeRegistry(__import__("pathlib").Path({scopes!r}), "isolated"),
    store_id={store_id!r},
)
service.launch_id = {launch!r}
with SingleInstance(service.selection.path):
    service.write_record(dict(
        service.new_record(pid=os.getpid()),
        readyAt="2026-01-01T00:00:00Z", pid=None, workerPid=None, stoppedAt="cleanup",
    ))
    while True:
        time.sleep(0.05)
"""


class ServiceTestCase(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.mkdtemp(prefix="relay-service-")
        self.addCleanup(shutil.rmtree, self.tmp, ignore_errors=True)
        self.scopes = os.path.join(self.tmp, "scopes")
        self.children = []
        self.addCleanup(self._reap)

    def _reap(self):
        for child in self.children:
            if child.poll() is None:
                child.kill()
            try:
                child.wait(timeout=10)
            except subprocess.TimeoutExpired:  # pragma: no cover - a hung child is a failure
                self.fail(f"child {child.pid} did not exit")

    def service(self, name="a", *, socket=SOCKET, scopes=None):
        state = os.path.join(self.tmp, name)
        os.makedirs(state, exist_ok=True)
        store = Store(Path(state) / "relay.sqlite3")
        store_id = store.identity
        store.close()
        return RelayService(
            resolve_state_dir(state), socket_path=socket,
            scope=ScopeRegistry(Path(scopes or self.scopes), ISOLATED), store_id=store_id,
        )

    def holder(self, service, *, launch=None):
        """Start a child that really holds the lock and the claim, and wait until it does."""
        program = HOLDER.format(
            src=os.path.join(REPO, "src"), state=str(service.selection.path),
            socket=service.socket_path, scopes=str(service.scope.root),
            store_id=service.store_id, launch=launch,
        )
        child = subprocess.Popen(
            [sys.executable, "-c", program], stdout=subprocess.DEVNULL,
            stderr=subprocess.PIPE, text=True,
        )
        self.children.append(child)
        deadline = time.monotonic() + 20.0
        while time.monotonic() < deadline:
            record = service.record()
            if record and record.get("pid") and service.lock_is_held():
                return child, record["pid"]
            if child.poll() is not None:
                self.fail(f"holder exited {child.returncode}: {child.stderr.read()}")
            time.sleep(0.02)
        self.fail("the holder never took the claim")


class Ownership(ServiceTestCase):
    def test_a_second_start_is_refused_and_the_first_is_untouched(self):
        service = self.service("a")
        child, pid = self.holder(service)
        service.intent.write(enabled=True, actor="test")
        refused = service.start(allow_isolated=True, launcher=self._never_launch)
        self.assertFalse(refused["ok"])
        self.assertEqual(refused["reason"], "already_running")
        self.assertIsNone(child.poll(), "the running service must not be disturbed")

    def _never_launch(self, *_args, **_kwargs):  # pragma: no cover - reaching it is the failure
        self.fail("start launched a second process while one was already running")

    def test_a_different_state_directory_cannot_serve_the_same_socket(self):
        """The hole a per-state-directory lock cannot close."""
        first = self.service("a")
        self.holder(first)
        second = self.service("b")
        self.assertNotEqual(second.store_id, first.store_id)
        second.intent.write(enabled=True, actor="test")
        refused = second.start(allow_isolated=True, launcher=self._never_launch)
        self.assertFalse(refused["ok"])
        self.assertEqual(refused["reason"], "scope_owned_by_other_store")
        self.assertEqual(refused["conflicts"][0]["storeId"], first.store_id)

    def test_a_stopped_registration_on_another_store_is_still_a_conflict(self):
        """Live-only scanning would miss this one entirely."""
        first = self.service("a")
        child, _pid = self.holder(first)
        child.terminate()
        child.wait(timeout=10)
        second = self.service("b")
        conflicts = second.conflicts()
        self.assertEqual([c["storeId"] for c in conflicts], [first.store_id])
        self.assertFalse(conflicts[0]["live"])

    def test_a_stopped_registration_is_not_silently_overwritten(self):
        """Overwriting it would erase the only evidence two stores served one socket."""
        first = self.service("a")
        child, _pid = self.holder(first)
        child.terminate()
        child.wait(timeout=10)
        second = self.service("b")
        refused = second.scope.claim(
            second.socket_path, second.new_record(pid=os.getpid()),
        )
        self.assertFalse(refused["ok"])
        self.assertEqual(refused["reason"], "scope_registered_to_other_store")
        self.assertEqual(refused["held_by"]["storeId"], first.store_id)

    def test_a_definite_mismatch_beats_a_missing_start_time(self):
        """Installation identity already proves foreign; unverifiable would discard that."""
        service = self.service("a")
        child, _pid = self.holder(service)
        record = service.record()
        service.write_record(
            dict(record, startTicks=None, installationId="someone-else"),
        )
        refused = service.stop()
        self.assertEqual(refused["reason"], "not_ours")
        self.assertIn("another installation", refused["detail"])
        self.assertIsNone(child.poll())

    def test_stop_refuses_a_process_another_installation_owns(self):
        service = self.service("a")
        child, pid = self.holder(service)
        record = service.record()
        service.write_record(dict(record, installationId="someone-else"))
        refused = service.stop()
        self.assertFalse(refused["ok"])
        self.assertEqual(refused["reason"], "not_ours")
        self.assertIn("another installation", refused["detail"])
        self.assertIsNone(child.poll(), "a foreign process must not be signalled")

    def test_disable_refuses_a_foreign_service_without_touching_shared_intent(self):
        """service.json is shared by everything pointed at this state directory.

        Writing enabled=false and only then discovering the supervisor is foreign refused
        the stop while still shutting that supervisor down at its next worker boundary,
        because it re-reads intent there. A refusal that still has an effect is not one.
        """
        service = self.service("a")
        service.enable(actor="owner")
        child, _pid = self.holder(service)
        service.write_record(dict(service.record(), installationId="someone-else"))

        refused = service.disable(actor="intruder")

        self.assertFalse(refused["ok"], refused)
        self.assertEqual(refused["reason"], "not_ours")
        self.assertTrue(
            service.intent.read()["enabled"],
            "the owner's intent must survive a refused disable",
        )
        self.assertIsNone(child.poll())
        self.assertFalse(service.stop_request_path.exists())

    def test_a_record_with_no_boot_id_is_unverifiable(self):
        """A reboot resets the pid space AND the start-tick counter together.

        Without a recorded boot, a matching pid with a matching tick count proves nothing:
        an unrelated process holding the old number passes both checks and would be signalled.
        """
        service = self.service("a")
        child, _pid = self.holder(service)
        service.write_record(dict(service.record(), bootId=None))

        refused = service.stop()

        self.assertFalse(refused["ok"], refused)
        self.assertEqual(refused["reason"], "ownership_unverifiable")
        self.assertIn("boot id", refused["detail"])
        self.assertIsNone(child.poll(), "refusing is the point")

    def test_enable_refuses_to_reverse_a_foreign_owners_intent(self):
        """service.json is shared, and disable already asks this question.

        An owner who has just disabled a service whose supervisor is still exiting must not
        have that reversed by another installation, leaving it eligible to restart.
        """
        service = self.service("a")
        service.enable(actor="owner")
        child, _pid = self.holder(service)
        service.intent.write(enabled=False, actor="owner")
        service.write_record(dict(service.record(), installationId="someone-else"))

        refused = service.enable(actor="intruder")

        self.assertFalse(refused["ok"], refused)
        self.assertEqual(refused["reason"], "not_ours")
        self.assertFalse(
            service.intent.read()["enabled"],
            "the owner's disable must not be reversed by another installation",
        )
        self.assertIsNone(child.poll())

    def test_enable_still_works_when_nothing_is_running_here(self):
        """A stopped registration is not a reason to make a state directory unusable."""
        service = self.service("b")
        service.write_record(dict(
            service.new_record(pid=os.getpid()), installationId="someone-else",
        ))

        enabled = service.enable(actor="owner")

        self.assertTrue(enabled["ok"], enabled)
        self.assertTrue(service.intent.read()["enabled"])

    def test_disable_still_works_on_a_service_this_installation_owns(self):
        service = self.service("b")
        service.enable(actor="owner")
        child, _pid = self.holder(service)

        disabled = service.disable(actor="owner")

        self.assertTrue(disabled["ok"], disabled)
        self.assertFalse(service.intent.read()["enabled"])
        child.wait(timeout=10)
    def test_stop_refuses_a_recycled_pid(self):
        service = self.service("a")
        child, pid = self.holder(service)
        record = service.record()
        service.write_record(dict(record, startTicks=(record["startTicks"] or 0) + 1))
        refused = service.stop()
        self.assertEqual(refused["reason"], "not_ours")
        self.assertIn("reused", refused["detail"])
        self.assertIsNone(child.poll())

    def test_stop_refuses_when_the_process_cannot_be_aimed_at(self):
        service = self.service("a")
        child, pid = self.holder(service)
        with mock.patch.object(os, "pidfd_open", side_effect=OSError("no pidfd here")):
            refused = service.stop()
        self.assertEqual(refused["reason"], "ownership_unverifiable")
        self.assertIsNone(child.poll(), "refusing is the point; never fall back to os.kill")
        self.assertFalse(
            service.stop_request_path.exists(),
            "a refused stop must leave no request behind; it would halt someone else",
        )

    def test_a_record_without_a_start_time_is_unverifiable_not_ours(self):
        """A pid with no start time is just a number, and anything holding it would pass."""
        service = self.service("a")
        child, _pid = self.holder(service)
        record = service.record()
        service.write_record(dict(record, startTicks=None))
        refused = service.stop()
        self.assertEqual(refused["reason"], "ownership_unverifiable")
        self.assertIn("start time", refused["detail"])
        self.assertIsNone(child.poll())
        self.assertFalse(service.stop_request_path.exists())

    def test_stop_terminates_a_process_this_installation_owns(self):
        service = self.service("a")
        child, pid = self.holder(service)
        stopped = service.stop()
        self.assertTrue(stopped["ok"], stopped)
        self.assertEqual(stopped["supervisor"], "exited")
        child.wait(timeout=10)
        self.assertIsNone(service.record()["pid"])
        self.assertFalse(service.lock_is_held())

    def test_stop_reaches_the_worker_the_record_names_now(self):
        """A supervisor replacing a worker while stop runs left the first read stale.

        Stopping the worker that already exited reports success while its replacement,
        which holds the inherited locks, is still delivering.
        """
        service = self.service("a")
        child, _pid = self.holder(service)
        first = subprocess.Popen(
            [sys.executable, "-c", "import time\nwhile True:\n    time.sleep(0.05)\n"],
            stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
        )
        self.children.append(first)
        first.terminate()
        first.wait(timeout=10)
        service.write_record(dict(
            service.record(), workerPid=first.pid, workerStartTicks=1,
        ))
        replacement = subprocess.Popen(
            [sys.executable, "-c", "import time\nwhile True:\n    time.sleep(0.05)\n"],
            stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
        )
        self.children.append(replacement)
        original = service._terminate

        def replace_then_terminate(handle, **kwargs):
            # The supervisor swapping workers in the window stop() used to read across.
            service.write_record(dict(
                service.record(), workerPid=replacement.pid,
                workerStartTicks=service_module.start_ticks(replacement.pid),
            ))
            return original(handle, **kwargs)

        with mock.patch.object(service, "_terminate", replace_then_terminate):
            stopped = service.stop()

        child.wait(timeout=10)
        self.assertIsNotNone(
            replacement.poll(), "the worker the record names NOW is the one stop must reach",
        )
        self.assertEqual(stopped["worker"], "exited")
    def test_a_stale_record_does_not_block_a_fresh_start(self):
        service = self.service("a")
        child, pid = self.holder(service)
        child.terminate()
        child.wait(timeout=10)
        status = service.status()
        self.assertFalse(status["running"])
        self.assertEqual(status["ownership"], "none")
        self.assertFalse(service.lock_is_held())


class LaunchReporting(ServiceTestCase):
    """start reports what the child managed to do, and a finished launch did nothing."""

    def finishing_launcher(self):
        """A launcher whose child reaches supervision's cleanup window and stays there."""
        def launcher(service, **_kw):
            program = FINISHING.format(
                src=os.path.join(REPO, "src"), state=str(service.selection.path),
                socket=service.socket_path, scopes=str(service.scope.root),
                store_id=service.store_id, launch=service.launch_id,
            )
            child = subprocess.Popen(
                [sys.executable, "-c", program], stdout=subprocess.DEVNULL,
                stderr=subprocess.PIPE, text=True,
            )
            self.children.append(child)
            return child
        return launcher

    def test_a_launch_that_already_finished_is_not_reported_as_running(self):
        """A bound small enough to finish during startup - or an expired deadline.

        The launch id still matches and the daemon lock is still held, so matching on
        those two alone hands the caller success for a service that is on its way out.
        """
        service = self.service("a")
        service.enable(actor="test")

        outcome = service.start(
            allow_isolated=True, launcher=self.finishing_launcher(), timeout=2.0,
        )

        self.assertFalse(outcome["ok"], f"a cleared pid is not a running service: {outcome}")
        self.assertIsNone(outcome.get("pid"))
        self.assertEqual(outcome["reason"], "did_not_report")

    def test_a_live_launch_is_still_reported_as_running(self):
        """The same path with a pid in the record, so the guard is not refusing everything."""
        service = self.service("b")
        service.enable(actor="test")
        started = {}

        def launcher(svc, **_kw):
            child, pid = self.holder(svc, launch=svc.launch_id)
            started["pid"] = pid
            return child

        outcome = service.start(allow_isolated=True, launcher=launcher, timeout=10.0)

        self.assertTrue(outcome["ok"], outcome)
        self.assertEqual(outcome["pid"], started["pid"])

    def test_a_launch_that_never_reports_is_not_left_running(self):
        """Reporting failure and walking away leaves the locks held against the retry.

        A child that is merely slow can come up after the caller was told the start failed,
        and it holds the daemon lock and the scope claim while it does.
        """
        service = self.service("c")
        service.enable(actor="test")
        program = "import time\nwhile True:\n    time.sleep(0.05)\n"

        def launcher(_svc, **_kw):
            child = subprocess.Popen(
                [sys.executable, "-c", program], stdout=subprocess.DEVNULL,
                stderr=subprocess.DEVNULL,
            )
            self.children.append(child)
            return child

        outcome = service.start(allow_isolated=True, launcher=launcher, timeout=0.5)

        self.assertFalse(outcome["ok"], outcome)
        self.assertEqual(outcome["reason"], "did_not_report")
        self.assertEqual(outcome["child"], "terminated")
        self.assertIsNotNone(self.children[-1].poll(), "the child this call started is gone")

class ForeignWorkers(ServiceTestCase):
    """A dead supervisor does not make its installation's worker ours to signal."""

    def test_a_foreign_record_whose_supervisor_died_still_protects_its_worker(self):
        """The gone check answered before the foreign markers were ever read.

        stop() deliberately falls through to the worker when the supervisor is gone, because
        that orphan is exactly what a stop has to reach. Classifying a foreign record as
        none first aimed that fall-through at another installation's worker.
        """
        service = self.service("a")
        child, worker_pid = self.holder(service)
        record = service.record()
        service.write_record(dict(
            record, installationId="someone-else", workerPid=worker_pid,
            workerStartTicks=service_module.start_ticks(worker_pid),
        ))
        real = os.pidfd_open

        def supervisor_is_gone(pid, *args, **kwargs):
            if pid == record["pid"]:
                raise ProcessLookupError(f"supervisor {pid} is gone")
            return real(pid, *args, **kwargs)

        with mock.patch.object(os, "pidfd_open", supervisor_is_gone):
            refused = service.stop()

        self.assertFalse(refused["ok"], refused)
        self.assertEqual(refused["reason"], "not_ours")
        self.assertIn("another installation", refused["detail"])
        self.assertEqual(refused["worker"], "untouched")
        self.assertIsNone(child.poll(), "a foreign worker must not be signalled")
        self.assertFalse(
            service.stop_request_path.exists(),
            "a refused stop must not leave a request that halts another installation",
        )

    def test_an_orphaned_foreign_worker_is_still_not_ours_to_signal(self):
        """The record supervise() now leaves behind: pid cleared, worker identity kept.

        Keeping the worker identity is what lets a stop reach an orphan, and the ownership
        check returned none as soon as the supervisor pid was gone - so that orphan path
        pointed straight at another installation's worker.
        """
        service = self.service("a")
        child, worker_pid = self.holder(service)
        service.write_record(dict(
            service.record(), pid=None, installationId="someone-else",
            workerPid=worker_pid,
            workerStartTicks=service_module.start_ticks(worker_pid),
        ))

        refused = service.stop()

        self.assertFalse(refused["ok"], refused)
        self.assertEqual(refused["reason"], "not_ours")
        self.assertEqual(refused["worker"], "untouched")
        self.assertIsNone(child.poll(), "a foreign orphan must not be signalled")
        self.assertFalse(service.stop_request_path.exists())

    def test_an_orphaned_worker_of_our_own_is_still_reachable(self):
        """The guard must not refuse the case it was added to support."""
        service = self.service("b")
        child, worker_pid = self.holder(service)
        service.write_record(dict(
            service.record(), pid=None, workerPid=worker_pid,
            workerStartTicks=service_module.start_ticks(worker_pid),
        ))

        stopped = service.stop()

        child.wait(timeout=10)
        self.assertEqual(stopped["worker"], "exited")

    def test_a_worker_with_no_recorded_start_time_is_unverifiable_not_killable(self):
        """The rule the supervisor already had. A reused worker pid is an unrelated process."""
        service = self.service("a")
        child, worker_pid = self.holder(service)
        service.write_record(dict(
            service.record(), workerPid=worker_pid, workerStartTicks=None,
        ))

        self.assertEqual(
            service._stop_worker(service.record(), timeout=1.0, grace=0.05), "unverifiable",
        )
        self.assertIsNone(child.poll(), "an unverifiable worker must not be signalled")


class RecordDurability(ServiceTestCase):
    def test_a_record_is_replaced_atomically_rather_than_truncated(self):
        """_note rewrites this file at every worker boundary; a reader must never see half.

        A concurrent stop reading the truncated middle would call a running supervisor
        absent and return not_running without ever signalling its worker.
        """
        service = self.service("a")
        service.write_record(service.new_record(pid=os.getpid()))
        before = service.record_path.stat()

        service._note(restarts=7)

        self.assertEqual(service.record()["restarts"], 7)
        self.assertNotEqual(
            service.record_path.stat().st_ino, before.st_ino,
            "an in-place rewrite is the truncation window; the file must be replaced",
        )
        self.assertEqual(
            [p.name for p in service.selection.path.glob(".daemon.json.*")], [],
            "no temporary file is left behind",
        )

    def test_the_intent_file_is_replaced_atomically_too(self):
        """read() calls an unreadable document not configured, which reads as disabled.

        The supervisor re-reads intent at every worker boundary, so a reader landing in the
        truncated middle of this write stops a service its owner left enabled.
        """
        service = self.service("a")
        service.enable(actor="test")
        before = service.intent.path.stat()

        service.intent.write(enabled=True, actor="owner")

        self.assertTrue(service.intent.read()["enabled"])
        self.assertNotEqual(service.intent.path.stat().st_ino, before.st_ino)
        self.assertEqual(
            [p.name for p in service.selection.path.glob(".service.json.*")], [],
        )


class SupervisorCleanup(ServiceTestCase):
    """What the supervisor leaves behind when it does not get to finish."""

    def test_a_worker_that_was_never_waited_on_keeps_its_identity(self):
        """The cleanup cleared workerPid unconditionally, including after an exception.

        The worker still holds the daemon lock and the scope claim it inherited, so erasing
        the only identity a stop can aim at leaves it delivering for the rest of its segment
        while status reports not_running.
        """
        service = self.service("a")
        service.enable(actor="test")
        worker = FakeWorker(0, pid=os.getpid())
        worker.wait = _explode

        with self.assertRaises(RuntimeError):
            service.supervise(allow_isolated=True, spawn=lambda **_k: worker,
                              sleeper=lambda _s: None, max_segments=1)

        record = service.record()
        self.assertIsNone(record["pid"], "the supervisor itself is gone")
        self.assertEqual(record["workerPid"], worker.pid,
                         "but the orphan it left behind is still reachable")
        self.assertIsNotNone(record["workerStartTicks"])

    def test_a_clean_run_still_clears_the_worker(self):
        service = self.service("b")
        service.enable(actor="test")
        service.supervise(allow_isolated=True, spawn=lambda **_k: FakeWorker(0),
                          sleeper=lambda _s: None, max_segments=1)
        record = service.record()
        self.assertIsNone(record["workerPid"])
        self.assertIsNone(record["workerStartTicks"])

    def test_the_restart_delay_never_outlives_the_supervisors_own_bound(self):
        """An unclamped delay made a 0.01-second deadline take seconds."""
        service = self.service("c")
        service.enable(actor="test")
        slept = []
        service.supervise(
            allow_isolated=True, spawn=lambda **_k: FakeWorker(1),
            sleeper=slept.append, max_segments=4, deadline=0.01,
            policy=RetryPolicy(restart_base_seconds=30.0, restart_backoff_max_seconds=300.0),
        )
        self.assertTrue(slept, "a failed segment does wait before its replacement")
        self.assertLess(
            max(slept), 30.0,
            "the unclamped delay is the policy interval, which outlives the whole bound",
        )
        self.assertTrue(all(value <= 0.01 for value in slept), slept)

    def test_a_long_failure_streak_keeps_retrying_at_the_cap(self):
        """The backoff built the product and clamped after, so it overflowed and killed the
        supervisor it was meant to pace.
        """
        policy = RetryPolicy()
        ceiling = policy.restart_backoff_max_seconds
        for failures in (1, 2, 10, 1024, 1025, 10 ** 6):
            delay = policy.restart_delay_for(failures)
            self.assertIsInstance(delay, (int, float))
            self.assertLessEqual(delay, ceiling)
        self.assertEqual(policy.restart_delay_for(10 ** 6), ceiling)
        self.assertLess(policy.restart_delay_for(1), policy.restart_delay_for(4))

    def test_the_bound_follows_the_policy_rather_than_a_fixed_step_count(self):
        """A small base needs more doublings, and a constant bound truncated its backoff."""
        # Deliberately extreme: this ratio needs about 70 doublings, so a fixed 64-step bound
        # returns the ceiling while the policy's own backoff still has room.
        fine = RetryPolicy(restart_base_seconds=1e-12, restart_backoff_max_seconds=1e9)
        self.assertLess(
            fine.restart_delay_for(65), fine.restart_backoff_max_seconds,
            "this policy has not reached its own ceiling yet",
        )
        self.assertLess(fine.restart_delay_for(65), fine.restart_delay_for(80))
        self.assertEqual(fine.restart_delay_for(10 ** 6), fine.restart_backoff_max_seconds)
        for failures in (1, 65, 1025, 10 ** 6):
            self.assertIsInstance(fine.restart_delay_for(failures), (int, float))


class Intent(ServiceTestCase):
    def test_start_never_enables_a_service_that_was_never_configured(self):
        service = self.service("a")
        refused = service.start(allow_isolated=True, launcher=self._fail)
        self.assertEqual(refused["reason"], "service_disabled")
        self.assertFalse(service.intent.read()["enabled"])
        self.assertFalse(service.intent.read()["configured"])
        self.assertFalse((service.selection.path / "service.json").exists())

    def _fail(self, *_a, **_k):  # pragma: no cover
        self.fail("a disabled service must not be launched")

    def test_restart_refuses_a_disabled_service_and_leaves_it_disabled(self):
        service = self.service("a")
        service.enable(actor="test")
        service.disable(actor="test")
        refused = service.restart(allow_isolated=True, launcher=self._fail)
        self.assertEqual(refused["reason"], "service_disabled")
        self.assertFalse(service.intent.read()["enabled"])

    def test_a_disable_during_restart_stops_the_replacement(self):
        """The window between restart stopping the old process and launching the new one."""
        service = self.service("a")
        service.enable(actor="test")
        original = service.stop

        def stop_then_disable(**kwargs):
            outcome = original(**kwargs)
            service.intent.write(enabled=False, actor="owner")
            return outcome

        with mock.patch.object(service, "stop", stop_then_disable):
            refused = service.restart(allow_isolated=True, launcher=self._fail)
        self.assertEqual(refused["reason"], "service_disabled")
        self.assertIn("while the service was stopping", refused["detail"])
        self.assertFalse(service.intent.read()["enabled"])


class ScopeAuthority(ServiceTestCase):
    def test_the_production_root_does_not_move_with_the_environment(self):
        fake = tempfile.mkdtemp(prefix="relay-passwd-home-")
        self.addCleanup(shutil.rmtree, fake, ignore_errors=True)
        entry = mock.Mock(pw_dir=fake)
        with mock.patch.object(service_module.pwd, "getpwuid", return_value=entry):
            with mock.patch.dict(os.environ, {"HOME": "/somewhere/else",
                                              "XDG_STATE_HOME": "/elsewhere"}, clear=False):
                first = production_scope_root()
            with mock.patch.dict(os.environ, {"HOME": "/a/third/place"}, clear=False):
                os.environ.pop("XDG_STATE_HOME", None)
                second = production_scope_root()
        self.assertEqual(first, second)
        self.assertTrue(str(first).startswith(fake))

    def test_an_override_is_isolated_and_refused_without_an_explicit_opt_in(self):
        """Two launches with DIFFERENT overrides would otherwise both own the same socket."""
        for name, root in (("a", "scopes-a"), ("b", "scopes-b")):
            service = self.service(name, scopes=os.path.join(self.tmp, root))
            service.intent.write(enabled=True, actor="test")
            refused = service.start(allow_isolated=False, launcher=self._fail)
            self.assertEqual(refused["reason"], "isolated_scope_not_allowed")
            self.assertIn("CODEX_SESSION_RELAY_SCOPE_DIR", refused["detail"])
            self.assertEqual(service.status()["scopeAuthority"], ISOLATED)

    def _fail(self, *_a, **_k):  # pragma: no cover
        self.fail("an isolated authority must not start without an explicit opt-in")

    def test_resolve_reports_production_when_nothing_is_overridden(self):
        with mock.patch.dict(os.environ, {}, clear=False):
            os.environ.pop("CODEX_SESSION_RELAY_SCOPE_DIR", None)
            _root, authority = resolve_scope_root()
        self.assertEqual(authority, PRODUCTION)

    def test_an_installation_is_this_checkout_and_this_state_directory(self):
        self.assertNotEqual(
            installation_id(os.path.join(self.tmp, "a")),
            installation_id(os.path.join(self.tmp, "b")),
        )

    def test_two_names_for_one_socket_are_one_operating_scope(self):
        """A symlinked socket is the same App Server, so it must be the same lock."""
        real = os.path.join(self.tmp, "real.sock")
        open(real, "w", encoding="utf-8").close()
        alias = os.path.join(self.tmp, "alias.sock")
        os.symlink(real, alias)
        registry = ScopeRegistry(Path(self.scopes), ISOLATED)
        self.assertEqual(
            registry.key(real), registry.key(alias),
            "two spellings of one socket must not give two owners",
        )


if __name__ == "__main__":
    unittest.main()


class FakeWorker:
    """Stands in for a bounded worker process, so cadence is tested without real time."""

    def __init__(self, code=0, pid=None):
        self.returncode = code
        self.pid = pid or os.getpid()

    def wait(self):
        return self.returncode


def _explode():
    """A worker the supervisor never gets an exit code from."""
    raise RuntimeError("the supervisor died between spawn and wait")



class Supervision(ServiceTestCase):
    def supervised(self, service, *, codes, **kwargs):
        """Run the supervisor over a scripted sequence of worker exits."""
        launches, slept = [], []
        queue = list(codes)

        def spawn(**call):
            launches.append(call)
            return FakeWorker(queue.pop(0) if queue else 0)

        outcome = service.supervise(
            allow_isolated=True, spawn=spawn, sleeper=slept.append,
            max_segments=len(codes), **kwargs,
        )
        return outcome, launches, slept

    def test_a_worker_that_exits_is_replaced_on_the_same_store(self):
        service = self.service("a")
        service.enable(actor="test")
        before = service.store_id
        outcome, launches, _slept = self.supervised(service, codes=[0, 0, 0])
        self.assertEqual(outcome["segments"], [0, 0, 0])
        self.assertEqual(len(launches), 3, "each bounded worker is replaced by a new one")
        # One lock, one claim, one store across every replacement.
        self.assertEqual(service.store_id, before)
        self.assertEqual(service.record()["restarts"], 3)
        self.assertFalse(service.lock_is_held(), "the supervisor released on the way out")

    def test_every_worker_inherits_the_descriptors_the_supervisor_holds(self):
        service = self.service("a")
        service.enable(actor="test")
        _outcome, launches, _slept = self.supervised(service, codes=[0, 0])
        self.assertEqual(len({call["lock_fd"] for call in launches}), 1)
        self.assertEqual(len({call["scope_fd"] for call in launches}), 1)
        self.assertEqual(len({call["token"] for call in launches}), 1)
        self.assertIsNotNone(launches[0]["lock_fd"])
        self.assertIsNotNone(launches[0]["scope_fd"])

    def test_repeated_failure_backs_off_and_is_reported(self):
        service = self.service("a")
        service.enable(actor="test")
        outcome, _launches, slept = self.supervised(service, codes=[1, 1, 1])
        self.assertEqual(outcome["consecutiveFailures"], 3)
        self.assertIsNotNone(outcome["degraded"], "repeated failure is reported, not hidden")
        self.assertIn("3 consecutive worker failures", outcome["degraded"])
        # Capped exponential, not a fixed interval and not a tight loop.
        self.assertEqual(slept[:2], [2.0, 4.0])
        log = (service.selection.path / "daemon.log").read_text(encoding="utf-8")
        self.assertIn("service_degraded", log)

    def test_a_clean_segment_resets_the_failure_count(self):
        service = self.service("a")
        service.enable(actor="test")
        outcome, _launches, slept = self.supervised(service, codes=[1, 1, 0])
        self.assertEqual(outcome["consecutiveFailures"], 0)
        self.assertEqual(slept[:3], [2.0, 4.0, 2.0])

    def test_disabling_the_service_ends_supervision_at_the_boundary(self):
        service = self.service("a")
        service.enable(actor="test")
        launches = []

        def spawn(**call):
            launches.append(call)
            service.intent.write(enabled=False, actor="owner")
            return FakeWorker(0)

        outcome = service.supervise(
            allow_isolated=True, spawn=spawn, sleeper=lambda _s: None, max_segments=5,
        )
        self.assertEqual(len(launches), 1, "no replacement after the owner disabled it")
        self.assertEqual(outcome["segments"], [0])
        self.assertFalse(service.intent.read()["enabled"])

    def test_a_stop_request_ends_supervision_without_a_replacement(self):
        service = self.service("a")
        service.enable(actor="test")
        launches = []

        def spawn(**call):
            launches.append(call)
            service.request_stop()
            return FakeWorker(0)

        service.supervise(
            allow_isolated=True, spawn=spawn, sleeper=lambda _s: None, max_segments=5,
        )
        self.assertEqual(len(launches), 1)

    def test_supervising_a_disabled_service_is_refused(self):
        service = self.service("a")
        with self.assertRaises(ServiceRefused) as caught:
            service.supervise(allow_isolated=True, spawn=lambda **_k: FakeWorker(0))
        self.assertEqual(caught.exception.reason, "service_disabled")

    def test_a_launch_is_not_ready_until_initialisation_has_returned(self):
        """start matched a record written before on_start ran.

        Recovery happens in on_start, and it can take longer than the poll interval or fail
        outright on the App Server connection. Everything start matched on - the pid, the
        launch id, the daemon lock - is already true while that is still in flight.
        """
        service = self.service("a")
        service.enable(actor="test")
        service.launch_id = "launch-under-test"
        seen = {}

        def on_start():
            seen["record"] = service.record()

        outcome, _launches, _slept = self.supervised(service, codes=[0], on_start=on_start)

        self.assertTrue(outcome["ok"], outcome)
        self.assertEqual(seen["record"]["launchId"], "launch-under-test")
        self.assertIsNotNone(seen["record"]["pid"], "the record start polls was already there")
        self.assertIsNone(
            seen["record"]["readyAt"],
            "initialisation had not returned, so this is not a started service",
        )
        self.assertIsNotNone(service.record()["readyAt"], "and it is ready afterwards")

    def test_a_failed_initialisation_never_becomes_ready(self):
        service = self.service("a")
        service.enable(actor="test")

        def on_start():
            raise RuntimeError("the App Server connection failed")

        with self.assertRaises(RuntimeError):
            self.supervised(service, codes=[0], on_start=on_start)
        self.assertIsNone(service.record()["readyAt"])


class Projects(ServiceTestCase):
    """The operations contract says status groups by project; it has to actually do it."""

    def assignment(self, service, name, *, cwd, issue, status="active"):
        from codex_session_relay.store import Store

        store = Store(service.selection.db_path)
        try:
            store.db.execute(
                "INSERT INTO relationships (relationship_id, issue_key, status,"
                " parent_task_id, parent_host_id, parent_cwd, child_task_id, child_host_id,"
                " execution_generation, artifact_roots, allowed_recipients, created_at,"
                " updated_at) VALUES (?,?,?,?,?,?,?,?,1,'[]','[]','now','now')",
                (f"rel-{name}", issue, status, f"01parent-{name}", "host",
                 cwd, f"01child-{name}", "host"),
            )
        finally:
            store.close()

    def test_status_groups_a_shared_service_by_project(self):
        service = self.service("a")
        self.assignment(service, "one", cwd="/code/alpha", issue="ALPHA-1")
        self.assignment(service, "two", cwd="/code/alpha", issue="ALPHA-2", status="paused")
        self.assignment(service, "three", cwd="/code/beta", issue="BETA-1")

        projects = service.status()["projects"]

        self.assertTrue(projects["available"], projects)
        self.assertEqual([p["project"] for p in projects["projects"]],
                         ["/code/alpha", "/code/beta"])
        alpha = projects["projects"][0]
        self.assertEqual(alpha["assignments"], 2)
        self.assertEqual(alpha["active"], 1, "a paused assignment is carried but not active")
        self.assertEqual(alpha["issues"], ["ALPHA-1", "ALPHA-2"])
        self.assertEqual(len(alpha["parents"]), 2)

    def test_a_store_that_does_not_exist_is_reported_rather_than_created(self):
        """status is an offline command; it must not bring a store into being to answer."""
        service = self.service("a")
        service.selection.db_path.unlink()

        projects = service.status()["projects"]

        self.assertFalse(projects["available"])
        self.assertEqual(projects["projects"], [])
        self.assertFalse(service.selection.db_path.exists(), "asking must not create it")

    def test_an_inventory_that_cannot_be_queried_is_not_an_empty_one(self):
        """The file opened and the query did not. Reporting available says there are none."""
        service = self.service("a")
        service.selection.db_path.write_bytes(b"")

        projects = service.status()["projects"]

        self.assertFalse(projects["available"])
        self.assertIsNotNone(projects["detail"])
        self.assertEqual(projects["projects"], [])


class SupervisedWorker(ServiceTestCase):
    """A worker adopts what its supervisor holds, or it is refused outright."""

    def adopt(self, service, **overrides):
        from codex_session_relay.cli import _adopt_supervised

        class Args:
            pass

        args = Args()
        args.supervised_token = overrides.get("token", "tok")
        args.supervised_lock_fd = overrides.get("lock_fd", 0)
        args.supervised_scope_fd = overrides.get("scope_fd", -1)
        return _adopt_supervised(service, args)

    def prepared(self, **record):
        service = self.service("a")
        service.selection.path.mkdir(parents=True, exist_ok=True)
        (service.selection.path / "daemon.lock").write_text("", encoding="utf-8")
        base = service.new_record(pid=os.getppid(), token="tok")
        service.write_record(dict(base, **record))
        return service

    def test_an_unsupervised_run_is_untouched(self):
        from codex_session_relay.cli import _adopt_supervised

        class Args:
            supervised_token = None
            supervised_lock_fd = None
            supervised_scope_fd = None

        self.assertEqual(_adopt_supervised(self.service("a"), Args()), {})

    def test_a_partial_supervised_invocation_is_refused(self):
        from codex_session_relay.cli import PayloadExit

        service = self.prepared()
        with self.assertRaises(PayloadExit) as caught:
            self.adopt(service, scope_fd=None)
        self.assertEqual(caught.exception.payload["reason"], "supervised_invocation_incomplete")

    def test_a_wrong_token_is_refused(self):
        from codex_session_relay.cli import PayloadExit

        service = self.prepared()
        with self.assertRaises(PayloadExit) as caught:
            self.adopt(service, token="not-the-token")
        self.assertEqual(caught.exception.payload["reason"], "supervised_token_mismatch")

    def test_a_descriptor_for_another_file_is_refused(self):
        from codex_session_relay.cli import PayloadExit

        service = self.prepared()
        stranger = os.open(os.path.join(self.tmp, "not-a-lock"), os.O_CREAT | os.O_RDWR, 0o600)
        self.addCleanup(os.close, stranger)
        with self.assertRaises(PayloadExit) as caught:
            self.adopt(service, lock_fd=stranger)
        self.assertEqual(caught.exception.payload["reason"], "supervised_fd_mismatch")

    def test_a_worker_whose_supervisor_is_already_gone_refuses_to_serve(self):
        from codex_session_relay.cli import PayloadExit

        # The record names a supervisor that is not this process's parent, which is exactly
        # what an orphan looks like when PDEATHSIG arrived too late to help.
        service = self.prepared(pid=999999)
        lock = os.open(service.selection.path / "daemon.lock", os.O_RDWR)
        self.addCleanup(os.close, lock)
        with self.assertRaises(PayloadExit) as caught:
            self.adopt(service, lock_fd=lock)
        self.assertEqual(caught.exception.payload["reason"], "supervisor_already_gone")
