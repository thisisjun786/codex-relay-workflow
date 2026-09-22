"""The daemon's polling cadence, and the CLI actually supplying it.

RelayDaemon.run waits only when it is given something to wait with. The CLI gave it nothing, so
'daemon --max-ticks 3' returned in 0.155 seconds against a 20 second poll interval, and a
deadline-only run busy-spun for its whole duration. These tests pin both halves.
"""

import os
import shutil
import tempfile
import time
import unittest
from contextlib import redirect_stderr
from io import StringIO
from pathlib import Path
from unittest import mock

from codex_session_relay.clock import FakeClock
from codex_session_relay.cli import (
    EXIT_USAGE, PayloadExit, Services, SystemExit2, _run_bounded, _scheduler_wait,
    _service_for, build_parser, cmd_daemon,
)
from codex_session_relay.daemon import RelayDaemon
from codex_session_relay.fakehost import FakeHostAdapter
from codex_session_relay.policy import RetryPolicy
from codex_session_relay.service import EXIT_BOUND_SPENT


class _Args:
    def __init__(self, state, socket=None, max_ticks=None, deadline=None,
                 deadline_monotonic=None):
        self.state = state
        self.socket = socket
        self.max_ticks = max_ticks
        self.deadline = deadline
        # The instant form, written by a supervisor rather than by a person. Named here so a
        # test can set it; the CLI reads both forms through one helper.
        self.deadline_monotonic = deadline_monotonic
        # A bounded daemon run now takes the same scope claim a managed service does, so a
        # test has to say which registry it is claiming in. Without this it would write an
        # ownership record under the real home.
        self.allow_isolated_scope = True


def build_services(case, **kwargs):
    """A Services container over a temporary state directory and its own scope registry."""
    directory = tempfile.mkdtemp(prefix="relay-cadence-")
    case.addCleanup(shutil.rmtree, directory, ignore_errors=True)
    scopes = tempfile.mkdtemp(prefix="relay-cadence-scopes-")
    case.addCleanup(shutil.rmtree, scopes, ignore_errors=True)
    previous = os.environ.get("CODEX_SESSION_RELAY_SCOPE_DIR")
    os.environ["CODEX_SESSION_RELAY_SCOPE_DIR"] = scopes
    case.addCleanup(
        lambda: os.environ.__setitem__("CODEX_SESSION_RELAY_SCOPE_DIR", previous)
        if previous is not None
        else os.environ.pop("CODEX_SESSION_RELAY_SCOPE_DIR", None)
    )
    args = _Args(directory, socket="/nonexistent-for-this-test", **kwargs)
    services = Services(args)
    # The lazy adapter returns whatever is already set, so no bridge is built and no socket
    # is touched; _require_adapter only checks that one was requested.
    services._adapter = FakeHostAdapter(services.clock)
    case.addCleanup(services.close)
    return services, args


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
        return build_services(self, **kwargs)

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


class _Tick:
    """One tick report, only as much of one as _run_bounded renders."""

    def as_dict(self):
        return {}


class TheFormOfTheBound(unittest.TestCase):
    """Which clock a worker reads its bound against, and what it does when it is already spent.

    test_service.BoundsAcrossAProcessBoundary drives a stand-in worker with two readings of the
    bound. These are the readings themselves, in the code that really has them: without this
    class that one would only be confirming its own stand-in.

    A DURATION starts counting at this process, so everything spent reaching it - fork,
    interpreter start, imports, argument parsing - lands on top of the bound. An INSTANT was
    decided before this process existed, so the same interval comes out of it.
    """

    def deadline_given_to(self, services, args, *, monotonic):
        """The deadline _run_bounded hands RelayDaemon.run, without running a tick."""
        captured = {}

        def run(_self, *, max_ticks=None, deadline=None, stop=None, sleep=None):
            captured["deadline"] = deadline
            return []

        with mock.patch.object(RelayDaemon, "run", run):
            _run_bounded(services, _service_for(services), args, require_intent=False,
                         monotonic=monotonic)
        return captured["deadline"]

    def test_an_instant_is_obeyed_as_given_rather_than_restarted_here(self):
        services, args = build_services(self)
        # The supervisor decided this thirty-second bound before the fork; twelve seconds of
        # it are already gone by the time this process can read a clock at all.
        args.deadline_monotonic = 5_000.0 + 30.0

        before = services.clock.now()
        deadline = self.deadline_given_to(services, args, monotonic=lambda: 5_000.0 + 12.0)
        after = services.clock.now()

        self.assertGreaterEqual(deadline, before + 18.0)
        self.assertLessEqual(
            deadline, after + 18.0,
            "the startup interval was added on top of the bound instead of taken out of it",
        )

    def test_a_duration_still_starts_counting_here(self):
        """The form a person and a unit file use, unchanged."""
        services, args = build_services(self, deadline=30.0)

        before = services.clock.now()
        deadline = self.deadline_given_to(services, args, monotonic=lambda: 5_000.0)
        after = services.clock.now()

        self.assertGreaterEqual(deadline, before + 30.0)
        self.assertLessEqual(deadline, after + 30.0)

    def test_no_bound_at_all_is_still_no_bound(self):
        services, args = build_services(self, max_ticks=2)

        self.assertIsNone(self.deadline_given_to(services, args, monotonic=lambda: 5_000.0))

    def test_a_bound_already_spent_takes_no_tick_and_says_which_ending_it_was(self):
        """The supervisor has to tell a worker that never started from one that ran and exited.

        A plain success would be counted as a clean segment and reset the failure streak; a
        plain failure would back off from a process that did not fail.
        """
        services, args = build_services(self)
        args.deadline_monotonic = 5_000.0

        with self.assertRaises(PayloadExit) as caught:
            self.deadline_given_to(services, args, monotonic=lambda: 5_050.0)

        self.assertEqual(caught.exception.code, EXIT_BOUND_SPENT)
        self.assertEqual(caught.exception.payload["reason"], "bound_already_spent")
        self.assertIn(
            "served nothing",
            (Path(args.state) / "daemon.log").read_text(encoding="utf-8"),
            "the worker exited on its bound without writing down why",
        )

    def test_two_bounds_at_once_is_a_usage_error(self):
        services, args = build_services(self, deadline=30.0)
        args.deadline_monotonic = 5_000.0

        with self.assertRaises(SystemExit2) as caught:
            self.deadline_given_to(services, args, monotonic=lambda: 4_000.0)

        self.assertEqual(caught.exception.code, EXIT_USAGE)
        self.assertIn("two different bounds", str(caught.exception))

    def test_a_bound_that_no_comparison_can_ever_pass_is_refused(self):
        """nan is truthy and every comparison against it is False, so a run given one would

        never reach its bound - the unbounded mode this daemon is built not to have. inf is the
        same thing spelled honestly, and a negative duration is an end already behind us.
        """
        for field, value in (("deadline", float("nan")), ("deadline", float("inf")),
                             ("deadline", -1.0), ("deadline_monotonic", float("nan")),
                             ("deadline_monotonic", float("inf")),
                             ("deadline_monotonic", -1.0)):
            with self.subTest(field=field, value=value):
                services, args = build_services(self, **{field: value})
                with self.assertRaises(SystemExit2) as caught:
                    self.deadline_given_to(services, args, monotonic=lambda: 5_000.0)
                self.assertEqual(caught.exception.code, EXIT_USAGE)

    def test_only_the_supervisor_is_offered_the_instant_form(self):
        """A person says how long. Only a process launched by another process is told when.

        service start and restart are where a duration is typed, and they convert it themselves
        for the supervisor they launch.
        """
        parser = build_parser()

        for command in (["daemon", "--deadline-monotonic", "1.0"],
                        ["service", "run", "--deadline-monotonic", "1.0"]):
            with self.subTest(command=command):
                self.assertEqual(parser.parse_args(command).deadline_monotonic, 1.0)

        for command in (["service", "start", "--deadline-monotonic", "1.0"],
                        ["service", "restart", "--deadline-monotonic", "1.0"]):
            with self.subTest(command=command):
                with redirect_stderr(StringIO()), self.assertRaises(SystemExit):
                    parser.parse_args(command)

    def test_a_bound_spent_while_the_run_was_starting_is_the_same_ending(self):
        """The window after this process reads its own clock and before its first tick.

        Adopting the inherited descriptors, taking the scope claim, building the adapter and
        publishing the worker policy all cost time. A run that had a sliver left when it looked,
        and none by the time it began, took no tick either - and returning success for that is
        what let a supervisor count a worker which served nothing as a clean segment.
        """
        services, args = build_services(self)
        args.deadline_monotonic = 5_000.1
        now = [5_000.0]

        def run(_self, *, max_ticks=None, deadline=None, stop=None, sleep=None):
            # What this process still had to pay after it read its own clock.
            now[0] += 5.0
            return []

        with mock.patch.object(RelayDaemon, "run", run):
            with self.assertRaises(PayloadExit) as caught:
                _run_bounded(services, _service_for(services), args, require_intent=False,
                             monotonic=lambda: now[0])

        self.assertEqual(caught.exception.code, EXIT_BOUND_SPENT)
        self.assertIn("taking its locks", caught.exception.payload["detail"])

    def test_a_zero_duration_is_a_spent_bound_and_not_the_absence_of_one(self):
        """Read for truth rather than for truthiness.

        Zero seconds is a bound; it is just one with nothing in it. Treated as falsy it became
        "no bound was given", and a run that also had --max-ticks went on ticking.
        """
        services, args = build_services(self, deadline=0.0, max_ticks=5)

        with self.assertRaises(PayloadExit) as caught:
            self.deadline_given_to(services, args, monotonic=lambda: 5_000.0)

        self.assertEqual(caught.exception.code, EXIT_BOUND_SPENT)

    def test_a_tick_budget_of_zero_is_not_a_bound_that_ran_out(self):
        """Two different reasons for taking no tick, and only one of them is a spent bound.

        RelayDaemon.run breaks on the tick count BEFORE it looks at the deadline, so a run asked
        for no ticks takes none whatever the clock says. Reading the empty result alone would
        report a bound that ran out, and the supervisor would stop replacing workers over a
        budget somebody set deliberately.
        """
        services, args = build_services(self, deadline=0.0, max_ticks=0)

        result = self.deadline_given_to(services, args, monotonic=lambda: 5_000.0)

        # The bound really has passed - the assertion is that this is not what ended the run.
        self.assertLessEqual(result, services.clock.now())

    def test_a_wall_clock_that_steps_back_cannot_extend_the_bound(self):
        """The deadline the daemon compares against is a wall clock, and wall clocks move.

        Converting the instant gives the run an end time on services.clock, exactly as a
        duration has always produced one. A backward step after that conversion pushes that end
        away and hands the run time nobody granted it. So the run is also given run's own
        additional early exit, reading the monotonic bound this process was given, and a tick
        cannot start past it however the wall clock behaves.
        """
        services, args = build_services(self)
        args.deadline_monotonic = 5_030.0
        now = [5_000.0]
        captured = {}

        def run(_self, *, max_ticks=None, deadline=None, stop=None, sleep=None):
            captured["stop"] = stop
            return [_Tick()]

        with mock.patch.object(RelayDaemon, "run", run):
            _run_bounded(services, _service_for(services), args, require_intent=False,
                         monotonic=lambda: now[0])

        self.assertIsNotNone(captured["stop"], "the run was given no guard on its own clock")
        self.assertFalse(captured["stop"](), "the guard fired while the bound still had time")
        now[0] = 5_030.0
        self.assertTrue(captured["stop"](), "the guard did not fire on its own instant")

    def test_a_duration_run_is_guarded_on_its_own_clock_as_well(self):
        """The same exposure predates the instant form: --deadline N has always become a wall

        deadline too. The guard is derived from whichever form arrived, so a run told how long
        is bounded on the same clock as one told when.
        """
        services, args = build_services(self, deadline=30.0)
        now = [5_000.0]
        captured = {}

        def run(_self, *, max_ticks=None, deadline=None, stop=None, sleep=None):
            captured["stop"] = stop
            return [_Tick()]

        with mock.patch.object(RelayDaemon, "run", run):
            _run_bounded(services, _service_for(services), args, require_intent=False,
                         monotonic=lambda: now[0])

        self.assertFalse(captured["stop"]())
        now[0] = 5_030.0
        self.assertTrue(captured["stop"]())

    def test_an_unbounded_run_is_given_no_guard_to_trip_over(self):
        services, args = build_services(self, max_ticks=2)
        captured = {}

        def run(_self, *, max_ticks=None, deadline=None, stop=None, sleep=None):
            captured["stop"] = stop
            return [_Tick()]

        with mock.patch.object(RelayDaemon, "run", run):
            _run_bounded(services, _service_for(services), args, require_intent=False,
                         monotonic=lambda: 5_000.0)

        self.assertIsNone(captured["stop"])
