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


class AdmittedTurnsWithoutReceipts(DaemonTestCase):
    """Admission makes a turn observable even when its producer never reports."""

    def admit(self, relation, turn, *, generation=1, status="completed"):
        from codex_session_relay.admission import admit_explicitly

        self.adapter.start_turn(CHILD, turn_id=turn, status=status)
        admit_explicitly(self.store, self.clock, relation["relationshipId"], generation,
                         turn, actor="assignment-owner", detail="authorized business turn")

    def test_terminal_admitted_turn_settles_without_fabricating_a_report(self):
        relation = self.register()
        self.admit(relation, "business-without-report")
        self.daemon.tick()
        row = self.store.one(
            "SELECT terminal_status FROM assignment_settlements"
            " WHERE relationship_id=? AND turn_id=?",
            (relation["relationshipId"], "business-without-report"))
        self.assertIsNotNone(row)
        self.assertEqual(row["terminal_status"], "completed")
        self.assertEqual(self.store.all("SELECT event_id FROM events"), [])
        self.assertEqual(self.adapter.sends, [])

    def test_unknown_generation_admission_is_not_assignment_work(self):
        relation = self.register()
        self.adapter.start_turn(CHILD, turn_id="unknown-generation-failure", status="failed")
        with self.store.transaction() as db:
            db.execute("INSERT INTO generation_turns VALUES (?,?,?,?,?,?,?)",
                       (relation["relationshipId"], 99, "unknown-generation-failure",
                        "explicit_admission", "old-writer", "", self.clock.iso()))
        selected = self.daemon._turns_to_poll(self.registry.get(relation["relationshipId"]), 8)
        self.assertNotIn("unknown-generation-failure", selected)
        self.daemon.tick()
        self.assertIsNone(self.store.one(
            "SELECT turn_id FROM assignment_settlements WHERE turn_id=?",
            ("unknown-generation-failure",)))
        self.assertEqual(self.store.all("SELECT event_id FROM events"), [])
        self.assertEqual(self.adapter.sends, [])

    def test_active_admitted_turn_is_read_again_but_not_settled(self):
        relation = self.register()
        self.admit(relation, "business-active", status="inProgress")
        self.daemon.tick()
        self.assertIsNotNone(self.store.one(
            "SELECT turn_id FROM poll_observations WHERE turn_id=?", ("business-active",)))
        self.assertIsNone(self.store.one(
            "SELECT turn_id FROM assignment_settlements WHERE turn_id=?", ("business-active",)))
        self.assertIn("business-active", self.daemon._turns_to_poll(
            self.registry.get(relation["relationshipId"]), 8))

    def test_admission_is_scoped_to_its_assignment_on_a_shared_child(self):
        first = self.register()
        second = self.register(issue_key="REL-2", dispatch_request_id="dispatch-2",
                               dispatch_turn_id="second-anchor")
        self.admit(first, "first-business")
        self.admit(second, "second-business")
        selected = self.daemon._turns_to_poll(self.registry.get(first["relationshipId"]), 8)
        self.assertIn("first-business", selected)
        self.assertNotIn("second-business", selected)

    def test_admitted_backlog_rotates_without_spending_the_anchor_share(self):
        relation = self.register()
        self.adapter.start_turn(CHILD, turn_id="turn-dispatch-1", status="inProgress")
        expected = {f"business-{i}" for i in range(12)}
        for turn in sorted(expected):
            self.admit(relation, turn, status="inProgress")
        seen = set()
        for _ in range(12):
            selected = self.daemon._turns_to_poll(self.registry.get(relation["relationshipId"]), 2)
            self.assertLessEqual(len(selected), 2)
            self.assertIn("turn-dispatch-1", selected)
            seen.update(selected)
        self.assertLessEqual(expected, seen)

    def test_old_generation_admission_is_read_until_its_own_settlement(self):
        relation = self.register()
        self.admit(relation, "older-business")
        self.registry.open_generation(relation["relationshipId"], dispatch_request_id="dispatch-next",
                                      dispatch_turn_id="next-anchor", reason="needs_changes_revision")
        current = self.registry.get(relation["relationshipId"])
        self.assertIn("older-business", self.daemon._turns_to_poll(current, 8))
        self.daemon.tick()
        self.assertNotIn("older-business", self.daemon._turns_to_poll(current, 8))


class AdmissionBinding(DaemonTestCase):
    def test_both_writers_refuse_unknown_and_unbound_generations_atomically(self):
        from codex_session_relay.admission import admit_explicitly, record_admission, Admission, EXPLICIT_ADMISSION
        from codex_session_relay.errors import RegistrationError
        relation = self.register()
        rid = relation["relationshipId"]
        self.registry.open_generation(rid, dispatch_request_id="pending", reason="needs_changes_revision")
        for generation in (2, 99):
            for writer in ("operator", "receipt"):
                with self.subTest(generation=generation, writer=writer):
                    with self.assertRaises(RegistrationError):
                        if writer == "operator":
                            admit_explicitly(self.store, self.clock, rid, generation, "bad", actor="owner")
                        else:
                            record_admission(self.store, self.clock, rid, generation, "bad",
                                             Admission(True, EXPLICIT_ADMISSION, "claim"))
        self.assertEqual(self.store.all("SELECT * FROM generation_turns"), [])

    def test_future_legacy_admission_stays_ineligible_after_generation_opens(self):
        from codex_session_relay.admission import AnchorOrExplicit
        relation = self.register()
        rid = relation["relationshipId"]
        self.adapter.start_turn(CHILD, turn_id="unrelated", status="failed")
        with self.store.transaction() as db:
            db.execute("INSERT INTO generation_turns VALUES (?,?,?,?,?,?,?)",
                       (rid, 2, "unrelated", "explicit_admission", "old-writer", "", self.clock.iso()))
        self.registry.open_generation(rid, dispatch_request_id="next", dispatch_turn_id="new-anchor",
                                      reason="needs_changes_revision")
        current = self.registry.get(rid)
        self.assertFalse(AnchorOrExplicit().admit(self.store, current, current["generations"][-1],
                                                "unrelated").admitted)
        self.assertNotIn("unrelated", self.daemon._turns_to_poll(current, 8))
        self.daemon.tick()
        self.assertIsNone(self.store.one("SELECT * FROM assignment_settlements WHERE turn_id='unrelated'"))
        self.assertEqual(self.store.all("SELECT * FROM events"), [])

    def test_fresh_explicit_admission_upgrades_legacy_record(self):
        from codex_session_relay.admission import admit_explicitly, AnchorOrExplicit
        relation = self.register()
        rid = relation["relationshipId"]
        with self.store.transaction() as db:
            db.execute("INSERT INTO generation_turns VALUES (?,?,?,?,?,?,?)",
                       (rid, 1, "legitimate", "explicit_admission", "old-writer", "", self.clock.iso()))
        self.assertFalse(AnchorOrExplicit().admit(self.store, relation, relation["generations"][0],
                                                "legitimate").admitted)
        admit_explicitly(self.store, self.clock, rid, 1, "legitimate", actor="confirmed-owner")
        self.assertTrue(AnchorOrExplicit().admit(self.store, relation, relation["generations"][0],
                                               "legitimate").admitted)
        self.assertEqual(len(self.store.all("SELECT * FROM generation_turns")), 1)
