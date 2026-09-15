"""Carrying the authorized execution settings, and refusing to send without them.

The boundary: a thread/resume with no overrides returned sandbox dangerFullAccess for a task
created workspaceWrite with networkAccess false. These cover the delivery-level half - settings
established before any transport call. The no-widened-start guarantee itself is proved against
the real adapter in tests/test_bridge_adapter.py::GuardedSettingsSeam, because FakeHostAdapter
implements delivery itself and could pass while the real adapter started an unguarded turn.
"""

from codex_session_relay import identity
from codex_session_relay.errors import RefusalReason
from codex_session_relay.registry import load_settings, record_settings
from codex_session_relay.settings import TaskSettings
from codex_session_relay.transport import (
    ACKNOWLEDGED,
    DISPATCHED,
    HELD_UNCERTAIN,
    INBOX_ONLY,
    WITHHELD_PRE_SEND,
    classify_operation_receipt,
)

from .support import PARENT, DeliveryTestCase, task_settings


class SettingsEstablishedBeforeAnySend(DeliveryTestCase):
    def test_an_unrecorded_recipient_withholds_before_any_transport_call(self):
        _relationship, event_id = self.queued_event(settings=None)
        self.assertIsNone(self.attempt(event_id))
        self.assertEqual(self.adapter.sends, [], "nothing may reach the host")
        self.assertEqual(self.attempts_for(event_id), [], "no attempt was claimed")
        self.assertEqual(self.delivery_row(event_id)["state"], WITHHELD_PRE_SEND)

    def test_the_withholding_names_what_is_missing(self):
        incomplete = task_settings("/parent")
        del incomplete["environments"]
        _relationship, event_id = self.queued_event(settings=incomplete)
        self.assertIsNone(self.attempt(event_id))
        self.assertEqual(self.adapter.sends, [])
        entry = self.store.all(
            "SELECT detail FROM journal WHERE kind = ? ORDER BY rowid DESC LIMIT 1",
            ("delivery_withheld",),
        )[0]
        self.assertIn("environments", entry["detail"])
        self.assertIn(RefusalReason.SETTINGS_INCOMPLETE.value, entry["detail"])

    def test_an_empty_environment_selection_is_a_decision_not_an_absence(self):
        """[] means no environments selected. None means we do not know. Only one is usable."""
        chosen = task_settings("/parent", environments=[])
        self.assertEqual(TaskSettings(chosen).missing(), [])
        unknown = task_settings("/parent")
        unknown["environments"] = None
        self.assertEqual(TaskSettings(unknown).missing(), ["environments"])

    def test_a_sandbox_type_with_no_resume_mode_is_refused_not_approximated(self):
        external = task_settings("/parent", sandbox={"type": "externalSandbox"})
        _relationship, event_id = self.queued_event(settings=external)
        self.assertIsNone(self.attempt(event_id))
        self.assertEqual(self.adapter.sends, [])
        entry = self.store.all(
            "SELECT detail FROM journal WHERE kind = ? ORDER BY rowid DESC LIMIT 1",
            ("delivery_withheld",),
        )[0]
        self.assertIn(RefusalReason.UNSUPPORTED_SANDBOX_TYPE.value, entry["detail"])

    def test_the_ordinary_path_hands_the_settings_to_the_adapter(self):
        _relationship, event_id = self.queued_event()
        record = self.attempt(event_id)
        self.assertEqual(record["deliveryState"], DISPATCHED)
        seen = dict(self.adapter.settings_seen)
        self.assertIn(record["requestId"], seen)
        settings = seen[record["requestId"]]
        self.assertIsNotNone(settings, "the adapter was handed no settings")
        self.assertEqual(settings.data["approvalPolicy"], "never")
        self.assertEqual(settings.data["sandbox"]["type"], "workspaceWrite")

    def test_recording_settings_later_makes_the_delivery_eligible_again(self):
        """A missing record is a temporary condition, not a permanent hold."""
        _relationship, event_id = self.queued_event(settings=None)
        self.assertIsNone(self.attempt(event_id))
        record_settings(self.store, self.clock, PARENT, task_settings("/parent"),
                        source="creation_result")
        later = self.clock.now() + 10_000
        self.assertIsNotNone(self.attempt(event_id, now=later))


class RefusalClassification(DeliveryTestCase):
    """Each settings refusal is a completed pre-send refusal, not an uncertain outcome."""

    CODES = ("settings_not_preserved", "environments_unknown",
             "unverifiable_permission_profile")

    def _receipt(self, code):
        return {
            "requestId": "del-000000000000-a1", "status": "failed",
            "resumed": {"approvalPolicy": "never"},
            "error": f"thread/resume: {code}",
            "rpcError": {"code": code, "message": code},
        }

    def test_every_settings_refusal_is_a_pre_send_refusal(self):
        for code in self.CODES:
            with self.subTest(code=code):
                facts = classify_operation_receipt(self._receipt(code))
                self.assertEqual(facts.delivery_state, WITHHELD_PRE_SEND)
                self.assertEqual(facts.send_attempted, "no")
                self.assertTrue(facts.retry_safe)
                self.assertEqual(facts.failed_operation, "thread/resume")
                self.assertEqual(facts.rpc_error_code, code)

    def test_an_unrecognised_refusal_code_stays_uncertain(self):
        """An unknown refusal is not a known non-delivery."""
        facts = classify_operation_receipt(self._receipt("something_new"))
        self.assertEqual(facts.delivery_state, HELD_UNCERTAIN)
        self.assertFalse(facts.retry_safe)

    def test_approval_policy_is_decided_before_the_generic_mismatch(self):
        """A permanently closed push channel must not become a retry loop."""
        facts = classify_operation_receipt({
            "requestId": "del-000000000000-a2", "status": "failed",
            "resumed": {"approvalPolicy": "on-request"},
            "error": "thread/resume: Interactive approvals unsupported; message withheld.",
            "rpcError": {"code": "unsupported_approval_policy", "message": "unsupported"},
        })
        self.assertEqual(facts.delivery_state, INBOX_ONLY)
        self.assertFalse(facts.retry_safe)
        settings = TaskSettings(task_settings("/parent"))
        findings = settings.mismatches({
            "approvalPolicy": "on-request",
            "sandbox": {"type": "dangerFullAccess"},
            "thread": {"environments": None},
        })
        self.assertEqual(len(findings), 1, "approval decides alone; nothing shadows it")
        self.assertEqual(findings[0]["code"], "unsupported_approval_policy")


class ViolationAnnotatesItDoesNotReclassify(DeliveryTestCase):
    """A delivered turn stays delivered. The frozen enum has no delivered-but-suspect state,

    and inventing one would cost the dispatch its reconciliation and its acknowledgement, which
    are exactly what a suspect delivery needs.
    """

    def _dispatched_with_violation(self):
        _relationship, event_id = self.queued_event()
        record = self.attempt(event_id)
        findings = [{"code": "settings_not_preserved", "field": "sandbox",
                     "expected": {"type": "workspaceWrite"},
                     "returned": {"type": "dangerFullAccess"}}]
        self.delivery.record_settings_violation(record["requestId"], event_id, findings)
        return event_id, record

    def test_the_state_stays_dispatched_and_the_turn_id_is_kept(self):
        event_id, record = self._dispatched_with_violation()
        self.assertEqual(record["deliveryState"], DISPATCHED)
        self.assertFalse(record["retrySafe"])
        self.assertTrue(record["turnId"])
        self.assertEqual(self.delivery_row(event_id)["state"], DISPATCHED)

    def test_the_violation_is_readable_afterwards(self):
        _event_id, record = self._dispatched_with_violation()
        stored = self.delivery.settings_violation(record["requestId"])
        self.assertEqual(stored["findings"][0]["code"], "settings_not_preserved")
        self.assertIsNone(self.delivery.settings_violation("del-never-seen-a1"))

    def test_the_violation_never_enters_the_frozen_record(self):
        _event_id, record = self._dispatched_with_violation()
        self.assertNotIn("settingsFindings", record)
        self.assertNotIn("violation", record)

    def test_an_annotated_dispatch_can_still_be_acknowledged(self):
        event_id, _record = self._dispatched_with_violation()
        self.clock.advance(5)
        turn = self.adapter.start_turn(PARENT, turn_id="parent-ack-turn", status="inProgress")
        proof = identity.ack_proof(event_id, turn.turn_id)
        result = self.ack.acknowledge(
            event_id, ack_turn_id=turn.turn_id, ack_proof=proof, accepted=True,
            adapter=self.adapter,
        )
        self.assertTrue(result["accepted"])
        self.assertEqual(self.delivery_row(event_id)["state"], ACKNOWLEDGED)

    def test_an_annotated_dispatch_is_not_retried(self):
        event_id, _record = self._dispatched_with_violation()
        eligible = self.delivery.eligible(now=self.clock.now() + 10_000)
        self.assertNotIn(event_id, [row["event_id"] for row in eligible])


class SettingsRegistration(DeliveryTestCase):
    def test_a_recorded_record_round_trips(self):
        self.register()
        loaded = load_settings(self.store, PARENT)
        self.assertEqual(loaded.data["cwd"], "/parent")
        self.assertEqual(loaded.missing(), [])

    def test_recording_an_incomplete_record_is_refused_at_registration(self):
        broken = task_settings("/parent")
        del broken["model"]
        self.assertRefused(
            RefusalReason.SETTINGS_INCOMPLETE,
            lambda: record_settings(self.store, self.clock, "01other", broken, source="test"),
        )
        self.assertIsNone(load_settings(self.store, "01other"))

    def test_re_recording_replaces_rather_than_duplicates(self):
        record_settings(self.store, self.clock, "01other", task_settings("/a"), source="test")
        record_settings(self.store, self.clock, "01other", task_settings("/b"), source="test")
        self.assertEqual(load_settings(self.store, "01other").data["cwd"], "/b")
