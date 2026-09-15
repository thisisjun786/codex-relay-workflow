"""The daemon's polling cadence, and the CLI actually supplying it.

RelayDaemon.run waits only when it is given something to wait with. The CLI gave it nothing, so
'daemon --max-ticks 3' returned in 0.155 seconds against a 20 second poll interval, and a
deadline-only run busy-spun for its whole duration. These tests pin both halves.
"""

import tempfile
import time
import unittest

from codex_session_relay.clock import FakeClock
from codex_session_relay.cli import Services, _scheduler_wait, cmd_daemon
from codex_session_relay.fakehost import FakeHostAdapter
from codex_session_relay.policy import RetryPolicy


class _Args:
    def __init__(self, state, socket=None, max_ticks=None, deadline=None):
        self.state = state
        self.socket = socket
        self.max_ticks = max_ticks
        self.deadline = deadline


class SchedulerWait(unittest.TestCase):
    def test_it_waits_the_interval_when_there_is_no_deadline(self):
        slept = []
        wait = _scheduler_wait(FakeClock(), None, sleeper=slept.append)
        self.assertEqual(wait(20.0), 20.0)
        self.assertEqual(slept, [20.0])

    def test_it_never_sleeps_past_the_deadline(self):
        clock = FakeClock()
        slept = []
        wait = _scheduler_wait(clock, clock.now() + 5.0, sleeper=slept.append)
        self.assertEqual(wait(20.0), 5.0)
        self.assertEqual(slept, [5.0])

    def test_a_deadline_already_reached_sleeps_not_at_all(self):
        clock = FakeClock()
        slept = []
        wait = _scheduler_wait(clock, clock.now() - 1.0, sleeper=slept.append)
        self.assertEqual(wait(20.0), 0.0)
        self.assertEqual(slept, [])


class CliSuppliesTheCadence(unittest.TestCase):
    """The regression itself: the CLI must hand the daemon a real wait."""

    def _services(self, **kwargs):
        directory = tempfile.mkdtemp(prefix="relay-cadence-")
        args = _Args(directory, socket="/nonexistent-for-this-test", **kwargs)
        services = Services(args)
        # The lazy adapter returns whatever is already set, so no bridge is built and no socket
        # is touched; _require_adapter only checks that one was requested.
        services._adapter = FakeHostAdapter(services.clock)
        self.addCleanup(services.close)
        return services, args

    def test_a_bounded_run_waits_between_ticks(self):
        services, args = self._services(max_ticks=3)
        slept = []
        original = time.sleep
        time.sleep = slept.append
        try:
            result = cmd_daemon(services, args)
        finally:
            time.sleep = original
        self.assertEqual(len(result["ticks"]), 3)
        # Two waits for three ticks: run does not wait after the final one.
        self.assertEqual(slept, [RetryPolicy().poll_interval_seconds] * 2)

    def test_a_deadline_run_waits_and_is_clamped_to_what_remains(self):
        # Bounded by ticks as well, because a sleeper that records instead of sleeping never
        # lets real time reach the deadline: the point here is that the CLI supplies a wait on
        # the deadline path and that the wait is clamped, not how long the loop runs.
        services, args = self._services(deadline=5.0, max_ticks=2)
        slept = []
        original = time.sleep
        time.sleep = slept.append
        try:
            cmd_daemon(services, args)
        finally:
            time.sleep = original
        self.assertTrue(slept, "a deadline-only run must poll rather than busy-spin")
        self.assertEqual(len(slept), 1)
        self.assertLessEqual(slept[0], RetryPolicy().poll_interval_seconds)
        self.assertLessEqual(slept[0], 5.0, "a wait must never exceed the time remaining")


class RealWallClockCadence(unittest.TestCase):
    """A real wait, measured, with a small interval so the proof costs a second rather than 40."""

    def test_a_real_run_spends_its_deadline_polling(self):
        from codex_session_relay.daemon import RelayDaemon
        from codex_session_relay.clock import SystemClock
        from codex_session_relay.registry import Registry
        from codex_session_relay.receipts import ReceiptIntake
        from codex_session_relay.delivery import DeliveryService
        from codex_session_relay.ack import AckService
        from codex_session_relay.reconcile import Reconciler
        from codex_session_relay.store import Store
        import os

        directory = tempfile.mkdtemp(prefix="relay-cadence-real-")
        store = Store(os.path.join(directory, "relay.sqlite3"))
        self.addCleanup(store.close)
        clock = SystemClock()
        registry = Registry(store, clock)
        intake = ReceiptIntake(store, registry, clock)
        policy = RetryPolicy(poll_interval_seconds=0.3)
        delivery = DeliveryService(store, registry, intake, clock, policy=policy)
        ack = AckService(store, registry, intake, delivery, clock)
        daemon = RelayDaemon(
            store, registry, intake, delivery, ack,
            Reconciler(store, registry, delivery, clock), FakeHostAdapter(clock),
            clock=clock, policy=policy,
        )
        deadline = clock.now() + 1.0
        started = time.monotonic()
        reports = daemon.run(deadline=deadline, sleep=_scheduler_wait(clock, deadline))
        elapsed = time.monotonic() - started
        self.assertGreaterEqual(elapsed, 0.9, "the run returned before its deadline")
        self.assertLess(elapsed, 3.0, "the run overran its deadline")
        self.assertGreater(len(reports), 1, "a polling run ticks more than once")
        self.assertTrue(all(r.quiet for r in reports), "an empty store has nothing to report")
