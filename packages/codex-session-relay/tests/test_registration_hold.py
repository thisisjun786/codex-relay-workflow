"""The registration hold, with the interleaving held still instead of raced.

test_registration_contention.py races a registration against an advance and asserts what must
hold under every ordering. That is the right shape for a barrier, and the wrong shape for the
question CRW-11 actually asks, which is about ONE ordering: the instant after a generation
check and before the marker fact lands. Here the advance opens its transaction and stops inside
it, so the registration runs at exactly that instant on every run rather than on the runs the
scheduler happens to arrange.

Nothing in this module reads a clock or starts a process. The threads block on events and on
the store's own lock, which is synchronisation rather than elapsed time, so this module stays
outside the map's real-time inventory.
"""

import threading
import unittest
from unittest import mock

from codex_session_relay import intent, marker
from codex_session_relay.clock import FakeClock
from codex_session_relay.errors import RefusalReason, RegistrationError
from codex_session_relay.registry import Registry
from codex_session_relay.store import Store

from .test_guard import DISPATCH, NOW, GuardTestCase

REVISION = "revision-held"


class AnAdvanceAndARegistrationCannotOverlap(GuardTestCase):
    """One lock, two parties, and the two orderings that lock produces."""

    def setUp(self):
        super().setUp()
        self.relationship = self.register()
        self.rid = self.relationship["relationshipId"]
        self.declare()
        self.claim()
        self.bind()
        self.directory = marker.assignment_dir(self.markers, self.workspace, self.assignment)

    def published(self):
        facts, _unreadable = marker.read_assignment(self.directory)
        return facts.get("relationship")

    def generation(self):
        row = self.store.one(
            "SELECT execution_generation AS g FROM relationships WHERE relationship_id = ?",
            (self.rid,),
        )
        return row["g"]

    def advance_stopped_inside_its_transaction(self, holding, release, errors):
        """A thread that opens a generation and then sits inside the uncommitted transaction.

        open_generation_in is the half of the advance that runs under the caller's transaction,
        so holding that transaction open is the advance holding the relay's write lock. The
        Store is built inside the thread because a sqlite3 connection belongs to the thread that
        created it.
        """
        def run():
            store = Store(self.store.path)
            try:
                with store.transaction() as db:
                    Registry(store, FakeClock()).open_generation_in(
                        db, self.rid, dispatch_request_id=REVISION,
                        reason="needs_changes_revision", dispatch_turn_id="turn-held",
                    )
                    holding.set()
                    release.wait(timeout=20)
            except Exception as error:  # noqa: BLE001 - asserted by the caller
                errors["advance"] = error
            finally:
                holding.set()
                store.close()

        return threading.Thread(target=run)

    def test_a_registration_cannot_publish_while_an_advance_holds_the_store(self):
        """The window, reproduced and closed.

        The advance has written both rows and has not committed. That is precisely the moment
        the old source published into: its check ran on a read-only connection, which WAL does
        not block, so it read the generation from before the advance and published over one the
        store was already replacing. The registration now has to take the same lock, so it
        cannot read that state at all.
        """
        holding, release, errors = threading.Event(), threading.Event(), {}
        thread = self.advance_stopped_inside_its_transaction(holding, release, errors)
        thread.start()
        self.addCleanup(thread.join, 60)
        self.assertTrue(holding.wait(timeout=20), "the advance never opened its transaction")

        try:
            with self.assertRaises(RegistrationError) as caught:
                self.register_marker({"relationshipId": self.rid})
        finally:
            release.set()
        thread.join(timeout=60)

        self.assertFalse(thread.is_alive(), "the advance never finished")
        self.assertNotIn("advance", errors, f"opening the next generation failed: {errors}")
        self.assertEqual(caught.exception.reason, RefusalReason.UNREGISTERED_RELATIONSHIP)
        self.assertIn(
            "write lock could not be taken", str(caught.exception),
            f"the refusal blamed something other than the lock: {caught.exception}",
        )
        self.assertIsNone(
            self.published(),
            "the registration published while an advance held the store, which is the window",
        )
        self.assertEqual(
            self.generation(), 2,
            "the advance did not commit, so the refusal was measured against a rollback rather",
        )
        state, readable = intent.dispatch_generation_state(str(self.store.path), self.rid, DISPATCH)
        self.assertTrue(readable)
        self.assertEqual(state, intent.DISPATCH_STALE)

    def test_an_advance_cannot_commit_while_a_registration_is_publishing(self):
        """The other side of the same lock: what the publication itself is protected from.

        The advance is released from inside intent.publish, so it contends for the store at the
        one moment that matters. Its Store waits up to thirty seconds for the write lock, so
        pausing here is a bounded observation rather than a race this assertion has to win: if
        the hold did not cover the publication, the advance would be free to commit during it.
        """
        ready, go, advanced, errors = (
            threading.Event(), threading.Event(), threading.Event(), {}
        )

        def advance():
            # Opened before the hold exists: Store() writes its schema rows on open, so a store
            # opened later would block in its constructor and never reach the generation call.
            store = Store(self.store.path)
            ready.set()
            go.wait(timeout=20)
            try:
                Registry(store, FakeClock()).open_generation(
                    self.rid, dispatch_request_id=REVISION,
                    reason="needs_changes_revision", dispatch_turn_id="turn-held",
                )
            except Exception as error:  # noqa: BLE001 - asserted by the caller
                errors["advance"] = error
            finally:
                advanced.set()
                store.close()

        thread = threading.Thread(target=advance)
        thread.start()
        self.addCleanup(thread.join, 60)
        self.assertTrue(ready.wait(timeout=20), "the advance thread never opened its store")
        observed = {}
        publishing = intent.publish

        def publish_while_the_advance_contends(*args, **kwargs):
            go.set()
            observed["advanced_before_the_fact_landed"] = advanced.wait(timeout=0.5)
            return publishing(*args, **kwargs)

        with mock.patch.object(intent, "publish", publish_while_the_advance_contends):
            registered = self.register_marker({"relationshipId": self.rid})
        thread.join(timeout=60)

        self.assertFalse(thread.is_alive(), "the advance never finished")
        self.assertNotIn("advance", errors, f"opening the next generation failed: {errors}")
        self.assertFalse(
            observed["advanced_before_the_fact_landed"],
            "the advance committed while the registration was publishing, so the check and the"
            " publication are not under one hold after all",
        )
        self.assertEqual(registered["outcome"], marker.PUBLISHED)
        self.assertEqual(
            registered["executionGeneration"], 1,
            "the registration published a generation other than the one it checked",
        )
        self.assertEqual(self.published()["executionGeneration"], 1)
        state, readable = intent.dispatch_generation_state(str(self.store.path), self.rid, DISPATCH)
        self.assertTrue(readable)
        self.assertEqual(
            state, intent.DISPATCH_STALE,
            "the advance that was waiting never landed, so this proves nothing about ordering",
        )

    def test_the_window_was_real_and_this_scenario_reaches_it(self):
        """A case that cannot fail proves nothing, so it is run against the shape it replaced.

        The first case asserts that a registration cannot publish inside an uncommitted advance.
        That is only worth something if the scenario reaches the contested region at all - if a
        later change made the advance stop somewhere harmless, or made the registration refuse
        for an unrelated reason, it would still pass and measure nothing. So the pre-CRW-11
        algorithm is run through the identical scenario and is required to publish: read the
        generation on a read-only connection, close it, publish. WAL does not block a reader
        behind a writer, so that read answers current from before the advance.
        """
        holding, release, errors = threading.Event(), threading.Event(), {}
        thread = self.advance_stopped_inside_its_transaction(holding, release, errors)
        thread.start()
        self.addCleanup(thread.join, 60)
        self.assertTrue(holding.wait(timeout=20), "the advance never opened its transaction")

        try:
            unheld = intent.dispatch_generation_state(str(self.store.path), self.rid, DISPATCH)
            outcome = None
            if unheld == (intent.DISPATCH_CURRENT, True):
                outcome = intent._publish_or_compare(
                    self.directory / "relationship.json",
                    {"relationshipId": self.rid, "at": NOW},
                    ("relationshipId",), root=self.markers,
                )
        finally:
            release.set()
        thread.join(timeout=60)

        self.assertNotIn("advance", errors, f"opening the next generation failed: {errors}")
        self.assertEqual(
            unheld, (intent.DISPATCH_CURRENT, True),
            "the unheld read no longer answers current from inside an uncommitted advance, so"
            " this scenario no longer reaches the window and the case above proves nothing",
        )
        self.assertEqual(
            outcome, marker.PUBLISHED,
            "the algorithm this change replaced did not publish here, so the control sample no"
            " longer has the shape the case above exists to rule out",
        )
        self.assertEqual(self.generation(), 2, "the advance did not commit")
        state, _readable = intent.dispatch_generation_state(str(self.store.path), self.rid, DISPATCH)
        self.assertEqual(
            state, intent.DISPATCH_STALE,
            "the fact the old algorithm published was not left naming a superseded generation,"
            " which is the defect CRW-11 closed",
        )


if __name__ == "__main__":
    unittest.main()

