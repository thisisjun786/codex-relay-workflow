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

import json
import unittest

from codex_session_relay import envelope, packets, report
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


# What every occasion must carry, written out from the packet contract (docs/packets.md,
# "Fourteen occasions, not two") as literals. The sweep runs over THIS table, and a test pins
# packets.REQUIRED_BY_PURPOSE to it, so dropping a field from the validator's table fails here
# rather than silently shrinking the sweep that was supposed to catch it.
CANONICAL_REQUIRED = {
    ("parent_to_child", "assignment"): ("issue", "criteriaDigest", "policy", "callback", "body"),
    ("parent_to_child", "revision_request"): (
        "issue", "generation", "criteriaDigest", "callback", "artifact", "body", "evidence"),
    ("parent_to_child", "resume"): ("issue", "policy", "callback", "artifact"),
    ("parent_to_child", "receipt_confirmation"): ("issue", "correlationId"),
    ("parent_to_child", "acceptance"): ("issue", "criteriaDigest", "artifact"),
    ("parent_to_child", "integration_result"): ("issue", "artifact"),
    ("child_to_parent", "completion"): (
        "issue", "generation", "criteriaDigest", "artifact", "evidence"),
    ("child_to_parent", "review_ready"): (
        "issue", "generation", "criteriaDigest", "artifact", "evidence"),
    ("child_to_parent", "blocked"): ("issue", "evidence"),
    ("child_to_parent", "decision_request"): ("issue", "decision", "evidence"),
    ("child_to_parent", "progress"): ("issue",),
    ("parent_to_supervisor", "completion"): ("issue", "generation", "evidence"),
    ("parent_to_supervisor", "blocked"): ("issue", "evidence"),
    ("parent_to_supervisor", "decision_request"): ("issue", "decision", "evidence"),
}

# How each required field is supplied to compose.
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
    for name in CANONICAL_REQUIRED[(direction, purpose)]:
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
              "headSha": HEAD, "policy": {"model": CHILD_PAIR[0], "effort": CHILD_PAIR[1],
                                          "sandbox": {"type": "dangerFullAccess"},
                                          "approval": "never"},
              "callback": a_callback(), "mode": "loop", "workflow": "CXC Loop",
              "refusedPolicies": [], "refusedCallbackPolicies": [], "tenureGeneration": 1,
              "tenureDispatchRequestId": DISPATCH}
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
                  "callback": a_callback(), "refusedPolicies": [], "refusedCallbackPolicies": [],
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

    def test_a_policy_naming_another_sandbox_or_approval_is_refused(self):
        cases = (("policy.sandbox", "read-only", "never"),
                 ("policy.sandbox", {"type": "workspaceWrite"}, "never"),
                 ("policy.approval", "danger-full-access", "on-request"))
        for field, sandbox, approval in cases:
            with self.subTest(sandbox=sandbox, approval=approval):
                stated = {**a_policy(), "sandbox": sandbox, "approval": approval}
                answer = self.received(lambda: self.assignment(policy_record=stated),
                                       a_record())
                self.assertEqual(answer["disposition"], packets.REFUSAL, answer)
                self.assertEqual([(m["kind"], m["field"]) for m in answer["mismatches"]],
                                 [("stale_policy", field)])

    def test_a_record_holding_no_sandbox_leaves_a_stated_one_unchecked(self):
        blind = a_record(policy={"model": CHILD_PAIR[0], "effort": CHILD_PAIR[1],
                                 "approval": "never"})
        answer = self.received(self.assignment, blind)
        self.assertEqual(answer["disposition"], packets.UNAVAILABLE, answer)
        self.assertIn("policy.sandbox", self.gap_fields(answer))

    def test_a_policy_value_that_is_not_text_is_refused_rather_than_stringified(self):
        # 123 read from disk would otherwise agree with a recorded "123".
        for name in ("model", "effort", "workflow"):
            with self.subTest(name=name):
                one = self.assignment()
                one[packets.POLICY] = {**one[packets.POLICY], name: 123}
                detail = self.refusal(lambda: packets.check(one))
                self.assertIn(name, detail)

    def test_a_record_value_of_another_shape_is_a_gap_not_agreement(self):
        # Each of these agreed once both sides were turned into strings.
        cases = (("callback.model", lambda: self.assignment(callback=a_callback(pair=("123", "xhigh"))),
                  {"callback": {"taskId": PARENT, "model": 123, "effort": "xhigh"}}),
                 ("generation", lambda: packets.compose(**packet_kwargs(P2C, "revision_request")),
                  {"generation": str(GENERATION)}),
                 ("relationRevision", lambda: packets.compose(**packet_kwargs(P2C, "revision_request")),
                  {"relationRevision": str(REVISION)}),
                 ("policy.model", lambda: self.assignment(policy_record={**a_policy(pair=("123", "xhigh"))}),
                  {"policy": {"model": 123, "effort": "xhigh",
                              "sandbox": {"type": "dangerFullAccess"}, "approval": "never"}}))
        for field, build, change in cases:
            with self.subTest(field=field):
                answer = self.received(build, a_record(**change))
                self.assertEqual(answer["disposition"], packets.UNAVAILABLE, answer)
                self.assertIn(field, self.gap_fields(answer))

    def test_an_artifact_identity_field_that_is_not_text_is_refused(self):
        cases = (("headSha", {**a_pull_request(), "headSha": 123}),
                 ("repository", {**a_pull_request(), "repository": 7}),
                 ("path", {"kind": "locator", "path": 7, "digest": "d" * 64,
                           "producedAt": None}),
                 ("digest", {"kind": "locator", "path": "/state/x.md", "digest": ["d"],
                             "producedAt": None}))
        for field, artifact in cases:
            with self.subTest(field=field):
                one = packets.compose(**packet_kwargs(C2P, "review_ready"))
                one[packets.ARTIFACT] = artifact
                detail = self.refusal(lambda: packets.check(one))
                self.assertIn(field, detail)

    def test_a_refusal_list_of_another_shape_is_a_gap_not_none(self):
        for held in (None, [7], {}, "x", 7, [{"model": "m", "effort": "e"}, 7], [{}],
                     [{"model": 123, "effort": "xhigh"}], [{"model": " ", "effort": "x"}]):
            with self.subTest(held=held):
                answer = self.received(self.assignment, a_record(refusedPolicies=held))
                self.assertEqual(answer["disposition"], packets.UNAVAILABLE, answer)
                self.assertIn("policy", self.gap_fields(answer))

    def test_a_callback_pair_the_policy_refuses_for_the_parent_is_refused(self):
        # The parent's own record can still hold the pair it left: nothing re-records it when
        # the policy moves. The reading judges that recorded pair against the current policy
        # for the parent's role, and a callback naming a pair refused there is stale - the
        # agreement with the record does not make it current.
        left = {"model": LEFT_PARENT_PAIR[0], "effort": LEFT_PARENT_PAIR[1],
                "reason": "the parent role runs another pair now"}
        record = a_record(callback=a_callback(pair=LEFT_PARENT_PAIR),
                          refusedCallbackPolicies=[left])
        answer = self.received(
            lambda: self.assignment(callback=a_callback(pair=LEFT_PARENT_PAIR)), record)
        self.assertEqual(answer["disposition"], packets.REFUSAL, answer)
        self.assertEqual([(m["kind"], m["field"]) for m in answer["mismatches"]],
                         [("stale_callback", "callback.model/effort")])

    def test_an_unread_callback_refusal_list_leaves_the_callback_unchecked(self):
        for held in ("absent", None, [7], {}, [{"model": 7, "effort": "max"}],
                     [{"model": " ", "effort": "max"}]):
            with self.subTest(held=held):
                record = a_record(refusedCallbackPolicies=held)
                if held == "absent":
                    del record["refusedCallbackPolicies"]
                answer = self.received(self.assignment, record)
                self.assertEqual(answer["disposition"], packets.UNAVAILABLE, answer)
                self.assertIn("callback", self.gap_fields(answer))

    def test_a_stated_workflow_is_compared_with_the_one_the_receiver_holds(self):
        resume = lambda workflow: packets.compose(**packet_kwargs(  # noqa: E731
            P2C, "resume", policy_record={**a_policy(), "workflow": workflow}))
        answer = self.received(lambda: resume("CXC Loop under another procedure"),
                               a_record(workflow="CXC Loop"))
        self.assertEqual(answer["disposition"], packets.REFUSAL, answer)
        self.assertEqual([(m["kind"], m["field"]) for m in answer["mismatches"]],
                         [("wrong_workflow", "policy.workflow")])
        held_none = {key: value for key, value in a_record().items() if key != "workflow"}
        answer = self.received(lambda: resume("CXC Loop"), held_none)
        self.assertEqual(answer["disposition"], packets.UNAVAILABLE, answer)
        self.assertIn("workflow", self.gap_fields(answer))
        answer = self.received(lambda: resume("CXC Loop"), a_record(workflow="CXC Loop"))
        self.assertEqual(answer["disposition"], packets.ACCEPTED, answer)


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


class RB6ReviewReadyInTheDeliveredEnvelope(unittest.TestCase):
    """A candidate offered for review is not delivered under the word completion."""

    def test_a_ready_for_review_outcome_is_delivered_as_review_ready(self):
        self.assertEqual(report.child_purpose("ready_for_review"), "review_ready")

    def test_a_blocked_outcome_and_any_other_stay_as_they_were(self):
        self.assertEqual(report.child_purpose("blocked_needs_input"), "blocked")
        for outcome in ("failed", "interrupted"):
            with self.subTest(outcome=outcome):
                self.assertEqual(report.child_purpose(outcome), "completion")


# Which record key answers which compared field, per the direction's roles.
def _read_keys(direction, purpose):
    required = CANONICAL_REQUIRED[(direction, purpose)]
    sender_role, recipient_role = envelope.ENDPOINT_ROLES[direction]
    keys = {"relationId", packets.RECORD_TASK_KEY[sender_role],
            packets.RECORD_TASK_KEY[recipient_role], "issue", "relationRevision",
            "relationStatus"}
    if direction in (P2C, C2P):
        keys.add(packets.DISPATCH_REQUEST)  # which tenure the packet belongs to
    if packets.GENERATION in required:
        keys.add("generation")
    if packets.CRITERIA_DIGEST in required:
        keys.add("criteriaDigest")
    if packets.ARTIFACT in required:
        keys.update(("repository", "prNumber", "headSha"))
    if packets.CALLBACK in required:
        keys.update(("callback", "refusedCallbackPolicies"))
    if packets.POLICY in required:
        keys.update(("policy", "refusedPolicies", "mode", "workflow"))
        if direction in (P2C, C2P):
            # A policy is held to the current tenure, which the reading has to name.
            keys.update((packets.TENURE_GENERATION, packets.TENURE_DISPATCH))
    if purpose == "assignment":
        # An assignment defines the mode and the workflow where none is held.
        keys.discard("mode")
        keys.discard("workflow")
    return sorted(keys)


def sweep_rows():
    """(direction, purpose, required fields, record keys read) for every declared occasion."""
    return [(direction, purpose, list(CANONICAL_REQUIRED[(direction, purpose)]),
             _read_keys(direction, purpose))
            for direction, purpose in sorted(CANONICAL_REQUIRED)]


class RequiredFieldSweep(_Reading):
    """Every required field of every purpose, missing and unreadable, one subtest each."""

    def test_the_validators_table_is_the_contracts_table(self):
        self.assertEqual(sorted(packets.REQUIRED_BY_PURPOSE), sorted(CANONICAL_REQUIRED))
        for occasion, fields in CANONICAL_REQUIRED.items():
            with self.subTest(occasion=occasion):
                self.assertEqual(sorted(packets.required_for(*occasion)), sorted(fields))

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


# The fields a packet may state where its purpose does not require them, each with the value
# the record agrees with, a stale one, the record key that answers it, the gap that key's
# absence leaves and the mismatch a stale value is refused as.
STATED = {
    packets.GENERATION: ("generation", GENERATION, GENERATION + 1, "generation",
                         "generation", packets.STALE_GENERATION),
    packets.CRITERIA_DIGEST: ("criteria_digest", DIGEST, "e" * 64, "criteriaDigest",
                              "criteriaDigest", packets.STALE_CRITERIA),
    packets.ARTIFACT: ("artifact", a_pull_request(), a_pull_request(head="f" * 40), "headSha",
                       "artifact.headSha", packets.STALE_HEAD),
    packets.CALLBACK: ("callback", a_callback(), a_callback(pair=LEFT_PARENT_PAIR), "callback",
                       "callback", packets.STALE_CALLBACK),
    packets.POLICY: ("policy_record", a_policy(), a_policy(pair=LEFT_PARENT_PAIR), "policy",
                     "policy", packets.STALE_POLICY),
}


def stated_rows():
    """(direction, purpose, field) for every compared field an occasion may state unasked."""
    return [(direction, purpose, name)
            for direction, purpose in sorted(CANONICAL_REQUIRED)
            for name in STATED if name not in CANONICAL_REQUIRED[(direction, purpose)]]


class StatedFieldsAreHeldWhateverThePurpose(_Reading):
    """A field a packet states is held to the record, required by its purpose or not.

    The purpose table says what an occasion cannot do without; it does not license what an
    occasion says. A blocked report naming the digest its sender judged against was accepted
    after the criteria had been registered again, and accepted again when the store could not
    read any digest at all, because only a required digest was compared. Stated is compared:
    the current value agrees, a stale one is refused as that, and one the record cannot
    answer is unavailable.
    """

    def test_the_current_value_agrees(self):
        for direction, purpose, name in stated_rows():
            keyword, current, _stale, _key, _gap, _kind = STATED[name]
            with self.subTest(direction=direction, purpose=purpose, field=name):
                answer = self.received(lambda: packets.compose(**packet_kwargs(
                    direction, purpose, **{keyword: current})), a_record())
                self.assertEqual(answer["disposition"], packets.ACCEPTED, answer)

    def test_a_stale_value_is_refused_as_stale(self):
        for direction, purpose, name in stated_rows():
            keyword, _current, stale, _key, _gap, kind = STATED[name]
            with self.subTest(direction=direction, purpose=purpose, field=name):
                answer = self.received(lambda: packets.compose(**packet_kwargs(
                    direction, purpose, **{keyword: stale})), a_record())
                self.assertEqual(answer["disposition"], packets.REFUSAL, answer)
                self.assertIn(kind, self.kinds(answer))

    def test_a_value_the_record_cannot_answer_is_unavailable(self):
        for direction, purpose, name in stated_rows():
            keyword, current, _stale, key, gap, _kind = STATED[name]
            with self.subTest(direction=direction, purpose=purpose, field=name):
                blind = a_record()
                del blind[key]
                answer = self.received(lambda: packets.compose(**packet_kwargs(
                    direction, purpose, **{keyword: current})), blind)
                self.assertEqual(answer["disposition"], packets.UNAVAILABLE, answer)
                self.assertEqual(answer["mismatches"], [])
                self.assertIn(gap, self.gap_fields(answer))


# A lone surrogate is a string JSON can carry and UTF-8 cannot encode.
WRONG_SHAPES = ([], 7, "x", {}, True, "\ud800")


class NoShapeEscapesAsAHostFailure(_Reading):
    """Every field of every purpose, replaced by each wrong shape: an answer or a refusal.

    A packet read back from disk can hold anything JSON can. Whatever it holds, reception
    either answers or refuses as a packet; any other exception is a host failure standing in
    for telling the producer what it sent.
    """

    def test_every_field_of_every_purpose_in_every_wrong_shape(self):
        escaped = []
        for direction, purpose in sorted(CANONICAL_REQUIRED):
            base = packets.compose(**packet_kwargs(direction, purpose))
            paths = [(name,) for name in base] + [("envelope", name) for name in base["envelope"]]
            paths += [(name, inner) for name in base if isinstance(base[name], dict)
                      and name != "envelope" for inner in base[name]]
            for path in paths:
                for shape in WRONG_SHAPES:
                    one = json.loads(json.dumps(base))
                    holder = one
                    for step in path[:-1]:
                        holder = holder[step]
                    holder[path[-1]] = shape
                    try:
                        packets.reception(one, a_record())
                    except RelayError:
                        pass
                    except Exception as fault:  # noqa: BLE001 - the class under test
                        escaped.append((direction, purpose, path, repr(shape),
                                        type(fault).__name__))
        self.assertEqual(escaped, [])

    # Envelope fields reception neither compares nor acts on: what the recipient owes is
    # derived from the kind, and the rest are renderings the receiver never reads back.
    NOT_READ = {("envelope", "answerOwedBy"), ("envelope", "basis"), ("envelope", "scope"),
                ("envelope", "observedAt"), ("envelope", "replyTo"), ("envelope", "evidence")}

    def test_a_compared_field_of_another_shape_never_ends_accepted(self):
        # Not only no exception: a field turned into another type is refused as a packet or
        # left unread, and never agrees. The packet side first, then every record key the
        # occasion reads.
        agreed = []
        for direction, purpose in sorted(CANONICAL_REQUIRED):
            base = packets.compose(**packet_kwargs(direction, purpose))
            paths = [(name,) for name in base] + [("envelope", name) for name in base["envelope"]]
            paths += [(name, inner) for name in base if isinstance(base[name], dict)
                      and name != "envelope" for inner in base[name]]
            for path in paths:
                original = base
                for step in path:
                    original = original[step]
                if path in self.NOT_READ or original is None or envelope.is_absent(original):
                    continue
                for shape in WRONG_SHAPES:
                    if type(shape) is type(original):
                        continue
                    one = json.loads(json.dumps(base))
                    holder = one
                    for step in path[:-1]:
                        holder = holder[step]
                    holder[path[-1]] = shape
                    try:
                        answer = packets.reception(one, a_record())
                    except RelayError:
                        continue
                    if answer["disposition"] == packets.ACCEPTED:
                        agreed.append(("packet", purpose, path, repr(shape)))
            for key in _read_keys(direction, purpose):
                for shape in WRONG_SHAPES:
                    if type(shape) is type(a_record()[key]):
                        continue
                    try:
                        answer = packets.reception(base, a_record(**{key: shape}))
                    except RelayError:
                        continue
                    if answer["disposition"] == packets.ACCEPTED:
                        agreed.append(("record", purpose, key, repr(shape)))
        self.assertEqual(agreed, [])


if __name__ == "__main__":
    unittest.main()
