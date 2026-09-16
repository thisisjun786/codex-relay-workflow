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


class SchedulingFairness(DaemonTestCase):
    """The finite-coverage claim, under the shapes that break a prefix."""

    def relationship(self, suffix, *, generations=1):
        from codex_session_relay.models import Endpoint

        child = f"01child-{suffix}"
        root = self.artifact(f"{suffix}/seed.txt", "seed")
        record = self.registry.register(
            parent=Endpoint(f"01parent-{suffix}", "host-a", cwd=f"/p/{suffix}"),
            child=Endpoint(child, "host-a", cwd=f"/c/{suffix}"),
            issue_key=f"REL-{suffix}", artifact_roots=[self.root],
            allowed_recipients=[f"01parent-{suffix}"],
            dispatch_request_id=f"dispatch-{suffix}-1",
            dispatch_turn_id=f"turn-{suffix}-1",
        )
        self.adapter.add_thread(child)
        for number in range(1, generations + 1):
            turn_id = f"turn-{suffix}-{number}"
            self.adapter.start_turn(child, turn_id=turn_id, status="inProgress")
            if number > 1:
                self.registry.open_generation(
                    record["relationshipId"],
                    dispatch_request_id=f"dispatch-{suffix}-{number}",
                    reason="needs_changes_revision", dispatch_turn_id=turn_id,
                )
            self.adapter.finish_turn(child, turn_id, status="completed")
        del root
        return record, child

    def observed_turns(self):
        return {
            row["turn_id"] for row in self.store.all("SELECT turn_id FROM observations")
        }

    def test_every_relationship_is_served_even_past_the_read_budget(self):
        """More relationships than the tick can read, so the rotation has to carry them."""
        names = [f"r{index}" for index in range(12)]
        for name in names:
            self.relationship(name)
        for _ in range(24):
            self.daemon.tick(now=self.clock.now())
            self.clock.advance(1)
        seen = self.observed_turns()
        missing = [name for name in names if f"turn-{name}-1" not in seen]
        self.assertEqual(missing, [], "the rotation left relationships unserved")

    def test_an_empty_window_does_not_pin_the_rotation(self):
        """The cursor has to move even when the served relationships had nothing to do."""
        idle = [f"i{index}" for index in range(6)]
        for name in idle:
            self.relationship(name)
        # Drain them, so the next ticks select windows with no candidates at all.
        for _ in range(12):
            self.daemon.tick(now=self.clock.now())
            self.clock.advance(1)
        self.assertTrue(all(f"turn-{name}-1" in self.observed_turns() for name in idle))

        record, child = self.relationship("latecomer")
        for _ in range(12):
            self.daemon.tick(now=self.clock.now())
            self.clock.advance(1)
        self.assertIn(
            "turn-latecomer-1", self.observed_turns(),
            "a window of relationships with nothing to do must not pin the cursor",
        )

    def test_a_backlog_larger_than_the_share_is_covered_in_finite_ticks(self):
        record, child = self.relationship("deep", generations=9)
        for _ in range(20):
            self.daemon.tick(now=self.clock.now())
            self.clock.advance(1)
        seen = self.observed_turns()
        missing = [n for n in range(1, 10) if f"turn-deep-{n}" not in seen]
        self.assertEqual(missing, [], "the ring cursor did not cover the whole backlog")

    def test_the_rotation_survives_a_restart(self):
        from codex_session_relay.daemon import RelayDaemon

        for index in range(8):
            self.relationship(f"s{index}")
        self.daemon.tick(now=self.clock.now())
        first = self.store.one(
            "SELECT cursor FROM discovery_cursors WHERE listing = ?", ("relationships",),
        )
        self.assertIsNotNone(first, "the rotation is persisted, not held in memory")
        fresh = RelayDaemon(
            self.store, self.registry, self.intake, self.delivery, self.ack,
            self.reconciler, self.adapter, clock=self.clock,
        )
        self.clock.advance(1)
        fresh.tick(now=self.clock.now())
        second = self.store.one(
            "SELECT cursor FROM discovery_cursors WHERE listing = ?", ("relationships",),
        )
        self.assertNotEqual(second["cursor"], first["cursor"])

    def test_a_turn_that_cannot_be_read_does_not_pin_the_ring(self):
        record, child = self.relationship("blocked", generations=4)
        original = self.adapter.read_turn

        def refuse(thread, turn):
            if turn == "turn-blocked-1":
                raise ConnectionError("this one never answers")
            return original(thread, turn)

        self.adapter.read_turn = refuse
        for _ in range(16):
            self.daemon.tick(now=self.clock.now())
            self.clock.advance(1)
        seen = self.observed_turns()
        self.assertTrue(
            {"turn-blocked-2", "turn-blocked-3", "turn-blocked-4"} <= seen,
            "one unreadable turn must not starve the rest of the ring",
        )
