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

    def test_a_mistyped_field_withholds_the_send_and_records_why(self):
        """The rule is proved above; this proves the PATH, and what a parent ends up reading.

        What is prevented is measured rather than asserted from the phrase this package uses
        elsewhere. By the time the settings gate runs, attempt() has already asked the host
        read_thread, is_archived and read_goal_status through observe(); those are lifecycle
        reads and they happen for every delivery. What the refusal stops is everything after
        it: no claim, no attempt record, nothing sent.

        The reason is then checked in BOTH places it is persisted, because they are separate
        writes and a caller reads different ones. The journal carries the detail a parent acts
        on; failed_operations carries the error_code status reports and the retry_safe flag
        that keeps the withhold from becoming a permanent hold.

        One field rather than seven: the withhold branch treats every refusal from the settings
        check alike, so repeating the shapes here would re-prove one branch rather than a fact.
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
        failure = self.store.all(
            "SELECT operation, error_code, retry_safe FROM failed_operations"
            " ORDER BY rowid DESC LIMIT 1",
        )[0]
        self.assertEqual(failure["operation"], "settings_check")
        self.assertEqual(failure["error_code"], RefusalReason.SETTINGS_MISTYPED.value)
        self.assertEqual(failure["retry_safe"], 1, "re-recording the row is the recovery")

    def test_an_absent_field_is_decided_before_a_mistyped_one(self):
        """The ordering mistyped() depends on, guarded where it can actually break.

        mistyped() subscripts self.data, so on an incomplete record it raises KeyError rather
        than refusing. require_usable() is what keeps that unreachable, by answering absence
        first. This record is wrong BOTH ways, and the completeness answer has to win.
        """
        stale = task_settings("/parent")
        del stale["cwd"]
        stale["model"] = 7
        view = TaskSettings(stale)
        refusal = self.assertRefused(RefusalReason.SETTINGS_INCOMPLETE, view.require_usable)
        self.assertEqual(refusal.detail, "missing cwd")

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


class AnApprovalPolicyThisTransportCannotCarry(DeliveryTestCase):
    """The rule read off the RECORD, where it decides the send, rather than off the response.

    CRW-225 carries never AND on-request (settings.CARRIED_APPROVAL_POLICIES); these rows use
    untrusted, which is still outside the set. The history below is the rule's own.

    It used to live in exactly one place: `mismatches` comparing what the HOST reported back.
    Nothing compared the recorded value, so a row recording `on-request` was registered, passed
    `require_usable()`, was built into resume params and sent - and what happened next was the
    host's choice. A host preserving the requested policy answered it back and the push channel
    closed; a host normalising it answered `never`, raised no finding, and the message was
    delivered. Measured both ways before this moved. Whether a message arrived therefore turned
    on what a host did with a fact the record already contained.
    """

    def test_a_recorded_policy_this_transport_cannot_carry_is_refused(self):
        """Four shapes, one rule, because the field can hold more than another policy name.

        A value comparison rather than a type rule, which is why `mistyped()` leaves this field
        alone: 7 and True and a granular object are all refused here, by the code that says the
        specific thing, instead of by the one that says only 'not a string'.
        """
        for recorded in ("untrusted", "never ", 7, True, {"mode": "on-request"}):
            with self.subTest(recorded=recorded):
                stale = task_settings("/parent")
                stale["approvalPolicy"] = recorded
                view = TaskSettings(stale)
                # Asserted, not assumed: a row refused one gate earlier would prove another rule.
                self.assertEqual(view.missing(), [], "the record is complete")
                self.assertEqual(view.mistyped(), [], "no string field is at fault here")
                refusal = self.assertRefused(
                    RefusalReason.UNSUPPORTED_APPROVAL_POLICY, view.require_usable,
                )
                # The value it actually holds: 'unsupported' does not tell an operator which
                # row to repair or what the creation result put there.
                self.assertIn(repr(recorded), refusal.detail)
                self.assertIn("'never' and 'on-request'", refusal.detail)

    def test_an_absent_policy_stays_incomplete_rather_than_unsupported(self):
        """Null is an absence, and absence has its own recovery: record the field.

        Refusing it as an unsupported policy would point the repair at choosing a different
        value, when nothing was chosen at all.
        """
        stale = task_settings("/parent")
        stale["approvalPolicy"] = None
        refusal = self.assertRefused(
            RefusalReason.SETTINGS_INCOMPLETE, TaskSettings(stale).require_usable,
        )
        self.assertEqual(refusal.detail, "missing approvalPolicy")

    def test_the_gates_decide_in_one_order_whatever_the_row_gets_wrong(self):
        """Shape before meaning, and the approval policy first among the meaning gates.

        One row wrong in four ways, repaired one way at a time, so the ORDER is what is proved
        rather than four rows each wrong once. Approval before sandbox is not a preference: it
        is the order `mismatches` already decides the same two fields in, so a row wrong in
        both gets one answer instead of two that depend on which surface refused it.
        """
        stale = task_settings("/parent", sandbox={"type": "externalSandbox"})
        del stale["cwd"]
        stale["model"] = 7
        stale["approvalPolicy"] = "untrusted"

        ladder = [
            (RefusalReason.SETTINGS_INCOMPLETE, "missing cwd"),
            (RefusalReason.SETTINGS_MISTYPED, "model is int, not str"),
            (RefusalReason.UNSUPPORTED_APPROVAL_POLICY, "'untrusted'"),
            (RefusalReason.UNSUPPORTED_SANDBOX_TYPE, "'externalSandbox'"),
        ]
        repairs = [
            lambda row: row.update(cwd="/parent"),
            lambda row: row.update(model="anthropic/claude-opus-5"),
            lambda row: row.update(approvalPolicy="never"),
            lambda row: row.update(sandbox=task_settings("/parent")["sandbox"]),
        ]
        for (reason, says), repair in zip(ladder, repairs):
            with self.subTest(refusal=reason.value):
                refusal = self.assertRefused(reason, TaskSettings(stale).require_usable)
                self.assertIn(says, refusal.detail)
                repair(stale)
        TaskSettings(stale).require_usable()

    def test_the_record_withholds_the_send_and_a_host_is_never_asked(self):
        """The path, and the defect stated as what it was.

        The fake host reports `never` regardless of what it is sent, which is precisely the
        host this row used to be delivered against: the send completed and the parent was woken
        under a policy the relay cannot service. Now nothing is claimed and nothing is sent, so
        the host's answer never enters it.
        """
        interactive = task_settings("/parent", approvalPolicy="untrusted")
        _relationship, event_id = self.queued_event(settings=interactive)
        self.assertEqual(self.adapter.threads[PARENT].approval_policy, "never",
                         "the fixture host is the one that used to make this send succeed")
        self.assertIsNone(self.attempt(event_id))
        self.assertEqual(self.adapter.sends, [], "nothing may reach the host")
        self.assertEqual(self.attempts_for(event_id), [], "no attempt was claimed")
        self.assertEqual(self.delivery_row(event_id)["state"], WITHHELD_PRE_SEND)
        entry = self.store.all(
            "SELECT detail FROM journal WHERE kind = ? ORDER BY rowid DESC LIMIT 1",
            ("delivery_withheld",),
        )[0]
        self.assertIn(RefusalReason.UNSUPPORTED_APPROVAL_POLICY.value, entry["detail"])
        self.assertIn("untrusted", entry["detail"])
        failure = self.store.all(
            "SELECT operation, error_code, retry_safe FROM failed_operations"
            " ORDER BY rowid DESC LIMIT 1",
        )[0]
        self.assertEqual(failure["operation"], "settings_check")
        self.assertEqual(failure["error_code"],
                         RefusalReason.UNSUPPORTED_APPROVAL_POLICY.value)
        self.assertEqual(failure["retry_safe"], 1, "re-recording the row is the recovery")

    def test_registration_refuses_the_row_rather_than_storing_it(self):
        """One rule, and the earliest surface that can apply it owns the first answer.

        A row refused at every send is a row that should never have been stored: leaving it to
        delivery means the refusal is discovered once per pass, by whoever is waiting for the
        message, instead of once by whoever recorded it.
        """
        self.assertRefused(
            RefusalReason.UNSUPPORTED_APPROVAL_POLICY,
            lambda: record_settings(
                self.store, self.clock, PARENT,
                task_settings("/parent", approvalPolicy="untrusted"),
                source="creation_result",
            ),
        )
        self.assertIsNone(load_settings(self.store, PARENT), "the refused row was stored")

    def test_the_record_rule_and_the_response_rule_stay_two_facts(self):
        """Same policy value, two different situations, and they must not collapse.

        On the RECORD it is a value we chose and can re-record, so the delivery is withheld and
        stays retryable. In the RESPONSE it is the host's own state, which no re-recording
        reaches, so the delivery is inbox_only and terminal. Moving the first one did not move
        the second.

        One event carries both halves, which also proves the recovery: the withhold is not a
        permanent hold, and re-recording the row is what releases it.
        """
        interactive = task_settings("/parent", approvalPolicy="untrusted")
        _relationship, recorded_event = self.queued_event(settings=interactive)
        self.assertIsNone(self.attempt(recorded_event))
        self.assertEqual(self.delivery_row(recorded_event)["state"], WITHHELD_PRE_SEND)
        self.assertEqual(self.adapter.sends, [], "the record decided it; no host was asked")

        record_settings(self.store, self.clock, PARENT, task_settings("/parent"),
                        source="creation_result")
        self.adapter.threads[PARENT].approval_policy = "untrusted"
        self.adapter.script("approval_policy")
        record = self.attempt(recorded_event, now=self.clock.now() + 10_000)
        self.assertEqual(record["deliveryState"], INBOX_ONLY)
        self.assertEqual(record["recipientApprovalPolicy"], "untrusted")
        self.assertFalse(record["retrySafe"])


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
            "resumed": {"approvalPolicy": "untrusted"},
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
            "resumed": {"approvalPolicy": "untrusted"},
            "error": "thread/resume: Interactive approvals unsupported; message withheld.",
            "rpcError": {"code": "unsupported_approval_policy", "message": "unsupported"},
        })
        self.assertEqual(facts.delivery_state, INBOX_ONLY)
        self.assertFalse(facts.retry_safe)
        settings = TaskSettings(task_settings("/parent"))
        findings = settings.mismatches({
            "approvalPolicy": "untrusted",
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


class ASandboxRecordedAsSomethingOtherThanAPolicy(DeliveryTestCase):
    """The row holds a value in place of the SandboxPolicy object, and it has to be REFUSED.

    What it used to do instead is the defect. `sandbox_mode()` read `.get('type')` off whatever
    the row held, so a legacy or hand-edited record carrying the bare mode name 'workspaceWrite'
    left an AttributeError where `require_usable()` owes its caller a `DeliveryRefused`, and a
    `{'type': {...}}` left a TypeError on an unhashable dict key. Registration, delivery and
    `settings-show` all validate through that one method, so each answered such a row with an
    exception rather than with the refusal this package already had for a policy it cannot read.

    The reason is that existing one. `normalise_policy()` reads a non-dict as unreadable
    (settings.py) and the gate below already answers `unsupported_sandbox_type` for exactly that,
    so these rows were always destined for this code; only the crash one gate earlier hid it.
    Refusing them as a mistyped field instead would have given one situation two vocabularies.

    Shapes rather than the two reported instances, because the ways a row goes wrong are not
    alike and two different lines used to raise: a TRUTHY non-dict died in the mode lookup, while
    a FALSY one ('', 0, [], False) reached the gate's message instead, since `... or {}` had
    already turned it into an empty dict. `missing()` counts all of them as present -- it tests
    `is None` -- so nothing earlier catches them.
    """

    NOT_AN_OBJECT = (
        "the recorded sandbox is {kind}, not the policy object a creation result reports,"
        " so it does not record the full policy a resume would have to restore"
    )

    def test_every_shape_a_row_can_hold_instead_of_a_policy_is_refused(self):
        """Nine shapes, one refusal, and the detail names the type the row actually holds."""
        shapes = [
            ("workspaceWrite", "str"),  # the reported instance: a mode name, not a policy
            ("", "str"),
            (["workspaceWrite"], "list"),
            ([], "list"),
            (7, "int"),
            (0, "int"),
            (1.5, "float"),
            (True, "bool"),
            (False, "bool"),
        ]
        for recorded, kind in shapes:
            with self.subTest(recorded=recorded):
                stale = task_settings("/parent", sandbox=recorded)
                view = TaskSettings(stale)
                # Asserted, not assumed: an absent sandbox is refused one gate earlier, and
                # this test would then be proving completeness rather than shape.
                self.assertEqual(view.missing(), [], "the record is complete, not incomplete")
                # Total: the answer comes BACK rather than being raised out of the gate.
                self.assertIsNone(view.sandbox_mode())
                refusal = self.assertRefused(
                    RefusalReason.UNSUPPORTED_SANDBOX_TYPE, view.require_usable,
                )
                # Exact, not a substring: 'names no sandbox' would be its own false statement
                # about a row holding 'workspaceWrite', which IS a mode name. What it lacks is
                # the policy around it, and the detail has to say that and nothing else.
                self.assertEqual(refusal.detail, self.NOT_AN_OBJECT.format(kind=kind))

    def test_a_policy_object_whose_type_cannot_be_read_is_refused_not_raised(self):
        """The same class one level in: the row IS an object and its type is not a mode name.

        The unhashable member is the one no gate ordering reaches: a dict key lookup raises
        TypeError before any refusal exists, which is the case `normalise_policy()` already
        guards for itself. Here the record keeps the dict wording, because a dict is what it is.
        """
        for recorded in ({"type": {"mode": "workspaceWrite"}}, {"type": ["workspaceWrite"]},
                         {"type": 7}, {"type": True}, {"type": None}, {}):
            with self.subTest(recorded=recorded):
                view = TaskSettings(task_settings("/parent", sandbox=recorded))
                self.assertEqual(view.missing(), [], "the record is complete")
                self.assertIsNone(view.sandbox_mode())
                refusal = self.assertRefused(
                    RefusalReason.UNSUPPORTED_SANDBOX_TYPE, view.require_usable,
                )
                self.assertEqual(
                    refusal.detail,
                    f"{recorded.get('type')!r} has no ThreadResumeParams.sandbox mode, so it"
                    " cannot be restored on a resume",
                )

    def test_an_absent_sandbox_stays_incomplete_rather_than_unreadable(self):
        """The class boundary. Absence has its own recovery: record the field.

        Refusing it here would point the repair at re-recording a policy that was never wrong,
        and `missing()` answers null and omitted alike one gate earlier.
        """
        for label, mutate in (("null", lambda row: row.update(sandbox=None)),
                              ("omitted", lambda row: row.pop("sandbox"))):
            with self.subTest(sandbox=label):
                stale = task_settings("/parent")
                mutate(stale)
                refusal = self.assertRefused(
                    RefusalReason.SETTINGS_INCOMPLETE, TaskSettings(stale).require_usable,
                )
                self.assertEqual(refusal.detail, "missing sandbox")

    def test_the_answers_for_a_readable_policy_are_unchanged(self):
        """The other half of the rule: a total function must not start refusing what it carried.

        The unsupported-type detail is asserted EXACTLY rather than by substring, because the
        ladder elsewhere in this file compares with assertIn and would not notice this text
        moving underneath it.
        """
        usable = TaskSettings(task_settings("/parent"))
        usable.require_usable()
        self.assertEqual(usable.sandbox_mode(), "workspace-write")
        self.assertEqual(usable.resume_params("t-1")["sandbox"], "workspace-write")

        external = TaskSettings(task_settings("/parent", sandbox={"type": "externalSandbox"}))
        refusal = self.assertRefused(
            RefusalReason.UNSUPPORTED_SANDBOX_TYPE, external.require_usable,
        )
        self.assertEqual(
            refusal.detail,
            "'externalSandbox' has no ThreadResumeParams.sandbox mode, so it cannot be"
            " restored on a resume",
        )

    def test_the_send_is_withheld_and_the_reason_is_what_a_parent_reads(self):
        """The PATH, not the rule: what a store holding such a row actually does to a delivery.

        Before this, the settings check raised AttributeError out of the delivery pass instead of
        withholding, so the journal and `failed_operations` never got the row's real answer.
        """
        _relationship, event_id = self.queued_event(
            settings=task_settings("/parent", sandbox="workspaceWrite"),
        )
        self.assertIsNone(self.attempt(event_id))
        self.assertEqual(self.adapter.sends, [], "nothing may reach the host")
        self.assertEqual(self.attempts_for(event_id), [], "no attempt was claimed")
        self.assertEqual(self.delivery_row(event_id)["state"], WITHHELD_PRE_SEND)
        entry = self.store.all(
            "SELECT detail FROM journal WHERE kind = ? ORDER BY rowid DESC LIMIT 1",
            ("delivery_withheld",),
        )[0]
        self.assertIn(RefusalReason.UNSUPPORTED_SANDBOX_TYPE.value, entry["detail"])
        self.assertIn("the recorded sandbox is str", entry["detail"])
        failure = self.store.all(
            "SELECT operation, error_code, retry_safe FROM failed_operations"
            " ORDER BY rowid DESC LIMIT 1",
        )[0]
        self.assertEqual(failure["operation"], "settings_check")
        self.assertEqual(failure["error_code"], RefusalReason.UNSUPPORTED_SANDBOX_TYPE.value)
        self.assertEqual(failure["retry_safe"], 1, "re-recording the row is the recovery")

    def test_registration_refuses_the_row_rather_than_storing_it(self):
        """The earliest surface owns the first answer, and it used to raise here too."""
        self.assertRefused(
            RefusalReason.UNSUPPORTED_SANDBOX_TYPE,
            lambda: record_settings(
                self.store, self.clock, "01other",
                task_settings("/parent", sandbox=["workspaceWrite"]),
                source="creation_result",
            ),
        )
        self.assertIsNone(load_settings(self.store, "01other"), "the refused row was stored")
