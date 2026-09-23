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
    CHILD_EFFORT, CHILD_MODEL, PARENT_EFFORT, PARENT_MODEL, SUPERSEDED_PARENT, write_policy)

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

    def a_policy(self, mode=packets.LOOP, workflow="CXC Loop"):
        return packets.policy(model=CHILD_MODEL, effort=CHILD_EFFORT, workflow=workflow,
                              mode=mode, sandbox="danger-full-access", approval="never")

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
                  "prNumber": 107, "headSha": HEAD}
        code, answer = self.packet_check(one, record=record)
        self.assertEqual(code, 0)
        self.assertEqual(answer["recordSource"], "supplied")
        self.assertEqual(answer["disposition"], packets.ACCEPTED, answer)


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
        self.assertEqual([entry["field"] for entry in after["instructed"]], [packets.MODE])
        with open(ledger, encoding="utf-8") as handle:
            held = json.load(handle)
        self.assertEqual(list(held["assignments"].values())[0]["mode"], packets.LOOP)

    def test_a_dispatch_that_did_not_open_the_generation_is_another_relation(self):
        self.registered(child=self.CREATED, issue="REL-FIRST", dispatch="dispatch-first",
                        turn="turn-first")
        _code, answer = self.packet_check(self.first_assignment(dispatch="dispatch-other"),
                                          receiver_id=self.CREATED)
        self.assertEqual(self.kinds(answer), [packets.WRONG_RELATION])

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
        self.assertEqual(self.kinds(answer), [packets.WRONG_MODE])
        self.assertEqual(answer["provenance"]["mode"],
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
