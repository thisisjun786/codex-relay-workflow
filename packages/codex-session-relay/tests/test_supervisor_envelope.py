"""CRW-148: can a recipient tell what it was asked, and can a reader tell what was not said.

Every case here is written from the reader's side. The question is never whether a key exists
but whether somebody holding this envelope would act on something nobody established.
"""

import unittest

from codex_session_relay import envelope, identity, report
from codex_session_relay.errors import RefusalReason

from .support import DeliveryTestCase
from .test_report_contract import a_report

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
    def test_the_supervisor_direction_has_no_mechanism_rather_than_no_evidence(self):
        """not_applicable and unmeasured are different news for whoever is waiting.

        Unmeasured says go and look. not_applicable says there is nothing to look at, which
        for this direction is the whole point: the relay carries no supervisor channel, so a
        caller waiting for a supervisor acknowledgement waits forever.
        """
        ladder = envelope.unreached(envelope.PARENT_TO_SUPERVISOR)
        for name in envelope.STAGES:
            self.assertEqual(ladder[name]["state"], envelope.IMPOSSIBLE)
            self.assertIn("no supervisor message channel", ladder[name]["detail"])

    def test_the_correction_direction_has_a_transport_but_no_acknowledgement(self):
        ladder = envelope.unreached(envelope.PARENT_TO_CHILD)
        self.assertEqual(ladder[envelope.TRANSPORT_ACCEPTED]["state"], envelope.UNMEASURED)
        self.assertEqual(ladder[envelope.RECEIVED]["state"], envelope.IMPOSSIBLE)
        self.assertIn("completion receipt", ladder[envelope.RECEIVED]["detail"])
        self.assertEqual(ladder[envelope.APPLIED]["state"], envelope.UNMEASURED)

    def test_silence_is_never_agreement_and_a_state_needs_a_source(self):
        ladder = envelope.unreached(envelope.CHILD_TO_PARENT)
        self.assertFalse(envelope.reached(ladder, envelope.AGREED))
        with self.assertRaises(envelope.EnvelopeRefused):
            envelope.stage(envelope.YES)
        self.assertEqual(
            envelope.stage(envelope.YES, source="acks")["source"], "acks")

    def test_a_conditional_acceptance_is_not_a_held_stage(self):
        ladder = envelope.unreached(envelope.CHILD_TO_PARENT)
        ladder[envelope.AGREED] = envelope.stage(
            envelope.CONDITIONAL, source="acks", detail="accepted subject to a follow-up")
        self.assertFalse(envelope.reached(ladder, envelope.AGREED))

    def test_a_stage_standing_on_one_that_does_not_hold_is_named(self):
        ladder = envelope.unreached(envelope.CHILD_TO_PARENT)
        ladder[envelope.TRANSPORT_ACCEPTED] = envelope.stage(envelope.YES, source="attempts")
        ladder[envelope.APPLIED] = envelope.stage(envelope.YES, source="verdicts")
        self.assertEqual(envelope.promotion_refused(ladder), [envelope.APPLIED])


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


class TheRenderedMessage(DeliveryTestCase):
    def recorded(self, **overrides):
        _relationship, event_id = self.queued_event()
        report.record(self.store, self.clock, event_id=event_id, **a_report(**overrides))
        return event_id

    def test_the_completion_says_what_it_is_and_which_message_it_is(self):
        event_id = self.recorded()
        message = self.delivery.render_message(event_id)
        self.assertIn("message: request", message)
        self.assertIn("child_to_parent/completion", message)
        self.assertIn("envelope: " + envelope.VERSION, message)
        self.assertIn("an answer is owed by the recipient", message)

    def test_the_message_id_in_the_bytes_is_the_one_a_reader_derives(self):
        event_id = self.recorded()
        row = self.delivery.get(event_id)
        expected = envelope.message_id(
            direction=envelope.CHILD_TO_PARENT, relation_id=row["relationship_id"],
            purpose="completion", subject=event_id)
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


if __name__ == "__main__":
    unittest.main()
