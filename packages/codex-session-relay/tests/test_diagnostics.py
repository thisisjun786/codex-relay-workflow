"""One word for five situations told an operator nothing about what to do.

withheld_pre_send covered a receipt not yet collected, a parent mid-turn, a host that would
not confirm the authorized settings, a started turn, and an outstanding acknowledgement. The
cause is the only part that suggests an action, and it was the part not recorded.
"""

from codex_session_relay.models import TurnRef

from .support import CHILD, PARENT, DeliveryTestCase
from .test_daemon import DaemonTestCase


class Phases(DeliveryTestCase):
    def phase_of(self, event_id):
        for item in self.delivery.snapshot()["deliveries"]:
            if item["eventId"] == event_id:
                return item
        raise AssertionError("no such delivery")

    def test_a_queued_delivery_is_awaiting_its_receipt(self):
        _relationship, event_id = self.queued_event()
        self.assertEqual(self.phase_of(event_id)["phase"], "awaiting_receipt")

    def test_a_busy_parent_is_named_as_such_with_its_next_retry(self):
        _relationship, event_id = self.queued_event()
        self.adapter.set_status(PARENT, "active")
        self.attempt(event_id)
        item = self.phase_of(event_id)
        self.assertEqual(item["phase"], "parent_busy")
        self.assertIsNotNone(item["nextRetryAt"], "an operator needs to know when, too")
        self.assertEqual(item["lastFailedOperation"]["operation"], "parent_busy")

    def test_a_dispatched_delivery_is_awaiting_acknowledgement(self):
        _relationship, event_id = self.queued_event()
        self.attempt(event_id)
        self.assertEqual(self.phase_of(event_id)["phase"], "awaiting_ack")

    def test_a_closed_channel_is_queryable_rather_than_hidden(self):
        _relationship, event_id = self.queued_event()
        self.adapter.script("approval_policy")
        self.attempt(event_id)
        item = self.phase_of(event_id)
        self.assertEqual(item["phase"], "channel_closed")
        self.assertIsNotNone(item["lastFailedOperation"])
        self.assertEqual(item["lastFailedOperation"]["error_code"],
                         "unsupported_approval_policy")

    def test_a_settings_rejection_records_the_field_the_host_disagreed_on(self):
        """The difference is dropped by classification, so it is read from the raw receipt."""
        _relationship, event_id = self.queued_event()
        original = self.adapter.send_message

        def rejecting(request_id, thread_id, message, settings=None):
            receipt = original(request_id, thread_id, message, settings)
            receipt.update(
                status="failed",
                error="thread/resume: settings_not_preserved",
                rpcError={"code": "settings_not_preserved", "message": "mismatch"},
                resumed={"approvalPolicy": "onRequest"},
                settingsFindings=[
                    {"field": "approvalPolicy", "expected": "never", "returned": "onRequest"},
                    {"field": "reasoningEffort", "expected": "xhigh", "returned": "low"},
                ],
            )
            self.adapter.ledger[request_id] = receipt
            return dict(receipt)

        self.adapter.send_message = rejecting
        self.attempt(event_id)

        failure = self.phase_of(event_id)["lastFailedOperation"]
        self.assertEqual(failure["operation"], "settings_check")
        self.assertIn("approvalPolicy", failure["difference"])
        self.assertIn("onRequest", failure["difference"])
        self.assertIn("reasoningEffort", failure["difference"],
                      "every mismatched field, not only the first")

    def test_the_diagnosis_survives_reopening_the_database(self):
        from codex_session_relay.delivery import DeliveryService
        from codex_session_relay.store import Store

        _relationship, event_id = self.queued_event()
        self.adapter.set_status(PARENT, "active")
        self.attempt(event_id)
        path = self.store.path
        self.store.close()
        reopened = Store(path)
        self.addCleanup(reopened.close)
        service = DeliveryService(reopened, self.registry, self.intake, self.clock)
        self.assertTrue(service.failures_for(event_id))


class ObservationHealth(DaemonTestCase):
    def test_a_live_loop_with_nothing_polled_is_not_healthy(self):
        """A live pid was the whole problem in the reproduced incident."""
        relationship = self.register()
        self.adapter.start_turn(CHILD, turn_id="turn-dispatch-1", status="inProgress")
        self.adapter.fail_reads("read_turn")
        self.daemon.tick(now=self.clock.now())

        health = self.delivery.observation_health(now=self.clock.now())
        anchor = health["anchors"][relationship["relationshipId"]]
        self.assertIsNone(anchor["lastPolledAt"], "a failed first read is not a poll")
        self.assertIsNotNone(anchor["lastError"])
        self.assertEqual(health["health"], "stalled")

    def test_a_successful_poll_then_a_failure_keeps_the_last_success(self):
        relationship = self.register()
        self.adapter.start_turn(CHILD, turn_id="turn-dispatch-1", status="inProgress")
        self.daemon.tick(now=self.clock.now())
        first = self.delivery.observation_health(now=self.clock.now())
        polled = first["anchors"][relationship["relationshipId"]]["lastPolledAt"]
        self.assertIsNotNone(polled, "an in-progress turn was still successfully looked at")

        self.adapter.fail_reads("read_turn")
        self.clock.advance(10)
        self.daemon.tick(now=self.clock.now())
        second = self.delivery.observation_health(now=self.clock.now())
        anchor = second["anchors"][relationship["relationshipId"]]
        self.assertEqual(anchor["lastPolledAt"], polled, "a failure is not a success")
        self.assertIsNotNone(anchor["lastError"])

    def test_a_staged_backlog_is_visible_with_its_age(self):
        relationship = self.register()
        self.adapter.start_turn(CHILD, turn_id="turn-dispatch-1", status="inProgress")
        path = self.artifact("out.txt", "still going")
        payload = self.ready_payload(
            relationship, [path], turn=TurnRef(CHILD, "turn-dispatch-1", "inProgress"),
        )
        self.accept(payload)
        self.clock.advance(7200)

        health = self.delivery.observation_health(now=self.clock.now())

        self.assertEqual(len(health["stagedEvents"]), 1)
        self.assertGreater(health["oldestStagedAgeSeconds"], 3600)
        self.assertEqual(health["backlog"][relationship["relationshipId"]], 1)
        self.assertIn("liveness", health["note"])

    def test_an_anchor_the_scheduler_has_not_reached_is_not_reported_healthy(self):
        """Starting from the poll table would omit it entirely and report healthy."""
        relationship = self.register()
        self.adapter.start_turn(CHILD, turn_id="turn-dispatch-1", status="inProgress")
        # No tick at all: nothing has ever looked at this anchor.
        health = self.delivery.observation_health(now=self.clock.now())
        anchor = health["anchors"][relationship["relationshipId"]]
        self.assertIsNone(anchor["lastPolledAt"])
        self.assertEqual(
            health["health"], "stalled",
            "an anchor nothing has read is not evidence of health",
        )
