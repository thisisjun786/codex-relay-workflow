"""A settings hold names its reason, who recovers it and how, on every surface (CRW-235).

CRW-124 G2: while a parent that another task's bridge message loaded stayed loaded, every relay
delivery to it was withheld as settings_not_preserved. status carried the reason
(lastFailedOperation settings_check) but no actor and no path, assignment-show named the daemon,
which could not deliver it, the fault sweep named only the attempt state, and after the attempt cap
nothing sent it again. The narrowing itself is delivered now (test_bridge_load_roots.py). These
cover every hold that remains: whatever holds a delivery on its recipient's settings, status,
assignment-show and the fault sweep name one reason, one actor and one supported command, from one
reader (delivery.current_settings_hold) that reads each cause where its transition recorded it.

The clock does not move unless a case says so, which is the fixed clock under which a reader that
ordered causes by their timestamps could not tell two of them apart.

Each test is labelled RED where it fails on the relay before this change, or GREEN where it pins
behaviour that must not change.
"""

import json
import os
import shlex
import unittest

from codex_session_relay import faultsweep
from codex_session_relay.assignment import (
    OPERATOR_RESTORES_SETTINGS_ACTION,
    PARENT_RECOVERS_SETTINGS_HOLD_ACTION,
    NEEDS_CHANGES,
    RECEIVED,
    completion_next_action,
    correction_next_action,
)
from codex_session_relay.delivery import current_settings_hold
from codex_session_relay.errors import DeliveryRefused, RefusalReason
from codex_session_relay.transport import DISPATCHED, INBOX_ONLY, WITHHELD_PRE_SEND

from .support import CHILD, PARENT
from .test_host_lost_turn import HostLossCase

NOT_PRESERVED = "settings_not_preserved"
UNAVAILABLE = RefusalReason.SETTINGS_UNAVAILABLE.value


class SettingsHoldCase(HostLossCase):
    def recovery(self):
        return self.assignments.state(self._rid).get("recovery") or {}

    def store_dir(self):
        return os.path.dirname(os.path.abspath(str(self.store.path)))

    def assert_settings_show(self, command):
        self.assertEqual(shlex.split(command or "")[-5:],
                         ["--state", self.store_dir(), "settings-show", "--task", PARENT])

    def assert_show_event(self, command, event_id):
        self.assertEqual(shlex.split(command or "")[-5:],
                         ["--state", self.store_dir(), "show", "--event", event_id])

    def refused(self, outcome=NOT_PRESERVED):
        self.parent_history()
        _relationship, event_id = self.queued_event()
        self.adapter.script(outcome)
        self.attempt(event_id)
        return event_id

    def again(self, event_id, outcome):
        row = self.delivery_row(event_id)
        self.adapter.script(outcome)
        return self.attempt(event_id, now=row["next_eligible_at"])

    def observations(self, source, event_id):
        page = source(self.store, product="crw", scope=None)
        return [one for one in page["observations"]
                if any(item["observed"].get("event") == event_id for item in one["evidence"])]

    @staticmethod
    def evidence(observation, kind):
        return [item["observed"] for item in observation["evidence"] if item["kind"] == kind]

    def strip_this_revisions_records(self, event_id):
        """What a store written before this revision holds: no key, no marker."""
        self.store.db.execute(
            "UPDATE journal SET detail = json_remove(detail, '$.settingsRefusal')"
            " WHERE subject = ? AND kind = 'delivery_attempted'", (event_id,))
        self.store.db.execute(
            "DELETE FROM journal WHERE subject = ? AND kind = 'delivery_presend_withheld'",
            (event_id,))
        self.store.db.commit()


class AWithheldSettingsHold(SettingsHoldCase):
    def test_status_names_the_reason_the_operator_and_settings_show(self):
        """RED: status named the reason and phase only."""
        event_id = self.refused()
        item = self.status_of(event_id)
        self.assertEqual(item["phase"], "settings_rejected")
        hold = item["settingsHold"]
        self.assertEqual((hold["kind"], hold["source"], hold["reason"], hold["field"]),
                         ("withheld", "attempt", NOT_PRESERVED, "runtimeWorkspaceRoots"))
        recovery = item["recovery"]
        self.assertEqual((recovery["actor"], recovery["reason"]), ("operator", NOT_PRESERVED))
        self.assert_settings_show(recovery["command"])
        self.assertIn("settings-record --source user_transition", recovery["then"])

    def test_assignment_show_names_the_operator_not_the_daemon(self):
        """RED: nextExpectedAction was daemon_delivers, which cannot fix a settings difference."""
        self.refused()
        self.assertEqual(self.next_action(), OPERATOR_RESTORES_SETTINGS_ACTION)
        self.assertEqual(self.completion_delivery()["settingsHold"]["reason"], NOT_PRESERVED)
        recovery = self.recovery()
        self.assertEqual((recovery.get("actor"), recovery.get("reason")),
                         ("operator", NOT_PRESERVED))
        self.assert_settings_show(recovery.get("command"))

    def test_the_fault_sweep_names_the_reason_and_the_recovery(self):
        """RED: the occurrence named only the attempt state."""
        event_id = self.refused()
        [observation] = self.observations(faultsweep.retry_faults, event_id)
        self.assertTrue(observation["detail"].endswith(f"settings {NOT_PRESERVED}"),
                        observation["detail"])
        [settings] = self.evidence(observation, "settings")
        self.assertEqual((settings["reason"], settings["current"]), (NOT_PRESERVED, True))
        [recovery] = self.evidence(observation, "recovery")
        self.assertEqual(recovery["actor"], "operator")
        self.assert_settings_show(recovery["command"])

    def test_an_answer_the_daemon_can_still_clear_stays_the_daemons(self):
        """RED for the recovery; GREEN for the next action, which stays the daemon's."""
        event_id = self.refused("setting_unobservable")
        self.assertEqual(self.next_action(), "daemon_delivers")
        recovery = self.status_of(event_id)["recovery"]
        self.assertEqual((recovery["actor"], recovery["reason"]), ("daemon", "setting_unobservable"))
        # assignment-show names the same recovery beside the daemon's action (Devin on 566eecb5).
        shown = self.recovery()
        self.assertEqual((shown.get("actor"), shown.get("reason"), shown.get("command")),
                         (recovery["actor"], recovery["reason"], recovery["command"]))


class ACappedSettingsHold(SettingsHoldCase):
    def test_after_the_cap_the_parent_reads_the_report(self):
        """RED: a capped settings hold named nobody, and assignment-show said daemon_delivers."""
        event_id = self.refused()
        for _ in range(self.delivery.policy.max_attempts - 1):
            self.again(event_id, NOT_PRESERVED)
        row = self.delivery_row(event_id)
        self.assertEqual((row["state"], row["hold_reason"]), (WITHHELD_PRE_SEND, "attempt_cap"))
        item = self.status_of(event_id)
        self.assertEqual(item["settingsHold"]["kind"], "capped")
        self.assertEqual(item["recovery"]["actor"], "parent")
        self.assert_show_event(item["recovery"]["command"], event_id)
        self.assertIn("settings-record", item["recovery"]["laterDeliveries"])
        self.assertEqual(self.next_action(), PARENT_RECOVERS_SETTINGS_HOLD_ACTION)
        recovery = self.recovery()
        self.assertEqual((recovery.get("actor"), recovery.get("reason")), ("parent", NOT_PRESERVED),
                         "named for the settings code, not the cap")
        self.assert_show_event(recovery.get("command"), event_id)
        self.assertTrue(recovery.get("laterDeliveries"))
        [observation] = self.observations(faultsweep.delivery_faults, event_id)
        self.assertIn(f"after settings {NOT_PRESERVED}", observation["detail"])
        [fault_recovery] = self.evidence(observation, "recovery")
        self.assertEqual(fault_recovery["actor"], "parent")



class ACorrectionHeldOnTheChildsSettings(SettingsHoldCase):
    """A revision request to the child held on the child's settings is named on assignment-show
    as a completion's hold is (Devin on aa9724f4)."""

    def test_a_withheld_correction_is_the_operators_on_the_childs_settings(self):
        """RED: assignment-show said daemon_delivers_correction, which cannot fix a settings
        difference, while status named the operator."""
        _completion, correction = self.correction_after_needs_changes()
        self.adapter.script(NOT_PRESERVED)
        self.attempt(correction)
        self.assertEqual(self.delivery_row(correction)["state"], WITHHELD_PRE_SEND)
        self.assertEqual(self.next_action(), OPERATOR_RESTORES_SETTINGS_ACTION)
        recovery = self.recovery()
        self.assertEqual((recovery.get("actor"), recovery.get("reason")),
                         ("operator", NOT_PRESERVED))
        child_settings = ["--state", self.store_dir(), "settings-show", "--task", CHILD]
        self.assertEqual(shlex.split(recovery.get("command") or "")[-5:], child_settings)
        item = self.status_of(correction)
        self.assertEqual((item["recovery"]["actor"], item["recovery"]["reason"]),
                         ("operator", NOT_PRESERVED))
        self.assertEqual(shlex.split(item["recovery"]["command"])[-5:], child_settings)

    def test_a_capped_correction_names_its_settings_code(self):
        """RED: at the cap the parent's recovery named only attempt_cap."""
        _completion, correction = self.correction_after_needs_changes()
        self.adapter.script(NOT_PRESERVED)
        self.attempt(correction)
        for _ in range(self.delivery.policy.max_attempts - 1):
            self.again(correction, NOT_PRESERVED)
        row = self.delivery_row(correction)
        self.assertEqual((row["state"], row["hold_reason"]), (WITHHELD_PRE_SEND, "attempt_cap"))
        self.assertEqual(self.next_action(), "parent_recovers_held_correction")
        recovery = self.recovery()
        self.assertEqual((recovery.get("actor"), recovery.get("reason")), ("parent", NOT_PRESERVED),
                         "named for the settings code, not the cap")
        self.assert_show_event(recovery.get("command"), correction)
        self.assertTrue(recovery.get("laterDeliveries"))
        self.assertEqual(self.status_of(correction)["recovery"]["reason"], NOT_PRESERVED)


    def test_a_correction_a_closed_channel_stored_is_named_for_its_settings_code(self):
        """RED: assignment-show named only push_channel_closed, and status told the parent to
        acknowledge a revision request, which takes no acknowledgement (Devin and the review
        of 566eecb5)."""
        from codex_session_relay.errors import AckRefused

        _completion, correction = self.correction_after_needs_changes()
        self.adapter.script("approval_policy")
        self.attempt(correction)
        self.assertEqual(self.delivery_row(correction)["state"], INBOX_ONLY)
        self.assertEqual(self.next_action(), "parent_recovers_held_correction")
        shown = self.recovery()
        status = self.status_of(correction)["recovery"]
        for recovery in (shown, status):
            self.assertEqual((recovery.get("actor"), recovery.get("reason")),
                             ("parent", "unsupported_approval_policy"))
            self.assertIn("takes no acknowledgement", recovery.get("then") or "")
            self.assertIn("generation-open", recovery.get("then") or "")
            self.assert_show_event(recovery.get("command"), correction)
            self.assertIn("never or on-request", recovery.get("laterDeliveries"))
        with self.assertRaises(AckRefused):
            self.ack.acknowledge(correction, ack_turn_id="t", ack_proof="p", accepted=True)


class AClosedChannel(SettingsHoldCase):
    def test_a_closed_channel_names_the_parent_and_files_no_fault(self):
        """RED for the recovery; GREEN for the rest: stored where the parent reads it, no fault."""
        event_id = self.refused("approval_policy")
        self.assertEqual(self.delivery_row(event_id)["state"], INBOX_ONLY)
        item = self.status_of(event_id)
        self.assertEqual(item["phase"], "channel_closed")
        self.assertEqual((item["settingsHold"]["kind"], item["settingsHold"]["reason"]),
                         ("channel_closed", "unsupported_approval_policy"))
        self.assertEqual(item["recovery"]["actor"], "parent")
        self.assert_show_event(item["recovery"]["command"], event_id)
        self.assertIn("never or on-request", item["recovery"]["laterDeliveries"])
        self.assertEqual(self.next_action(), "parent_acknowledges")
        self.assertEqual(self.recovery().get("actor"), "parent")
        for source in (faultsweep.retry_faults, faultsweep.delivery_faults):
            self.assertEqual(self.observations(source, event_id), [])


class APreSendRefusal(SettingsHoldCase):
    def test_a_record_refusal_before_any_attempt_is_named(self):
        """RED: the refusal was named by code only, with no actor or path."""
        self.parent_history()
        _relationship, event_id = self.queued_event(settings=None)
        self.assertIsNone(self.attempt(event_id))
        item = self.status_of(event_id)
        self.assertEqual(item["phase"], "withheld:settings_check")
        hold = item["settingsHold"]
        self.assertEqual((hold["source"], hold["reason"]),
                         ("pre_send", RefusalReason.SETTINGS_UNAVAILABLE.value))
        self.assertEqual(item["recovery"]["actor"], "operator")
        self.assert_settings_show(item["recovery"]["command"])
        # The record itself was refused: the step is the record, not the thread (Devin on
        # 1f0f5a89 found the same gap for the role gate).
        self.assertIn("record it again from the creation result", item["recovery"]["then"])
        self.assertNotIn("bring the recipient back", item["recovery"]["then"])
        self.assertIn("no authorized settings recorded", item["recovery"]["refusalDetail"])
        self.assertEqual(self.next_action(), OPERATOR_RESTORES_SETTINGS_ACTION)
        [observation] = self.observations(faultsweep.refusal_faults, event_id)
        [recovery] = self.evidence(observation, "recovery")
        self.assertEqual(recovery["actor"], "operator")

    def test_a_role_gate_refusal_carries_the_repair_its_refusal_names(self):
        """RED: the role gate's refusals were told to restore or re-record the recipient's
        settings (Devin on 1f0f5a89), then one fixed repair per code, which each code's other
        cause cannot use (the review of fa2bacf7), then pointed at lastFailedOperation, which a
        timestamp can give to an older lifecycle refusal (the review of fd2ee727). The recovery
        now names every repair the gate can give and carries the refusal's own text."""
        for reason in (RefusalReason.ROLE_POLICY_UNCONFIGURED, RefusalReason.ROLE_BINDING_MISMATCH,
                       RefusalReason.SETTINGS_RECORD_STALE_FOR_ROLE):
            with self.subTest(reason=reason.value):
                self.parent_history()
                _relationship, event_id = self.queued_event()
                # An older lifecycle refusal under the same timestamp as the gate's.
                self.adapter.threads[PARENT].archived = True
                self.attempt(event_id)
                self.adapter.threads[PARENT].archived = False
                gate = f"the gate's own repair for {reason.value}"
                row = self.delivery_row(event_id)
                self.delivery._withhold_settings(
                    event_id, self.clock.now(), DeliveryRefused(reason, gate),
                    attempts=row["attempt_count"], row=row)
                item = self.status_of(event_id)
                self.assertEqual((item["settingsHold"]["source"], item["settingsHold"]["reason"]),
                                 ("pre_send", reason.value))
                recovery = item["recovery"]
                self.assertEqual((recovery["actor"], recovery["refusalDetail"]), ("operator", gate))
                self.assertIn("refusalDetail", recovery["then"])
                for repair in ("declare the role", "restart", "fix the binding or the creation",
                               "re-record from a user-attributed source"):
                    self.assertIn(repair, recovery["then"].lower())
                self.assertNotIn("do not re-record", recovery["then"])
                self.assertNotIn("bring the recipient back", recovery["then"])
                shown = self.recovery()
                self.assertEqual((shown.get("then"), shown.get("refusalDetail")),
                                 (recovery["then"], gate))
                self.adapter.threads[PARENT].archived = False
                self.clock.advance(1)

    def test_the_refusal_text_is_the_chosen_withholds_own(self):
        """RED: refusalDetail was read from the settings_check failure row, which the attempt path
        also writes before it settles; a relay stopped in that window left the pre-send hold
        naming the in-flight attempt's text (the review of 77c0c641)."""
        _relationship, event_id = self.queued_event()
        row = self.delivery_row(event_id)
        gate = "the gate's own repair for role_binding_mismatch"
        self.delivery._withhold_settings(
            event_id, self.clock.now(),
            DeliveryRefused(RefusalReason.ROLE_BINDING_MISMATCH, gate),
            attempts=row["attempt_count"], row=row)
        self.clock.advance(1)
        # What a send writes before its settlement, left there by a relay that stopped.
        self.delivery.record_failure(
            event_id, "settings_check", detail="thread/resume: settings_not_preserved",
            relationship_id=row["relationship_id"], error_code=NOT_PRESERVED, retry_safe=True)
        item = self.status_of(event_id)
        self.assertEqual((item["settingsHold"]["source"], item["settingsHold"]["reason"]),
                         ("pre_send", RefusalReason.ROLE_BINDING_MISMATCH.value))
        self.assertEqual(item["recovery"]["refusalDetail"], gate)
        self.assertEqual(self.recovery().get("refusalDetail"), gate)

    def test_a_withhold_that_did_not_take_effect_earns_no_recovery(self):
        """GREEN: its refusal is still counted, and nothing names a hold the row is not in."""
        _relationship, event_id = self.queued_event()
        self.delivery._withhold_settings(
            event_id, self.clock.now(),
            DeliveryRefused(RefusalReason.SETTINGS_UNAVAILABLE, "gone"), attempts=99,
            row=self.delivery_row(event_id))
        self.assertEqual(current_settings_hold(self.store, event_id)["hold"], None)
        [observation] = self.observations(faultsweep.refusal_faults, event_id)
        self.assertEqual(self.evidence(observation, "recovery"), [])


    def test_a_stale_refusal_row_beside_another_current_hold_earns_no_recovery(self):
        """GREEN for the refusal occurrence, RED-proof for its recovery: the row names
        settings_unavailable, the delivery's current hold is the attempt's settings_not_preserved,
        so the occurrence must not carry a recovery for a hold it is not."""
        event_id = self.refused()
        self.delivery._withhold_settings(
            event_id, self.clock.now(),
            DeliveryRefused(RefusalReason.SETTINGS_UNAVAILABLE, "gone"), attempts=99,
            row=self.delivery_row(event_id))
        self.assertEqual(current_settings_hold(self.store, event_id)["hold"]["reason"],
                         NOT_PRESERVED)
        [observation] = self.observations(faultsweep.refusal_faults, event_id)
        self.assertEqual(self.evidence(observation, "recovery"), [])


class TheCurrentCauseWhateverTheClock(SettingsHoldCase):
    def test_a_transport_failure_after_a_settings_refusal_ends_the_hold(self):
        """RED: under one timestamp the older settings failure could still name the phase."""
        event_id = self.refused()
        self.again(event_id, "resume_fail")
        item = self.status_of(event_id)
        self.assertIsNone(item["settingsHold"])
        self.assertEqual(item["phase"], "withheld:thread/resume")
        self.assertEqual(self.next_action(), "daemon_delivers")
        self.assertEqual(self.recovery(), {})
        self.again(event_id, NOT_PRESERVED)
        self.assertEqual(self.status_of(event_id)["settingsHold"]["reason"], NOT_PRESERVED)

    def test_an_attempts_fault_keeps_its_own_cause_after_a_later_presend_refusal(self):
        """RED: the older attempt's occurrence was named for the later pre-send refusal, whose
        own occurrence (refusal_faults) is where that refusal and its recovery belong."""
        event_id = self.refused()
        self.store.db.execute("DELETE FROM authorized_settings WHERE task_id = ?", (PARENT,))
        self.store.db.commit()
        self.assertIsNone(self.attempt(event_id, now=self.delivery_row(event_id)["next_eligible_at"]))
        hold = self.status_of(event_id)["settingsHold"]
        self.assertEqual((hold["source"], hold["reason"]), ("pre_send", UNAVAILABLE))
        [observation] = self.observations(faultsweep.retry_faults, event_id)
        self.assertTrue(observation["detail"].endswith(f"settings {NOT_PRESERVED}"),
                        observation["detail"])
        [settings] = self.evidence(observation, "settings")
        self.assertEqual((settings["reason"], settings["current"]), (NOT_PRESERVED, False))
        self.assertEqual(self.evidence(observation, "recovery"), [])
        [refusal] = self.observations(faultsweep.refusal_faults, event_id)
        [recovery] = self.evidence(refusal, "recovery")
        self.assertEqual((recovery["actor"], recovery["reason"]), ("operator", UNAVAILABLE))

    def test_a_lifecycle_withhold_after_a_settings_refusal_is_not_a_settings_hold(self):
        """RED: the lifecycle withhold was reported as the older settings rejection."""
        event_id = self.refused()
        self.adapter.threads[PARENT].archived = True
        self.attempt(event_id, now=self.delivery_row(event_id)["next_eligible_at"])
        item = self.status_of(event_id)
        self.assertIsNone(item["settingsHold"])
        self.assertEqual(item["phase"], "withheld:lifecycle_read")

    def test_a_settings_refusal_after_a_lifecycle_withhold_is_named(self):
        """RED: nothing named it."""
        self.parent_history()
        _relationship, event_id = self.queued_event(settings=None)
        self.adapter.threads[PARENT].archived = True
        self.attempt(event_id)
        self.adapter.threads[PARENT].archived = False
        self.attempt(event_id, now=self.delivery_row(event_id)["next_eligible_at"])
        item = self.status_of(event_id)
        self.assertEqual(item["phase"], "withheld:settings_check")
        self.assertEqual(item["settingsHold"]["source"], "pre_send")

    def test_a_pause_after_a_settings_refusal_is_not_a_settings_hold(self):
        """RED: the paused delivery still read as a settings rejection."""
        event_id = self.refused()
        self.registry.set_status(self._rid, "paused", actor="test")
        self.attempt(event_id)
        item = self.status_of(event_id)
        self.assertIsNone(item["settingsHold"])
        self.assertEqual(item["phase"], "withheld_pre_send")


class ReconciliationNamesTheCauseToo(SettingsHoldCase):
    def left_for_reconciliation(self, outcome):
        """An attempt the relay stopped before settling, with the transport's receipt kept."""
        self.parent_history()
        _relationship, event_id = self.queued_event()
        self.adapter.script(outcome)
        settle = self.delivery._settle

        def stopped(*args, **kwargs):
            raise RuntimeError("the relay stopped before settling the attempt")

        self.delivery._settle = stopped
        with self.assertRaises(RuntimeError):
            self.attempt(event_id)
        self.delivery._settle = settle
        [attempt] = self.attempts_for(event_id)
        self.assertEqual(attempt["internal_state"], "in_flight")
        return event_id, attempt

    def test_a_settings_refusal_settled_only_by_reconciliation_is_named(self):
        """RED: an attempt a crash left for reconciliation carried no cause at all."""
        event_id, attempt = self.left_for_reconciliation(NOT_PRESERVED)
        self.reconciler.reconcile_attempt(attempt["request_id"], self.adapter)
        self.assertEqual(self.delivery_row(event_id)["state"], WITHHELD_PRE_SEND)
        hold = self.status_of(event_id)["settingsHold"]
        self.assertEqual((hold["source"], hold["reason"], hold["requestId"]),
                         ("attempt", NOT_PRESERVED, attempt["request_id"]))
        # Its field too, from the receipt's findings as the sender reads them (Devin on fa2bacf7).
        self.assertEqual(hold["field"], "runtimeWorkspaceRoots")
        self.assertEqual(self.next_action(), OPERATOR_RESTORES_SETTINGS_ACTION)

    def test_a_code_that_is_not_text_names_no_cause_and_the_attempt_settles(self):
        """RED: the settings-cause lookup raised on a resume refusal whose code is an object, so
        reconciliation could not settle the attempt. A malformed host answer is no settings
        refusal; the attempt settles as the pre-send withhold it is and names no cause."""
        event_id, attempt = self.left_for_reconciliation("resume_fail")
        self.adapter.ledger[attempt["request_id"]]["rpcError"]["code"] = {"number": -32000}
        self.reconciler.reconcile_attempt(attempt["request_id"], self.adapter)
        [attempt] = self.attempts_for(event_id)
        self.assertEqual(attempt["internal_state"], "settled")
        self.assertEqual(self.delivery_row(event_id)["state"], WITHHELD_PRE_SEND)
        settled = self.store.one(
            "SELECT detail FROM journal WHERE subject = ? AND kind = 'reconciled'",
            (attempt["request_id"],))
        self.assertIsNone(json.loads(settled["detail"])["settingsRefusal"])
        self.assertIsNone(self.status_of(event_id)["settingsHold"])


    def test_a_narrowing_settled_only_by_reconciliation_is_journaled_once(self):
        """RED: reconciliation promoted the delivery without the note its receipt carried."""
        event_id, attempt = self.left_for_reconciliation("accepted_with_notes")
        self.reconciler.reconcile_attempt(attempt["request_id"], self.adapter)
        self.assertEqual(self.delivery_row(event_id)["state"], DISPATCHED)
        self.reconciler.reconcile_attempt(attempt["request_id"], self.adapter)
        rows = self.store.all(
            "SELECT detail FROM journal WHERE subject = ? AND kind = 'delivery_settings_noted'",
            (event_id,))
        self.assertEqual(len(rows), 1, "one row per request, however often it is settled")
        detail = json.loads(rows[0]["detail"])
        self.assertEqual(detail["requestId"], attempt["request_id"])
        self.assertEqual(detail["notes"][0]["code"], "runtime_roots_narrower_than_record")


class RowsRecordedBeforeThisRevision(SettingsHoldCase):
    def test_a_hold_whose_cause_was_never_written_down_is_undetermined(self):
        """RED: nothing named it; now it is named undetermined, never guessed."""
        event_id = self.refused()
        self.strip_this_revisions_records(event_id)
        item = self.status_of(event_id)
        self.assertEqual((item["settingsHold"]["source"], item["settingsHold"]["reason"]),
                         ("undetermined", None))
        recovery = item["recovery"]
        # The cause is unknown, the actor is not: an uncapped withhold is the daemon's to retry,
        # as assignment-show says (Devin on fd2ee727).
        self.assertEqual((recovery["actor"], recovery["reason"]), ("daemon", "undetermined"))
        self.assert_show_event(recovery["command"], event_id)
        self.assertIn("nothing here claims a settings fix", recovery["then"])
        self.assertEqual(self.next_action(), "daemon_delivers",
                         "an undetermined cause is not an operator settings fix")
        self.assertEqual((self.recovery().get("actor"), self.recovery().get("reason")),
                         ("daemon", "undetermined"))
        [observation] = self.observations(faultsweep.retry_faults, event_id)
        [fault_recovery] = self.evidence(observation, "recovery")
        self.assertEqual(fault_recovery["reason"], "undetermined")

    def test_a_closed_channel_whose_cause_was_never_written_down_is_undetermined(self):
        """RED: an inbox_only row from before this revision carries no settings_check failure
        row (the closed channel records its failure under thread/resume), so it was named no hold
        and no recovery at all, although only a settings refusal closes the channel."""
        event_id = self.refused("approval_policy")
        self.strip_this_revisions_records(event_id)
        item = self.status_of(event_id)
        self.assertEqual(item["phase"], "channel_closed")
        self.assertEqual((item["settingsHold"]["kind"], item["settingsHold"]["source"],
                          item["settingsHold"]["reason"]),
                         ("channel_closed", "undetermined", None))
        self.assertEqual((item["recovery"]["actor"], item["recovery"]["reason"]),
                         ("parent", "undetermined"))
        self.assert_show_event(item["recovery"]["command"], event_id)
        self.assertEqual(self.next_action(), "parent_acknowledges")
        recovery = self.recovery()
        self.assertEqual((recovery.get("actor"), recovery.get("reason")), ("parent", "undetermined"))
        self.assert_show_event(recovery.get("command"), event_id)

    def test_a_capped_hold_whose_cause_was_never_written_down_is_the_parents(self):
        """RED: named the operator, while nothing sends it again and assignment-show names the
        parent (Devin on fd2ee727)."""
        event_id = self.refused()
        for _ in range(self.delivery.policy.max_attempts - 1):
            self.again(event_id, NOT_PRESERVED)
        self.strip_this_revisions_records(event_id)
        item = self.status_of(event_id)
        self.assertEqual((item["settingsHold"]["kind"], item["settingsHold"]["source"]),
                         ("capped", "undetermined"))
        for recovery in (item["recovery"], self.recovery()):
            self.assertEqual((recovery.get("actor"), recovery.get("reason")),
                             ("parent", "undetermined"))
            self.assert_show_event(recovery.get("command"), event_id)
            self.assertIn("generation-open", recovery.get("then") or "")
            self.assertIn("nothing here claims a settings fix", recovery.get("then") or "")
        self.assertEqual(self.next_action(), PARENT_RECOVERS_SETTINGS_HOLD_ACTION)

    def test_a_strictly_later_pause_is_no_settings_hold(self):
        """RED: a pause writes no failure row, so a settings refusal followed by a pause, both
        recorded before this revision, still read as an undetermined settings hold (Devin on
        77c0c641)."""
        event_id = self.refused()
        self.clock.advance(5)
        self.registry.set_status(self._rid, "paused", actor="test")
        self.attempt(event_id, now=self.delivery_row(event_id)["next_eligible_at"])
        self.strip_this_revisions_records(event_id)
        item = self.status_of(event_id)
        self.assertIsNone(item["settingsHold"])
        self.assertIsNone(item["recovery"])

    def test_a_strictly_later_lifecycle_withhold_is_no_settings_hold(self):
        """GREEN: a later lifecycle withhold recorded before this revision names no settings hold."""
        event_id = self.refused()
        self.clock.advance(60)
        self.adapter.threads[PARENT].archived = True
        self.attempt(event_id, now=self.delivery_row(event_id)["next_eligible_at"])
        self.strip_this_revisions_records(event_id)
        self.assertIsNone(current_settings_hold(self.store, event_id)["hold"])

    def test_an_order_the_timestamps_cannot_settle_is_undetermined(self):
        """RED: one timestamp for both, so no side is claimed."""
        event_id = self.refused()
        self.adapter.threads[PARENT].archived = True
        self.attempt(event_id, now=self.delivery_row(event_id)["next_eligible_at"])
        self.strip_this_revisions_records(event_id)
        self.assertEqual(current_settings_hold(self.store, event_id)["hold"]["source"],
                         "undetermined")


class TheOrderOfTheNextAction(unittest.TestCase):
    @staticmethod
    def projection(state, hold, *, host_lost=1, pacing=None, kind="withheld"):
        return {"completion": {
            "eventId": "event-1", "ack": None,
            "delivery": {"state": state, "holdReason": hold, "hostLostAttempts": host_lost,
                         "pacing": pacing,
                         "settingsHold": {"kind": kind, "source": "attempt",
                                          "reason": NOT_PRESERVED}},
        }}

    def test_a_settings_hold_is_named_before_a_host_loss_or_the_send_policy(self):
        """RED: a withheld settings hold after a host loss read as a redelivery the daemon owns."""
        self.assertEqual(completion_next_action(RECEIVED, self.projection(WITHHELD_PRE_SEND, None)),
                         OPERATOR_RESTORES_SETTINGS_ACTION)
        self.assertEqual(completion_next_action(
            RECEIVED, self.projection(WITHHELD_PRE_SEND, "attempt_cap", kind="capped")),
            PARENT_RECOVERS_SETTINGS_HOLD_ACTION)
        # A budget no window reopens comes first: nothing is sent until the policy changes, not
        # even the retry that would re-read the recorded refusal (Devin on 566eecb5).
        never = {"reason": "hourly_cap", "reopensAt": None}
        self.assertEqual(completion_next_action(
            RECEIVED, self.projection(WITHHELD_PRE_SEND, None, host_lost=0, pacing=never)),
            "operator_changes_send_policy")

    def test_a_correction_asks_the_same_order(self):
        """RED: a correction withheld on its child's settings read as the daemon's."""
        def correction(pacing):
            return {"correction": {
                "eventId": "event-2", "supersession": None, "undeliveredReason": None,
                "delivery": {"state": WITHHELD_PRE_SEND, "holdReason": None, "pacing": pacing,
                             "settingsHold": {"kind": "withheld", "source": "pre_send",
                                              "reason": NOT_PRESERVED}},
            }}
        self.assertEqual(correction_next_action(NEEDS_CHANGES, correction(None)),
                         OPERATOR_RESTORES_SETTINGS_ACTION)
        self.assertEqual(correction_next_action(
            NEEDS_CHANGES, correction({"reason": "hourly_cap", "reopensAt": None})),
            "operator_changes_send_policy")


class AnAcceptedNarrowingIsWrittenDown(SettingsHoldCase):
    def test_an_accepted_narrowing_is_journaled_beside_its_settlement(self):
        """RED: the note lived only on the transport receipt."""
        self.parent_history()
        _relationship, event_id = self.queued_event()
        self.adapter.script("accepted_with_notes")
        record = self.attempt(event_id)
        self.assertEqual(record["deliveryState"], DISPATCHED)
        row = self.store.one("SELECT detail FROM journal WHERE subject = ? AND kind = ?",
                             (event_id, "delivery_settings_noted"))
        self.assertIsNotNone(row)
        detail = json.loads(row["detail"])
        self.assertEqual(detail["requestId"], record["requestId"])
        self.assertEqual(detail["notes"][0]["code"], "runtime_roots_narrower_than_record")
        self.assertIsNone(self.status_of(event_id)["settingsHold"], "a delivered send holds nothing")


if __name__ == "__main__":
    unittest.main()
