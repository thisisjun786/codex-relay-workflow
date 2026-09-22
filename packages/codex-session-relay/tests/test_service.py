"""Who owns the daemon, and who is allowed to stop it.

These tests use real child processes on purpose. A mocked flock proves that the code called
flock; only a second process trying to start proves that the first one is actually excluded.
"""

import contextlib
import errno
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
from codex_session_relay.store import Store, probe as store_probe, resolve_state_dir

REPO = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
SOCKET = "/nonexistent-app-server.sock"

# A child that does nothing but stay alive, for tests that only need a live pid.
IDLE_CHILD = "import time\nwhile True:\n    time.sleep(0.05)\n"

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


class _Captured(Exception):
    """Stops a start before it launches anything."""


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

    def test_enable_is_refused_while_a_foreign_supervisor_is_shutting_down(self):
        """A supervisor clears its pids BEFORE releasing the lock.

        ownership answers none once both are absent, which is exactly the interval in which
        a foreign shutdown could have its owner's disable reversed.
        """
        service = self.service("a")
        service.enable(actor="owner")
        child, _pid = self.holder(service)
        service.intent.write(enabled=False, actor="owner")
        service.write_record(dict(
            service.record(), pid=None, workerPid=None, installationId="someone-else",
        ))
        self.assertTrue(service.lock_is_held(), "the fixture needs the lock still held")

        refused = service.enable(actor="intruder")

        self.assertFalse(refused["ok"], refused)
        self.assertEqual(refused["reason"], "not_ours")
        self.assertFalse(service.intent.read()["enabled"])
        self.assertIsNone(child.poll())

    def test_a_host_with_no_boot_id_can_still_stop_its_own_service(self):
        """Refusing every record without one traded a narrow risk for a certain failure.

        Where no boot id is available at all, every record lacks one - including the record
        this installation just wrote - so a blanket refusal makes the service unstoppable by
        its own owner.
        """
        service = self.service("a")
        child, _pid = self.holder(service)
        service.write_record(dict(service.record(), bootId=None))

        with mock.patch.object(service_module, "boot_id", lambda: None):
            stopped = service.stop()

        self.assertTrue(stopped["ok"], stopped)
        child.wait(timeout=10)

    def test_an_orphan_worker_with_no_boot_id_is_unverifiable_too(self):
        """_stop_worker checks start ticks only, and a reboot resets those with the pids.

        The live-supervisor path already refused this; the orphan branch did not, so an
        unrelated process holding the old worker number would have been signalled.
        """
        service = self.service("a")
        child, worker_pid = self.holder(service)
        service.write_record(dict(
            service.record(), pid=None, bootId=None, workerPid=worker_pid,
            workerStartTicks=service_module.start_ticks(worker_pid),
        ))

        refused = service.stop()

        self.assertFalse(refused["ok"], refused)
        self.assertEqual(refused["reason"], "ownership_unverifiable")
        self.assertIsNone(child.poll(), "an unidentifiable orphan must not be signalled")

    def test_disable_is_refused_while_a_foreign_supervisor_is_shutting_down(self):
        """The same cleanup window enable guards, in the command that writes the same file."""
        service = self.service("a")
        service.enable(actor="owner")
        child, _pid = self.holder(service)
        service.write_record(dict(
            service.record(), pid=None, workerPid=None, installationId="someone-else",
        ))
        self.assertTrue(service.lock_is_held(), "the fixture needs the lock still held")

        refused = service.disable(actor="intruder")

        self.assertFalse(refused["ok"], refused)
        self.assertEqual(refused["reason"], "not_ours")
        self.assertTrue(
            service.intent.read()["enabled"],
            "a foreign owner's service must not be left disabled by a refused command",
        )
        self.assertIsNone(child.poll())

    def test_the_takeover_flag_reaches_the_process_that_claims_the_scope(self):
        """It was set on the parent object and dropped at the process boundary.

        The supervisor is the process that claims the scope, so a flag the parent keeps to
        itself leaves the takeover silently doing nothing.
        """
        service = self.service("a")
        service.enable(actor="test")

        self.assertNotIn("--takeover-scope", self._launch_argv(service))

        service.takeover = True

        self.assertIn("--takeover-scope", self._launch_argv(service))

    def _launch_argv(self, service):
        """The argv default_launcher would build, without starting anything."""
        import subprocess
        from unittest import mock

        captured = {}

        class Fake:
            pid = os.getpid()
            returncode = None

            def poll(self):
                return None

        def popen(argv, **_kwargs):
            captured["argv"] = argv
            return Fake()

        with mock.patch.object(subprocess, "Popen", popen):
            service.default_launcher(service, allow_isolated=True)
        return captured["argv"]

    def test_a_stopped_registration_can_be_taken_over_deliberately(self):
        """Otherwise a replaced database blocks its socket forever.

        Recovery was finding and deleting an internal registry file by hand, which is not an
        operation anyone should have to discover.
        """
        first = self.service("a")
        child, _pid = self.holder(first)
        child.terminate()
        child.wait(timeout=10)
        second = self.service("b")
        self.assertNotEqual(second.store_id, first.store_id)

        refused = second.scope.claim(second.socket_path, second.new_record(pid=os.getpid()))
        self.assertEqual(refused["reason"], "scope_registered_to_other_store")
        second.scope.release(second.socket_path)

        second.takeover = True
        taken = second.scope.claim(second.socket_path, second.new_record(pid=os.getpid()))

        self.assertTrue(taken["ok"], taken)
        self.assertEqual(second.scope.read(second.socket_path)["storeId"], second.store_id)
        second.scope.release(second.socket_path)

    def test_a_takeover_cannot_displace_a_live_owner(self):
        """It only ever replaces a registration nothing is running behind."""
        first = self.service("a")
        self.holder(first)
        second = self.service("b")
        second.intent.write(enabled=True, actor="test")

        refused = second.start(
            allow_isolated=True, launcher=self._never_launch_here, takeover=True,
        )

        self.assertFalse(refused["ok"], refused)
        self.assertEqual(refused["reason"], "scope_owned_by_other_store")

    def _never_launch_here(self, *_a, **_k):  # pragma: no cover - reaching it is the failure
        self.fail("a takeover must not launch past a live owner")

    def test_disable_decides_and_writes_under_the_lock(self):
        """A foreign supervisor starting between the check and the write read enabled=true.

        It took the lock, then saw the intent we changed afterwards and exited at its next
        boundary - a refused disable that still stopped it. The classification and the write
        happen under the lock now, so no supervisor can start between them.
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
            "a refused disable must not change the intent a foreign supervisor reads",
        )
        self.assertIsNone(child.poll())

    def test_disable_takes_the_lock_before_touching_shared_intent(self):
        """The ordering itself, so a later edit cannot quietly put the write back first."""
        service = self.service("b")
        service.enable(actor="owner")
        order = []
        original_lock = service.daemon_lock_if_free
        original_write = service.intent.write

        @contextlib.contextmanager
        def watched_lock():
            order.append("lock")
            with original_lock() as held:
                yield held

        def watched_write(**kwargs):
            order.append("write")
            return original_write(**kwargs)

        service.daemon_lock_if_free = watched_lock
        service.intent.write = watched_write
        try:
            service.disable(actor="owner")
        finally:
            service.daemon_lock_if_free = original_lock
            service.intent.write = original_write

        self.assertEqual(order[:2], ["lock", "write"])
        self.assertFalse(service.intent.read()["enabled"])

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

    def test_enable_takes_the_lock_before_touching_shared_intent(self):
        """The ordering itself, so a later edit cannot quietly put the write back first.

        Classifying and then writing are two operations. A foreign supervisor that passes its
        own enabled-intent check and takes the lock in between would have enabled=true written
        over a disable that happened during the handoff, leaving it running and eligible to
        restart. disable already decides under the lock; enable did not.
        """
        service = self.service("f")
        order = []
        original_lock = service.daemon_lock_if_free
        original_write = service.intent.write

        @contextlib.contextmanager
        def watched_lock():
            order.append("lock")
            with original_lock() as held:
                yield held

        def watched_write(**kwargs):
            order.append("write")
            return original_write(**kwargs)

        service.daemon_lock_if_free = watched_lock
        service.intent.write = watched_write
        try:
            service.enable(actor="owner")
        finally:
            service.daemon_lock_if_free = original_lock
            service.intent.write = original_write

        self.assertEqual(order[:2], ["lock", "write"])
        self.assertTrue(service.intent.read()["enabled"])

    def test_enable_refuses_a_lock_holder_nothing_here_can_identify(self):
        """The same hole disable had: a supervisor that has the lock and has published
        nothing is not a supervisor whose owner asked for this."""
        import fcntl

        service = self.service("g")
        service.disable(actor="owner")
        service.selection.path.mkdir(mode=0o700, parents=True, exist_ok=True)
        handle = open(service.selection.path / service_module.DAEMON_LOCK, "a+")
        self.addCleanup(handle.close)
        fcntl.flock(handle, fcntl.LOCK_EX | fcntl.LOCK_NB)
        try:
            refused = service.enable(actor="owner")
        finally:
            fcntl.flock(handle, fcntl.LOCK_UN)

        self.assertFalse(refused["ok"], refused)
        self.assertEqual(refused["reason"], "ownership_unverifiable")
        self.assertFalse(
            service.intent.read()["enabled"],
            "a refused enable must not reverse the owner's disable",
        )

    def test_an_interrupted_start_does_not_leave_its_child_running(self):
        """The default launcher puts the supervisor in its own session, so a terminal signal
        never reaches it. Anything that leaves start() without a confirmed result - an
        exception, a Ctrl-C while recovery is still initialising - has to stop it, or it comes
        up afterwards holding both locks for a launch the caller was told nothing about."""
        service = self.service("h")
        service.enable(actor="owner")

        class Interrupted:
            returncode = None

            def __init__(self):
                self.stopped = []
                self.polls = 0

            def poll(self):
                self.polls += 1
                if self.polls == 1:
                    raise KeyboardInterrupt
                return self.returncode

            def terminate(self):
                self.stopped.append("terminate")
                self.returncode = -15

            def kill(self):  # pragma: no cover - terminate already settled it
                self.stopped.append("kill")

            def wait(self, timeout=None):
                return self.returncode

        child = Interrupted()
        with self.assertRaises(KeyboardInterrupt):
            service.start(allow_isolated=True, launcher=lambda *a, **k: child, timeout=5.0)

        self.assertIn(
            "terminate", child.stopped,
            "an unconfirmed child was left running after the start was interrupted",
        )

    def test_a_scope_claim_that_cannot_publish_its_record_releases_the_lock(self):
        """A lock with no record is a claim on behalf of a registration that does not exist,
        and it goes on refusing every later start in this process until the object dies."""
        registry = ScopeRegistry(Path(self.scopes), ISOLATED)
        registry.prepare()
        with mock.patch.object(service_module.Path, "write_text",
                               side_effect=OSError("no space left on device")):
            with self.assertRaises(OSError):
                registry.claim(SOCKET, {"storeId": "a-store"})

        self.assertIsNone(registry._handle, "the lock outlived the claim that failed")
        self.assertTrue(
            registry.claim(SOCKET, {"storeId": "a-store"})["ok"],
            "a later claim in the same process was refused by the abandoned lock",
        )

    def test_stop_finalises_the_record_under_the_lock(self):
        """The re-read that detects a replacement is a snapshot.

        The daemon lock is free the moment the old supervisor and worker are gone, so a
        replacement can acquire it after that read and publish its own record - and the
        clearing write would erase the identity of a launch that is running, leaving every
        later status and stop with no handle on it.
        """
        service = self.service("i")
        service.enable(actor="owner")
        child, _pid = self.holder(service)
        order = []
        original_lock = service.daemon_lock_if_free
        original_write = service.write_record

        @contextlib.contextmanager
        def watched_lock():
            order.append("lock")
            with original_lock() as held:
                yield held

        def watched_write(payload):
            order.append("write")
            return original_write(payload)

        service.daemon_lock_if_free = watched_lock
        service.write_record = watched_write
        try:
            stopped = service.stop(actor="owner")
        finally:
            service.daemon_lock_if_free = original_lock
            service.write_record = original_write

        child.wait(timeout=10)
        self.assertTrue(stopped["ok"], stopped)
        self.assertEqual(
            order[-2:], ["lock", "write"],
            "the stopped record was published without holding the lock that keeps a"
            " replacement out",
        )

    def test_a_gone_supervisor_with_no_boot_id_leaves_its_worker_unverifiable(self):
        """stop() falls through to _stop_worker when ownership answers none, and that
        validates start ticks only. A reboot resets those along with the pid space, so an
        unrelated process holding the old worker number would be signalled. The cleared-pid
        path already refused this; the already-gone path is just as stale and did not.
        """
        if service_module.boot_id() is None:
            self.skipTest("this host records no boot id, so the rule cannot apply")
        service = self.service("j")
        finished = subprocess.Popen([sys.executable, "-c", "pass"])
        finished.wait(timeout=10)
        service.write_record(dict(
            service.new_record(pid=os.getpid()), pid=finished.pid, workerPid=os.getpid(),
            workerStartTicks=service_module.start_ticks(os.getpid()), bootId=None,
        ))

        owner, handle, detail = service.ownership()
        if handle is not None:
            handle.close()

        self.assertEqual(owner, service_module.UNVERIFIABLE)
        self.assertIn("boot id", detail)

    def test_stop_reads_a_lock_it_cannot_take_after_both_exits_as_a_replacement(self):
        """A replacement usually has not published yet, so latest still reads the OLD record
        and the identity comparison cannot see it. Both recorded processes are confirmed gone
        by this point, so nothing this stop was acting on can be holding the lock, and the
        failed acquisition is the only evidence available in that window.
        """
        service = self.service("k")
        service.enable(actor="owner")
        child, _pid = self.holder(service)

        @contextlib.contextmanager
        def never_free():
            yield None

        service.daemon_lock_if_free = never_free
        stopped = service.stop(actor="owner")

        child.wait(timeout=10)
        self.assertFalse(stopped["ok"], stopped)
        self.assertEqual(stopped["reason"], "replaced_by_new_launch")

    def test_stop_writes_nothing_when_it_cannot_hold_the_lock(self):
        """One rule, because three narrower ones each left a window.

        A replacement that has published, one that has not, and an absent record are all the
        same situation: this process does not hold the state directory, so it does not get to
        write the record. Holding the lock is the only thing that excludes a replacement for
        the duration of the write rather than at one instant.
        """
        service = self.service("m")
        service.enable(actor="owner")
        child, _pid = self.holder(service)
        before = service.record()

        @contextlib.contextmanager
        def never_free():
            yield None

        writes = []
        original_write = service.write_record
        service.daemon_lock_if_free = never_free
        service.write_record = lambda payload: (writes.append(payload),
                                                original_write(payload))[1]
        try:
            stopped = service.stop(actor="owner")
        finally:
            service.write_record = original_write

        child.wait(timeout=10)
        self.assertFalse(stopped["ok"], stopped)
        self.assertEqual(stopped["reason"], "replaced_by_new_launch")
        self.assertEqual(writes, [], "a stop that holds nothing wrote the shared record")
        self.assertEqual(service.record()["launchId"], before["launchId"])

    def test_a_lock_that_fails_operationally_is_not_reported_as_a_replacement(self):
        """flock reports contention with EACCES or EAGAIN. Every other OSError is an
        operational failure - an unreadable directory, no descriptors left - and mapping it
        to "someone holds it" made stop announce a replacement that does not exist, which a
        restart then refuses to work around."""
        service = self.service("n")
        service.selection.path.mkdir(mode=0o700, parents=True, exist_ok=True)

        def refuse_to_open(*_args, **_kwargs):
            raise OSError(errno.EMFILE, "too many open files")

        with mock.patch("builtins.open", side_effect=refuse_to_open):
            with self.assertRaises(OSError) as caught:
                with service.daemon_lock_if_free():
                    pass

        self.assertEqual(caught.exception.errno, errno.EMFILE)

    def test_disable_refuses_a_lock_holder_nothing_here_can_identify(self):
        """A supervisor that has taken the lock and not yet published its record.

        ownership() answers none, _foreign_markers has nothing to read from, and the old
        condition asked only about foreign and unverifiable - so this wrote the shared
        enabled=false anyway. The starting supervisor reads that at its first worker boundary
        and exits, which is a refused disable that still shut another installation down.
        """
        import fcntl

        service = self.service("c")
        service.enable(actor="owner")
        service.selection.path.mkdir(mode=0o700, parents=True, exist_ok=True)
        handle = open(service.selection.path / service_module.DAEMON_LOCK, "a+")
        self.addCleanup(handle.close)
        fcntl.flock(handle, fcntl.LOCK_EX | fcntl.LOCK_NB)
        try:
            refused = service.disable(actor="owner")
        finally:
            fcntl.flock(handle, fcntl.LOCK_UN)

        self.assertFalse(refused["ok"], refused)
        self.assertEqual(refused["reason"], "ownership_unverifiable")
        self.assertTrue(
            service.intent.read()["enabled"],
            "a refused disable must not change the intent the starting supervisor reads",
        )

    def test_a_stale_worker_number_does_not_make_a_held_lock_ours(self):
        """A recorded worker pid is not ownership until its start time still matches.

        Once that process is gone the number proves nothing, and something else is holding
        this directory - which is the case the guard exists for.
        """
        import fcntl

        service = self.service("d")
        service.enable(actor="owner")
        service.write_record(dict(
            service.new_record(pid=os.getpid()), pid=None, workerPid=os.getpid(),
            workerStartTicks=(service_module.start_ticks(os.getpid()) or 0) + 1,
        ))
        handle = open(service.selection.path / service_module.DAEMON_LOCK, "a+")
        self.addCleanup(handle.close)
        fcntl.flock(handle, fcntl.LOCK_EX | fcntl.LOCK_NB)
        try:
            refused = service.disable(actor="owner")
        finally:
            fcntl.flock(handle, fcntl.LOCK_UN)

        self.assertFalse(refused["ok"], refused)
        self.assertEqual(refused["reason"], "ownership_unverifiable")
        self.assertTrue(service.intent.read()["enabled"])

    def test_disable_still_reaches_an_orphaned_worker_of_our_own(self):
        """The guard must not refuse the case stop() deliberately still supports.

        A supervisor that died leaving our own worker alive answers none, and the worker holds
        the lock through the descriptor it inherited. Refusing here would leave an owner unable
        to disable their own orphan.
        """
        service = self.service("e")
        service.enable(actor="owner")
        child, worker_pid = self.holder(service)
        service.write_record(dict(
            service.record(), pid=None, workerPid=worker_pid,
            workerStartTicks=service_module.start_ticks(worker_pid),
        ))

        disabled = service.disable(actor="owner")

        child.wait(timeout=10)
        self.assertTrue(disabled["ok"], disabled)
        self.assertFalse(service.intent.read()["enabled"])
    def test_stop_refuses_a_recycled_pid(self):
        service = self.service("a")
        child, pid = self.holder(service)
        record = service.record()
        service.write_record(dict(record, startTicks=(record["startTicks"] or 0) + 1))
        refused = service.stop()
        self.assertEqual(refused["reason"], "not_ours")
        self.assertIn("reused", refused["detail"])
        self.assertIsNone(child.poll())

    def test_stop_leaves_no_request_when_a_supervisor_starts_in_the_window(self):
        """The probe and the write were two operations, with a startup in between.

        supervise() clears pending requests BEFORE it acquires the lock, so a request left
        by a stop that saw the lock free is consumed by the supervisor that took it - which
        then exits, although the stop reported not_running.
        """
        service = self.service("a")
        service.enable(actor="test")
        original = service.daemon_lock_if_free
        calls = {"n": 0}

        @contextlib.contextmanager
        def taken_by_a_starting_supervisor():
            calls["n"] += 1
            # The lock is free when the old code probed it and held by the time the decision
            # is actually made. Holding it across the decision is what closes the window.
            yield None

        service.daemon_lock_if_free = taken_by_a_starting_supervisor
        try:
            refused = service.stop()
        finally:
            service.daemon_lock_if_free = original

        self.assertEqual(calls["n"], 1, "the decision must be taken under the lock")
        self.assertFalse(refused["ok"], refused)
        self.assertEqual(refused["reason"], "ownership_unverifiable")
        self.assertFalse(
            service.stop_request_path.exists(),
            "a stop that could not establish ownership must leave nothing behind",
        )

    def test_stop_reports_not_running_without_a_request_when_the_lock_is_free(self):
        """Holding the lock IS the proof that nothing is running and nothing can start."""
        service = self.service("a")
        service.enable(actor="test")

        outcome = service.stop()

        self.assertFalse(outcome["ok"])
        self.assertEqual(outcome["reason"], "not_running")
        self.assertFalse(
            service.stop_request_path.exists(),
            "nothing was running, so nothing needs to be told to stop",
        )

    def test_stop_writes_no_request_for_a_lock_held_by_an_unidentified_process(self):
        """A supervisor between taking the lock and writing its record has no identity yet.

        The stop request is not harmless there: a supervisor clears pending requests only
        BEFORE taking the lock, so a request written after that is consumed by the very
        service the caller was told it had not stopped.
        """
        service = self.service("a")
        child, _pid = self.holder(service)
        service.record_path.unlink()
        self.assertTrue(service.lock_is_held())

        refused = service.stop()

        self.assertFalse(refused["ok"], refused)
        self.assertEqual(refused["reason"], "ownership_unverifiable")
        self.assertFalse(
            service.stop_request_path.exists(),
            "a refused stop must not leave a request the service will act on",
        )
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

    def test_a_supervisor_finishing_normally_is_not_a_replacement(self):
        """Cleanup clears the supervisor's own pid while keeping its launch id.

        A launch identity that included the pid turned that ordinary exit into a phantom
        replacement, so a stop that genuinely stopped the service reported failure.
        """
        service = self.service("a")
        live = dict(service.new_record(pid=os.getpid()), launchId="launch-7")
        cleaned = dict(live, pid=None)

        self.assertEqual(
            service._launch_identity(live), service._launch_identity(cleaned),
            "the same launch clearing its own pid is still the same launch",
        )
        self.assertNotEqual(
            service._launch_identity(live),
            service._launch_identity(dict(live, launchId="launch-8")),
        )
        # And anonymous runs still separate on when they started.
        anon = dict(live, launchId=None, startedAt="2026-01-01T00:00:00Z")
        self.assertNotEqual(
            service._launch_identity(anon),
            service._launch_identity(dict(anon, startedAt="2026-01-02T00:00:00Z")),
        )

    def test_two_anonymous_launches_are_still_distinguishable(self):
        """A direct 'service run' carries no launch id, so comparing that field alone made
        two different launches compare equal - and handed the replacement straight back."""
        service = self.service("a")
        child, _pid = self.holder(service)
        ours = dict(service.record(), launchId=None)
        service.write_record(ours)
        replacement = subprocess.Popen(
            [sys.executable, "-c", IDLE_CHILD],
            stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
        )
        self.children.append(replacement)
        original = service._terminate

        def publish_anonymous_replacement(handle, **kwargs):
            outcome = original(handle, **kwargs)
            service.write_record(dict(
                ours, launchId=None, pid=os.getpid(), startedAt="2099-01-01T00:00:00Z",
                workerPid=replacement.pid,
                workerStartTicks=service_module.start_ticks(replacement.pid),
            ))
            return outcome

        with mock.patch.object(service, "_terminate", publish_anonymous_replacement):
            outcome = service.stop()

        child.wait(timeout=10)
        self.assertIsNone(replacement.poll(), "the replacement's worker is not ours")
        self.assertFalse(
            outcome["ok"],
            "a stop that left a replacement running has not stopped the service",
        )
        self.assertEqual(outcome["reason"], "replaced_by_new_launch")

    def test_stop_does_not_reach_into_a_replacement_launch(self):
        """Re-reading after the supervisor exits can pick up a NEW launch's record.

        Acting on that stops the replacement's worker and clears the replacement's pid using
        the outcome of the supervisor we actually stopped - reporting success while the new
        service is still alive.
        """
        service = self.service("a")
        child, _pid = self.holder(service)
        ours = dict(service.record(), launchId="launch-ours")
        service.write_record(ours)
        replacement = subprocess.Popen(
            [sys.executable, "-c", IDLE_CHILD],
            stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
        )
        self.children.append(replacement)
        original = service._terminate

        def publish_a_new_launch(handle, **kwargs):
            outcome = original(handle, **kwargs)
            # A start that acquired the lock the moment ours let go.
            service.write_record(dict(
                ours, launchId="launch-theirs", pid=os.getpid(),
                workerPid=replacement.pid,
                workerStartTicks=service_module.start_ticks(replacement.pid),
            ))
            return outcome

        with mock.patch.object(service, "_terminate", publish_a_new_launch):
            service.stop()

        child.wait(timeout=10)
        self.assertIsNone(
            replacement.poll(),
            "the replacement launch's worker is not ours to signal",
        )
        self.assertEqual(
            service.record()["launchId"], "launch-theirs",
            "and its record must not be overwritten with our outcome",
        )
        self.assertEqual(service.record()["workerPid"], replacement.pid)

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

    def test_supervision_refuses_a_disable_that_landed_while_it_was_starting(self):
        """The first intent check runs while nothing is held.

        A disable that lands between it and the instance lock would otherwise start a
        supervisor its owner had already turned off - and one the disabling caller never
        examined, because what it classified was the PREVIOUS holder. Whoever holds the lock
        is the one whose intent decides, so the answer is re-read under it.
        """
        service = self.service("a")
        service.enable(actor="test")
        enabled = service.intent.read()
        reads = []

        def once_then_disabled():
            reads.append(1)
            return enabled if len(reads) == 1 else dict(enabled, enabled=False)

        service.intent.read = once_then_disabled
        launches = []

        def spawn(**call):  # pragma: no cover - reaching it is the failure
            launches.append(call)
            return FakeWorker(0)

        with self.assertRaises(ServiceRefused) as caught:
            service.supervise(
                allow_isolated=True, spawn=spawn, sleeper=lambda _s: None, max_segments=1,
            )

        self.assertEqual(caught.exception.reason, "service_disabled")
        self.assertEqual(launches, [], "a disabled service was supervised anyway")

    def test_a_stopped_supervisor_reports_no_pending_restart(self):
        """nextRestartAt named a restart this supervisor is no longer going to make, so a
        stopped service read as though one were still scheduled."""
        service = self.service("a")
        service.enable(actor="test")
        self.supervised(service, codes=[1, 1])
        self.assertIsNone(service.record()["nextRestartAt"])

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
        # A fourth segment so the reset is observable as a delay. The delay after the LAST
        # segment no longer exists - there is nothing left to wait for - so a three-code run
        # would show the backoff climbing and never show it come back down.
        outcome, _launches, slept = self.supervised(service, codes=[1, 1, 0, 1])
        self.assertEqual(outcome["consecutiveFailures"], 1)
        self.assertEqual(slept[:3], [2.0, 4.0, 2.0])

    def test_the_last_allowed_segment_does_not_wait_to_restart_nothing(self):
        """The segment bound was only checked at the top of the loop, so every finite run
        slept one restart delay it had no use for - up to the backoff cap after repeated
        failures - before returning."""
        service = self.service("a")
        service.enable(actor="test")
        _outcome, launches, slept = self.supervised(service, codes=[1, 1])
        self.assertEqual(len(launches), 2)
        self.assertEqual(slept, [2.0], "the run waited after the segment it was never going"
                                       " to replace")

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

    def test_a_record_naming_another_store_is_refused(self):
        """The supervisor recorded which store it registered the scope for.

        This worker opened whatever relay.sqlite3 the path resolves to now, and a database
        deleted or atomically replaced between worker segments is a different one. Without
        this the worker serves an empty or unrelated store while the supervisor and the scope
        registration still name the original - and every participant comparing identities is
        told they agree.
        """
        from codex_session_relay.cli import PayloadExit

        service = self.prepared(storeId="the-supervisors-store")
        service.store_id = "a-replacement-store"
        with self.assertRaises(PayloadExit) as caught:
            self.adopt(service)
        self.assertEqual(caught.exception.payload["reason"], "supervised_store_mismatch")

    def test_a_record_naming_the_same_store_is_not_stopped_by_the_store_check(self):
        """The refusal must not swallow the ordinary supervised run.

        This gets as far as the descriptor check, which is where a test with no genuinely
        inherited descriptor has to stop. What it establishes is that the store check was not
        what stopped it.
        """
        from codex_session_relay.cli import PayloadExit

        service = self.prepared(storeId="one-store")
        service.store_id = "one-store"
        with self.assertRaises(PayloadExit) as caught:
            self.adopt(service)
        self.assertEqual(caught.exception.payload["reason"], "supervised_fd_mismatch")

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


FOUR_HOURS = 4 * 60 * 60


class ScriptedClock:
    """Monotonic time that moves only when something says it spent time.

    Real elapsed hours are the one input a test cannot have. This makes the supervisor's own
    arithmetic observable instead: a worker that runs its whole granted segment advances the
    clock by that segment, a restart delay advances it by the delay, and nothing else moves
    it at all.
    """

    START = 1000.0

    def __init__(self):
        self.now = self.START
        self.spent = []

    def __call__(self):
        return self.now

    def advance(self, seconds):
        self.now += seconds
        self.spent.append(seconds)

    @property
    def elapsed(self):
        return self.now - self.START


class FourHourBoundary(ServiceTestCase):
    """An assignment that outlives the limit on any single process.

    WHAT THESE PROVE. The supervisor's own boundary behaviour, driven past four hours of
    scripted monotonic time: that it goes on replacing bounded workers, that the store
    identity and the generations under it are untouched across every replacement, that one
    lock, one scope claim and one token are inherited by all of them, and that a stop or a
    disable written part-way through is honoured at the next worker boundary rather than at
    the end. The socket these run against does not exist, which is the point of the last
    assertion in the first test: the crossing needs no host contact and no parent at all.

    WHAT THEY DO NOT PROVE, and no scripted clock can. Nothing here observes four hours of
    real elapsed time, so nothing here says anything about what accumulates during them:
    process memory, sqlite WAL growth, file-descriptor or socket drift, or an App Server that
    answers differently after hours of uptime. A FakeWorker that exits in microseconds is
    also not a worker that ran for half an hour - only the supervisor's view of it is
    reproduced, never the worker's own. Those are properties of a real long run and need one
    to observe. This is the boundary logic, and the distinction is written here so that a
    later reader cannot take one for the other.
    """

    def assignment(self, service):
        """A real registered assignment in the store this supervisor holds."""
        from codex_session_relay.clock import FakeClock
        from codex_session_relay.models import Endpoint
        from codex_session_relay.registry import Registry

        store = Store(Path(service.selection.path) / "relay.sqlite3")
        self.addCleanup(store.close)
        relationship = Registry(store, FakeClock()).register(
            parent=Endpoint("01parent-task", "host-a", cwd="/parent"),
            child=Endpoint("01child-task", "host-a", cwd=self.tmp),
            issue_key="REL-9",
            artifact_roots=[self.tmp],
            allowed_recipients=["01parent-task"],
            dispatch_request_id="dispatch-9",
            dispatch_turn_id="turn-dispatch-9",
        )
        return store, relationship["relationshipId"]

    def generations(self, store, relationship_id):
        return [dict(row) for row in store.all(
            "SELECT * FROM generations WHERE relationship_id = ?"
            " ORDER BY execution_generation",
            (relationship_id,),
        )]

    def crossing(self, service, *, segment_seconds, segments, deadline=None, on_segment=None):
        """Run the supervisor over workers that each spend their whole granted segment."""
        clock = ScriptedClock()
        launches = []

        def spawn(**call):
            launches.append(call)
            # The worker running is the only reason time passes here, so the clock moves by
            # what this worker was actually granted rather than by what was asked for - which
            # is what makes the deadline clamp observable at the end of a bounded run.
            clock.advance(call["segment_seconds"])
            if on_segment is not None:
                on_segment(len(launches))
            return FakeWorker(0)

        outcome = service.supervise(
            allow_isolated=True, spawn=spawn, sleeper=clock.advance,
            segment_seconds=segment_seconds, max_segments=segments, deadline=deadline,
            monotonic=clock,
        )
        return outcome, launches, clock

    def test_an_assignment_crosses_four_hours_on_the_same_store_and_generation(self):
        service = self.service("a")
        service.enable(actor="test")
        store, relationship_id = self.assignment(service)
        before_id = service.store_id
        before_generations = self.generations(store, relationship_id)
        self.assertTrue(before_generations, "the fixture registered no generation to carry")

        # Half-hour segments, so nine of them is the first count that clears four hours.
        outcome, launches, clock = self.crossing(
            service, segment_seconds=30 * 60, segments=9,
        )

        self.assertGreater(
            clock.elapsed, FOUR_HOURS,
            "the run stopped short of the boundary it exists to cross",
        )
        self.assertEqual(len(launches), 9)
        self.assertEqual(outcome["segments"], [0] * 9)
        self.assertEqual(service.store_id, before_id, "the store changed under the assignment")
        self.assertEqual(
            self.generations(store, relationship_id), before_generations,
            "the generation did not survive the crossing intact",
        )
        # One lock, one claim, one token, on both sides of the boundary.
        self.assertEqual(len({call["lock_fd"] for call in launches}), 1)
        self.assertEqual(len({call["scope_fd"] for call in launches}), 1)
        self.assertEqual(len({call["token"] for call in launches}), 1)
        # And none of it asked anything of a host or a parent: SOCKET does not exist.
        self.assertFalse(
            os.path.exists(SOCKET),
            "this assertion is only meaningful while the socket really is absent",
        )

    def test_a_stop_past_the_boundary_ends_it_at_the_next_worker(self):
        service = self.service("a")
        service.enable(actor="test")

        def stop_after_the_boundary(count):
            if count == 9:
                service.request_stop()

        _outcome, launches, clock = self.crossing(
            service, segment_seconds=30 * 60, segments=40,
            on_segment=stop_after_the_boundary,
        )

        self.assertGreater(clock.elapsed, FOUR_HOURS)
        self.assertEqual(
            len(launches), 9,
            "the supervisor spawned another worker after the owner asked it to stop",
        )

    def test_a_disable_past_the_boundary_is_obeyed_and_left_as_the_owner_wrote_it(self):
        service = self.service("a")
        service.enable(actor="test")

        def disable_after_the_boundary(count):
            if count == 9:
                service.intent.write(enabled=False, actor="owner")

        _outcome, launches, clock = self.crossing(
            service, segment_seconds=30 * 60, segments=40,
            on_segment=disable_after_the_boundary,
        )

        self.assertGreater(clock.elapsed, FOUR_HOURS)
        self.assertEqual(len(launches), 9, "a disabled service was given another worker")
        intent = service.intent.read()
        self.assertFalse(intent["enabled"], "the supervisor rewrote the owner's intent")
        self.assertEqual(intent["changedBy"], "owner")

    def test_one_real_worker_reads_the_assignment_back_through_inherited_state(self):
        """The boundary a scripted clock cannot cross: another process.

        Every other test in this class drives FakeWorker, which never calls spawn_worker,
        never adopts the inherited descriptors and never opens the store. Those show the
        SUPERVISOR preserving the assignment across replacements; they cannot show a worker
        reading it back, and would still pass if a real worker lost --state or opened a
        different database. This runs one real boundary: the real supervisor, spawning the
        real worker, which has to name the assignment it found.

        Real time rather than the scripted clock, deliberately - one short segment, because
        what is under test here is the process boundary and not the four-hour arithmetic.
        """
        import json

        # A socket path this test owns, inside its own temporary directory, rather than the
        # module-level absolute one. A test that reaches a path outside its own fixture is
        # what already went wrong in this package once: test_store.py's StateDirectory opened
        # the real user state directory until its HOME was pinned. Here it fails closed, so
        # the constant was harmless - but the absence is this test's evidence, and evidence
        # should not rest on a path the test does not own.
        socket = os.path.join(self.tmp, "absent-app-server.sock")
        service = self.service("a", socket=socket)
        service.enable(actor="test")
        store, relationship_id = self.assignment(service)
        before_id = service.store_id
        before_generations = self.generations(store, relationship_id)
        store.close()

        # RelayService.spawn_worker itself, wrapped only to note the pid it returns.
        spawned = []

        def spawn(**call):
            child = service.spawn_worker(**call)
            spawned.append(child.pid)
            return child

        # Checked here rather than at import time: the assignment below is only named
        # because the worker's read fails, so the absence has to hold at this moment.
        self.assertFalse(
            os.path.exists(socket), f"the worker would have reached a real endpoint: {socket}",
        )
        outcome = service.supervise(
            allow_isolated=True, segment_seconds=1.0, max_segments=1, spawn=spawn,
        )

        log = (service.selection.path / "daemon.log").read_text(encoding="utf-8")
        self.assertEqual(
            outcome["segments"], [0],
            f"the worker did not exit cleanly; its log said:\n{log}",
        )
        # The worker's own tick report, written by the child process into the state
        # directory it was given.
        report = json.loads(log)
        self.assertTrue(report["ok"], report)
        self.assertEqual(len(spawned), 1, spawned)
        self.assertNotEqual(spawned[0], os.getpid(), "that was not a separate process")
        # The report's own pid is not the worker's. `_run_bounded` returns `record["pid"]`
        # from the record `owned_service` adopted, and in a supervised run that record was
        # written by the supervisor - so this names the launch the worker belongs to.
        self.assertEqual(report["pid"], os.getpid(), report)

        # The assignment, named by the worker. DISPATCH_TURN appears nowhere in the argv or
        # the environment spawn_worker builds - only inside the store - so a worker that had
        # not read the inherited database could not have produced this line. The read fails
        # because this test's socket does not exist, which is what makes it name the turn it
        # was reaching for.
        notes = " ".join(report["ticks"][0]["notes"])
        self.assertIn(
            "turn-dispatch-9", notes,
            f"the worker never reached this assignment: {report['ticks'][0]}",
        )

        # And it left the store and the generation as it found them.
        self.assertEqual(service.store_id, before_id)
        reopened = Store(Path(service.selection.path) / "relay.sqlite3")
        self.addCleanup(reopened.close)
        self.assertEqual(self.generations(reopened, relationship_id), before_generations)

    def test_a_bound_past_four_hours_clamps_the_worker_that_would_outlive_it(self):
        """The last segment is shortened rather than allowed to run past the bound.

        A worker started just before a deadline outlives it by a whole segment otherwise, and
        at these durations that is half an hour of a service its owner asked to end.
        """
        service = self.service("a")
        service.enable(actor="test")
        bound = FOUR_HOURS + 15 * 60

        outcome, launches, clock = self.crossing(
            service, segment_seconds=30 * 60, segments=40, deadline=bound,
        )

        granted = [call["segment_seconds"] for call in launches]
        self.assertEqual(granted[:-1], [30 * 60] * (len(granted) - 1))
        # Shorter than a full segment, but not by the round fifteen minutes the arithmetic
        # suggests: the restart delay between each pair of workers is spent from the same
        # bound, so the remainder is fifteen minutes minus whatever the delays already took.
        self.assertLess(granted[-1], 30 * 60)
        self.assertAlmostEqual(granted[-1], 15 * 60 - sum(clock.spent[1::2]), places=6)
        # The bound is what it is measured against, and it lands on it exactly.
        self.assertAlmostEqual(clock.elapsed, bound, places=6)
        self.assertEqual(outcome["segments"], [0] * len(granted))


class UnidentifiableStore(ServiceTestCase):
    """A store that is here and would not say which one it is.

    The diagnostic reads refuse anything they cannot bind to the file it came from, so the
    identity comes back absent in exactly the case a comparison matters most: something moving
    the store under the command - every such move the read can observe; store._hold_database
    states the in-call window it cannot. Absent is not agreement. The question that separates
    two stores of one installation simply did not get an answer, and a lifecycle command must
    not signal on that.
    """

    def unidentified(self, service):
        """The same state directory, seen by a process whose probe could not read the identity."""
        return RelayService(
            service.selection, socket_path=service.socket_path, scope=service.scope,
            store_id=None, store_unidentified=True,
        )

    def test_a_live_supervisor_we_cannot_compare_stores_with_is_not_signalled(self):
        service = self.service("a")
        child, _pid = self.holder(service)
        blind = self.unidentified(service)

        self.assertEqual(blind.ownership()[0], service_module.UNVERIFIABLE)
        refused = blind.stop()

        self.assertFalse(refused["ok"], refused)
        self.assertEqual(refused["reason"], "ownership_unverifiable", refused)
        self.assertIn("could not be read", refused["detail"])
        self.assertEqual(refused["supervisor"], "untouched")
        self.assertIsNone(child.poll(), "a service we could not attribute was signalled")

    def test_a_worker_left_by_a_dead_supervisor_is_not_signalled_either(self):
        """The orphan path stop() deliberately reaches, which is the other way to mutate."""
        service = self.service("a")
        child, worker_pid = self.holder(service)
        record = service.record()
        service.write_record(dict(
            record, workerPid=worker_pid,
            workerStartTicks=service_module.start_ticks(worker_pid),
        ))
        blind = self.unidentified(service)
        real = os.pidfd_open

        def supervisor_is_gone(pid, *args, **kwargs):
            if pid == record["pid"]:
                raise ProcessLookupError(f"supervisor {pid} is gone")
            return real(pid, *args, **kwargs)

        with mock.patch.object(os, "pidfd_open", supervisor_is_gone):
            refused = blind.stop()

        self.assertFalse(refused["ok"], refused)
        self.assertEqual(refused["reason"], "ownership_unverifiable", refused)
        self.assertEqual(refused["worker"], "untouched")
        self.assertIsNone(child.poll(), "a worker we could not attribute was signalled")

    def test_a_record_that_names_no_store_is_untouched(self):
        """Nothing to compare is not the same as a comparison that could not be made.

        A record written before any store existed names none, and there is no question to fail
        to answer. Refusing there would stop an owner from reaching their own service for a
        reason that has nothing to do with them.
        """
        service = self.service("a")
        self.holder(service)
        service.write_record(dict(service.record(), storeId=None))
        blind = self.unidentified(service)

        self.assertEqual(blind.ownership()[0], service_module.OURS)

    def test_holding_an_identity_settles_it_however_the_flag_was_set(self):
        """The flag cannot go stale, because holding an identity is what is asked.

        _await_launch adopts the child's store id after a first launch, so a process that
        probed before the store existed ends up holding an identity while the flag it was built
        with is still set. Asking whether we hold one - rather than clearing the flag at each
        place an identity can arrive - is what keeps that from reporting our own new service as
        unverifiable the moment it comes up.
        """
        service = self.service("a")
        self.holder(service)
        blind = self.unidentified(service)
        self.assertEqual(blind.ownership()[0], service_module.UNVERIFIABLE)

        blind.store_id = service.store_id
        self.assertEqual(blind.ownership()[0], service_module.OURS)

    def test_a_definite_mismatch_still_outranks_what_could_not_be_established(self):
        """Precedence: what is proven first, then what could not be answered."""
        service = self.service("a")
        self.holder(service)
        service.write_record(dict(service.record(), installationId="someone-else"))
        blind = self.unidentified(service)

        owner, _handle, detail = blind.ownership()
        self.assertEqual(owner, service_module.FOREIGN)
        self.assertIn("another installation", detail)

    def test_the_command_surface_carries_the_distinction_the_probe_measured(self):
        """cli._service_for is where the probe's answer becomes ownership's question.

        A file that is present and says nothing about itself is the shape that matters: it
        exists, so this is not "there is no relay here", and it has no identity to compare.
        """
        from codex_session_relay import cli

        blank = os.path.join(self.tmp, "blank")
        os.makedirs(blank)
        with open(os.path.join(blank, "relay.sqlite3"), "wb"):
            pass
        selection = resolve_state_dir(blank)
        measured = store_probe(selection)["store"]
        self.assertTrue(measured["exists"])
        self.assertIsNone(measured["storeId"])

        class Services:
            pass

        services = Services()
        services.selection = selection
        services.socket_path = SOCKET
        built = cli._service_for(services)

        self.assertIsNone(built.store_id)
        self.assertTrue(built.store_unidentified)
