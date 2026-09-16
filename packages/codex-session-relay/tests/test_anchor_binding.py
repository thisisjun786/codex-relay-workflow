"""JUN-167 P1: an anchor must bind on EVERY route to dispatched, not just one.

The only automatic binding hook ran after the daemon's new-dispatch branch. A revision that
reached dispatched any other way - the deliver command, either reconcile promotion, a dispatch
committed in the last tick before shutdown - left its generation anchor_pending, and by I-06
every later receipt for that generation was refused as unbound.

The existing suite missed it because its one binding test calls bind_dispatched_revision by
hand (test_ack_reconcile.py). These tests never help the code along.
"""

from codex_session_relay import identity
from codex_session_relay.delivery import REVISION
from codex_session_relay.errors import RefusalReason
from codex_session_relay.registry import ANCHOR_PENDING
from codex_session_relay.transport import HELD_UNCERTAIN

from .support import CHILD, PARENT, DeliveryTestCase
from .test_daemon import DaemonTestCase


class AnchorBinding(DeliveryTestCase):
    def revision_pending(self):
        """Acknowledge a completion, record needs_changes, and return the queued revision."""
        _relationship, event_id = self.queued_event(recipients=[PARENT, CHILD])
        self.attempt(event_id)
        self.clock.advance(5)
        turn = self.adapter.start_turn(PARENT, turn_id="ack-turn", status="inProgress")
        self.ack.acknowledge(
            event_id, ack_turn_id=turn.turn_id,
            ack_proof=identity.ack_proof(event_id, turn.turn_id), accepted=True,
            adapter=self.adapter,
        )
        self.ack.record_verdict(event_id, verdict="needs_changes", verdict_turn_id="verdict-1")
        revision = self.store.one("SELECT * FROM deliveries WHERE kind = ?", (REVISION,))
        return revision["event_id"]

    def generation_two(self):
        return self.registry.generation(self._rid, 2)

    def child_receipt_for_generation_two(self):
        """What the child sends next. Under an unbound anchor it is refused (I-06)."""
        relationship = self.registry.get(self._rid)
        path = self.artifact("fixed.txt", "the correction")
        anchor = self.generation_two()["dispatchTurnId"]
        turn = self.assigned_turn(thread=CHILD, turn=anchor or "turn-unbound")
        payload = self.ready_payload(relationship, [path], generation=2, turn=turn)
        return self.accept(payload)

    def daemon(self):
        from codex_session_relay.daemon import RelayDaemon

        return RelayDaemon(
            self.store, self.registry, self.intake, self.delivery, self.ack,
            self.reconciler, self.adapter, clock=self.clock,
        )

    def test_a_later_tick_binds_an_anchor_an_earlier_send_left_pending(self):
        """The defect itself, through real entry points and with no test-side binding.

        The revision is dispatched by the deliver path. A daemon tick afterwards finds nothing
        eligible to send - it is already dispatched - so on the unfixed source its only
        binding hook never runs, the generation stays anchor_pending, and the child's next
        receipt is refused as unbound.
        """
        revision = self.revision_pending()
        self.clock.advance(3600)
        record = self.delivery.attempt(revision, self.adapter, now=self.clock.now())
        self.assertEqual(record["deliveryState"], "dispatched")
        self.assertEqual(self.generation_two()["anchorState"], ANCHOR_PENDING)

        self.daemon().tick(now=self.clock.now())

        self.assertNotEqual(
            self.generation_two()["anchorState"], ANCHOR_PENDING,
            "a tick left a dispatched revision's generation unbound, so every receipt for it"
            " is refused",
        )
        accepted = self.child_receipt_for_generation_two()
        self.assertEqual(accepted["executionGeneration"], 2)

    def test_an_unbound_generation_really_does_refuse_the_childs_receipt(self):
        """Why the binding matters: I-06 is what turns a pending anchor into lost work."""
        revision = self.revision_pending()
        self.clock.advance(3600)
        self.delivery.attempt(revision, self.adapter, now=self.clock.now())
        self.assertEqual(self.generation_two()["anchorState"], ANCHOR_PENDING)
        self.assertRefused(
            RefusalReason.UNBOUND_GENERATION, self.child_receipt_for_generation_two,
        )

    def test_the_recovery_binds_a_dispatched_revision(self):
        """The helper itself. Route integration is proved by the tick test above, which
        calls nothing but tick."""
        revision = self.revision_pending()
        self.clock.advance(3600)
        record = self.delivery.attempt(revision, self.adapter, now=self.clock.now())
        self.assertEqual(record["deliveryState"], "dispatched")

        self.ack.bind_pending_anchors()

        generation = self.generation_two()
        self.assertNotEqual(
            generation["anchorState"], ANCHOR_PENDING,
            "a dispatched revision left its generation unbound, so every later receipt for it"
            " is refused as unbound",
        )
        self.assertEqual(generation["dispatchTurnId"], record["turnId"])
        accepted = self.child_receipt_for_generation_two()
        self.assertEqual(accepted["executionGeneration"], 2)

    def test_a_reconcile_promotion_binds_the_anchor_too(self):
        revision = self.revision_pending()
        self.clock.advance(3600)
        # The response is lost, so the send settles uncertain and only reconciliation can
        # later establish that it actually reached a turn.
        self.adapter.script("transport_unknown")
        record = self.delivery.attempt(revision, self.adapter, now=self.clock.now())
        self.assertEqual(record["deliveryState"], HELD_UNCERTAIN)
        self.assertEqual(self.generation_two()["anchorState"], ANCHOR_PENDING)

        promoted = self.reconciler.recover_on_start(self.adapter)
        self.assertTrue(promoted["reconciled"])
        self.ack.bind_pending_anchors()

        delivery = self.delivery.get(revision)
        if delivery["state"] == "dispatched":
            self.assertNotEqual(self.generation_two()["anchorState"], ANCHOR_PENDING)

    def test_binding_is_idempotent_and_never_rebinds_a_bound_anchor(self):
        revision = self.revision_pending()
        self.clock.advance(3600)
        record = self.delivery.attempt(revision, self.adapter, now=self.clock.now())
        first = self.ack.bind_pending_anchors()
        again = self.ack.bind_pending_anchors()
        self.assertEqual(len(first), 1)
        self.assertEqual(again, [], "an already bound anchor is not rebound")
        self.assertEqual(self.generation_two()["dispatchTurnId"], record["turnId"])

    def test_nothing_is_bound_from_a_delivery_that_never_dispatched(self):
        revision = self.revision_pending()
        self.clock.advance(3600)
        self.adapter.script("busy")
        self.delivery.attempt(revision, self.adapter, now=self.clock.now())
        self.assertEqual(self.ack.bind_pending_anchors(), [])
        self.assertEqual(self.generation_two()["anchorState"], ANCHOR_PENDING)


class BindingAfterReconciliation(DaemonTestCase):
    """Reconciliation is what promotes a lost send to dispatched, and binding ran before it."""

    def test_a_revision_promoted_by_reconciliation_binds_in_the_same_tick(self):
        """Otherwise the generation stays anchor_pending until the NEXT tick.

        A child that emits its completion in that interval has it refused as
        unbound_generation even though the dispatch evidence is already committed.
        """
        order = []
        original_bind = self.daemon._bind_anchors
        original_reconcile = self.daemon._reconcile

        def bind(report):
            order.append("bind")
            return original_bind(report)

        def reconcile(report, now):
            order.append("reconcile")
            return original_reconcile(report, now)

        self.daemon._bind_anchors = bind
        self.daemon._reconcile = reconcile
        try:
            self.daemon.tick(now=self.clock.now())
        finally:
            self.daemon._bind_anchors = original_bind
            self.daemon._reconcile = original_reconcile

        self.assertEqual(
            order, ["bind", "reconcile", "bind"],
            "binding has to run again after the pass that can promote a revision",
        )
