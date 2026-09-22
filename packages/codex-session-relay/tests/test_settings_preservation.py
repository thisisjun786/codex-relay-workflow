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

    def test_a_recorded_string_field_that_is_not_a_string_is_refused(self):
        """Present is not usable, and no receipt could ever have said which it was.

        ThreadResumeParams types cwd, model and reasoningEffort as strings, and resume_params
        copies each recorded value straight into the params. Before this rule every shape below
        passed the completeness gate and went on the wire, so the only answer about it came back
        from a host whose schema is not in this repository - which is why the rule has to run
        before the send rather than be read off the response.

        Seven shapes rather than one, because the ways a row goes wrong are not alike: a
        number, two containers, and True, which isinstance(x, str) excludes and a truthiness
        test would have let through as a model name.
        """
        shapes = [
            ("cwd", 7, "int"),
            ("cwd", {"path": "/parent"}, "dict"),
            ("cwd", ["/parent"], "list"),
            ("model", 7, "int"),
            ("model", True, "bool"),
            ("reasoningEffort", 7, "int"),
            ("reasoningEffort", {"level": "xhigh"}, "dict"),
        ]
        for field, recorded, kind in shapes:
            with self.subTest(field=field, recorded=kind):
                stale = task_settings("/parent")
                stale[field] = recorded
                view = TaskSettings(stale)
                # Asserted, not assumed: a record that were merely incomplete would be refused
                # by the gate before this one, and this test would prove the wrong rule.
                self.assertEqual(view.missing(), [], "the record is complete, not incomplete")
                self.assertEqual(view.mistyped(), [field])
                refusal = self.assertRefused(
                    RefusalReason.SETTINGS_MISTYPED, view.require_usable,
                )
                # The type it actually holds, because "not a string" does not tell an operator
                # what the creation result put there.
                self.assertEqual(refusal.detail, f"{field} is {kind}, not str")

    def test_every_mistyped_field_is_named_in_one_refusal(self):
        """One round rather than three. missing() answers this way and the two read alike."""
        stale = task_settings("/parent")
        stale.update(cwd=7, model=True, reasoningEffort=["xhigh"])
        view = TaskSettings(stale)
        self.assertEqual(view.mistyped(), ["cwd", "model", "reasoningEffort"])
        refusal = self.assertRefused(RefusalReason.SETTINGS_MISTYPED, view.require_usable)
        self.assertEqual(
            refusal.detail,
            "cwd is int, not str; model is bool, not str; reasoningEffort is list, not str",
        )

    def test_a_mistyped_field_withholds_the_send_before_any_transport_call(self):
        """The rule is proved above; this proves the PATH, and what a parent ends up reading.

        The unit refusal never touches the serialization, and the journal is where the reason
        string a parent acts on is actually written. One field rather than seven: the withhold
        branch treats every refusal from the settings check alike, so repeating the shapes here
        would re-prove one branch rather than another fact.
        """
        mistyped = task_settings("/parent")
        mistyped["cwd"] = 7
        _relationship, event_id = self.queued_event(settings=mistyped)
        self.assertIsNone(self.attempt(event_id))
        self.assertEqual(self.adapter.sends, [], "nothing may reach the host")
        self.assertEqual(self.attempts_for(event_id), [], "no attempt was claimed")
        self.assertEqual(self.delivery_row(event_id)["state"], WITHHELD_PRE_SEND)
        entry = self.store.all(
            "SELECT detail FROM journal WHERE kind = ? ORDER BY rowid DESC LIMIT 1",
            ("delivery_withheld",),
        )[0]
        self.assertIn(RefusalReason.SETTINGS_MISTYPED.value, entry["detail"])
        self.assertIn("cwd is int, not str", entry["detail"])

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

    CODES = ("settings_not_preserved", "setting_unobservable", "environments_unknown",
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

    def test_an_absent_policy_withholds_while_a_reported_one_closes_the_channel(self):
        """The two approval outcomes must not collapse into one another.

        A REPORTED non-never policy means the push channel is closed for good: inbox_only, not
        retryable. An ABSENT one means we could not see the policy at all, which proves nothing
        about the channel, so it withholds before the send and stays retryable.
        """
        absent = classify_operation_receipt({
            "requestId": "del-000000000000-a3", "status": "failed",
            "resumed": {"approvalPolicy": None},
            "error": "thread/resume: setting_unobservable",
            "rpcError": {"code": "setting_unobservable", "message": "approvalPolicy"},
        })
        self.assertEqual(absent.delivery_state, WITHHELD_PRE_SEND)
        self.assertEqual(absent.send_attempted, "no")
        self.assertTrue(absent.retry_safe)

        reported = classify_operation_receipt({
            "requestId": "del-000000000000-a4", "status": "failed",
            "resumed": {"approvalPolicy": "on-request"},
            "error": "thread/resume: Interactive approvals unsupported",
            "rpcError": {"code": "unsupported_approval_policy", "message": "unsupported"},
        })
        self.assertEqual(reported.delivery_state, INBOX_ONLY)
        self.assertFalse(reported.retry_safe)

        settings = TaskSettings(task_settings("/parent"))
        findings = settings.mismatches({
            "approvalPolicy": None,
            "sandbox": {"type": "dangerFullAccess"},
            "thread": {"environments": None},
        })
        self.assertEqual(len(findings), 1, "an unreadable policy decides alone too")
        self.assertEqual(findings[0]["code"], "setting_unobservable")

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


class AnUnreadablePolicyNeverAgreesWithItself(DeliveryTestCase):
    """Two unreadable policies both normalise to None, and None == None is not agreement.

    A supported `type` is not enough to make a record usable: a malformed stored policy and an
    equally malformed response would have compared equal and sent under a sandbox that nothing
    ever verified.
    """

    MALFORMED = {"type": "workspaceWrite", "writableRoots": None}

    def test_an_unreadable_record_is_refused_before_any_send(self):
        broken = task_settings("/parent", sandbox=dict(self.MALFORMED))
        _relationship, event_id = self.queued_event(settings=broken)
        self.assertIsNone(self.attempt(event_id))
        self.assertEqual(self.adapter.sends, [], "nothing may reach the host")
        entry = self.store.all(
            "SELECT detail FROM journal WHERE kind = ? ORDER BY rowid DESC LIMIT 1",
            ("delivery_withheld",),
        )[0]
        self.assertIn(RefusalReason.UNSUPPORTED_SANDBOX_TYPE.value, entry["detail"])

    def test_the_comparison_refuses_even_if_such_a_record_reached_it(self):
        record = task_settings("/parent", sandbox=dict(self.MALFORMED))
        settings = TaskSettings(record)
        # Everything else agrees, so only the sandbox can be the finding.
        findings = settings.mismatches({
            "approvalPolicy": "never",
            "sandbox": dict(self.MALFORMED),
            "cwd": record["cwd"],
            "runtimeWorkspaceRoots": list(record["runtimeWorkspaceRoots"]),
            "model": record["model"],
            "reasoningEffort": record["reasoningEffort"],
            "thread": {"environments": [dict(e) for e in record["environments"]]},
        })
        self.assertTrue(findings, "two unreadable policies must not agree")
        self.assertEqual(findings[0]["field"], "sandbox")
        self.assertEqual(findings[0]["code"], "settings_not_preserved")
