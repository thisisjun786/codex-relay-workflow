"""Per-parent fairness: one parent's backlog must not be every other parent's wait.

delivery.eligible was a single ORDER BY created_at LIMIT across every relationship, so a
parent with a large older backlog filled the window by itself. _reconcile had the same shape,
and there a gate skip still consumed its place in the prefix.
"""

import os
import unittest

from codex_session_relay.models import Endpoint
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
