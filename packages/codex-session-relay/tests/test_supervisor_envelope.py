"""CRW-148: can a recipient tell what it was asked, and can a reader tell what was not said.

Every case here is written from the reader's side. The question is never whether a key exists
but whether somebody holding this envelope would act on something nobody established.
"""

import unittest

from codex_session_relay import envelope, identity, linkage as linkage_module, report
from codex_session_relay.errors import RefusalReason

from .support import DeliveryTestCase
from .test_report_contract import a_report
from .test_linkage import INITIATIVE, PROJECT, SUPERVISOR_TASK, LinkageTestCase

LINK = "lnk-0123456789abcdef"
DIGEST = "d" * 64


class TheVocabulary(unittest.TestCase):
    def test_a_purpose_belongs_to_one_direction_and_carries_its_own_kind(self):
        self.assertEqual(
            envelope.kind_of(envelope.PARENT_TO_SUPERVISOR, "decision_request"),
            envelope.DECISION)
        self.assertEqual(
            envelope.kind_of(envelope.SUPERVISOR_TO_PARENT, "scope_correction"),
            envelope.REQUEST)
        with self.assertRaises(envelope.EnvelopeRefused):
            envelope.kind_of(envelope.SUPERVISOR_TO_PARENT, "completion")

    def test_a_relayed_decision_asks_the_parent_to_apply_one_already_made(self):
        """Jun decided. The parent owes the application, not another opinion.

        Classifying it as a decision would send the parent looking for somebody to decide
        something that has been decided, which is how a relayed instruction becomes a second
        round of deliberation.
        """
        self.assertEqual(
            envelope.kind_of(envelope.SUPERVISOR_TO_PARENT, "relayed_decision"),
            envelope.REQUEST)
        self.assertEqual(envelope.ANSWER_OWED_BY[envelope.REQUEST], envelope.RECIPIENT)
        self.assertEqual(envelope.ANSWER_OWED_BY[envelope.DECISION], envelope.USER)
        self.assertIsNone(envelope.ANSWER_OWED_BY[envelope.NOTIFICATION])

    def test_the_roles_come_from_the_direction_and_not_from_a_caller(self):
        one = self._region(envelope.SUPERVISOR_TO_PARENT, "midpoint_check")
        self.assertEqual(one["sender"]["role"], "supervisor")
        self.assertEqual(one["recipient"]["role"], "parent")

    def _region(self, direction, purpose, **kw):
        return envelope.region(
            direction=direction, purpose=purpose, relation_id=LINK, sender="01supervisor",
            recipient="01parent", subject=DIGEST, observed_at="2026-09-22T00:00:00Z", **kw)


class TheTwoIdentifiers(unittest.TestCase):
    """A logical message and a transport attempt are not the same thing."""

    def test_the_message_id_survives_the_retries_the_request_id_counts(self):
        event = "a" * 32
        first = identity.request_id(event, 1)
        tenth = identity.request_id(event, 10)
        self.assertNotEqual(first, tenth, "the attempt identity is supposed to move")
        stable = [envelope.message_id(direction=envelope.CHILD_TO_PARENT,
                                      relation_id="rel-0123456789abcdef",
                                      purpose="completion", subject=event)
                  for _ in range(3)]
        self.assertEqual(len(set(stable)), 1, "the logical identity is supposed not to")

    def test_a_different_purpose_or_subject_is_a_different_message(self):
        base = dict(direction=envelope.SUPERVISOR_TO_PARENT, relation_id=LINK, subject=DIGEST)
        assignment = envelope.message_id(purpose="project_assignment", **base)
        correction = envelope.message_id(purpose="scope_correction", **base)
        elsewhere = envelope.message_id(purpose="project_assignment",
                                        direction=envelope.SUPERVISOR_TO_PARENT,
                                        relation_id="lnk-ffffffffffffffff", subject=DIGEST)
        self.assertNotEqual(assignment, correction)
        self.assertNotEqual(assignment, elsewhere)

    def test_the_linear_scope_is_not_an_input_to_identity(self):
        """Otherwise the id would depend on whether the reader happened to hold a scope row.

        A renderer inside the claim transaction may not be able to read the scope table. If
        that changed the id, the same message would converge on two different obligations
        depending on which caller looked at it.
        """
        without = envelope.region(
            direction=envelope.CHILD_TO_PARENT, purpose="completion",
            relation_id="rel-0123456789abcdef", sender="01child", recipient="01parent",
            subject="a" * 32, observed_at="2026-09-22T00:00:00Z")
        with_scope = envelope.region(
            direction=envelope.CHILD_TO_PARENT, purpose="completion",
            relation_id="rel-0123456789abcdef", sender="01child", recipient="01parent",
            subject="a" * 32, observed_at="2026-09-22T00:00:00Z",
            scope="project CRW, issue CRW-148")
        self.assertEqual(without["messageId"], with_scope["messageId"])


class WhatWasNotSaid(unittest.TestCase):
    def test_the_three_absences_are_not_interchangeable(self):
        one = envelope.region(
            direction=envelope.PARENT_TO_SUPERVISOR, purpose="completion",
            relation_id=LINK, sender="01parent",
            recipient=envelope.absent(envelope.UNKNOWN, "no supervisor binding was read"),
            subject=DIGEST, observed_at="2026-09-22T00:00:00Z")
        self.assertEqual(one["scope"]["absent"], envelope.UNKNOWN)
        self.assertEqual(one["correlationId"]["absent"], envelope.NOT_APPLICABLE)
        self.assertIn("unknown", envelope.shown(one["recipient"]["taskId"]))
        self.assertNotEqual(envelope.absent(envelope.UNKNOWN),
                            envelope.absent(envelope.NOT_APPLICABLE))

    def test_an_absence_renders_as_an_answer_rather_than_as_a_blank(self):
        line = envelope.shown(envelope.absent(envelope.INHERITED, "stated on the assignment"))
        self.assertIn("inherited", line)
        self.assertIn("stated on the assignment", line)

    def test_a_word_nobody_defined_is_not_one_of_the_three(self):
        """Checking the key rather than the reason let a typo pass as a stated absence."""
        self.assertFalse(envelope.is_absent({"absent": "probably"}))
        self.assertFalse(envelope.is_absent({"absent": None}))
        for reason in envelope.ABSENCES:
            self.assertTrue(envelope.is_absent(envelope.absent(reason)))
        with self.assertRaises(envelope.EnvelopeRefused):
            envelope.absent("probably")

    def test_a_kind_cannot_omit_what_it_exists_to_carry(self):
        with self.assertRaises(envelope.EnvelopeRefused) as caught:
            envelope.region(
                direction=envelope.PARENT_TO_SUPERVISOR, purpose="decision_request",
                relation_id=LINK, sender="01parent", recipient="01supervisor",
                subject=DIGEST, observed_at="2026-09-22T00:00:00Z")
        self.assertEqual(caught.exception.reason, RefusalReason.MALFORMED_RECEIPT)
        self.assertIn("decision", str(caught.exception))
        allowed = envelope.region(
            direction=envelope.PARENT_TO_SUPERVISOR, purpose="decision_request",
            relation_id=LINK, sender="01parent", recipient="01supervisor", subject=DIGEST,
            observed_at="2026-09-22T00:00:00Z",
            decision="whether to hold the merge window open for CRW-123")
        self.assertEqual(allowed["kind"], envelope.DECISION)

    def test_a_status_response_must_name_what_it_answers(self):
        with self.assertRaises(envelope.EnvelopeRefused):
            envelope.region(
                direction=envelope.PARENT_TO_SUPERVISOR, purpose="status_response",
                relation_id=LINK, sender="01parent", recipient="01supervisor",
                subject=DIGEST, observed_at="2026-09-22T00:00:00Z")


class TheFiveStages(unittest.TestCase):
    def test_the_upward_direction_separates_what_is_unanswered_from_what_cannot_answer(self):
        """not_applicable and unmeasured are different news for whoever is waiting.

        Unmeasured says go and look. not_applicable says there is nothing to look at, and the
        upward direction is now both at once: a report is staged, sent and read back, so the
        first two stages have records and start unanswered; a supervisor agreeing, applying
        or verifying is still not a fact this store holds, so those three stay impossible and
        a caller waiting for one waits forever.
        """
        ladder = envelope.unreached(envelope.PARENT_TO_SUPERVISOR)
        for name in (envelope.TRANSPORT_ACCEPTED, envelope.RECEIVED):
            self.assertEqual(ladder[name]["state"], envelope.UNMEASURED)
        for name in (envelope.AGREED, envelope.APPLIED, envelope.VERIFIED):
            self.assertEqual(ladder[name]["state"], envelope.IMPOSSIBLE)
            self.assertIn("agreed, applied or verified", ladder[name]["detail"])

    def test_the_directive_direction_still_records_rather_than_sends(self):
        ladder = envelope.unreached(envelope.SUPERVISOR_TO_PARENT)
        self.assertEqual(ladder[envelope.TRANSPORT_ACCEPTED]["state"], envelope.IMPOSSIBLE)
        self.assertIn("recorded rather than sent",
                      ladder[envelope.TRANSPORT_ACCEPTED]["detail"])
        self.assertEqual(ladder[envelope.RECEIVED]["state"], envelope.UNMEASURED)

    def test_the_correction_direction_has_a_transport_but_no_acknowledgement(self):
        ladder = envelope.unreached(envelope.PARENT_TO_CHILD)
        self.assertEqual(ladder[envelope.TRANSPORT_ACCEPTED]["state"], envelope.UNMEASURED)
        self.assertEqual(ladder[envelope.RECEIVED]["state"], envelope.IMPOSSIBLE)
        self.assertIn("completion receipt", ladder[envelope.RECEIVED]["detail"])
        self.assertEqual(ladder[envelope.APPLIED]["state"], envelope.UNMEASURED)

    def test_silence_is_never_agreement_and_a_state_needs_a_source(self):
        ladder = envelope.unreached(envelope.CHILD_TO_PARENT)
        self.assertEqual(ladder[envelope.AGREED]["state"], envelope.UNMEASURED,
                         "the stage is present and unanswered, not missing")
        self.assertFalse(envelope.stage_holds(ladder, envelope.AGREED))
        with self.assertRaises(envelope.EnvelopeRefused):
            envelope.stage(envelope.YES)
        self.assertEqual(
            envelope.stage(envelope.YES, source="acks")["source"], "acks")

    def test_a_conditional_acceptance_is_not_a_held_stage(self):
        ladder = envelope.unreached(envelope.CHILD_TO_PARENT)
        ladder[envelope.AGREED] = envelope.stage(
            envelope.CONDITIONAL, source="acks", detail="accepted subject to a follow-up")
        self.assertFalse(envelope.stage_holds(ladder, envelope.AGREED))

    def test_a_stage_standing_on_one_that_does_not_hold_is_named(self):
        ladder = envelope.unreached(envelope.CHILD_TO_PARENT)
        ladder[envelope.TRANSPORT_ACCEPTED] = envelope.stage(envelope.YES, source="attempts")
        ladder[envelope.APPLIED] = envelope.stage(envelope.YES, source="verdicts")
        self.assertEqual(envelope.promotion_refused(ladder), [envelope.APPLIED])

    def test_a_stage_with_no_mechanism_is_not_a_missing_prerequisite(self):
        """The correction direction's applied=yes is legitimate and used to read as a promotion.

        It has no acknowledgement at all - the next generation's receipt is what shows it was
        applied - so counting its impossible stages as unheld reported every correct ladder as
        a false promotion, which is the opposite of what this check is for.
        """
        ladder = envelope.unreached(envelope.PARENT_TO_CHILD)
        ladder[envelope.TRANSPORT_ACCEPTED] = envelope.stage(envelope.YES, source="attempts")
        ladder[envelope.APPLIED] = envelope.stage(envelope.YES, source="events")
        self.assertEqual(envelope.promotion_refused(ladder), [])
        ladder[envelope.TRANSPORT_ACCEPTED] = envelope.stage(
            envelope.UNMEASURED, detail="nothing answered")
        self.assertEqual(envelope.promotion_refused(ladder), [envelope.APPLIED],
                         "a stage that COULD have answered and did not is still a prerequisite")

    def test_a_supervisor_acknowledgement_cannot_be_written_into_the_contract(self):
        """The table is enforced, not advice.

        Without this the one module that exists to stop a supervisor being credited with an
        acknowledgement would happily record one, because a caller handed it the ladder.
        """
        ladder = envelope.unreached(envelope.PARENT_TO_SUPERVISOR)
        ladder[envelope.RECEIVED] = envelope.stage(envelope.YES, source="acks")
        with self.assertRaises(envelope.EnvelopeRefused) as caught:
            envelope.region(
                direction=envelope.PARENT_TO_SUPERVISOR, purpose="completion",
                relation_id=LINK, sender="01parent", recipient="01supervisor",
                subject=DIGEST, observed_at="2026-09-22T00:00:00Z", reach=ladder)
        # The stage has a record of its own now, so the refusal names WHICH record answers it
        # rather than saying there is none. A parent-child acknowledgement is still not one.
        self.assertIn("supervisor_readbacks", str(caught.exception))
        ladder[envelope.RECEIVED] = envelope.stage(
            envelope.YES, source="supervisor_readbacks")
        ladder[envelope.AGREED] = envelope.stage(envelope.YES, source="supervisor_readbacks")
        with self.assertRaises(envelope.EnvelopeRefused) as agreed:
            envelope.check_reach(envelope.PARENT_TO_SUPERVISOR, ladder)
        self.assertIn("agreed, applied or verified", str(agreed.exception))

    def test_a_stage_may_not_be_answered_by_a_record_its_direction_does_not_read(self):
        ladder = envelope.unreached(envelope.CHILD_TO_PARENT)
        ladder[envelope.RECEIVED] = envelope.stage(envelope.YES, source="a screenshot")
        with self.assertRaises(envelope.EnvelopeRefused):
            envelope.check_reach(envelope.CHILD_TO_PARENT, ladder)


class TheDirectivePointer(unittest.TestCase):
    def test_a_pointer_derived_from_this_row_agrees_with_it(self):
        text = envelope.directive_reference(purpose="project_assignment", link_id=LINK,
                                            digest=DIGEST)
        self.assertIsNone(envelope.contradiction(text, link_id=LINK, digest=DIGEST))
        parsed = envelope.parse_reference(text)
        self.assertEqual(parsed["purpose"], "project_assignment")
        self.assertEqual(parsed["direction"], envelope.SUPERVISOR_TO_PARENT)

    def test_a_pointer_belonging_to_another_instruction_is_caught(self):
        text = envelope.directive_reference(purpose="project_assignment", link_id=LINK,
                                            digest=DIGEST)
        problem = envelope.contradiction(text, link_id=LINK, digest="e" * 64)
        self.assertIn("belongs to another instruction", problem)

    def test_a_column_that_is_not_ours_contradicts_nothing(self):
        """The reference column predates this contract and an operator's note is ordinary."""
        self.assertIsNone(envelope.parse_reference("see the thread from Tuesday"))
        self.assertIsNone(envelope.contradiction("see the thread from Tuesday",
                                                 link_id=LINK, digest=DIGEST))
        self.assertIsNone(envelope.contradiction(None, link_id=LINK, digest=DIGEST))

    def test_a_correlation_the_pointer_cannot_carry_is_refused_where_it_can_be_fixed(self):
        """Writing a value its own parser erases is worse than refusing it.

        A pipe splits the pointer into an extra field, which parse_reference rejects outright.
        A bare dash is how the pointer spells no correlation, so storing one reads back as an
        answer to nothing.
        """
        for bad in ("crw-148|extra", "-", "", "   "):
            with self.assertRaises(envelope.EnvelopeRefused):
                envelope.directive_reference(purpose="project_assignment", link_id=LINK,
                                             digest=DIGEST, correlation_id=bad)
        good = envelope.directive_reference(purpose="project_assignment", link_id=LINK,
                                            digest=DIGEST, correlation_id="msg-1")
        self.assertEqual(envelope.parse_reference(good)["correlationId"], "msg-1")


class TheDirectiveCommand(LinkageTestCase):
    """A correlation with nowhere to go used to be dropped while the command reported success."""

    def record(self, **overrides):
        from argparse import Namespace
        from codex_session_relay import cli

        execution = getattr(self, "_edge", None) or self.supervise()
        self._edge = execution
        arguments = {"scope_kind": linkage_module.PROJECT, "scope": PROJECT,
                     "from_task": SUPERVISOR_TASK, "from_scope": INITIATIVE,
                     "link": execution["linkId"], "digest": "d-one", "reference": None,
                     "purpose": None, "correlation": None}
        arguments.update(overrides)
        return cli.cmd_linkage_directive(self._services(), Namespace(**arguments))

    def _services(self):
        case = self

        class _Services:
            linkage = case.linkage

        return _Services()

    def test_a_correlation_without_a_purpose_is_refused_rather_than_dropped(self):
        from codex_session_relay import cli

        with self.assertRaises(cli.SystemExit2) as caught:
            self.record(correlation="msg-1")
        self.assertIn("requires --purpose", str(caught.exception))
        self.assertIsNone(
            self.store.one("SELECT directive_id FROM scope_directives WHERE digest = ?",
                           ("d-one",)),
            "and nothing was stored for the instruction that was refused")

    def test_a_purpose_carries_the_correlation_into_the_stored_pointer(self):
        row = self.record(purpose="scope_correction", correlation="msg-1")
        self.assertEqual(envelope.parse_reference(row["reference"]),
                         {"direction": envelope.SUPERVISOR_TO_PARENT,
                          "purpose": "scope_correction",
                          "messageId": envelope.message_id(
                              direction=envelope.SUPERVISOR_TO_PARENT,
                              relation_id=self._edge["linkId"], purpose="scope_correction",
                              subject="d-one"),
                          "correlationId": "msg-1"})

    def test_a_purpose_and_a_hand_written_reference_together_are_refused(self):
        from codex_session_relay import cli

        with self.assertRaises(cli.SystemExit2):
            self.record(purpose="scope_correction", reference="see Tuesday")

    def test_one_digest_cannot_answer_two_messages(self):
        """Same purpose, different correlation, and the replay handed back the old answer.

        The stored id carries neither, so without comparing the correlation the caller got a
        directive that answers the message theirs replaced.
        """
        self.record(purpose="scope_correction", correlation="msg-1")
        refusal = self.assertRefused(
            RefusalReason.LINK_CONFLICT,
            lambda: self.record(purpose="scope_correction", correlation="msg-2"))
        self.assertIn("cannot answer two messages", refusal.detail)
        stored = self.store.one(
            "SELECT reference FROM scope_directives WHERE digest = ?", ("d-one",))["reference"]
        self.assertEqual(envelope.parse_reference(stored)["correlationId"], "msg-1",
                         "the instruction already recorded is preserved")

    def test_replaying_the_same_correlation_still_converges(self):
        first = self.record(purpose="scope_correction", correlation="msg-1")
        self.assertEqual(
            self.record(purpose="scope_correction", correlation="msg-1")["directiveId"],
            first["directiveId"])

    def test_a_legacy_row_does_not_swallow_the_pointer_a_caller_asked_for(self):
        """It used to report success while the purpose and correlation vanished.

        A row written before this contract has no room for them, and the derived id does not
        carry them either, so returning that row answered a request it had not honoured.
        """
        self.record(reference="see the thread from Tuesday")
        refusal = self.assertRefused(
            RefusalReason.LINK_CONFLICT,
            lambda: self.record(purpose="scope_correction", correlation="msg-1"))
        self.assertIn("nowhere to go", refusal.detail)
        self.assertEqual(
            self.store.one("SELECT reference FROM scope_directives WHERE digest = ?",
                           ("d-one",))["reference"], "see the thread from Tuesday",
            "and the instruction already recorded is preserved rather than rewritten")

    def test_a_caller_asserting_no_purpose_still_converges_on_the_stored_row(self):
        """It claims nothing the row could contradict, so this is an ordinary replay."""
        first = self.record(purpose="scope_correction", correlation="msg-1")
        self.assertEqual(self.record()["directiveId"], first["directiveId"])
        self.assertEqual(self.record(reference="an operator note")["directiveId"],
                         first["directiveId"])


class TheRenderedMessage(DeliveryTestCase):
    def recorded(self, **overrides):
        _relationship, event_id = self.queued_event()
        report.record(self.store, self.clock, event_id=event_id, **a_report(**overrides))
        return event_id

    def test_a_candidate_for_review_says_what_it_is_and_which_message_it_is(self):
        """A ready_for_review receipt is delivered as review_ready, not as a completion."""
        event_id = self.recorded()
        message = self.delivery.render_message(event_id)
        self.assertIn("message: request", message)
        self.assertIn("child_to_parent/review_ready", message)
        self.assertNotIn("child_to_parent/completion", message)
        self.assertIn("envelope: " + envelope.VERSION, message)
        self.assertIn("an answer is owed by the recipient", message)

    def test_the_message_id_in_the_bytes_is_the_one_a_reader_derives(self):
        event_id = self.recorded()
        row = self.delivery.get(event_id)
        expected = envelope.message_id(
            direction=envelope.CHILD_TO_PARENT, relation_id=row["relationship_id"],
            purpose="review_ready", subject=event_id)
        self.assertIn(f"messageId: {expected}", self.delivery.render_message(event_id))

    def test_the_sender_and_the_scope_are_read_rather_than_asserted(self):
        event_id = self.recorded()
        message = self.delivery.render_message(event_id)
        self.assertIn("from: child 01child-task", message)
        self.assertIn("to: parent 01parent-task", message)

    def test_a_renderer_with_no_scope_row_says_so_instead_of_naming_one(self):
        event_id = self.recorded()
        row = self.delivery.get(event_id)
        stored = report.read(self.store, event_id)
        receipt = self.intake.get(event_id)
        bare = report.render_completion(row, receipt, "del-x-a1", stored)
        self.assertIn("<unknown", bare, "an unread field says unknown rather than nothing")

    def test_what_the_message_asks_survives_a_budget_that_drops_everything_else(self):
        event_id = self.recorded()
        row = self.delivery.get(event_id)
        stored = report.read(self.store, event_id)
        receipt = self.intake.get(event_id)
        tight = report.render_completion(row, receipt, "del-x-a1", stored, budget=2300)
        self.assertIn("omitted:", tight, "this budget is supposed to cost something")
        self.assertIn("message: request", tight)
        self.assertIn("messageId:", tight)


class TheDirectiveSeam(LinkageTestCase):
    """What the row can authoritatively contradict, and what it must leave alone."""

    def directive(self, digest, reference):
        execution = getattr(self, "_edge", None) or self.supervise()
        self._edge = execution
        return self.linkage.record_directive(
            scope_kind=linkage_module.PROJECT, scope_key=PROJECT,
            from_task_id=SUPERVISOR_TASK, from_scope_key=INITIATIVE,
            link_id_value=execution["linkId"], digest=digest, reference=reference)

    def test_a_pointer_derived_from_this_row_is_stored_as_given(self):
        execution = self.supervise()
        self._edge = execution
        pointer = envelope.directive_reference(
            purpose="project_assignment", link_id=execution["linkId"], digest="d-one")
        row = self.directive("d-one", pointer)
        self.assertEqual(row["reference"], pointer)
        self.assertEqual(envelope.parse_reference(row["reference"])["purpose"],
                         "project_assignment")

    def test_a_pointer_belonging_to_another_instruction_is_refused_and_the_contest_kept(self):
        execution = self.supervise()
        self._edge = execution
        stolen = envelope.directive_reference(
            purpose="project_assignment", link_id=execution["linkId"], digest="d-other")
        self.assertRefused(RefusalReason.LINK_CONFLICT,
                           lambda: self.directive("d-one", stolen))
        self.assertTrue(
            [row for row in self.linkage.conflicts(linkage_module.PROJECT, PROJECT)
             if row.get("reason") == RefusalReason.LINK_CONFLICT.value],
            "a refusal that leaves no record is the contest disappearing with it")
        self.assertIsNone(
            self.store.one("SELECT directive_id FROM scope_directives WHERE digest = ?",
                           ("d-one",)),
            "and nothing was stored for the instruction that was refused")

    def test_a_column_that_is_not_ours_is_stored_unchanged(self):
        """The reference column is free-form and predates this contract."""
        execution = self.supervise()
        self._edge = execution
        row = self.directive("d-one", "see the thread from Tuesday")
        self.assertEqual(row["reference"], "see the thread from Tuesday")

    def test_a_directive_written_before_this_contract_reads_as_unknown(self):
        execution = self.supervise()
        self._edge = execution
        row = self.directive("d-one", None)
        self.assertIsNone(row["reference"])
        self.assertIsNone(envelope.parse_reference(row["reference"]))

    def test_one_digest_cannot_be_two_instructions(self):
        """The stored id does not carry the purpose, so the pointer is what tells them apart.

        Returning the first row for the second instruction answered a caller about somebody
        else's directive, which is the same silent collapse this module refuses everywhere.
        """
        execution = self.supervise()
        self._edge = execution
        assignment = envelope.directive_reference(
            purpose="project_assignment", link_id=execution["linkId"], digest="d-one")
        self.directive("d-one", assignment)
        correction = envelope.directive_reference(
            purpose="scope_correction", link_id=execution["linkId"], digest="d-one")
        self.assertRefused(RefusalReason.LINK_CONFLICT,
                           lambda: self.directive("d-one", correction))
        self.assertEqual(
            self.store.one("SELECT reference FROM scope_directives WHERE digest = ?",
                           ("d-one",))["reference"], assignment,
            "the instruction that was already recorded is preserved")
        self.assertTrue(
            [row for row in self.linkage.conflicts(linkage_module.PROJECT, PROJECT)
             if row.get("reason") == RefusalReason.LINK_CONFLICT.value])

    def test_replaying_the_same_instruction_still_converges(self):
        execution = self.supervise()
        self._edge = execution
        pointer = envelope.directive_reference(
            purpose="project_assignment", link_id=execution["linkId"], digest="d-one")
        first = self.directive("d-one", pointer)
        self.assertEqual(self.directive("d-one", pointer)["directiveId"],
                         first["directiveId"])

    def test_one_refusal_leaves_one_conflict_and_one_journal_row(self):
        """The refusal branch records the contest once, not once per nesting level.

        Raised in review against this path: the shared refusal branch at the end of the method
        binds to the outer test, which was already false here, so it does not run a second
        time. The journal has no uniqueness constraint, so a duplicate would be invisible in
        linkage_conflicts and visible only here.
        """
        execution = self.supervise()
        self._edge = execution
        self.directive("d-one", envelope.directive_reference(
            purpose="project_assignment", link_id=execution["linkId"], digest="d-one"))
        before = self.store.one(
            "SELECT COUNT(*) AS n FROM journal WHERE kind = 'linkage_refused'")["n"]
        self.assertRefused(RefusalReason.LINK_CONFLICT, lambda: self.directive(
            "d-one", envelope.directive_reference(
                purpose="scope_correction", link_id=execution["linkId"], digest="d-one")))
        after = self.store.one(
            "SELECT COUNT(*) AS n FROM journal WHERE kind = 'linkage_refused'")["n"]
        self.assertEqual(after - before, 1, "one refusal, one journal row")
        self.assertEqual(
            self.store.one("SELECT COUNT(*) AS n FROM linkage_conflicts"
                           " WHERE reason = ?", (RefusalReason.LINK_CONFLICT.value,))["n"], 1)


if __name__ == "__main__":
    unittest.main()
