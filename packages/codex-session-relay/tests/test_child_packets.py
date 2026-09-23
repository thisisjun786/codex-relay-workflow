"""CRW-149: could a receiver act on this, and would it notice if it should not.

Every case is written from the receiver's side. relay-envelope/1 already answers what a
message is; what is tested here is whether the message carries what its own occasion cannot
do without, and whether a packet that disagrees with the record the receiver read for itself
is refused by field rather than accepted and discovered three rounds later.

The controls matter as much as the positives. A wrong parent, a stale generation, an old
criteria digest, a moved head, a refused model pair and a correction arriving twice each have
a case that must NOT pass, because a validator whose negatives were never run is a validator
nobody has tested.
"""

import inspect
import json
import unittest

from codex_session_relay import cxc, envelope, packets, report
from codex_session_relay.errors import RefusalReason

from .support import DeliveryTestCase
from .test_report_contract import a_report

RELATION = "rel-46d5b5ac690ef861"
PARENT = "01parent-task"
CHILD = "01child-task"
ISSUE = "CRW-149"
DIGEST = "d" * 64
HEAD = "a1b2c3d4e5f60718293a4b5c6d7e8f9012345678"
# The link revision the receiver reads for this relationship, and the dispatch request that
# opened its current generation, which is what a first assignment names.
REVISION = 3
DISPATCH = "dispatch-crw149-first"

BODY = chr(10).join((
    "TASK: land the typed packet contract",
    "SCOPE: packages/codex-session-relay and the crw-run references",
    "MUST DO: one pull request to dev, its CI and its review",
    "MUST NOT: merge it, install anything, or touch the main-owned surface",
    "PROOF: the relay suite green on the head you report",
    "RETURN FORMAT: result.json plus a final report naming that head",
    "DECISION BOUNDARY: routine implementation choices are yours; a scope change comes back",
))
# A correction says what it corrects, in its own form, and carries the evidence it rests on.
CORRECTION_BODY = chr(10).join((
    "VIOLATED CRITERION: criterion 5, the review on the current head is unresolved",
    "WHAT CHANGED: a finding arrived on the head the child reported",
    "FIX SCOPE: the file the finding names",
    "PRESERVE: the pull request, its branch and every passing check",
    "REVERIFY AND RETURN: rerun the suite on the new head and report review_ready",
))
CORRECTION_EVIDENCE = ["/state/crw/crw-149/evidence/finding.txt"]


def a_policy(**overrides):
    base = {"model": "anthropic/claude-opus-5", "effort": "xhigh",
            "workflow": "CXC Loop", "mode": packets.LOOP, "sandbox": "danger-full-access",
            "approval": "never"}
    base.update(overrides)
    return packets.policy(**base)


# The pair the parent runs on since 2026-09-23, and the one it left. A callback names the task
# to answer and the pair that task is authorised to run now, so the left pair is what a stale
# callback carries.
PARENT_PAIR = ("anthropic/claude-opus-5-5", "xhigh")
LEFT_PARENT_PAIR = ("devin/swe-2", "max")


def a_callback(**overrides):
    base = {"task_id": PARENT, "model": PARENT_PAIR[0], "effort": PARENT_PAIR[1]}
    base.update(overrides)
    return packets.callback(**base)


def an_assignment(**overrides):
    base = dict(
        direction=envelope.PARENT_TO_CHILD, purpose="assignment", relation_id=RELATION,
        sender=PARENT, recipient=CHILD, subject=ISSUE, issue=ISSUE,
        criteria_digest=DIGEST, policy_record=a_policy(), callback=a_callback(),
        body=BODY, relation_revision=REVISION,
    )
    base.update(overrides)
    return packets.compose(**base)


def a_first_assignment(**overrides):
    """The assignment as it is really sent: before the child exists.

    It cannot name the relationship, whose id is derived from the child's task id, or the
    recipient. It names the dispatch request registration binds, and states the recipient as
    an absence. The earlier round trip pre-filled a future child id here, which no parent can.
    """
    base = dict(relation_id=DISPATCH, relation_revision=None, recipient=envelope.absent(
        envelope.UNKNOWN, "creation has not returned the child's task id"))
    base.update(overrides)
    return an_assignment(**base)


def a_review_ready(**overrides):
    base = dict(
        direction=envelope.CHILD_TO_PARENT, purpose="review_ready", relation_id=RELATION,
        sender=CHILD, recipient=PARENT, subject="evt-1", issue=ISSUE, generation=1,
        criteria_digest=DIGEST, relation_revision=REVISION,
        artifact=packets.pull_request(repository="thisisjun786/codex-relay-workflow",
                                      number=107, head_sha=HEAD),
        evidence=["/state/crw/crw-149/evidence/suite.txt"],
    )
    base.update(overrides)
    return packets.compose(**base)


def a_record(**overrides):
    base = {"relationId": RELATION, "parentTaskId": PARENT, "childTaskId": CHILD,
            "issue": ISSUE, "generation": 1, "criteriaDigest": DIGEST, "headSha": HEAD,
            "repository": "thisisjun786/codex-relay-workflow", "prNumber": 107,
            "refusedPolicies": [], "relationRevision": REVISION, "relationStatus": "active",
            "dispatchRequestId": DISPATCH, "mode": packets.LOOP, "workflow": "CXC Loop",
            "policy": {"model": "anthropic/claude-opus-5", "effort": "xhigh",
                       "sandbox": {"type": "dangerFullAccess"}, "approval": "never"},
            "callback": a_callback()}
    base.update(overrides)
    return base


AUDIT = "/state/crw/crw-149/evidence/audit.md"


class WhatAnOccasionCannotDoWithout(unittest.TestCase):
    def test_every_parent_and_child_purpose_declares_its_required_data(self):
        """A purpose the envelope carries and this module forgot would validate nothing."""
        for direction in (envelope.PARENT_TO_CHILD, envelope.CHILD_TO_PARENT):
            for purpose in envelope.PURPOSES[direction]:
                with self.subTest(direction=direction, purpose=purpose):
                    self.assertIn((direction, purpose), packets.REQUIRED_BY_PURPOSE)
                    self.assertTrue(packets.required_for(direction, purpose))

    def test_an_assignment_without_its_criteria_digest_is_refused_by_name(self):
        with self.assertRaises(packets.PacketRefused) as caught:
            an_assignment(criteria_digest=None)
        self.assertIn(packets.CRITERIA_DIGEST, caught.exception.detail)

    def test_an_assignment_without_the_instruction_body_names_the_missing_sections(self):
        with self.assertRaises(packets.PacketRefused) as caught:
            an_assignment(body="TASK: do the thing")
        for name in ("SCOPE", "MUST DO", "MUST NOT", "PROOF", "RETURN FORMAT",
                     cxc.DECISION_BOUNDARY):
            self.assertIn(name, caught.exception.detail)

    def test_a_stated_absence_is_not_a_value(self):
        """The whole point of the three absences is that none of them fills a field."""
        with self.assertRaises(packets.PacketRefused) as caught:
            an_assignment(criteria_digest=envelope.absent(
                envelope.UNKNOWN, "nobody registered them"))
        self.assertIn(packets.CRITERIA_DIGEST, caught.exception.detail)

    def test_a_resume_that_drops_the_workflow_is_refused(self):
        """No transport carries it, so an unstated workflow is dropped rather than deferred."""
        with self.assertRaises(packets.PacketRefused) as caught:
            packets.policy(model="opus", effort="xhigh", workflow="", mode=packets.LOOP)
        self.assertIn("workflow", caught.exception.detail)

    def test_a_progress_note_is_not_made_to_invent_a_head(self):
        """Requiring an artifact from a child with nothing to show is how a value gets made up."""
        one = packets.compose(
            direction=envelope.CHILD_TO_PARENT, purpose="progress", relation_id=RELATION,
            sender=CHILD, recipient=PARENT, subject="evt-1", issue=ISSUE)
        self.assertEqual(one["envelope"]["kind"], envelope.NOTIFICATION)
        self.assertIsNone(one[packets.ARTIFACT])

    def test_a_blocked_report_owes_an_answer_and_a_decision_owes_the_user_one(self):
        blocked = packets.compose(
            direction=envelope.CHILD_TO_PARENT, purpose="blocked", relation_id=RELATION,
            sender=CHILD, recipient=PARENT, subject="evt-1", issue=ISSUE,
            evidence=["/state/crw/crw-149/evidence/blocked.txt"])
        self.assertEqual(blocked["envelope"]["answerOwedBy"], envelope.RECIPIENT)
        decision = packets.compose(
            direction=envelope.CHILD_TO_PARENT, purpose="decision_request",
            relation_id=RELATION, sender=CHILD, recipient=PARENT, subject="evt-1",
            issue=ISSUE, decision="whether to accept the known defect for this release",
            evidence=["/state/crw/crw-149/evidence/finding.txt"])
        self.assertEqual(decision["envelope"]["answerOwedBy"], envelope.USER)


class ANonPullRequestAudit(unittest.TestCase):
    def test_a_locator_and_a_digest_are_a_whole_artifact(self):
        one = a_review_ready(artifact=packets.locator(path=AUDIT, digest="9" * 64))
        self.assertEqual(one[packets.ARTIFACT]["kind"], packets.LOCATOR)
        self.assertEqual(
            packets.reception(one, a_record(artifactDigest="9" * 64,
                                            artifactPath=AUDIT))["disposition"],
            packets.ACCEPTED)

    def test_an_audit_is_never_measured_against_a_head_it_does_not_have(self):
        """The empty commit and the invented head are what this refuses to ask for."""
        one = a_review_ready(artifact=packets.locator(path=AUDIT, digest="9" * 64))
        answer = packets.reception(one, a_record(headSha="0" * 40, artifactDigest="9" * 64,
                                                 artifactPath=AUDIT))
        self.assertEqual(answer["disposition"], packets.ACCEPTED)
        self.assertEqual(answer["mismatches"], [])

    def test_a_deliverable_nobody_can_hash_is_not_one_this_can_identify(self):
        with self.assertRaises(packets.PacketRefused):
            packets.locator(path=AUDIT, digest=None)


class TheControlsThatMustNotPass(unittest.TestCase):
    """Each of these is a packet that reads perfectly and is about something else."""

    def refused(self, one, record, kind):
        answer = packets.reception(one, record)
        self.assertEqual(answer["disposition"], packets.REFUSAL, answer)
        self.assertIn(kind, [m["kind"] for m in answer["mismatches"]], answer)
        return answer

    def test_a_packet_from_another_parent_is_refused_on_the_registered_pair(self):
        """A self-description is not identity: the registered pair is what says who may send."""
        self.refused(an_assignment(sender="01someone-else"), a_record(), packets.WRONG_SENDER)

    def test_a_packet_addressed_to_another_child_is_not_this_child_instruction(self):
        self.refused(an_assignment(recipient="01another-child"), a_record(),
                     packets.WRONG_RECIPIENT)

    def test_a_packet_naming_another_relationship_is_refused(self):
        self.refused(an_assignment(relation_id="rel-000000000000beef"), a_record(),
                     packets.WRONG_RELATION)

    def test_a_stale_generation_is_refused_rather_than_worked_under(self):
        """A receipt emitted under it would be refused, so the work would have nowhere to go."""
        self.refused(a_review_ready(generation=1), a_record(generation=2),
                     packets.STALE_GENERATION)

    def test_an_old_criteria_digest_is_refused(self):
        self.refused(a_review_ready(criteria_digest="0" * 64), a_record(),
                     packets.STALE_CRITERIA)

    def test_a_head_that_moved_after_the_packet_was_written_is_refused(self):
        answer = self.refused(a_review_ready(), a_record(headSha="f" * 40), packets.STALE_HEAD)
        self.assertIn("another commit", answer["mismatches"][0]["reason"])

    def test_a_refused_model_and_effort_pair_is_a_settings_answer_not_a_provider_failure(self):
        record = a_record(refusedPolicies=[{
            "model": "anthropic/claude-opus-5", "effort": "xhigh",
            "reason": "this pair is recorded refused for the child role"}])
        answer = self.refused(an_assignment(), record, packets.REFUSED_SETTINGS)
        self.assertIn("refused", answer["mismatches"][0]["reason"])

    def test_a_superseded_relationship_revision_is_refused(self):
        one = an_assignment(relation_revision=3)
        self.refused(one, a_record(relationRevision=4), packets.SUPERSEDED_RELATION)

    def test_a_record_that_cannot_answer_is_unavailable_and_not_accepted(self):
        """The one that matters. Unchecked is not checked, however complete the packet reads."""
        blind = a_record()
        del blind["generation"]
        answer = packets.reception(a_review_ready(), blind)
        self.assertEqual(answer["disposition"], packets.UNAVAILABLE)
        self.assertEqual([m["kind"] for m in answer["gaps"]], [packets.UNREADABLE])
        self.assertEqual(answer["mismatches"], [])

    def test_an_unread_settings_record_leaves_the_policy_unchecked_rather_than_approved(self):
        blind = a_record()
        del blind["refusedPolicies"]
        self.assertEqual(packets.reception(an_assignment(), blind)["disposition"],
                         packets.UNAVAILABLE)

    def test_a_packet_that_agrees_with_the_record_is_accepted(self):
        answer = packets.reception(a_review_ready(), a_record())
        self.assertEqual(answer["disposition"], packets.ACCEPTED)
        self.assertEqual((answer["mismatches"], answer["gaps"]), ([], []))


class ACorrectionArrivingTwice(unittest.TestCase):
    def correction(self, **overrides):
        base = dict(
            direction=envelope.PARENT_TO_CHILD, purpose="revision_request",
            relation_id=RELATION, sender=PARENT, recipient=CHILD, subject="evt-1",
            issue=ISSUE, generation=2, criteria_digest=DIGEST,
            callback=a_callback(), body=CORRECTION_BODY, evidence=CORRECTION_EVIDENCE,
            artifact=packets.pull_request(repository="thisisjun786/codex-relay-workflow",
                                          number=107, head_sha=HEAD))
        base.update(overrides)
        return packets.compose(**base)

    def test_the_same_correction_twice_is_answered_once(self):
        one = self.correction()
        answered = {one["envelope"]["messageId"]: {
            "contentDigest": packets.content_digest(one), "disposition": packets.ACCEPTED}}
        again = packets.repeat(one, answered)
        self.assertEqual(again["state"], packets.REPLAY)
        self.assertEqual(again["disposition"], packets.ACCEPTED)

    def test_one_id_asking_for_something_else_is_raised_rather_than_given_the_old_answer(self):
        """Otherwise a decision made about one request is applied to a request nobody made."""
        first = self.correction()
        answered = {first["envelope"]["messageId"]: {
            "contentDigest": packets.content_digest(first), "disposition": packets.ACCEPTED}}
        second = self.correction(generation=3)
        self.assertEqual(packets.repeat(second, answered)["state"], packets.COLLISION)

    def test_a_first_arrival_is_neither_of_those(self):
        self.assertEqual(packets.repeat(self.correction(), {})["state"], packets.FIRST)


class TheSevenStatesOfAHandover(unittest.TestCase):
    def test_read_has_no_mechanism_and_says_so(self):
        """No row says a recipient read anything, and prose saying so is prose."""
        ladder = packets.unobserved()
        self.assertEqual(ladder[packets.READ]["state"], envelope.IMPOSSIBLE)
        self.assertIn("no row in this store", ladder[packets.READ]["detail"])

    def test_each_state_is_answered_by_the_record_that_actually_holds_it(self):
        ladder = packets.unobserved()
        ladder[packets.RELAY_ACK] = envelope.stage(envelope.YES, source="attempts")
        with self.assertRaises(packets.PacketRefused) as caught:
            packets.check_progression(ladder)
        self.assertIn("answered by acks", caught.exception.detail)

    def test_an_acceptance_standing_over_an_unmeasured_verdict_is_a_promotion(self):
        ladder = packets.unobserved()
        ladder[packets.TRANSPORT_ACCEPTED] = envelope.stage(envelope.YES, source="attempts")
        ladder[packets.RELAY_ACK] = envelope.stage(envelope.YES, source="acks")
        ladder[packets.PARENT_ACCEPTANCE] = envelope.stage(envelope.YES, source="verdicts")
        self.assertEqual(packets.unsupported_promotions(ladder), [packets.PARENT_ACCEPTANCE])

    def test_a_complete_ladder_promotes_nothing(self):
        ladder = packets.unobserved()
        for name, source in ((packets.TRANSPORT_ACCEPTED, "attempts"),
                             (packets.RELAY_ACK, "acks"),
                             (packets.CRITERIA_VERDICT, "verdicts"),
                             (packets.PARENT_ACCEPTANCE, "verdicts"),
                             (packets.MERGE_LANDING, "merge_turns"),
                             (packets.LINEAR_DONE, "linear_issue_status")):
            ladder[name] = envelope.stage(envelope.YES, source=source)
        self.assertEqual(packets.unsupported_promotions(ladder), [])

    def test_every_state_renders_separately_rather_than_as_one_word(self):
        lines = packets.progression_lines(packets.unobserved())
        for name in packets.PROGRESSION:
            self.assertTrue(any(name in line for line in lines), name)


class WhetherTheChildActuallyArmedAnything(unittest.TestCase):
    def triple(self, **overrides):
        one = packets.unexamined(packets.LOOP)
        one.update(overrides)
        return one

    def test_the_three_facts_start_unverified_rather_than_absent(self):
        """Nobody has looked is not the same as nothing is there."""
        reading = packets.unexamined(packets.LOOP)
        for name in packets.ACTIVATION_FACTS:
            self.assertEqual(reading[name]["state"], packets.UNVERIFIED, name)
        self.assertEqual(reading[packets.MODE], packets.LOOP)

    def test_an_answered_fact_names_the_record_that_answered_it(self):
        with self.assertRaises(packets.PacketRefused):
            packets.activation_fact(packets.OBSERVED)

    def test_an_assignment_that_never_carried_the_invocation_is_l0(self):
        one = self.triple(instructed=packets.activation_fact(
            packets.ABSENT, source="the dispatched prompt read back from the task"))
        self.assertEqual(packets.activation_class(one)["class"], "L0")

    def test_a_recorded_refusal_is_l2_and_not_a_negative_reading(self):
        one = self.triple(activated=packets.activation_fact(
            packets.REFUSED, source="the binding refusal the child recorded"))
        self.assertEqual(packets.activation_class(one)["class"], "L2")

    def test_an_active_goal_with_no_goalplan_is_l3(self):
        one = self.triple(
            instructed=packets.activation_fact(packets.OBSERVED, source="the prompt"),
            activated=packets.activation_fact(packets.ABSENT, source="orchestrate status"),
            nativeGoal=packets.activation_fact(packets.OBSERVED, source="get_goal"))
        self.assertEqual(packets.activation_class(one)["class"], "L3")

    def test_activation_seen_earlier_and_gone_now_is_l4_rather_than_l1(self):
        """Opening a second goal for one assignment reads from outside as a duplicate."""
        earlier = self.triple(activated=packets.activation_fact(
            packets.OBSERVED, source="the bound goalplan"))
        now = self.triple(
            instructed=packets.activation_fact(packets.OBSERVED, source="the prompt"),
            activated=packets.activation_fact(packets.ABSENT, source="orchestrate status"))
        self.assertEqual(packets.activation_class(now, earlier=earlier)["class"], "L4")

    def test_a_single_negative_reading_on_its_own_is_l6(self):
        """A child inside its first turn reads exactly like one that never armed anything."""
        one = self.triple(activated=packets.activation_fact(
            packets.ABSENT, source="orchestrate status"))
        self.assertEqual(packets.activation_class(one)["class"], "L6")

    def test_a_coordination_parent_is_not_read_as_an_unarmed_child(self):
        """It schedules on its own goal and persists no implementation FSM by design."""
        one = packets.unexamined(packets.COORDINATION)
        self.assertEqual(one[packets.ACTIVATED]["state"], packets.INAPPLICABLE)
        self.assertEqual(
            packets.activation_class(one, mode=packets.COORDINATION)["class"], "L5")

    def test_an_authorised_non_loop_assignment_is_not_a_defect_either(self):
        one = packets.unexamined(packets.NON_LOOP)
        self.assertEqual(packets.activation_class(one, mode=packets.NON_LOOP)["class"], "L5")

    def test_a_packet_carrying_a_partial_triple_is_refused(self):
        with self.assertRaises(packets.PacketRefused):
            an_assignment(activation={packets.INSTRUCTED: packets.activation_fact(
                packets.OBSERVED, source="the prompt")})


class TheInstructionBody(unittest.TestCase):
    def test_a_complete_body_is_missing_nothing(self):
        self.assertEqual(cxc.dispatch_problems(BODY), [])

    def test_a_section_mentioned_in_a_sentence_does_not_satisfy_it(self):
        """Grep over a prompt finds a word. It does not find an instruction."""
        prose = "This packet has a TASK: and a SCOPE: described in the paragraph above."
        self.assertEqual(len(cxc.dispatch_problems(prose)), len(cxc.DISPATCH_SECTIONS))

    def test_a_subsection_does_not_answer_for_the_section(self):
        self.assertIn("TASK", cxc.dispatch_problems("SUBTASK: something narrower"))

    def test_list_and_heading_markers_are_stepped_over(self):
        marked = chr(10).join("- " + line for line in BODY.splitlines())
        self.assertEqual(cxc.dispatch_problems(marked), [])

    def test_something_that_is_not_text_is_missing_everything(self):
        self.assertEqual(cxc.dispatch_problems(None), list(cxc.DISPATCH_SECTIONS))

    def test_bare_headings_with_nothing_under_them_name_every_field_and_instruct_nobody(self):
        headings = chr(10).join(name + ":" for name in cxc.DISPATCH_SECTIONS)
        self.assertEqual(cxc.dispatch_problems(headings), list(cxc.DISPATCH_SECTIONS))

    def test_content_on_the_line_below_the_heading_counts(self):
        body = chr(10).join(
            line for name in cxc.DISPATCH_SECTIONS
            for line in (name + ":", "  what this section actually says"))
        self.assertEqual(cxc.dispatch_problems(body), [])

    def test_a_heading_whose_only_follower_is_the_next_heading_is_still_empty(self):
        body = "TASK:" + chr(10) + "SCOPE: the references" + chr(10)
        self.assertIn("TASK", cxc.dispatch_problems(body))
        self.assertNotIn("SCOPE", cxc.dispatch_problems(body))


class TheRestoreSectionOnARealReport(DeliveryTestCase):
    def recorded(self, **overrides):
        _relationship, event_id = self.queued_event()
        report.record(self.store, self.clock, event_id=event_id, **a_report(**overrides))
        return event_id

    def test_a_restore_section_that_drops_the_workflow_is_refused(self):
        error = self.assertRefused(
            RefusalReason.MALFORMED_RECEIPT,
            lambda: self.recorded(restore={"phase": "C", "plan": "devlog/_plan/x"}))
        self.assertIn("mode", error.detail)

    def test_a_restore_section_whose_values_were_all_blank_is_still_no_section(self):
        """Unchanged. A rule about what a block must say is not a rule about an absent one."""
        event_id = self.recorded(restore={"mode": "   ", "scope": None})
        self.assertEqual(report.read(self.store, event_id)["restore"], {})

    def test_a_restore_section_stating_the_workflow_is_accepted(self):
        event_id = self.recorded(restore={"mode": "CXC Loop", "phase": "C"})
        self.assertEqual(report.read(self.store, event_id)["restore"]["mode"], "CXC Loop")


class WhichChildMessageThisActuallyIs(unittest.TestCase):
    def test_a_blocked_outcome_no_longer_renders_as_a_completion(self):
        self.assertEqual(report.child_purpose("blocked_needs_input"), "blocked")

    def test_it_reads_the_receipt_outcome_and_nothing_that_can_move(self):
        """The purpose feeds the message id, which is promised to stay put across retries.

        An earlier version also read whether a merge-readiness handoff had been recorded.
        That is not a property of the event: a resubmission can add one, so the same event
        would derive a second id and the recipient would owe two obligations where one fact
        happened. Taking only the outcome is what keeps the identity still.
        """
        self.assertEqual(report.child_purpose("ready_for_review"), "review_ready")
        self.assertEqual(
            len(inspect.signature(report.child_purpose).parameters), 1,
            "a second input is a second thing that can move the message id")

    def test_every_derived_purpose_is_one_the_envelope_carries(self):
        for outcome in ("ready_for_review", "blocked_needs_input", "failed", "interrupted"):
            with self.subTest(outcome=outcome):
                envelope.kind_of(envelope.CHILD_TO_PARENT, report.child_purpose(outcome))


class TheWholeRoundTrip(unittest.TestCase):
    """Assignment, review-ready, correction, resubmission, acceptance, integration.

    Replayed read-only against records held in memory. That is evidence about this contract
    and about nothing else: no store is opened, no delivery is claimed, no acknowledgement row
    is written, and none of this says a message reached any task on any host.
    """

    def accepted(self, one, record):
        answer = packets.reception(one, record)
        self.assertEqual(answer["disposition"], packets.ACCEPTED, answer)
        return answer

    def test_the_round_trip_stays_on_one_task_one_pull_request_and_one_callback(self):
        record = a_record()
        callback = a_callback()

        # The first message is written before the child exists, and is taken up once
        # registration has bound its dispatch to this relationship.
        assignment = a_first_assignment(callback=callback)
        self.accepted(assignment, record)

        candidate = packets.pull_request(
            repository="thisisjun786/codex-relay-workflow", number=107, head_sha=HEAD)
        ready = a_review_ready(artifact=candidate)
        self.accepted(ready, record)

        # The parent rules needs_changes, which is what OPENS the next generation. A
        # correction naming the current one names the generation the child has just stopped
        # working in, so the record moves with the verdict rather than after it.
        record = a_record(generation=2)
        correction = packets.compose(
            direction=envelope.PARENT_TO_CHILD, purpose="revision_request",
            relation_id=RELATION, sender=PARENT, recipient=CHILD, subject="evt-1",
            issue=ISSUE, generation=2, criteria_digest=DIGEST, callback=callback,
            artifact=candidate, relation_revision=REVISION, body=CORRECTION_BODY,
            evidence=CORRECTION_EVIDENCE)
        self.accepted(correction, record)

        # The same child, the same pull request, a new head. Nothing here creates a second
        # writer: the resubmission carries the relationship and the child the assignment did.
        moved = "b2c3d4e5f60718293a4b5c6d7e8f90123456789a"
        record = a_record(generation=2, headSha=moved)
        again = a_review_ready(generation=2, artifact=packets.pull_request(
            repository="thisisjun786/codex-relay-workflow", number=107, head_sha=moved))
        self.accepted(again, record)
        self.assertEqual(again["envelope"]["recipient"]["taskId"],
                         assignment["envelope"]["sender"]["taskId"])
        self.assertEqual(again["envelope"]["relationId"], correction["envelope"]["relationId"])

        # And the report from before the correction is not reusable at the new head, which is
        # the reading that would otherwise hand the parent a readiness claim about a commit
        # nobody is looking at any more.
        stale = packets.reception(ready, record)
        self.assertEqual(stale["disposition"], packets.REFUSAL)
        self.assertEqual(sorted(m["kind"] for m in stale["mismatches"]),
                         [packets.STALE_GENERATION, packets.STALE_HEAD])

        acceptance = packets.compose(
            direction=envelope.PARENT_TO_CHILD, purpose="acceptance", relation_id=RELATION,
            sender=PARENT, recipient=CHILD, subject="evt-1", issue=ISSUE,
            criteria_digest=DIGEST, relation_revision=REVISION, artifact=packets.pull_request(
                repository="thisisjun786/codex-relay-workflow", number=107, head_sha=moved))
        self.accepted(acceptance, record)
        self.assertEqual(acceptance["envelope"]["kind"], envelope.NOTIFICATION)
        # The absence reason is pinned rather than the predicate. is_absent folds more than
        # one input, so asserting it would measure a property with a summary and would owe
        # the regression map a declared verdict; naming the reason is also the stronger claim.
        self.assertEqual(acceptance["envelope"]["answerOwedBy"]["absent"],
                         envelope.NOT_APPLICABLE)

    def test_the_states_advance_one_at_a_time_and_nothing_promotes_them(self):
        """Six readings of the same handover, each adding exactly the record it earned."""
        ladder = packets.unobserved()
        for name, source in ((packets.TRANSPORT_ACCEPTED, "attempts"),
                             (packets.RELAY_ACK, "acks"),
                             (packets.CRITERIA_VERDICT, "verdicts"),
                             (packets.PARENT_ACCEPTANCE, "verdicts"),
                             (packets.MERGE_LANDING, "merge_turns"),
                             (packets.LINEAR_DONE, "linear_issue_status")):
            with self.subTest(state=name):
                self.assertEqual(packets.unsupported_promotions(ladder), [])
                ladder[name] = envelope.stage(envelope.YES, source=source)
        self.assertEqual(packets.unsupported_promotions(ladder), [])
        self.assertEqual(ladder[packets.READ]["state"], envelope.IMPOSSIBLE)

    def test_a_send_the_transport_took_is_not_an_acknowledgement(self):
        """The one collapse this ladder exists to prevent."""
        ladder = packets.unobserved()
        ladder[packets.TRANSPORT_ACCEPTED] = envelope.stage(envelope.YES, source="attempts")
        self.assertEqual(ladder[packets.RELAY_ACK]["state"], envelope.UNMEASURED)
        self.assertEqual(ladder[packets.PARENT_ACCEPTANCE]["state"], envelope.UNMEASURED)

    def test_a_non_pull_request_audit_completes_the_same_round_trip(self):
        """No pull request is opened to fill a field, and nothing is refused for its absence."""
        record = a_record(artifactDigest="9" * 64, artifactPath=AUDIT)
        del record["headSha"]
        del record["repository"]
        del record["prNumber"]
        artifact = packets.locator(path=AUDIT, digest="9" * 64)
        self.accepted(a_review_ready(artifact=artifact), record)
        self.accepted(packets.compose(
            direction=envelope.PARENT_TO_CHILD, purpose="acceptance", relation_id=RELATION,
            sender=PARENT, recipient=CHILD, subject="evt-1", issue=ISSUE,
            criteria_digest=DIGEST, artifact=artifact, relation_revision=REVISION), record)


class APacketNobodyConstructed(unittest.TestCase):
    """Every rule that lived only in a constructor was a rule the disk path did not have.

    compose builds a packet here, where each constructor has run. packet-check reads one back
    from JSON, where none of them has. These are the cases that separate the two, and every
    one of them passed before the entry point was made to validate for itself.
    """

    def loaded(self, one, **changes):
        """The same packet as it comes back from disk, with a field changed on the way."""
        text = json.dumps(one)
        back = json.loads(text)
        back.update(changes)
        return back

    def test_a_packet_missing_required_data_is_refused_rather_than_compared(self):
        """Comparing the fields that are there says nothing about the ones that are not."""
        thin = self.loaded(a_review_ready(), evidence=[])
        with self.assertRaises(packets.PacketRefused) as caught:
            packets.reception(thin, a_record())
        self.assertIn(packets.EVIDENCE, caught.exception.detail)

    def test_an_artifact_missing_half_its_identity_is_refused(self):
        broken = self.loaded(a_review_ready())
        del broken[packets.ARTIFACT]["headSha"]
        with self.assertRaises(packets.PacketRefused) as caught:
            packets.reception(broken, a_record())
        self.assertIn("headSha", caught.exception.detail)

    def test_a_version_nobody_mapped_is_diagnosed_rather_than_read_under_these_rules(self):
        future = self.loaded(a_review_ready(), version="relay-packet/2")
        with self.assertRaises(packets.PacketRefused) as caught:
            packets.reception(future, a_record())
        self.assertIn("relay-packet/2", caught.exception.detail)

    def test_an_activation_fact_claiming_a_state_with_no_record_is_refused(self):
        one = self.loaded(an_assignment(activation=packets.unexamined(packets.LOOP)))
        one["activation"][packets.ACTIVATED] = {"state": packets.OBSERVED, "source": None}
        with self.assertRaises(packets.PacketRefused):
            packets.reception(one, a_record())

    def test_an_activation_state_nobody_defined_is_refused(self):
        one = self.loaded(an_assignment(activation=packets.unexamined(packets.LOOP)))
        one["activation"][packets.ACTIVATED] = {"state": "probably", "source": "a transcript"}
        with self.assertRaises(packets.PacketRefused):
            packets.reception(one, a_record())

    def test_a_packet_that_is_not_an_object_is_refused_rather_than_raised_through(self):
        """A validator that crashes on malformed input is not validating it."""
        for shape in ([], "a packet", 7, None):
            with self.subTest(shape=type(shape).__name__):
                with self.assertRaises(packets.PacketRefused):
                    packets.reception(shape, a_record())

    def test_a_nested_field_of_the_wrong_shape_is_refused_by_name(self):
        for field, value in (("envelope", []), (packets.POLICY, "opus/xhigh"),
                             ("activation", [])):
            with self.subTest(field=field):
                broken = self.loaded(an_assignment(), **{field: value})
                with self.assertRaises(packets.PacketRefused):
                    packets.reception(broken, a_record())

    def test_a_typed_field_of_the_wrong_shape_is_refused_rather_than_compared(self):
        """Two wrong answers compare equal, and the reading comes back agreed."""
        for field, value in ((packets.GENERATION, {"n": 1}), (packets.ISSUE, 149),
                             (packets.CRITERIA_DIGEST, ["d"]), (packets.GENERATION, True)):
            with self.subTest(field=field, shape=type(value).__name__):
                broken = self.loaded(a_review_ready(), **{field: value})
                with self.assertRaises(packets.PacketRefused):
                    packets.reception(broken, a_record(**{field: value}))

    def test_an_evidence_entry_nobody_can_follow_is_refused(self):
        broken = self.loaded(a_review_ready(), evidence=[{"path": "somewhere"}])
        with self.assertRaises(packets.PacketRefused):
            packets.reception(broken, a_record())

    def test_an_envelope_version_nobody_mapped_is_refused(self):
        broken = self.loaded(a_review_ready())
        broken["envelope"]["version"] = "relay-envelope/2"
        with self.assertRaises(packets.PacketRefused) as caught:
            packets.reception(broken, a_record())
        self.assertIn("relay-envelope/2", caught.exception.detail)

    def test_a_handover_ladder_of_the_wrong_shape_is_refused_rather_than_replaced(self):
        """Falling back on falsiness answered for a reading nobody checked."""
        for shape in ({}, [], "unobserved"):
            with self.subTest(shape=type(shape).__name__):
                with self.assertRaises(packets.PacketRefused):
                    packets.unsupported_promotions(shape)


class TheRegionFieldsNobodyGetsToWrite(unittest.TestCase):
    """Three of them are derived, so they are recomputed rather than read.

    A comparison of copies would catch nothing here: the packet carries no duplicate of
    anything, it carries values computed from the direction, relation, purpose and subject.
    """

    def loaded(self, one):
        return json.loads(json.dumps(one))

    def test_a_relabelled_kind_is_refused(self):
        """Otherwise a notification arrives shaped like something that owes an answer."""
        one = self.loaded(a_review_ready())
        one["envelope"]["kind"] = envelope.NOTIFICATION
        with self.assertRaises(packets.PacketRefused) as caught:
            packets.reception(one, a_record())
        self.assertIn("derived rather than declared", caught.exception.detail)

    def test_a_role_a_caller_wrote_for_itself_is_refused(self):
        one = self.loaded(a_review_ready())
        one["envelope"]["sender"]["role"] = "parent"
        with self.assertRaises(packets.PacketRefused) as caught:
            packets.reception(one, a_record())
        self.assertIn("a direction fixes both roles", caught.exception.detail)

    def test_a_message_id_belonging_to_another_message_is_refused(self):
        """It keys the replay reading, so a writable one can be spent in advance."""
        one = self.loaded(a_review_ready())
        one["envelope"]["messageId"] = envelope.message_id(
            direction=envelope.CHILD_TO_PARENT, relation_id=RELATION,
            purpose="review_ready", subject="evt-someone-else")
        with self.assertRaises(packets.PacketRefused) as caught:
            packets.reception(one, a_record())
        self.assertIn("belongs to another message", caught.exception.detail)


class ADifferentArtifactUnderTheSameHead(unittest.TestCase):
    def refused(self, artifact, field):
        answer = packets.reception(a_review_ready(artifact=artifact), a_record())
        self.assertEqual(answer["disposition"], packets.REFUSAL, answer)
        self.assertIn(field, [m["field"] for m in answer["mismatches"]], answer)

    def test_another_repository_on_the_same_commit_is_refused(self):
        """The same number on two projects is two different pull requests."""
        self.refused(packets.pull_request(repository="thisisjun786/somewhere-else",
                                          number=107, head_sha=HEAD), "artifact.repository")

    def test_another_pull_request_on_the_same_commit_is_refused(self):
        self.refused(packets.pull_request(repository="thisisjun786/codex-relay-workflow",
                                          number=999, head_sha=HEAD), "artifact.number")

    def test_another_deliverable_with_the_same_digest_is_refused(self):
        answer = packets.reception(
            a_review_ready(artifact=packets.locator(path="/state/elsewhere.md",
                                                    digest="9" * 64)),
            a_record(artifactPath=AUDIT, artifactDigest="9" * 64))
        self.assertEqual(answer["disposition"], packets.REFUSAL)
        self.assertIn("artifact.path", [m["field"] for m in answer["mismatches"]])


class AModeThatCannotAnswerThatWay(unittest.TestCase):
    def test_a_loop_reading_cannot_call_its_activation_inapplicable(self):
        """Accepting it read an unarmed loop as a working one, which hides a defect."""
        one = packets.unexamined(packets.LOOP)
        one[packets.ACTIVATED] = packets.activation_fact(
            packets.INAPPLICABLE, detail="borrowed from a parent's reading")
        with self.assertRaises(packets.PacketRefused) as caught:
            packets.activation_class(one, mode=packets.LOOP)
        self.assertIn("arms nothing", caught.exception.detail)


class CurrencyTheReceiverCouldNotRead(unittest.TestCase):
    def test_a_pull_request_against_a_record_with_no_head_is_unavailable(self):
        """An unchecked head and a matching head are not the same news."""
        blind = a_record()
        del blind["headSha"]
        answer = packets.reception(a_review_ready(), blind)
        self.assertEqual(answer["disposition"], packets.UNAVAILABLE)
        self.assertIn("artifact.headSha", [g["field"] for g in answer["gaps"]])

    def test_a_locator_against_a_record_with_no_digest_is_unavailable(self):
        answer = packets.reception(
            a_review_ready(artifact=packets.locator(path="/state/x.md", digest="9" * 64)),
            a_record())
        self.assertEqual(answer["disposition"], packets.UNAVAILABLE)
        self.assertIn("artifact.digest", [g["field"] for g in answer["gaps"]])


class WhatMakesTwoPacketsTheSameInstruction(unittest.TestCase):
    """A digest built from a hand-picked list is how a changed packet becomes a replay."""

    def correction(self, **overrides):
        base = dict(
            direction=envelope.PARENT_TO_CHILD, purpose="revision_request",
            relation_id=RELATION, sender=PARENT, recipient=CHILD, subject="evt-1",
            issue=ISSUE, generation=2, criteria_digest=DIGEST,
            callback=a_callback(), body=CORRECTION_BODY, evidence=CORRECTION_EVIDENCE,
            artifact=packets.pull_request(repository="thisisjun786/codex-relay-workflow",
                                          number=107, head_sha=HEAD))
        base.update(overrides)
        return packets.compose(**base)

    def answered(self, one):
        return {one["envelope"]["messageId"]: {
            "contentDigest": packets.content_digest(one), "disposition": packets.ACCEPTED}}

    def test_a_different_callback_under_one_id_is_a_collision(self):
        first = self.correction()
        second = self.correction(callback=a_callback(task_id="some other task"))
        self.assertEqual(packets.repeat(second, self.answered(first))["state"],
                         packets.COLLISION)

    def test_a_different_pull_request_under_one_id_is_a_collision(self):
        first = self.correction()
        second = self.correction(artifact=packets.pull_request(
            repository="thisisjun786/somewhere-else", number=107, head_sha=HEAD))
        self.assertEqual(packets.repeat(second, self.answered(first))["state"],
                         packets.COLLISION)

    def test_a_different_criteria_digest_under_one_id_is_a_collision(self):
        first = self.correction()
        second = self.correction(criteria_digest="0" * 64)
        self.assertEqual(packets.repeat(second, self.answered(first))["state"],
                         packets.COLLISION)

    def test_when_the_sender_observed_it_does_not_make_it_a_different_instruction(self):
        """A repeat observed a minute later asks for exactly the same thing."""
        first = self.correction(observed_at="2026-09-22T04:00:00+00:00")
        second = self.correction(observed_at="2026-09-22T04:01:00+00:00")
        self.assertEqual(packets.repeat(second, self.answered(first))["state"],
                         packets.REPLAY)

    def test_every_required_field_of_every_purpose_moves_the_digest(self):
        """Derived rather than listed, so a field added later is covered without an edit."""
        base = self.correction()
        for field, changed in ((packets.ISSUE, "CRW-999"), (packets.GENERATION, 7),
                               (packets.CRITERIA_DIGEST, "1" * 64),
                               (packets.CALLBACK, a_callback(model=LEFT_PARENT_PAIR[0]))):
            with self.subTest(field=field):
                other = self.correction(**{
                    {packets.ISSUE: "issue", packets.GENERATION: "generation",
                     packets.CRITERIA_DIGEST: "criteria_digest",
                     packets.CALLBACK: "callback"}[field]: changed})
                self.assertNotEqual(packets.content_digest(base),
                                    packets.content_digest(other), field)
