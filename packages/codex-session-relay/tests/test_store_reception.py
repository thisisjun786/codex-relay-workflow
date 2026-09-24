"""The receive step, against a real store: packet-check --receiver.

Every control here runs the command a receiver runs, against a relay store this test builds
through the registry, the linkage, the criteria service and the settings recorder, with the
role policy staged the way a host declares it. Each wrong packet has to come back with the
specific mismatch that names it; each field the store cannot answer has to come back as a gap;
and a non-PR audit has to pass without anybody asking it for a head.

What this establishes is the source's behaviour against isolated stores in a temporary
directory. It is not an installed relay, a receiver actually running the step, or a packet
reaching any task on any host.
"""

import contextlib
import hashlib
import io
import json
import os
from pathlib import Path

from codex_session_relay import cli, envelope, packets, receiver, rolepolicy
from codex_session_relay.criteria import CriteriaService, normalise_criteria, set_digest
from codex_session_relay.linkage import Linkage, PARENT as PARENT_ROLE
from codex_session_relay.models import Endpoint
from codex_session_relay.registry import record_settings

from .support import CHILD, DISPATCH_TURN, HOST, PARENT, DeliveryTestCase, task_settings
from .test_rolepolicy import (
    CHILD_EFFORT, CHILD_MODEL, PARENT_EFFORT, PARENT_MODEL, POLICY, SUPERSEDED_PARENT,
    write_policy)

P2C, C2P = envelope.PARENT_TO_CHILD, envelope.CHILD_TO_PARENT
PROJECT = "CRW-PROJECT"
ISSUE = "REL-1"
REPOSITORY = "thisisjun786/codex-relay-workflow"
HEAD = "a1b2c3d4e5f60718293a4b5c6d7e8f9012345678"
CRITERIA = [{"id": "c1", "title": "the receiver reads its own record", "required": True}]
EVIDENCE = ["/state/crw/crw-149/evidence/suite.txt"]
CORRECTION = "\n".join((
    "VIOLATED CRITERION: c1, the receiver reads its own record",
    "WHAT CHANGED: the review found the reading is supplied",
    "FIX SCOPE: receiver.py",
    "PRESERVE: the supplied form and its label",
    "REVERIFY AND RETURN: the store reception suite, then report review_ready",
))
BODY = "\n".join((
    "TASK: make packet-check read the receiver's own record",
    "SCOPE: packages/codex-session-relay",
    "MUST DO: one pull request to dev with its CI and review",
    "MUST NOT: merge it or touch a live store",
    "PROOF: the relay suite green on the reported head",
    "RETURN FORMAT: result.json naming that head",
    "DECISION BOUNDARY: routine choices are yours; scope changes come back",
))


class StoreReception(DeliveryTestCase):
    def setUp(self):
        super().setUp()
        path = write_policy(Path(self.tmp))
        previous = os.environ.get(rolepolicy.ENVIRONMENT_VARIABLE)
        os.environ[rolepolicy.ENVIRONMENT_VARIABLE] = path
        rolepolicy.reset()

        def restore():
            if previous is None:
                os.environ.pop(rolepolicy.ENVIRONMENT_VARIABLE, None)
            else:
                os.environ[rolepolicy.ENVIRONMENT_VARIABLE] = previous
            rolepolicy.reset()

        self.addCleanup(restore)
        self.state = os.path.dirname(str(self.store.path))
        self.linkage = Linkage(self.store, self.clock)
        self.criteria = CriteriaService(self.store, self.clock)
        self.parent = Endpoint(PARENT, HOST, cwd="/parent", cxc_session="cxc-parent")
        self.linkage.bind_scope(role=PARENT_ROLE, scope_key=PROJECT, endpoint=self.parent)
        record_settings(self.store, self.clock, PARENT,
                        task_settings("/parent", model=PARENT_MODEL,
                                      reasoningEffort=PARENT_EFFORT),
                        source="creation_result")
        self.files = 0

    # ------------------------------------------------------------------------- fixtures

    def registered(self, *, child=CHILD, issue=ISSUE, dispatch="dispatch-1",
                   turn=DISPATCH_TURN, project=PROJECT):
        self.adapter.add_thread(child)
        relationship = self.registry.register(
            parent=self.parent,
            child=Endpoint(child, HOST, cwd=self.root, cxc_session="cxc-" + child),
            issue_key=issue, artifact_roots=[self.root], allowed_recipients=[PARENT, child],
            dispatch_request_id=dispatch, dispatch_turn_id=turn, project_key=project)
        record_settings(self.store, self.clock, child,
                        task_settings(self.root, model=CHILD_MODEL,
                                      reasoningEffort=CHILD_EFFORT),
                        source="creation_result")
        self.criteria.register(relationship["relationshipId"], CRITERIA)
        return relationship

    def digest(self, entries=CRITERIA):
        return set_digest(normalise_criteria(entries))

    def revision(self, relationship):
        attached = self.linkage.attachment(relationship["relationshipId"])
        return attached["link"]["revision"] if attached else None

    def event(self, relationship):
        payload = self.ready_payload(relationship, [self.artifact("out.txt", "the work")])
        self.accept(payload)
        return payload["eventId"]

    def observed(self, head=HEAD, **extra):
        return {"source": "forge readback in this test", "repository": REPOSITORY,
                "prNumber": 107, "headSha": head, **extra}

    def a_callback(self, pair=(PARENT_MODEL, PARENT_EFFORT)):
        return packets.callback(task_id=PARENT, model=pair[0], effort=pair[1])

    def a_policy(self, mode=packets.LOOP, workflow="CXC Loop", sandbox="workspace-write",
                 approval="never"):
        # The permissions task_settings records for every task here: a workspaceWrite sandbox
        # and approval never.
        return packets.policy(model=CHILD_MODEL, effort=CHILD_EFFORT, workflow=workflow,
                              mode=mode, sandbox=sandbox, approval=approval)

    def report(self, relationship, event, **overrides):
        base = dict(direction=C2P, purpose="review_ready",
                    relation_id=relationship["relationshipId"], sender=CHILD,
                    recipient=PARENT, subject=event, issue=ISSUE,
                    generation=relationship["executionGeneration"],
                    criteria_digest=self.digest(), relation_revision=self.revision(relationship),
                    artifact=packets.pull_request(repository=REPOSITORY, number=107,
                                                  head_sha=HEAD),
                    evidence=EVIDENCE)
        base.update(overrides)
        return packets.compose(**base)

    def correction(self, relationship, *, generation, **overrides):
        base = dict(direction=P2C, purpose="revision_request",
                    relation_id=relationship["relationshipId"], sender=PARENT,
                    recipient=CHILD, subject="evt-corrected", issue=ISSUE,
                    generation=generation, criteria_digest=self.digest(),
                    relation_revision=self.revision(relationship), callback=self.a_callback(),
                    artifact=packets.pull_request(repository=REPOSITORY, number=107,
                                                  head_sha=HEAD),
                    body=CORRECTION, evidence=EVIDENCE)
        base.update(overrides)
        return packets.compose(**base)

    def first_assignment(self, *, dispatch, **overrides):
        base = dict(direction=P2C, purpose="assignment", relation_id=dispatch, sender=PARENT,
                    recipient=envelope.absent(envelope.UNKNOWN,
                                              "creation has not returned the child's task id"),
                    subject="REL-FIRST", issue="REL-FIRST", criteria_digest=self.digest(),
                    policy_record=self.a_policy(), callback=self.a_callback(), body=BODY)
        base.update(overrides)
        return packets.compose(**base)

    def _write(self, what, document):
        self.files += 1
        path = os.path.join(self.tmp, "%s-%d.json" % (what, self.files))
        with open(path, "w", encoding="utf-8") as handle:
            json.dump(document, handle)
        return path

    def packet_check(self, packet, *, receiver_id=None, observation=None, ledger=None,
                     record=None, state=None, applied=False):
        """The command a receiver runs, in process, and what it printed.

        An exit the argument parser raises comes back as its code, and a run that printed
        nothing as an empty answer, so a control whose flag or path does not exist fails as
        an assertion about the code rather than escaping as an exception.
        """
        argv = ["--state", state or self.state, "packet-check",
                "--packet", self._write("packet", packet)]
        if record is not None:
            argv += ["--record", self._write("record", record)]
        else:
            argv += ["--receiver", receiver_id]
        if observation is not None:
            argv += ["--observation", self._write("observation", observation)]
        if ledger is not None:
            argv += ["--ledger", ledger]
        if applied:
            argv.append("--applied")
        printed = io.StringIO()
        with contextlib.redirect_stdout(printed):
            try:
                code = cli.main(argv)
            except SystemExit as stopped:
                code = stopped.code
        output = printed.getvalue()
        return code, json.loads(output) if output.strip() else {}

    def kinds(self, answer):
        return sorted({m["kind"] for m in answer["mismatches"]})

    def gap_fields(self, answer):
        return sorted({g["field"] for g in answer["gaps"]})


class TheReceiversOwnReading(StoreReception):
    def test_an_agreeing_report_is_accepted_and_every_field_names_what_answered_it(self):
        relationship = self.registered()
        event = self.event(relationship)
        code, answer = self.packet_check(self.report(relationship, event), receiver_id=PARENT,
                                         observation=self.observed())
        self.assertEqual(code, 0)
        self.assertEqual(answer["disposition"], packets.ACCEPTED, answer)
        self.assertEqual(answer["recordSource"], "store")
        provenance = answer["provenance"]
        self.assertEqual(provenance["parentTaskId"], "receiver")
        self.assertTrue(provenance["relationId"].startswith("relationships"))
        self.assertEqual(provenance["generation"], "relationships.execution_generation")
        self.assertTrue(provenance["relationRevision"].startswith("scope_links"))
        self.assertEqual(provenance["criteriaDigest"], "canonical_criteria (managed set)")
        self.assertEqual(provenance["headSha"], "observation: forge readback in this test")
        self.assertEqual(answer["repeat"]["state"], packets.UNCHECKED)
        self.assertFalse(answer["act"])

    def test_the_offline_supplied_form_still_says_it_was_supplied(self):
        relationship = self.registered()
        event = self.event(relationship)
        one = self.report(relationship, event)
        record = {"relationId": relationship["relationshipId"], "parentTaskId": PARENT,
                  "childTaskId": CHILD, "issue": ISSUE, "generation": 1,
                  "criteriaDigest": self.digest(), "relationRevision": self.revision(
                      relationship), "relationStatus": "active", "repository": REPOSITORY,
                  "prNumber": 107, "headSha": HEAD, "dispatchRequestId": "dispatch-1"}
        code, answer = self.packet_check(one, record=record)
        self.assertEqual(code, 0)
        self.assertEqual(answer["recordSource"], "supplied")
        self.assertEqual(answer["disposition"], packets.ACCEPTED, answer)

    def test_a_supplied_reading_or_packet_of_the_wrong_shape_never_ends_as_a_host_failure(self):
        relationship = self.registered()
        one = self.report(relationship, self.event(relationship))
        record = {"relationId": relationship["relationshipId"], "parentTaskId": PARENT,
                  "childTaskId": CHILD, "issue": ISSUE, "generation": 1,
                  "criteriaDigest": self.digest(), "relationRevision": self.revision(
                      relationship), "relationStatus": "active", "repository": REPOSITORY,
                  "prNumber": 107, "headSha": HEAD}
        # An assignment reads the refusals. Against a reading that otherwise agrees, one that
        # is not a list of text pairs is unread - never a crash and never "none refused".
        assignment = self.first_assignment(dispatch="dispatch-1", issue=ISSUE)
        agreeing = {"dispatchRequestId": "dispatch-1", "parentTaskId": PARENT,
                    "childTaskId": CHILD, "issue": ISSUE, "relationStatus": "active",
                    "criteriaDigest": self.digest(), "callback": self.a_callback(),
                    "policy": {"model": CHILD_MODEL, "effort": CHILD_EFFORT,
                               "sandbox": {"type": "workspaceWrite"}, "approval": "never"},
                    "refusedPolicies": [], "refusedCallbackPolicies": [], "tenureGeneration": 1,
                    "tenureDispatchRequestId": "dispatch-1"}
        code, answer = self.packet_check(assignment, record=agreeing)
        self.assertEqual((code, answer["disposition"]), (0, packets.ACCEPTED), answer)
        for held in ([7], None, [{"model": 7, "effort": CHILD_EFFORT}]):
            with self.subTest(held=held):
                code, answer = self.packet_check(assignment,
                                                 record={**agreeing, "refusedPolicies": held})
                self.assertEqual(code, 0, answer)
                self.assertEqual(answer["disposition"], packets.UNAVAILABLE, answer)
        code, answer = self.packet_check(one, record={**record, "callback": 7})
        self.assertEqual(code, 0, answer)
        code, answer = self.packet_check(one, record=["not", "a", "reading"])
        self.assertEqual(code, cli.EXIT_REFUSED, answer)
        # A head that is a number, against a reading holding the same number: the packet's
        # own shape is refused before anything is compared.
        numeric = self.report(relationship, self.event(relationship))
        numeric[packets.ARTIFACT]["headSha"] = 123
        code, answer = self.packet_check(numeric, record={**record, "headSha": 123})
        self.assertEqual(code, cli.EXIT_REFUSED, answer)
        self.assertIn("headSha", answer.get("detail", ""))

    def test_json_nested_past_any_reading_is_refused_as_input_not_a_host_failure(self):
        relationship = self.registered()
        one = self.correction(relationship, generation=1)
        deep = os.path.join(self.tmp, "deep.json")
        with open(deep, "w", encoding="utf-8") as handle:
            handle.write("[" * 200000 + "]" * 200000)
        cases = {
            "packet": ["--packet", deep, "--receiver", CHILD],
            "record": ["--packet", self._write("packet", one), "--record", deep],
            "observation": ["--packet", self._write("packet", one), "--receiver", CHILD,
                            "--observation", deep],
            "ledger": ["--packet", self._write("packet", one), "--receiver", CHILD,
                       "--ledger", deep],
        }
        for what, args in cases.items():
            with self.subTest(what=what):
                printed = io.StringIO()
                with contextlib.redirect_stdout(printed):
                    try:
                        code = cli.main(["--state", self.state, "packet-check"] + args)
                    except SystemExit as stopped:
                        code = stopped.code
                self.assertEqual(code, cli.EXIT_USAGE, printed.getvalue()[:300])


class TheControlsThatMustNotPassTheReceiveStep(StoreReception):
    def test_a_missing_required_field_is_refused_by_name(self):
        relationship = self.registered()
        thin = dict(self.report(relationship, self.event(relationship)), evidence=[])
        code, answer = self.packet_check(thin, receiver_id=PARENT, observation=self.observed())
        self.assertEqual(code, cli.EXIT_REFUSED)
        self.assertEqual(answer["error"], "refused")
        self.assertIn(packets.EVIDENCE, answer["detail"])

    def test_a_packet_for_another_child_is_refused_as_that(self):
        mine = self.registered()
        theirs = self.registered(child="01another-child", issue="REL-2",
                                 dispatch="dispatch-2", turn="turn-dispatch-2")
        one = self.correction(theirs, generation=1, recipient="01another-child", issue="REL-2")
        code, answer = self.packet_check(one, receiver_id=CHILD, observation=self.observed())
        self.assertEqual(answer["disposition"], packets.REFUSAL, answer)
        self.assertIn(packets.WRONG_RELATION, self.kinds(answer))
        self.assertIn(packets.WRONG_RECIPIENT, self.kinds(answer))
        self.assertEqual(answer["record"]["relationId"], mine["relationshipId"])

    def test_a_correction_whose_generation_was_superseded_is_stale(self):
        relationship = self.registered()
        rid = relationship["relationshipId"]
        self.registry.open_generation(rid, dispatch_request_id="dispatch-rev-2",
                                      reason="needs_changes_revision")
        one = self.correction(relationship, generation=2)
        ledger = os.path.join(self.tmp, "child-ledger.json")
        _code, first = self.packet_check(one, receiver_id=CHILD,
                                         observation=self.observed(), ledger=ledger)
        self.assertEqual(first["disposition"], packets.ACCEPTED, first)
        self.assertTrue(first["act"])
        self.registry.open_generation(rid, dispatch_request_id="dispatch-rev-3",
                                      reason="needs_changes_revision")
        _code, again = self.packet_check(one, receiver_id=CHILD,
                                         observation=self.observed(), ledger=ledger)
        # Today's reading stands: the replay is refused, with the earlier answer beside it.
        self.assertEqual(again["disposition"], packets.REFUSAL, again)
        self.assertIn(packets.STALE_GENERATION, self.kinds(again))
        self.assertEqual(again["repeat"]["state"], packets.REPLAY)
        self.assertEqual(again["repeat"]["previousDisposition"], packets.ACCEPTED)
        self.assertFalse(again["act"])

    def test_criteria_registered_again_make_the_old_digest_stale(self):
        relationship = self.registered()
        event = self.event(relationship)
        self.criteria.register(relationship["relationshipId"], CRITERIA + [
            {"id": "c2", "title": "a criterion added later", "required": True}])
        _code, answer = self.packet_check(self.report(relationship, event), receiver_id=PARENT,
                                          observation=self.observed())
        self.assertEqual(self.kinds(answer), [packets.STALE_CRITERIA])

    def test_a_digest_a_block_states_is_held_to_the_registered_one(self):
        # A block is not required to name a digest; one that does is held to it. The digest
        # the child judged against was accepted as current after the criteria changed.
        relationship = self.registered()
        ledger = os.path.join(self.tmp, "parent-ledger.json")

        def blocked(subject, digest):
            return packets.compose(
                direction=C2P, purpose="blocked", relation_id=relationship["relationshipId"],
                sender=CHILD, recipient=PARENT, subject=subject, issue=ISSUE,
                relation_revision=self.revision(relationship), criteria_digest=digest,
                evidence=EVIDENCE)

        old = self.digest()
        _code, current = self.packet_check(blocked("blocked-current", old),
                                           receiver_id=PARENT, ledger=ledger)
        self.assertEqual(current["disposition"], packets.ACCEPTED, current)
        self.criteria.register(relationship["relationshipId"], CRITERIA + [
            {"id": "c2", "title": "a criterion added later", "required": True}])
        _code, answer = self.packet_check(blocked("blocked-stale", old), receiver_id=PARENT,
                                          ledger=ledger)
        self.assertEqual(answer["disposition"], packets.REFUSAL, answer)
        self.assertEqual(self.kinds(answer), [packets.STALE_CRITERIA])
        self.assertFalse(answer["act"], answer)

    def test_a_head_that_moved_is_stale_and_an_unobserved_head_is_unchecked(self):
        relationship = self.registered()
        one = self.report(relationship, self.event(relationship))
        _code, moved = self.packet_check(one, receiver_id=PARENT,
                                         observation=self.observed(head="f" * 40))
        self.assertEqual(self.kinds(moved), [packets.STALE_HEAD])
        _code, blind = self.packet_check(one, receiver_id=PARENT)
        self.assertEqual(blind["disposition"], packets.UNAVAILABLE, blind)
        self.assertIn("artifact.headSha", self.gap_fields(blind))

    def test_a_duplicate_correction_is_applied_once_and_a_changed_one_collides(self):
        relationship = self.registered()
        ledger = os.path.join(self.tmp, "child-ledger.json")
        one = self.correction(relationship, generation=1)
        _code, first = self.packet_check(one, receiver_id=CHILD,
                                         observation=self.observed(), ledger=ledger)
        self.assertTrue(first["act"], first)
        code, recorded = self.packet_check(one, receiver_id=CHILD, ledger=ledger, applied=True)
        self.assertEqual(code, 0, recorded)
        self.assertTrue(recorded["applied"], recorded)
        _code, again = self.packet_check(one, receiver_id=CHILD,
                                         observation=self.observed(), ledger=ledger)
        self.assertEqual(again["disposition"], packets.ACCEPTED)
        self.assertEqual(again["repeat"]["state"], packets.REPLAY)
        self.assertTrue(again["repeat"]["applied"], again)
        self.assertFalse(again["act"])
        changed = self.correction(relationship, generation=1,
                                  body=CORRECTION.replace("receiver.py", "packets.py"))
        _code, collided = self.packet_check(changed, receiver_id=CHILD,
                                            observation=self.observed(), ledger=ledger)
        self.assertEqual(collided["disposition"], packets.REFUSAL)
        self.assertIn(packets.COLLISION_MISMATCH, self.kinds(collided))
        self.assertFalse(collided["act"])

    def test_an_accepted_instruction_is_acted_on_until_it_is_recorded_applied(self):
        # The receiver checks, then crashes before it acts. The ledger holds the answer it was
        # given, and the second check is the only way the instruction reaches it again, so a
        # replay that is accepted and not yet applied still has to be acted on.
        relationship = self.registered()
        ledger = os.path.join(self.tmp, "child-ledger.json")
        one = self.correction(relationship, generation=1)
        _code, first = self.packet_check(one, receiver_id=CHILD,
                                         observation=self.observed(), ledger=ledger)
        self.assertTrue(first["act"], first)
        _code, again = self.packet_check(one, receiver_id=CHILD,
                                         observation=self.observed(), ledger=ledger)
        self.assertEqual(again["repeat"]["state"], packets.REPLAY)
        self.assertEqual(again["repeat"]["previousDisposition"], packets.ACCEPTED)
        self.assertTrue(again["act"], again)
        self.assertFalse(again["repeat"]["applied"], again)
        code, recorded = self.packet_check(one, receiver_id=CHILD, ledger=ledger, applied=True)
        self.assertEqual(code, 0, recorded)
        self.assertTrue(recorded["applied"], recorded)
        _code, after = self.packet_check(one, receiver_id=CHILD,
                                         observation=self.observed(), ledger=ledger)
        self.assertFalse(after["act"], after)
        # Recording it again changes nothing and says so.
        code, twice = self.packet_check(one, receiver_id=CHILD, ledger=ledger, applied=True)
        self.assertEqual(code, 0, twice)
        self.assertTrue(twice["alreadyApplied"], twice)

    def test_only_an_accepted_answer_can_be_recorded_applied(self):
        relationship = self.registered()
        ledger = os.path.join(self.tmp, "child-ledger.json")
        one = self.correction(relationship, generation=1)
        code, unseen = self.packet_check(one, receiver_id=CHILD, ledger=ledger, applied=True)
        self.assertEqual(code, cli.EXIT_USAGE, unseen)
        self.assertIn("never checked", unseen.get("detail", ""))
        stale = self.correction(relationship, generation=1, subject="evt-other",
                                callback=self.a_callback(pair=SUPERSEDED_PARENT))
        _code, refused = self.packet_check(stale, receiver_id=CHILD,
                                           observation=self.observed(), ledger=ledger)
        self.assertEqual(refused["disposition"], packets.REFUSAL)
        code, denied = self.packet_check(stale, receiver_id=CHILD, ledger=ledger, applied=True)
        self.assertEqual(code, cli.EXIT_USAGE, denied)
        self.assertIn(packets.REFUSAL, denied.get("detail", ""))
        self.packet_check(one, receiver_id=CHILD, observation=self.observed(), ledger=ledger)
        changed = self.correction(relationship, generation=1,
                                  body=CORRECTION.replace("receiver.py", "packets.py"))
        code, collided = self.packet_check(changed, receiver_id=CHILD, ledger=ledger,
                                           applied=True)
        self.assertEqual(code, cli.EXIT_USAGE, collided)
        self.assertIn("asks for something else", collided.get("detail", ""))
        code, bare = self.packet_check(one, receiver_id=CHILD, applied=True)
        self.assertEqual(code, cli.EXIT_USAGE, bare)

    def test_a_paused_relationship_holds_act_until_it_is_resumed(self):
        # Paused is current, so the packet is accepted; but nothing proceeds on a paused
        # relationship until relationship-resume, so it is not to be acted on yet. Nothing is
        # recorded applied, so once resumed the same packet comes back to be acted on.
        relationship = self.registered()
        rid = relationship["relationshipId"]
        ledger = os.path.join(self.tmp, "child-ledger.json")
        self.registry.set_status(rid, "paused", actor=PARENT)
        one = self.correction(relationship, generation=1)
        _code, paused = self.packet_check(one, receiver_id=CHILD, observation=self.observed(),
                                          ledger=ledger)
        self.assertEqual(paused["disposition"], packets.ACCEPTED, paused)
        self.assertFalse(paused["act"], paused)
        self.assertIn("paused", paused.get("actHeld", ""), paused)
        self.registry.resume(rid, expect_generation=1, expect_artifact_roots=[self.root],
                             expect_allowed_recipients=[PARENT, CHILD], actor=PARENT)
        _code, resumed = self.packet_check(one, receiver_id=CHILD, observation=self.observed(),
                                           ledger=ledger)
        self.assertEqual(resumed["disposition"], packets.ACCEPTED, resumed)
        self.assertEqual(resumed["repeat"]["state"], packets.REPLAY)
        self.assertTrue(resumed["act"], resumed)
        self.assertNotIn("actHeld", resumed)

    def test_applied_is_recorded_only_after_a_check_said_act(self):
        # A check that held act (paused) did not tell the receiver to act. Recording an
        # application then would make the same packet, after the resume, say act false for
        # work nobody did.
        relationship = self.registered()
        rid = relationship["relationshipId"]
        ledger = os.path.join(self.tmp, "child-ledger.json")
        self.registry.set_status(rid, "paused", actor=PARENT)
        one = self.correction(relationship, generation=1)
        _code, held = self.packet_check(one, receiver_id=CHILD, observation=self.observed(),
                                        ledger=ledger)
        self.assertFalse(held["act"], held)
        code, refused = self.packet_check(one, receiver_id=CHILD, ledger=ledger, applied=True)
        self.assertEqual(code, cli.EXIT_USAGE, refused)
        self.registry.resume(rid, expect_generation=1, expect_artifact_roots=[self.root],
                             expect_allowed_recipients=[PARENT, CHILD], actor=PARENT)
        _code, resumed = self.packet_check(one, receiver_id=CHILD, observation=self.observed(),
                                           ledger=ledger)
        self.assertTrue(resumed["act"], resumed)
        code, recorded = self.packet_check(one, receiver_id=CHILD, ledger=ledger, applied=True)
        self.assertEqual(code, 0, recorded)
        _code, after = self.packet_check(one, receiver_id=CHILD, observation=self.observed(),
                                         ledger=ledger)
        self.assertFalse(after["act"], after)

    def test_work_done_on_a_check_that_said_act_can_be_recorded_after_a_held_replay(self):
        # Told to act, the receiver acts; before it records that, the same packet is checked
        # again while the relationship is paused. The held replay does not take back what the
        # first check told it, so the application can still be recorded, and after the resume
        # the packet is not handed back to be done a second time.
        relationship = self.registered()
        rid = relationship["relationshipId"]
        ledger = os.path.join(self.tmp, "child-ledger.json")
        one = self.correction(relationship, generation=1)
        _code, first = self.packet_check(one, receiver_id=CHILD, observation=self.observed(),
                                         ledger=ledger)
        self.assertTrue(first["act"], first)
        self.registry.set_status(rid, "paused", actor=PARENT)
        _code, held = self.packet_check(one, receiver_id=CHILD, observation=self.observed(),
                                        ledger=ledger)
        self.assertFalse(held["act"], held)
        code, recorded = self.packet_check(one, receiver_id=CHILD, ledger=ledger, applied=True)
        self.assertEqual(code, 0, recorded)
        self.registry.resume(rid, expect_generation=1, expect_artifact_roots=[self.root],
                             expect_allowed_recipients=[PARENT, CHILD], actor=PARENT)
        _code, resumed = self.packet_check(one, receiver_id=CHILD, observation=self.observed(),
                                           ledger=ledger)
        self.assertFalse(resumed["act"], resumed)
        self.assertTrue(resumed["repeat"]["applied"], resumed)

    def test_an_empty_container_where_an_optional_field_belongs_is_refused(self):
        relationship = self.registered()
        base = packets.compose(direction=C2P, purpose="progress",
                               relation_id=relationship["relationshipId"], sender=CHILD,
                               recipient=PARENT, subject="progress-1", issue=ISSUE,
                               relation_revision=self.revision(relationship))
        for field, empty in (("artifact", []), ("artifact", {}), ("policy", []),
                             ("policy", {}), ("callback", {})):
            with self.subTest(field=field, empty=empty):
                one = json.loads(json.dumps(base))
                one[field] = empty
                code, answer = self.packet_check(one, receiver_id=PARENT)
                self.assertEqual(code, cli.EXIT_REFUSED, answer)

    def test_the_callback_pair_the_parent_left_is_refused(self):
        relationship = self.registered()
        one = self.correction(relationship, generation=1,
                              callback=self.a_callback(pair=SUPERSEDED_PARENT))
        _code, answer = self.packet_check(one, receiver_id=CHILD, observation=self.observed())
        self.assertEqual(answer["disposition"], packets.REFUSAL, answer)
        self.assertEqual(self.kinds(answer), [packets.STALE_CALLBACK])
        self.assertEqual(sorted(m["field"] for m in answer["mismatches"]),
                         ["callback.effort", "callback.model"])
        self.assertEqual(answer["provenance"]["callback"],
                         "relationships.parent_task_id + authorized_settings[" + PARENT + "]")

    def test_a_callback_the_parents_own_record_still_holds_is_refused_once_the_policy_moved(self):
        # The parent's settings were recorded under the pair it ran then, and the role policy
        # has since moved the parent role on. Nothing re-records the parent, so its record and
        # a correction naming the old pair agree - and the pair is still one the parent is no
        # longer authorised to run. The reading judges the parent's recorded pair against the
        # current policy for its role, as it does the child's.
        relationship = self.registered()
        roles = {**POLICY["roles"], "parent": {"model": SUPERSEDED_PARENT[0],
                                               "reasoningEffort": SUPERSEDED_PARENT[1]}}
        write_policy(Path(self.tmp), {"roles": roles})
        record_settings(self.store, self.clock, PARENT,
                        task_settings("/parent", model=SUPERSEDED_PARENT[0],
                                      reasoningEffort=SUPERSEDED_PARENT[1]),
                        source="user_transition")
        write_policy(Path(self.tmp), POLICY)
        ledger = os.path.join(self.tmp, "child-ledger.json")
        stale = self.correction(relationship, generation=1,
                                callback=self.a_callback(pair=SUPERSEDED_PARENT))
        code, answer = self.packet_check(stale, receiver_id=CHILD, observation=self.observed(),
                                         ledger=ledger)
        self.assertEqual(code, 0, answer)
        self.assertEqual(answer["disposition"], packets.REFUSAL, answer)
        self.assertEqual(self.kinds(answer), [packets.STALE_CALLBACK])
        self.assertEqual([m["field"] for m in answer["mismatches"]], ["callback.model/effort"])
        self.assertFalse(answer["act"], answer)
        # The pair the policy names now is not what the parent's record holds, so it is not
        # accepted either: the receiver cannot read that the parent runs it.
        current = self.correction(relationship, generation=1, subject="evt-current-pair",
                                  callback=self.a_callback())
        _code, answer = self.packet_check(current, receiver_id=CHILD,
                                          observation=self.observed(), ledger=ledger)
        self.assertNotEqual(answer["disposition"], packets.ACCEPTED, answer)

    def test_a_policy_naming_permissions_the_task_was_not_created_with_is_refused(self):
        # Recorded: a workspaceWrite sandbox with its defaults, and approval never.
        self.registered()
        cases = (("policy.sandbox", {"sandbox": "danger-full-access"}),
                 ("policy.sandbox", {"sandbox": {"type": "workspaceWrite",
                                                 "networkAccess": True}}),
                 ("policy.approval", {"approval": "on-request"}))
        for number, (field, change) in enumerate(cases, 1):
            with self.subTest(change=change):
                one = self.first_assignment(dispatch="dispatch-1", issue=ISSUE,
                                            subject="assign-%d" % number,
                                            policy_record=self.a_policy(**change))
                _code, answer = self.packet_check(one, receiver_id=CHILD)
                self.assertEqual(answer["disposition"], packets.REFUSAL, answer)
                self.assertEqual(self.kinds(answer), [packets.STALE_POLICY])
                self.assertEqual([m["field"] for m in answer["mismatches"]], [field])
        # The recorded object with its defaults left out, and the mode spelling, are the same.
        for sandbox in ({"type": "workspaceWrite"}, "workspace-write"):
            with self.subTest(sandbox=sandbox):
                one = self.first_assignment(dispatch="dispatch-1", issue=ISSUE,
                                            subject="assign-same",
                                            policy_record=self.a_policy(sandbox=sandbox))
                _code, answer = self.packet_check(one, receiver_id=CHILD)
                self.assertEqual(answer["disposition"], packets.ACCEPTED, answer)
                self.assertEqual(answer["provenance"]["policy"],
                                 "authorized_settings[" + CHILD + "]")

    def test_a_recorded_setting_of_another_shape_is_unread_rather_than_agreed_with(self):
        # The parent's recorded model is a number, and the packet's callback names its
        # spelling. Compared as strings the two agreed and the packet came back accepted.
        relationship = self.registered()
        self.store.db.execute(
            "UPDATE authorized_settings SET settings = json_set(settings, '$.model', 123)"
            " WHERE task_id = ?", (PARENT,))
        one = self.correction(relationship, generation=1,
                              callback=self.a_callback(pair=("123", PARENT_EFFORT)))
        code, answer = self.packet_check(one, receiver_id=CHILD, observation=self.observed())
        self.assertEqual(code, 0, answer)
        self.assertEqual(answer["disposition"], packets.UNAVAILABLE, answer)
        # And a pair that is not text cannot be judged against the role policy either, so
        # whether the callback is still authorised is unread as well.
        self.assertEqual(self.gap_fields(answer), ["callback", "callback.model"])

    def test_an_envelope_of_the_wrong_shape_is_refused_as_a_packet(self):
        relationship = self.registered()
        for name, value in (("sender", []), ("recipient", 7), ("sender", "01parent-task")):
            with self.subTest(name=name, value=value):
                one = self.correction(relationship, generation=1)
                one["envelope"][name] = value
                code, answer = self.packet_check(one, receiver_id=CHILD,
                                                 observation=self.observed())
                self.assertEqual(code, cli.EXIT_REFUSED, answer)

    def test_text_that_cannot_be_encoded_is_refused_as_a_packet(self):
        relationship = self.registered()
        one = self.correction(relationship, generation=1)
        one["envelope"]["subject"] = "\ud800"
        code, answer = self.packet_check(one, receiver_id=CHILD, observation=self.observed())
        self.assertEqual(code, cli.EXIT_REFUSED, answer)

    def test_a_non_pull_request_audit_passes_without_being_asked_for_a_head(self):
        relationship = self.registered()
        audit = packets.locator(path="/state/crw/crw-149/evidence/audit.md", digest="9" * 64)
        one = self.report(relationship, self.event(relationship), artifact=audit)
        _code, answer = self.packet_check(one, receiver_id=PARENT, observation={
            "source": "sha256 of the file read by the receiver",
            "artifactPath": "/state/crw/crw-149/evidence/audit.md", "artifactDigest": "9" * 64})
        self.assertEqual(answer["disposition"], packets.ACCEPTED, answer)
        self.assertFalse([g for g in answer["gaps"] if "head" in g["field"].lower()])

    def test_an_ended_relationship_takes_no_packet(self):
        relationship = self.registered()
        one = self.correction(relationship, generation=1)
        self.registry.set_status(relationship["relationshipId"], "archived", actor=PARENT)
        _code, answer = self.packet_check(one, receiver_id=CHILD, observation=self.observed())
        self.assertIn(packets.RELATION_STATUS, [m["field"] for m in answer["mismatches"]])


class AFirstAssignmentThroughCreateAndRegister(StoreReception):
    CREATED = "01created-child"

    def test_it_is_unavailable_before_registration_and_accepted_after(self):
        one = self.first_assignment(dispatch="dispatch-first")
        ledger = os.path.join(self.tmp, "created-ledger.json")
        _code, before = self.packet_check(one, receiver_id=self.CREATED, ledger=ledger)
        self.assertEqual(before["disposition"], packets.UNAVAILABLE, before)
        self.assertEqual(before["mismatches"], [])
        self.assertIn(packets.RELATION_STATUS, self.gap_fields(before))
        self.assertFalse(before["act"])
        # Creation returns the task id; registration binds the dispatch to it.
        self.registered(child=self.CREATED, issue="REL-FIRST", dispatch="dispatch-first",
                        turn="turn-first")
        _code, after = self.packet_check(one, receiver_id=self.CREATED, ledger=ledger)
        self.assertEqual(after["disposition"], packets.ACCEPTED, after)
        self.assertTrue(after["act"])
        self.assertEqual([entry["field"] for entry in after["instructed"]],
                         [packets.MODE, "workflow"])
        with open(ledger, encoding="utf-8") as handle:
            held = json.load(handle)
        self.assertEqual(list(held["assignments"].values())[0]["mode"], packets.LOOP)

    def test_a_dispatch_that_did_not_open_the_generation_is_another_relation(self):
        self.registered(child=self.CREATED, issue="REL-FIRST", dispatch="dispatch-first",
                        turn="turn-first")
        _code, answer = self.packet_check(self.first_assignment(dispatch="dispatch-other"),
                                          receiver_id=self.CREATED)
        self.assertEqual(self.kinds(answer), [packets.WRONG_RELATION])

    def test_a_shared_child_takes_the_first_assignment_its_dispatch_opened(self):
        # One child task in two live relationships: the dispatch id a first assignment carries
        # names no relationship, so it has to select the one whose generation it opened. A
        # project scope binds a task to one issue, so the shared child is an unscoped one.
        self.registered(child=self.CREATED, issue="REL-A", dispatch="dispatch-a",
                        turn="turn-a", project=None)
        self.registered(child=self.CREATED, issue="REL-B", dispatch="dispatch-b",
                        turn="turn-b", project=None)
        for issue, dispatch in (("REL-A", "dispatch-a"), ("REL-B", "dispatch-b")):
            with self.subTest(dispatch=dispatch):
                one = self.first_assignment(dispatch=dispatch, issue=issue, subject=issue)
                _code, answer = self.packet_check(one, receiver_id=self.CREATED)
                self.assertEqual(answer["disposition"], packets.ACCEPTED, answer)
                self.assertEqual(answer["record"]["dispatchRequestId"], dispatch)
                self.assertEqual(answer["record"]["issue"], issue)
        _code, other = self.packet_check(
            self.first_assignment(dispatch="dispatch-c", issue="REL-A", subject="REL-A"),
            receiver_id=self.CREATED)
        self.assertNotEqual(other["disposition"], packets.ACCEPTED, other)

    def test_a_later_packet_cannot_redefine_the_mode_the_ledger_holds(self):
        relationship = self.registered(child=self.CREATED, issue="REL-FIRST",
                                       dispatch="dispatch-first", turn="turn-first")
        ledger = os.path.join(self.tmp, "created-ledger.json")
        _code, first = self.packet_check(self.first_assignment(dispatch="dispatch-first"),
                                         receiver_id=self.CREATED, ledger=ledger)
        self.assertTrue(first["act"], first)
        resume = packets.compose(
            direction=P2C, purpose="resume", relation_id=relationship["relationshipId"],
            sender=PARENT, recipient=self.CREATED, subject="resume-1", issue="REL-FIRST",
            relation_revision=self.revision(relationship), callback=self.a_callback(),
            policy_record=self.a_policy(mode=packets.NON_LOOP,
                                        workflow="an authorised read-only audit"),
            artifact=packets.pull_request(repository=REPOSITORY, number=107, head_sha=HEAD))
        _code, answer = self.packet_check(resume, receiver_id=self.CREATED,
                                          observation=self.observed(), ledger=ledger)
        # This resume names another mode and another workflow, and each is refused by name.
        self.assertEqual(self.kinds(answer), [packets.WRONG_MODE, packets.WRONG_WORKFLOW])
        self.assertEqual(answer["provenance"]["mode"],
                         "ledger: assignment " + first["messageId"])

    def test_a_returning_tenure_takes_its_own_assignment(self):
        # A -> B -> A: the returning registration reuses the relationship id under a later
        # generation opened by a new dispatch. The mode and workflow the ledger holds belong to
        # the first tenure, so the new tenure's assignment defines its own and replaces them.
        relationship = self.registered(project=None)
        ledger = os.path.join(self.tmp, "return-ledger.json")
        _code, first = self.packet_check(self.first_assignment(dispatch="dispatch-1",
                                                               issue=ISSUE),
                                         receiver_id=CHILD, ledger=ledger)
        self.assertEqual(first["disposition"], packets.ACCEPTED, first)
        successor = "01child-b"
        self.adapter.add_thread(successor)
        other = self.registry.register(
            parent=self.parent,
            child=Endpoint(successor, HOST, cwd=self.root, cxc_session="cxc-b"),
            issue_key=ISSUE, artifact_roots=[self.root], allowed_recipients=[PARENT, successor],
            dispatch_request_id="dispatch-b", dispatch_turn_id="turn-b",
            supersedes=relationship["relationshipId"], project_key=None)
        record_settings(self.store, self.clock, successor,
                        task_settings(self.root, model=CHILD_MODEL,
                                      reasoningEffort=CHILD_EFFORT),
                        source="creation_result")
        returned = self.registry.register(
            parent=self.parent,
            child=Endpoint(CHILD, HOST, cwd=self.root, cxc_session="cxc-" + CHILD),
            issue_key=ISSUE, artifact_roots=[self.root], allowed_recipients=[PARENT, CHILD],
            dispatch_request_id="dispatch-return", dispatch_turn_id="turn-return",
            supersedes=other["relationshipId"], project_key=None)
        self.assertEqual(returned["relationshipId"], relationship["relationshipId"])
        again = self.first_assignment(
            dispatch="dispatch-return", issue=ISSUE,
            policy_record=self.a_policy(mode=packets.NON_LOOP,
                                        workflow="an authorised read-only audit"))
        _code, answer = self.packet_check(again, receiver_id=CHILD, ledger=ledger)
        self.assertEqual(answer["disposition"], packets.ACCEPTED, answer)
        self.assertEqual(sorted(entry["field"] for entry in answer["instructed"]),
                         [packets.MODE, "workflow"])
        with open(ledger, encoding="utf-8") as handle:
            held = json.load(handle)["assignments"][returned["relationshipId"]]
        self.assertEqual((held["dispatchRequestId"], held["mode"]),
                         ("dispatch-return", packets.NON_LOOP))

    def test_a_revision_generation_keeps_the_tenures_mode_and_workflow(self):
        # A correction opens a generation under a new dispatch inside the same tenure. The
        # mode and workflow the tenure's assignment gave still hold there, so a resume after
        # the correction is read against them rather than left unread.
        relationship = self.registered(child=self.CREATED, issue="REL-FIRST",
                                       dispatch="dispatch-first", turn="turn-first")
        rid = relationship["relationshipId"]
        ledger = os.path.join(self.tmp, "created-ledger.json")
        _code, first = self.packet_check(self.first_assignment(dispatch="dispatch-first"),
                                         receiver_id=self.CREATED, ledger=ledger)
        self.assertTrue(first["act"], first)
        self.registry.open_generation(rid, dispatch_request_id="dispatch-rev-2",
                                      reason="needs_changes_revision")
        correction = self.correction(relationship, generation=2, recipient=self.CREATED,
                                     issue="REL-FIRST")
        _code, corrected = self.packet_check(correction, receiver_id=self.CREATED,
                                             observation=self.observed(), ledger=ledger)
        self.assertEqual(corrected["disposition"], packets.ACCEPTED, corrected)
        resume = packets.compose(
            direction=P2C, purpose="resume", relation_id=rid, sender=PARENT,
            recipient=self.CREATED, subject="resume-after-correction", issue="REL-FIRST",
            relation_revision=self.revision(relationship), callback=self.a_callback(),
            policy_record=self.a_policy(),
            artifact=packets.pull_request(repository=REPOSITORY, number=107, head_sha=HEAD))
        _code, resumed = self.packet_check(resume, receiver_id=self.CREATED,
                                           observation=self.observed(), ledger=ledger)
        self.assertEqual(resumed["disposition"], packets.ACCEPTED, resumed)
        self.assertEqual(resumed["provenance"].get("mode"),
                         "ledger: assignment " + first["messageId"])
        # And a resume naming another workflow in that generation is still refused.
        other = packets.compose(
            direction=P2C, purpose="resume", relation_id=rid, sender=PARENT,
            recipient=self.CREATED, subject="resume-other", issue="REL-FIRST",
            relation_revision=self.revision(relationship), callback=self.a_callback(),
            policy_record=self.a_policy(workflow="CXC Loop under another procedure"),
            artifact=packets.pull_request(repository=REPOSITORY, number=107, head_sha=HEAD))
        _code, refused = self.packet_check(other, receiver_id=self.CREATED,
                                           observation=self.observed(), ledger=ledger)
        self.assertEqual(self.kinds(refused), [packets.WRONG_WORKFLOW])

    def resume_for(self, relationship, recipient, issue, subject, **overrides):
        base = dict(direction=P2C, purpose="resume", relation_id=relationship["relationshipId"],
                    sender=PARENT, recipient=recipient, subject=subject, issue=issue,
                    relation_revision=self.revision(relationship), callback=self.a_callback(),
                    policy_record=self.a_policy(),
                    artifact=packets.pull_request(repository=REPOSITORY, number=107,
                                                  head_sha=HEAD))
        base.update(overrides)
        return packets.compose(**base)

    def test_an_assignment_accepted_in_a_revision_generation_is_held_for_the_tenure(self):
        relationship = self.registered()
        rid = relationship["relationshipId"]
        ledger = os.path.join(self.tmp, "child-ledger.json")
        self.registry.open_generation(rid, dispatch_request_id="dispatch-rev-2",
                                      reason="needs_changes_revision")
        assignment = packets.compose(
            direction=P2C, purpose="assignment", relation_id=rid, sender=PARENT,
            recipient=CHILD, subject="assignment-in-2", issue=ISSUE,
            relation_revision=self.revision(relationship), criteria_digest=self.digest(),
            policy_record=self.a_policy(), callback=self.a_callback(), body=BODY)
        _code, answer = self.packet_check(assignment, receiver_id=CHILD, ledger=ledger)
        self.assertEqual(answer["disposition"], packets.ACCEPTED, answer)
        with open(ledger, encoding="utf-8") as handle:
            self.assertEqual(json.load(handle)["assignments"][rid]["dispatchRequestId"],
                             "dispatch-1")
        _code, resumed = self.packet_check(
            self.resume_for(relationship, CHILD, ISSUE, "resume-in-2"),
            receiver_id=CHILD, observation=self.observed(), ledger=ledger)
        self.assertEqual(resumed["disposition"], packets.ACCEPTED, resumed)
        other = packets.compose(
            direction=P2C, purpose="assignment", relation_id=rid, sender=PARENT,
            recipient=CHILD, subject="assignment-other", issue=ISSUE,
            relation_revision=self.revision(relationship), criteria_digest=self.digest(),
            policy_record=self.a_policy(workflow="CXC Loop under another procedure"),
            callback=self.a_callback(), body=BODY)
        _code, refused = self.packet_check(other, receiver_id=CHILD, ledger=ledger)
        self.assertEqual(self.kinds(refused), [packets.WRONG_WORKFLOW])

    def test_only_a_registration_begins_a_tenure(self):
        # A generation opened with the initial_assignment reason is still a generation, not a
        # registration: the tenure and what its assignment gave stand.
        relationship = self.registered()
        rid = relationship["relationshipId"]
        ledger = os.path.join(self.tmp, "child-ledger.json")
        _code, first = self.packet_check(self.first_assignment(dispatch="dispatch-1",
                                                               issue=ISSUE),
                                         receiver_id=CHILD, ledger=ledger)
        self.assertTrue(first["act"], first)
        self.registry.open_generation(rid, dispatch_request_id="dispatch-x",
                                      reason="initial_assignment")
        _code, resumed = self.packet_check(
            self.resume_for(relationship, CHILD, ISSUE, "resume-after-x"),
            receiver_id=CHILD, observation=self.observed(), ledger=ledger)
        self.assertEqual(resumed["disposition"], packets.ACCEPTED, resumed)

    def returned(self, relationship):
        """A -> B -> A on one unscoped relationship id; the returned registration."""
        successor = "01child-b"
        self.adapter.add_thread(successor)
        other = self.registry.register(
            parent=self.parent,
            child=Endpoint(successor, HOST, cwd=self.root, cxc_session="cxc-b"),
            issue_key=ISSUE, artifact_roots=[self.root], allowed_recipients=[PARENT, successor],
            dispatch_request_id="dispatch-b", dispatch_turn_id="turn-b",
            supersedes=relationship["relationshipId"], project_key=None)
        record_settings(self.store, self.clock, successor,
                        task_settings(self.root, model=CHILD_MODEL,
                                      reasoningEffort=CHILD_EFFORT),
                        source="creation_result")
        return self.registry.register(
            parent=self.parent,
            child=Endpoint(CHILD, HOST, cwd=self.root, cxc_session="cxc-" + CHILD),
            issue_key=ISSUE, artifact_roots=[self.root], allowed_recipients=[PARENT, CHILD],
            dispatch_request_id="dispatch-return", dispatch_turn_id="turn-return",
            supersedes=other["relationshipId"], project_key=None)

    def test_a_returned_tenure_whose_opening_row_is_gone_is_unread_not_the_old_one(self):
        relationship = self.registered(project=None)
        rid = relationship["relationshipId"]
        ledger = os.path.join(self.tmp, "return-ledger.json")
        self.packet_check(self.first_assignment(dispatch="dispatch-1", issue=ISSUE),
                          receiver_id=CHILD, ledger=ledger)
        back = self.returned(relationship)
        self.registry.open_generation(rid, dispatch_request_id="dispatch-rev",
                                      reason="needs_changes_revision")
        self.store.db.execute(
            "DELETE FROM generations WHERE relationship_id = ? AND execution_generation = ?",
            (rid, back["executionGeneration"]))
        _code, answer = self.packet_check(
            self.resume_for(relationship, CHILD, ISSUE, "resume-returned",
                            generation=back["executionGeneration"] + 1),
            receiver_id=CHILD, observation=self.observed(), ledger=ledger)
        self.assertNotEqual(answer["disposition"], packets.ACCEPTED, answer)
        self.assertFalse(answer["act"], answer)

    def test_a_policy_packet_whose_tenure_is_unread_is_not_accepted(self):
        # The policy is read against, or defines, the mode and workflow of the current
        # tenure. With the tenure unread, an assignment accepted here could not be recorded
        # for it, so it is unavailable rather than acted on.
        relationship = self.registered()
        ledger = os.path.join(self.tmp, "child-ledger.json")
        self.store.db.execute("DELETE FROM journal WHERE kind = 'relationship_registered'"
                              " AND subject = ?", (relationship["relationshipId"],))
        _code, answer = self.packet_check(self.first_assignment(dispatch="dispatch-1",
                                                                issue=ISSUE),
                                          receiver_id=CHILD, ledger=ledger)
        self.assertEqual(answer["disposition"], packets.UNAVAILABLE, answer)
        self.assertIn("tenureGeneration", self.gap_fields(answer))
        self.assertFalse(answer["act"], answer)
        with open(ledger, encoding="utf-8") as handle:
            self.assertEqual(json.load(handle)["assignments"], {})

    def test_a_returned_tenure_with_a_damaged_generation_is_unread_not_a_host_failure(self):
        relationship = self.registered(project=None)
        ledger = os.path.join(self.tmp, "return-ledger.json")
        self.returned(relationship)
        self.store.db.execute("UPDATE relationships SET execution_generation = 'damaged'"
                              " WHERE relationship_id = ?", (relationship["relationshipId"],))
        code, answer = self.packet_check(
            self.resume_for(relationship, CHILD, ISSUE, "resume-damaged"),
            receiver_id=CHILD, observation=self.observed(), ledger=ledger)
        self.assertEqual(code, 0, answer)
        self.assertNotEqual(answer["disposition"], packets.ACCEPTED, answer)

    def test_a_tenure_the_store_answers_two_ways_is_unread(self):
        # The registration journal and the generation rows each say where the current tenure
        # began. Where one of them is damaged they disagree, and the tenure is unread rather
        # than taken from whichever still answers - which, for a lost reopening, was the
        # earlier tenure.
        damage = (("UPDATE journal SET kind = x'00' WHERE kind = 'relationship_tenure_reopened'"
                   " AND subject = ?"),
                  ("UPDATE generations SET reason = 'needs_changes_revision'"
                   " WHERE relationship_id = ? AND reason IS NULL"),
                  # A reopening the reader cannot parse at all: JSON nested deeper than it can
                  # descend. Unread, never a host failure.
                  ("UPDATE journal SET detail = replace(hex(zeroblob(15000)), '00', '[')"
                   " || '0' || replace(hex(zeroblob(15000)), '00', ']')"
                   " WHERE kind = 'relationship_tenure_reopened' AND subject = ?"))
        for number, statement in enumerate(damage, 1):
            with self.subTest(damage=statement):
                child, issue = CHILD + "-twoways%d" % number, "REL-TWO-%d" % number
                self.adapter.add_thread(child)
                relationship = self.registered(child=child, issue=issue, project=None,
                                               dispatch="dispatch-two-%d" % number)
                rid = relationship["relationshipId"]
                ledger = os.path.join(self.tmp, "two-%d.json" % number)
                # The first tenure's assignment is held, so the earlier tenure's mode and
                # workflow would answer a resume if the earlier tenure were taken as current.
                _code, first = self.packet_check(
                    self.first_assignment(dispatch="dispatch-two-%d" % number, issue=issue,
                                          subject=issue),
                    receiver_id=child, ledger=ledger)
                self.assertEqual(first["disposition"], packets.ACCEPTED, first)
                stale = self.resume_for(relationship, child, issue, "resume-two-%d" % number)
                successor = child + "-b"
                self.adapter.add_thread(successor)
                other = self.registry.register(
                    parent=self.parent,
                    child=Endpoint(successor, HOST, cwd=self.root, cxc_session="cxc-" + successor),
                    issue_key=issue, artifact_roots=[self.root],
                    allowed_recipients=[PARENT, successor],
                    dispatch_request_id="dispatch-two-b-%d" % number,
                    dispatch_turn_id="turn-two-b-%d" % number, supersedes=rid, project_key=None)
                record_settings(self.store, self.clock, successor,
                                task_settings(self.root, model=CHILD_MODEL,
                                              reasoningEffort=CHILD_EFFORT),
                                source="creation_result")
                self.registry.register(
                    parent=self.parent,
                    child=Endpoint(child, HOST, cwd=self.root, cxc_session="cxc-" + child),
                    issue_key=issue, artifact_roots=[self.root],
                    allowed_recipients=[PARENT, child],
                    dispatch_request_id="dispatch-two-return-%d" % number,
                    dispatch_turn_id="turn-two-return-%d" % number,
                    supersedes=other["relationshipId"], project_key=None)
                self.store.db.execute(statement, (rid,))
                code, answer = self.packet_check(stale, receiver_id=child,
                                                 observation=self.observed(), ledger=ledger)
                self.assertEqual(code, 0, answer)
                self.assertNotEqual(answer["disposition"], packets.ACCEPTED, answer)
                self.assertFalse(answer["act"], answer)

    def test_a_store_value_of_the_wrong_shape_never_ends_as_a_host_failure(self):
        # Every column the reader reads, holding what SQLite lets any column hold. The answer
        # is a refusal or a reading that says what it could not read - never a host error -
        # and a compared column of the wrong shape never agrees.
        columns = (("relationships", "execution_generation", True),
                   ("relationships", "issue_key", True),
                   ("relationships", "parent_task_id", True),
                   ("relationships", "status", True),
                   ("relationships", "supersedes", False),
                   ("generations", "dispatch_request_id", True),
                   ("generations", "execution_generation", True),
                   ("generations", "reason", False),
                   ("scope_links", "revision", True),
                   ("scope_links", "status", True),
                   ("scope_links", "lower_task_id", True),
                   ("scope_links", "superseded_by", True),
                   ("authorized_settings", "settings", True))
        number = 0
        for table, column, compared in columns:
            for shape in ("damaged", 7, 2.5):
                number += 1
                with self.subTest(table=table, column=column, shape=shape):
                    child, issue = CHILD + "-shape%d" % number, "REL-SHAPE-%d" % number
                    relationship = self.registered(child=child, issue=issue,
                                                   dispatch="dispatch-shape-%d" % number)
                    rid = relationship["relationshipId"]
                    ledger = os.path.join(self.tmp, "shape-%d.json" % number)
                    self.packet_check(self.first_assignment(dispatch="dispatch-shape-%d" % number,
                                                            issue=issue, subject=issue),
                                      receiver_id=child, ledger=ledger)
                    resume = self.resume_for(relationship, child, issue, "resume-%d" % number)
                    if table == "scope_links":
                        link = self.linkage.attachment(rid)["link"]["linkId"]
                        where, key = "link_id = ?", link
                    elif table == "authorized_settings":
                        where, key = "task_id = ?", PARENT
                        shape = json.dumps({"model": shape})
                    else:
                        where, key = "relationship_id = ?", rid
                    try:
                        self.store.db.execute("UPDATE %s SET %s = ? WHERE %s"
                                              % (table, column, where), (shape, key))
                    except Exception:  # noqa: BLE001 - a shape the schema itself refuses
                        continue
                    code, answer = self.packet_check(resume, receiver_id=child,
                                                     observation=self.observed(), ledger=ledger)
                    self.assertNotEqual(code, cli.EXIT_HOST, answer)
                    if compared and code == 0:
                        self.assertNotEqual(answer["disposition"], packets.ACCEPTED, answer)
                    if table == "authorized_settings":
                        record_settings(self.store, self.clock, PARENT,
                                        task_settings("/parent", model=PARENT_MODEL,
                                                      reasoningEffort=PARENT_EFFORT),
                                        source="creation_result")

    def test_settings_nested_too_deep_to_read_leave_their_field_unread(self):
        # Settings the reader cannot parse - JSON nested deeper than the decoder descends - or
        # whose model is not the text every writer records answer nothing for it. The policy
        # or the callback read from them is a gap: never a host failure, never agreement, and
        # the rest of the reading still stands, at every depth and on every interpreter.
        relationship = self.registered()
        packet = self.first_assignment(dispatch="dispatch-1", issue=ISSUE)
        _code, before = self.packet_check(packet, receiver_id=CHILD)
        self.assertEqual(before["disposition"], packets.ACCEPTED, before)
        marker = '"deep-value-marker"'
        for task, field in ((CHILD, packets.POLICY), (PARENT, packets.CALLBACK)):
            (original,) = self.store.db.execute(
                "SELECT settings FROM authorized_settings WHERE task_id = ?",
                (task,)).fetchone()
            data = json.loads(original)
            for depth in (40, 900, 5000, 15000):
                deep = "[" * depth + '"x"' + "]" * depth
                placements = {
                    "whole": deep,
                    "model": json.dumps(dict(data, model="deep-value-marker")),
                }
                if task == CHILD:
                    # The one structured value the reading copies: bounded, so unread here.
                    placements["sandbox"] = json.dumps(dict(data, sandbox=dict(
                        data["sandbox"], extra="deep-value-marker")))
                for placement, text in placements.items():
                    with self.subTest(task=task, depth=depth, placement=placement):
                        code, answer = self.checked_under(task, text.replace(marker, deep),
                                                          packet)
                        self.assertNotEqual(code, cli.EXIT_HOST, answer)
                        self.assertEqual(answer["disposition"], packets.UNAVAILABLE, answer)
                        self.assertTrue(any(gap == field or gap.startswith(field + ".")
                                            for gap in self.gap_fields(answer)), answer)
                        self.assertEqual(answer["record"].get("relationId"),
                                         relationship["relationshipId"], answer)

    def test_deep_values_a_writer_records_are_read_wherever_they_parse(self):
        # The writer bounds what a reading answers with - the model, effort and approval are
        # text, the sandbox a policy object it can read - and nothing else. A value nested deep
        # under a key the reading never uses is a record it accepts, so the reading accepts it
        # as well: a bound over the whole row turned a legal first assignment unavailable.
        # Where the decoder stops descending, the settings are unread, and at that edge the
        # answer is still an answer - compared and printed - never a host failure.
        self.registered()
        packet = self.first_assignment(dispatch="dispatch-1", issue=ISSUE)
        deep = "x"
        for _ in range(40):
            deep = [deep]
        for task, cwd, model, effort in ((CHILD, self.root, CHILD_MODEL, CHILD_EFFORT),
                                         (PARENT, "/parent", PARENT_MODEL, PARENT_EFFORT)):
            (original,) = self.store.db.execute(
                "SELECT settings FROM authorized_settings WHERE task_id = ?",
                (task,)).fetchone()
            legal = task_settings(cwd, model=model, reasoningEffort=effort)
            for placement, settings in (
                    ("creationMetadata", dict(legal, creationMetadata=deep)),
                    ("sandbox", dict(legal, sandbox=dict(legal["sandbox"], extra=[["x"]])))):
                with self.subTest(task=task, placement=placement):
                    record_settings(self.store, self.clock, task, settings,
                                    source="creation_result")
                    try:
                        code, answer = self.packet_check(packet, receiver_id=CHILD)
                    finally:
                        self.store.db.execute(
                            "UPDATE authorized_settings SET settings = ? WHERE task_id = ?",
                            (original, task))
                    self.assertEqual(code, 0, answer)
                    self.assertEqual(answer["disposition"], packets.ACCEPTED, answer)
        (original,) = self.store.db.execute(
            "SELECT settings FROM authorized_settings WHERE task_id = ?", (CHILD,)).fetchone()
        data = json.loads(original)

        # The deepest value this reader parses here, found rather than assumed, under a key the
        # reading never uses (accepted up to the edge) and inside the sandbox it copies (the
        # sandbox unread up to the edge). Past it the settings are unread. Every probe on the
        # way is an answer; none is a host failure.
        def unused(answer):
            if answer["disposition"] == packets.ACCEPTED:
                return True
            self.assertEqual(self.gap_fields(answer), [packets.POLICY], answer)
            return False

        def sandbox(answer):
            self.assertEqual(answer["disposition"], packets.UNAVAILABLE, answer)
            if self.gap_fields(answer) == ["policy.sandbox"]:
                return True
            self.assertEqual(self.gap_fields(answer), [packets.POLICY], answer)
            return False

        for placement, parsed in (("creationMetadata", unused), ("sandbox", sandbox)):
            with self.subTest(edge=placement):
                if placement == "sandbox":
                    shaped = dict(data, sandbox=dict(data["sandbox"], extra="deep-value-marker"))
                else:
                    shaped = dict(data, creationMetadata="deep-value-marker")
                self.parse_edge(CHILD, json.dumps(shaped), packet, parsed)

    def parse_edge(self, task, shaped, packet, parsed):
        """The deepest marker depth parsed(answer) holds for, every probe an answer."""
        def probe(depth):
            text = shaped.replace('"deep-value-marker"', "[" * depth + '"x"' + "]" * depth)
            code, answer = self.checked_under(task, text, packet)
            self.assertEqual(code, 0, (depth, answer))
            return parsed(answer)

        low, high = 40, 20000
        self.assertTrue(probe(low))
        self.assertFalse(probe(high))
        while high - low > 1:
            middle = (low + high) // 2
            if probe(middle):
                low = middle
            else:
                high = middle
        return low

    def checked_under(self, task, settings_text, packet):
        """packet-check with the task's settings row replaced as written, then restored."""
        (original,) = self.store.db.execute(
            "SELECT settings FROM authorized_settings WHERE task_id = ?", (task,)).fetchone()
        self.store.db.execute("UPDATE authorized_settings SET settings = ? WHERE task_id = ?",
                              (settings_text, task))
        try:
            return self.packet_check(packet, receiver_id=CHILD)
        finally:
            self.store.db.execute(
                "UPDATE authorized_settings SET settings = ? WHERE task_id = ?",
                (original, task))

    def test_a_packet_from_an_earlier_tenure_of_an_unscoped_relationship_is_not_accepted(self):
        relationship = self.registered(project=None)
        ledger = os.path.join(self.tmp, "return-ledger.json")
        stale = self.resume_for(relationship, CHILD, ISSUE, "resume-tenure-1")
        # One tenure: a generation-less packet on an unscoped relationship is not stranded.
        self.packet_check(self.first_assignment(dispatch="dispatch-1", issue=ISSUE),
                          receiver_id=CHILD, ledger=ledger)
        _code, current = self.packet_check(stale, receiver_id=CHILD,
                                           observation=self.observed(), ledger=ledger)
        self.assertEqual(current["disposition"], packets.ACCEPTED, current)
        back = self.returned(relationship)
        self.packet_check(self.first_assignment(dispatch="dispatch-return", issue=ISSUE,
                                                subject="REL-RETURN"),
                          receiver_id=CHILD, ledger=ledger)
        # Delayed from the first tenure: no revision and no generation tie it to this one.
        late = self.resume_for(relationship, CHILD, ISSUE, "resume-tenure-1-late")
        _code, answer = self.packet_check(late, receiver_id=CHILD,
                                          observation=self.observed(), ledger=ledger)
        self.assertNotEqual(answer["disposition"], packets.ACCEPTED, answer)
        self.assertFalse(answer["act"], answer)
        # One that states the generation it belongs to is compared on it.
        old = self.resume_for(relationship, CHILD, ISSUE, "resume-gen-1", generation=1)
        _code, answer = self.packet_check(old, receiver_id=CHILD,
                                          observation=self.observed(), ledger=ledger)
        self.assertEqual(self.kinds(answer), [packets.STALE_GENERATION], answer)
        now = self.resume_for(relationship, CHILD, ISSUE, "resume-now",
                              generation=back["executionGeneration"])
        _code, answer = self.packet_check(now, receiver_id=CHILD,
                                          observation=self.observed(), ledger=ledger)
        self.assertEqual(answer["disposition"], packets.ACCEPTED, answer)

    def test_a_policy_of_the_wrong_shape_is_refused_and_leaves_the_ledger_usable(self):
        # Read back from disk, a packet never went through policy(). A workflow that is not
        # text used to be accepted and written into the ledger, which the next check then
        # refused as damaged: one malformed packet stopped every later reception.
        self.registered(child=self.CREATED, issue="REL-FIRST", dispatch="dispatch-first",
                        turn="turn-first")
        ledger = os.path.join(self.tmp, "created-ledger.json")
        for name, value in (("workflow", 123), ("model", 123), ("effort", ["xhigh"])):
            with self.subTest(name=name):
                one = self.first_assignment(dispatch="dispatch-first", subject="bad-" + name)
                one[packets.POLICY][name] = value
                code, answer = self.packet_check(one, receiver_id=self.CREATED, ledger=ledger)
                self.assertEqual(code, cli.EXIT_REFUSED, answer)
                self.assertIn(name, answer.get("detail", ""))
        code, answer = self.packet_check(self.first_assignment(dispatch="dispatch-first"),
                                         receiver_id=self.CREATED, ledger=ledger)
        self.assertEqual(code, 0, answer)
        self.assertEqual(answer["disposition"], packets.ACCEPTED, answer)

    def test_a_later_packet_cannot_replace_the_workflow_the_ledger_holds(self):
        # Same mode, another workflow: the workflow is what no transport carries, so the one
        # the accepted assignment gave is the receiver's reading, and a resume restates it.
        relationship = self.registered(child=self.CREATED, issue="REL-FIRST",
                                       dispatch="dispatch-first", turn="turn-first")
        ledger = os.path.join(self.tmp, "created-ledger.json")
        _code, first = self.packet_check(self.first_assignment(dispatch="dispatch-first"),
                                         receiver_id=self.CREATED, ledger=ledger)
        self.assertTrue(first["act"], first)
        resume = packets.compose(
            direction=P2C, purpose="resume", relation_id=relationship["relationshipId"],
            sender=PARENT, recipient=self.CREATED, subject="resume-2", issue="REL-FIRST",
            relation_revision=self.revision(relationship), callback=self.a_callback(),
            policy_record=self.a_policy(workflow="CXC Loop under another procedure"),
            artifact=packets.pull_request(repository=REPOSITORY, number=107, head_sha=HEAD))
        _code, answer = self.packet_check(resume, receiver_id=self.CREATED,
                                          observation=self.observed(), ledger=ledger)
        self.assertEqual(answer["disposition"], packets.REFUSAL, answer)
        self.assertEqual(self.kinds(answer), ["wrong_workflow"])
        self.assertEqual(answer["provenance"].get("workflow"),
                         "ledger: assignment " + first["messageId"])


class WhatTheStoreCannotAnswer(StoreReception):
    def test_criteria_that_are_not_registered_leave_the_digest_unchecked(self):
        relationship = self.registered()
        event = self.event(relationship)
        self.store.db.execute("DELETE FROM canonical_criteria WHERE relationship_id = ?",
                              (relationship["relationshipId"],))
        _code, answer = self.packet_check(self.report(relationship, event), receiver_id=PARENT,
                                          observation=self.observed())
        self.assertEqual(answer["disposition"], packets.UNAVAILABLE, answer)
        self.assertEqual(self.gap_fields(answer), [packets.CRITERIA_DIGEST])

    def test_a_digest_stated_unasked_is_unchecked_where_no_criteria_are_registered(self):
        relationship = self.registered()
        self.store.db.execute("DELETE FROM canonical_criteria WHERE relationship_id = ?",
                              (relationship["relationshipId"],))
        one = packets.compose(
            direction=C2P, purpose="blocked", relation_id=relationship["relationshipId"],
            sender=CHILD, recipient=PARENT, subject="blocked-unregistered", issue=ISSUE,
            relation_revision=self.revision(relationship), criteria_digest=self.digest(),
            evidence=EVIDENCE)
        _code, answer = self.packet_check(one, receiver_id=PARENT,
                                          ledger=os.path.join(self.tmp, "parent-ledger.json"))
        self.assertEqual(answer["disposition"], packets.UNAVAILABLE, answer)
        self.assertEqual(self.gap_fields(answer), [packets.CRITERIA_DIGEST])
        self.assertFalse(answer["act"], answer)

    def test_a_current_generation_no_dispatch_opened_leaves_the_tenure_unread(self):
        # Which dispatch opened the current generation is what says which tenure a packet
        # belongs to. Unread, nothing is accepted and no assignment is recorded against it.
        relationship = self.registered()
        ledger = os.path.join(self.tmp, "child-ledger.json")
        self.store.db.execute("DELETE FROM generations WHERE relationship_id = ?",
                              (relationship["relationshipId"],))
        _code, answer = self.packet_check(self.correction(relationship, generation=1),
                                          receiver_id=CHILD, observation=self.observed(),
                                          ledger=ledger)
        self.assertEqual(answer["disposition"], packets.UNAVAILABLE, answer)
        self.assertIn(packets.DISPATCH_REQUEST, self.gap_fields(answer))
        self.assertFalse(answer["act"], answer)
        late = packets.compose(
            direction=P2C, purpose="assignment", relation_id=relationship["relationshipId"],
            sender=PARENT, recipient=CHILD, subject="late-assignment", issue=ISSUE,
            relation_revision=self.revision(relationship), criteria_digest=self.digest(),
            policy_record=self.a_policy(), callback=self.a_callback(), body=BODY)
        _code, answer = self.packet_check(late, receiver_id=CHILD, ledger=ledger)
        self.assertEqual(answer["disposition"], packets.UNAVAILABLE, answer)
        with open(ledger, encoding="utf-8") as handle:
            self.assertEqual(json.load(handle)["assignments"], {})

    def test_a_ledger_entry_missing_a_field_is_damaged_not_defaulted(self):
        # An applied correction whose ledger entry lost "applied" must not come back to be
        # done again; the ledger is refused as damaged instead.
        relationship = self.registered()
        one = self.correction(relationship, generation=1)
        rid, message = relationship["relationshipId"], one["envelope"]["messageId"]
        base = os.path.join(self.tmp, "base-ledger.json")
        self.packet_check(self.first_assignment(dispatch="dispatch-1", issue=ISSUE),
                          receiver_id=CHILD, ledger=base)
        self.packet_check(one, receiver_id=CHILD, observation=self.observed(), ledger=base)
        code, _recorded = self.packet_check(one, receiver_id=CHILD, ledger=base, applied=True)
        self.assertEqual(code, 0)
        with open(base, encoding="utf-8") as handle:
            good = json.load(handle)
        cuts = [("answered", message, name) for name in ("applied", "toldToAct",
                                                         "disposition", "contentDigest")]
        cuts += [("assignments", rid, name) for name in ("mode", "workflow", "messageId",
                                                         "dispatchRequestId")]
        for number, (part, key, name) in enumerate(cuts, 1):
            with self.subTest(part=part, field=name):
                damaged = json.loads(json.dumps(good))
                del damaged[part][key][name]
                ledger = os.path.join(self.tmp, "cut-%d.json" % number)
                with open(ledger, "w", encoding="utf-8") as handle:
                    json.dump(damaged, handle)
                code, answer = self.packet_check(one, receiver_id=CHILD,
                                                 observation=self.observed(), ledger=ledger)
                self.assertEqual(code, cli.EXIT_USAGE, answer)

    def test_a_ledger_entry_no_writer_could_produce_is_damaged(self):
        # Applied is recorded only for an accepted packet some check said to act on, so an
        # entry saying applied without both is damage, not a reading - read, it would treat
        # work the receiver was never told to do as done.
        relationship = self.registered()
        one = self.correction(relationship, generation=1)
        message = one["envelope"]["messageId"]
        for number, change in enumerate(({"applied": True, "toldToAct": False},
                                         {"applied": True, "disposition": packets.REFUSAL}), 1):
            with self.subTest(change=change):
                document = receiver.empty_ledger(CHILD)
                document["answered"][message] = {**{
                    "contentDigest": packets.content_digest(one),
                    "disposition": packets.ACCEPTED, "applied": True, "toldToAct": True},
                    **change}
                ledger = os.path.join(self.tmp, "impossible-%d.json" % number)
                with open(ledger, "w", encoding="utf-8") as handle:
                    json.dump(document, handle)
                code, answer = self.packet_check(one, receiver_id=CHILD,
                                                 observation=self.observed(), ledger=ledger)
                self.assertEqual(code, cli.EXIT_USAGE, answer)

    def test_a_store_that_is_not_there_is_not_created_and_answers_nothing(self):
        missing = os.path.join(self.tmp, "nowhere")
        before = sorted(os.listdir(self.tmp))
        relationship = self.registered()
        one = self.report(relationship, self.event(relationship))
        before = sorted(name for name in os.listdir(self.tmp) if not name.endswith(".json"))
        _code, answer = self.packet_check(one, receiver_id=PARENT, state=missing)
        self.assertEqual(answer["disposition"], packets.UNAVAILABLE, answer)
        self.assertFalse(os.path.exists(missing))
        after = sorted(name for name in os.listdir(self.tmp) if not name.endswith(".json"))
        self.assertEqual(before, after)

    def test_a_file_that_is_not_a_relay_store_answers_nothing(self):
        relationship = self.registered()
        one = self.report(relationship, self.event(relationship))
        other = os.path.join(self.tmp, "other")
        os.makedirs(other)
        import sqlite3
        with contextlib.closing(sqlite3.connect(os.path.join(other, "relay.sqlite3"))) as db:
            db.execute("CREATE TABLE unrelated (x)")
            db.commit()
        _code, answer = self.packet_check(one, receiver_id=PARENT, state=other)
        self.assertEqual(answer["disposition"], packets.UNAVAILABLE, answer)
        self.assertTrue(any("could not be read" in note for note in answer["notes"]))

    def test_an_unscoped_relationship_has_no_revision_and_that_is_an_answer(self):
        relationship = self.registered(project=None)
        one = self.report(relationship, self.event(relationship), relation_revision=None)
        _code, answer = self.packet_check(one, receiver_id=PARENT, observation=self.observed())
        self.assertIsNone(answer["record"]["relationRevision"])
        self.assertNotIn("relationRevision", self.gap_fields(answer))

    def test_a_link_that_is_not_this_relationships_live_link_leaves_the_revision_unread(self):
        # The revision is the link's, and it answers for this relationship only while the link
        # is live, not superseded, and still joins this relationship's two tasks.
        changes = (("lower_task_id", "01other-child"), ("upper_task_id", "01other-parent"),
                   ("status", "archived"), ("status", "cancelled"),
                   ("superseded_by", "link-successor"))
        for number, (column, value) in enumerate(changes, 1):
            with self.subTest(column=column, value=value):
                child, issue = CHILD + "-link%d" % number, "REL-LINK-%d" % number
                relationship = self.registered(child=child, issue=issue,
                                               dispatch="dispatch-link-%d" % number)
                link = self.linkage.attachment(relationship["relationshipId"])["link"]
                one = self.correction(relationship, generation=1, recipient=child, issue=issue)
                self.store.db.execute(
                    "UPDATE scope_links SET " + column + " = ? WHERE link_id = ?",
                    (value, link["linkId"]))
                _code, answer = self.packet_check(one, receiver_id=child,
                                                  observation=self.observed())
                self.assertEqual(answer["disposition"], packets.UNAVAILABLE, answer)
                self.assertIn("relationRevision", self.gap_fields(answer))
                self.assertNotIn("relationRevision", answer["record"])

    def test_a_store_missing_a_column_answers_nothing_rather_than_failing(self):
        relationship = self.registered()
        one = self.correction(relationship, generation=1)
        self.store.db.execute("ALTER TABLE scope_links RENAME COLUMN revision TO revision_was")
        code, answer = self.packet_check(one, receiver_id=CHILD, observation=self.observed())
        self.assertEqual(code, 0, answer)
        self.assertEqual(answer.get("disposition"), packets.UNAVAILABLE, answer)
        self.assertTrue(any("could not be read" in note for note in answer["notes"]), answer)
        self.assertEqual(sorted(answer["record"]), sorted(
            [packets.RECORD_TASK_KEY["child"]]
            + [name for name in receiver.OBSERVATION_FIELDS if name in self.observed()]))

    def test_a_ledger_in_a_directory_not_made_yet_is_created_with_it(self):
        relationship = self.registered()
        ledger = os.path.join(self.tmp, "not-made-yet", "child-ledger.json")
        code, answer = self.packet_check(self.correction(relationship, generation=1),
                                         receiver_id=CHILD, observation=self.observed(),
                                         ledger=ledger)
        self.assertEqual(code, 0, answer)
        self.assertTrue(answer["act"], answer)
        self.assertTrue(os.path.exists(ledger))

    def test_a_ledger_belonging_to_another_receiver_is_refused(self):
        relationship = self.registered()
        ledger = os.path.join(self.tmp, "someone-elses.json")
        with open(ledger, "w", encoding="utf-8") as handle:
            json.dump(receiver.empty_ledger("01someone-else"), handle)
        code, answer = self.packet_check(self.correction(relationship, generation=1),
                                         receiver_id=CHILD, ledger=ledger)
        self.assertEqual(code, cli.EXIT_USAGE)
        self.assertIn("01someone-else", answer["detail"])

    def test_a_ledger_whose_entries_are_not_entries_is_refused_rather_than_failing(self):
        relationship = self.registered()
        one = self.correction(relationship, generation=1)
        rid, message = relationship["relationshipId"], one["envelope"]["messageId"]
        broken = (("answered", message, 7),
                  ("answered", "msg-other", {"contentDigest": "", "disposition": "accepted"}),
                  ("answered", "msg-other", {"contentDigest": "d" * 64, "disposition": "maybe"}),
                  ("answered", "msg-other", {"contentDigest": "d" * 64,
                                             "disposition": "accepted", "applied": "yes"}),
                  ("assignments", rid, 7),
                  ("assignments", rid, {"mode": 3, "workflow": "CXC Loop",
                                        "messageId": "m", "dispatchRequestId": None}),
                  # An entry that cannot name the assignment it came from supplies no mode.
                  ("assignments", rid, {"mode": "loop"}),
                  ("assignments", rid, {"mode": "sometimes", "workflow": "CXC Loop",
                                        "messageId": "m", "dispatchRequestId": None}),
                  ("assignments", rid, {"mode": "loop", "workflow": " ", "messageId": "m",
                                        "dispatchRequestId": None}))
        for number, (part, key, entry) in enumerate(broken, 1):
            with self.subTest(part=part, entry=entry):
                ledger = os.path.join(self.tmp, "broken-%d.json" % number)
                document = receiver.empty_ledger(CHILD)
                document[part][key] = entry
                with open(ledger, "w", encoding="utf-8") as handle:
                    json.dump(document, handle)
                code, answer = self.packet_check(one, receiver_id=CHILD,
                                                 observation=self.observed(), ledger=ledger)
                self.assertEqual(code, cli.EXIT_USAGE, answer)
                self.assertIn(repr(key), answer.get("detail", ""))


class TheRolePolicyTheServiceDeclares(StoreReception):
    """The receive step judges pairs by the policy the store's service declares.

    packet-check runs in whatever shell the receiver has, and that shell need not carry the
    variable the service was launched with. Read from the environment alone, every role-bound
    packet came back unavailable there, however well it agreed, and the receiver judged pairs
    by a different file than the relay enforces wherever the two differed.
    """

    def setUp(self):
        super().setUp()
        self.policy_file = os.environ[rolepolicy.ENVIRONMENT_VARIABLE]
        self.registered()
        self.assignment = self.first_assignment(dispatch="dispatch-1", issue=ISSUE)

    def declare(self, path):
        from codex_session_relay.service import LAUNCH_POLICY, LaunchPolicy
        LaunchPolicy(Path(self.state) / LAUNCH_POLICY).write(path=path, actor="test")

    def environment_names(self, path):
        previous = os.environ.get(rolepolicy.ENVIRONMENT_VARIABLE)

        def restore():
            if previous is None:
                os.environ.pop(rolepolicy.ENVIRONMENT_VARIABLE, None)
            else:
                os.environ[rolepolicy.ENVIRONMENT_VARIABLE] = previous
            rolepolicy.reset()

        self.addCleanup(restore)
        if path is None:
            os.environ.pop(rolepolicy.ENVIRONMENT_VARIABLE, None)
        else:
            os.environ[rolepolicy.ENVIRONMENT_VARIABLE] = path
        rolepolicy.reset()

    def test_a_check_run_without_the_variable_reads_the_declared_policy(self):
        self.declare(self.policy_file)
        self.environment_names(None)
        code, answer = self.packet_check(self.assignment, receiver_id=CHILD)
        self.assertEqual(code, 0, answer)
        self.assertEqual(answer["disposition"], packets.ACCEPTED, answer)
        self.assertTrue(answer["provenance"]["refusedPolicies"].startswith("rolepolicy "),
                        answer)

    def test_a_variable_naming_another_file_than_the_declaration_is_refused_as_that(self):
        self.declare(self.policy_file)
        other = Path(self.tmp) / "other-policy"
        other.mkdir()
        self.environment_names(write_policy(other))
        code, answer = self.packet_check(self.assignment, receiver_id=CHILD)
        self.assertEqual(code, cli.EXIT_REFUSED, answer)
        self.assertEqual(answer.get("reason"), "launch_policy_conflict", answer)

    def test_with_no_policy_declared_or_set_a_bound_packet_is_unavailable_and_says_why(self):
        self.environment_names(None)
        code, answer = self.packet_check(self.assignment, receiver_id=CHILD)
        self.assertEqual(code, 0, answer)
        self.assertEqual(answer["disposition"], packets.UNAVAILABLE, answer)
        self.assertIn(packets.POLICY, self.gap_fields(answer))
        self.assertFalse(answer["act"], answer)
        self.assertTrue(any(rolepolicy.ENVIRONMENT_VARIABLE in note for note in answer["notes"]),
                        answer["notes"])

    def test_recording_an_applied_packet_does_not_wait_on_the_policy(self):
        # --applied reads no store and judges no pair: it records, in the receiver's own
        # ledger, that the receiver acted. Refused over the policy after the work was done, the
        # ledger stayed unapplied and every replay said act again.
        ledger = os.path.join(self.tmp, "child-ledger.json")
        _code, first = self.packet_check(self.assignment, receiver_id=CHILD, ledger=ledger)
        self.assertTrue(first["act"], first)
        self.declare(self.policy_file)
        other = Path(self.tmp) / "other-policy"
        other.mkdir()
        self.environment_names(write_policy(other))
        code, applied = self.packet_check(self.assignment, receiver_id=CHILD, ledger=ledger,
                                          applied=True)
        self.assertEqual(code, 0, applied)
        self.environment_names(self.policy_file)
        _code, replay = self.packet_check(self.assignment, receiver_id=CHILD, ledger=ledger)
        self.assertFalse(replay["act"], replay)

    def test_the_modes_that_open_no_store_are_not_held_to_its_selection(self):
        # A store selection nobody made is refused before a command that reads that store.
        # --record and --applied open none, so they are not refused over one; the store-backed
        # check is.
        class NoSelection:
            @property
            def selection(self):
                raise AssertionError("the store selection was consulted")

        parser = cli.build_parser()
        packet = self._write("packet", self.assignment)
        for extra, reads in ((["--record", packet], False),
                             (["--receiver", CHILD, "--ledger", packet, "--applied"], False),
                             (["--receiver", CHILD], True)):
            with self.subTest(extra=extra):
                args = parser.parse_args(["packet-check", "--packet", packet] + extra)
                if reads:
                    with self.assertRaises(AssertionError):
                        cli._refuse_ambiguous_state(NoSelection(), args)
                else:
                    cli._refuse_ambiguous_state(NoSelection(), args)

    def test_the_store_backed_check_writes_nothing_in_the_state_directory(self):
        # It opens the store read-only and writes only the receiver's own ledger, which lives
        # elsewhere. Resolving the declared policy through the service's status probe left a
        # probe file created and removed in the state directory.
        self.declare(self.policy_file)
        self.environment_names(None)
        ledgers = Path(self.tmp) / "ledgers"
        ledgers.mkdir()
        packet = self._write("packet", self.assignment)
        before = (sorted(os.listdir(self.state)), os.stat(self.state).st_mtime_ns)
        printed = io.StringIO()
        with contextlib.redirect_stdout(printed):
            code = cli.main(["--state", self.state, "packet-check", "--packet", packet,
                             "--receiver", CHILD, "--ledger", str(ledgers / "child.json")])
        self.assertEqual(code, 0, printed.getvalue())
        self.assertEqual(json.loads(printed.getvalue())["disposition"], packets.ACCEPTED)
        self.assertEqual((sorted(os.listdir(self.state)), os.stat(self.state).st_mtime_ns),
                         before)

    def test_a_policy_file_too_deep_to_parse_ends_no_command_as_a_host_failure(self):
        # Every command takes the role-policy snapshot before its handler, and a file its
        # parser cannot descend raised out of it. After the receiver had acted, --applied then
        # failed as a host error and the replays asked for the work again; --record, which
        # reads no policy, failed the same way.
        ledger = os.path.join(self.tmp, "child-ledger.json")
        _code, first = self.packet_check(self.assignment, receiver_id=CHILD, ledger=ledger)
        self.assertTrue(first["act"], first)
        policy = Path(self.policy_file)
        original = policy.read_text(encoding="utf-8")

        def restore():
            policy.write_text(original, encoding="utf-8")
            rolepolicy.reset()

        self.addCleanup(restore)
        policy.write_text("[" * 15000 + "]" * 15000, encoding="utf-8")
        rolepolicy.reset()
        code, applied = self.packet_check(self.assignment, receiver_id=CHILD, ledger=ledger,
                                          applied=True)
        self.assertEqual(code, 0, applied)
        code, supplied = self.packet_check(self.assignment, record={"childTaskId": CHILD})
        self.assertEqual(code, 0, supplied)
        self.assertEqual(supplied["recordSource"], "supplied", supplied)
        code, checked = self.packet_check(self.assignment, receiver_id=CHILD)
        self.assertEqual(code, 0, checked)
        self.assertEqual(checked["disposition"], packets.UNAVAILABLE, checked)
        self.assertIn(packets.POLICY, self.gap_fields(checked))
        restore()
        _code, replay = self.packet_check(self.assignment, receiver_id=CHILD, ledger=ledger)
        self.assertFalse(replay["act"], replay)

    def test_a_declaration_too_deep_to_parse_is_refused_as_unreadable(self):
        from codex_session_relay.service import LAUNCH_POLICY
        (Path(self.state) / LAUNCH_POLICY).write_text("[" * 15000 + "]" * 15000,
                                                      encoding="utf-8")
        self.environment_names(None)
        code, answer = self.packet_check(self.assignment, receiver_id=CHILD)
        self.assertEqual(code, cli.EXIT_REFUSED, answer)
        self.assertEqual(answer.get("reason"), "launch_policy_unreadable", answer)

    def test_a_declaration_that_is_there_and_cannot_be_read_is_refused(self):
        # Absence is the one answer that lets a check fall back to its shell's variable. A
        # link to nothing was read as absence, so a shell still naming the policy the parent
        # left judged its old callback pair current; undecodable bytes ended the check as a
        # host failure, and a FIFO held it until something wrote to it.
        import threading

        from codex_session_relay.service import LAUNCH_POLICY
        declaration = Path(self.state) / LAUNCH_POLICY
        left = Path(self.tmp) / "left-policy"
        left.mkdir()
        roles = {**POLICY["roles"], "parent": {"model": SUPERSEDED_PARENT[0],
                                               "reasoningEffort": SUPERSEDED_PARENT[1]}}
        self.environment_names(write_policy(left, {"roles": roles}))
        stale = self.first_assignment(dispatch="dispatch-1", issue=ISSUE, subject="stale",
                                      callback=self.a_callback(pair=SUPERSEDED_PARENT))

        def dangling():
            declaration.symlink_to(Path(self.tmp) / "no-such-declaration.json")

        def undecodable():
            declaration.write_bytes(b'{"path": "\xff\xfe"}')

        def fifo():
            os.mkfifo(declaration)

        for make in (dangling, undecodable, fifo):
            with self.subTest(declaration=make.__name__):
                declaration.unlink(missing_ok=True)
                make()
                self.addCleanup(declaration.unlink, missing_ok=True)
                answers = []
                worker = threading.Thread(
                    target=lambda: answers.append(self.packet_check(stale, receiver_id=CHILD)),
                    daemon=True)
                worker.start()
                worker.join(10)
                if worker.is_alive():
                    # Release the reader blocked on the FIFO before failing, so the suite ends.
                    os.close(os.open(declaration, os.O_WRONLY | os.O_NONBLOCK))
                    worker.join(10)
                    self.fail("the check waited on a declaration that is not a regular file")
                code, answer = answers[0]
                self.assertEqual(code, cli.EXIT_REFUSED, answer)
                self.assertEqual(answer.get("reason"), "launch_policy_unreadable", answer)


class TheSevenStatesAsTheStoreHoldsThem(StoreReception):
    def setUp(self):
        super().setUp()
        self.relationship = self.registered()
        self.rid = self.relationship["relationshipId"]
        self.subject = self.event(self.relationship)

    def states(self, observation=None):
        return receiver.ladder(self.store.db, relationship_id=self.rid, subject=self.subject,
                               observation=observation)

    def attempt(self, number, state, internal="settled"):
        self.store.db.execute(
            "INSERT INTO attempts (request_id, event_id, attempt_no, kind, internal_state, state,"
            " observed_at) VALUES (?,?,?,?,?,?,?)",
            ("req-%d" % number, self.subject, number, "completion", internal, state,
             self.clock.iso()))

    def test_only_a_dispatched_attempt_is_a_transport_acceptance(self):
        self.attempt(1, "inbox_only")
        self.assertEqual(self.states()["handover"][packets.TRANSPORT_ACCEPTED]["state"],
                         envelope.NO)
        self.attempt(2, "held_uncertain")
        self.assertEqual(self.states()["handover"][packets.TRANSPORT_ACCEPTED]["state"],
                         envelope.UNMEASURED)
        self.attempt(3, "dispatched")
        self.assertEqual(self.states()["handover"][packets.TRANSPORT_ACCEPTED]["state"],
                         envelope.YES)

    def test_an_acknowledgement_holds_only_once_verified(self):
        self.assertEqual(self.states()["handover"][packets.RELAY_ACK]["state"], envelope.NO)
        self.store.db.execute(
            "INSERT INTO acks (event_id, record, ack_turn_id, accepted, verified, ack_at)"
            " VALUES (?,?,?,?,?,?)", (self.subject, "{}", "turn-ack", 1, "pending",
                                      self.clock.iso()))
        self.assertEqual(self.states()["handover"][packets.RELAY_ACK]["state"],
                         envelope.CONDITIONAL)
        self.store.db.execute("UPDATE acks SET verified = 'verified' WHERE event_id = ?",
                              (self.subject,))
        self.assertEqual(self.states()["handover"][packets.RELAY_ACK]["state"], envelope.YES)

    def test_a_verdict_and_an_acceptance_are_two_states(self):
        self.store.db.execute(
            "INSERT INTO verdicts (event_id, record, verdict, verdict_turn_id, decided_at)"
            " VALUES (?,?,?,?,?)", (self.subject, "{}", "needs_changes", "turn-v",
                                    self.clock.iso()))
        held = self.states()["handover"]
        self.assertEqual(held[packets.CRITERIA_VERDICT]["state"], envelope.YES)
        self.assertEqual(held[packets.PARENT_ACCEPTANCE]["state"], envelope.NO)

    def merge_turn(self, head, state="landed", repository=REPOSITORY):
        now = self.clock.iso()
        self.store.db.execute(
            "INSERT INTO merge_turns (turn_id, target_key, repository, base_ref, project_key,"
            " holder_task_id, holder_host_id, relationship_id, pr_number, candidate_head,"
            " state, tenure, requested_at, updated_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)",
            ("turn-" + head[:6] + repository[-4:], "target", repository, "dev", PROJECT,
             PARENT, HOST, self.rid, 107, head, state, 1, now, now))

    def test_a_landing_counts_only_for_the_observed_repository_and_head(self):
        self.assertEqual(self.states()["handover"][packets.MERGE_LANDING]["state"],
                         envelope.UNMEASURED)
        self.merge_turn("b" * 40)
        self.assertEqual(self.states(self.observed())["handover"][packets.MERGE_LANDING]
                         ["state"], envelope.NO)
        self.merge_turn(HEAD, repository="thisisjun786/somewhere-else")
        self.assertEqual(self.states(self.observed())["handover"][packets.MERGE_LANDING]
                         ["state"], envelope.NO)
        self.merge_turn(HEAD)
        self.assertEqual(self.states(self.observed())["handover"][packets.MERGE_LANDING]
                         ["state"], envelope.YES)

    def test_a_confirmed_outbox_row_is_coordination_and_not_linear_done(self):
        now = self.clock.iso()
        self.store.db.execute(
            "INSERT INTO sync_outbox (sync_id, relationship_id, issue_key, target, target_ref,"
            " subject_kind, event_id, identity_digest, summary, state, created_at, updated_at)"
            " VALUES (?,?,?,?,?,?,?,?,?,?,?,?)",
            ("sync-1", self.rid, ISSUE, "document", "doc-1", "verdict", self.subject, "d" * 64,
             "summary", "confirmed", now, now))
        read = self.states()
        self.assertEqual(read["handover"][packets.LINEAR_DONE]["state"], envelope.UNMEASURED)
        self.assertEqual(read["coordinationSync"]["state"], envelope.YES)

    def test_a_merge_and_a_direct_send_are_not_an_acknowledgement(self):
        """The CRW-177 shape: a send and a merge happened, and no relay ACK exists."""
        self.merge_turn(HEAD)
        one = dict(self.report(self.relationship, self.subject))
        claimed = packets.unobserved()
        claimed[packets.RELAY_ACK] = envelope.stage(envelope.YES, source="acks")
        claimed[packets.LINEAR_DONE] = envelope.stage(envelope.YES,
                                                      source="linear_issue_status")
        one["progression"] = claimed
        _code, answer = self.packet_check(one, receiver_id=PARENT, observation=self.observed())
        self.assertEqual(answer["handover"][packets.RELAY_ACK]["state"], envelope.NO)
        self.assertEqual(answer["handover"][packets.MERGE_LANDING]["state"], envelope.YES)
        self.assertIn(packets.MERGE_LANDING, answer["promotions"])
        self.assertEqual(answer["unbackedClaims"], [packets.RELAY_ACK])
        self.assertEqual(answer["unmeasurableClaims"], [packets.LINEAR_DONE])


class TheReceiveStepForAPacketNamingAnArtifact(StoreReception):
    """The receive step as the served instructions give it, for a packet that names an artifact.

    An artifact is never a store fact, so a correction naming one and checked without the
    receiver's own observation is unavailable on exactly the artifact's fields, and the
    instruction is to observe it and check the same packet again - not to act, and not to report
    a refusal. That step was missing from the served receive step, and a real child following
    it literally could not apply a correction (CRW-149 criterion-8 installed trial, F-C8-1).
    """

    def locator_correction(self, relationship, path):
        with open(path, "w", encoding="utf-8") as handle:
            handle.write("7\n")
        digest = "sha256:" + hashlib.sha256(open(path, "rb").read()).hexdigest()
        return self.correction(relationship, generation=1,
                               artifact=packets.locator(path=path, digest=digest)), digest

    def test_a_locator_correction_is_observed_and_checked_again_before_it_is_acted_on(self):
        relationship = self.registered()
        ledger = os.path.join(self.tmp, "child-ledger.json")
        path = os.path.join(self.tmp, "answer.txt")
        one, digest = self.locator_correction(relationship, path)

        _code, unobserved = self.packet_check(one, receiver_id=CHILD, ledger=ledger)
        self.assertEqual(unobserved["disposition"], packets.UNAVAILABLE, unobserved)
        self.assertEqual(self.gap_fields(unobserved), ["artifact.digest", "artifact.path"])
        self.assertFalse(unobserved["act"], unobserved)

        mine = {"source": "the child's own sha256 of answer.txt in this test",
                "artifactPath": path, "artifactDigest": digest}
        _code, observed = self.packet_check(one, receiver_id=CHILD, ledger=ledger,
                                            observation=mine)
        self.assertEqual(observed["disposition"], packets.ACCEPTED, observed)
        self.assertTrue(observed["act"], observed)
        self.assertEqual(observed["repeat"]["state"], packets.REPLAY)
        self.assertEqual(observed["repeat"]["previousDisposition"], packets.UNAVAILABLE)
        self.assertEqual(observed["provenance"]["artifactDigest"],
                         "observation: " + mine["source"])
        # Not applied yet, so the reason may not say it is not acted on: act is true.
        self.assertNotIn("not acted on", observed["repeat"]["reason"])
        self.assertIn("--applied", observed["repeat"]["reason"])

        code, refused = self.packet_check(one, receiver_id=CHILD, ledger=ledger,
                                          observation=mine, applied=True)
        self.assertNotEqual(code, 0, refused)
        self.assertEqual(refused.get("error"), "usage", refused)
        code, recorded = self.packet_check(one, receiver_id=CHILD, ledger=ledger, applied=True)
        self.assertEqual(code, 0, recorded)
        self.assertTrue(recorded["applied"], recorded)

        _code, after = self.packet_check(one, receiver_id=CHILD, ledger=ledger, observation=mine)
        self.assertFalse(after["act"], after)
        self.assertIn("recorded applied", after["repeat"]["reason"])

    def test_a_pull_request_correction_without_an_observation_is_unavailable_on_its_identity(self):
        relationship = self.registered()
        _code, answer = self.packet_check(self.correction(relationship, generation=1),
                                          receiver_id=CHILD,
                                          ledger=os.path.join(self.tmp, "child-ledger.json"))
        self.assertEqual(answer["disposition"], packets.UNAVAILABLE, answer)
        self.assertEqual(self.gap_fields(answer),
                         ["artifact.headSha", "artifact.number", "artifact.repository"])
        self.assertFalse(answer["act"], answer)

    def test_a_held_replay_is_not_told_it_is_acted_on(self):
        # A replay not yet recorded applied, on a paused relationship: accepted, act held. The
        # repeat's reason cannot know the pause, so it may not promise action either way.
        relationship = self.registered()
        rid = relationship["relationshipId"]
        ledger = os.path.join(self.tmp, "child-ledger.json")
        path = os.path.join(self.tmp, "answer.txt")
        one, digest = self.locator_correction(relationship, path)
        mine = {"source": "the child's own sha256 of answer.txt in this test",
                "artifactPath": path, "artifactDigest": digest}
        _code, first = self.packet_check(one, receiver_id=CHILD, ledger=ledger, observation=mine)
        self.assertTrue(first["act"], first)
        self.registry.set_status(rid, "paused", actor=PARENT)
        _code, held = self.packet_check(one, receiver_id=CHILD, ledger=ledger, observation=mine)
        self.assertEqual(held["disposition"], packets.ACCEPTED, held)
        self.assertFalse(held["act"], held)
        self.assertIn("paused", held.get("actHeld", ""), held)
        self.assertEqual(held["repeat"]["state"], packets.REPLAY)
        self.assertNotIn("acted on while", held["repeat"]["reason"])
        self.assertIn("today's answer decides act", held["repeat"]["reason"])
