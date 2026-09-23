"""The post-merge review of PR #116, one finding per class, each written to fail first.

The review reproduced seven packets the merged validator accepted or crashed on. Each class
here is seeded from that reproduction and asserts the behaviour the finding asked for. They
use as little new interface as each finding allows, so that against the unfixed validator they
fail as assertions rather than erroring out: shapes that did not exist yet are written as dict
literals, new mismatch names as strings, and a refusal raised while a packet is still being
built is turned into a failure that says so rather than escaping as an exception.

The last class is the sweep the review asked to be stated: for every purpose, every required
field missing is a refusal naming it, and every field the receiver's record is read for,
when the record cannot answer it, is unavailable rather than accepted.
"""

import unittest

from codex_session_relay import envelope, packets
from codex_session_relay.errors import RelayError

RELATION = "rel-46d5b5ac690ef861"
DISPATCH = "dispatch-crw149-first"
PARENT = "01parent-task"
CHILD = "01child-task"
SUPERVISOR = "01supervisor-task"
ISSUE = "CRW-149"
DIGEST = "d" * 64
HEAD = "a1b2c3d4e5f60718293a4b5c6d7e8f9012345678"
REPOSITORY = "thisisjun786/codex-relay-workflow"
REVISION = 3
GENERATION = 2
# The parent's pair since 2026-09-23 and the one it left that day; the child's pair.
PARENT_PAIR = ("anthropic/claude-opus-5-5", "xhigh")
LEFT_PARENT_PAIR = ("devin/swe-2", "max")
CHILD_PAIR = ("anthropic/claude-opus-5-5", "xhigh")
EVIDENCE = ["/state/crw/crw-149/evidence/finding.txt"]

P2C, C2P, P2S = envelope.PARENT_TO_CHILD, envelope.CHILD_TO_PARENT, envelope.PARENT_TO_SUPERVISOR
ENDPOINTS = {P2C: (PARENT, CHILD), C2P: (CHILD, PARENT), P2S: (PARENT, SUPERVISOR)}

BODY = "\n".join((
    "TASK: land the store-backed reception check",
    "SCOPE: packages/codex-session-relay and the crw-run references",
    "MUST DO: one pull request to dev, its CI and its review",
    "MUST NOT: merge it, install anything, or touch the live store",
    "PROOF: the relay suite green on the head you report",
    "RETURN FORMAT: result.json plus a final report naming that head",
    "DECISION BOUNDARY: routine implementation choices are yours; a scope change comes back",
))
CORRECTION = "\n".join((
    "VIOLATED CRITERION: criterion 3, a correction names what it corrects",
    "WHAT CHANGED: a revision request now carries its body and its evidence",
    "FIX SCOPE: packets.py and its tests",
    "PRESERVE: the merged validator and every control that passes today",
    "REVERIFY AND RETURN: the relay suite on the new head, then report review_ready",
))


def a_callback(task=PARENT, pair=PARENT_PAIR):
    return {"taskId": task, "model": pair[0], "effort": pair[1]}


def a_policy(mode="loop", workflow="CXC Loop", pair=CHILD_PAIR):
    return {"model": pair[0], "effort": pair[1], "workflow": workflow, "mode": mode,
            "sandbox": "danger-full-access", "approval": "never"}


def a_pull_request(head=HEAD):
    return {"kind": "pull_request", "repository": REPOSITORY, "number": 107, "headSha": head,
            "baseSha": None, "url": None}


def a_reading(mode, **facts):
    """An activation reading under a mode, with the mode written in as a literal."""
    reading = packets.unexamined(mode)
    reading["mode"] = mode
    reading.update(facts)
    return reading


# How each required field is supplied to compose, so the sweep derives from the table rather
# than from a list someone has to remember to extend.
VALUES = {
    packets.ISSUE: ("issue", ISSUE),
    packets.GENERATION: ("generation", GENERATION),
    packets.CRITERIA_DIGEST: ("criteria_digest", DIGEST),
    packets.POLICY: ("policy_record", a_policy()),
    packets.CALLBACK: ("callback", a_callback()),
    packets.ARTIFACT: ("artifact", a_pull_request()),
    packets.EVIDENCE: ("evidence", EVIDENCE),
    packets.BODY: ("body", BODY),
    packets.DECISION: ("decision", "whether to accept the known defect for this release"),
    packets.CORRELATION: ("correlation_id", "msg-earlier"),
}


def packet_kwargs(direction, purpose, *, without=None, **overrides):
    sender, recipient = ENDPOINTS[direction]
    kwargs = dict(direction=direction, purpose=purpose, relation_id=RELATION, sender=sender,
                  recipient=recipient, subject="evt-1", relation_revision=REVISION)
    for name in packets.required_for(direction, purpose):
        if name == without:
            continue
        keyword, value = VALUES[name]
        if name == packets.BODY and purpose == "revision_request":
            value = CORRECTION
        kwargs[keyword] = value
    kwargs.update(overrides)
    return kwargs


def a_record(**overrides):
    """Everything a receiver could have read, agreeing with every packet built above."""
    record = {"relationId": RELATION, "parentTaskId": PARENT, "childTaskId": CHILD,
              "supervisorTaskId": SUPERVISOR, "issue": ISSUE, "relationRevision": REVISION,
              "relationStatus": "active", "generation": GENERATION, "criteriaDigest": DIGEST,
              "dispatchRequestId": DISPATCH, "repository": REPOSITORY, "prNumber": 107,
              "headSha": HEAD, "policy": {"model": CHILD_PAIR[0], "effort": CHILD_PAIR[1]},
              "callback": a_callback(), "mode": "loop", "refusedPolicies": []}
    record.update(overrides)
    return record


class _Reading(unittest.TestCase):
    def received(self, build, record):
        """The reception of a packet, or a failure saying the packet never got that far."""
        try:
            return packets.reception(build(), record)
        except RelayError as refused:
            self.fail("the packet was refused before its record was read: " + refused.detail)

    def refusal(self, build):
        """The refusal a packet gets while it is built, or a failure saying there was none."""
        try:
            build()
        except RelayError as refused:
            return refused.detail
        self.fail("the packet was accepted as complete")

    def kinds(self, answer):
        return sorted(m["kind"] for m in answer["mismatches"])

    def gap_fields(self, answer):
        return sorted(g["field"] for g in answer["gaps"])


class RB1FirstAssignmentLifecycle(_Reading):
    """A first assignment has to be acceptable at some point of its own lifecycle.

    It is written before the child exists, so it cannot name the relationship (its id is
    derived from the child's task id) or the recipient. It names the dispatch request that
    registration binds to the relationship, and states its recipient as an absence.
    """

    def first_assignment(self):
        return packets.compose(**packet_kwargs(
            P2C, "assignment", relation_id=DISPATCH, relation_revision=None,
            recipient=envelope.absent(envelope.UNKNOWN,
                                      "creation has not returned the child's task id")))

    def test_before_registration_it_is_unavailable_rather_than_refused(self):
        before = {"parentTaskId": PARENT, "issue": ISSUE, "criteriaDigest": DIGEST,
                  "callback": a_callback(), "refusedPolicies": [],
                  "policy": {"model": CHILD_PAIR[0], "effort": CHILD_PAIR[1]}}
        answer = self.received(self.first_assignment, before)
        self.assertEqual(answer["disposition"], packets.UNAVAILABLE, answer)
        self.assertEqual(answer["mismatches"], [])
        self.assertIn("recipient.taskId", self.gap_fields(answer))

    def test_after_registration_the_same_packet_is_accepted(self):
        answer = self.received(self.first_assignment, a_record())
        self.assertEqual(answer["disposition"], packets.ACCEPTED, answer)

    def test_a_dispatch_that_did_not_open_the_current_generation_is_another_relation(self):
        answer = self.received(self.first_assignment,
                               a_record(dispatchRequestId="dispatch-after-a-correction"))
        self.assertEqual(answer["disposition"], packets.REFUSAL, answer)
        self.assertIn("relationId", [m["field"] for m in answer["mismatches"]
                                     if m["kind"] == packets.WRONG_RELATION])

    def test_an_ended_relationship_does_not_take_its_first_assignment(self):
        answer = self.received(self.first_assignment, a_record(relationStatus="archived"))
        self.assertEqual(answer["disposition"], packets.REFUSAL, answer)
        self.assertIn("relationStatus", [m["field"] for m in answer["mismatches"]])


class RB2AnEmptyCorrection(_Reading):
    """A correction that says nothing about what it corrects is refused by name."""

    def correction(self, **overrides):
        return packets.compose(**packet_kwargs(P2C, "revision_request", **overrides))

    def test_no_body_and_no_evidence_is_refused_naming_the_body(self):
        detail = self.refusal(lambda: packets.compose(**{
            **packet_kwargs(P2C, "revision_request", without=packets.BODY), "evidence": ()}))
        self.assertIn("body", detail)

    def test_no_evidence_is_refused_naming_it(self):
        detail = self.refusal(lambda: self.correction(evidence=()))
        self.assertIn("evidence", detail)

    def test_a_body_without_the_correction_form_names_each_missing_section(self):
        detail = self.refusal(lambda: self.correction(
            body="VIOLATED CRITERION: criterion 3"))
        for section in ("WHAT CHANGED", "FIX SCOPE", "PRESERVE", "REVERIFY AND RETURN"):
            self.assertIn(section, detail)

    def test_a_complete_correction_is_read_against_the_record(self):
        answer = self.received(self.correction, a_record())
        self.assertEqual(answer["disposition"], packets.ACCEPTED, answer)


class RB3CallbackAndPolicy(_Reading):
    """The callback and the policy are compared with what the receiver holds."""

    def assignment(self, **overrides):
        return packets.compose(**packet_kwargs(P2C, "assignment", **overrides))

    def test_the_pair_the_parent_left_is_refused_as_a_stale_callback(self):
        answer = self.received(
            lambda: self.assignment(callback=a_callback(pair=LEFT_PARENT_PAIR)), a_record())
        self.assertEqual(answer["disposition"], packets.REFUSAL, answer)
        stale = [m["field"] for m in answer["mismatches"] if m["kind"] == "stale_callback"]
        self.assertEqual(sorted(stale), ["callback.effort", "callback.model"])

    def test_another_callback_task_is_refused(self):
        answer = self.received(
            lambda: self.assignment(callback=a_callback(task="01unrelated")), a_record())
        self.assertIn("wrong_callback", self.kinds(answer))

    def test_a_policy_the_receiver_was_not_created_with_is_refused(self):
        acceptance = lambda: packets.compose(**packet_kwargs(  # noqa: E731
            P2C, "acceptance", policy_record=a_policy(pair=("other/model", "low"))))
        answer = self.received(acceptance, a_record())
        self.assertEqual(answer["disposition"], packets.REFUSAL, answer)
        self.assertIn("stale_policy", self.kinds(answer))

    def test_a_record_with_no_callback_leaves_it_unchecked(self):
        blind = a_record()
        del blind["callback"]
        answer = self.received(self.assignment, blind)
        self.assertEqual(answer["disposition"], packets.UNAVAILABLE, answer)
        self.assertIn("callback", self.gap_fields(answer))


class RB4UnreadableRevision(_Reading):
    """A revision the record cannot answer is unchecked, not agreed."""

    def ready(self, **overrides):
        return packets.compose(**packet_kwargs(C2P, "review_ready", **overrides))

    def test_a_record_without_the_revision_is_unavailable(self):
        blind = a_record()
        del blind["relationRevision"]
        answer = self.received(self.ready, blind)
        self.assertEqual(answer["disposition"], packets.UNAVAILABLE, answer)
        self.assertEqual(self.gap_fields(answer), ["relationRevision"])

    def test_an_unscoped_relationship_agrees_with_a_packet_quoting_no_revision(self):
        answer = self.received(lambda: self.ready(relation_revision=None),
                               a_record(relationRevision=None))
        self.assertEqual(answer["disposition"], packets.ACCEPTED, answer)

    def test_an_unscoped_relationship_refuses_a_packet_quoting_a_revision(self):
        answer = self.received(self.ready, a_record(relationRevision=None))
        self.assertIn(packets.SUPERSEDED_RELATION, self.kinds(answer))

    def test_a_relationship_that_is_no_longer_live_is_refused(self):
        for status in ("archived", "cancelled"):
            with self.subTest(status=status):
                answer = self.received(self.ready, a_record(relationStatus=status))
                self.assertIn("relationStatus", [m["field"] for m in answer["mismatches"]])

    def test_a_paused_relationship_is_still_current(self):
        answer = self.received(self.ready, a_record(relationStatus="paused"))
        self.assertEqual(answer["disposition"], packets.ACCEPTED, answer)


class RB5ActivationUnderItsMode(_Reading):
    """An activation reading is bound to the mode the policy and the receiver hold."""

    def acceptance(self, **overrides):
        return packets.compose(**packet_kwargs(P2C, "acceptance", **overrides))

    def test_a_non_loop_reading_under_a_loop_policy_is_refused(self):
        detail = self.refusal(lambda: self.acceptance(
            policy_record=a_policy(), activation=a_reading(packets.NON_LOOP)))
        self.assertIn("mode", detail)

    def test_a_loop_reading_cannot_call_its_activation_inapplicable(self):
        inapplicable = packets.activation_fact(packets.INAPPLICABLE, detail="borrowed")
        detail = self.refusal(lambda: self.acceptance(
            policy_record=a_policy(),
            activation=a_reading(packets.LOOP, activated=inapplicable)))
        self.assertIn("not_applicable", detail)

    def test_a_workflow_naming_cxc_loop_cannot_state_another_mode(self):
        detail = self.refusal(lambda: self.acceptance(
            policy_record=a_policy(mode=packets.NON_LOOP, workflow="CXC Loop")))
        self.assertIn("CXC Loop", detail)

    def test_a_mode_the_receiver_does_not_hold_is_refused(self):
        answer = self.received(lambda: self.acceptance(policy_record=a_policy(
            mode=packets.NON_LOOP, workflow="an authorised read-only audit")), a_record())
        self.assertIn("wrong_mode", self.kinds(answer))

    def test_a_receiver_holding_no_mode_leaves_a_report_unchecked(self):
        blind = a_record()
        del blind["mode"]
        answer = self.received(lambda: packets.compose(**packet_kwargs(
            C2P, "review_ready", activation=a_reading(packets.LOOP))), blind)
        self.assertEqual(answer["disposition"], packets.UNAVAILABLE, answer)
        self.assertIn("mode", self.gap_fields(answer))

    def test_an_assignment_defines_the_mode_only_where_none_is_held(self):
        blind = a_record()
        del blind["mode"]
        answer = self.received(
            lambda: packets.compose(**packet_kwargs(P2C, "assignment")), blind)
        self.assertEqual(answer["disposition"], packets.ACCEPTED, answer)
        self.assertEqual([entry["field"] for entry in answer.get("instructed", [])], ["mode"])
        held = self.received(lambda: packets.compose(**packet_kwargs(
            P2C, "assignment", policy_record=a_policy(
                mode=packets.NON_LOOP, workflow="an authorised read-only audit"))),
            a_record())
        self.assertIn("wrong_mode", self.kinds(held))

    def test_a_loop_reading_under_a_loop_policy_passes(self):
        answer = self.received(lambda: self.acceptance(
            policy_record=a_policy(), activation=a_reading(packets.LOOP)), a_record())
        self.assertEqual(answer["disposition"], packets.ACCEPTED, answer)


class RB7AMalformedHandoverEntry(unittest.TestCase):
    """A handover state that is not an object is a refusal, never a host error."""

    def test_each_malformed_entry_is_refused_as_a_packet(self):
        for bad in (1, [], "yes", None):
            with self.subTest(entry=bad):
                ladder = packets.unobserved()
                ladder[packets.RELAY_ACK] = bad
                try:
                    packets.unsupported_promotions(ladder)
                except packets.PacketRefused:
                    continue
                except Exception as error:  # noqa: BLE001 - the finding IS the host error
                    self.fail("a malformed entry escaped as " + repr(error))
                self.fail("a malformed entry passed")


# Which record key answers which compared field, per the direction's roles.
def _read_keys(direction, purpose):
    required = packets.required_for(direction, purpose)
    sender_role, recipient_role = envelope.ENDPOINT_ROLES[direction]
    keys = {"relationId", packets.RECORD_TASK_KEY[sender_role],
            packets.RECORD_TASK_KEY[recipient_role], "issue", "relationRevision",
            "relationStatus"}
    if packets.GENERATION in required:
        keys.add("generation")
    if packets.CRITERIA_DIGEST in required:
        keys.add("criteriaDigest")
    if packets.ARTIFACT in required:
        keys.update(("repository", "prNumber", "headSha"))
    if packets.CALLBACK in required:
        keys.add("callback")
    if packets.POLICY in required:
        keys.update(("policy", "refusedPolicies", "mode"))
    if purpose == "assignment":
        keys.discard("mode")  # an assignment defines the mode where none is held
    return sorted(keys)


def sweep_rows():
    """(direction, purpose, required fields, record keys read) for every declared occasion."""
    return [(direction, purpose, list(packets.required_for(direction, purpose)),
             _read_keys(direction, purpose))
            for direction, purpose in sorted(packets.REQUIRED_BY_PURPOSE)]


class RequiredFieldSweep(_Reading):
    """Every required field of every purpose, missing and unreadable, one subtest each."""

    def test_the_full_packet_of_every_purpose_is_accepted(self):
        for direction, purpose, _required, _keys in sweep_rows():
            with self.subTest(direction=direction, purpose=purpose):
                answer = self.received(
                    lambda: packets.compose(**packet_kwargs(direction, purpose)), a_record())
                self.assertEqual(answer["disposition"], packets.ACCEPTED, answer)

    def test_a_missing_required_field_is_refused_by_its_name(self):
        for direction, purpose, required, _keys in sweep_rows():
            for name in required:
                with self.subTest(direction=direction, purpose=purpose, field=name):
                    detail = self.refusal(lambda: packets.compose(**packet_kwargs(
                        direction, purpose, without=name)))
                    self.assertIn(name, detail)

    def test_a_field_the_record_cannot_answer_is_unavailable(self):
        for direction, purpose, _required, keys in sweep_rows():
            for key in keys:
                with self.subTest(direction=direction, purpose=purpose, key=key):
                    blind = a_record()
                    del blind[key]
                    answer = self.received(
                        lambda: packets.compose(**packet_kwargs(direction, purpose)), blind)
                    self.assertEqual(answer["disposition"], packets.UNAVAILABLE, answer)
                    self.assertEqual(answer["mismatches"], [])
                    self.assertTrue(answer["gaps"])


if __name__ == "__main__":
    unittest.main()
