"""The observation budget must never permanently skip the current generation.

Reproduced in real operation as JUN-100 generation 11 and JUN-101 generation 13: the daemon
was alive and inside its time bound, the child turns were completed, and the events sat staged
with no delivery forever.

_turns_to_poll collected every generation anchor oldest-first, sliced to the per-tick budget,
and _observe filtered already-observed turns AFTER that slice. Past eight generations the
slice was permanently the first eight, all of them already observed, so the current generation
was never selected again.
"""

from codex_session_relay.models import TurnRef

from .support import CHILD, DeliveryTestCase

from .test_daemon import DaemonTestCase


class ObservationBudget(DaemonTestCase):
    def history(self, generations: int):
        """A relationship whose generation count is past the per-tick budget.

        Every older anchor is a completed turn that has ALREADY been observed, which is the
        ordinary state of a long-running assignment and the exact state that starved the",
        current one.
        """
        relationship = self.register()
        self._rid = relationship["relationshipId"]
        for number in range(2, generations + 1):
            turn_id = f"turn-dispatch-{number}"
            self.adapter.start_turn(CHILD, turn_id=turn_id, status="inProgress")
            self.registry.open_generation(
                self._rid, dispatch_request_id=f"dispatch-{number}",
                reason="needs_changes_revision", dispatch_turn_id=turn_id,
            )
            self.adapter.finish_turn(CHILD, turn_id, status="completed")
        # The first anchor too.
        self.adapter.start_turn(CHILD, turn_id="turn-dispatch-1", status="inProgress")
        self.adapter.finish_turn(CHILD, "turn-dispatch-1", status="completed")
        return self.registry.get(self._rid)

    def observe_everything_older(self, current: int):
        """Mark every anchor except the current one as already seen."""
        for number in range(1, current):
            self.intake.record_observation(
                TurnRef(CHILD, f"turn-dispatch-{number}", "completed"), "execution_only",
                relationship_id=self._rid,
            )

    def staged_on_current(self, relationship, generation: int):
        turn_id = f"turn-dispatch-{generation}"
        self.adapter.start_turn(CHILD, turn_id=turn_id + "-live", status="inProgress")
        path = self.artifact("out.txt", "work for the current generation")
        payload = self.ready_payload(
            relationship, [path], generation=generation,
            turn=TurnRef(CHILD, turn_id, "inProgress"),
        )
        self.accept(payload)
        return payload["eventId"]

    def test_the_current_generation_is_polled_however_long_the_history_is(self):
        relationship = self.history(12)
        self.observe_everything_older(12)
        event_id = self.staged_on_current(relationship, 12)
        self.assertEqual(self.intake.row(event_id)["stage"], "staged")

        self.daemon.tick(now=self.clock.now())

        self.assertEqual(
            self.intake.row(event_id)["stage"], "final",
            "twelve generations of already-observed history starved the current one",
        )

    def test_a_tick_never_exceeds_its_read_budget(self):
        relationship = self.history(12)
        self.observe_everything_older(12)
        self.staged_on_current(relationship, 12)
        reads = []
        original = self.adapter.read_turn
        self.adapter.read_turn = lambda thread, turn: (
            reads.append(turn), original(thread, turn),
        )[1]
        self.daemon.tick(now=self.clock.now())
        self.assertLessEqual(
            len(reads), self.daemon.policy.max_turn_reads_per_tick,
            "the bound the loop promises must hold even while it catches up",
        )

    def test_a_receipt_staged_after_the_turn_was_observed_is_still_resolved(self):
        """The ordering the old guard got wrong: already observed is not already finished."""
        relationship = self.register()
        self._rid = relationship["relationshipId"]
        self.adapter.start_turn(CHILD, turn_id="turn-dispatch-1", status="inProgress")
        self.adapter.finish_turn(CHILD, "turn-dispatch-1", status="completed")
        self.daemon.tick(now=self.clock.now())
        self.assertIsNotNone(
            self.store.one(
                "SELECT 1 FROM observations WHERE turn_id = ?", ("turn-dispatch-1",),
            ),
        )

        # The receipt lands AFTER the completion was observed.
        path = self.artifact("late.txt", "written just after the turn ended")
        payload = self.ready_payload(
            relationship, [path], turn=TurnRef(CHILD, "turn-dispatch-1", "inProgress"),
        )
        self.accept(payload)
        self.assertEqual(self.intake.row(payload["eventId"])["stage"], "staged")

        self.daemon.tick(now=self.clock.now())

        self.assertEqual(
            self.intake.row(payload["eventId"])["stage"], "final",
            "a turn already observed must still have its later staged claim resolved",
        )
