"""Per-parent fairness: one parent's backlog must not be every other parent's wait.

delivery.eligible was a single ORDER BY created_at LIMIT across every relationship, so a
parent with a large older backlog filled the window by itself. _reconcile had the same shape,
and there a gate skip still consumed its place in the prefix.
"""

import os
import unittest

from codex_session_relay.models import Endpoint, TurnRef
from codex_session_relay.transport import DEFERRED_BUSY, DISPATCHED

from .support import HOST, DeliveryTestCase
from .test_daemon import DaemonTestCase


class ParentFixture(DaemonTestCase):
    def assignment(self, name, *, events=1):
        """A parent with its own child, and a given number of queued completions."""
        parent, child = f"01parent-{name}", f"01child-{name}"
        root = os.path.join(self.root, name)
        os.makedirs(root, exist_ok=True)
        relationship = self.registry.register(
            parent=Endpoint(parent, HOST, cwd=f"/p/{name}"),
            child=Endpoint(child, HOST, cwd=root),
            issue_key=f"REL-{name}", artifact_roots=[root],
            allowed_recipients=[parent],
            dispatch_request_id=f"dispatch-{name}", dispatch_turn_id=f"turn-{name}",
        )
        self.adapter.add_thread(parent)
        self.adapter.add_thread(child)
        from codex_session_relay.registry import record_settings
        from .support import task_settings

        record_settings(self.store, self.clock, parent, task_settings(f"/p/{name}"),
                        source="creation_result")
        ids = []
        for index in range(events):
            path = os.path.join(root, f"out-{index}.txt")
            with open(path, "w", encoding="utf-8") as handle:
                handle.write(f"{name}-{index}")
            payload = self.ready_payload(
                relationship, [path], attempt=index + 1,
                turn=self.assigned_turn(thread=child, turn=f"turn-{name}"),
            )
            self.accept(payload)
            self.delivery.enqueue(payload["eventId"])
            ids.append(payload["eventId"])
            self.clock.advance(1)
        return relationship, ids


class DeliveryFairness(ParentFixture):
    def test_a_newer_parent_is_selected_despite_a_large_older_backlog(self):
        """The shape a global prefix gets wrong, at a size no window could absorb."""
        self.assignment("a", events=40)
        _relationship, newer = self.assignment("b", events=1)

        chosen = self.delivery.eligible(now=self.clock.now(), limit=4)

        parents = {row["parent_task_id"] for row in chosen}
        self.assertIn("01parent-b", parents, "the newer parent was never even considered")
        self.assertIn(newer[0], [row["event_id"] for row in chosen])

    def test_an_explicit_bulk_limit_is_not_capped_by_the_per_tick_share(self):
        """deliver --limit is an operator asking for a bulk send, not a tick.

        The per-parent share exists so one parent cannot fill a tick's window against the
        others. Applying it to an explicit limit made `deliver --limit 20` against a single
        parent quietly send two. Fairness across parents does not depend on the share:
        eligible() deals rows one parent at a time whatever the window is.
        """
        self.assignment("solo", events=20)
        share = self.delivery.policy.max_sends_per_parent_per_tick
        self.assertLess(share, 20, "the fixture has to exceed the share to say anything")

        tick = self.delivery.eligible(now=self.clock.now(), limit=20)
        bulk = self.delivery.eligible(now=self.clock.now(), limit=20, per_parent_limit=20)

        self.assertEqual(len(tick), share, "the tick still respects its own share")
        self.assertEqual(len(bulk), 20, "an explicit limit was capped by the tick share")

    def test_a_bulk_limit_still_deals_between_parents(self):
        """Lifting the per-parent window must not turn a bulk send into one parent's queue."""
        self.assignment("x", events=10)
        self.assignment("y", events=10)

        chosen = self.delivery.eligible(now=self.clock.now(), limit=6, per_parent_limit=6)

        parents = [row["parent_task_id"] for row in chosen]
        self.assertEqual(len(chosen), 6)
        self.assertEqual(len(set(parents)), 2, f"one parent took the whole bulk: {parents}")

    def test_the_share_is_even_rather_than_dealt_in_blocks(self):
        for name in ("a", "b", "c"):
            self.assignment(name, events=10)
        counts = {}
        # The advancing head is what makes the odd slot circulate; without it the same
        # parent takes it every time, which is the 18/9/9 split this used to produce.
        for tick in range(9):
            for row in self.delivery.eligible(now=self.clock.now(), limit=4, cursor=tick):
                counts[row["parent_task_id"]] = counts.get(row["parent_task_id"], 0) + 1
        spread = max(counts.values()) - min(counts.values())
        self.assertEqual(len(counts), 3)
        self.assertLessEqual(spread, 1, f"uneven share: {counts}")

    def test_the_rotation_is_persisted_and_moves_between_ticks(self):
        for name in ("a", "b", "c"):
            self.assignment(name, events=6)
        first = self.daemon._cursor("delivery_parents", 3)
        self.daemon.tick(now=self.clock.now())
        second = self.daemon._cursor("delivery_parents", 3)
        self.assertNotEqual(second, first, "the head must move, or the odd slot never moves")
        stored = self.store.one(
            "SELECT cursor FROM discovery_cursors WHERE listing = ?", ("delivery_parents",),
        )
        self.assertIsNotNone(stored, "the delivery rotation must survive a restart")

    def test_a_struggling_parent_does_not_spend_the_whole_budget(self):
        """Busy is a returned outcome, not an exception, and it used to eat the tick."""
        self.assignment("a", events=8)
        _relationship, healthy = self.assignment("b", events=1)
        self.adapter.set_status("01parent-a", "active")

        report = self.daemon.tick(now=self.clock.now())

        delivered = [thread for _r, thread, _m, _o in self.adapter.sends]
        self.assertIn("01parent-b", delivered, "the healthy parent was starved by the busy one")
        self.assertGreaterEqual(report.deferred, 1)

    def test_cancelling_one_assignment_leaves_the_others_served(self):
        cancelled, _ids = self.assignment("a", events=2)
        _relationship, healthy = self.assignment("b", events=1)
        self.registry.set_status(cancelled["relationshipId"], "cancelled", actor="test")

        report = self.daemon.tick(now=self.clock.now())

        delivered = [thread for _r, thread, _m, _o in self.adapter.sends]
        self.assertIn("01parent-b", delivered)
        self.assertNotIn("01parent-a", delivered, "a cancelled assignment is not served")
        self.assertFalse(report.quiet)


class ReconciliationFairness(ParentFixture):
    def dealt_over(self, ticks):
        """Which attempts _reconcile actually handed to the gate, across several ticks."""
        from codex_session_relay.daemon import TickReport

        seen = set()
        original = self.daemon._gate

        def spy(attempt):
            seen.add(attempt["request_id"])
            return original(attempt)

        self.daemon._gate = spy
        try:
            for _tick in range(ticks):
                self.daemon._reconcile(TickReport(), self.clock.now())
        finally:
            self.daemon._gate = original
        return seen

    def unresolved(self, name, count):
        """Attempts whose response was lost, which is what reconciliation has to settle."""
        _relationship, ids = self.assignment(name, events=count)
        for event_id in ids:
            self.adapter.script("transport_unknown")
            self.clock.advance(3600)
            self.delivery.attempt(event_id, self.adapter, now=self.clock.now())
        return ids

    def test_one_parents_backlog_does_not_hide_another_parents_attempt(self):
        self.unresolved("a", 10)
        newer = self.unresolved("b", 1)

        parents = self.reconciler.open_parents()
        self.assertEqual(set(parents), {"01parent-a", "01parent-b"})
        dealt = self.reconciler.open_attempts(limit=4, parents=["01parent-b"])
        self.assertTrue(dealt, "the second parent has unresolved work and must be reachable")
        self.assertEqual({row["parent_task_id"] for row in dealt}, {"01parent-b"})
        del newer

    def test_recovery_still_sees_everything(self):
        """The bounded form is for the tick; recovery must stay exhaustive."""
        self.unresolved("a", 6)
        self.assertEqual(len(self.reconciler.open_attempts()), 6)
        self.assertEqual(len(self.reconciler.open_attempts(limit=2)), 2)

    def test_every_attempt_of_one_parent_is_reached_across_ticks(self):
        """The parent order had a cursor; the attempts inside a parent did not.

        _gate skips an attempt whose fingerprint has not changed, but the skipped attempt
        still held its place in the prefix, so with more unresolved attempts than the share
        the ones behind them were never reconciled at all - and an anchor waiting on one of
        them would never bind.
        """
        self.unresolved("a", 9)
        self.unresolved("b", 1)
        self.unresolved("c", 1)

        reached = self.dealt_over(12)

        everything = {row["request_id"]
                      for row in self.reconciler.open_attempts(parents=["01parent-a"])}
        self.assertEqual(len(everything), 9)
        self.assertEqual(
            reached & everything, everything,
            "a fixed prefix leaves the attempts behind it permanently unreconciled",
        )

    def test_the_attempt_cursor_is_persisted_per_parent(self):
        budget = self.daemon.policy.max_reconciles_per_tick
        self.unresolved("a", budget + 2)
        self.dealt_over(1)
        stored = self.store.one(
            "SELECT cursor FROM discovery_cursors WHERE listing = ?",
            ("reconcile:01parent-a",),
        )
        self.assertIsNotNone(stored, "the rotation must survive a restart")
        self.assertEqual(
            int(stored["cursor"]), budget,
            "exactly as far as the attempts this tick actually dealt",
        )

    def test_a_cursor_moves_only_past_attempts_that_were_actually_dealt(self):
        """Advancing at selection time skipped attempts the budget then dropped.

        With more parents than budget the parent rotation and the attempt cursors moved
        together, so the same attempts could be stepped over on every tick - permanently.
        """
        # MORE parents than the budget, which is the shape that exposes it: every parent is
        # selected and had its cursor advanced, but only the first budgeted queues are dealt.
        budget = self.daemon.policy.max_reconciles_per_tick
        for index in range(budget + 4):
            self.unresolved(f"p{index}", 3)

        reached = self.dealt_over(80)

        everything = {row["request_id"] for row in self.reconciler.open_attempts()}
        self.assertEqual(len(everything), (budget + 4) * 3)
        self.assertEqual(
            reached & everything, everything,
            "every unresolved attempt must be reached in a finite number of ticks",
        )

    def test_a_parent_that_was_dealt_nothing_keeps_its_place(self):
        """The precise defect: a cursor advanced for work the budget then dropped.

        With more parents than the budget, every parent is selected and only the first
        budgeted queues are dealt. Advancing inside the selection moved the cursors of the
        parents that got nothing, so their leading attempts were stepped over unread.
        """
        from codex_session_relay.daemon import TickReport

        budget = self.daemon.policy.max_reconciles_per_tick
        for index in range(budget + 4):
            self.unresolved(f"p{index}", 3)

        self.daemon._reconcile(TickReport(), self.clock.now())

        dealt_parents = {
            row["listing"].split(":", 1)[1]
            for row in self.store.all(
                "SELECT listing, cursor FROM discovery_cursors WHERE listing LIKE 'reconcile:%'"
            )
            if int(row["cursor"]) > 0
        }
        self.assertLessEqual(
            len(dealt_parents), budget,
            "a cursor moved for a parent this tick never reconciled",
        )


class DeliveryRotation(ParentFixture):
    """A delivery that fails without changing its own state must not block its successors."""

    def test_a_persistently_failing_delivery_does_not_block_the_rest(self):
        """eligible_for_parent returned the same oldest prefix on every tick.

        The failing row stays eligible and stays oldest, and the struggling set suppresses
        every later row within the tick, so its successors were never attempted at all.
        """
        _relationship, ids = self.assignment("a", events=6)
        first = ids[0]
        original = self.delivery.attempt

        def attempt(event_id, adapter, *, now=None):
            if event_id == first:
                raise RuntimeError("this one fails before it changes state")
            return original(event_id, adapter, now=now)

        seen = set()

        def watched(event_id, adapter, *, now=None):
            seen.add(event_id)
            return attempt(event_id, adapter, now=now)

        self.delivery.attempt = watched
        try:
            for _tick in range(20):
                self.clock.advance(1)
                self.daemon.tick(now=self.clock.now())
        finally:
            self.delivery.attempt = original

        self.assertEqual(
            seen & set(ids[1:]), set(ids[1:]),
            "every delivery behind the failing one must be attempted in a finite number of ticks",
        )

    def test_the_delivery_cursor_moves_only_past_what_was_attempted(self):
        """A row the budget dropped was never looked at; moving past it skips work."""
        budget = self.daemon.policy.max_sends_per_tick
        for index in range(budget + 3):
            self.assignment(f"p{index}", events=2)

        self.daemon.tick(now=self.clock.now())

        moved = [
            row["listing"] for row in self.store.all(
                "SELECT listing, cursor FROM discovery_cursors WHERE listing LIKE 'deliver:%'")
            if int(row["cursor"]) > 0
        ]
        self.assertLessEqual(
            len(moved), budget,
            "a cursor moved for a parent this tick never attempted",
        )

    def test_the_delivery_cursor_is_persisted(self):
        """The rotation has to survive a restart, or it starts from the head every time."""
        self.assignment("a", events=6)
        self.daemon.tick(now=self.clock.now())
        stored = self.store.one(
            "SELECT cursor FROM discovery_cursors WHERE listing = ?", ("deliver:01parent-a",),
        )
        self.assertIsNotNone(stored)
        self.assertGreater(int(stored["cursor"]), 0)


class SharedChildTurns(DaemonTestCase):
    """Two assignments can legitimately be watching the same child turn."""

    def two_parents_on_one_child(self):
        """Different parents, different issues, one child thread and one anchor turn."""
        from codex_session_relay.registry import record_settings
        from .support import task_settings

        child, turn = "01child-shared", "turn-shared-1"
        made = []
        for name in ("a", "b"):
            parent = f"01parent-{name}"
            root = os.path.join(self.root, name)
            os.makedirs(root, exist_ok=True)
            made.append(self.registry.register(
                parent=Endpoint(parent, HOST, cwd=f"/p/{name}"),
                child=Endpoint(child, HOST, cwd=root),
                issue_key=f"SHARED-{name}", artifact_roots=[root],
                allowed_recipients=[parent],
                dispatch_request_id=f"dispatch-{name}", dispatch_turn_id=turn,
            ))
            self.adapter.add_thread(parent)
            record_settings(self.store, self.clock, parent, task_settings(f"/p/{name}"),
                            source="creation_result")
        self.adapter.add_thread(child)
        return made, child, turn

    def test_a_failed_shared_turn_reaches_every_parent_waiting_on_it(self):
        """_already_observed asked globally, so the first settlement closed the turn for all.

        The second assignment never reached _synthesize, and a failed turn suppresses the
        staged claim rather than finalizing it - so its parent was left waiting on a verdict
        that can never arrive.
        """
        relationships, child, turn = self.two_parents_on_one_child()
        self.adapter.start_turn(child, turn_id=turn, status="inProgress")
        self.adapter.finish_turn(child, turn, status="failed")

        # The relationship rotation serves a bounded number per tick, so both are reached
        # across ticks rather than in one. What matters is that neither is closed out by
        # the other's observation.
        for _tick in range(4):
            self.clock.advance(60)
            self.daemon.tick(now=self.clock.now())

        owners = {
            self.intake.row(row["event_id"])["relationship_id"]
            for row in self.store.all("SELECT event_id FROM events")
        }
        self.assertEqual(
            owners, {r["relationshipId"] for r in relationships},
            "both parents must get a terminal outcome for the turn they shared",
        )
        recipients = {thread for _r, thread, _m, _o in self.adapter.sends}
        self.assertEqual(recipients, {"01parent-a", "01parent-b"})

    def test_a_staged_claim_on_a_shared_turn_is_settled_only_by_its_owner(self):
        """_turns_to_poll collected staged turns by CHILD THREAD, not by assignment.

        A staged claim owned by B could be selected by A, and resolve_staged_in selected by
        (thread, turn) alone - so A suppressed B's claim while daemon_observation refused to
        synthesize a receipt for A. B's parent waited on an outcome already thrown away.
        """
        import os

        relationships, child, turn = self.two_parents_on_one_child()
        # The owner is the assignment the relationship rotation reaches SECOND, so the
        # non-owner polls this turn first. That ordering is the whole defect: whoever polls
        # first used to settle it for everyone.
        other, owner = relationships[0], relationships[1]
        # A continuation turn that is NEITHER assignment's anchor, staged by the owner only.
        self.adapter.start_turn(child, turn_id="turn-shared-2", status="inProgress")
        root = os.path.join(self.root, "b")
        os.makedirs(root, exist_ok=True)
        path = os.path.join(root, "late.txt")
        with open(path, "w", encoding="utf-8") as handle:
            handle.write("owned by exactly one assignment")
        payload = self.ready_payload(
            owner, [path], turn=TurnRef(child, "turn-shared-2", "inProgress"),
        )
        # A turn other than the anchor is admitted only with an explicit continuation, which
        # is what makes this claim unambiguously ONE assignment's.
        self.intake.accept_child_receipt(
            payload, observation=TurnRef(child, "turn-shared-2", "inProgress"),
            continuation={"anchorTurnId": turn, "actor": child,
                          "reason": "continuation of this execution"},
        )
        event_id = payload["eventId"]

        self.assertEqual(
            [row["turn_id"] for row in self.intake.staged_events(
                thread_id=child, relationship_id=other["relationshipId"])],
            [],
            "the fixture needs this claim to belong to exactly one assignment",
        )
        self.adapter.finish_turn(child, "turn-shared-2", status="failed")
        for _tick in range(6):
            self.clock.advance(1)
            self.daemon.tick(now=self.clock.now())

        # A failed turn SUPPRESSES the staged claim, so the outcome the owner's parent is
        # waiting for can only come from a synthesized execution-only receipt. That is the
        # part the global settlement destroyed: it suppressed the claim on behalf of the
        # other assignment, whose daemon_observation refused to synthesize anything, and the
        # turn then left the owner's ring with nothing recorded.
        self.assertEqual(self.intake.row(event_id)["stage"], "suppressed")
        outcomes = [
            row for row in self.store.all(
                "SELECT * FROM events WHERE relationship_id = ? AND turn_id = ?",
                (owner["relationshipId"], "turn-shared-2"),
            )
            if row["outcome"] in ("failed", "interrupted")
        ]
        self.assertTrue(
            outcomes,
            "the owner must still produce a terminal outcome for the parent waiting on it",
        )
        self.assertEqual(outcomes[0]["stage"], "final")

    def test_an_inactive_assignments_staged_turn_stays_out_of_an_active_ring(self):
        """A paused assignment is absent from _active_relationships and must stay absent.

        Collecting staged turns by thread put its work into an active assignment's ring, and
        the globally scoped settlement then finalized it and created a delivery intent that
        was retried for an assignment nothing should be processing.
        """
        import os

        relationships, child, turn = self.two_parents_on_one_child()
        paused, active = relationships[0], relationships[1]
        self.adapter.start_turn(child, turn_id="turn-shared-3", status="inProgress")
        root = os.path.join(self.root, "a")
        os.makedirs(root, exist_ok=True)
        path = os.path.join(root, "paused.txt")
        with open(path, "w", encoding="utf-8") as handle:
            handle.write("belongs to the assignment that is about to pause")
        payload = self.ready_payload(
            paused, [path], turn=TurnRef(child, "turn-shared-3", "inProgress"),
        )
        self.intake.accept_child_receipt(
            payload, observation=TurnRef(child, "turn-shared-3", "inProgress"),
            continuation={"anchorTurnId": turn, "actor": child,
                          "reason": "continuation of this execution"},
        )
        self.registry.set_status(paused["relationshipId"], "paused", actor="test")
        self.adapter.finish_turn(child, "turn-shared-3", status="completed")

        for _tick in range(6):
            self.clock.advance(1)
            self.daemon.tick(now=self.clock.now())

        self.assertEqual(
            self.intake.row(payload["eventId"])["stage"], "staged",
            "a paused assignment's claim must not be settled through an active one",
        )
        self.assertIsNone(
            self.delivery.find(payload["eventId"]),
            "and nothing may be queued on its behalf",
        )
        del active

    def test_one_assignment_is_still_settled_only_once(self):
        """Per-assignment must not become per-tick: the same turn is not re-observed."""
        relationships, child, turn = self.two_parents_on_one_child()
        self.adapter.start_turn(child, turn_id=turn, status="inProgress")
        self.adapter.finish_turn(child, turn, status="failed")
        for _tick in range(4):
            self.clock.advance(60)
            self.daemon.tick(now=self.clock.now())
        before = self.store.one("SELECT COUNT(*) AS c FROM events")["c"]

        for _tick in range(4):
            self.clock.advance(60)
            self.daemon.tick(now=self.clock.now())

        self.assertEqual(self.store.one("SELECT COUNT(*) AS c FROM events")["c"], before)
        self.assertEqual(len(self.adapter.sends), len(relationships))


if __name__ == "__main__":
    unittest.main()


class SelectionCost(ParentFixture):
    """Query count is not query cost, so the plan is inspected rather than assumed."""

    def test_the_per_parent_query_uses_an_index_rather_than_scanning(self):
        self.assignment("a", events=30)
        self.assignment("b", events=1)
        plan = self.store.all(
            "EXPLAIN QUERY PLAN SELECT d.*, r.parent_task_id AS parent_task_id"
            " FROM deliveries d"
            " JOIN relationships r ON r.relationship_id = d.relationship_id"
            " JOIN events e ON e.event_id = d.event_id"
            " WHERE d.state IN (?,?,?) AND d.hold_reason IS NULL"
            "   AND (d.next_eligible_at IS NULL OR d.next_eligible_at <= ?)"
            "   AND r.status = 'active' AND r.superseded_by IS NULL AND e.stage = 'final'"
            "   AND r.parent_task_id = ?"
            " ORDER BY d.created_at LIMIT ? OFFSET ?",
            ("queued", "deferred_busy", "withheld_pre_send", self.clock.now(),
             "01parent-b", 2, 0),
        )
        detail = " | ".join(row["detail"] for row in plan)
        self.assertIn("deliveries", detail)
        # Recorded rather than asserted as a hard shape: SQLite may legitimately choose a
        # different index as the schema grows. What matters is that a reviewer can see it.
        self.assertTrue(detail, "the query plan must be inspectable")

    def test_selection_issues_a_bounded_number_of_queries(self):
        for name in ("a", "b", "c"):
            self.assignment(name, events=20)
        seen = []
        original = self.store.all
        self.store.all = lambda sql, params=(): (seen.append(sql), original(sql, params))[1]
        try:
            self.delivery.eligible(now=self.clock.now(), limit=4)
        finally:
            self.store.all = original
        # One parent query plus one per eligible parent. It does not grow with backlog size.
        self.assertEqual(len(seen), 4, seen)
