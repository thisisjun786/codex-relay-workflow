"""JUN-167 P1: an anchor must bind on EVERY route to dispatched, not just one.

The only automatic binding hook ran after the daemon's new-dispatch branch. A revision that
reached dispatched any other way - the deliver command, either reconcile promotion, a dispatch
committed in the last tick before shutdown - left its generation anchor_pending, and by I-06
every later receipt for that generation was refused as unbound.

The existing suite missed it because its one binding test calls bind_dispatched_revision by
hand (test_ack_reconcile.py). These tests never help the code along.
"""

from unittest import mock

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

    def lost_settle_write(self):
        """A revision the transport really accepted, whose settle write was lost.

        The same shape RestartRecovery uses: the send is held uncertain, and only re-reading
        the operation receipt establishes afterwards that it reached a turn.
        """
        revision = self.revision_pending()
        self.clock.advance(3600)
        self.adapter.script("in_progress")
        record = self.delivery.attempt(revision, self.adapter, now=self.clock.now())
        self.assertEqual(record["deliveryState"], HELD_UNCERTAIN)
        self.assertEqual(self.generation_two()["anchorState"], ANCHOR_PENDING)
        turn = self.adapter.start_turn(CHILD, status="inProgress")
        self.adapter.ledger[record["requestId"]] = {
            "requestId": record["requestId"], "status": "accepted",
            "resumed": {"approvalPolicy": "never"}, "turnId": turn.turn_id,
        }
        return revision, turn.turn_id

    def test_a_reconciled_promotion_binds_before_any_repair_pass_runs(self):
        """The interval the second binding pass narrowed but could not close.

        _write commits the promotion to dispatched and the repair pass commits the binding in
        a LATER transaction. A child emitting from another process in between has a valid
        completion refused as unbound_generation even though the dispatch evidence is already
        durable. So the binding travels in the transaction that promotes - and this test never
        calls a binding pass, because the whole point is that it no longer has to.
        """
        revision, turn_id = self.lost_settle_write()

        self.reconciler.recover_on_start(self.adapter)

        self.assertEqual(self.delivery.get(revision)["state"], "dispatched")
        self.assertNotEqual(
            self.generation_two()["anchorState"], ANCHOR_PENDING,
            "the promotion committed without its binding, so a receipt arriving before the"
            " next repair pass is refused as unbound",
        )
        self.assertEqual(self.generation_two()["dispatchTurnId"], turn_id)
        self.assertEqual(self.child_receipt_for_generation_two()["executionGeneration"], 2)

    def test_a_promotion_disagreeing_with_a_bound_anchor_is_recorded_not_swallowed(self):
        """A conflict the repair pass would never see, because it reads pending ones only.

        Deferring it to that pass was the first correction I proposed, and it was wrong: the
        generation is already bound, so bind_pending_anchors never selects it again and the
        disagreement would simply vanish. The bound anchor is still never overwritten.
        """
        _revision, _turn_id = self.lost_settle_write()
        self.registry.bind_anchor(
            self._rid, 2, dispatch_turn_id="a-different-turn", source="dispatch_receipt",
        )

        self.reconciler.recover_on_start(self.adapter)

        self.assertEqual(
            self.generation_two()["dispatchTurnId"], "a-different-turn",
            "a promotion must never move an anchor that is already bound",
        )
        recorded = self.store.all(
            "SELECT detail FROM journal WHERE kind = ?", ("anchor_conflict",),
        )
        self.assertTrue(recorded, "the disagreement was neither reported nor recorded")
        self.assertIn("a-different-turn", recorded[0]["detail"])

    def test_a_stale_reconciliation_does_not_bind_the_obsolete_attempts_turn(self):
        """The guarded UPDATE is the race check, not the snapshot _is_current read.

        _is_current answers from a delivery row fetched before the transaction opened. If
        another worker settles that delivery and dispatches a later attempt in between, the
        guarded UPDATE matches no rows - and binding anyway would hand the generation this
        obsolete attempt's turn, after which the turn the real dispatch reached can never
        bind and its receipts are refused.
        """
        revision, _turn_id = self.lost_settle_write()
        # Another worker settles this delivery and dispatches a later attempt while our
        # reconciliation is still reading. The guarded UPDATE will now match nothing.
        self.store.db.execute(
            "UPDATE deliveries SET attempt_count = attempt_count + 1 WHERE event_id = ?",
            (revision,),
        )

        with mock.patch.object(self.reconciler, "_is_current", return_value=True):
            self.reconciler.recover_on_start(self.adapter)

        self.assertEqual(
            self.generation_two()["anchorState"], ANCHOR_PENDING,
            "a stale attempt bound the generation to a turn the real dispatch never used",
        )

    def test_a_revision_request_is_not_exempt_from_supersession(self):
        """The execution-only exemption must not cover the relay's own ask.

        A revision_request is not reviewable either, but it IS answered the moment the child
        emits the receipt it asked for. Exempting it left the request reported as current -
        awaiting_child_receipt - after that receipt had already arrived.
        """
        revision = self.revision_pending()
        self.clock.advance(3600)
        self.delivery.attempt(revision, self.adapter, now=self.clock.now())
        self.ack.bind_pending_anchors()
        accepted = self.child_receipt_for_generation_two()
        self.assertEqual(accepted["executionGeneration"], 2)

        with self.store.transaction() as db:
            reason = self.delivery._supersession_reason(db, revision)

        self.assertEqual(
            reason, "superseded_revision",
            "the relay's own revision request outlived the reply it asked for",
        )

    def test_a_revision_request_answered_by_a_failure_is_still_retired(self):
        """head_revision considers only ready_for_review receipts.

        Routing a relay-owned request through it left one answered by a failed, interrupted
        or blocked reply reported as awaiting_child_receipt forever - the child had supplied
        exactly the completion receipt that was asked for, and the ask stayed current.
        """
        revision = self.revision_pending()
        self.clock.advance(3600)
        self.delivery.attempt(revision, self.adapter, now=self.clock.now())
        self.ack.bind_pending_anchors()
        relationship = self.registry.get(self._rid)
        turn = self.assigned_turn(
            thread=CHILD, turn=self.generation_two()["dispatchTurnId"],
        )
        payload = self.execution_payload(relationship, "failed", generation=2, turn=turn)
        self.accept(payload)

        with self.store.transaction() as db:
            reason = self.delivery._supersession_reason(db, revision)

        self.assertEqual(
            reason, "superseded_revision",
            "the request outlived the completion receipt it asked for",
        )

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

    def test_both_binding_passes_are_counted(self):
        """Assigning let the second pass erase what the first repaired.

        A tick that bound a durable anchor then reported anchorsBound 0 and even quiet, which
        is the opposite of what happened.
        """
        from codex_session_relay.daemon import TickReport

        report = TickReport()
        calls = {"n": 0}

        def two_then_none():
            calls["n"] += 1
            return ["a", "b"] if calls["n"] == 1 else []

        self.daemon.ack.bind_pending_anchors = two_then_none
        self.daemon._bind_anchors(report)
        self.daemon._bind_anchors(report)

        self.assertEqual(report.anchorsBound, 2, "the second pass must not erase the first")
