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

import sqlite3
import time
import unittest

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

    def test_the_bound_that_applies_is_decided_by_read_only_connection(self):
        """Where the lock wait this hook can spend is actually decided, asked of the object.

        The measured two seconds above come from intent.read_only_connection, so that is the
        place a change to the bound has to pass through, and this asks the function itself
        rather than reading anybody's source text. The day someone gives it a timeout
        parameter - which is what wiring guard.SQLITE_TIMEOUT would require - the signature
        moves and this fails, which turns a silent drift into a visible choice.

        Deliberately narrower than it once was. An earlier version also counted occurrences of
        the name in guard.py and claimed from that count that nothing anywhere reads the
        constant. One file is not the repository, so the claim was wider than the check, and a
        second mention in a comment would have broken it with nothing wired. Whether a second
        literal survives anywhere is CRW-96's question and its acceptance criteria say so.
        """
        import inspect

        self.assertEqual(
            list(inspect.signature(intent.read_only_connection).parameters), ["db_path"],
            "read_only_connection now takes a timeout, so the bound can be passed in rather"
            " than fixed here; CRW-96 owns what that should be and this test should follow it",
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
