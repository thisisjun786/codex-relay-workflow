"""Recovery from the two failures a live host actually produces: a process that stops
mid-transaction, and a store somebody else is holding.

Restarts at each point of the handoff are already covered, one point at a time, by
test_ack_reconcile.py RestartRecovery and test_delivery.py RestartPreservation. What is
new here is the interruption landing INSIDE a tick's own write transaction and a
different daemon finishing the work afterwards, which is the shape a killed worker
actually has.

The contention half exists because the completion hook runs guard-evaluate as a separate
process against a store the daemon is writing. Two cases, and they are not the same case:
an ordinary writer, which the store's write-ahead log lets the reader past, and a writer
holding the file exclusively, which is the only way a real lock timeout happens here.
That one waits real seconds - a timeout is the one thing an injected clock cannot
produce - and tests/test_regression_map.py is what keeps this module named as real-time
evidence in the map.
"""

import ast
import pathlib
import sqlite3
import time
import unittest
from unittest import mock

from codex_session_relay import guard, intent, marker
from codex_session_relay.ack import AckService
from codex_session_relay.daemon import RelayDaemon
from codex_session_relay.delivery import DeliveryService
from codex_session_relay.receipts import ReceiptIntake
from codex_session_relay.reconcile import Reconciler
from codex_session_relay.registry import Registry
from codex_session_relay.store import Store
from codex_session_relay.transport import DISPATCHED

from .support import CHILD, DISPATCH_TURN, DeliveryTestCase
from .test_guard import DISPATCH, LATER, NOW, GuardTestCase

REVISION_DISPATCH = "dispatch-2-revision"
LATER_DECLARATION = "2026-01-01T00:01:00+00:00"


class ATickInterruptedInsideItsOwnTransaction(DeliveryTestCase):
    """A worker killed mid-write, and the next one picking the work up."""

    def daemon(self):
        return RelayDaemon(
            self.store, self.registry, self.intake, self.delivery, self.ack,
            self.reconciler, self.adapter, clock=self.clock,
        )

    def reopen(self):
        """Close the store and open it again: the process boundary, minus the process.

        The adapter deliberately survives, because it is standing in for the host - the one
        thing a restart does NOT reset - and counting its sends across the boundary is how
        exactly-once is asserted at all.
        """
        path = self.store.path
        self.store.close()
        self.store = Store(path)
        self.addCleanup(self.store.close)
        self.registry = Registry(self.store, self.clock)
        self.intake = ReceiptIntake(self.store, self.registry, self.clock)
        self.delivery = DeliveryService(self.store, self.registry, self.intake, self.clock)
        self.ack = AckService(self.store, self.registry, self.intake, self.delivery, self.clock)
        self.reconciler = Reconciler(self.store, self.registry, self.delivery, self.clock)

    def test_an_aborted_tick_leaves_no_partial_state_and_a_new_daemon_delivers_once(self):
        _relationship, event_id = self.queued_event()
        self.clock.advance(3600)

        faults = {"count": 0}

        def die():
            faults["count"] += 1
            raise RuntimeError("the worker was killed inside its own write transaction")

        self.store.fault_hook = die
        try:
            self.daemon().tick(now=self.clock.now())
        except RuntimeError:
            pass
        finally:
            self.store.fault_hook = None

        self.assertGreaterEqual(
            faults["count"], 1,
            "no transaction was interrupted, so this test asserted nothing about recovery",
        )
        self.assertEqual(
            self.adapter.sends, [],
            "a send escaped a transaction that rolled back, so the state and the host disagree",
        )
        self.assertEqual(
            self.store.all("SELECT * FROM attempts"), [],
            "an attempt row survived the rollback that was supposed to take it",
        )

        self.reopen()
        self.daemon().tick(now=self.clock.now())

        self.assertEqual(
            len(self.adapter.sends), 1,
            f"the handoff was not delivered exactly once: {self.adapter.sends}",
        )
        self.assertEqual(self.delivery.get(event_id)["state"], DISPATCHED)
        self.assertEqual(
            len(self.store.all("SELECT * FROM attempts WHERE event_id = ?", (event_id,))), 1,
        )


class TheStoreUnderContention(GuardTestCase):
    """The hook reads while somebody else writes. Real elapsed time, deliberately.

    Two cases, and only the first is contention the daemon can produce. Production configures
    write-ahead logging and writes with BEGIN IMMEDIATE, so the ordinary-writer case below IS
    the hook-versus-daemon situation the completion hook created, and its measured answer is
    that the reader is not blocked at all. The exclusive case sets a locking mode nothing else
    in this repository sets - it is the only user of it - and exists because it is the only way
    to reach the bounded-timeout path. What that one establishes is what the guard does when a
    read genuinely cannot complete: bounded, unreadable rather than missing, released rather
    than held, and recorded. It does not establish that the relay can put its own store into
    that state, and the map says so.
    """

    def ready(self):
        relationship = self.managed()
        self.emit_ready(relationship)
        self.dispose("ready_for_review")
        return relationship

    def writer(self, *, exclusive=False):
        """A second connection to the real store file, holding a real write transaction."""
        path = str(self.store.path)
        if exclusive:
            # Exclusive locking mode takes the whole file, so it cannot be granted while this
            # process still holds the fixture's own connection open. Closing it first is what a
            # separate hook process has by construction: the daemon's connection is not its.
            self.store.close()
        connection = sqlite3.connect(path, timeout=30)
        self.addCleanup(connection.close)
        if exclusive:
            # The only configuration in which a reader here actually waits. An ordinary write
            # transaction does not block a write-ahead-log reader at all, which is the point of
            # the test above; this is what a checkpointing or exclusive-mode holder looks like.
            connection.execute("PRAGMA locking_mode=EXCLUSIVE")
        connection.execute("BEGIN IMMEDIATE")
        connection.execute(
            "INSERT INTO journal (kind, subject, detail, at) VALUES ('probe','x','y','t')"
        )
        return connection

    def timed_evaluate(self):
        started = time.monotonic()
        verdict = self.evaluate()
        return verdict, time.monotonic() - started

    def measured(self, bound):
        """One real evaluation with the owning constant rebound, and what reached SQLite.

        Returns (verdict, elapsed, timeouts, statements): the timeout arguments that actually
        arrived at sqlite3.connect, and the SQL that actually ran on the connections those calls
        returned. The trace is how an indirect read is counted rather than assumed - two of the
        four reads one evaluation makes are issued from currency.py on the connection it was
        handed, and no amount of reading guard.py would show them.
        """
        timeouts, statements = [], []
        opener = sqlite3.connect

        def watching(*args, **kwargs):
            timeouts.append(kwargs.get("timeout"))
            connection = opener(*args, **kwargs)
            connection.set_trace_callback(statements.append)
            return connection

        with mock.patch.object(intent, "SQLITE_TIMEOUT", bound):
            with mock.patch("sqlite3.connect", watching):
                started = time.monotonic()
                verdict = self.evaluate()
                elapsed = time.monotonic() - started
        return verdict, elapsed, timeouts, statements

    def test_an_ordinary_writer_does_not_make_the_readiness_check_unreadable(self):
        self.ready()
        connection = self.writer()

        verdict, elapsed = self.timed_evaluate()

        connection.rollback()
        self.assertEqual(
            verdict["observation"], "declared_ready_receipted",
            "a daemon writing made the hook unable to see a receipt that was right there",
        )
        self.assertEqual(verdict["decision"], guard.RELEASE)
        self.assertLess(
            elapsed, 1.0,
            f"the read waited {elapsed:.2f}s on an ordinary writer, so it is not reading past it",
        )

    def test_a_writer_holding_the_file_exclusively_times_out_bounded_and_answers_unreadable(self):
        self.ready()
        connection = self.writer(exclusive=True)

        verdict, elapsed = self.timed_evaluate()

        self.assertEqual(
            verdict["observation"], "state_unreadable",
            "a store nobody could read answered as something other than unreadable",
        )
        self.assertNotEqual(
            verdict["observation"], "receipt_missing",
            "a lock became a missing receipt, which is the conflation that holds a child that"
            " did its work",
        )
        self.assertEqual(
            verdict["decision"], guard.RELEASE,
            "a turn was held on evidence nobody could read",
        )
        self.assertIsNotNone(
            verdict["recordedAs"], "the wait produced no record, so nothing can be repaired",
        )
        self.assertGreater(
            elapsed, 1.0,
            f"the read returned in {elapsed:.2f}s, so no lock wait happened and this test is"
            " measuring nothing",
        )
        self.assertLess(
            elapsed, 5.0,
            f"the wait was {elapsed:.2f}s, at or past the five-second budget the hook contract"
            " gives this whole evaluation",
        )

        # Recovery: the same check on the same turn, once the holder lets go. A release is not a
        # hold, so nothing was reserved and this is an ordinary second look rather than a retry.
        connection.rollback()
        connection.close()

        again = self.evaluate()
        self.assertEqual(
            again["observation"], "declared_ready_receipted",
            "the check never recovered: it is still not reading the store it waited for",
        )
        self.assertEqual(again["decision"], guard.RELEASE)

    def test_changing_the_value_at_its_owning_site_changes_the_wait_that_is_spent(self):
        """CRW-96's measurement: set the bound twice at the one place that decides it, and time
        what happens against a real exclusive lock.

        The issue is explicit that reading the source is not evidence here, because the two
        values agreed all along and a source that looks wired can still be wired to nothing.
        What makes this a measurement is that nothing below names intent.py's connect call. The
        constant is rebound, the real evaluation runs against a real held lock, and the clock
        answers. A bound the constant did not reach would show as two equal waits.
        """
        self.ready()
        self.writer(exclusive=True)

        brief, patient = 0.5, 2.0

        quick_verdict, quick, quick_timeouts, _ = self.measured(brief)
        slow_verdict, slow, slow_timeouts, _ = self.measured(patient)

        self.assertEqual(
            set(quick_timeouts), {brief},
            f"the value set at the owning site is not what reached SQLite: {quick_timeouts}",
        )
        self.assertEqual(
            set(slow_timeouts), {patient},
            f"the value set at the owning site is not what reached SQLite: {slow_timeouts}",
        )
        for verdict in (quick_verdict, slow_verdict):
            self.assertEqual(
                verdict["observation"], "state_unreadable",
                "changing the wait changed the answer as well as the wait",
            )
            self.assertEqual(verdict["decision"], guard.RELEASE)

        self.assertGreater(
            quick, brief / 2,
            f"the {brief}s bound returned in {quick:.2f}s, so no lock wait happened and this"
            " measured nothing",
        )
        self.assertGreater(
            slow, patient / 2,
            f"the {patient}s bound returned in {slow:.2f}s, so it did not wait either",
        )
        self.assertGreater(
            slow - quick, (patient - brief) / 2,
            f"{brief}s waited {quick:.2f}s and {patient}s waited {slow:.2f}s. The wait does not"
            " track the value, so something other than this constant is deciding it",
        )

    def test_the_bound_that_applies_is_decided_by_read_only_connection(self):
        """Where the lock wait this hook can spend is actually decided, asked of the object.

        The measured two seconds above come from intent.read_only_connection, so that is the
        place a change to the bound has to pass through, and this asks the function itself
        rather than reading anybody's source text. The day someone gives it a timeout
        parameter the signature moves and this fails, which turns a silent drift into a visible
        choice. CRW-96 wired the bound without adding one - intent.SQLITE_TIMEOUT is read inside
        this function - so the absent parameter is now what stops a caller picking its own wait
        while still calling this, and the pin guards that rather than merely reporting it.

        Deliberately narrower than it once was. An earlier version also counted occurrences of
        the name in guard.py and claimed from that count that nothing anywhere reads the
        constant. One file is not the repository, so the claim was wider than the check, and a
        second mention in a comment would have broken it with nothing wired. Whether a second
        literal survives anywhere was CRW-96's question, and TheWaitBoundIsDeclaredInOnePlace
        below is where it is answered from source.
        """
        import inspect

        self.assertEqual(
            list(inspect.signature(intent.read_only_connection).parameters), ["db_path"],
            "read_only_connection now takes a timeout, so the bound can be passed in rather"
            " than fixed here; CRW-96 owns what that should be and this test should follow it",
        )


class TheWaitBoundReachesEveryReadItGoverns(GuardTestCase):
    """Not one connection: every read the evaluation makes, on the connection that bound opened.

    One acquisition and one reading path are different statements, and the criterion is about
    reading paths. A single evaluation issues four reads and only two of them are written in
    guard.py - the relationship row and the head event. head_revision and _requested_predecessors
    issue the other two from currency.py on the connection they were handed. Tracing what
    executed is what makes "every path" a measurement instead of a walk of the call graph.
    """

    def watched(self, bound, work):
        """Run work with the owning constant rebound; report the timeouts and the SQL seen."""
        timeouts, statements = [], []
        opener = sqlite3.connect

        def watching(*args, **kwargs):
            timeouts.append(kwargs.get("timeout"))
            connection = opener(*args, **kwargs)
            connection.set_trace_callback(statements.append)
            return connection

        with mock.patch.object(intent, "SQLITE_TIMEOUT", bound):
            with mock.patch("sqlite3.connect", watching):
                result = work()
        return result, timeouts, statements

    def test_every_read_one_evaluation_makes_runs_under_the_configured_bound(self):
        relationship = self.managed()
        self.emit_ready(relationship)
        self.dispose("ready_for_review")
        sentinel = 1.25

        verdict, timeouts, statements = self.watched(sentinel, self.evaluate)

        self.assertEqual(
            verdict["observation"], "declared_ready_receipted",
            "the evaluation never reached the store, so nothing here was measured",
        )
        self.assertEqual(
            timeouts, [sentinel],
            "the evaluation opened something other than exactly one connection carrying the"
            f" configured bound: {timeouts}",
        )
        reads = [line for line in statements if line.lstrip().upper().startswith("SELECT")]
        self.assertGreaterEqual(
            len(reads), 4,
            f"only {len(reads)} reads were traced, so this is not watching a whole evaluation:"
            f" {reads}",
        )
        self.assertTrue(
            any("verdicts" in line for line in reads),
            "no read naming verdicts was traced. guard.py never names that table, so its absence"
            " means currency's reads did not run on the connection this bound opened, which is"
            f" the reach being asserted: {reads}",
        )

    def test_the_other_caller_of_the_opener_receives_the_same_bound(self):
        """dispatch_generation_state is the second caller the issue names. Measured, not argued.

        Silent divergence is the defect, so the interesting question is not whether the source
        passes a value but which value arrives. It cannot differ here, because the opener takes
        no timeout parameter; this is what establishes that rather than asserting it.
        """
        relationship = self.managed()
        sentinel = 1.75

        def ask():
            return intent.dispatch_generation_state(
                str(self.store.path), relationship["relationshipId"], DISPATCH,
            )

        (state, readable), timeouts, _ = self.watched(sentinel, ask)

        self.assertTrue(readable, "the second caller could not read the store")
        self.assertEqual(state, intent.DISPATCH_CURRENT)
        self.assertEqual(
            timeouts, [sentinel],
            "the other caller of read_only_connection did not receive the bound the hook's"
            f" evaluation receives, so the two have diverged again: {timeouts}",
        )


class TheWaitBoundIsDeclaredInOnePlace(unittest.TestCase):
    """Where the bound is decided, derived from the source that is actually imported.

    The bound used to be declared in guard.py with the whole reasoning beside it and enforced by
    a separate literal in intent.read_only_connection, so changing the declaration changed
    nothing and the values agreeing hid it. These are derived rather than listed: the scan walks
    the package import closure reachable from guard and finds every connection this hook can
    open, so a second opener added later fails here instead of quietly carrying its own bound.

    Scanned through intent.__file__ rather than a path relative to this test, so what is read is
    the source that is actually imported rather than a copy that might not be.
    """

    PACKAGE = pathlib.Path(intent.__file__).resolve().parent

    def tree(self, module):
        return ast.parse((self.PACKAGE / module).read_text(encoding="utf-8"))

    def siblings_imported_by(self, module):
        found = set()
        for node in ast.walk(self.tree(module)):
            if not (isinstance(node, ast.ImportFrom) and node.level == 1):
                continue
            if node.module:
                found.add(node.module + ".py")
            found.update(alias.name + ".py" for alias in node.names)
        return {name for name in found if (self.PACKAGE / name).exists()}

    def closure(self, start="guard.py"):
        """Every module in this package guard can reach by import, including guard itself."""
        seen, pending = set(), [start]
        while pending:
            module = pending.pop()
            if module in seen:
                continue
            seen.add(module)
            pending.extend(self.siblings_imported_by(module))
        return seen

    @staticmethod
    def connect_sites(tree):
        """(function, line, timeout argument) for every connect call in a parsed module."""
        sites = {}
        for function in ast.walk(tree):
            if not isinstance(function, (ast.FunctionDef, ast.AsyncFunctionDef)):
                continue
            for node in ast.walk(function):
                if not (isinstance(node, ast.Call) and isinstance(node.func, ast.Attribute)):
                    continue
                if node.func.attr != "connect":
                    continue
                given = [keyword for keyword in node.keywords if keyword.arg == "timeout"]
                sites[node.lineno] = (
                    function.name, node.lineno, given[0].value if given else None,
                )
        return sorted(sites.values(), key=lambda site: site[1])

    def test_guard_can_open_a_database_in_exactly_one_place(self):
        openers = {
            (module, site[0])
            for module in self.closure()
            for site in self.connect_sites(self.tree(module))
        }
        self.assertEqual(
            openers, {("intent.py", "read_only_connection")},
            "the hook can now open a database somewhere else, and that place carries a lock wait"
            f" of its own: {sorted(openers)}",
        )

    def test_that_one_opener_takes_its_bound_from_the_constant_rather_than_a_literal(self):
        sites = self.connect_sites(self.tree("intent.py"))
        self.assertEqual(len(sites), 1, f"intent.py opens more than one connection: {sites}")
        _function, _line, timeout = sites[0]
        self.assertIsInstance(
            timeout, ast.Name,
            "the lock wait is written at the connect call again, so the constant above it is"
            " decorative and changing it changes nothing - which is the defect, not a style",
        )
        self.assertEqual(timeout.id, "SQLITE_TIMEOUT")

    def test_the_constant_is_declared_once_in_the_whole_package(self):
        """A second declaration is the defect whether or not the two values agree."""
        declarations = []
        for path in sorted(self.PACKAGE.glob("*.py")):
            for node in ast.walk(ast.parse(path.read_text(encoding="utf-8"))):
                if isinstance(node, ast.Assign):
                    targets = node.targets
                elif isinstance(node, ast.AnnAssign):
                    targets = [node.target]
                else:
                    continue
                for target in targets:
                    if isinstance(target, ast.Name) and target.id == "SQLITE_TIMEOUT":
                        declarations.append((path.name, node.lineno))
        self.assertEqual(
            [name for name, _line in declarations], ["intent.py"],
            f"the bound is declared in more than one place: {declarations}",
        )

    def test_the_scan_still_fails_against_the_source_this_change_replaced(self):
        """A scan that cannot fail proves nothing, so it is run against the shape it removed."""
        before = ast.parse(
            "def read_only_connection(db_path):\n"
            "    import sqlite3\n"
            "    connection = sqlite3.connect(uri, uri=True, timeout=2.0)\n"
            "    return connection\n"
        )
        sites = self.connect_sites(before)
        self.assertEqual(len(sites), 1, "the control sample lost the call this scan looks for")
        _function, _line, timeout = sites[0]
        self.assertNotIsInstance(
            timeout, ast.Name,
            "the control sample no longer has the shape this scan exists to catch, so the scan"
            " passing on the real source says nothing",
        )


class TheGenerationBoundReleasesIntoTheNextGeneration(GuardTestCase):
    """Repetition stops at the bound, and the next generation is not serving the old sentence."""

    def exhaust(self, relationship):
        for index in range(guard.MAX_HOLDS_PER_GENERATION):
            held = self.evaluate(turn_id="turn-gen1-" + str(index))
            self.assertEqual(held["decision"], guard.BLOCK, held)
        return self.evaluate(turn_id="turn-gen1-last")

    def next_generation(self, relationship):
        """What a needs_changes revision produces: a new dispatch id, so a new assignment."""
        rid = relationship["relationshipId"]
        self.registry.open_generation(
            rid, dispatch_request_id=REVISION_DISPATCH, reason="needs_changes_revision",
            dispatch_turn_id="turn-dispatch-2",
        )
        assignment = marker.assignment_id(REVISION_DISPATCH)
        intent.declare_intent(
            self.markers, workspace=self.workspace, dispatch_request_id=REVISION_DISPATCH,
            issue_key="REL-1", declared_at=LATER_DECLARATION, db_path=str(self.store.path),
        )
        intent.publish_claim(
            self.markers, workspace=self.workspace, assignment=assignment, session_id=CHILD,
            dispatch_request_id=REVISION_DISPATCH, first_turn_id="turn-dispatch-2", at=NOW,
        )
        intent.bind(
            self.markers, workspace=self.workspace, assignment=assignment, session_id=CHILD,
            task_id=CHILD, at=NOW,
        )
        intent.register_relationship(
            self.markers, workspace=self.workspace, assignment=assignment, relationship_id=rid,
            dispatch_request_id=REVISION_DISPATCH, at=NOW, db_path=str(self.store.path),
        )
        return assignment

    def test_the_generation_bound_stops_repeating_and_the_next_generation_may_hold_again(self):
        relationship = self.managed()
        exhausted = self.exhaust(relationship)
        self.assertEqual(exhausted["state"], "unresolved_handoff")
        self.assertEqual(exhausted["decision"], guard.RELEASE)

        assignment = self.next_generation(relationship)

        verdict = self.evaluate(turn_id="turn-gen2-0")
        self.assertEqual(
            verdict["assignmentId"], assignment,
            "the next generation's turn was judged against the generation it replaced",
        )
        self.assertEqual(
            verdict["decision"], guard.BLOCK,
            "the new generation inherited a budget it never spent, so a corrected child cannot"
            " be held even once",
        )
        self.assertEqual(verdict["counters"]["holdsThisGeneration"], 0)
        self.assertEqual(
            verdict["counters"]["holdsThisSessionWindow"], guard.MAX_HOLDS_PER_GENERATION,
            "the window forgot the holds the previous generation spent",
        )

    def test_the_rolling_window_still_counts_the_holds_the_previous_generation_spent(self):
        relationship = self.managed()
        self.exhaust(relationship)
        self.next_generation(relationship)
        self.evaluate(turn_id="turn-gen2-0")

        verdict = self.evaluate(turn_id="turn-gen2-1")

        self.assertEqual(
            verdict["state"], "unresolved_handoff",
            "the session window is per generation after all, so a child can be held forever by"
            " opening one more generation",
        )
        self.assertEqual(verdict["decision"], guard.RELEASE)
        self.assertEqual(
            verdict["counters"]["holdsThisSessionWindow"], guard.MAX_HOLDS_PER_SESSION_WINDOW,
        )


if __name__ == "__main__":
    unittest.main()
